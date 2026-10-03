package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brunoborges/ghx/src/internal/allowlist"
	"github.com/brunoborges/ghx/src/internal/authenv"
	"github.com/brunoborges/ghx/src/internal/cache"
	execctx "github.com/brunoborges/ghx/src/internal/context"
	"github.com/brunoborges/ghx/src/internal/executor"
	"github.com/brunoborges/ghx/src/internal/protocol"
	"github.com/brunoborges/ghx/src/internal/resource"
)

func captured(t *testing.T, name string) []byte {
	t.Helper()
	b, e := os.ReadFile("../../../testdata/resource/" + name + ".json")
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func resourceHandler(t *testing.T) (*Handler, []byte, *atomic.Int32) {
	t.Helper()
	h := newTestHandler()
	h.cfg.ResourceViews = true
	h.cfg.ImmutableTTL = 24 * time.Hour
	body := captured(t, "pr21")
	calls := &atomic.Int32{}
	h.execute = func(_ context.Context, _ string, args []string, _ string, _ authenv.Environment) *executor.Result {
		calls.Add(1)
		if args[0] == "pr" && args[1] == "view" {
			return &executor.Result{Stdout: body}
		}
		return &executor.Result{}
	}
	return h, body, calls
}
func view(fields string) *protocol.Request {
	return &protocol.Request{Args: []string{"pr", "view", "21", "--json", fields}, Context: execctx.ExecContext{Host: "github.com", Repo: "brunoborges/ghx", Branch: "one", TokenHash: "identity"}}
}
func expire(t *testing.T, h *Handler, key string, age time.Duration) {
	t.Helper()
	e := h.cache.Peek(key)
	if e == nil {
		t.Fatal("entry not retained")
	}
	copy := *e
	copy.CachedAt = time.Now().Add(-age)
	h.cache.Set(&copy)
}
func TestResourceProjectionHitAndLongLivedFixedFields(t *testing.T) {
	h, body, calls := resourceHandler(t)
	first := h.Handle(view("number,title"))
	secondReq := view("body,mergeCommit")
	secondReq.Context.Branch = "two"
	second := h.Handle(secondReq)
	shape := resource.Prepare(secondReq.Context, secondReq.Args)
	expected, _ := shape.Project(body)
	if first.Cached || !second.Cached || !bytes.Equal(second.Stdout, expected) || calls.Load() != 1 {
		t.Fatalf("responses: first=%+v second=%+v calls=%d", first, second, calls.Load())
	}
	key := execctx.CacheKey(shape.Context, shape.Args)
	expire(t, h, key, time.Minute)
	fixed := h.Handle(view("number,mergedAt,mergeCommit"))
	if !fixed.Cached || calls.Load() != 1 {
		t.Fatal("merged fixed fields expired with ordinary TTL")
	}
	mutable := h.Handle(view("number,title"))
	if mutable.Cached || calls.Load() != 2 {
		t.Fatal("merged title wrongly immutable")
	}
	expire(t, h, key, 25*time.Hour)
	if h.Handle(view("number,mergedAt")).Cached || calls.Load() != 3 {
		t.Fatal("long TTL is unbounded")
	}
}
func TestResourceAuthAndNoCache(t *testing.T) {
	h, _, calls := resourceHandler(t)
	h.Handle(view("number"))
	other := view("number")
	other.Context.TokenHash = "other"
	h.Handle(other)
	bypass := view("number")
	bypass.NoCache = true
	h.Handle(bypass)
	if calls.Load() != 3 {
		t.Fatalf("auth or bypass collapsed: %d", calls.Load())
	}
}
func TestResourceSingleflightProjectsEachWaiter(t *testing.T) {
	h, body, _ := resourceHandler(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	h.execute = func(context.Context, string, []string, string, authenv.Environment) *executor.Result {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return &executor.Result{Stdout: body}
	}
	const n = 12
	var wg sync.WaitGroup
	responses := make([]*protocol.Response, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fields := "number"
			if i%2 == 1 {
				fields = "title"
			}
			responses[i] = h.Handle(view(fields))
		}()
	}
	<-started
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("superset executions=%d", calls.Load())
	}
	for i, r := range responses {
		var got map[string]json.RawMessage
		if json.Unmarshal(r.Stdout, &got) != nil || len(got) != 1 {
			t.Fatalf("waiter %d projection=%s", i, r.Stdout)
		}
	}
}
func TestPreciseWriteInvalidation(t *testing.T) {
	for _, args := range [][]string{
		{"pr", "edit", "21", "--repo", "brunoborges/ghx"},
		{"pr", "comment", "https://github.com/brunoborges/ghx/pull/21"},
		{"api", "--method=PATCH", "repos/brunoborges/ghx/pulls/21"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, _, _ := resourceHandler(t)
			seed := func(key, repo, id string, kind allowlist.ResourceType) {
				h.cache.Set(&cache.Entry{Key: key, Host: "github.com", Repo: repo, Resource: kind, ResourceID: id, CachedAt: time.Now(), TTL: time.Hour})
			}
			seed("target", "brunoborges/ghx", "21", allowlist.ResourcePR)
			seed("list", "brunoborges/ghx", "", allowlist.ResourcePR)
			seed("issue-comments", "brunoborges/ghx", "21", allowlist.ResourceIssue)
			seed("other-object", "brunoborges/ghx", "22", allowlist.ResourcePR)
			seed("other-repo", "other/repo", "21", allowlist.ResourcePR)
			req := view("number")
			req.Args = args
			req.Context.Repo = "default/repo"
			req.NoCache = true
			h.Handle(req)
			for _, key := range []string{"target", "list", "issue-comments"} {
				if h.cache.Peek(key) != nil {
					t.Errorf("write retained %s", key)
				}
			}
			for _, key := range []string{"other-object", "other-repo"} {
				if h.cache.Peek(key) == nil {
					t.Errorf("write evicted %s", key)
				}
			}
		})
	}
}
func TestFailedWriteRetainsCacheAndCreateOnlyEvictsCollections(t *testing.T) {
	h, _, _ := resourceHandler(t)
	h.Handle(view("number"))
	req := view("number")
	req.Args = []string{"pr", "edit", "21"}
	h.execute = func(context.Context, string, []string, string, authenv.Environment) *executor.Result {
		return &executor.Result{ExitCode: 1}
	}
	h.Handle(req)
	if !h.Handle(view("number")).Cached {
		t.Fatal("failed write evicted state")
	}
	h.execute = func(context.Context, string, []string, string, authenv.Environment) *executor.Result {
		return &executor.Result{}
	}
	h.cache.Set(&cache.Entry{Key: "list", Host: req.Context.Host, Repo: req.Context.Repo, Resource: allowlist.ResourcePR, TTL: time.Hour, CachedAt: time.Now()})
	req.Args = []string{"pr", "create"}
	h.Handle(req)
	if h.cache.Peek("list") != nil || !h.Handle(view("number")).Cached {
		t.Fatal("create did not preserve existing objects while evicting collections")
	}
}
func TestReadBeforeWriteCannotRefillOrBeJoinedAfterWrite(t *testing.T) {
	h, body, _ := resourceHandler(t)
	started, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	h.execute = func(_ context.Context, _ string, args []string, _ string, _ authenv.Environment) *executor.Result {
		if args[1] == "view" {
			if reads.Add(1) == 1 {
				close(started)
				<-release
			}
			return &executor.Result{Stdout: body}
		}
		return &executor.Result{}
	}
	done := make(chan struct{})
	go func() { h.Handle(view("number")); close(done) }()
	<-started
	mutation := view("number")
	mutation.Args = []string{"pr", "edit", "21"}
	h.Handle(mutation)
	second := h.Handle(view("title"))
	if second.Cached || reads.Load() != 2 {
		t.Fatal("post-write read joined pre-write flight")
	}
	s := resource.Prepare(view("number").Context, view("number").Args)
	key := execctx.CacheKey(s.Context, s.Args)
	snapshot := h.cache.Peek(key)
	close(release)
	<-done
	if h.cache.Peek(key) != snapshot {
		t.Fatal("pre-write flight replaced post-write cache entry")
	}
	if !h.Handle(view("body")).Cached || reads.Load() != 2 {
		t.Fatal("post-write snapshot not retained")
	}
	// Directly fence an in-flight insert even when no cached entry matched.
	version := h.cache.Version()
	h.cache.Flush()
	h.cache.SetVersion(&cache.Entry{Key: "stale"}, version)
	if h.cache.Peek("stale") != nil {
		t.Fatal("flush permitted obsolete insert")
	}
}
func httpOutput(body []byte, status int, etag string) []byte {
	// Protocol framing wraps a captured GitHub body, never generated response content.
	return append([]byte(fmt.Sprintf("HTTP/2.0 %d %s\r\nContent-Type: application/json\r\nETag: %s\r\nContent-Length: %d\r\n\r\n", status, map[int]string{200: "OK", 304: "Not Modified", 403: "Forbidden"}[status], etag, len(body))), body...)
}
func TestConditionalGETReuses304AndReplacesChangedBodies(t *testing.T) {
	h := newTestHandler()
	body := captured(t, "repository")
	req := &protocol.Request{Args: []string{"api", "repos/brunoborges/ghx"}, Context: execctx.ExecContext{Host: "github.com", Repo: "brunoborges/ghx", TokenHash: "identity"}}
	key := execctx.CacheKey(req.Context, req.Args)
	calls := 0
	h.execute = func(_ context.Context, _ string, args []string, _ string, _ authenv.Environment) *executor.Result {
		calls++
		if !reflect.DeepEqual(args[len(req.Args):], []string{"--include"}) {
			t.Fatalf("cold arguments=%v", args)
		}
		return &executor.Result{Stdout: httpOutput(body, 200, `W/"captured"`)}
	}
	first := h.Handle(req)
	if first.Cached || !bytes.Equal(first.Stdout, body) {
		t.Fatal("cold GET leaked headers or changed body")
	}
	if !h.Handle(req).Cached || calls != 1 {
		t.Fatal("fresh GET not reused")
	}
	expire(t, h, key, time.Minute)
	h.execute = func(_ context.Context, _ string, args []string, _ string, _ authenv.Environment) *executor.Result {
		calls++
		if !reflect.DeepEqual(args[len(req.Args):], []string{"--include", "--header", `If-None-Match: W/"captured"`}) {
			t.Fatalf("revalidation arguments=%v", args)
		}
		return &executor.Result{Stdout: httpOutput(nil, 304, "")}
	}
	validated := h.Handle(req)
	if !validated.Cached || !bytes.Equal(validated.Stdout, body) || calls != 2 || h.cache.Peek(key).IsExpired() {
		t.Fatal("304 did not refresh original bytes")
	}
	expire(t, h, key, time.Minute)
	changed := captured(t, "pr22")
	h.execute = func(context.Context, string, []string, string, authenv.Environment) *executor.Result {
		return &executor.Result{Stdout: httpOutput(changed, 200, `"changed"`)}
	}
	if r := h.Handle(req); r.Cached || !bytes.Equal(r.Stdout, changed) || h.cache.Peek(key).ETag != `"changed"` {
		t.Fatal("changed GET did not replace body and validator")
	}
}
func TestConditionalFailureDoesNotCacheOrReplaceValidator(t *testing.T) {
	h := newTestHandler()
	req := &protocol.Request{Args: []string{"api", "repos/brunoborges/ghx"}, Context: execctx.ExecContext{Host: "github.com"}}
	body := captured(t, "repository")
	key := execctx.CacheKey(req.Context, req.Args)
	h.cache.Set(&cache.Entry{Key: key, Stdout: body, ETag: `"old"`, TTL: time.Second, CachedAt: time.Now().Add(-time.Minute)})
	h.execute = func(context.Context, string, []string, string, authenv.Environment) *executor.Result {
		return &executor.Result{Stdout: httpOutput(body, 403, `"bad"`), Stderr: []byte("HTTP 403"), ExitCode: 1}
	}
	r := h.Handle(req)
	if r.ExitCode != 1 || h.cache.Peek(key).ETag != `"old"` || r.Cached {
		t.Fatal("failed GET cached or changed old validator")
	}
}
func TestCapturedWorkloadReplay(t *testing.T) {
	fields := []string{"number", "title", "body", "mergedAt", "state", "number,title", "title,number", "mergeCommit", "headRefOid", "labels"}
	run := func(enabled bool) (hits, calls int) {
		h, body, _ := resourceHandler(t)
		h.cfg.ResourceViews = enabled
		h.execute = func(context.Context, string, []string, string, authenv.Environment) *executor.Result {
			calls++
			return &executor.Result{Stdout: body}
		}
		for _, f := range fields {
			if h.Handle(view(f)).Cached {
				hits++
			}
		}
		return
	}
	beforeHits, beforeCalls := run(false)
	afterHits, afterCalls := run(true)
	t.Logf("captured workload: calls=%d; before hits=%d upstream=%d; after hits=%d upstream=%d", len(fields), beforeHits, beforeCalls, afterHits, afterCalls)
	if beforeHits != 0 || beforeCalls != 10 || afterHits != 9 || afterCalls != 1 {
		t.Fatal("resource workload hit rate regressed")
	}
}

