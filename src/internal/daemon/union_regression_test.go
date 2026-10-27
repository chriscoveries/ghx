package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
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

const recordedValidator = `W/"6329ed53d22e2529639603a464d9395ad859fc88b4ec0d6adb12335fba5bb5ce"`

func awaitSignal(t *testing.T, signal <-chan struct{}, phase string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", phase)
	}
}

// This real 304 was captured for issue #140, not the list or repository below.
// Tests use its protocol/exit bytes with a separately retained body solely as
// an explicit transport control; this is not a claimed live endpoint exchange.
func captured304(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../../testdata/api-throttle/issue140-conditional.http")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != "c4534bfdda0147cd87d4c47fe779172d4171b7754845e5bb3c6c1d8bfff0797e" {
		t.Fatal("retained 304 bytes changed")
	}
	return raw
}

// The bodies come from the public captured ghx repository fixture. Channels
// and invalidation are explicit schedule controls, not new API observations.
func TestUnionSingleflightLeaderAndFollower(t *testing.T) {
	h := newTestHandler()
	body := captured(t, "repository")
	started, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseLeader := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseLeader()
	type outcome struct {
		result *executor.Result
		shared bool
	}
	leaderDone, followerDone := make(chan outcome, 1), make(chan outcome, 1)
	var calls atomic.Int32
	execute := func() *executor.Result {
		calls.Add(1)
		close(started)
		<-release
		return &executor.Result{Stdout: body}
	}
	go func() {
		result, shared := h.doSingleflightExec("same-key", execute)
		leaderDone <- outcome{result, shared}
	}()
	awaitSignal(t, started, "singleflight leader start")
	go func() {
		result, shared := h.doSingleflightExecWithJoin("same-key", execute, func() { close(joined) })
		followerDone <- outcome{result, shared}
	}()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("follower did not join the blocked leader")
	}
	select {
	case <-followerDone:
		t.Fatal("follower completed before the leader was released")
	default:
	}
	releaseLeader()
	var leader, follower outcome
	select {
	case leader = <-leaderDone:
	case <-time.After(5 * time.Second):
		t.Fatal("leader did not complete")
	}
	select {
	case follower = <-followerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("follower did not complete")
	}
	if calls.Load() != 1 || leader.shared || !follower.shared || leader.result == nil || follower.result == nil ||
		!bytes.Equal(leader.result.Stdout, body) || !bytes.Equal(follower.result.Stdout, body) {
		t.Fatal("singleflight leader and joined follower did not share the retained result")
	}
}

func TestUnionNormalizedPostInvalidateStartsNewFlight(t *testing.T) {
	h := newTestHandler()
	oldBody, newBody := captured(t, "pr21"), captured(t, "pr22")
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseOld := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseOld()
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
	awaitSignal(t, started, "old normalized flight start")
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
			releaseOld()
			awaitSignal(t, oldDone, "old normalized flight cleanup")
			t.Fatal("post-invalidation caller joined the old flight")
		}
		h.store(key, "pr_view", allowlist.Classification{}, req, fresh.result, fresh.version)
	case <-time.After(5 * time.Second):
		releaseOld()
		awaitSignal(t, oldDone, "old normalized flight cleanup")
		t.Fatal("post-invalidation caller waited for the old flight")
	}
	releaseOld()
	awaitSignal(t, oldDone, "old normalized flight completion")
	if entry := h.cache.Get(key); entry == nil || !bytes.Equal(entry.Stdout, newBody) || calls.Load() != 2 {
		t.Fatal("old flight replaced the fresh retained body")
	}
}

func TestUnionCacheAdmissionPreservesForkExitRules(t *testing.T) {
	// Exit codes are transport controls on the retained body, not a claim that
	// the captured endpoint produced either exit status.
	body := captured(t, "pr21")
	for _, tc := range []struct {
		name       string
		args       []string
		exitCode   int
		shouldHold bool
	}{
		{"error is not stored", []string{"pr", "view", "21"}, 1, false},
		{"pending checks remain cacheable", []string{"pr", "checks", "21"}, 8, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler()
			var calls atomic.Int32
			h.execute = func(context.Context, string, []string, string, authenv.Environment) *executor.Result {
				calls.Add(1)
				return &executor.Result{Stdout: body, ExitCode: tc.exitCode}
			}
			req := &protocol.Request{Args: tc.args, Context: execctx.ExecContext{Host: "github.com", Repo: "brunoborges/ghx"}}
			first, second := h.Handle(req), h.Handle(req)
			if first.ExitCode != tc.exitCode || second.ExitCode != tc.exitCode || second.Cached != tc.shouldHold {
				t.Fatalf("responses = %+v, %+v", first, second)
			}
			want := int32(2)
			if tc.shouldHold {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("fetch calls = %d, want %d", calls.Load(), want)
			}
		})
	}
}

