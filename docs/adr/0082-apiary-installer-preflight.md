# ADR-0082: `apiaryinstall`, a host preflight/provisioning tool

## Status

Accepted

## Context

Apiary's own `internal/*` packages assume a long list of FreeBSD host
prerequisites, each disclosed only in an ADR's or the README's own prose,
never checked in code: `vmm.ko`/`nmdm.ko` loaded (ADR-0032), `bhyve-firmware`
and `dnsmasq` installed (ADR-0022), a ZFS pool present (ADR-0006), `pf`
enabled with an `anchor "apiary/*"` stanza reserved (ADR-0022,
`internal/pf`'s own package doc comment), `gateway_enable`/IP forwarding for
self-hosted NAT (ADR-0048), a bridge interface enslaving the real uplink NIC
(ADR-0022), `/etc/rc.conf` not being group/world readable (ADR-0067, which
found it world-readable in production with a live `-peer-api-key` secret),
and PAM/`hastd` setup (ADR-0030, ADR-0026). None of it is verified anywhere
today - `internal/bhyve.CreateVM` will happily try to exec `bhyve` even if
`vmm.ko` isn't loaded and fail with a raw, unfriendly exec error; every
other package behaves the same way.

Two things confirmed there was no existing home for this: `internal/
assumecheck` (ADR-0055) only monitors an already-running cluster's raft/peer
health via remote RPCs - it never inspects local host provisioning, and
assumes `managerd` is already up. `internal/hostconfig` (ADR-0069) only
exports `/etc/rc.conf`/`/etc/pf.conf`/`/etc/master.passwd` as redacted text
and is explicitly, permanently export-only ("No restore/apply path exists
or is planned"). There is also no separate operator-CLI precedent in this
repo - ADR-0038 and ADR-0051's "CLI" work is just one-shot flags on
`raftd`/`managerd`'s own `main()`. Since this tool must run *before* any
daemon or raft state exists, it is the first genuinely standalone binary in
the repo.

## Decision

**A new package, `internal/install`, and a new binary, `cmd/apiaryinstall`.**
Every prerequisite becomes a `Check{ID, Description, Risk, Applicable,
Probe, Apply}` (`internal/install/install.go`). `Probe` always runs and
reports a `Result{Status, Detail, FixHint}` - `FixHint` is populated
whenever `Status != ok`, whether or not the check can fix itself, so a
manual-only finding is exactly as actionable to read as an automated one.
Commands are shelled via a narrow `Runner` interface (real `execRunner` in
production, `fakeRunner` in tests) - this package's own private exec
helper, matching every other `internal/*` package's stated convention of
not sharing one across package boundaries; the same interface-injection
style `internal/assumecheck`'s `raftReader`/`peerChecker` already use, and
the same exec-vs-pure-parser split `internal/netroute` uses for
testability without a real FreeBSD host.

**A three-tier risk model, not a single `-apply` flag:**

- **`RiskSafe`** - runs under a plain `-apply`. Idempotent, low-blast-radius
  changes only: `kldload`+persisting to `kld_list`, `pkg install`, `sysrc`
  assignments, `chmod 600 /etc/rc.conf`, appending the `apiary/*` anchor to
  `/etc/pf.conf` (after writing a `.bak` copy first - the first place in
  this codebase that mutates one of the three files ADR-0069 only ever
  reads, so the same backup-before-mutate caution applies with more force).
- **`RiskNetwork`** - creating/modifying a bridge interface and enslaving
  the uplink NIC to it. ADR-0022's own text describes a live incident where
  exactly this operation nearly cost the operator their SSH session
  ("rollback was cancelled with time to spare"). This tier never runs under
  plain `-apply`; it requires a separate flag whose *value* must exactly
  equal a confirmation phrase (`-apply-network yes-modify-network`),
  mirroring `cmd/raftd`'s `-reset`/`-restore` phrase-gate pattern exactly -
  the same "the safe path needs no phrase, the risky one needs an exact
  match or nothing happens" posture.
- **`RiskManualOnly`** - `Apply` is `nil`; the check only ever reports and
  prints a `FixHint`. This is a **permanent** boundary, not a v1 gap,
  mirroring `internal/hostconfig`'s own "not planned" framing: ZFS pool
  creation (disk layout is host-specific, Apiary has no business guessing
  it), PAM `/etc/pam.d/<service>` file existence and UNIX account
  existence for `-role-map` entries (identity/auth configuration stays
  operator-owned, always), and the `hastd_proto_recv_hdr` FreeBSD source
  patch for bug 298085 (ADR-0022) - a source rebuild, not something a
  preflight tool can safely automate.

**Default invocation is read-only.** No flags means "check everything
applicable and report" - no mutation, no confirmation phrase needed,
matching `raftd -restore-dry-run`'s already-established "the safe path
needs nothing" precedent. Exit code is non-zero if *any* check - including
`manual` ones - is not `ok`, so this can gate a bring-up script the way a
`doctor`/`terraform plan`-style tool does. `-json` emits the same `Result`
list as JSON for scripting.

**Explicitly out of scope, disclosed here rather than silently skipped:**
rc.d service scripts for the four daemons themselves (none exist in this
repo today, so "is `apiary_raftd` a known rc.d service" isn't yet a
checkable fact) and any Kubernetes-guest prerequisites (guest-image
concerns, not this host's).

## Consequences

- A fresh host's readiness is now a single command instead of manually
  cross-referencing a dozen ADRs, without touching the FreeBSD daemons'
  own established flag/`main()` conventions (stdlib `flag` only, no new
  CLI framework, no new shared exec-helper package).
- The riskiest operation this project's own history has already burned an
  operator's time on (bridge/uplink changes over SSH) gets its own explicit
  phrase gate, not lumped in with safe, purely-additive fixes.
- PAM, ZFS pool layout, and the `hastd` patch stay permanently
  operator-owned - `apiaryinstall` will always tell you they're missing,
  never attempt them, so there is no future expectation to walk back.
