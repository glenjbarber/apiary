package migration

import (
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/recovery"
)

// TestGateOnQuorumSurvivesProceeds: quorum held is the only case that
// lets a migration move, and it cites the arithmetic it relied on.
func TestGateOnQuorumSurvivesProceeds(t *testing.T) {
	d := GateOnQuorum(QuorumFact{
		IsLeader:   false,
		Fact:       quorumSurvives(true),
		ReadOK:     true,
		ObservedAt: baseUnix,
	})
	if !d.Proceed {
		t.Fatalf("quorum survived but the gate blocked: %s", d.Detail)
	}
	if d.Verdict != recovery.QuorumSurvives {
		t.Errorf("verdict = %s, want %s", d.Verdict, recovery.QuorumSurvives)
	}
	if d.Outcome != OutcomeInProgress {
		t.Errorf("outcome on an allowing gate = %s, want it left in flight", d.Outcome)
	}
	if len(d.Evidence) == 0 {
		t.Fatal("an allowing gate cited no evidence")
	}
	if d.Evidence[0].ObservedAtUnix != baseUnix {
		t.Errorf("evidence observed at %d, want %d", d.Evidence[0].ObservedAtUnix, baseUnix)
	}
	if !strings.Contains(d.Detail, "2/2") {
		t.Errorf("allowing detail does not show the arithmetic: %q", d.Detail)
	}
}

// TestGateOnQuorumUnknownIsNotFailureAndNotSuccess: the requirement
// stated directly for the middle state. QuorumUnknown must halt, must
// be reported as unknown, and must never be reported as a failure.
func TestGateOnQuorumUnknownIsNotFailureAndNotSuccess(t *testing.T) {
	d := GateOnQuorum(QuorumFact{
		Fact:       quorumMightClose(),
		ReadOK:     true,
		ObservedAt: baseUnix,
	})
	if d.Proceed {
		t.Fatal("an unknown quorum allowed the migration to proceed")
	}
	if d.Verdict != recovery.QuorumUnknown {
		t.Errorf("verdict = %s, want %s", d.Verdict, recovery.QuorumUnknown)
	}
	if d.Outcome.IsFailure() {
		t.Errorf("an unknown quorum was recorded as a failure (%s)", d.Outcome)
	}
	if d.Outcome.IsSuccess() {
		t.Errorf("an unknown quorum was recorded as a success (%s)", d.Outcome)
	}
	if d.Outcome != OutcomeUnverified {
		t.Errorf("outcome = %s, want %s", d.Outcome, OutcomeUnverified)
	}
	if !strings.Contains(d.Detail, "could close") {
		t.Errorf("unknown-quorum detail does not explain the deficit: %q", d.Detail)
	}
	if !strings.Contains(d.Detail, "never as a failure") {
		t.Errorf("unknown-quorum detail does not say it is not a failure: %q", d.Detail)
	}
}

// TestGateOnQuorumLostIsUnknownNotFailure: quorum genuinely lost halts
// and is likewise recorded as unknown. Lost is a much more severe
// finding than unknown, so the two must be distinguishable to the
// operator - but neither of them is a statement about the migration.
func TestGateOnQuorumLostIsUnknownNotFailure(t *testing.T) {
	d := GateOnQuorum(QuorumFact{
		Fact:       quorumGone(),
		ReadOK:     true,
		ObservedAt: baseUnix,
	})
	if d.Proceed {
		t.Fatal("a lost quorum allowed the migration to proceed")
	}
	if d.Verdict != recovery.QuorumLost {
		t.Errorf("verdict = %s, want %s", d.Verdict, recovery.QuorumLost)
	}
	if d.Outcome != OutcomeUnverified {
		t.Errorf("outcome = %s, want %s", d.Outcome, OutcomeUnverified)
	}
	if d.Outcome.IsFailure() || d.Outcome.IsSuccess() {
		t.Errorf("a lost quorum produced a positive outcome: %s", d.Outcome)
	}
	if !strings.Contains(d.Detail, "majority cannot be reached") {
		t.Errorf("lost-quorum detail does not say why: %q", d.Detail)
	}
}

