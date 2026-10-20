package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brunoborges/ghx/src/internal/authenv"
	execctx "github.com/brunoborges/ghx/src/internal/context"
	"github.com/brunoborges/ghx/src/internal/executor"
	"github.com/brunoborges/ghx/src/internal/protocol"
	"github.com/brunoborges/ghx/src/internal/resource"
)

const runsCollection = `{"total_count":2,"workflow_runs":[` +
	`{"id":39434723691,"run_number":363,"display_title":"admission: a broker pause is a wait (#171)","head_branch":"main","head_sha":"8db74d5c","event":"push","status":"in_progress","conclusion":null,"created_at":"2026-10-20T13:56:07Z","run_started_at":"2026-10-20T13:56:07Z","updated_at":"2026-10-20T13:56:22Z","html_url":"https://github.com/chriscoveries/cifast/actions/runs/39434723691","name":"cifast","workflow_id":387289233},` +
	`{"id":39433383865,"run_number":362,"display_title":"fix(runtime): allow unshare (#169)","head_branch":"main","head_sha":"41f0aaf7","event":"push","status":"completed","conclusion":"success","created_at":"2026-10-20T13:46:57Z","run_started_at":"2026-10-20T13:46:57Z","updated_at":"2026-10-20T13:51:01Z","html_url":"https://github.com/chriscoveries/cifast/actions/runs/39433383865","name":"cifast","workflow_id":387289233}]}`

func httpResponse(status, etag, body string) []byte {
	return []byte("HTTP/2.0 " + status + "\r\nETag: " + etag + "\r\nContent-Type: application/json\r\n\r\n" + body)
}

func TestConditionalListRevalidatesAnExpiredBody(t *testing.T) {
	h := newTestHandler()
	h.cfg.ResourceViews = true
	var mu sync.Mutex
	fetches := 0
	conditional := 0
	h.execute = func(_ context.Context, _ string, args []string, _ string, _ authenv.Environment) *executor.Result {
		mu.Lock()
		defer mu.Unlock()
		if args[0] != "api" {
			t.Errorf("unexpected command %v", args)
			return &executor.Result{ExitCode: 1}
		}
		for i, a := range args {
			if a == "--header" && i+1 < len(args) && strings.Contains(args[i+1], "If-None-Match") {
				conditional++
			}
		}
		if conditional > 0 {
			return &executor.Result{Stdout: httpResponse("304 Not Modified", `"etag-1"`, "")}
		}
		fetches++
		return &executor.Result{Stdout: httpResponse("200 OK", `"etag-1"`, runsCollection)}
	}
	req := &protocol.Request{
		Args:    []string{"run", "list", "--json", "databaseId,status,conclusion", "-L", "1"},
		Context: execctx.ExecContext{Host: "github.com", Repo: "chriscoveries/cifast", TokenHash: "identity"},
	}
	first := h.Handle(req)
	if first.Cached {
		t.Fatal("the first collection read cannot be a hit")
	}
	var rows []map[string]any
	if err := json.Unmarshal(first.Stdout, &rows); err != nil {
		t.Fatalf("first read is not JSON: %s", first.Stdout)
	}
	if len(rows) != 1 || len(rows[0]) != 3 {
		t.Fatalf("projection = %v", rows)
	}
	if rows[0]["conclusion"] != "" {
		t.Fatalf("a null conclusion must read as empty: %v", rows[0]["conclusion"])
	}
	shape := resource.PrepareList(req.Context, req.Args)
	if shape == nil {
		t.Fatal("shape expected")
	}
	key := execctx.CacheKey(shape.Context, shape.Args)
	entry := h.cache.Peek(key)
	if entry == nil || entry.ETag != `"etag-1"` {
		t.Fatalf("validator not stored: %+v", entry)
	}
	copyEntry := *entry
	copyEntry.CachedAt = time.Now().Add(-time.Hour)
	h.cache.Set(&copyEntry)

	second := h.Handle(req)
	mu.Lock()
	defer mu.Unlock()
	if !second.Cached {
		t.Fatal("a revalidated read is served from the cache")
	}
	if fetches != 1 || conditional != 1 {
		t.Fatalf("fetches=%d conditional=%d, want 1 and 1", fetches, conditional)
	}
	if string(second.Stdout) != string(first.Stdout) {
		t.Fatalf("revalidated body changed: %s vs %s", second.Stdout, first.Stdout)
	}
	if got := h.stats.Snapshot(1, 1).Revalidated; got != 1 {
		t.Fatalf("revalidated counter = %d, want 1", got)
	}
	if refreshed := h.cache.Peek(key); refreshed.IsExpired() {
		t.Fatal("a 304 must restart the entry's age")
	}
}

func TestConditionalListFallsBackForUnmappedFilters(t *testing.T) {
	shape := resource.PrepareList(execctx.ExecContext{Host: "github.com", Repo: "chriscoveries/cifast"}, []string{"run", "list", "--json", "databaseId", "--user", "someone"})
	if shape == nil {
		t.Fatal("shape expected")
	}
	if _, ok := conditionalRunsPath(shape); ok {
		t.Fatal("a filter without a collection parameter must keep native execution")
	}
}

func TestTranslateRunsKeepsEveryFieldForLaterProjections(t *testing.T) {
	out, err := translateRuns([]byte(runsCollection), strings.Split(resource.ListFields("run"), ","))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	for _, f := range strings.Split(resource.ListFields("run"), ",") {
		if _, ok := rows[0][f]; !ok {
			t.Fatalf("translated body lost %s", f)
		}
	}
	if string(rows[0]["databaseId"]) != "39434723691" {
		t.Fatalf("id lost precision: %s", rows[0]["databaseId"])
	}
}
