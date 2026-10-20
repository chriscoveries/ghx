package governor

import (
	"bytes"
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
}
type Policy struct {
	// Floor is the only refusal for reads: stop when GitHub reports fewer than
	// this many calls remaining on the identity. There is no box or caller cap;
	// Classes/Callers only choose the credential and attribute the call.
	Floor     int
	StatusTTL int64
	Classes   map[string]Class
	Tokens    map[string]string
	Callers   map[string]Class
	Floors    map[string]int
}
type Rate struct {
	Remaining      int
	Reset, Blocked int64
	Failures       int64
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
	// URL is the path the entry was stored for (no query). Entries written
	// before this field existed have it empty and are purged on any write,
	// which is the conservative reading of an unknown scope.
	URL string `json:"url,omitempty"`
}
type Caller struct {
	Lane, Cwd, Class string
	Essential        bool
	Revalidate       bool
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
	g := &Gate{Dir: dir, Policy: Policy{Floor: 500, StatusTTL: 120, Classes: map[string]Class{"owner": {Identity: "owner"}, "automation": {Identity: "owner"}}, Tokens: map[string]string{}}, State: State{Rates: map[string]Rate{}, Calls: map[string][]int64{}, Counts: map[string]int64{}}, Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 30 * time.Second}, Now: time.Now}
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
	if g.Policy.Floor < 500 {
		return nil, fmt.Errorf("floor must be >= 500")
	}
	if g.Policy.StatusTTL <= 0 {
		g.Policy.StatusTTL = 120
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

// bodyReadDeadline bounds the mutex-held upstream body read (D4).
const bodyReadDeadline = 30 * time.Second

// readAllDeadline bounds a body read while the gate mutex is held: on expiry
// the body is closed so a hung upstream cannot serialize the governor (D4).
func readAllDeadline(body io.ReadCloser, limit int64, timeout time.Duration) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(body, limit+1))
		done <- result{b, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.b, r.err
	case <-timer.C:
		body.Close()
		return nil, fmt.Errorf("upstream body read exceeded %s deadline", timeout)
	}
}

func failure(code int, msg string) *http.Response {
	b, _ := json.Marshal(map[string]string{"message": "ghgate: " + msg})
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(b)), ContentLength: int64(len(b))}
}

// HitRate is the share of reads answered from the shared cache without an
// upstream call. Every upstream attempt counts, including 304 revalidations,
// because both spend GitHub calls.
func (g *Gate) HitRate() float64 {
	hit, upstream := g.State.Counts["hit"], g.State.Counts["upstream"]
	if total := hit + upstream; total > 0 {
		return float64(hit) / float64(total)
	}
	return 0
}

