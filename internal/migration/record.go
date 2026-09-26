// Package migration implements ADR-0128's durable migration record: the
// raft-journalled account of one in-flight attempt to move a guest
// (a VM or a jail) from one cluster node to another.
//
// ADR-0128 itself was written against assumptions that did not survive
// contact with the code, and this package is shaped by the corrected
// facts rather than by the ADR's sketch:
//
//   - MigrateVM and MigrateJail already exist (api/rpc/manager.proto)
//     and target a replica_node_id. They are a cold ownership transfer
//     between two nodes that already hold identical data, and they are
//     deliberately kept as-is. Nothing here changes their contract.
//   - There is no guest_type field to copy and no generic CRD platform
//     underneath. The raft state shape is flat and map-backed
//     (internalpb.FSMSnapshotState is a struct of maps), so a migration
//     record is one more map plus one index map, and the conversions in
//     statewire.go mirror exactly that.
//   - A "Cell" in this codebase is a workload, not a failure-domain
//     grouping: VMDefinition and JailDefinition are the only two things
//     that can be migrated, and each carries its own node_id.
//   - Raft stores desired state and safe aggregate evidence. It never
//     stores guest memory, open handles, in-flight I/O, or key
//     material. Nothing in this package is in that category: the
//     heaviest thing a Record carries is a byte count and two opaque
//     incremental-send resume tokens, both of which are aggregate
//     provenance about a copy, never the copy.
//
// The three things this package refuses to do, stated up front because
// each one is a decision a reader should be able to check:
//
//  1. It never treats silence as success. A migration whose completion
//     was never observed is a distinct, named outcome (OutcomeUnobserved
//     / OutcomeUnverified), it is never rendered as a success, and it
//     keeps the guest fenced (see fence.go) precisely because we do not
//     know where the guest is.
//  2. It never lets a caller drive the record to an illegal phase or an
//     unbacked outcome. Every mutation goes through command.go, which
//     rejects forwards, rewinds, replays and stale tokens with distinct,
//     citable errors rather than absorbing them.
//  3. It never records a quorum problem as a migration failure. Losing
//     or being unable to read quorum means the record must halt; it says
//     nothing about whether the migration worked. See quorum.go.
//
// Everything here is pure computation over already-gathered facts: no
// raft reads, no RPCs, no FreeBSD-specific effects. The RPC handler in
// internal/manager gathers, this package decides, and the raft FSM
// applies - the same separation internal/cluster/simulate.go and
// internal/guardrail already establish.
package migration

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// WorkloadKind is which kind of guest a migration is moving. It is a
// deliberate local duplicate of internal/cluster.ResourceKind and NOT an
// import of it: this package is pure computation with no dependencies,
// while internal/cluster is OS-exec-heavy, and the established
// convention in this codebase when a small vocabulary crosses a package
// boundary is a documented duplicate rather than an import (see
// internal/health.Reachability and internal/invariant.Reachability,
// which are two more copies of the same three values on purpose).
type WorkloadKind string

const (
	WorkloadKindVM   WorkloadKind = "vm"
	WorkloadKindJail WorkloadKind = "jail"
)

// Valid reports whether k is a kind this package can migrate. Anything
// else is refused at validation rather than defaulted, because ADR-0128
// names VM and jail and no third thing; silently coercing an unknown
// kind to a VM would journal a migration of something this code has
// never heard of.
func (k WorkloadKind) Valid() bool {
	return k == WorkloadKindVM || k == WorkloadKindJail
}

// Workload is the guest being moved. It is identity only: what is
// running, how fast, and where its files are is node-local observation
// and never part of this record.
type Workload struct {
	Kind WorkloadKind
	ID   string
}

func (w Workload) String() string { return string(w.Kind) + "/" + w.ID }

func (w Workload) valid() bool { return w.Kind.Valid() && strings.TrimSpace(w.ID) != "" }

