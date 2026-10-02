// Package replay models only information present in an anonymised command log.
// It never executes gh, resolves credentials, or makes a network request.
package replay

import (
	"bufio"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/brunoborges/ghx/src/internal/allowlist"
)

type Call struct {
	At      time.Time `json:"at"`
	Command string    `json:"command"`
	Key     string    `json:"key"`
	Exit    int       `json:"exit"`
	Cached  bool      `json:"cached"`
}
type Result struct {
	Reads   int     `json:"reads"`
	Hits    int     `json:"hits"`
	Misses  int     `json:"misses"`
	HitRate float64 `json:"hit_rate_percent"`
}
type Report struct {
	SHA256                  string   `json:"input_sha256"`
	Calls                   int      `json:"calls"`
	ObservedHits            int      `json:"observed_cached_calls"`
	KnownResourceViewShapes int      `json:"known_resource_view_shapes"`
	AmbiguousAPI            int      `json:"ambiguous_api_calls"`
	PotentialWriteFlushes   int      `json:"potential_write_flushes"`
	TTLSeconds              float64  `json:"ttl_seconds"`
	Capacity                int      `json:"capacity"`
	Before                  Result   `json:"before_exact_hash_model"`
	After                   Result   `json:"after_exact_hash_model"`
	ResourceHitRate         *float64 `json:"after_resource_hit_rate_percent"`
	Limit                   string   `json:"measurement_limit"`
}

// Analyze assumes 30s TTL/1000 entries, cold start, observed result codes, and
// flushes globally on any known or possible write because repo/selector data is
// absent. The after model never caches failures (except pending pr checks).
func Analyze(r io.Reader) (Report, error) {
	hash := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(r, hash))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var calls []Call
	for scanner.Scan() {
		var c Call
		if err := json.Unmarshal(scanner.Bytes(), &c); err != nil {
			return Report{}, fmt.Errorf("invalid JSON on line %d", len(calls)+1)
		}
		if c.At.IsZero() || c.Command == "" || c.Key == "" {
			return Report{}, fmt.Errorf("missing timestamp, command or key on line %d", len(calls)+1)
		}
		calls = append(calls, c)
	}
	if err := scanner.Err(); err != nil {
		return Report{}, err
	}
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].At.Before(calls[j].At) })
	p := Report{SHA256: hex.EncodeToString(hash.Sum(nil)), Calls: len(calls), TTLSeconds: 30, Capacity: 1000,
		Limit: "Opaque full-command hashes cannot identify shared resources, requested fields, completed attempts, validators, repository dependencies, response sizes, or command durations. Additional resource/immutable/304 hit rate is unmeasurable, not zero. Exact-hash results are conservative models, not observed deployment rates; possible writes flush both models globally."}
	before, after := newStore(p.Capacity), newStore(p.Capacity)
	classifier := allowlist.NewClassifier(nil)
	ttl := time.Duration(p.TTLSeconds) * time.Second
	for _, c := range calls {
		if c.Cached {
			p.ObservedHits++
		}
		args := strings.Fields(c.Command)
		cl := classifier.Classify(args)
		if c.Command == "pr view" || c.Command == "issue view" || c.Command == "run view" {
			p.KnownResourceViewShapes++
		}
		ambiguous := len(args) > 0 && args[0] == "api"
		if ambiguous {
			p.AmbiguousAPI++
		}
		if ambiguous || cl.Type == allowlist.Mutation {
			p.PotentialWriteFlushes++
			before.clear()
			after.clear()
			continue
		}
		if cl.Type != allowlist.Cacheable {
			continue
		}
		p.Before.Reads++
		p.After.Reads++
		if before.get(c.Key, c.At) {
			p.Before.Hits++
		} else {
			before.set(c.Key, c.At.Add(ttl))
		}
		if after.get(c.Key, c.At) {
			p.After.Hits++
		} else if c.Exit == 0 || c.Command == "pr checks" && c.Exit == 8 {
			after.set(c.Key, c.At.Add(ttl))
		}
	}
	for _, result := range []*Result{&p.Before, &p.After} {
		result.Misses = result.Reads - result.Hits
		if result.Reads > 0 {
			result.HitRate = 100 * float64(result.Hits) / float64(result.Reads)
		}
	}
	return p, nil
}

type item struct {
	key    string
	expiry time.Time
}
type store struct {
	capacity int
	items    map[string]*list.Element
	order    *list.List
}

func newStore(capacity int) *store {
	return &store{capacity: capacity, items: map[string]*list.Element{}, order: list.New()}
}
func (s *store) clear() { s.items = map[string]*list.Element{}; s.order.Init() }
func (s *store) get(key string, at time.Time) bool {
	e := s.items[key]
	if e == nil {
		return false
	}
	if at.After(e.Value.(item).expiry) {
		delete(s.items, key)
		s.order.Remove(e)
		return false
	}
	s.order.MoveToFront(e)
	return true
}
func (s *store) set(key string, expiry time.Time) {
	if s.capacity <= 0 {
		return
	}
	if e := s.items[key]; e != nil {
		delete(s.items, key)
		s.order.Remove(e)
	}
	if s.order.Len() >= s.capacity {
		e := s.order.Back()
		delete(s.items, e.Value.(item).key)
		s.order.Remove(e)
	}
	s.items[key] = s.order.PushFront(item{key, expiry})
}
