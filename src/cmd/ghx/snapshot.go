package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/brunoborges/ghx/src/internal/config"
	"github.com/brunoborges/ghx/src/internal/governor"
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
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        title
        isDraft
        state
        createdAt
        updatedAt
        mergeable
        mergeStateStatus
        reviewDecision
        headRefName
        headRefOid
        baseRefName
        baseRefOid
        changedFiles
        additions
        deletions
        files(first: 100) {
          pageInfo { hasNextPage }
          totalCount
          nodes { path additions deletions }
        }
        reviews(last: 100) {
          pageInfo { hasPreviousPage }
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
                  pageInfo { hasNextPage }
                  nodes {
                    __typename
                    ... on CheckRun {
                      databaseId
                      name
                      status
                      conclusion
                      checkSuite { app { slug databaseId } workflowRun { event databaseId } }
                    }
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
	allowStale  bool
}

// parseSnapshotArgs is pure so the flag contract can be tested without a gh
// binary, a gate or a network.
func parseSnapshotArgs(args []string) (snapshotOptions, error) {
	opts := snapshotOptions{limit: 100}
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
		case a == "--allow-stale":
			opts.allowStale = true
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

// snapshotGHArgs asks gh for the response headers as well as the body: the
// gate's staleness marker lives in X-Ghgate-Stale, and a JSON-only reader that
// does not look for it cannot tell a shared read from an obsolete one.
func snapshotGHArgs(opts snapshotOptions) []string {
	return []string{
		"api", "graphql", "-i",
		"-f", "query=" + snapshotQuery,
		"-f", "owner=" + opts.owner,
		"-f", "name=" + opts.name,
		"-F", fmt.Sprintf("limit=%d", opts.limit),
	}
}

// applySnapshotProxy re-encodes the gate caller for the child process with the
// freshness this command actually means. It is the only place a snapshot can
// ask the gate to bypass its window.
func applySnapshotProxy(fresh bool) error {
	addr := os.Getenv("GHX_GATE_ADDR")
	if addr == "" {
		return nil
	}
	dir := os.Getenv("GHX_GATE_DIR")
	if dir == "" {
		return fmt.Errorf("GHX_GATE_DIR missing; run devmsg box install-ghx")
	}
	lane := os.Getenv("GH_CALLER")
	os.Setenv("HTTPS_PROXY", governor.ProxyURL(addr, governor.Caller{Lane: lane, Class: "automation", Revalidate: fresh}, dir))
	os.Setenv("HTTP_PROXY", os.Getenv("HTTPS_PROXY"))
	os.Setenv("NO_PROXY", "")
	os.Setenv("SSL_CERT_FILE", filepath.Join(dir, "ca.pem"))
	if fresh {
		os.Setenv("GHX_NO_CACHE", "1")
	} else {
		os.Unsetenv("GHX_NO_CACHE")
	}
	return nil
}

func handleSnapshot(cfg *config.Config, args []string) {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println("Usage: ghx snapshot -R owner/name [--limit N] [--fresh]")
		fmt.Println("  --limit N   open PRs to return, 1..100 (default 100)")
		fmt.Println("  --fresh     bypass the shared window; authority decisions only")
		fmt.Println("  --allow-stale  accept a cached body the gate marked stale (exit 3 otherwise)")
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
	// The proxy URL is built once, in prepareFleet, from the flags it parsed —
	// and it does not know --fresh. Setting GHX_NO_CACHE here would be too late:
	// the caller (and with it Revalidate) is already encoded in HTTPS_PROXY, so
	// a "fresh" snapshot would quietly be served from the window it was meant to
	// bypass. Rebuild the proxy environment for this child instead.
	if err := applySnapshotProxy(opts.fresh); err != nil {
		fmt.Fprintln(os.Stderr, "ghx snapshot:", err)
		os.Exit(1)
	}
	cmd := exec.Command(cfg.GHPath, snapshotGHArgs(opts)...)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	code := 0
	if err != nil {
		code = 1
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		}
	}
	header, body, split := splitGHResponse(out)
	if !split {
		os.Stdout.Write(out)
	} else {
		os.Stdout.Write(body)
		if code == 0 {
			code = staleExit(header, opts.allowStale)
		}
	}
	logCall([]string{"snapshot", opts.owner + "/" + opts.name}, false, code)
	os.Exit(code)
}

// splitGHResponse separates the status line and headers that `gh api -i`
// prints from the JSON body. The blank line ends the header block.
func splitGHResponse(raw []byte) (map[string]string, []byte, bool) {
	sep := []byte("\r\n\r\n")
	idx := bytes.Index(raw, sep)
	skip := len(sep)
	if idx < 0 {
		sep = []byte("\n\n")
		idx = bytes.Index(raw, sep)
		skip = len(sep)
	}
	if idx < 0 {
		return nil, raw, false
	}
	header := map[string]string{}
	for _, line := range strings.Split(string(raw[:idx]), "\n") {
		line = strings.TrimRight(line, "\r")
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		header[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}
	return header, raw[idx+skip:], true
}

// staleExit refuses to let a planning read pass as current when the gate
// answered from a cached body it marked stale. The body stays untouched on
// stdout; the refusal is the exit code, so a controller cannot schedule work
// off an outage without noticing.
func staleExit(header map[string]string, allowStale bool) int {
	if header["x-ghgate-stale"] != "true" {
		return 0
	}
	age := header["age"]
	if age == "" {
		age = "unknown"
	}
	if allowStale {
		fmt.Fprintf(os.Stderr, "ghx snapshot: WARNING stale response served from cache (age %ss); --allow-stale given, continuing\n", age)
		return 0
	}
	fmt.Fprintf(os.Stderr, "ghx snapshot: refusing a stale response (age %ss): the gate could not reach GitHub. Re-run, or pass --allow-stale to accept it.\n", age)
	return 3
}