// HourCalls reports the last hour's calls per caller. Attribution and
// reporting only: no count here refuses a call.
func (g *Gate) HourCalls() map[string]int64 {
	out := map[string]int64{}
	after := g.Now().Unix() - 3600
	for key, calls := range g.State.Calls {
		if strings.HasPrefix(key, "read:") || key == "box" {
			continue
		}
		var count int64
		for _, at := range calls {
			if at > after {
				count++
			}
		}
		if count > 0 {
			out[key] = count
		}
	}
	return out
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
	if cls.Identity == "" {
		return failure(403, "caller identity not configured")
	}
	r = r.Clone(r.Context())
	r.RequestURI = ""
	r.Header = r.Header.Clone()
	if c.Revalidate {
		r.Header.Set("Cache-Control", "no-cache")
	}
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
	key := readKey(r, body)
	path := filepath.Join(g.Dir, "rest", key+".json")
	var e Entry
	cacheable := (r.Method == "GET" || (r.URL.Path == "/graphql" && kind == ast.Query)) && r.Header.Get("Range") == "" && !directive(r.Header, "no-store")
	if cacheable {
		if b, err := os.ReadFile(path); err == nil {
			json.Unmarshal(b, &e)
		}
	}
	stale := func(reason string) *http.Response {
		// An explicit revalidation is a promise that the answer is current. A
		// stale body carries its marker in a response header a CLI does not
		// print (gh snapshot prints JSON only), so serving stale here would hand
		// an authority caller an old approval it cannot detect. Fail instead.
		if c.Revalidate {
			return nil
		}
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
	// /rate_limit is free measurement: never budgeted, refused or upstream-counted.
	freeMeasurement := r.URL.Path == "/rate_limit"
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
	// The gate is the one serialized cache for the whole box. An ordinary
	// fresh hit consumes no caller/upstream budget. Explicit no-cache reads
	// always revalidate and cannot use this path for authority decisions.
	if reason == "" && cacheable && freshEntry(r, e, now, g.readTTL(r)) {
		g.State.Counts["hit"]++
		_ = os.Chtimes(path, g.Now(), g.Now())
		h := e.Header.Clone()
		h.Set("Age", strconv.FormatInt(now-e.At, 10))
		h.Set("X-Ghgate-Cache", "hit")
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(bytes.NewReader(e.Body)), ContentLength: int64(len(e.Body))}
	}
	// Attribution only: the per-caller hour count is reported, never enforced.
	caller := c.Lane
	if caller == "" {
		caller = c.Cwd
	}
	kept := g.keep(caller, now-3600)

	if reason != "" && !freeMeasurement {
		g.State.Counts["refused"]++
		if v := stale(reason); read && v != nil {
			return v
		}
		return failure(403, reason)
	}
	if !read {
		// A write invalidates what it can have changed. A repo-scoped write
		// drops only that repo's reads: check-runs and run lists for other
		// repos are untouched by it, and wiping them turns every watching lane
		// into a revalidation. Anything else (GraphQL mutations, /user,
		// /orgs, /notifications) can affect reads we cannot name, so it still
		// clears the cache.
		scope := repoScope(r.URL.Path)
		g.clearREST(scope)
		defer g.clearREST(scope)
		if wait := time.Duration(g.State.NextWrite - g.Now().UnixNano()); wait > 0 {
			time.Sleep(wait)
		}
		g.State.NextWrite = g.Now().Add(2 * time.Second).UnixNano()
		if g.Invalidate != nil {
			g.Invalidate()
			defer g.Invalidate()
		}
	}
	if r.Method == "GET" && cacheable && e.Header.Get("ETag") != "" {
		r.Header.Set("If-None-Match", e.Header.Get("ETag"))
	} else if r.Method == "GET" && cacheable && e.Header.Get("Last-Modified") != "" {
		r.Header.Set("If-Modified-Since", e.Header.Get("Last-Modified"))
	}
	if !freeMeasurement {
		g.State.Calls[caller] = append(kept, now)
	}
	for k, v := range g.State.Calls {
		if strings.HasPrefix(k, "read:") && len(v) > 0 && v[len(v)-1] <= now-60 {
			delete(g.State.Calls, k)
		}
	}
	if !freeMeasurement {
		g.State.Counts["upstream"]++
	}
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
	freshUpstream := true
	// /rate_limit can disagree with real request headers; it never clears a block/floor.
	// D3: rate introspection is exempt from the cooldown block itself.
	if r.URL.Path != "/rate_limit" {
		observed := resp.Header.Get("X-RateLimit-Resource")
		if observed != "" {
			id = cls.Identity + "/" + observed
			rate = g.State.Rates[id]
		}
		if remaining, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining")); err == nil {
			rate.Remaining = remaining
		}
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			rate.Reset = reset
		}
	}
	// GraphQL can surface rate errors inside HTTP 200. Preserve the original
	// bytes, but apply the same persistent cooldown and never cache partial data.
	limitedQuery := false
	if r.URL.Path == "/graphql" && resp.StatusCode == 200 {
		b, err := readAllDeadline(resp.Body, 8*1024*1024, bodyReadDeadline)
		if err != nil {
			resp.Body.Close()
			return failure(503, "upstream body unavailable")
		}
		if len(b) <= 8*1024*1024 {
			resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(b))
			resp.ContentLength = int64(len(b))
			limitedQuery = rateLimitedQueryBody(b)
		} else {
			resp.Body = &joinedBody{Reader: io.MultiReader(bytes.NewReader(b), resp.Body), Closer: resp.Body}
		}
	}
	// D2 (seat ruling): cool down on rate-limit signals only — a permission
	// denial is not a rate signal. Rate-limit 403s carry Retry-After or a
	// depleted Remaining counter; plain denials carry neither.
	rateLimited := resp.StatusCode == 429 || limitedQuery ||
		(resp.StatusCode == 403 &&
			(resp.Header.Get("Retry-After") != "" ||
				resp.Header.Get("X-RateLimit-Remaining") == "0"))
	if rateLimited {
		if rate.Failures < 32 {
			rate.Failures++
		}
		rate.Blocked = blockedUntil(resp.Header, now, rate.Failures)
		g.State.Counts["backoff"]++
	} else if r.URL.Path != "/rate_limit" && resp.StatusCode >= 200 && resp.StatusCode < 400 {
		rate.Failures = 0
		rate.Blocked = 0
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			rate.Blocked = blockedUntil(resp.Header, now, 1)
		}
	}
	g.State.Rates[id] = rate

	if resp.StatusCode == 304 && e.At <= 0 && r.Header.Get("If-None-Match") == "" && r.Header.Get("If-Modified-Since") == "" {
		resp.Body.Close()
		return failure(503, "304 without a cached representation")
	}
	if resp.StatusCode == 304 && e.At > 0 {
		resp.Body.Close()
		e.At = now
		for name, values := range resp.Header {
			// 304 carries no representation: framing headers describe the empty
			// response, not the cached body (review: Content-Length: 0 poisoned
			// the stored entry).
			if name == "Content-Length" || name == "Transfer-Encoding" || name == "Content-Encoding" || name == "Content-Range" {
				continue
			}
			e.Header[name] = values
		}
		if err := save(path, e); err != nil {
			return failure(503, "cache save failed")
		}
		g.State.Counts["304"]++
		// A revalidation is a call: it leaves the box and spends a GitHub
		// request even though 304 costs no quota. Keeping the stamp is what
		// makes hour_calls a call count rather than a quota count.
		resp = &http.Response{StatusCode: 200, Header: e.Header.Clone(), Body: io.NopCloser(bytes.NewReader(e.Body)), ContentLength: int64(len(e.Body))}
	}
	// D1: the block gates FUTURE requests; the fresh response just received is
	// returned before cooling — never swap a real upstream answer for stale/403.
	if rate.Blocked > now && read && r.URL.Path != "/rate_limit" && !freshUpstream && resp.StatusCode != 403 && resp.StatusCode != 429 {
		resp.Body.Close()
		if v := stale("blocked"); v != nil {
			resp = v
		} else {
			resp = failure(403, "blocked until "+time.Unix(rate.Blocked, 0).UTC().Format(time.RFC3339))
		}
	}
	if cacheable && resp.StatusCode == 200 && resp.Header.Get("X-Ghgate-Stale") == "" {
		b, err := readAllDeadline(resp.Body, 8*1024*1024, bodyReadDeadline)
		if err != nil {
			resp.Body.Close()
			return failure(503, "upstream body unavailable")
		}
		if len(b) <= 8*1024*1024 {
			resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(b))
			resp.ContentLength = int64(len(b))
			if !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") && resp.Header.Get("Vary") != "*" {
				if r.URL.Path != "/graphql" || cacheableQueryBody(b) {
					if err = g.storeEntry(path, Entry{At: now, URL: r.URL.Path, Header: resp.Header.Clone(), Body: b}); err != nil {
						return failure(503, "cache save failed")
					}
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

// repoScope returns the "/repos/OWNER/NAME" prefix a request path belongs to,
// or "" when the request is not repo-scoped and a write must clear everything.
func repoScope(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "repos" {
		return "/repos/" + parts[1] + "/" + parts[2]
	}
	return ""
}

func (g *Gate) clearREST(scope string) {
	files, _ := filepath.Glob(filepath.Join(g.Dir, "rest", "*.json"))
	for _, p := range files {
		if scope == "" {
			os.Remove(p)
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var e Entry
		if json.Unmarshal(b, &e) != nil || e.URL == "" || strings.HasPrefix(e.URL, scope) {
			os.Remove(p)
		}
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
