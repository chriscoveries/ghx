# Resource caching and offline measurement

Numeric `pr view`, `issue view`, and `run view` JSON reads now use one resource
key across requested fields, their order, and numeric/URL selectors. That key
includes host, repository, credential identity and (for runs) an explicit attempt;
it omits the current git branch. The pinned field supersets and merged-PR fixed
field mask live in `src/internal/resource/policy.json`. A cold read fetches the
superset through the installed `gh`; later reads project only requested fields.

`resource_views` defaults to `true`. Set it to `false` in the existing ghx config
to retain exact-command view caching. `immutable_ttl` defaults to `24h` and bounds
reuse of merged PR fixed fields and completed, explicitly selected run attempts.
Zero disables this extra reuse. Titles, bodies, labels, comments, review/check
state and other editable PR fields use the ordinary command TTL. A latest run
selector can rerun and uses the ordinary TTL. All retained bytes remain subject
to #2's LRU entry/byte budgets, including immutable snapshots and validators.

Unix JSON `--jq` requests run the installed CLI against projected cached bytes
over a temporary private Unix socket. This uses native jq semantics, starts one
local CLI process for each formatting request, and sends no GitHub request.
The temporary configuration contains no user credentials. An unsupported socket
setting encounters a blocked loopback proxy. Windows jq, templates, branch or
implicit selectors, unknown flags and fields use native exact-command execution.
Unsupported supersets fall back with a daemon-log diagnostic; successful native
fallbacks are cached, preventing repeated superset attempts for the same command.
Rate-limit/authentication/timeout failures do not trigger a retry or enter cache.

Plain, single-page `gh api` GETs retain the ETag separately from the body. After
the ordinary TTL expires they send `If-None-Match`; a 304 refreshes the retained
body without leaking HTTP headers into stdout. A 200 replaces body and validator.
Formatting, explicit include, pagination, input/fields and user validators retain
the native path. GraphQL is conservatively uncached; implicit POST and attached
or equals-form method flags are recognized as writes. `ghx xcache stats` reports
304 revalidations separately. They count as body reuse, and still make a request.

Successful writes invalidate the addressed numeric object, its collections and
related issue/PR API dependencies, across credential identities. Explicit `-R`
and URL selectors resolve the invalidation repository. Other repositories and
numeric objects survive. Creates evict collections while preserving existing
objects. Ambiguous API paths/selectors and repo/label writes evict more broadly;
search and unresolved API dependencies are conservative. `--no-cache` writes
still invalidate. A cache generation changes on every write invalidation/flush,
so earlier reads cannot refill invalidated state or be joined by later reads.
Errors are not retained (pending `pr checks`, exit 8, remains cacheable).

## Measurement

Run the offline tool with:

```sh
go run ./src/cmd/ghx-replay ~/.local/state/gh-calls-v2.jsonl
go test -v ./src/internal/daemon -run TestCapturedWorkloadReplay
```

The frozen input was a read-only snapshot of that log covering
2026-10-02 15:17:18 through 20:30:24 UTC. Its SHA-256 and aggregate output are in
[resource-cache-replay.json](resource-cache-replay.json). The raw log and its
working directories, identities and callers are not included in this PR.

| Offline replay | Before | After |
| --- | ---: | ---: |
| Known read command shapes, 30s TTL, 1000 entries | 1,173 | 1,173 |
| Exact-hash hits | 9 (0.77%) | 8 (0.68%) |
| Ten distinct JSON selections of captured upstream PR #21 | 0/10 (0%) | 9/10 (90%) |
| Fixture backend executions for those ten selections | 10 | 1 |

The exact-hash model deliberately flushes both caches on every possible write
because repository/selector dependencies are absent. The single removed hit was
a cached failure. The input has 1,953 calls, 11 observed cached calls, 566 resource
view shapes and 545 ambiguous API calls. Its hashes encode whole commands: they
cannot be reversed to identify the same resource across different field lists.
It also lacks response state, ETags, sizes and durations. Consequently the new
resource/immutable/304 workload hit rate is **unmeasurable from this log**, and
is emitted as `null`. Grouping all `pr view` calls or cwd values as one resource
would invent savings and could conflate unrelated repositories and credentials.

The 90% figure is a controlled offline regression measurement against captured
public bytes, not an estimated deployment rate. Backend executions are CLI fetch
invocations in that fixture replay, not measured live HTTP requests. Supersets
fetch more fields; run jobs can require extra CLI-internal requests on a miss.
Actual API-rate/latency gains depend on field mix, superset size and response
state. No live replay, deployment or configuration changes were performed.
