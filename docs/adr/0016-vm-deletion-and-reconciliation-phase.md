# ADR-0016: Real VM deletion, and a reconciliation phase separate from desired_state

## Status

Accepted

## Context

`DeleteVM` has existed since the earliest external API work (ADR-0005),
but it only ever did one thing: remove the `VMDefinition` from raft's
ephemeral state. Once `internal/cluster`'s reconciler and `internal/bhyve`
started provisioning *real* resources for a VM (ADR-0012, ADR-0015), that
became a real bug, not just an incomplete feature - deleting a VM through
the web UI left its ZFS dataset and running bhyve VM orphaned on whatever
node owned it, with nothing left in raft's state to say they should be
cleaned up. This was caught directly: deleting a VM through the live web
UI on `apiarium` left a real dataset and a real running bhyve VM behind.

Separately, the web UI's VM table had a "State" column, but it only ever
echoed `desired_state` - what a caller asked for when creating the VM,
set once and never revisited. It could show "running" for a VM whose
dataset didn't exist yet, whose bhyve VM had failed to boot, or that had
already been fully torn down. There was no way to see the reconciler's
actual progress.

## Decisions

### Delete is a tombstone, not an immediate removal - but only when there's something to tear down

`DeleteVM`'s FSM handler now checks `node_id`. If it's empty, the VM was
never assigned to any node - there's no real resource to tear down, so
removing the record immediately (the original behavior) is still exactly
correct. If `node_id` is set, the record survives with
`desired_state = VM_STATE_DELETING` instead. The owning node's
reconciler notices this on its next tick, destroys the bhyve VM (if any)
and the ZFS dataset, and only then submits a new `PurgeVM` command to
remove the record entirely.

This keeps ADR-0012's create-only safety property intact rather than
undermining it: that ADR's guardrail was specifically about never
*inferring* removal from a VM's absence from a list (a fetch could be
partial or failed). Nothing here changes that - teardown only ever
happens for a VM that is still present in the list and carries an
explicit, caller-originated `VM_STATE_DELETING` marker. There's no new
inference, just a new explicit signal to act on.

### `UpdateVMPhase` and `PurgeVM` are reconciler-only commands

Both are part of `api/internalpb.Command`'s oneof, alongside
`CreateVM`/`UpdateVM`/`DeleteVM`, but neither is ever issued by an
external caller - only by a node's own `Reconciler`, using the same
`raftClient.Apply` path `CreateVM`/`DeleteVM` already go through from
`managerd`. There's no separate authorization boundary for this today
(matching the project's current no-auth-anywhere stage), but the
distinction is enforced by convention: `internal/manager`'s RPC-facing
`Server` never constructs these two command types.

### `VMPhase` is separate from `VMState` (desired_state)

`desired_state` stays exactly what it always was: the caller's request,
set once, never touched by the reconciler. `Phase` is new: the
reconciler's own observed progress, written back via `UpdateVMPhase` as
it works - `creating` before an attempt, `ready` once the dataset (and
bhyve VM, if configured) are confirmed to exist, `deleting` during
teardown, `error` (with `phase_error` set) if reconciliation fails. This
mirrors the real distinction between "what was requested" and "what's
actually true," the same distinction Kubernetes draws between spec and
status - collapsing them into one field would have made either the
create form's input or the table's real-time display wrong for the
other's sake.

### Phase updates are best-effort and read-back-gated

`Reconciler.applyPhase` swallows its own errors: a phase update is
status reporting, not the operation itself, so a transient raft failure
or a lost race against a concurrent purge shouldn't turn into a
reconciliation failure on its own account - the next tick just tries
again. Each tick also compares against the *previously observed* phase
(carried on `VMPlacement.Phase`, read back from the same `ListVMs` call
that started the tick) before writing anything, so a VM that's already
`ready` doesn't get a redundant `UpdateVMPhase` submitted to the raft log
every 30 seconds forever.

### `VMPlacement` stays plain Go types, not internalpb's

`Deleting bool` and `Phase string` were added to `VMPlacement` rather
than reusing `internalpb.VMState`/`VMPhase` directly, continuing the
existing pattern (ADR-0012) of keeping `plan.go`'s core types independent
of the wire schema. `reconciler.go` does the enum-to-string translation
at the boundary where it already talks to `internalpb` directly.

## Consequences

- Verified for real on `apiarium`: restarting the stack with this change
  replayed raftd's persisted log through the new FSM logic, which
  retroactively soft-deleted and then fully tore down a VM's dataset and
  bhyve VM that had been orphaned by the pre-fix `DeleteVM` behavior -
  the fix cleaned up its own prior damage as a side effect of normal
  operation, not a special migration step.
- A full live create/delete cycle through the actual web UI showed the
  State column progress through `pending -> ready`, then the row
  disappearing once teardown and purge completed on the next tick after
  clicking Delete - the real-time behavior this ADR set out to add.
- A VM assigned to a node that never comes back online stays tombstoned
  forever (`desired_state = VM_STATE_DELETING`, real resources
  untouched) - there's no forced/orphan-reclaim path, matching the
  project's existing caution about ever acting on uncertain state
  (ADR-0012). This is a known, accepted limitation, not an oversight.
- `desired_state`'s `VM_STATE_RUNNING`/`VM_STATE_STOPPED` distinction is
  still not enforced anywhere - the reconciler always provisions
  regardless of which one is set. That gap (already noted in CLAUDE.md)
  is unrelated to and unaffected by this change.
