package daemon

import (
	"bytes"
	"context"
	"strings"
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

// The bodies come from the public captured ghx repository fixture. Channels
// and invalidation are explicit schedule controls, not new API observations.
func TestUnionSingleflightLeaderAndFollower(t *testing.T) {
	h := newTestHandler()
	body := captured(t, "repository")
	leader, shared := h.doSingleflightExec("leader", func() *executor.Result {
		return &executor.Result{Stdout: body}
	})
	if shared || !bytes.Equal(leader.Stdout, body) {
		t.Fatal("singleflight leader did not complete with the retained body")
	}

	// Seed a real in-flight waitgroup, hold its leader until the follower has
	// started, then publish the captured body before releasing the waiter.
	c := &call{}
	c.wg.Add(1)
	h.mu.Lock()
	h.inflight["follower"] = c
	h.mu.Unlock()
	started, finished := make(chan struct{}), make(chan *executor.Result, 1)
	joined := make(chan bool, 1)
	go func() {
		close(started)
		result, coalesced := h.doSingleflightExec("follower", func() *executor.Result {
			return &executor.Result{ExitCode: 1}
		})
		joined <- coalesced
		finished <- result
	}()
	<-started
	c.res = &executor.Result{Stdout: body}
	c.wg.Done()
	select {
	case result := <-finished:
		if !<-joined || !bytes.Equal(result.Stdout, body) {
			t.Fatal("follower did not join the published result")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follower did not complete")
	}
}

func TestUnionNormalizedPostInvalidateStartsNewFlight(t *testing.T) {
	h := newTestHandler()
	oldBody, newBody := captured(t, "pr21"), captured(t, "pr22")
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	h.execute = func(_ context.Context, _ string, _ []string, _ string, _ authenv.Environment) *executor.Result {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return &executor.Result{Stdout: oldBody}
		}
		return &executor.Result{Stdout: newBody}
	}
	req := &protocol.Request{Args: []string{"pr", "view", "21"}, Context: execctx.ExecContext{Host: "github.com", Repo: "brunoborges/ghx"}}
	key := execctx.CacheKey(req.Context, req.Args)
	oldDone := make(chan struct{})
	go func() {
		result, version, _ := h.doSingleflight(key, req.Args, req)
		h.store(key, "pr_view", allowlist.Classification{}, req, result, version)
		close(oldDone)
	}()
	<-started
	h.cache.Invalidate(func(*cache.Entry) bool { return true })
	type outcome struct {
		result    *executor.Result
		version   uint64
		coalesced bool
	}
	freshDone := make(chan outcome, 1)
	go func() {
		result, version, coalesced := h.doSingleflight(key, req.Args, req)
		freshDone <- outcome{result, version, coalesced}
	}()
	select {
	case fresh := <-freshDone:
		if fresh.coalesced || !bytes.Equal(fresh.result.Stdout, newBody) {
			close(release)
			<-oldDone
			t.Fatal("post-invalidation caller joined the old flight")
		}
		h.store(key, "pr_view", allowlist.Classification{}, req, fresh.result, fresh.version)
	case <-time.After(5 * time.Second):
		close(release)
		<-oldDone
		t.Fatal("post-invalidation caller waited for the old flight")
	}
	close(release)
	<-oldDone
	if entry := h.cache.Get(key); entry == nil || !bytes.Equal(entry.Stdout, newBody) || calls.Load() != 2 {
		t.Fatal("old flight replaced the fresh retained body")
	}
}

func TestUnionConditionalListInvalidationRefusesWithoutFallback(t *testing.T) {
	h := newTestHandler()
	h.cfg.ResourceViews = true
	req := &protocol.Request{Args: []string{"run", "list", "--json", "databaseId,status,conclusion", "-L", "1"},
		Context: execctx.ExecContext{Host: "github.com", Repo: "chriscoveries/cifast", TokenHash: "identity"}}
	s := resource.PrepareList(req.Context, req.Args)
	if s == nil {
		t.Fatal("retained list request has no resource shape")
	}
	body, err := translateRuns([]byte(runsCollection), strings.Split(resource.ListFields(s.Kind), ","))
	if err != nil {
		t.Fatal(err)
	}
	key := execctx.CacheKey(s.Context, s.Args)
	h.cache.Set(&cache.Entry{Key: key, Stdout: body, ETag: `"etag-1"`, CachedAt: time.Now().Add(-time.Hour), TTL: time.Minute})
	var calls atomic.Int32
	h.execute = func(_ context.Context, _ string, args []string, _ string, _ authenv.Environment) *executor.Result {
		calls.Add(1)
		if !strings.Contains(strings.Join(args, " "), "If-None-Match") {
			return &executor.Result{ExitCode: 1, Stderr: []byte("unexpected fallback")}
		}
		h.cache.Invalidate(func(*cache.Entry) bool { return true })
		return &executor.Result{Stdout: httpResponse("304 Not Modified", `"etag-1"`, "")}
	}
	result := h.Handle(req)
	if result == nil || result.ExitCode == 0 || result.Cached || calls.Load() != 1 || h.cache.Peek(key) != nil {
		t.Fatal("straddled 304 must refuse without returning, restoring, or refetching the old body")
	}
}

func TestUnionConditionalGETInvalidationRefusesWithoutFallback(t *testing.T) {
	h := newTestHandler()
	body := captured(t, "repository")
	req := &protocol.Request{Args: []string{"api", "repos/brunoborges/ghx"}, Context: execctx.ExecContext{Host: "github.com", Repo: "brunoborges/ghx", TokenHash: "identity"}}
	key := execctx.CacheKey(req.Context, req.Args)
	h.cache.Set(&cache.Entry{Key: key, Stdout: body, ETag: `W/"captured"`, TTL: time.Second, CachedAt: time.Now().Add(-time.Minute)})
	var calls atomic.Int32
	h.execute = func(_ context.Context, _ string, args []string, _ string, _ authenv.Environment) *executor.Result {
		calls.Add(1)
		if !strings.Contains(strings.Join(args, " "), "If-None-Match") {
			return &executor.Result{ExitCode: 1, Stderr: []byte("unexpected fallback")}
		}
		h.cache.Invalidate(func(*cache.Entry) bool { return true })
		return &executor.Result{Stdout: httpOutput(nil, 304, "")}
	}
	result := h.Handle(req)
	if result == nil || result.ExitCode == 0 || result.Cached || calls.Load() != 1 || h.cache.Peek(key) != nil {
		t.Fatal("straddled conditional GET must refuse without returning, restoring, or refetching the old body")
	}
}