// TestGateOnUnreadableQuorumFailsClosed: no read is not a pass, and it
// is not a fabricated QuorumLost either.
func TestGateOnUnreadableQuorumFailsClosed(t *testing.T) {
	d := GateOnQuorum(QuorumFact{
		ReadOK:     false,
		ReadError:  "context deadline exceeded",
		ObservedAt: baseUnix,
		Error:      "raft status unavailable",
	})
	if d.Proceed {
		t.Fatal("an unreadable quorum fact allowed the migration to proceed")
	}
	if d.Verdict != recovery.QuorumUnknown {
		t.Errorf("verdict = %s, want %s", d.Verdict, recovery.QuorumUnknown)
	}
	if d.Outcome != OutcomeUnverified {
		t.Errorf("outcome = %s, want %s", d.Outcome, OutcomeUnverified)
	}
	if !strings.Contains(d.Detail, "could not check") {
		t.Errorf("unreadable detail does not distinguish 'could not check' from 'checked': %q", d.Detail)
	}
	if !strings.Contains(d.Detail, "context deadline exceeded") {
		t.Errorf("unreadable detail does not carry the read error: %q", d.Detail)
	}

	// No error string at all still produces a usable, non-empty reason
	// rather than an empty one.
	bare := GateOnQuorum(QuorumFact{ReadOK: false})
	if strings.TrimSpace(bare.Detail) == "" {
		t.Error("an unreadable quorum with no error string produced an empty detail")
	}
	if bare.Proceed {
		t.Error("an unreadable quorum with no error string allowed the migration to proceed")
	}
}

// TestGateRejectsAnInconsistentQuorumFact: recovery.ValidQuorumFact
// exists because a silently-failed upstream read produces a
// zero-valued fact that would otherwise classify as a genuine
// QuorumLost. Reporting a fabricated outage is as bad as missing a real
// one, so the gate refuses the malformed fact as unknown.
func TestGateRejectsAnInconsistentQuorumFact(t *testing.T) {
	for name, fact := range map[string]recovery.QuorumFact{
		"zero value":         quorumUnreadable(),
		"quorum size wrong":  {TargetIsVoter: true, TotalVoters: 3, RemainingVoters: 2, RemainingReachable: 2, QuorumSize: 1},
		"reachable too big":  {TotalVoters: 3, RemainingVoters: 2, RemainingReachable: 3, QuorumSize: 2},
		"remaining too big":  {TotalVoters: 3, RemainingVoters: 5, RemainingReachable: 2, QuorumSize: 2},
		"two voters removed": {TotalVoters: 5, RemainingVoters: 3, RemainingReachable: 3, QuorumSize: 3},
	} {
		if recovery.ValidQuorumFact(fact) {
			t.Fatalf("fixture %q is actually valid; the test would prove nothing", name)
		}
		d := GateOnQuorum(QuorumFact{Fact: fact, ReadOK: true, ObservedAt: baseUnix})
		if d.Proceed {
			t.Errorf("inconsistent fact %q allowed the migration to proceed", name)
		}
		if d.Verdict == recovery.QuorumLost {
			t.Errorf("inconsistent fact %q was classified as a genuine QuorumLost - that is a fabricated outage", name)
		}
		if d.Outcome != OutcomeUnverified {
			t.Errorf("inconsistent fact %q produced outcome %s, want %s", name, d.Outcome, OutcomeUnverified)
		}
	}
}

// TestGateDelegatesToRecoveryClassifyQuorum, over a sweep of reachable
// counts, so that a future change to this gate cannot quietly diverge
// from internal/recovery's single implementation of the arithmetic.
func TestGateDelegatesToRecoveryClassifyQuorum(t *testing.T) {
	for reachable := uint32(0); reachable <= 3; reachable++ {
		for unknown := uint32(0); unknown+reachable <= 2; unknown++ {
			fact := recovery.QuorumFact{
				TargetIsVoter:      true,
				TotalVoters:        3,
				RemainingVoters:    2,
				RemainingReachable: reachable,
				RemainingUnknown:   unknown,
				QuorumSize:         2,
			}
			for _, isLeader := range []bool{false, true} {
				d := GateOnQuorum(QuorumFact{Fact: fact, ReadOK: true, IsLeader: isLeader, ObservedAt: baseUnix})
				want := recovery.ClassifyQuorum(fact, isLeader)
				if d.Verdict != want {
					t.Errorf("reachable=%d unknown=%d leader=%v: gate verdict = %s, recovery says %s",
						reachable, unknown, isLeader, d.Verdict, want)
				}
				if d.Proceed != (want == recovery.QuorumSurvives) {
					t.Errorf("reachable=%d unknown=%d leader=%v: Proceed = %v for verdict %s",
						reachable, unknown, isLeader, d.Proceed, want)
				}
			}
		}
	}
}

