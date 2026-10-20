package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/brunoborges/ghx/src/internal/config"
)

// One batched GraphQL read per repository: the default branch head, the open
// pull requests with their head commit, mergeability and review decision, and
// each head's check rollup. Every lane watching a repo asks for this snapshot
// and shares the gate's window, so N lanes cost one GitHub call per interval
// instead of N×3 (PR, checks, runs).
//
// The snapshot is a read: it never asks for a fresh copy by default. A caller
// that needs authority (the final approval immediately before a landing) asks
// for --fresh, which is the agreed single exception.
const snapshotQuery = `query($owner: String!, $name: String!, $limit: Int!) {
  repository(owner: $owner, name: $name) {
    nameWithOwner
    isPrivate
    defaultBranchRef {
      name
      target { oid ... on Commit { committedDate statusCheckRollup { state } } }
    }
    pullRequests(first: $limit, states: OPEN, orderBy: {field: UPDATED_AT, direction: DESC}) {
      nodes {
        number
        title
        isDraft
        mergeable
        mergeStateStatus
        reviewDecision
        headRefName
        headRefOid
        baseRefName
        baseRefOid
        updatedAt
        changedFiles
        files(first: 100) { totalCount nodes { path additions deletions } }
        reviews(first: 20) {
          totalCount
          nodes { state submittedAt author { login } commit { oid } }
        }
        commits(last: 1) {
          nodes {
            commit {
              oid
              statusCheckRollup {
                state
                contexts(first: 100) {
                  nodes {
                    __typename
                    ... on CheckRun { name status conclusion checkSuite { app { slug } } }
                    ... on StatusContext { context state targetUrl }
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`

type snapshotOptions struct {
	owner, name string
	limit       int
	fresh       bool
}

// parseSnapshotArgs is pure so the flag contract can be tested without a gh
// binary, a gate or a network.
func parseSnapshotArgs(args []string) (snapshotOptions, error) {
	opts := snapshotOptions{limit: 20}
	repo := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-R" || a == "--repo":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("%s needs owner/name", a)
			}
			repo = args[i+1]
			i++
		case strings.HasPrefix(a, "--repo="):
			repo = strings.TrimPrefix(a, "--repo=")
		case strings.HasPrefix(a, "-R="):
			repo = strings.TrimPrefix(a, "-R=")
		case a == "--limit":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--limit needs a number")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 || n > 100 {
				return opts, fmt.Errorf("--limit must be 1..100")
			}
			opts.limit = n
			i++
		case strings.HasPrefix(a, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err != nil || n < 1 || n > 100 {
				return opts, fmt.Errorf("--limit must be 1..100")
			}
			opts.limit = n
		case a == "--fresh":
			opts.fresh = true
		default:
			return opts, fmt.Errorf("unknown argument %q", a)
		}
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return opts, fmt.Errorf("snapshot needs -R owner/name")
	}
	opts.owner, opts.name = parts[0], parts[1]
	return opts, nil
}

func snapshotGHArgs(opts snapshotOptions) []string {
	return []string{
		"api", "graphql",
		"-f", "query=" + snapshotQuery,
		"-f", "owner=" + opts.owner,
		"-f", "name=" + opts.name,
		"-F", fmt.Sprintf("limit=%d", opts.limit),
	}
}

func handleSnapshot(cfg *config.Config, args []string) {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println("Usage: ghx snapshot -R owner/name [--limit N] [--fresh]")
		fmt.Println()
		fmt.Println("One batched GraphQL read of a repo: default branch head, open PRs,")
		fmt.Println("each head's checks. Shared through the gate window; --fresh is for")
		fmt.Println("authority decisions only (the final approval before a landing).")
		return
	}
	opts, err := parseSnapshotArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghx snapshot:", err)
		os.Exit(2)
	}
	if opts.fresh {
		os.Setenv("GHX_NO_CACHE", "1")
	}
	cmd := exec.Command(cfg.GHPath, snapshotGHArgs(opts)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	code := 0
	if err := cmd.Run(); err != nil {
		code = 1
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		}
	}
	logCall([]string{"snapshot", opts.owner + "/" + opts.name}, false, code)
	os.Exit(code)
}
