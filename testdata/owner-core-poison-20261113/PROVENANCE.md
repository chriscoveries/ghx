# Owner/core credential-custody incident

`observed-state.json` is an allowlisted transcription of the seat's root-side
inspection, CCCC event `9d22ae260de94b48915e4684982bfd7f`, sent
2026-11-13T03:39:08Z. It records actual `state.json` owner/core Remaining59,
Reset04:16:36Z, and independently observed authenticated quota4411/5000.
It is not a raw HTTP response or a copy of the whole private state file.

Trigger: sol made an attributed CONNECT request without origin Authorization.
The private job endpoint returned404. Its anonymous rate headers were recorded
under owner/core; the subsequent authenticated read was refused until the
anonymous epoch expired. The original404 wire was not retained. The seat ruled
against state edits/restart: let it expire and fix source credential custody.

Tests replay the existing unchanged `testdata/api-throttle/issue140.http`
response and use its parsed headers/body. The incident's observed Remaining
and Reset replace those two headers as a **named negative transport control**;
HTTP401/403, resource remapping and credential changes are also explicit fault
injection, not claimed upstream captures. Missing credential must prevent this
prepared response from ever reaching the transport. The owner/core record must
remain exactly unchanged. No token or signed URL is stored.

Tests execute only on hosted CI. No Go build/test ran on firestar. Deployment
is a separate reviewed seat window; no running daemon/state was modified.

The first external GARM gate (cifast run `43373781816`, ghx `34013e95`)
passed the anonymous-poison controls but exposed a separate old replay request
constructor with no Authorization. That constructor now reuses the suite's
existing nonsecret credential stand-in. All original shared-GET, conditional
304 and restart assertions and captured response bytes remain unchanged.
Deleting Authorization from that same request after populating the real shared
cache is an explicit negative control: it must refuse before serving the cache
or reaching transport. This is not a captured credential or an anonymous
positive fixture, and no production admission rule is weakened for the replay.
