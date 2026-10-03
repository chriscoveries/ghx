package governor

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixture(t *testing.T) []byte {
	b, e := os.ReadFile("../../../testdata/pr-description.json")
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func setup(t *testing.T) *Gate {
	g, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	g.Now = func() time.Time { return time.Unix(10000, 0) }
	return g
}
func req(method, path string) *http.Request {
	r, _ := http.NewRequest(method, "https://api.github.com"+path, nil)
	r.Header.Set("Authorization", "Bearer fixture-owner")
	return r
}
func response(code int, b []byte, h http.Header) *http.Response {
	return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(bytes.NewReader(b)), ContentLength: int64(len(b))}
}

var caller = Caller{Lane: "codex-A", Cwd: "fixture", Class: "owner"}

func content(t *testing.T, r *http.Response) []byte {
	t.Helper()
	defer r.Body.Close()
	b, e := io.ReadAll(r.Body)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestETag304AndWrites(t *testing.T) {
	g := setup(t)
	b := fixture(t)
	calls := 0
	g.Transport = transport(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 2 {
			if r.Header.Get("If-None-Match") != `"fixture"` {
				t.Fatal("missing conditional header")
			}
			return response(304, nil, http.Header{"X-Ratelimit-Remaining": []string{"1000"}, "X-Ratelimit-Reset": []string{"11000"}}), nil
		}
		return response(200, b, http.Header{"Etag": []string{`"fixture"`}}), nil
	})
	for i := 0; i < 2; i++ {
		r := g.RoundTrip(req("GET", "/repos/o/r/pulls/479"), caller)
		if r.StatusCode != 200 || !bytes.Equal(content(t, r), b) {
			t.Fatal("304 failed to restore captured body")
		}
	}
	if g.State.Counts["304"] != 1 || len(g.State.Calls["codex-A"]) != 1 {
		t.Fatal(g.State)
	}
	for i := 0; i < 2; i++ {
		r := g.RoundTrip(req("POST", "/repos/o/r/issues/479/comments"), caller)
		if r.StatusCode != 200 {
			t.Fatal(r.StatusCode)
		}
		content(t, r)
	}
	if calls != 4 {
		t.Fatal("write cached")
	}
	files, _ := filepath.Glob(filepath.Join(g.Dir, "rest", "*.json"))
	if len(files) != 0 {
		t.Fatal("write didn't invalidate REST entries")
	}
}
func TestFloorStaleAndPersistent403(t *testing.T) {
	g := setup(t)
	b := fixture(t)
	calls := 0
	g.Transport = transport(func(r *http.Request) (*http.Response, error) {
		calls++
		return response(200, b, http.Header{"Etag": []string{`"fixture"`}, "X-Ratelimit-Remaining": []string{"499"}, "X-Ratelimit-Reset": []string{"11000"}}), nil
	})
	content(t, g.RoundTrip(req("GET", "/repos/o/r/pulls/479"), caller))
	s := g.RoundTrip(req("GET", "/repos/o/r/pulls/479"), caller)
	if s.Header.Get("X-Ghgate-Stale") != "true" || !bytes.Equal(content(t, s), b) {
		t.Fatal("floor failed to serve stale")
	}
	r := g.RoundTrip(req("GET", "/repos/o/r/pulls/480"), caller)
	message := string(content(t, r))
	if r.StatusCode != 403 || !strings.Contains(message, "03:03:20") {
		t.Fatal("floor refusal lacks reset", message)
	}
	content(t, g.RoundTrip(req("POST", "/repos/o/r/comments"), caller))
	if calls != 2 {
		t.Fatal("floor blocked write or sent read")
	}
	g.State.Rates = map[string]Rate{}
	g.Transport = transport(func(r *http.Request) (*http.Response, error) {
		calls++
		return response(403, []byte(`{"message":"API rate limit exceeded"}`), http.Header{"X-Ratelimit-Remaining": []string{"0"}, "X-Ratelimit-Reset": []string{"11000"}}), nil
	})
	r = g.RoundTrip(req("GET", "/fresh"), caller)
	if r.StatusCode != 403 {
		t.Fatal(r.StatusCode)
	}
	content(t, r)
	before := calls
	restarted, e := New(g.Dir)
	if e != nil {
		t.Fatal(e)
	}
	restarted.Now = g.Now
	restarted.Transport = g.Transport
	for _, method := range []string{"GET", "POST"} {
		r := restarted.RoundTrip(req(method, "/graphql"), caller)
		if r.StatusCode != 403 {
			t.Fatal("block didn't apply across categories")
		}
		content(t, r)
	}
	if calls != before {
		t.Fatal("blocked request retried")
	}
	state, _ := os.ReadFile(filepath.Join(g.Dir, "fleet-status.json"))
	if !bytes.Contains(state, []byte("GITHUB API BLOCKED")) {
		t.Fatal("alarm missing")
	}
}
func TestQueryClassificationAndIdentity(t *testing.T) {
	for _, s := range []string{`{"query":"query Q { viewer { login } }"}`, `{"query":"#comment\n{viewer{login}}"}`} {
		if !ReadQuery([]byte(s)) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{`{"query":"mutation { addComment(input:{}){id} }"}`, `{"query":"{viewer{login}} mutation {x}"}`, `{"query":"query {x} mutation {y}"}`, `{"query":"subscription {x}"}`} {
		if ReadQuery([]byte(s)) {
			t.Fatal("unsafe query", s)
		}
	}
	g := setup(t)
	g.Policy.Classes["automation"] = Class{Identity: "machine", Hourly: 3}
	g.Policy.Tokens["machine"] = "GHGATE_TEST_TOKEN"
	t.Setenv("GHGATE_TEST_TOKEN", "fixture-machine")
	b := fixture(t)
	g.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer fixture-machine" {
			t.Fatal("wrong identity")
		}
		return response(200, b, http.Header{}), nil
	})
	for i := 0; i < 2; i++ {
		content(t, g.RoundTrip(req("POST", "/comments"), Caller{Class: "automation", Lane: "CI"}))
	}
}
func TestCallerBudgetAndOutage(t *testing.T) {
	g := setup(t)
	g.Policy.Classes["owner"] = Class{Identity: "owner", Hourly: 1}
	g.Transport = transport(func(r *http.Request) (*http.Response, error) {
		return response(200, fixture(t), http.Header{"Etag": []string{`"a"`}}), nil
	})
	content(t, g.RoundTrip(req("GET", "/x"), caller))
	r := g.RoundTrip(req("GET", "/new"), caller)
	if r.StatusCode != 403 {
		t.Fatal("caller budget not enforced")
	}
	content(t, r)
	g.Policy.Classes["owner"] = Class{Identity: "owner", Hourly: 150}
	g.Transport = transport(func(r *http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF })
	r = g.RoundTrip(req("GET", "/x"), caller)
	if r.Header.Get("X-Ghgate-Stale") != "true" {
		t.Fatal("outage didn't serve stale")
	}
	content(t, r)
}
func BenchmarkLocalMiss(b *testing.B) {
	g, e := New(b.TempDir())
	if e != nil {
		b.Fatal(e)
	}
	g.Policy.Classes["owner"] = Class{Identity: "owner", Hourly: 1000000}
	payload, _ := os.ReadFile("../../../testdata/pr-description.json")
	g.Transport = transport(func(r *http.Request) (*http.Response, error) { return response(200, payload, http.Header{}), nil })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := g.RoundTrip(req("GET", "/fresh?"+strconv.Itoa(i)), caller)
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}
}
func TestRateLimitEndpointCannotClearState(t *testing.T) {
	g := setup(t)
	g.State.Rates["owner/core"] = Rate{Remaining: 500, Reset: 11000}
	g.Transport = transport(func(r *http.Request) (*http.Response, error) {
		return response(200, []byte(`{}`), http.Header{"X-Ratelimit-Remaining": []string{"5000"}, "X-Ratelimit-Reset": []string{"15000"}}), nil
	})
	content(t, g.RoundTrip(req("GET", "/rate_limit"), caller))
	if g.State.Rates["owner/core"].Remaining != 500 {
		t.Fatal("trusted rate_limit")
	}
}
func TestGraphQLReadArgsRejectsFilesAndMutations(t *testing.T) {
	for _, a := range [][]string{{"api", "graphql", "-f", "query=query Q {viewer{login}}"}, {"api", "graphql", "--method=POST", "-fquery={viewer{login}}"}} {
		if !ReadArgs(a) {
			t.Fatal(a)
		}
	}
	for _, a := range [][]string{{"api", "graphql", "-fquery=mutation {x}"}, {"api", "graphql", "-Fquery=@file"}, {"api", "graphql", "--input=file"}, {"api", "graphql", "-XDELETE", "-fquery={viewer{login}}"}} {
		if ReadArgs(a) {
			t.Fatal("unsafe query", a)
		}
	}
}
func TestWritePurposeMetadataSurvivesFloor(t *testing.T) {
	g := setup(t)
	g.State.Rates["owner/graphql"] = Rate{Remaining: 499, Reset: 11000}
	calls := 0
	g.Transport = transport(func(r *http.Request) (*http.Response, error) {
		calls++
		return response(200, fixture(t), http.Header{}), nil
	})
	r := req("POST", "/graphql")
	r.Body = io.NopCloser(strings.NewReader(`{"query":"query {viewer{login}}"}`))
	c := caller
	c.Essential = true
	v := g.RoundTrip(r, c)
	if v.StatusCode != 200 || calls != 1 {
		t.Fatal("metadata needed by a write was blocked")
	}
	content(t, v)
}
func TestNamedGraphQLOperationsAndQuotedMutation(t *testing.T) {
	for _, s := range []string{`{"query":"query { repository(owner:\"mutation\",name:\"r\"){name}}"}`, `{"query":"fragment F on User {login} query Q {viewer{...F}}"}`, `{"query":"query Read {viewer{login}} mutation Write {x}","operationName":"Read"}`} {
		if !ReadQuery([]byte(s)) {
			t.Fatal(s)
		}
	}
	if ReadQuery([]byte(`{"query":"query Read {viewer{login}} mutation Write {x}","operationName":"Write"}`)) {
		t.Fatal("selected mutation cached")
	}
}

