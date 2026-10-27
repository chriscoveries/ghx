package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/brunoborges/ghx/src/internal/allowlist"
	"github.com/brunoborges/ghx/src/internal/authenv"
	"github.com/brunoborges/ghx/src/internal/cache"
	execctx "github.com/brunoborges/ghx/src/internal/context"
	"github.com/brunoborges/ghx/src/internal/executor"
	"github.com/brunoborges/ghx/src/internal/protocol"
)

// Regression controls for the ghx upstream-sync union at f0edccf. Each test
// names the concrete defect it guards against.

// doSingleflightExec must Add(1) before Done() — missing Add panics the first
// miss and lets a joiner read nil res. Leader/follower completion.
func TestUnionRegression_FencedSingleflightWaitgroup(t *testing.T) {
	h := newTestHandler()
	started := make(chan struct{})
	finish := make(chan struct{})
	h.execute = func(_ context.Context, _ string, args []string, _ string, _ authenv.Environment) *executor.Result {
		close(started)
		<-finish
		return &executor.Result{Stdout: []byte("data"), ExitCode: 0}
	}
	var r *executor.Result
	var done bool
	go func() {
		r, _ = h.doSingleflightExec("k", func() *executor.Result { return h.execGH([]string{"x"}, "", nil) })
		done = true
	}()
	<-started
	go func() { r2, _ := h.doSingleflightExec("k", nil); _ = r2 }()
	close(finish)
	for !done {
		time.Sleep(time.Millisecond)
	}
	if r == nil || string(r.Stdout) != "data" {
		t.Fatal("leader result lost or waiter raced a nil res")
	}
}

// SetVersion fences the normalized fetch path: an invalidate between
// cache.Get and the fetch's store must drop the body.
func TestUnionRegression_NormalizedStoreFence(t *testing.T) {
	h := newTestHandler()
	key := "k"
	h.execute = func(_ context.Context, _ string, _ []string, _ string, _ authenv.Environment) *executor.Result {
		h.cache.Invalidate(func(_ *cache.Entry) bool { return true })
		return &executor.Result{Stdout: []byte("stale"), ExitCode: 0}
	}
	result, version, _ := h.doSingleflight(key, []string{"api", "x"}, &protocol.Request{})
	if result == nil {
		t.Fatal("fetch returned nil")
	}
	h.store(key, "cmd", allowlist.Classification{}, &protocol.Request{}, result, version)
	if got := h.cache.Get(key); got != nil {
		t.Fatalf("stale body stored across an invalidate: %q", got.Stdout)
	}
}

// InvalidateRepo carries generation++/persistLocked — without it,
// invalidateWrite's api-scope route lets old fills resurrect and disk
// removals vanish.
func TestUnionRegression_InvalidateRepoBumpsAndPersists(t *testing.T) {
	h := newTestHandler()
	h.cache.Set(&cache.Entry{Key: "a", Stdout: []byte("x"), Host: "h", Repo: "o/r", CachedAt: time.Now()})
	h.cache.Set(&cache.Entry{Key: "b", Stdout: []byte("y"), Host: "h", Repo: "o/r", CachedAt: time.Now()})
	before := h.cache.Version()
	if n := h.cache.InvalidateRepo("h", "o/r"); n != 2 {
		t.Fatalf("InvalidateRepo = %d, want 2", n)
	}
	if h.cache.Version() == before {
		t.Fatal("InvalidateRepo did not bump the generation")
	}
	if h.cache.Get("a") != nil || h.cache.Get("b") != nil {
		t.Fatal("invalidated entries still returned")
	}
}

// A 304 whose fetch straddled an invalidation must not return old.Stdout —
// the fallthrough is charged as another upstream call by handleExec's
// normal path, not free.
func TestUnionRegression_304StaleBodyRefused(t *testing.T) {
	h := newTestHandler()
	h.cfg.ResourceViews = true
	h.execute = func(_ context.Context, _ string, _ []string, _ string, _ authenv.Environment) *executor.Result {
		h.cache.Invalidate(func(_ *cache.Entry) bool { return true })
		return &executor.Result{Stdout: httpResponse("304 Not Modified", `"etag-1"`, "")}
	}
	resp := h.Handle(&protocol.Request{Type: protocol.TypeExec,
		Args: []string{"api", "repos/o/r/actions/runs"}, Context: execctx.ExecContext{Host: "h", Repo: "o/r"}})
	if resp == nil || string(resp.Stdout) == "old" {
		t.Fatal("304 stale body was returned after an in-fetch invalidation")
	}
}
