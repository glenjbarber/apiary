package raft

// The colony-wide controlled-update single-flight (ADR-0145) and the
// durable update-operation state that travels with it.
//
// WHY THE LOCK IS HERE, AND WHY IT IS NOT A MUTEX
//
// ADR-0145's first open question asks whether the per-step update state
// lives in managerd memory or somewhere durable, and the answer recorded
// there is that managerd memory fails for the exact case the feature is
// for: the Comb most likely to be updated is the one running managerd,
// and a coordinator's own progress dies with it. That is already enough
// to rule out process memory. It is not, on its own, enough to choose
// between this file and a file on disk, and the honest argument for
// raft-replication rather than a file is:
//
//   - The property being bought is a COLONY-wide invariant: at most one
//     controlled update may exist in the Colony at a time, regardless of
//     how many operators, clients or coordinators are involved. A file
//     cannot enforce that. Two coordinators on two Combs each holding a
//     valid-looking local file would both proceed, and ADR-0103's own
//     record of the 2026-09-11 incident is that a purely local check can
//     never provide atomicity across nodes. Raft's own serialized
//     log-apply order is what does, and that is the same argument
//     ADR-0103 already accepted for AcquireRestartLease.
//   - A file and a lock in different places can disagree, and a
//     disagreement here is not a degraded view, it is two updates. The
//     record below IS the lock, so there is nothing to disagree.
//   - The lease must be visible to a replacement process, and - across
//     coordinator failover - to a replacement process on a DIFFERENT
//     Comb. Only replicated state is.
//
// WHY THE FENCE IS FOUR FIELDS AND NOT ONE
//
// An operation id alone cannot fence, because the holder must also be
// pinned. A monotonically increasing fence token (the raft log index of
// the granting command, exactly as ADR-0103's RestartLease.lease_id is
// "unique and monotonic for free") is what makes takeover safe: a
// takeover is a NEW grant at a strictly higher index, so every token
// issued before it is stale the instant the takeover commits, on every
// replica, with no clock and no timeout anywhere.
//
// WHY THERE IS NO TTL
//
// ADR-0103 is deliberately no-TTL and ADR-0145 carries that constraint
// forward. The reason is not squeamishness about expiry: a time-based
// auto-clear can lapse a record while the operation it describes is still
// genuinely mid-flight, which is exactly the window this exists to close.
// Recovery is an EXPLICIT takeover, which fails in the safe direction - a
// takeover that should not have happened costs one abandoned update, and
// can never produce two concurrent ones, because the displaced holder is
// fenced out of the restart-lease path (see applyAcquireRestartLease).

