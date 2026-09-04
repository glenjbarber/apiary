# ADR-0054: Image availability in node-failure simulation

## Status

Accepted

## Context

ADR-0052 reports quorum, ownership, and HAST-placement consequences when a
Hive disappears. VM installer and base images are node-local physical data,
not raft state. ADR-0041 can fetch a missing image from another Hive, but only
if at least one reachable peer actually has the named file. A running VM does
not need its source image to continue running, while later provisioning or
recovery may depend on it.

## Decision

Extend `SimulateNodeFailure` rather than add a disconnected image scenario.
For every VM that references `iso_name` or `base_image_name`, directly query
the image inventory of each remaining raft member and report one of:

- `AVAILABLE`: at least one remaining Hive confirms the named image;
- `UNKNOWN`: no source is confirmed, but at least one remaining inventory
  could not be read; or
- `UNAVAILABLE`: every remaining inventory was read and none contains it.

The failed Hive is excluded even if its inventory is still reachable while the
simulation runs. A peer error is unknown, never an empty inventory. Results
name confirmed source Hives and Hives whose inventories were unreadable.
Queries reuse `ListISOs`, are bounded by the simulator's existing three-second
peer timeout, and remain read-only.

The `/simulate` Hive-loss report gains an image-availability section. Its copy
states that the finding concerns future provisioning or recovery and does not
claim that a running Cell stops. Jails are absent because `JailDefinition`
currently has no image reference.

## Consequences

- Operators can see when losing a Hive also removes the last confirmed source
  for an image required to recreate a Cell.
- One confirmed remaining source establishes availability even when another
  inventory is unknown.
- Image identity remains name-based because VM definitions store a name, not a
  hash. Detecting conflicting same-name images is a separate future issue.
- The report does not prove that a source will remain reachable during an
  actual recovery, nor that sufficient capacity exists to recreate the Cell.
