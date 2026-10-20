# Recorded API cache evidence

`issue140.http` and `issue140-conditional.http` are the complete real GET and
conditional GET responses through the installed ghx gate on 2026-10-20. The
endpoint, host, UTC capture times and SHA-256 bindings are in PROVENANCE.json.
No token, request authorization or JIT data is retained. The second request used
the first response's ETag. Native gh exits 1 for the real HTTP 304; that exit is
recorded, not disguised as a successful command.

The tests replay the captured bytes, and calculate cooldown policy using their
unchanged headers. Retry counts/times are policy parameters, not observed rate
errors. No real 403/429/Retry-After response was retained by the installed gate;
that error-transport replay remains unobserved. No rate limit was provoked.
