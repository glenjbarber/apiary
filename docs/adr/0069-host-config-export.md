# ADR-0069: Redacted host-config export

## Status

Accepted

## Context

`raftd -export`/`-restore` (ADR-0051) covers only raft's own
ephemeral state - not a node's actual host configuration. Three
concrete gaps were named in an earlier session: `/etc/rc.conf` (this
node's real daemon flags - notably `-peer-api-key`, a live secret since
ADR-0037), `/etc/pf.conf` (the host's own packet-filter rules, distinct
from Apiary's per-VM firewall records already in raft), and
`/etc/master.passwd` (PAM/system-account state backing ADR-0030's real
per-identity login).

The user previously scoped a redaction approach for `master.passwd`
(replace the crypt hash field with `*`, never an empty field) but
explicitly deferred implementation. Asked again directly whether to
build export, restore, or neither this time, the user chose export
only.

## Decision

**Export only. No restore/apply path exists or is planned as part of
this ADR.** Automatically writing `master.passwd`/`pf.conf` back to a
live host is a materially different, higher-risk problem than reading
them - a bug could lock out every account or break network
connectivity - and needs its own careful design and live testing on
non-critical infrastructure first. This is a deliberate scope boundary,
stated in `internal/hostconfig`'s own package doc comment, not an
oversight to revisit casually.

**Never a network RPC.** Unlike raft state, host configuration
includes real account records and (via `rc.conf`) a live API-key
secret - exposing either over any network path, even authenticated,
meaningfully increases risk for something already sensitive. This is a
local, operator-run one-shot CLI action (`managerd -export-host-config
<dir>`), mirroring `raftd -export`/`-restore`'s own posture exactly.

**Redaction, not exclusion.** `master.passwd`'s account structure
(username, uid, gid, class, gecos, home, shell) is preserved with only
the crypt-hash field replaced by `*` - useful for understanding what
accounts exist without ever persisting a crackable hash. `rc.conf` is
similarly redacted rather than dropped: `-peer-api-key`'s value is
replaced with a fixed placeholder via a narrow, explicit
pattern-match, not a general secret scanner - any future flag that
carries a live secret needs to be added to that allowlist
deliberately. `pf.conf` is copied as-is; it wasn't identified as
carrying secrets.

**Three separate plain files, not one archive format.** Unlike
`raftd -export`'s versioned envelope (meant for `-restore` to parse
back), this is a human-facing backup with no machine-restore
counterpart - an operator should be able to open and read
`rc.conf.redacted`/`pf.conf`/`master.passwd.redacted` directly with no
special tooling. Output directory and files are `0700`/`0600`
regardless - redacted account/daemon-configuration data is still worth
keeping off a shared or world-readable path (the same posture ADR-0067
established for the source files themselves).

## Verification

`internal/hostconfig`'s redaction functions are pure and fully unit
tested, including the edge case of a secret placed last inside
`rc.conf`'s quoted args string (confirming the replacement doesn't
consume the closing quote) and confirming an already-locked (`*`)
account isn't disturbed. `cmd/managerd`'s `-export-host-config` wiring
is tested against fixture files, not a real host's own `/etc/rc.conf`
et al. `go build`/`go vet`/`go test ./...`/`gofmt -l`/`git diff --check`
all pass, and the FreeBSD cross-compile for managerd/raftd/restshimd
succeeds. Live verification against the real files on `apiarium`/
`apiverse` has not been performed as part of landing this.

## Deferred

- A restore/apply path, as discussed above - a distinctly higher-risk
  problem needing its own design.
- Detecting and redacting any *other* secret that might end up in
  `rc.conf` in the future - the current allowlist covers only
  `-peer-api-key`, the one secret this project has actually put there.
- Bundling the three output files into a single downloadable archive,
  or exposing this through the web UI - deliberately a local CLI-only
  action for now, matching the sensitivity of what it reads.