func TestUnsupportedSupersetFallsBackAndDoesNotRepeat(t *testing.T) {
	h, body, _ := resourceHandler(t)
	calls := 0
	h.execute = func(_ context.Context, _ string, args []string, _ string, _ authenv.Environment) *executor.Result {
		calls++
		if len(args) > 4 && strings.Contains(strings.Join(args, " "), "reviewRequests") {
			return &executor.Result{ExitCode: 1, Stderr: []byte("Unknown JSON field: reviewRequests")}
		}
		return &executor.Result{Stdout: body}
	}
	first := h.Handle(view("number"))
	second := h.Handle(view("number"))
	if first.ExitCode != 0 || !second.Cached || calls != 2 {
		t.Fatalf("native fallback repeated: calls=%d first=%+v second=%+v", calls, first, second)
	}
}
func TestUnavailableSupersetDoesNotRetryOrCacheFailure(t *testing.T) {
	h, _, calls := resourceHandler(t)
	h.execute = func(context.Context, string, []string, string, authenv.Environment) *executor.Result {
		calls.Add(1)
		return &executor.Result{ExitCode: 1, Stderr: []byte("HTTP 403: API rate limit exceeded")}
	}
	r := h.Handle(view("number"))
	if r.ExitCode != 1 || calls.Load() != 1 || h.cache.Size() != 0 {
		t.Fatal("outage retried or cached")
	}
}
func TestETagDependenciesInvalidatedByCLIWrite(t *testing.T) {
	h, body, _ := resourceHandler(t)
	req := view("number")
	req.Args = []string{"api", "repos/brunoborges/ghx/pulls/21"}
	var commands [][]string
	h.execute = func(_ context.Context, _ string, args []string, _ string, _ authenv.Environment) *executor.Result {
		commands = append(commands, args)
		if args[0] == "api" {
			return &executor.Result{Stdout: httpOutput(body, 200, `"old"`)}
		}
		return &executor.Result{}
	}
	h.Handle(req)
	write := view("number")
	write.Args = []string{"pr", "edit", "21"}
	h.Handle(write)
	h.Handle(req)
	if len(commands) != 3 || strings.Contains(strings.Join(commands[2], " "), "If-None-Match") {
		t.Fatal("invalidated ETag survived CLI mutation")
	}
}