func TestUnionConditionalListInvalidationRefusesWithoutFallback(t *testing.T) {
	h := newTestHandler()
	h.cfg.ResourceViews = true
	response304 := captured304(t)
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
	h.cache.Set(&cache.Entry{Key: key, Stdout: body, ETag: recordedValidator, CachedAt: time.Now().Add(-time.Hour), TTL: time.Minute})
	var calls atomic.Int32
	h.execute = func(_ context.Context, _ string, args []string, _ string, _ authenv.Environment) *executor.Result {
		calls.Add(1)
		if !strings.Contains(strings.Join(args, " "), "If-None-Match: "+recordedValidator) {
			return &executor.Result{ExitCode: 1, Stderr: []byte("unexpected fallback")}
		}
		h.cache.Invalidate(func(*cache.Entry) bool { return true })
		return &executor.Result{Stdout: response304, ExitCode: 1}
	}
	result := h.Handle(req)
	if result == nil || result.ExitCode == 0 || result.Cached || calls.Load() != 1 || h.cache.Peek(key) != nil {
		t.Fatal("straddled 304 must refuse without returning, restoring, or refetching the old body")
	}
	if stats := h.stats.Snapshot(0, 100); stats.Total != 1 || stats.Misses != 1 || stats.Revalidated != 0 {
		t.Fatalf("refused list revalidation metrics = %+v", stats)
	}
}

func TestUnionConditionalGETInvalidationRefusesWithoutFallback(t *testing.T) {
	h := newTestHandler()
	body := captured(t, "repository")
	response304 := captured304(t)
	req := &protocol.Request{Args: []string{"api", "repos/brunoborges/ghx"}, Context: execctx.ExecContext{Host: "github.com", Repo: "brunoborges/ghx", TokenHash: "identity"}}
	key := execctx.CacheKey(req.Context, req.Args)
	h.cache.Set(&cache.Entry{Key: key, Stdout: body, ETag: recordedValidator, TTL: time.Second, CachedAt: time.Now().Add(-time.Minute)})
	var calls atomic.Int32
	h.execute = func(_ context.Context, _ string, args []string, _ string, _ authenv.Environment) *executor.Result {
		calls.Add(1)
		if !strings.Contains(strings.Join(args, " "), "If-None-Match: "+recordedValidator) {
			return &executor.Result{ExitCode: 1, Stderr: []byte("unexpected fallback")}
		}
		h.cache.Invalidate(func(*cache.Entry) bool { return true })
		return &executor.Result{Stdout: response304, ExitCode: 1}
	}
	result := h.Handle(req)
	if result == nil || result.ExitCode == 0 || result.Cached || calls.Load() != 1 || h.cache.Peek(key) != nil {
		t.Fatal("straddled conditional GET must refuse without returning, restoring, or refetching the old body")
	}
	if stats := h.stats.Snapshot(0, 100); stats.Total != 1 || stats.Misses != 1 || stats.Revalidated != 0 {
		t.Fatalf("refused GET revalidation metrics = %+v", stats)
	}
}

func TestUnionConditionalListAcceptsObservedGh304Exit(t *testing.T) {
	h := newTestHandler()
	h.cfg.ResourceViews = true
	response304 := captured304(t)
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
	want, err := s.Project(body)
	if err != nil {
		t.Fatal(err)
	}
	key := execctx.CacheKey(s.Context, s.Args)
	h.cache.Set(&cache.Entry{Key: key, Stdout: body, ETag: recordedValidator, CachedAt: time.Now().Add(-time.Hour), TTL: time.Minute})
	var calls atomic.Int32
	h.execute = func(_ context.Context, _ string, args []string, _ string, _ authenv.Environment) *executor.Result {
		calls.Add(1)
		if !strings.Contains(strings.Join(args, " "), "If-None-Match: "+recordedValidator) {
			return &executor.Result{ExitCode: 1, Stderr: []byte("unexpected fallback")}
		}
		return &executor.Result{Stdout: response304, ExitCode: 1}
	}
	result := h.Handle(req)
	if result == nil || result.ExitCode != 0 || !result.Cached || !bytes.Equal(result.Stdout, want) || calls.Load() != 1 {
		t.Fatal("observed gh exit 1 with a 304 should refresh the matching retained body without fallback")
	}
	if stats := h.stats.Snapshot(1, 100); stats.Total != 1 || stats.Revalidated != 1 {
		t.Fatalf("successful list revalidation metrics = %+v", stats)
	}
}
