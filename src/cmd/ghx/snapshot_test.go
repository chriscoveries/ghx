package main

import (
	"strings"
	"testing"
)

func TestSnapshotArgsNameOneRepoAndOneQuery(t *testing.T) {
	opts, err := parseSnapshotArgs([]string{"-R", "chriscoveries/ghx"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.owner != "chriscoveries" || opts.name != "ghx" || opts.limit != 20 || opts.fresh {
		t.Fatalf("opts %+v", opts)
	}
	args := snapshotGHArgs(opts)
	joined := strings.Join(args, " ")
	if !strings.HasPrefix(joined, "api graphql ") {
		t.Fatalf("not one graphql call: %s", joined)
	}
	// The batch: head, PRs with their head commit, and the check rollup.
	for _, want := range []string{"defaultBranchRef", "pullRequests(", "headRefOid", "baseRefOid", "changedFiles", "files(first: 100)", "reviews(first: 20)", "statusCheckRollup", "reviewDecision", "checkSuite { app { slug } }"} {
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
