# Prove resilience when `drone` is offline

This procedure checks whether the rest of the Colony keeps its control plane
available when `drone.lab3.apiary.work` becomes unreachable. It is an operator-led
failure exercise: the dashboard simulations are preflight evidence, and the
physical outage is the proof.

## What this can prove

A successful exercise can show that the surviving Raft voters retain quorum,
that an operator can still reach Apiary through another Comb, and that the
Colony returns to a healthy state after `drone` rejoins. Watchgoose can show
that `managerd` stopped reaching its ping endpoint within the configured check
grace period.

It does not prove automatic VM or jail failover. Apiary's HAST replication
provides data redundancy; a replicated guest does not automatically start on
its replica when its owner is lost. Keep guests on `drone` stopped or explicitly
migrated before testing a host outage, and verify their placement afterward.

## Go/no-go preflight

Use the Apiary UI through a Comb other than `drone`, so the evidence path
survives the test. Record the time and current leader.

1. Open **Resilience Coverage** (`/resilience-coverage`). Find the Hive-failure
   row for `drone.lab3.apiary.work` and the quorum-tolerance rows for every
   current voter. Treat **Lost** as a no-go. **Unknown** means the page lacks
   enough reachability evidence to predict the result; it is not a claim that
   the cluster will fail. A controlled one-Comb test can resolve that unknown
   when the membership and rollback conditions below are met.
2. Open **Recovery Handbook** (`/recovery-handbook?node_id=drone.lab3.apiary.work`).
   Review the quorum verdict and every workload recovery step. If Drone is the
   current leader, a reachability-based **Unknown** verdict means leader-local
   probes cannot predict the survivors' election; use the membership and
   controlled-test gates below to decide whether to proceed.
3. Confirm the **current Raft membership**, counting entries whose suffrage is
   `Voter` (the Comb rows on the coverage page are not proof of voter
   membership). For `N` voters, quorum is `floor(N/2)+1`. Do not proceed with
   two voters: losing either one ends quorum. Proceed with a one-Comb test only
   when there are three current voters, so the two survivors can form a
   majority, and you can restore Drone's network promptly if they do not.
   A simulation based on checks initiated by the current leader does not
   establish that the remaining voters can reach one another after that
   leader is gone.
4. Review **Maintenance** (`/maintenance`) or **Simulate** (`/simulate?node_id=drone.lab3.apiary.work`)
   as supporting evidence. These are read-only counterfactuals, not a physical
   rehearsal and not a substitute for the checks above.
5. Stop or explicitly migrate every VM and jail owned by Drone. Confirm that
   each guest needed during the test is running on a surviving Comb. Confirm
   any HAST-backed workload has a usable destination and current replication
   before relying on its data. Do not promote a HAST secondary by hand during
   this network-loss test.
6. Ensure you can reach the surviving Comb's UI and host console without
   relying on Drone. Know the Watchgoose check interval and grace period; the
   ping URL only reports that `managerd` reached Watchgoose, not guest health.

**Do not disconnect Drone** if current voter membership cannot be confirmed,
if the coverage result is **Lost**, if the cluster already has a voter or
workload fault, or if a required workload still runs only on Drone. An
**Unknown** coverage result alone does not block this controlled test once the
three-voter condition and every other preflight above are met.

## Controlled exercise

1. Capture the pre-test voter list, leader, guest placement, and healthy status
   from the surviving Comb. Record a timestamp.
2. For a network-isolation exercise, disconnect Drone's network cable. This
   tests loss of network reachability; it does not test host power loss. For a
   full host-loss exercise, use an out-of-band console to shut down or power off
   Drone. Do not use a cable pull as evidence that its local storage or processes
   are powered off.
3. From the surviving Comb, observe Raft membership, leader, quorum, and the
   status of the guests you intentionally left running. Record the time for any
   election and any interruption. Confirm that management operations still
   succeed through the surviving Comb.
4. Wait at least the configured Watchgoose check interval plus its configured
   grace period. Confirm the Watchgoose alert, if this is part of the test, and
   record when it fires. Apiary's default ping interval is five minutes, but the
   Watchgoose-side schedule and grace are configured separately.
5. Restore Drone's network or power. Wait for it to rejoin and catch up. From a
   surviving Comb, confirm all expected voters are present, Raft has one leader,
   guests are healthy and correctly placed, and Watchgoose records recovery.
6. Save the before/during/after observations with timestamps. A successful
   quorum check proves control-plane continuity for this topology and test; it
   does not establish automatic guest failover or cover other failure domains.

## Stop and recovery conditions

If the surviving voters lose quorum, stop making Raft-mutating changes and
restore Drone's network or power. If Drone does not rejoin, preserve the
remaining Raft state and use the Offline Recovery Handbook's applicable
recovery steps. Do not reset Raft state, manually promote a HAST secondary, or
start duplicate guests as an improvised recovery action.
