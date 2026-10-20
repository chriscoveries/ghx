package resource

import (
	"encoding/json"
	"testing"

	execctx "github.com/brunoborges/ghx/src/internal/context"
)

func listContext() execctx.ExecContext {
	return execctx.ExecContext{Host: "github.com", Repo: "chriscoveries/cifast", Branch: "one", TokenHash: "identity"}
}

func TestPrepareListSharesOneKeyAcrossCallerShapes(t *testing.T) {
	a := PrepareList(listContext(), []string{"run", "list", "-L", "2", "--json", "databaseId,status"})
	b := PrepareList(listContext(), []string{"run", "list", "--json", "status,databaseId", "--limit", "5"})
	if a == nil || b == nil {
		t.Fatal("both list reads should get a shape")
	}
	if got, want := len(a.Args), len(b.Args); got != want {
		t.Fatalf("arg lengths differ: %v vs %v", a.Args, b.Args)
	}
	for i := range a.Args {
		if a.Args[i] != b.Args[i] {
			t.Fatalf("keys diverge at %d: %v vs %v", i, a.Args, b.Args)
		}
	}
	if a.Limit != 2 || b.Limit != 5 {
		t.Fatalf("caller limits not retained: %d %d", a.Limit, b.Limit)
	}
}

func TestPrepareListFetchesTheSupersetOnce(t *testing.T) {
	s := PrepareList(listContext(), []string{"run", "list", "--status", "failure", "--event", "pull_request", "-L", "3", "--json", "databaseId"})
	if s == nil {
		t.Fatal("shape expected")
	}
	want := []string{"run", "list", "--repo", "github.com/chriscoveries/cifast", "--limit", "100", "--json", policies["run"].ListFields,
		"--event", "pull_request", "--status", "failure"}
	if len(s.Args) != len(want) {
		t.Fatalf("args = %v, want %v", s.Args, want)
	}
	for i := range want {
		if s.Args[i] != want[i] {
			t.Fatalf("args = %v, want %v", s.Args, want)
		}
	}
	if s.Context.Branch != "" {
		t.Fatal("collection reads must not carry the caller's branch")
	}
}

func TestProjectListNarrowsRowsAndFields(t *testing.T) {
	s := PrepareList(listContext(), []string{"run", "list", "--json", "databaseId,status", "-L", "2"})
	if s == nil {
		t.Fatal("shape expected")
	}
	body := []byte(`[{"databaseId":1,"status":"completed","conclusion":"success"},{"databaseId":2,"status":"in_progress","conclusion":""},{"databaseId":3,"status":"queued","conclusion":""}]`)
	if !s.Valid(body) {
		t.Fatal("superset body should be valid")
	}
	out, err := s.Project(body)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if len(row) != 2 {
			t.Fatalf("projected fields = %v", row)
		}
		if _, ok := row["conclusion"]; ok {
			t.Fatalf("unrequested field survived projection: %v", row)
		}
	}
}

func TestProjectListDeclinesOrErrorsRatherThanGuessing(t *testing.T) {
	cases := []struct {
		name string
		args []string
		body string
	}{
		{"template", []string{"run", "list", "--json", "status", "--template", "{{.status}}"}, ``},
		{"unknown flag", []string{"run", "list", "--json", "status", "--watch"}, ``},
		{"positional", []string{"run", "list", "12345", "--json", "status"}, ``},
		{"more rows than the superset", []string{"run", "list", "--json", "status", "-L", "101"}, ``},
		{"field the command cannot emit", []string{"run", "list", "--json", "jobs"}, ``},
		{"jq without json", []string{"run", "list", "--jq", ".status"}, ``},
		{"repeated flag", []string{"run", "list", "--json", "status", "--json", "conclusion"}, ``},
		{"dangling flag", []string{"run", "list", "--json"}, ``},
	}
	for _, tt := range cases {
		if s := PrepareList(listContext(), tt.args); s != nil {
			t.Fatalf("%s: shape should decline, got %v", tt.name, s.Args)
		}
	}
	s := PrepareList(listContext(), []string{"run", "list", "--json", "status,conclusion"})
	if s == nil {
		t.Fatal("shape expected")
	}
	if _, err := s.Project([]byte(`[{"status":"completed"}]`)); err == nil {
		t.Fatal("a superset missing a requested field must not be projected")
	}
}

func TestTextListReadsKeepTheCallerLimit(t *testing.T) {
	s := PrepareList(listContext(), []string{"run", "list", "-L", "5"})
	if s == nil {
		t.Fatal("shape expected")
	}
	if s.Limit != 5 || len(s.Fields) != 0 {
		t.Fatalf("text shape = %+v", s)
	}
	want := []string{"run", "list", "--repo", "github.com/chriscoveries/cifast", "--limit", "5"}
	for i, w := range want {
		if s.Args[i] != w {
			t.Fatalf("text args = %v, want %v", s.Args, want)
		}
	}
	body := []byte("STATUS  TITLE\ncompleted  gate\n")
	out, err := s.Project(body)
	if err != nil || string(out) != string(body) {
		t.Fatalf("text projection altered the answer: %q %v", out, err)
	}
}
