package governor

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// No HTTP status, header or body is manufactured. These are the retained real
// 200/304 exchanges through ghx. Time, callers and retry counts below are policy
// parameters, not claimed observations of provider throttling.
func capturedHTTP(t *testing.T, name string) *http.Response {
	t.Helper()
	dir := "../../../testdata/api-throttle"
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := os.ReadFile(filepath.Join(dir, "PROVENANCE.json"))
	if err != nil {
		t.Fatal(err)
	}
	var provenance struct{ Files map[string]string }
	if err := json.Unmarshal(meta, &provenance); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(b)) != provenance.Files[name] {
		t.Fatal("capture changed")
	}
	r, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(b)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func recordedRequest(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequest("GET", "https://api.github.com/repos/chriscoveries/cifast/issues/140", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("User-Agent", "cifast-jit-broker")
	return r
}

func TestRecordedSharedGETAnd304(t *testing.T) {
	g, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := capturedHTTP(t, "issue140.http")
	firstETag := first.Header.Get("ETag")
	now, err := http.ParseTime(first.Header.Get("Date"))
	first.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	g.Now = func() time.Time { return now }
	calls := 0
	g.Transport = transport(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return capturedHTTP(t, "issue140.http"), nil
		}
		if calls > 3 {
			t.Fatal("extra upstream request")
		}
		conditional := capturedHTTP(t, "issue140-conditional.http")
		expectedETag := firstETag
		if calls == 3 {
			expectedETag = conditional.Header.Get("ETag")
		}
		if r.Header.Get("If-None-Match") != expectedETag {
			t.Fatal("incorrect captured validator")
		}
		return conditional, nil
	})
	owner := Caller{Lane: "lab-sol-b", Class: "owner"}
	one := g.RoundTrip(recordedRequest(t), owner)
	body, err := io.ReadAll(one.Body)
	one.Body.Close()
	if err != nil || one.StatusCode != 200 {
		t.Fatal(one.StatusCode, err)
	}
	var issue struct{ Number int }
	if json.Unmarshal(body, &issue) != nil || issue.Number != 140 {
		t.Fatal("not captured issue")
	}
	twoReq := recordedRequest(t)
	twoReq.Header.Set("User-Agent", "cifast-scale-set")
	twoReq.Header.Set("Content-Type", "application/json")
	two := g.RoundTrip(twoReq, Caller{Lane: "lab-devin-rt", Class: "owner"})
	twoBody, err := io.ReadAll(two.Body)
	two.Body.Close()
	if err != nil || !bytes.Equal(body, twoBody) || calls != 1 || two.Header.Get("X-Ghgate-Cache") != "hit" {
		t.Fatal("cross-lane cache missed", calls, err)
	}
	// The real cifast authority path requests no-cache, forcing authenticated
	// conditional revalidation rather than using the local TTL.
	threeReq := recordedRequest(t)
	threeReq.Header.Set("Cache-Control", "no-cache")
	three := g.RoundTrip(threeReq, owner)
	threeBody, err := io.ReadAll(three.Body)
	three.Body.Close()
	if err != nil || three.StatusCode != 200 || !bytes.Equal(body, threeBody) || calls != 2 || g.State.Counts["304"] != 1 {
		t.Fatal("captured 304 was not restored", calls, err)
	}
	// Native ghx --no-cache/GHX_NO_CACHE must bypass the shared gate TTL too.
	noCache := owner
	noCache.Revalidate = true
	native := g.RoundTrip(recordedRequest(t), noCache)
	nativeBody, err := io.ReadAll(native.Body)
	native.Body.Close()
	if err != nil || !bytes.Equal(body, nativeBody) || calls != 3 || g.State.Counts["304"] != 2 {
		t.Fatal("native no-cache did not revalidate", calls, err)
	}
	// Reload the real files emitted by this gate, rather than hand-building state.
	restarted, err := New(g.Dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Now = g.Now
	restarted.Transport = transport(func(*http.Request) (*http.Response, error) { t.Fatal("restart lost shared cache"); return nil, nil })
	four := restarted.RoundTrip(recordedRequest(t), owner)
	defer four.Body.Close()
	if four.StatusCode != 200 || four.Header.Get("X-Ghgate-Cache") != "hit" {
		t.Fatal("restart miss")
	}
}

func TestRecordedBackoffPolicy(t *testing.T) {
	r := capturedHTTP(t, "issue140.http")
	defer r.Body.Close()
	now, err := http.ParseTime(r.Header.Get("Date"))
	if err != nil {
		t.Fatal(err)
	}
	for i, delay := range []int64{300, 600, 1200, 1800, 1800, 1800} {
		if got := blockedUntil(r.Header, now.Unix(), int64(i+1)); got != now.Unix()+delay {
			t.Fatal("backoff policy", i+1, got-now.Unix())
		}
	}
	if strings.Contains(r.Header.Get("Retry-After"), "\n") {
		t.Fatal("invalid capture")
	}
}