// TestGateLeaderDowngradeIsCarriedThrough: a migration driven from the
// leader cannot be told it has quorum on reachability data gathered
// through the leader, because that data proves nothing about whether
// the remaining voters can reach each other.
func TestGateLeaderDowngradeIsCarriedThrough(t *testing.T) {
	fact := quorumSurvives(true)
	follower := GateOnQuorum(QuorumFact{Fact: fact, ReadOK: true, IsLeader: false, ObservedAt: baseUnix})
	leader := GateOnQuorum(QuorumFact{Fact: fact, ReadOK: true, IsLeader: true, ObservedAt: baseUnix})

	if !follower.Proceed {
		t.Error("a follower with quorum held was blocked")
	}
	if leader.Proceed {
		t.Error("a leader with quorum held was allowed through; the leader-loss downgrade was dropped")
	}
	if !strings.Contains(leader.Detail, "proves nothing about whether") {
		t.Errorf("leader-downgrade detail does not explain itself: %q", leader.Detail)
	}
}

// TestStallFromDecisionNamesTheReason: the record must be able to say
// WHY it stopped, and the three blocking cases must be distinguishable
// from each other in the record.
func TestStallFromDecisionNamesTheReason(t *testing.T) {
	cases := []struct {
		name   string
		fact   QuorumFact
		reason string
	}{
		{"lost", QuorumFact{Fact: quorumGone(), ReadOK: true, ObservedAt: baseUnix}, "quorum-lost"},
		{"unknown", QuorumFact{Fact: quorumMightClose(), ReadOK: true, ObservedAt: baseUnix}, "quorum-unknown"},
		{"unreadable", QuorumFact{ReadOK: false, ReadError: "raft status unavailable", ObservedAt: baseUnix}, "quorum-unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := advanceTo(t, newRecordAt("mig-quorum-"+tc.name, baseUnix), PhaseBulk)
			d := GateOnQuorum(tc.fact)
			if d.Proceed {
				t.Fatalf("%s: gate allowed the migration; nothing to stall", tc.name)
			}
			stalled, err := StallFromDecision(r, d, baseUnix+100)
			requireNoError(t, err, "stall from decision")
			if stalled.Stall == nil {
				t.Fatal("no stall was recorded")
			}
			if stalled.Stall.Reason != tc.reason {
				t.Errorf("stall reason = %q, want %q", stalled.Stall.Reason, tc.reason)
			}
			if stalled.Stall.Verdict != string(d.Verdict) {
				t.Errorf("stall verdict = %q, want %q", stalled.Stall.Verdict, d.Verdict)
			}
			if stalled.Stall.SinceUnix != baseUnix+100 {
				t.Errorf("stall since = %d, want %d", stalled.Stall.SinceUnix, baseUnix+100)
			}
			// Still in flight, still fenced: the ADR's rule is "halt; do
			// not cut over", not "fail".
			if !stalled.InFlight() {
				t.Errorf("a quorum-stalled record is %s, want it still in flight", stalled.Outcome)
			}
			if !stalled.Fenced() {
				t.Error("a quorum-stalled record is not fenced; the guest would be resumable on a guess")
			}
			requireNoError(t, stalled.Validate(), "stalled record")
		})
	}
}

// TestStallFromAnAllowingDecisionIsANoOp: a gate that passed is not a
// stall, and writing one would leave a record claiming to be waiting
// while it is in fact progressing.
func TestStallFromAnAllowingDecisionIsANoOp(t *testing.T) {
	r := advanceTo(t, newRecordAt("mig-quorum-ok", baseUnix), PhaseBulk)
	d := GateOnQuorum(QuorumFact{Fact: quorumSurvives(true), ReadOK: true, ObservedAt: baseUnix})
	if !d.Proceed {
		t.Fatal("fixture did not allow the migration")
	}
	same, err := StallFromDecision(r, d, baseUnix+100)
	requireNoError(t, err, "stall from an allowing decision")
	if same.Stall != nil {
		t.Errorf("an allowing gate wrote a stall: %+v", same.Stall)
	}
}
