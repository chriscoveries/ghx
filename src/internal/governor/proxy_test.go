//go:build ghintegration

package governor

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeGHConnectProxyPreservesHostAuthAndFramesHTTP2Body(t *testing.T) {
	if _, err := os.Stat("/usr/bin/gh"); err != nil {
		t.Skip("native gh unavailable")
	}
	g := setup(t)
	payload := fixture(t)
	calls := 0
	g.Transport = transport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.github.com" || r.URL.Path != "/repos/chriscoveries/wick/pulls/479" || r.Header.Get("Authorization") != "token offline-proxy-owner" && r.Header.Get("Authorization") != "Bearer offline-proxy-owner" {
			t.Fatalf("native host/path/auth altered: %s", r.URL)
		}
		// HTTP/2 upstreams have no HTTP/1 transfer framing. Add it for native gh.
		out := response(200, payload, http.Header{})
		out.Proto, out.ProtoMajor, out.ProtoMinor, out.ContentLength = "HTTP/2.0", 2, 0, -1
		return out, nil
	})
	cert := exec.Command("openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes", "-days", "1", "-subj", "/CN=api.github.com", "-addext", "subjectAltName=DNS:api.github.com", "-keyout", filepath.Join(g.Dir, "ca-key.pem"), "-out", filepath.Join(g.Dir, "ca.pem"))
	if b, err := cert.CombinedOutput(); err != nil {
		t.Fatal(err, string(b))
	}
	ln, err := g.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	home := t.TempDir()
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/usr/bin/gh", "api", "repos/chriscoveries/wick/pulls/479")
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "GH_TOKEN=offline-proxy-owner", "HTTPS_PROXY=" + ProxyURL(ln.Addr().String(), caller, g.Dir), "SSL_CERT_FILE=" + filepath.Join(g.Dir, "ca.pem"), "NO_PROXY="}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("native gh failed: %v %s", err, out)
		}
		var actual, recorded any
		if json.Unmarshal(out, &actual) != nil || json.Unmarshal(payload, &recorded) != nil {
			t.Fatal("invalid captured payload")
		}
		a, _ := json.Marshal(actual)
		b, _ := json.Marshal(recorded)
		if !bytes.Equal(a, b) {
			t.Fatal("native output altered")
		}
	}
	if calls != 2 {
		t.Fatal("native HTTP/2 framing failed", calls)
	}
}
func TestProxyMutationIsForwardedTwiceUncached(t *testing.T) {
	g := setup(t)
	calls := 0
	g.Transport = transport(func(r *http.Request) (*http.Response, error) {
		calls++
		b, _ := io.ReadAll(r.Body)
		if ReadQuery(b) {
			t.Fatal("mutation classified read")
		}
		return response(200, fixture(t), http.Header{}), nil
	})
	for i := 0; i < 2; i++ {
		r := req("POST", "/graphql")
		r.Body = io.NopCloser(strings.NewReader(`{"query":"mutation { addComment(input:{body:\"transport regression\"}){id} }"}`))
		content(t, g.RoundTrip(r, caller))
	}
	if calls != 2 {
		t.Fatal("mutation cached")
	}
}

func TestNativeMergeAndCommentReachUncachedMutations(t *testing.T) {
	if _, e := os.Stat("/usr/bin/gh"); e != nil {
		t.Skip("native gh unavailable")
	}
	g := setup(t)
	metadata, e := os.ReadFile("../../../testdata/pr-metadata.json")
	if e != nil {
		t.Fatal(e)
	}
	writes := []string{}
	g.State.Rates["owner/graphql"] = Rate{Remaining: 499, Reset: 11000}
	g.Transport = transport(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if ReadQuery(body) {
			return response(200, projectFixture(t, metadata, body), http.Header{}), nil
		}
		writes = append(writes, string(body))
		// Protocol control: never perform a merge/comment in this offline native CLI proof.
		return failure(403, "offline transport stops mutation"), nil
	})
	cert := exec.Command("openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes", "-days", "1", "-subj", "/CN=api.github.com", "-addext", "subjectAltName=DNS:api.github.com", "-keyout", filepath.Join(g.Dir, "ca-key.pem"), "-out", filepath.Join(g.Dir, "ca.pem"))
	if b, e := cert.CombinedOutput(); e != nil {
		t.Fatal(e, string(b))
	}
	ln, e := g.Start("127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	c := caller
	c.Essential = true
	for _, args := range [][]string{{"pr", "merge", "17", "-R", "chriscoveries/devmsg", "--merge", "--admin"}, {"pr", "comment", "17", "-R", "chriscoveries/devmsg", "--body", "transport regression"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, "/usr/bin/gh", args...)
		cmd.Env = []string{"HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(), "PATH=" + os.Getenv("PATH"), "GH_TOKEN=offline-owner", "HTTPS_PROXY=" + ProxyURL(ln.Addr().String(), c, g.Dir), "SSL_CERT_FILE=" + filepath.Join(g.Dir, "ca.pem"), "NO_PROXY="}
		out, e := cmd.CombinedOutput()
		cancel()
		if e == nil || !strings.Contains(string(out), "offline transport stops mutation") {
			t.Fatalf("mutation didn't reach proxy: %v %s", e, out)
		}
	}
	if len(writes) != 2 || !strings.Contains(writes[0], "mergePullRequest") || !strings.Contains(writes[1], "addComment") {
		t.Fatal("native mutation routing changed", writes)
	}
}

// GraphQL responses contain only selected fields. Project real captured metadata;
// fail on every unrecorded field rather than inventing content.
func projectFixture(t *testing.T, metadata, body []byte) []byte {
	t.Helper()
	var q struct{ Query string }
	json.Unmarshal(body, &q)
	doc, e := parser.ParseQuery(&ast.Source{Input: q.Query})
	if e != nil {
		t.Fatal(e)
	}
	var data map[string]any
	json.Unmarshal(metadata, &data)
	var schema map[string]any
	captured, e := os.ReadFile("../../../testdata/graphql-schema.json")
	if e != nil {
		t.Fatal(e)
	}
	json.Unmarshal(captured, &schema)
	for k, v := range schema["data"].(map[string]any) {
		data["data"].(map[string]any)[k] = v
	}
	var project func(any, ast.SelectionSet) any
	project = func(value any, selections ast.SelectionSet) any {
		if value == nil {
			return nil
		}
		if list, ok := value.([]any); ok {
			out := []any{}
			for _, v := range list {
				out = append(out, project(v, selections))
			}
			return out
		}
		record := value.(map[string]any)
		out := map[string]any{}
		for _, selection := range selections {
			switch field := selection.(type) {
			case *ast.Field:
				v, ok := record[field.Alias]
				if !ok {
					v, ok = record[field.Name]
				}
				if !ok {
					t.Fatalf("unrecorded field %s", field.Name)
				}
				if len(field.SelectionSet) > 0 {
					v = project(v, field.SelectionSet)
				}
				out[field.Alias] = v
			case *ast.FragmentSpread:
				for k, v := range project(value, doc.Fragments.ForName(field.Name).SelectionSet).(map[string]any) {
					out[k] = v
				}
			case *ast.InlineFragment:
				for k, v := range project(value, field.SelectionSet).(map[string]any) {
					out[k] = v
				}
			}
		}
		return out
	}
	b, _ := json.Marshal(map[string]any{"data": project(data["data"], doc.Operations[0].SelectionSet)})
	return b
}