// Phase is where a migration is in ADR-0128's six-step protocol. Phase
// is deliberately NOT an enum that also carries the terminal states.
//
// ADR-0128's own sketch put COMPLETE and ABORTED in the same enum as
// the six in-flight steps. That is rejected here, and the reason is the
// whole point of requirement 4: a single phase enum cannot distinguish
// "we saw the guest come up on the target" from "we stopped looking"
// without overloading one value, and every frontend that renders the
// value would then have to guess which one it got. Phase answers only
// "which step is the migration on"; Outcome (see outcome.go) answers
// "what has actually been established about it", and the two are
// orthogonal - a record sitting at cutover is in_progress, failed, or
// completion-never-observed, and nothing in the phase alone says which.
type Phase string

const (
	PhasePreflight Phase = "preflight"
	PhaseFreeze    Phase = "freeze"
	PhaseBulk      Phase = "bulk"
	PhaseSync      Phase = "sync"
	PhaseCutover   Phase = "cutover"
	PhaseTeardown  Phase = "teardown"
)

// PhaseOrder is the canonical forward order, exported so a caller
// (a frontend progress bar, a test) asserts against it rather than
// re-deriving it from the transition table.
var PhaseOrder = []Phase{
	PhasePreflight,
	PhaseFreeze,
	PhaseBulk,
	PhaseSync,
	PhaseCutover,
	PhaseTeardown,
}

// Valid reports whether p is one of the six real phases.
func (p Phase) Valid() bool {
	for _, known := range PhaseOrder {
		if p == known {
			return true
		}
	}
	return false
}

// Index is p's position in PhaseOrder, or -1 for an unknown phase.
// Callers must not treat -1 as "phase 0".
func (p Phase) Index() int {
	for i, known := range PhaseOrder {
		if p == known {
			return i
		}
	}
	return -1
}

// IsLast reports whether p is the final in-flight phase. Entering it is
// not the same as the migration being done: the record is still in
// flight until an outcome is settled with evidence.
func (p Phase) IsLast() bool { return p == PhaseTeardown }

// Timestamps records when each phase was entered. A zero value means
// that phase was never entered - which is a real fact about the
// record, not a missing field, and is why these are a struct of int64
// rather than a map keyed by phase (a map would let a caller "look up"
// a phase that never happened and get a zero time either way).
//
// FirstEnteredUnix is immutable once set: it answers "since when has
// this guest been frozen", which is the question the ADR's bounded-freeze
// guarantee is expressed in. LastEnteredUnix moves on every retry of the
// same phase, so an operator can see that a phase was attempted more
// than once without losing the original timestamp.
type Timestamps struct {
	FirstEnteredUnix map[Phase]int64
	LastEnteredUnix  map[Phase]int64
	CreatedUnix      int64
	UpdatedUnix      int64
	// SettledUnix is when the outcome last stopped being
	// in_progress. Zero while the record is still live.
	SettledUnix int64
}

func newTimestamps(createdUnix int64) Timestamps {
	return Timestamps{
		FirstEnteredUnix: map[Phase]int64{},
		LastEnteredUnix:  map[Phase]int64{},
		CreatedUnix:      createdUnix,
		UpdatedUnix:      createdUnix,
	}
}

// normalize makes the maps usable on a zero-valued Timestamps, so a
// Record built by hand in a test (or decoded from a wire record that
// omitted the maps entirely) cannot panic on the first write.
func (t *Timestamps) normalize() {
	if t.FirstEnteredUnix == nil {
		t.FirstEnteredUnix = map[Phase]int64{}
	}
	if t.LastEnteredUnix == nil {
		t.LastEnteredUnix = map[Phase]int64{}
	}
}

// Entered records an entry into p at atUnix. first keeps the earliest
// timestamp for a phase across any number of retries, so freezing a
// guest, backing off, and freezing again does not make the guest look
// less-frozen than it is.
func (t *Timestamps) Entered(p Phase, atUnix int64) {
	t.normalize()
	if existing, ok := t.FirstEnteredUnix[p]; !ok || atUnix < existing {
		t.FirstEnteredUnix[p] = atUnix
	}
	t.LastEnteredUnix[p] = atUnix
	if atUnix > t.UpdatedUnix {
		t.UpdatedUnix = atUnix
	}
}

// Since returns how long ago p was first entered, and whether it was
// ever entered at all. A phase that was never entered reports false
// rather than a duration measured from the zero time, which would be a
// fabricated fifty-plus-year outage.
func (t Timestamps) Since(p Phase, now time.Time) (time.Duration, bool) {
	first, ok := t.FirstEnteredUnix[p]
	if !ok || first == 0 {
		return 0, false
	}
	return now.Sub(time.Unix(first, 0)), true
}

