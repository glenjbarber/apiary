package zfs

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// This file holds the last-sent/last-acked generation bookkeeping and
// the naming conventions a replication run depends on, with no raft,
// no policy and no scheduling.
//
// The generation counter is the thing ADR-0130 calls the actual
// split-brain fence: a stream carrying a generation not strictly
// greater than the recorded one is refused. Recording the counter
// itself is the FSM's job, and its wire home is a raft field that this
// task must not allocate (state field numbers are being allocated by
// several workers at once). So the TYPE and the ACCESSOR live here and
// the state.proto field is reported rather than added — see
// ReplStateFieldNote below and the commit message.

// ErrGenerationRollback is returned when something tries to record an
// acked generation lower than the one already recorded. It mirrors
// ADR-0130's rule that "RecordReplicationAck with a generation lower
// than last_acked_generation is rejected, so a late-arriving ack from a
// run whose lease lapsed can never roll the counter backwards".
var ErrGenerationRollback = errors.New("zfs: refusing to roll the acked generation backwards")

// ErrGenerationExhausted is returned when a uint32 generation counter
// would pass the largest generation the snapshot naming convention can
// represent. Reaching this is a many-million-run bug, and the right
// answer is to re-seed the policy deliberately, not to keep going.
var ErrGenerationExhausted = errors.New("zfs: replication generation counter exhausted")

// MaxGeneration is the largest generation a uint32 can hold. It is NOT
// the largest generation this package will mint: see
// MaxReplGeneration.
const MaxGeneration = math.MaxUint32

// MaxReplGeneration is the largest generation whose snapshot name is
// representable under ADR-0130's `apiary-repl-<gen:08d>` convention.
//
// The cap is load-bearing, not cosmetic. The name is 8 zero-padded
// decimal digits, and ParseReplSnapshotName is anchored to exactly 8
// digits, so a generation of 100000000 or more mints a name the parser
// can never read back. Since every derived name in this package — the
// one in LastAcked.RecordAck, the one in ReplSnapshotName, the one
// ObserveSource looks for — is minted FROM a generation and then parsed
// back, exceeding the cap would make every run believe its own last
// acked snapshot is gone and escalate to a full resend, forever. The
// counter stops at the cap instead.
//
// 99,999,999 runs of one policy is not a realistic workload, so the
// cost of this guard is a clean refusal at a boundary rather than a
// silent one inside it.
const MaxReplGeneration uint32 = 99999999

// LastAcked is the node's record of how far a replication stream has
// got. It is a plain value with no raft dependency, so it can live
// behind whatever the FSM stores it as.
//
// Snapshot is the base-relative "dataset@snapshot" of Generation. It is
// deliberately separate from the generation number rather than derived
// from it, because the generation is replicated state and the snapshot
// name is a local filesystem fact: after a raft snapshot restore the
// generation may be known while the snapshot is not, and the honest
// answer to "which snapshot do I send from" is then "I don't know",
// which escalates to a full send.
type LastAcked struct {
	// PolicyID is the policy this counter belongs to.
	PolicyID string
	// Dataset is the base-relative dataset being replicated.
	Dataset string

	// Generation is the highest generation the target has confirmed
	// holding. Only meaningful when HaveGeneration.
	Generation uint32
	// HaveGeneration distinguishes "generation 0 was acked" from
	// "nothing has ever been acked", which is what decides between a
	// full and an incremental send.
	HaveGeneration bool

	// Snapshot is the base-relative snapshot of Generation, e.g.
	// "vm-1@apiary-repl-00000007". Empty when unknown.
	Snapshot string
}

// NewLastAcked returns a record for a policy that has never completed a
// run: the first send must be a full one.
func NewLastAcked(policyID, dataset string) LastAcked {
	return LastAcked{PolicyID: policyID, Dataset: dataset}
}

// NewLastAckedAt returns a record for a policy whose target has
// confirmed holding the given generation and its snapshot.
func NewLastAckedAt(policyID, dataset string, gen uint32, snapshot string) LastAcked {
	return LastAcked{
		PolicyID:       policyID,
		Dataset:        dataset,
		Generation:     gen,
		HaveGeneration: true,
		Snapshot:       snapshot,
	}
}

// ReplSnapshot returns the base-relative snapshot name of a generation
// for this record's dataset, e.g. "vm-1@apiary-repl-00000007".
func (l LastAcked) ReplSnapshot(gen uint32) string {
	return l.Dataset + "@" + ReplSnapshotName(gen)
}

// NextGeneration returns the next generation to mint for this policy.
// It is a pure function of the current counter, refuses to wrap, and
// never skips: every run gets a fresh, strictly larger generation, and
// a generation is never reused (ADR-0130: "monotonically increasing,
// never reused").
func (l LastAcked) NextGeneration() (uint32, error) {
	if !l.HaveGeneration {
		return 1, nil
	}
	if l.Generation >= MaxReplGeneration {
		return 0, fmt.Errorf("%w: policy %s is at generation %d, and generation %d is the largest the %q snapshot convention can represent", ErrGenerationExhausted, l.PolicyID, l.Generation, MaxReplGeneration, ReplSnapshotName(0))
	}
	return l.Generation + 1, nil
}