func TestRESTCacheBoundAndAmbiguousWriteInvalidation(t *testing.T) {
	g := setup(t)
	g.Policy.Classes["owner"] = Class{Identity: "owner", Hourly: 1000}
	g.Transport = transport(func(r *http.Request) (*http.Response, error) {
		return response(200, fixture(t), http.Header{"Etag": []string{`"bounded"`}}), nil
	})
	for n := 0; n < 130; n++ {
		content(t, g.RoundTrip(req("GET", "/x?"+strconv.Itoa(n)), caller))
	}
	files, _ := filepath.Glob(filepath.Join(g.Dir, "rest", "*.json"))
	if len(files) != 128 {
		t.Fatal("unbounded cache", len(files))
	}
	g.Transport = transport(func(r *http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF })
	content(t, g.RoundTrip(req("POST", "/write"), caller))
	files, _ = filepath.Glob(filepath.Join(g.Dir, "rest", "*.json"))
	if len(files) != 0 {
		t.Fatal("ambiguous write retained stale state")
	}
}

func TestRepeatedGraphQLReadAndRemainingZero(t *testing.T) {
	g := setup(t)
	calls := 0
	g.Transport = transport(func(r *http.Request) (*http.Response, error) {
		calls++
		return response(200, fixture(t), http.Header{}), nil
	})
	for n := 0; n < 4; n++ {
		r := req("POST", "/graphql")
		r.Body = io.NopCloser(strings.NewReader(`{"query":"query {viewer{login}}"}`))
		v := g.RoundTrip(r, caller)
		content(t, v)
		if n == 3 && v.StatusCode != 403 {
			t.Fatal("repeated read went upstream")
		}
	}
	if calls != 3 {
		t.Fatal(calls)
	}
	g.Transport = transport(func(r *http.Request) (*http.Response, error) {
		calls++
		return response(200, fixture(t), http.Header{"X-Ratelimit-Remaining": []string{"0"}, "X-Ratelimit-Reset": []string{"11000"}}), nil
	})
	content(t, g.RoundTrip(req("GET", "/fresh-zero"), caller))
	before := calls
	v := g.RoundTrip(req("POST", "/write"), caller)
	content(t, v)
	if v.StatusCode != 403 || calls != before {
		t.Fatal("zero budget allowed a write")
	}
}

func TestBoxBudgetAndFreeRateLimit(t *testing.T) {
	g := setup(t)
	g.Transport = transport(func(r *http.Request) (*http.Response, error) { return response(200, fixture(t), http.Header{}), nil })
	g.Policy.BoxHourly = 1
	first := g.RoundTrip(req("GET", "/repos/chriscoveries/ghx/pulls/21"), Caller{Lane: "runner-a", Class: "automation"})
	first.Body.Close()
	second := g.RoundTrip(req("GET", "/repos/chriscoveries/ghx/pulls/22"), Caller{Lane: "runner-b", Class: "automation"})
	defer second.Body.Close()
	if second.StatusCode != 403 {
		t.Fatal("shared box budget bypassed", second.StatusCode)
	}
	before := g.State.Counts["upstream"]
	free := g.RoundTrip(req("GET", "/rate_limit"), Caller{Lane: "measurement", Class: "automation"})
	free.Body.Close()
	if free.StatusCode != 200 || g.State.Counts["upstream"] != before {
		t.Fatal("free measurement budgeted")
	}
}
