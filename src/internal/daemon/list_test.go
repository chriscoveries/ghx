package daemon

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brunoborges/ghx/src/internal/authenv"
	execctx "github.com/brunoborges/ghx/src/internal/context"
	"github.com/brunoborges/ghx/src/internal/executor"
	"github.com/brunoborges/ghx/src/internal/protocol"
)

// Two lanes reading the same run list with different projections must cost one
// fetch, not one each.
func listHandler(t *testing.T) (*Handler, *[]string, *sync.Mutex) {
	t.Helper()
	h := newTestHandler()
	h.cfg.ResourceViews = true
	h.cfg.ImmutableTTL = 24 * time.Hour
	body := []byte(`[{"databaseId":11,"status":"completed","conclusion":"success","headBranch":"main"},{"databaseId":12,"status":"in_progress","conclusion":"","headBranch":"main"}]`)
	calls := &[]string{}
	mu := &sync.Mutex{}
	h.execute = func(_ context.Context, _ string, args []string, _ string, _ authenv.Environment) *executor.Result {
		mu.Lock()
		*calls = append(*calls, strings.Join(args, " "))
		mu.Unlock()
		switch args[0] {
		case "run":
			return &executor.Result{Stdout: body}
		case "api":
			// The local jq renderer answers from the projected bytes.
			return &executor.Result{Stdout: []byte("11\n")}
		}
		return &executor.Result{}
	}
	return h, calls, mu
}

func listReq(args ...string) *protocol.Request {
	return &protocol.Request{Args: args, Context: execctx.ExecContext{RepoBound: true, Host: "github.com", Repo: "chriscoveries/cifast", Branch: "one", TokenHash: "identity"}}
}

func runListCalls(calls []string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "run list") {
			n++
		}
	}
	return n
}

func TestListReadsShareOneSupersetFetch(t *testing.T) {
	h, calls, mu := listHandler(t)

	first := h.Handle(listReq("run", "list", "-L", "1", "--json", "databaseId,status"))
	if first.Cached {
		t.Fatal("the first read cannot be a hit")
	}
	second := h.Handle(listReq("run", "list", "--json", "status", "--limit", "2"))
	third := h.Handle(listReq("run", "list", "--json", "databaseId"))

	mu.Lock()
	defer mu.Unlock()
	if got := runListCalls(*calls); got != 1 {
		t.Fatalf("run list executed %d times, want 1 (%v)", got, *calls)
	}
	if !second.Cached || !third.Cached {
		t.Fatalf("later projections missed: second=%+v third=%+v", second, third)
	}
	if strings.Contains(string(second.Stdout), "conclusion") {
		t.Fatalf("unrequested field served: %s", second.Stdout)
	}
}

func TestListJQIsRenderedLocally(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the local jq renderer is Unix-only, so the shape declines there")
	}
	h, calls, mu := listHandler(t)
	h.Handle(listReq("run", "list", "--json", "databaseId,status"))
	out := h.Handle(listReq("run", "list", "--json", "databaseId", "--jq", ".[].databaseId"))
	if !out.Cached {
		t.Fatalf("jq read should reuse the superset: %+v", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := runListCalls(*calls); got != 1 {
		t.Fatalf("run list executed %d times for a jq read (%v)", got, *calls)
	}
}

func TestTextListReadIsKeyedOnCanonicalArgs(t *testing.T) {
	h, calls, mu := listHandler(t)
	h.Handle(listReq("run", "list", "-L", "2", "--status", "completed"))
	second := h.Handle(listReq("run", "list", "--status", "completed", "--limit", "2"))
	mu.Lock()
	defer mu.Unlock()
	if !second.Cached {
		t.Fatalf("reordered flags missed: %+v", second)
	}
	if got := runListCalls(*calls); got != 1 {
		t.Fatalf("run list executed %d times, want 1 (%v)", got, *calls)
	}
}