// Transfer is the aggregate provenance of the copy: how much of it was
// scheduled, how much was actually moved, and where an interrupted
// incremental send should resume from.
//
// Everything here is safe to replicate. FromToken/ToToken are the
// opaque incremental-send resume tokens the storage layer hands back
// (ADR-0128 §Storage handoff); they describe a position in a stream,
// they are not credentials and not key material. BytesMoved is an
// aggregate. None of it is the guest's data, and none of it can bring a
// guest back to life by itself - a Record with a perfectly complete
// Transfer and no observed outcome still does not mean the guest
// started on the target.
type Transfer struct {
	// RequiredBytes is the size the bulk transfer was told to move.
	// Zero is legitimate and common: a target that is already a synced
	// HAST replica has nothing to receive, which ADR-0128 calls a
	// genuine no-op bulk rather than a phase to skip.
	RequiredBytes uint64
	// MovedBytes is how much was actually moved. It is clamped to
	// RequiredBytes by Record.Validate; a moved count larger than the
	// requirement is a lie about progress, not a fast link.
	MovedBytes uint64
	// FromToken and ToToken bound the last completed incremental send,
	// so a dropped connection resumes instead of restarting.
	FromToken uint64
	ToToken   uint64
	// TargetAlreadyReplica records the preflight finding that the
	// target already holds a synced copy (ADR-0128's v1 default path).
	// It is recorded rather than inferred, because "the target already
	// had it" and "we just sent it" lead to the same RequiredBytes of 0
	// and must not be confused afterwards.
	TargetAlreadyReplica bool
}

// Complete reports whether the scheduled copy finished. It is a fact
// about bytes, never about the guest running - see the type comment.
func (t Transfer) Complete() bool {
	return t.RequiredBytes > 0 && t.MovedBytes >= t.RequiredBytes
}

// Verification is what the target node actually reported after the
// ownership swap: one direct observation of a running guest, with the
// two things that prove it is the right guest rather than some other
// thing that happens to share the name.
//
// Observed is the load-bearing field. false means the query was
// attempted and did not return a usable answer (a timeout, a refused
// connection, a target that answered with a phase it does not
// understand). That is never evidence of failure, and Outcome derives
// completion_unobserved from exactly this.
type Verification struct {
	Observed     bool
	GuestRunning bool
	ObservedGUID string
	ExpectedGUID string
	ObservedIP   string
	ExpectedIP   string
	ObservedAt   int64
	QueryError   string
	ObservedOn   string // the node that answered
}

// Matches reports whether the observation is of the guest we moved: a
// running guest whose dataset GUID and preserved IP both match. A
// running guest with the wrong GUID is not a partial success, it is a
// wrong answer, and settle.go treats it as a failure with evidence
// rather than quietly calling it complete.
func (v Verification) Matches() bool {
	return v.Observed && v.GuestRunning &&
		v.ObservedGUID != "" && v.ObservedGUID == v.ExpectedGUID &&
		v.ObservedIP != "" && v.ObservedIP == v.ExpectedIP
}

// Stalled explains why a record stopped advancing without becoming
// terminal. A stalled record is still in flight: the guest is still
// frozen, still fenced, and the operator still owes it an abort.
//
// This exists as a first-class field because the ADR's honest answer to
// "the link died during bulk" is not an error, it is a held record with
// a reason. Without a place to write that reason, the only two options
// are to keep retrying silently (which hides a stuck guest behind a
// spinner) or to fail the record (which claims the migration broke when
// in fact nobody knows).
type Stalled struct {
	Reason    string // stable id, e.g. "quorum-lost", "target-unreachable"
	Detail    string
	Verdict   string // recovery.QuorumVerdict's own string, for quorum stalls
	SinceUnix int64
}

// Evidence is one cited, timestamped reason behind a decision. It is a
// small local copy of internal/invariant.Evidence for the same reason
// internal/invariant keeps its own Reachability: this package has no
// dependencies and the type is three fields.
//
// ObservedAtUnix is zero when the fact was never actually observed, and
// that is left visible rather than filled in with "now". A fabricated
// observation time on a migration record is exactly the quiet lie this
// feature must not ship.
type Evidence struct {
	Rule           string
	Detail         string
	ObservedAtUnix int64
}

