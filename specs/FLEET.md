Fleet deployment is installed by `devmsg box install-ghx BOX|--all`, which pins
release archive SHA-256s and every budget in fleet.toml. No credentials are shipped.

On Unix, `cache_file` enables a private atomic disk snapshot bounded by the
configured entry/response-byte limits. One daemon owns a snapshot and its socket.
Windows retains the memory cache and refuses cache_file because POSIX modes do
not establish private Windows ACLs. Disk write failures appear as disk_errors;
a failed replacement removes the old snapshot to avoid replaying invalidated data.

`ghxd --gate --dir DIRECTORY --addr LOOPBACK` runs the box-wide native API governor.
The directory contains locally generated TLS material and JSON policy. The private
key is never distributed to users; proxy-proof and ca.pem are public on the box.
The API transport reads Authorization only in memory, preserves native CLI output,
paces writes, accounts all users against BoxHourly, and keeps per-caller budgets.
`rate_limit` bypasses cache and budget accounting. `/health` reports counts only.

The shim exports GH_CALLER from an explicit caller, CODEX_LANE, DEVMSG_LANE, or
box:user. Runner services export their own identities. A successful upstream write
advances the box epoch, making prior users' command snapshots unreachable. The
ledger records sanitized command shapes and hashes, never arguments or tokens.

Devmsg's five-minute health timer alarms on missing installation and a hit rate
under 30% in its trailing one-hour window. Zero requests means insufficient data.
