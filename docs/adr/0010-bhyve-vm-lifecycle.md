# ADR-0010: internal/bhyve VM lifecycle

## Status

Accepted

## Context

`internal/bhyve` is the last of the four FreeBSD-specific packages, and
the first tested against real hardware-assisted virtualization
(`apiarium`, an ASUS P8Z77-V LX / i7-2700K running FreeBSD 16.0-CURRENT)
rather than a VM — nested virtualization can't provide what bhyve itself
needs. This slice proves create/destroy/list, following the same
"lifecycle only, no disk/network yet" scope `internal/zfs` and
`internal/jail` used for their first slices.

## Decisions

### VM names are scoped by a prefix, like `internal/jail`, not a hierarchical `Base`

bhyve VMs show up flatly under `/dev/vmm/<name>` — there's no
containment hierarchy the way ZFS datasets have under a `Base` path.
`Manager.Prefix` is the same convention `internal/jail` already
established for exactly this reason (see ADR-0007).

### No disk or network in v1

A VM boots with just a host bridge, LPC device, and a UEFI boot ROM —
nothing to actually run an OS. Real disk support (backed by
`internal/zfs` datasets) and networking (tap/bridge devices) are
separate future slices, once there's a concrete need driving their
design (e.g. `CreateVM` provisioning an actual bootable disk). This
mirrors how `internal/zfs`/`internal/jail` proved their primitives
before wiring in anything else.

### `bhyve` itself is launched detached via `daemon(8)`, not as a child process

This is the one place `internal/bhyve` differs structurally from
`internal/zfs`/`internal/jail`: those packages' commands (`zfs`,
`jail -c`) each perform one action and exit; `bhyve` *is* the running
VM — it stays alive for the VM's entire lifetime. If `CreateVM` started
it as an ordinary child process, the VM would die the moment whatever
called `CreateVM` (eventually `managerd`, or a test binary) exited or
restarted — unacceptable for a real hypervisor manager, whose own
restarts must never take down the VMs it manages.

`daemon(8)` (FreeBSD's standard detachment utility — double-forks and
redirects I/O) with a `-p <pidfile>` is used instead, so the VM process
fully reparents to init and survives independently. `DestroyVM` reads
that pidfile to stop the process after tearing down the vmm context via
`bhyvectl --vm=<name> --destroy`.

### Requires root, like `internal/jail`

`vmm(4)` access has no delegation mechanism (same as jail creation) —
tests run as root on `apiarium`, not a delegated unprivileged user, the
same reasoning ADR-0007 used for `internal/jail`.

## Consequences

- Wiring a real disk into `CreateVM` later will need a `-s N,ahci-hd,<path>`-style
  argument sourced from an `internal/zfs`-backed dataset/zvol path — the
  `Config` struct will grow, not change shape.
- Networking (tap/bridge) is a real design question of its own (per-VM
  IP allocation is exactly the kind of small ephemeral-state fact
  `api/internalpb/state.proto`'s `VMDefinition` already anticipates,
  per ADR-0004) — deliberately deferred, not overlooked.
- `DestroyVM`'s pidfile-based stop is a hard kill, not a graceful ACPI
  shutdown request to the guest OS — acceptable for now since v1 boots
  no OS at all; revisit once real guest boot is wired in, since a hard
  kill mid-write to a real disk is a correctness concern a graceful
  path would avoid.
