# ADR-0190: `apiaryctl` is installed in `$LOCALBASE/sbin`

## Status

**Accepted.** This record decides where the operator command line is
installed.

Supersedes: None
Superseded by: None
Affected projects: Apiary
Amends: ADR-0136 (the local CLI), which decided the opposite
Affected code: `Makefile`'s `install` rule

## Context

### What ADR-0136 decided, and why it was right then

ADR-0136 built `apiaryctl` as a single subcommand — `force-restart` —
and installed it at `/usr/local/libexec/apiary/apiaryctl`, alongside
the daemons. Its reasoning was explicit:

> No symlink into `/usr/local/bin`; the project keeps daemons under
> `libexec/apiary` and a root-privileged tool in `bin` widens the
> accidental-invocation surface.

That was a reasonable reading of what `apiaryctl` then was: one
root-only subcommand, invoked in the middle of an incident, whose
whole safety property was that you could not run it by accident.

### What `apiaryctl` became

`apiaryctl` is now eleven subcommands, and they split into two
groups:

- **Root-only, touching the host**: `install`, `force-restart`,
  `colony-ca`, `cert-setup`, `restart-guardrail-provision`,
  `peer-credential-backfill`, `migrate-tls-dir`, `pin-trusted-peers`.
- **Not root, and run where there is nothing else**: `join-introduce`
  and `join-authorize`. These are the two an operator runs on a
  **headless** Comb — a Comb with no source checkout, no README, and
  no other reason to know anything about Apiary's filesystem layout.

The second group is the problem the original decision did not
anticipate. A tool whose reason to exist is being run on a host with
nothing else on it should not be hidden in a directory whose name the
operator would have to know.

## Decision

**`apiaryctl` is installed at `/usr/local/sbin/apiaryctl`.**

It is no longer in `INSTALL_SRCS` — the daemons' list. The `install`
rule has its own step for it, so the two halves of the install are
distinguishable by where they live:

| Path | What |
| --- | --- |
| `/usr/local/libexec/apiary/` | the four daemons, `apiaryinstall`, `apiary-mcp` |
| `/usr/local/sbin/apiaryctl` | the one command an operator types |

`$LOCALBASE/sbin` is where a system puts a root-privileged
administrative command, and it is on root's PATH.

### The accidental-invocation surface

The original objection was that `bin` widens the accidental-invocation
surface. That is a real cost, and it is bounded here:

- **`sbin` is not on an unprivileged user's PATH by default.** It is
  on root's, and every subcommand that could do damage already
  refuses to run without root.
- **Nothing that could do damage runs unprivileged.** `install`,
  `force-restart`, `colony-ca` and `cert-setup` all check for root
  before touching anything.
- **The daemons are unaffected.** They stay in `libexec/apiary`, so
  they remain off the operator's PATH entirely.

The cost is real and accepted: a root shell can now type `apiaryctl`
and get something. That is the point — an operator in a root shell on
a headless Comb should be able to.

### The upgrade path

**A Comb that already has an `apiaryctl` in `libexec/apiary` keeps
it.** Nothing removes it, because an install that silently deleted a
binary some other tool might still reference would be a worse problem
than a stale copy sitting in a directory nothing points at any more.

A fresh install, and `make install` on an existing Comb, put the new
copy in `sbin`.

## What this ADR does not change

- **No subcommand's behaviour, argument, or output.** Only where the
  binary lands, and every path that refers to it.
- **The daemons' paths.** `raftd`, `managerd`, `frontend`,
  `restshimd`, `apiaryinstall` and `apiary-mcp` all stay in
  `/usr/local/libexec/apiary`.
- **`apiaryinstall`'s path.** It stays with the daemons: it is a
  host-preparation tool that the install process itself runs, not a
  command an operator types on a running Comb.
- **The port's layout.** A packaging skeleton for the Ports
  Collection has to place `apiaryctl` in `sbin` too, and its
  `pkg-plist` reflects that.

## Consequences

- An operator on a headless Comb can type `apiaryctl` without
  knowing where Apiary puts its binaries.
- The two halves of the install are distinguishable by directory: the
  daemons are in `libexec/apiary`, and the one command an operator
  types is in `sbin`.
- Every message in the codebase that tells an operator how to run
  `apiaryctl` now names the path that will actually work.

## Validation

- `internal/forcerestart`: the install rule installs `apiaryctl` to
  `/usr/local/sbin`, and does **not** also list it in `INSTALL_SRCS` —
  which would put two copies on the host that drift apart.