// Record is the durable, raft-replicated account of one migration
// attempt. It is the thing a new leader reads to know what it inherited,
// and the thing a reconciler reads to know it must not touch the guest.
//
// It deliberately contains no guest runtime state: no memory, no open
// handles, no in-flight I/O, no keys, no dataset contents, no bhyve or
// jail process. Raft's guarantee is that every replica applies the same
// log in the same order. It is not, and cannot be, a guarantee about
// two independent FreeBSD kernels running the same guest - see
// ADR-0128's central design commitment and Rejected alternative 1.
type Record struct {
	ID string

	Guest    Workload
	SourceID string
	TargetID string

	Phase   Phase
	Outcome Outcome

	// Attempt counts entries into a phase, starting at 1. It is what
	// makes a record's own history legible ("bulk, attempt 3") after a
	// migration that has retried through a flaky link.
	Attempt uint32

	// Token is the per-phase-attempt token. It is minted by the leader
	// on every phase entry and every retry, strictly increasing, and
	// must be presented by any peer-side report for this record. Its
	// only job is to make a delayed RPC from attempt N-1 unable to act
	// during attempt N. It is NOT a ZFS resume token - those are
	// Transfer.FromToken/ToToken, which are a different thing entirely
	// and are named so the two are never confused.
	Token uint64

	Timestamps Timestamps
	Transfer   Transfer

	// Verification is the target's own last report. Storing it on the
	// record is what lets a settled outcome keep its evidence after
	// the fact, instead of a record that says "complete" with nothing
	// behind it.
	Verification Verification

	// Detail is the human-readable reason for the current outcome or
	// stall. It is never the evidence: Evidence is.
	Detail string

	// Evidence is the cited basis for the current outcome or stall,
	// oldest first. An outcome with no evidence cannot be settled -
	// see Settle.
	Evidence []Evidence

	Stall *Stalled
}

// Clone returns a deep-enough copy for a caller to mutate freely
// without disturbing the record another caller holds. The maps inside
// Timestamps and the Evidence slice are copied; the scalar fields and
// the Verification/Transfer values are copied by assignment.
func (r Record) Clone() Record {
	out := r
	t := Timestamps{
		CreatedUnix:      r.Timestamps.CreatedUnix,
		UpdatedUnix:      r.Timestamps.UpdatedUnix,
		SettledUnix:      r.Timestamps.SettledUnix,
		FirstEnteredUnix: make(map[Phase]int64, len(r.Timestamps.FirstEnteredUnix)),
		LastEnteredUnix:  make(map[Phase]int64, len(r.Timestamps.LastEnteredUnix)),
	}
	for k, v := range r.Timestamps.FirstEnteredUnix {
		t.FirstEnteredUnix[k] = v
	}
	for k, v := range r.Timestamps.LastEnteredUnix {
		t.LastEnteredUnix[k] = v
	}
	out.Timestamps = t
	out.Evidence = append([]Evidence(nil), r.Evidence...)
	if r.Stall != nil {
		s := *r.Stall
		out.Stall = &s
	}
	return out
}

// InFlight reports whether the migration has not settled yet.
func (r Record) InFlight() bool { return r.Outcome == OutcomeInProgress }

// IsTerminal reports whether the outcome will never change again. A
// stalled-but-unsettled record is NOT terminal - it is in flight with a
// reason, and the operator's abort is still pending.
func (r Record) IsTerminal() bool { return r.Outcome.IsTerminal() }

// Frozen reports whether the record has reached the freeze step, which
// is the point at which the guest is an outage rather than merely busy.
// Used by the bounded-freeze check and by anything that wants to say
// "this migration is costing the guest its uptime".
func (r Record) Frozen() bool {
	i := r.Phase.Index()
	return i >= 0 && i >= PhaseFreeze.Index()
}