// SourceState converts this record into the input PlanSend fences on.
// snapshotPresent is the caller's answer to "does Snapshot still exist
// on this source" — the only part the fence cannot answer for itself,
// because it is a question about a filesystem this package did not
// write.
func (l LastAcked) SourceState(snapshotPresent bool) SourceState {
	return SourceState{
		Dataset:                  l.Dataset,
		LastAckedGeneration:      l.Generation,
		HaveLastAcked:            l.HaveGeneration,
		LastAckedSnapshot:        l.Snapshot,
		LastAckedSnapshotPresent: snapshotPresent,
	}
}

// RecordAck folds a confirmed target ack into the record. The ack must
// be strictly greater than what is already recorded; a lower or equal
// one is refused with ErrGenerationRollback rather than applied,
// because a late ack from a run whose lease lapsed must never be able to
// move the counter backwards and re-open the fence window.
func (l LastAcked) RecordAck(gen uint32) (LastAcked, error) {
	if l.HaveGeneration && gen <= l.Generation {
		return l, fmt.Errorf("%w: policy %s has acked generation %d and was asked to record %d", ErrGenerationRollback, l.PolicyID, l.Generation, gen)
	}
	if gen == 0 {
		return l, fmt.Errorf("zfs: policy %s: generation 0 is not a valid acked generation (it is the 'nothing acked' sentinel)", l.PolicyID)
	}
	if gen > MaxReplGeneration {
		return l, fmt.Errorf("zfs: policy %s: generation %d has no representable %q snapshot name, so acking it would record a point this package can never find again", l.PolicyID, gen, SnapshotPrefix)
	}
	next := l
	next.Generation = gen
	next.HaveGeneration = true
	next.Snapshot = l.ReplSnapshot(gen)
	return next, nil
}

// FenceCheck reports whether gen may be sent now, without building a
// plan. It exists so a caller can refuse a stream at the door — the
// receiving node's check, per ADR-0130's "a stream carrying a
// generation lower than the target's recorded one is rejected with a
// fence error" — using exactly the same decision the source-side
// planner uses. One decision function, used on both ends, so the two
// ends cannot drift apart.
func (l LastAcked) FenceCheck(gen uint32) error {
	if l.HaveGeneration && gen <= l.Generation {
		return &FenceError{
			Reason:         FenceRefuseStale,
			Requested:      gen,
			LastAcked:      l.Generation,
			LastAckedValid: true,
			Dataset:        l.Dataset,
		}
	}
	return nil
}

// Lag is how many generations behind a target holding targetGen is, and
// whether that comparison is even meaningful. targetGenValid false
// means the target's held generation was not observed, and the lag is
// reported as unknown rather than as 0.
func (l LastAcked) Lag(targetGen uint32, targetGenValid bool) (uint32, bool) {
	if !l.HaveGeneration || !targetGenValid || targetGen > l.Generation {
		return 0, false
	}
	return l.Generation - targetGen, true
}

// StagingDatasetName returns the first-receive staging dataset name for
// a policy: `<base-relative>/.apiary-replication/<policy-id>`. ADR-0130
// stages a first full receive under this name and renames it to the
// target dataset only on success, so a failed first receive cannot
// leave a half-populated dataset occupying the final name where it
// would be indistinguishable from a real one.
//
// This function only produces the NAME. Creating it, receiving into it,
// and renaming it are the caller's, and the rename is deliberately not
// implemented here: it is a policy-lifecycle step, not a transport
// primitive, and a half-finished rename is worse than a missing one.
func StagingDatasetName(policyID string) (string, error) {
	if err := validateTokenPolicyID(policyID); err != nil {
		return "", err
	}
	return strings.Join([]string{StagingDatasetPrefix, policyID}, "/"), nil
}

// StagingDatasetPrefix is the directory every replication staging
// dataset lives under, relative to a Manager's Base. It is dot-prefixed
// so it does not collide with a Cell's own dataset name and sorts
// apart from them in `zfs list`.
const StagingDatasetPrefix = ".apiary-replication"

// ReplStateFieldNote is the raft state this package needs and
// deliberately does not define, because api/internalpb/state.proto
// field numbers are being allocated by several workers concurrently and
// this task is forbidden from allocating one. It is a constant rather
// than only a comment so that anything reporting the requirement -- a
// commit message, a startup log, an operator command -- can print the
// whole thing instead of pointing at a comment nobody reads.
const ReplStateFieldNote = `internal/zfs needs one new field in api/internalpb/state.proto, which it
must not allocate itself:

  message ReplicationProgress {       // the last-sent/last-acked point
    string policy_id = 1;
    uint32 last_acked_generation = 2;  // 0 with last_acked_known=false = none
    bool   last_acked_known = 3;      // must exist: 0 is a real sentinel
    string last_acked_snapshot = 4;   // base-relative "ds@snap", may be empty
  }

  // in FSMSnapshotState:
  map<string, ReplicationProgress> replication_progress = <N>;

N must be taken from whatever the next free number is when the field is
actually added. ADR-0130 named 10 and 11 for replication_policies and
replication_leases, and ADR-0133 has already claimed 10, so
replication_progress belongs after those and must be re-checked against
the live file rather than assumed. Generations are capped at
MaxReplGeneration (99999999) because the snapshot name is 8 digits.

The Go type and its accessor already exist here as LastAcked and
LastAcked.Snapshot, so the FSM side is a one-line conversion. The field
belongs in FSMSnapshotState for the same reason restart_leases does: a
raft snapshot restore or a "raftd -restore" seed must not silently drop
the recorded generation, or the fence window reopens on the next run
and a lapsed source can stream again.`
