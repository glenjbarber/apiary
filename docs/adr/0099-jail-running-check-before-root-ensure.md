# ADR-0099: Check a jail's running state before touching its dataset/root

## Status

Accepted

## Context

Deploying ADR-0098 (jail base archives and the empty-root safety check)
surfaced a real, currently-live production problem within hours:
`apiverse`'s `freebsd-sync6`, `apiarium`'s `freebsd-sync1`, and
`apiarium`'s `www0` all began failing reconciliation every tick with
`jail "<id>"'s root filesystem (/zroot/apiary/<id>) is empty` - even
though all three jails were, and remained, genuinely healthy and
running the entire time.

The actual cause predates ADR-0098 and isn't specific to it:
`ensureJail` (`internal/cluster/jail.go`) checked whether the jail's
ZFS dataset existed, created one if not, and ensured its root before
ever checking whether `jail(8)` already reports the jail as running -
that check happened last, right before the (now unreachable, since the
dataset work already ran) `CreateJail` call. For any jail whose root
was never actually managed as an Apiary `zroot/apiary/<id>` dataset in
the first place - a real, live case for a jail adopted into this
Colony's raft state that predates Apiary ever provisioning it -
`DatasetExists` correctly reports false every single tick, and the
reconciler would try to create one. Before ADR-0098, this silently
created an unused, empty ZFS dataset nobody ever looked at again -
wasteful, but invisible, since the actual running jail was never
touched. ADR-0098's own new unconditional empty-root check turned that
long-standing silent waste into a loud, recurring reconciliation
failure instead, even though the jail itself was never broken.

## Decision

`ensureJail` now checks `Jail.JailExists` first, immediately after the
existing provisioning-disabled guard, before any dataset lookup,
template/archive validation, or HAST device work runs at all. A jail
`jail(8)` already reports running short-circuits with no further work
this tick - there is nothing left to ensure for a jail already
satisfying its own definition, regardless of whether its dataset
matches Apiary's own naming convention. The former, now-redundant
`JailExists` check just before `CreateJail` at the end of the function
is removed.

This is a pure reordering, not a new capability or a relaxed check:
every existing validation (`base_template`/`replica_node_id` mutual
exclusion, `base_template`/`base_archive_name` mutual exclusion,
dataset creation, HAST provisioning, the empty-root safety check
itself) still runs in full, unchanged, for any jail that is *not*
already running - including a jail that stops and needs to be
recreated. The only behavior change is skipping all of it for a jail
already confirmed running, which is the correct action regardless of
ADR-0098: reconciling an already-satisfied resource should do nothing,
the same posture every other resource type in this reconciler already
takes.

**Not extended to `ensureVM`.** The equivalent VM path
(`internal/cluster/reconciler.go`'s `ensureVM`) has the identical
ordering, but there is no evidence it causes the same problem in
practice - a bhyve VM has no realistic "adopted, already running, no
Apiary-managed disk dataset" scenario the way a pre-existing FreeBSD
jail does. Left unchanged rather than fixed speculatively; revisit if
a real case surfaces.

## Consequences

- `freebsd-sync6`/`freebsd-sync1`/`www0` (and any other adopted jail in
  the same situation) stop failing reconciliation immediately once this
  ships, with zero configuration or intervention needed on any of them.
- A genuinely new, empty-rooted jail (freshly created, dataset doesn't
  exist yet, not yet running) is completely unaffected - `JailExists`
  correctly reports false for it, so it falls through to the same
  dataset-creation and empty-root-check path as before.
- New regression test,
  `TestReconciler_RunOnce_SkipsJailAlreadyRunningWithNoManagedDataset`,
  proves the exact live scenario: a running jail with no dataset at all
  triggers neither `CreateDataset` nor `CreateJail`.

## Verification

1. `internal/cluster/jail_test.go` - new test above; full existing jail
   test suite re-run to confirm the reordering changes no other
   behavior (dataset creation, base template cloning, base archive
   extraction, HAST provisioning, deletion, disabled-provisioning
   paths all still pass unchanged).
2. `go build ./...`, `go vet ./...`, `go test ./...`, `gofmt -l .` all
   clean.
3. Live: deploy to `apiverse`/`apiarium`, confirm the recurring
   `freebsd-sync6`/`freebsd-sync1`/`www0` empty-root errors stop
   appearing in `managerd`'s log within one reconcile interval.
