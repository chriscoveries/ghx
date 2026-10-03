package governor

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Class struct {
	Identity string
	Hourly   int
}
type Policy struct {
	BoxHourly int
	Floor     int
	PerMinute int
	Classes   map[string]Class
	Tokens    map[string]string
	Callers   map[string]Class
	Floors    map[string]int
}
type Rate struct {
	Remaining      int
	Reset, Blocked int64
}
type State struct {
	Rates     map[string]Rate
	Calls     map[string][]int64
	Counts    map[string]int64
	NextWrite int64
}
type Entry struct {
	At     int64
	Header http.Header
	Body   []byte
}
type Caller struct {
	Lane, Cwd, Class string
	Essential        bool
}
type Gate struct {
	Dir        string
	Policy     Policy
	State      State
	Transport  http.RoundTripper
	Now        func() time.Time
	mu         sync.Mutex
	Invalidate func()
}

func New(dir string) (*Gate, error) {
	g := &Gate{Dir: dir, Policy: Policy{Floor: 500, PerMinute: 3, Classes: map[string]Class{"owner": {Identity: "owner", Hourly: 150}, "automation": {Identity: "owner", Hourly: 150}}, Tokens: map[string]string{}}, State: State{Rates: map[string]Rate{}, Calls: map[string][]int64{}, Counts: map[string]int64{}}, Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 30 * time.Second}, Now: time.Now}
	if err := os.MkdirAll(filepath.Join(dir, "rest"), 0700); err != nil {
		return nil, err
	}
	for name, target := range map[string]any{"policy.json": &g.Policy, "state.json": &g.State} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			if err = json.Unmarshal(b, target); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if g.Policy.Floor < 500 || g.Policy.PerMinute < 1 {
		return nil, fmt.Errorf("floor must be >=500 and PerMinute positive")
	}
	return g, nil
}
func save(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p := path + ".tmp"
	if err = os.WriteFile(p, b, 0600); err != nil {
		return err
	}
	return os.Rename(p, path)
}
func (g *Gate) persist() error {
	if err := save(filepath.Join(g.Dir, "state.json"), g.State); err != nil {
		return err
	}
	findings := []string{}
	for id, r := range g.State.Rates {
		if r.Blocked > g.Now().Unix() {
			findings = append(findings, fmt.Sprintf("GITHUB API BLOCKED: %s until %s", id, time.Unix(r.Blocked, 0).UTC().Format(time.RFC3339)))
		} else if r.Reset > g.Now().Unix() && r.Remaining < g.floor(strings.SplitN(id, "/", 2)[1]) {
			findings = append(findings, fmt.Sprintf("GITHUB API LOW: %s until %s", id, time.Unix(r.Reset, 0).UTC().Format(time.RFC3339)))
		}
	}
	return save(filepath.Join(g.Dir, "fleet-status.json"), map[string]any{"at": g.Now().UTC(), "findings": findings, "counts": g.State.Counts})
}
func failure(code int, msg string) *http.Response {
	b, _ := json.Marshal(map[string]string{"message": "ghgate: " + msg})
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(b)), ContentLength: int64(len(b))}
}

