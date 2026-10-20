package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brunoborges/ghx/src/internal/governor"
)

func TestSnapshotArgsNameOneRepoAndOneQuery(t *testing.T) {
	opts, err := parseSnapshotArgs([]string{"-R", "chriscoveries/ghx"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.owner != "chriscoveries" || opts.name != "ghx" || opts.limit != 100 || opts.fresh {
		t.Fatalf("opts %+v", opts)
	}
	args := snapshotGHArgs(opts)
	joined := strings.Join(args, " ")
	if !strings.HasPrefix(joined, "api graphql ") {
		t.Fatalf("not one graphql call: %s", joined)
	}
	// The batch: head, PRs with their head commit, and the check rollup.
	for _, want := range []string{"defaultBranchRef", "pullRequests(", "pageInfo { hasNextPage endCursor }", "headRefOid", "baseRefOid", "changedFiles", "additions", "deletions", "files(first: 100)", "reviews(last: 100)", "hasPreviousPage", "statusCheckRollup", "contexts(first: 100)", "reviewDecision", "checkSuite { app { slug databaseId } workflowRun { event databaseId } }"} {
		if !strings.Contains(snapshotQuery, want) {
			t.Fatalf("query lacks %s", want)
		}
	}
	if strings.Count(joined, "api graphql") != 1 {
		t.Fatal("batch must be a single call")
	}
}

func TestSnapshotFreshIsOptIn(t *testing.T) {
	opts, err := parseSnapshotArgs([]string{"--repo=o/r", "--limit=5"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.fresh || opts.limit != 5 {
		t.Fatalf("opts %+v", opts)
	}
	opts, err = parseSnapshotArgs([]string{"-R", "o/r", "--fresh"})
	if err != nil || !opts.fresh {
		t.Fatalf("fresh not parsed: %+v %v", opts, err)
	}
	if _, err := parseSnapshotArgs([]string{"--limit", "0", "-R", "o/r"}); err == nil {
		t.Fatal("limit 0 accepted")
	}
	if _, err := parseSnapshotArgs([]string{"snapshot"}); err == nil {
		t.Fatal("missing repo accepted")
	}
}

// --fresh must reach the gate as the caller it encodes, not as an env var set
// after the proxy URL was already built (prepareFleet runs before the
// subcommand and does not know --fresh).
func TestSnapshotFreshIsEncodedInTheProxyCaller(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "proxy-proof"), []byte("proof\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHX_GATE_ADDR", "127.0.0.1:18771")
	t.Setenv("GHX_GATE_DIR", dir)
	t.Setenv("GH_CALLER", "lab-hermes5")
	t.Setenv("CCCC_ACTOR_ID", "")

	callerOf := func() governor.Caller {
		t.Helper()
		u, err := url.Parse(os.Getenv("HTTPS_PROXY"))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(u.User.Username())
		if err != nil {
			t.Fatal(err)
		}
		var c governor.Caller
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		return c
	}

	if err := applySnapshotProxy(false); err != nil {
		t.Fatal(err)
	}
	shared := callerOf()
	if shared.Revalidate || shared.Lane != "lab-hermes5" {
		t.Fatalf("ordinary snapshot encoded %+v", shared)
	}
	if err := applySnapshotProxy(true); err != nil {
		t.Fatal(err)
	}
	fresh := callerOf()
	if !fresh.Revalidate {
		t.Fatalf("--fresh did not reach the gate: %+v", fresh)
	}
	if fresh.Lane != "lab-hermes5" {
		t.Fatalf("fresh call lost its lane: %+v", fresh)
	}
	if os.Getenv("GHX_NO_CACHE") != "1" {
		t.Fatal("fresh did not set GHX_NO_CACHE for the child")
	}
	if err := applySnapshotProxy(false); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GHX_NO_CACHE") != "" {
		t.Fatal("stale GHX_NO_CACHE survived a shared snapshot")
	}
}

func TestSplitGHResponseFindsBodyAndStaleMarker(t *testing.T) {
	raw := []byte("HTTP/2.0 200 OK\r\nContent-Type: application/json\r\nX-Ghgate-Stale: true\r\nAge: 412\r\n\r\n{\"data\":{\"repository\":{\"nameWithOwner\":\"o/r\"}}}\n")
	header, body, ok := splitGHResponse(raw)
	if !ok {
		t.Fatal("headers not split from body")
	}
	if strings.Contains(string(body), "HTTP/2.0") || !strings.HasPrefix(string(body), "{\"data\"") {
		t.Fatalf("body carries header text: %q", body)
	}
	if header["x-ghgate-stale"] != "true" || header["age"] != "412" {
		t.Fatalf("stale marker lost: %v", header)
	}
	if code := staleExit(header, false); code != 3 {
		t.Fatalf("stale accepted silently, exit %d", code)
	}
	if code := staleExit(header, true); code != 0 {
		t.Fatalf("--allow-stale did not accept, exit %d", code)
	}
	// A fresh read passes, and a body with no header block is not mangled.
	if code := staleExit(map[string]string{"age": "0"}, false); code != 0 {
		t.Fatalf("fresh read refused, exit %d", code)
	}
	if _, body, ok := splitGHResponse([]byte("{\"data\":{}}")); ok || string(body) != "{\"data\":{}}" {
		t.Fatal("a bodyless-header response was split")
	}
}

func TestSnapshotAsksGHForHeaders(t *testing.T) {
	opts, err := parseSnapshotArgs([]string{"-R", "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(snapshotGHArgs(opts), " ")
	if !strings.Contains(args, "api graphql -i") {
		t.Fatalf("snapshot does not request headers, so staleness is invisible: %s", args)
	}
}