import (
	"fmt"
	"sort"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// maxSettledColonyUpdates bounds the retained history of settled
// operations.
//
// The ACTIVE record is never bounded and never expires - that is the
// single-flight. This cap applies only to settled entries, which are
// history rather than a safety record, and it exists because this map
// lives in raft state: it is replicated to every voter, written into
// every snapshot, and read back on every restore. An unbounded
// append-only map in that position is a slow-motion problem that only
// shows up on a cluster nobody is watching.
//
// The eviction rule is by lowest fence_token - which is the log index at
// which the operation was granted - so every replica computes the same
// survivors from the same state with no clock and no coordination. That
// determinism is the same requirement stateddigest.go's own canonical
// encoding exists to satisfy, and getting it wrong here would show up as
// a digest mismatch (ADR-0143) with no other symptom.
const maxSettledColonyUpdates = 32

// restartplan outcome constants, restated here rather than imported.
//
// internal/restartplan does not import internal/raft and importing it
// would invert the dependency the ADR-0125 note describes (restartplan
// is deliberately importable by internal/manager, which also imports
// internal/raft, so raft -> restartplan is not a cycle today but couples
// the consensus layer to a package whose whole point is being
// raft-free). These strings ARE the vocabulary, and
// TestColonyUpdateOutcomeVocabularyMatchesRestartplan asserts them
// against the real constants so a future rename cannot drift silently.
const (
	colonyOutcomeConfirmed  = "confirmed"
	colonyOutcomeFailed     = "failed"
	colonyOutcomeBlocked    = "blocked"
	colonyOutcomeUnobserved = "unobserved"
	colonyOutcomeUnverified = "unverified"
)

// validColonyUpdateOutcome reports whether o is one of the recognised
// outcomes. An empty outcome is NOT valid: "no result yet" is the
// absence of a record, not a record saying so, and admitting it would
// make "the update finished" and "we did not finish finding out"
// indistinguishable to every later reader.
func validColonyUpdateOutcome(o string) bool {
	switch o {
	case colonyOutcomeConfirmed, colonyOutcomeFailed, colonyOutcomeBlocked,
		colonyOutcomeUnobserved, colonyOutcomeUnverified:
		return true
	default:
		return false
	}
}

// outcomeRequiresEvidence reports whether o is a positive claim that must
// cite what proves it.
//
// This is internal/restartplan's Result.Validate rule, applied to a
// different medium and for the same reason: a durable record that claims
// "confirmed" with no evidence is a lie that outlives the process that
// wrote it, and the cheapest place to make it unrepresentable is the
// write. "blocked" is exempt because the absence of an attempt IS the
// fact, and the two unknown outcomes are exempt because they explicitly
// claim no observation was established.
func outcomeRequiresEvidence(o string) bool {
	return o == colonyOutcomeConfirmed || o == colonyOutcomeFailed
}

// validateColonyUpdateStep enforces the same honesty invariants on one
// step record that restartplan.Result.Validate enforces on a whole
// result: a named step, a recognised outcome, a stated reason, and real
// evidence behind either positive claim.
//
// The index is checked by the caller, not here - it is an ordering
// question about the operation's own history rather than a property of
// the record in isolation.
func validateColonyUpdateStep(s *internalpb.ColonyUpdateStepRecord) error {
	if s == nil {
		return nil
	}
	if s.GetStep() == "" {
		return fmt.Errorf("step record names no step, so its outcome cannot be attributed to anything")
	}
	if !validColonyUpdateOutcome(s.GetOutcome()) {
		return fmt.Errorf("step record for %q carries outcome %q, which is not one of the recognised outcomes (%q, %q, %q, %q, %q)",
			s.GetStep(), s.GetOutcome(),
			colonyOutcomeConfirmed, colonyOutcomeFailed, colonyOutcomeBlocked,
			colonyOutcomeUnobserved, colonyOutcomeUnverified)
	}
	if s.GetDetail() == "" {
		return fmt.Errorf("step record for %q carries outcome %q but no detail - an unbacked verdict is not a record", s.GetStep(), s.GetOutcome())
	}
	if outcomeRequiresEvidence(s.GetOutcome()) && len(s.GetEvidence()) == 0 {
		verb := "confirms"
		if s.GetOutcome() == colonyOutcomeFailed {
			verb = "proves"
		}
		return fmt.Errorf("step record for %q claims %q with no evidence - a %q must cite what %s it",
			s.GetStep(), s.GetOutcome(), s.GetOutcome(), verb)
	}
	return nil
}

// fenceMatchesActive reports whether fence is a complete, exact
// description of the currently active operation.
//
// All four halves matter and none is a convenience:
//
//   - a zero fence_token can never match a granted one, because a grant
//     always records its own log index and index 0 is the bootstrap
//     entry, which is never a grant;
//   - holder_incarnation is what separates a replacement managerd on a
//     Comb from the predecessor it replaced on that same Comb, which no
//     other field can express;
//   - operation_id is what stops a coordinator that is a fence-token
//     collision away from acting on somebody else's operation.
//
// A nil or empty fence is never a match. That is the fail-closed default
// and it is why every caller in this package tests it rather than
// treating "no fence" as "no constraint".
func fenceMatchesActive(fence *internalpb.ColonyUpdateFence, rec *internalpb.ColonyUpdate) bool {
	if fence == nil || rec == nil {
		return false
	}
	if fence.GetOperationId() == "" || fence.GetHolderNodeId() == "" || fence.GetHolderIncarnation() == "" {
		return false
	}
	if fence.GetFenceToken() == 0 {
		return false
	}
	return fence.GetOperationId() == rec.GetOperationId() &&
		fence.GetHolderNodeId() == rec.GetHolderNodeId() &&
		fence.GetHolderIncarnation() == rec.GetHolderIncarnation() &&
		fence.GetFenceToken() == rec.GetFenceToken()
}

// fenceRefusal is the refusal every fenced operation gets when the
// caller's token does not describe the active record. It names what IS
// held, because a refusal that does not say who holds it forces the
// reader to go and look - and in the middle of a failover that is exactly
// the wrong thing to ask of a safety refusal.
func fenceRefusal(verb string, fence *internalpb.ColonyUpdateFence, active *internalpb.ColonyUpdate) string {
	if active == nil {
		return fmt.Sprintf("refusing to %s: no controlled update is currently in progress, so the fence presented (operation %q, holder %q, incarnation %q, token %d) names nothing that exists",
			verb, fence.GetOperationId(), fence.GetHolderNodeId(), fence.GetHolderIncarnation(), fence.GetFenceToken())
	}
	where := "nothing yet"
	if active.GetTargetNodeId() != "" {
		where = "on " + active.GetTargetNodeId()
	}
	step := "no step recorded"
	if active.GetStep() != "" {
		step = "at step " + active.GetStep()
	}
	return fmt.Sprintf("refusing to %s: the colony is running controlled update %q, held by %q (incarnation %q) at fence token %d, %s, %s - the fence presented (operation %q, holder %q, incarnation %q, token %d) does not match it",
		verb, active.GetOperationId(), active.GetHolderNodeId(), active.GetHolderIncarnation(),
		active.GetFenceToken(), where, step,
		fence.GetOperationId(), fence.GetHolderNodeId(), fence.GetHolderIncarnation(), fence.GetFenceToken())
}

// validateColonyUpdateHandover enforces the self-contained parts of a
// handover: that it names a coordinator to hand to, and says why.
//
// The membership of to_node_id is deliberately NOT checked here. The FSM
// has no authoritative view of Colony membership - applyAcquireRestartLease
// takes its voter list from the calling command for the same reason - so a
// check written in this file could only be a guess. Deciding whether the
// named Comb is a real voter is the resolving managerd's job, on facts it
// actually has, and a half-check in the consensus layer would be worse
// than none: it would be a check that looks load-bearing and can be wrong.
func validateColonyUpdateHandover(req *internalpb.HandoverColonyUpdate) error {
	if req.GetToNodeId() == "" {
		return fmt.Errorf("no receiving node was named, so there is no coordinator to hand the operation to")
	}
	if req.GetToHolderIncarnation() == "" {
		// Same reason acquire demands one: without it the incoming
		// coordinator is not distinguishable from the outgoing one, and
		// the fence it receives could not fence it.
		return fmt.Errorf("no receiving incarnation was named, so the incoming coordinator could not be told apart from the process it replaces")
	}
	if req.GetReason() == "" {
		return fmt.Errorf("no reason was given - \"who gave this away, and why\" is the first question an operator asks when a sweep stops near its end, and the record has to answer it")
	}
	return nil
}

// activeColonyUpdate returns the one active record, or nil.
//
// The map is keyed by operation_id and at most one entry is active, so
// this is a scan. It is a scan over a map that is bounded by
// maxSettledColonyUpdates plus one, on a path that runs once per
// controlled-update command, and it exists so that "exactly one active"
// is CHECKED rather than assumed - an invariant that holds only because
// every writer respects it is an invariant waiting for the one writer
// that does not.
func activeColonyUpdate(updates map[string]*internalpb.ColonyUpdate) *internalpb.ColonyUpdate {
	for _, rec := range updates {
		if rec.GetActive() {
			return rec
		}
	}
	return nil
}

// settledColonyUpdates returns the settled records newest first, by fence
// token. Newest-first is what a reader actually wants: the most recent
// update is the one they are asking about, and the oldest is the one most
// likely already to have been dropped by the cap.
func settledColonyUpdates(updates map[string]*internalpb.ColonyUpdate) []*internalpb.ColonyUpdate {
	var out []*internalpb.ColonyUpdate
	for _, rec := range updates {
		if !rec.GetActive() {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetFenceToken() > out[j].GetFenceToken() })
	return out
}

// evictSettledColonyUpdates drops the lowest-fence-token settled records
// until at most limit of them remain.
//
// Called only from applyAcquireColonyUpdate, and deliberately NOT called
// from a background sweeper: this codebase's one existing precedent for
// expiry (the Assumption Register's ExpiresAt) is lazy, checked at
// read/apply time, and nothing anywhere in it runs a purge goroutine.
// Eviction is instead deterministic housekeeping performed by the one
// operation that legitimately creates history, which means every replica
// performs it at the same log index with the same inputs.
func evictSettledColonyUpdates(updates map[string]*internalpb.ColonyUpdate, limit int) {
	if limit < 0 {
		limit = 0
	}
	settled := settledColonyUpdates(updates)
	if len(settled) <= limit {
		return
	}
	for _, rec := range settled[limit:] {
		delete(updates, rec.GetOperationId())
	}
}
