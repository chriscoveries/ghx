# Credential-bound rate custody

An attributed owner-class request must carry a single supported GitHub token
or receive one from its configured identity. CONNECT proxy authorization only
authenticates the local caller. It never substitutes for origin authorization.
Missing, blank, unsupported or duplicate origin authorization refuses before
cache lookup, stale responses, diagnostics, writes or any upstream request.

Each existing identity/resource rate slot now records a SHA-256 digest of the
effective credential. The digest covers the forwarded token, after configured
credential replacement; Bearer and token schemes for the same token agree.
No credential or digest appears in a refusal. There is no per-token map, new
file, timer, probe or worker.

While either Reset or Blocked is in the future, a different credential under
the same identity refuses before upstream. This also protects resource
remapping: the observed X-RateLimit-Resource destination is checked before
updating it. After both deadlines expire, rotation can bind the same fixed
slot and discard the previous credential's failure exponent. Deployments
using several simultaneous tokens must give them distinct configured
identities. A credential rotation can wait for an active epoch to expire.

Legacy rows with an empty digest remain unknown. Their active floor and
cooldown cannot be relabelled or relaxed by a successful essential request or
write. Healthy responses preserve its original remaining/reset, without binding the
legacy observation. Only an actual new below-floor observation can extend its
primary epoch; genuine new backoff can extend its cooldown. A healthy token
with a later reset cannot renew the old anonymous low counter. After expiry, an ordinary
authenticated response establishes custody. Existing malformed nonempty
digests refuse startup instead of silently becoming unknown.

An authenticated /rate_limit diagnostic remains available even with a foreign
active binding, but it cannot change any rate field or binding. HTTP401 also
preserves the previous record: an invalid/expired bearer must not import
anonymous headers. There is no /user probe or extra API call.

The regression uses the retained issue140 response as its byte-real body and
header source, plus the observed owner/core incident values. All substitutions
are explicitly named negative controls in the test and fixture provenance.
No Go build/test runs on firestar. Hosted CI must execute the controls before
reviewed deployment; this source change does not modify a running gate or its
state. The incident is left to expire per the seat's ruling, without a restart
or state edit.
