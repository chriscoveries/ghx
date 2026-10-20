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
new caller name. Fresh cache hits consume no upstream/caller budget. Counters
include hit, 304, upstream, refused and backoff. The companion controller change
removes direct fallback around the gate; deployment must include both pieces.

Recorded tests use the exact private-repository HTTP 200/304 exchange in
testdata/api-throttle (provenance and limitations there). Backoff counts/times
in the tests are explicit policy inputs, not fabricated rate-error receipts.
No production limiter or daemon is changed by the tests.

Contract reference:
https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api