// OwnershipMoved reports whether the record has reached cutover, which
// is the point at which the guest's recorded owner is the target rather
// than the source.
//
// This is the boundary ADR-0128's abort is defined against. "Return the
// guest to the source and run it there" is a well-defined action while
// the guest is still the source's - frozen, copied, half-synced, all
// reversible. After the ownership swap it is not: raft says the target
// owns the guest, the source holds a dataset nobody is meant to start,
// and moving it back is itself a migration with its own preflight,
// fence and quorum gate. So the two are separate predicates on purpose:
// a guest can be frozen (an outage) and still be abortable (not yet
// somebody else's).
func (r Record) OwnershipMoved() bool {
	i := r.Phase.Index()
	return i >= 0 && i >= PhaseCutover.Index()
}

// Fenced reports whether the ordinary reconcilers must leave this guest
// alone right now. See fence.go for why an unobserved completion still
// fences.
func (r Record) Fenced() bool { return FenceHeld(r) }

// Validate is the record's own integrity check, run on every decode and
// on every mutation before it is returned to a caller. It is the reason
// a hand-built or replayed wire record cannot put the state machine
// somewhere it does not belong: the transition rules answer "is this
// move legal", and Validate answers "is this record even coherent".
func (r Record) Validate() error {
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("migration: record has no id")
	}
	if !r.Guest.valid() {
		return fmt.Errorf("migration: record %s has no usable workload identity (kind %q id %q)", r.ID, r.Guest.Kind, r.Guest.ID)
	}
	if strings.TrimSpace(r.SourceID) == "" {
		return fmt.Errorf("migration: record %s has no source node", r.ID)
	}
	if strings.TrimSpace(r.TargetID) == "" {
		return fmt.Errorf("migration: record %s has no target node", r.ID)
	}
	if r.SourceID == r.TargetID {
		// Refused rather than tolerated: a self-migration is a
		// cutover with no second node, which is the single most
		// dangerous shape this protocol has (it freezes a guest and
		// then stops it with nowhere to start it). Making it
		// unrepresentable here means a caller cannot construct one
		// by mistake and have it survive a round trip.
		return fmt.Errorf("migration: record %s has the same source and target node %q - there is nowhere to move the guest to", r.ID, r.SourceID)
	}
	if !r.Phase.Valid() {
		return fmt.Errorf("migration: record %s has invalid phase %q", r.ID, r.Phase)
	}
	if r.Outcome == "" {
		return fmt.Errorf("migration: record %s has no outcome - a record with no outcome cannot be told apart from a corrupt one", r.ID)
	}
	if !r.Outcome.Recognized() {
		return fmt.Errorf("migration: record %s has unrecognized outcome %q", r.ID, r.Outcome)
	}
	if r.Timestamps.CreatedUnix == 0 {
		// "When did this migration start" is not decoration: it is the
		// first thing an operator asks about a stuck guest, and a zero
		// here is 1970 rather than a real answer.
		return fmt.Errorf("migration: record %s has no creation time", r.ID)
	}
	if r.Attempt == 0 {
		return fmt.Errorf("migration: record %s has attempt 0 - every record has been through at least one phase entry", r.ID)
	}
	if r.Token == 0 {
		// Zero is never a legal token: a report that presents 0 is
		// presenting nothing, and "nothing matches nothing" is
		// precisely the check this token exists to perform.
		return fmt.Errorf("migration: record %s has token 0 - no report may present an unallocated token", r.ID)
	}
	if r.Transfer.RequiredBytes > 0 && r.Transfer.MovedBytes > r.Transfer.RequiredBytes {
		return fmt.Errorf("migration: record %s reports %d bytes moved of %d required - progress cannot exceed the requirement",
			r.ID, r.Transfer.MovedBytes, r.Transfer.RequiredBytes)
	}
	if r.Transfer.FromToken > r.Transfer.ToToken {
		// Resume tokens only ever move forward. A record whose resume
		// point is behind the one it started from would resume from a
		// position that was already superseded, which is a silent
		// replay of data the target has already received.
		return fmt.Errorf("migration: record %s has resume token %d after %d - a resume point cannot move backwards",
			r.ID, r.Transfer.FromToken, r.Transfer.ToToken)
	}
	if r.Outcome.IsSuccess() && len(r.Evidence) == 0 {
		return fmt.Errorf("migration: record %s claims %q with no evidence - a confirmation must cite what confirmed it", r.ID, r.Outcome)
	}
	if r.Outcome.IsFailure() && len(r.Evidence) == 0 {
		return fmt.Errorf("migration: record %s claims %q with no evidence - a failure must cite what proved it", r.ID, r.Outcome)
	}
	if r.Outcome.IsTerminal() && r.Timestamps.SettledUnix == 0 {
		return fmt.Errorf("migration: record %s is terminal (%s) with no settle timestamp - when a migration stopped is not decoration", r.ID, r.Outcome)
	}
	if r.Outcome != OutcomeInProgress && r.Stall != nil {
		return fmt.Errorf("migration: record %s is %q but also stalled (%s) - a record cannot be both settled and waiting", r.ID, r.Outcome, r.Stall.Reason)
	}
	if r.Stall != nil && strings.TrimSpace(r.Stall.Reason) == "" {
		return fmt.Errorf("migration: record %s has a stall with no reason", r.ID)
	}
	if r.Outcome.IsSuccess() && !r.Verification.Matches() {
		return fmt.Errorf("migration: record %s claims %q without a matching target observation (observed=%v running=%v guid %q/%q ip %q/%q) - an unbacked success is not a record",
			r.ID, r.Outcome, r.Verification.Observed, r.Verification.GuestRunning,
			r.Verification.ObservedGUID, r.Verification.ExpectedGUID,
			r.Verification.ObservedIP, r.Verification.ExpectedIP)
	}
	return nil
}

