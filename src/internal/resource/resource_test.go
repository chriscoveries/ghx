package resource

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	execctx "github.com/brunoborges/ghx/src/internal/context"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../testdata/resource/pr21.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestEquivalentViewsAndBoundaries(t *testing.T) {
	ctx := execctx.ExecContext{Host: "github.com", Repo: "brunoborges/ghx", Branch: "topic", TokenHash: "one"}
	a := Prepare(ctx, []string{"pr", "view", "21", "--json", "title,number"})
	b := Prepare(ctx, []string{"pr", "view", "https://github.com/brunoborges/ghx/pull/21", "--json=number,body", "-R", "BRUNOBORGES/GHX"})
	if a == nil || b == nil {
		t.Fatal("numeric/URL views not prepared")
	}
	if execctx.CacheKey(a.Context, a.Args) != execctx.CacheKey(b.Context, b.Args) {
		t.Fatal("field selections/URL/repository spelling fragmented resource key")
	}
	for _, change := range []func(*execctx.ExecContext){func(c *execctx.ExecContext) { c.TokenHash = "two" }, func(c *execctx.ExecContext) { c.Host = "enterprise.example" }, func(c *execctx.ExecContext) { c.Repo = "other/repo" }} {
		other := ctx
		change(&other)
		s := Prepare(other, []string{"pr", "view", "21", "--json", "number"})
		if s == nil || execctx.CacheKey(a.Context, a.Args) == execctx.CacheKey(s.Context, s.Args) {
			t.Fatal("identity boundary collapsed")
		}
	}
	bad := [][]string{
		{"pr", "view", "21", "--json", "title", "--json", "number"},
		{"pr", "view", "topic", "--json", "number"}, {"pr", "view", "--json", "number"},
		{"pr", "view", "21", "--json", "unknown"}, {"pr", "view", "21", "--json", "number", "--web"},
		{"pr", "view", "https://enterprise.example/o/r/pull/21", "--json", "number"},
		{"pr", "view", "21", "--json", "number", "--template", "{{.number}}"},
	}
	for _, args := range bad {
		if Prepare(ctx, args) != nil {
			t.Errorf("unsafe selector prepared: %v", args)
		}
	}
}
func TestProjectionAndImmutableMask(t *testing.T) {
	ctx := execctx.ExecContext{Host: "github.com", Repo: "brunoborges/ghx"}
	s := Prepare(ctx, []string{"pr", "view", "21", "--json", "number,mergeCommit,mergedAt"})
	b := fixture(t)
	if !s.Valid(b) || !s.Immutable(b) {
		t.Fatal("captured merged PR fixed fields not reusable")
	}
	out, err := s.Project(b)
	if err != nil {
		t.Fatal(err)
	}
	var original, got map[string]json.RawMessage
	json.Unmarshal(b, &original)
	json.Unmarshal(out, &got)
	if len(got) != 3 || !reflect.DeepEqual(got["mergeCommit"], original["mergeCommit"]) {
		t.Fatalf("projection changed captured values: %s", out)
	}
	s.Fields = []string{"title"}
	if s.Immutable(b) {
		t.Fatal("merged PR title remains editable")
	}
	s.Fields = []string{"missing"}
	if _, err := s.Project(b); err == nil {
		t.Fatal("missing field accepted")
	}
	// State-only probes exercise the freshness rule; response content comes from captures.
	run := Prepare(ctx, []string{"run", "view", "33885950671", "--attempt", "1", "--json", "conclusion"})
	latest := Prepare(ctx, []string{"run", "view", "33885950671", "--json", "conclusion"})
	if !run.Immutable([]byte(`{"status":"completed"}`)) || latest.Immutable([]byte(`{"status":"completed"}`)) || run.Immutable([]byte(`{"status":"in_progress"}`)) {
		t.Fatal("run attempts were not distinguished from rerunnable IDs")
	}
	run2 := Prepare(ctx, []string{"run", "view", "33885950671", "-a", "2", "--json", "status"})
	if reflect.DeepEqual(run.Args, run2.Args) {
		t.Fatal("different attempts share identity")
	}
}
func TestAPIMethodParsing(t *testing.T) {
	cases := []struct {
		args        []string
		method      string
		conditional bool
	}{
		{[]string{"api", "repos/o/r/pulls/1"}, "GET", true},
		{[]string{"api", "--method=PATCH", "repos/o/r/pulls/1"}, "PATCH", false},
		{[]string{"api", "-XDELETE", "repos/o/r/pulls/1"}, "DELETE", false},
		{[]string{"api", "repos/o/r/issues", "-f", "title=a"}, "POST", false},
		{[]string{"api", "repos/o/r/issues", "--field=state=open", "-X", "GET"}, "GET", false},
		{[]string{"api", "graphql", "-f", "query=mutation{}"}, "GRAPHQL", false},
		{[]string{"api", "repos/o/r", "-H", "If-None-Match: tag"}, "GET", false},
		{[]string{"api", "repos/o/r", "--paginate"}, "GET", false},
	}
	for _, c := range cases {
		a := ParseAPI(c.args)
		if !a.Valid || a.Method != c.method || a.Conditional != c.conditional {
			t.Errorf("%v: %+v", c.args, a)
		}
	}
	if ParseAPI([]string{"api", "repos/o/r", "-X"}).Valid {
		t.Fatal("missing method value accepted")
	}
}
func TestDependencyIdentity(t *testing.T) {
	ctx := execctx.ExecContext{Host: "github.com", Repo: "default/repo"}
	for _, args := range [][]string{
		{"pr", "edit", "21", "-R", "BRUNOBORGES/GHX"},
		{"pr", "edit", "https://github.com/brunoborges/ghx/pull/21"},
		{"api", "--method=PATCH", "repos/brunoborges/ghx/pulls/21"},
	} {
		if got := Identify(ctx, args); got != (Identity{"github.com", "brunoborges/ghx", "pr", "21"}) {
			t.Errorf("%v: %+v", args, got)
		}
	}
	if got := Identify(ctx, []string{"repo", "edit", "brunoborges/ghx"}); got.Repo != "brunoborges/ghx" {
		t.Fatal("explicit repository edit scoped to caller repo")
	}
}
