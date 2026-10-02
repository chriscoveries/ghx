These JSON bodies were captured from the public MIT project brunoborges/ghx
(PRs #21 and #22 and its repository metadata) by the preceding resource work.
They contain public upstream responses, not user call logs. Tests preserve their
bytes and project fields locally; they never query GitHub. HTTP framing in unit
tests exercises validators around these captured bodies. State-only run probes
exercise freshness decisions, without manufacturing a run response fixture.