func operation(b []byte) ast.Operation {
	var q struct {
		Query         string
		OperationName string
	}
	if json.Unmarshal(b, &q) != nil {
		return ""
	}
	doc, err := parser.ParseQueryWithTokenLimit(&ast.Source{Input: q.Query}, 10000)
	if err != nil {
		return ""
	}
	if q.OperationName == "" && len(doc.Operations) != 1 {
		return ""
	}
	if op := doc.Operations.ForName(q.OperationName); op != nil {
		return op.Operation
	}
	return ""
}
func ReadQuery(b []byte) bool { return operation(b) == ast.Query }
func (g *Gate) RoundTrip(r *http.Request, c Caller) (result *http.Response) {
	// One queue is deliberate: primary/secondary budgets are shared; no retries/polling.
	g.mu.Lock()
	defer g.mu.Unlock()
	defer func() {
		if g.persist() != nil {
			if result != nil {
				result.Body.Close()
			}
			result = failure(503, "state save failed")
		}
	}()
	now := g.Now().Unix()
	cls, ok := g.Policy.Classes[c.Class]
	if override, exists := g.Policy.Callers[c.Lane]; exists {
		cls = override
		ok = true
	}
	if !ok {
		return failure(403, "unknown caller class")
	}
	if cls.Hourly <= 0 {
		return failure(403, "caller budget not configured")
	}
	r = r.Clone(r.Context())
	r.RequestURI = ""
	r.Header = r.Header.Clone()
	if name := g.Policy.Tokens[cls.Identity]; name != "" {
		token := os.Getenv(name)
		if token == "" {
			return failure(403, "configured identity unavailable")
		}
		r.Header.Set("Authorization", "Bearer "+token)
	} else if cls.Identity != "owner" {
		return failure(403, "configured identity unavailable")
	}
	body := []byte{}
	if r.Body != nil && r.URL.Path == "/graphql" {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, 2*1024*1024+1))
		if err != nil || len(body) > 2*1024*1024 {
			return failure(413, "request body too large")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	if r.URL.Path == "/rate_limit" && r.Method == "GET" {
		resp, err := g.Transport.RoundTrip(r)
		if err != nil {
			return failure(503, "upstream unavailable")
		}
		return resp
	}
	kind := operation(body)
	read := r.Method == "GET" || r.Method == "HEAD" || (r.URL.Path == "/graphql" && kind == ast.Query)
	nonessential := read || (r.URL.Path == "/graphql" && kind != ast.Mutation)
	category := "core"
	if r.URL.Path == "/graphql" {
		category = "graphql"
	}
	if r.URL.Path == "/search/code" {
		category = "code_search"
	} else if strings.HasPrefix(r.URL.Path, "/search/") {
		category = "search"
	}
	id := cls.Identity + "/" + category
	rate := g.State.Rates[id]
	headers, _ := json.Marshal(r.Header)
	rawKey := r.URL.String() + "\x00" + string(headers) + "\x00" + string(body)
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(rawKey)))
	path := filepath.Join(g.Dir, "rest", key+".json")
	var e Entry
	cacheable := r.Method == "GET" && r.Header.Get("Range") == ""
	if cacheable {
		if b, err := os.ReadFile(path); err == nil {
			json.Unmarshal(b, &e)
		}
	}
	stale := func(reason string) *http.Response {
		if e.At > 0 && now-e.At <= 3600 {
			g.State.Counts["stale"]++
			h := e.Header.Clone()
			h.Set("X-Ghgate-Stale", "true")
			h.Set("Age", strconv.FormatInt(now-e.At, 10))
			h.Set("Warning", `110 ghgate "`+reason+`"`)
			return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(bytes.NewReader(e.Body)), ContentLength: int64(len(e.Body))}
		}
		return nil
	}
	floor := g.floor(category)
	reason := ""
	for k, v := range g.State.Rates {
		if strings.HasPrefix(k, cls.Identity+"/") && v.Blocked > now && v.Blocked > rate.Blocked {
			rate.Blocked = v.Blocked
		}
	}
	essential := c.Essential
	if rate.Blocked > now {
		reason = "blocked until " + time.Unix(rate.Blocked, 0).UTC().Format(time.RFC3339)
	} else if nonessential && !essential && rate.Reset > now && rate.Remaining < floor {
		reason = "budget floor until " + time.Unix(rate.Reset, 0).UTC().Format(time.RFC3339)
	}
	caller := c.Lane
	if caller == "" {
		caller = c.Cwd
	}
	kept := g.keep(caller, now-3600)
	repeatKey := "read:" + key
	var repeated []int64
	if read {
		repeated = g.keep(repeatKey, now-60)
	}
	if read && !essential && len(repeated) >= g.Policy.PerMinute {
		reason = "repeated read refused until " + time.Unix(repeated[0]+60, 0).UTC().Format(time.RFC3339)
	}
	boxCalls := g.keep("box", now-3600)
	if g.Policy.BoxHourly > 0 && len(boxCalls) >= g.Policy.BoxHourly {
		reason = "box hourly budget exceeded; inspect devmsg box health"
	}
	if nonessential && !essential && len(kept) >= cls.Hourly {
		reason = "caller hourly budget exceeded; inspect ghx xcache stats"
	}
	if reason != "" {
		g.State.Counts["refused"]++
		if v := stale(reason); read && v != nil {
			return v
		}
		return failure(403, reason)
	}
	if !read {
		g.clearREST()
		defer g.clearREST()
		if wait := time.Duration(g.State.NextWrite - g.Now().UnixNano()); wait > 0 {
			time.Sleep(wait)
		}
		g.State.NextWrite = g.Now().Add(2 * time.Second).UnixNano()
		if g.Invalidate != nil {
			g.Invalidate()
			defer g.Invalidate()
		}
	}
	if cacheable && e.Header.Get("ETag") != "" {
		r.Header.Set("If-None-Match", e.Header.Get("ETag"))
	}
	g.State.Calls[caller] = append(kept, now)
	g.State.Calls["box"] = append(boxCalls, now)
	if read {
		g.State.Calls[repeatKey] = append(repeated, now)
	}
	for k, v := range g.State.Calls {
		if strings.HasPrefix(k, "read:") && len(v) > 0 && v[len(v)-1] <= now-60 {
			delete(g.State.Calls, k)
		}
	}
	g.State.Counts["upstream"]++
	if c.Lane == "" {
		g.State.Counts["unattributed"]++
	}
	if err := g.persist(); err != nil {
		return failure(503, "state save failed")
	}
	resp, err := g.Transport.RoundTrip(r)
	if err != nil {
		g.State.Counts["outage"]++
		if v := stale("upstream unavailable"); read && v != nil {
			return v
		}
		return failure(503, "upstream unavailable")
	}
	// /rate_limit can disagree with real request headers; it never clears a block/floor.
	if r.URL.Path != "/rate_limit" {
		observed := resp.Header.Get("X-RateLimit-Resource")
		if observed != "" {
			id = cls.Identity + "/" + observed
			rate = g.State.Rates[id]
		}
		limited := false
		if resp.StatusCode == 403 || resp.StatusCode == 429 {
			b, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
			resp.Body.Close()
			if err != nil {
				return failure(503, "upstream body unavailable")
			}
			resp.Body = io.NopCloser(bytes.NewReader(b))
			resp.ContentLength = int64(len(b))
			lower := strings.ToLower(string(b))
			limited = strings.Contains(lower, "rate limit") || strings.Contains(lower, "secondary rate")
		}
		if remaining, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining")); err == nil {
			rate.Remaining = remaining
		}
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			rate.Reset = reset
		}
		if resp.Header.Get("X-RateLimit-Remaining") == "0" || ((resp.StatusCode == 403 || resp.StatusCode == 429) && (resp.Header.Get("Retry-After") != "" || limited)) {
			rate.Blocked = rate.Reset
			if rate.Blocked <= now {
				rate.Blocked = now + 3600
			}
			if retry, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64); err == nil && now+retry > rate.Blocked {
				rate.Blocked = now + retry
			}
		}
		g.State.Rates[id] = rate
	}
	if resp.StatusCode == 304 && e.At > 0 {
		resp.Body.Close()
		e.At = now
		save(path, e)
		g.State.Counts["304"]++
		g.State.Calls[caller] = g.State.Calls[caller][:len(g.State.Calls[caller])-1]
		g.State.Calls["box"] = g.State.Calls["box"][:len(g.State.Calls["box"])-1]
		resp = &http.Response{StatusCode: 200, Header: e.Header.Clone(), Body: io.NopCloser(bytes.NewReader(e.Body)), ContentLength: int64(len(e.Body))}
	}
	if rate.Blocked > now && read {
		resp.Body.Close()
		if v := stale("blocked"); v != nil {
			resp = v
		} else {
			resp = failure(403, "blocked until "+time.Unix(rate.Blocked, 0).UTC().Format(time.RFC3339))
		}
	}
	if cacheable && resp.StatusCode == 200 && resp.Header.Get("X-Ghgate-Stale") == "" && resp.Header.Get("ETag") != "" {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024+1))
		if err != nil {
			resp.Body.Close()
			return failure(503, "upstream body unavailable")
		}
		if len(b) <= 512*1024 {
			resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(b))
			resp.ContentLength = int64(len(b))
			if !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") && resp.Header.Get("Vary") != "*" {
				files, _ := filepath.Glob(filepath.Join(g.Dir, "rest", "*.json"))
				if len(files) >= 128 {
					for _, p := range files {
						if p != path {
							os.Remove(p)
							break
						}
					}
				}
				if err = save(path, Entry{now, resp.Header.Clone(), b}); err != nil {
					return failure(503, "cache save failed")
				}
			}
		} else {
			resp.Body = &joinedBody{Reader: io.MultiReader(bytes.NewReader(b), resp.Body), Closer: resp.Body}
		}
	}
	return resp
}

type joinedBody struct {
	io.Reader
	io.Closer
}

func (g *Gate) floor(category string) int {
	if v, ok := g.Policy.Floors[category]; ok && category != "core" && category != "graphql" {
		return v
	}
	if category == "search" || category == "code_search" {
		return 3
	}
	return g.Policy.Floor
}

func (g *Gate) clearREST() {
	files, _ := filepath.Glob(filepath.Join(g.Dir, "rest", "*.json"))
	for _, p := range files {
		os.Remove(p)
	}
}

func (g *Gate) keep(key string, after int64) []int64 {
	kept := g.State.Calls[key][:0]
	for _, t := range g.State.Calls[key] {
		if t > after {
			kept = append(kept, t)
		}
	}
	g.State.Calls[key] = kept
	return kept
}