// Render produces the one-line operator-facing form of the record.
//
// The one thing Render must never do is print a migration as finished
// when we did not observe it finish. That is why the success branch is
// gated on IsSuccess() rather than on the record simply being terminal:
// completion_unobserved and completion_unverified both fall through to
// the unknown branch and say so in those words.
func (r Record) Render() string {
	who := r.Guest.String() + " " + r.SourceID + " -> " + r.TargetID
	switch {
	case r.Outcome.IsSuccess():
		return fmt.Sprintf("%s: complete (phase %s, %d/%d bytes, frozen at %s)", who, r.Phase, r.Transfer.MovedBytes, r.Transfer.RequiredBytes, unixOrDash(r.Timestamps.FirstEnteredUnix[PhaseFreeze]))
	case r.Outcome.IsFailure():
		return fmt.Sprintf("%s: FAILED at %s - %s", who, r.Phase, r.Detail)
	case r.Outcome == OutcomeAborted:
		return fmt.Sprintf("%s: aborted at %s by operator - %s (guest returned to %s)", who, r.Phase, r.Detail, r.SourceID)
	case r.Outcome.IsCompletionNeverObserved():
		return fmt.Sprintf("%s: outcome UNKNOWN (%s) at %s - not a confirmed success and not a confirmed failure; the guest stays fenced: %s",
			who, r.Outcome, r.Phase, r.Detail)
	default:
		stall := ""
		if r.Stall != nil {
			stall = fmt.Sprintf(" [stalled: %s - %s]", r.Stall.Reason, r.Stall.Detail)
		}
		return fmt.Sprintf("%s: in flight at %s (attempt %d, token %d, %d/%d bytes)%s",
			who, r.Phase, r.Attempt, r.Token, r.Transfer.MovedBytes, r.Transfer.RequiredBytes, stall)
	}
}

func unixOrDash(unix int64) string {
	if unix == 0 {
		return "-"
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// PhaseProgress renders "3/6 bulk" for a frontend progress indicator
// driven by the phase sequence alone. It deliberately says nothing
// about the outcome: a migration at 6/6 is not thereby complete, and a
// UI that conflated the two is how an unobserved migration gets
// displayed as a success.
func (r Record) PhaseProgress() (current, total int) {
	i := r.Phase.Index()
	if i < 0 {
		return 0, len(PhaseOrder)
	}
	return i + 1, len(PhaseOrder)
}

// SortRecords orders a slice for deterministic listing: by creation
// time, then by id, so two records created in the same second never
// swap places between calls. Ordering that is not deterministic is a
// bug an operator sees as a list that reorders itself on refresh.
func SortRecords(recs []Record) {
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].Timestamps.CreatedUnix != recs[j].Timestamps.CreatedUnix {
			return recs[i].Timestamps.CreatedUnix < recs[j].Timestamps.CreatedUnix
		}
		return recs[i].ID < recs[j].ID
	})
}
