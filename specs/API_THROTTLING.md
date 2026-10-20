# Shared API read cache and throttling

The box governor's existing directory/socket is the shared cache authority.
The request key retains credential and representation headers, omitting lane
transport metadata, User-Agent and conditional validators. Command caching has
a five-minute floor, including old configuration overrides. The governor
honors a shorter upstream max-age and explicit no-cache revalidation; it never
invents an ETag for an endpoint that supplies none.

Successful REST GETs and complete read-only GraphQL query results can reuse a
fresh entry. GETs revalidate with ETag or Last-Modified. GraphQL has no assumed
304 behavior; partial/error query results are not reusable. A mutation clears
read entries. Cache files retain the existing credential hash boundary and
are bounded to 128 entries, 64 MiB on disk, and 8 MiB per response body.

Any upstream 403/429 creates a persisted cooldown: 300, 600, 1200, then 1800
seconds. Retry-After seconds/date and a depleted primary reset may extend it.
No retry is performed inside a call. A denied call cannot be bypassed with a
new caller name. Fresh cache hits consume no upstream budget. There is no box and no
per-caller cap: the per-caller hour count is attribution and reporting only.
The single read refusal is GitHub's own signal — a persisted 403/429 cooldown
or the floor (Remaining under 500). Status reads (run lists, check runs and
suites, commit statuses, GraphQL queries) share one box-wide StatusTTL window
(default 120s) instead of shrinking to GitHub's advertised 60s max-age, so
every lane polling a repo inside that window is served by one upstream read.
Counters include hit, hit_rate, 304, upstream, refused and backoff. The companion controller change
removes direct fallback around the gate; deployment must include both pieces.

Recorded tests use the exact private-repository HTTP 200/304 exchange in
testdata/api-throttle (provenance and limitations there). Backoff counts/times
in the tests are explicit policy inputs, not fabricated rate-error receipts.
No production limiter or daemon is changed by the tests.

Contract reference:
https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api
