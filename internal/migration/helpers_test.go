package migration

import (
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/recovery"
)

// --- shared fixtures -----------------------------------------------------
//
// Every test builds its records through these helpers rather than by
// hand, so that a fixture change is visible everywhere at once and no
// test accidentally passes because of a hand-set field the real code
// path would never set.

const (
	testVM      = "vm-under-test"
	testSource  = "comb-a"
	testTarget  = "comb-b"
	testToken1  = uint64(1000)
	testToken2  = uint64(1001)
	testToken3  = uint64(1002)
	testToken4  = uint64(1003)
	testToken5  = uint64(1004)
	testToken6  = uint64(1005)
	testToken7  = uint64(1006)
	testToken8  = uint64(1007)
	baseUnix    = int64(1_780_000_000) // 2026-09-26-ish, fixed for determinism
	stepSeconds = int64(60)
)

func testGuest() Workload { return Workload{Kind: WorkloadKindVM, ID: testVM} }

func jailGuest() Workload { return Workload{Kind: WorkloadKindJail, ID: "jail-under-test"} }

// baseTime is a fixed instant so elapsed-time assertions are exact
// rather than dependent on how fast the test machine is.
var baseTime = time.Unix(baseUnix, 0).UTC()

// newRecordAt builds a valid, in-flight record at preflight - the state
// every real migration starts in.
func newRecordAt(id string, at int64) Record {
	r := newRecord(Command{
		Kind:     CommandStart,
		ID:       id,
		Guest:    testGuest(),
		SourceID: testSource,
		TargetID: testTarget,
		Token:    testToken1,
		AtUnix:   at,
	})
	return r
}

// advanceTo walks a record forward to want, minting a fresh token at
// each step exactly as the leader would. It fails the test if any step
// is illegal, so a test that reaches an impossible state says so here
// rather than somewhere confusing.
func advanceTo(t *testing.T, r Record, want Phase) Record {
	t.Helper()
	idx := r.Phase.Index()
	target := want.Index()
	if target < idx {
		t.Fatalf("advanceTo: cannot rewind from %s to %s", r.Phase, want)
	}
	tokens := []uint64{testToken2, testToken3, testToken4, testToken5, testToken6, testToken7, testToken8}
	step := baseUnix
	if first, ok := r.Timestamps.FirstEnteredUnix[PhasePreflight]; ok {
		step = first
	}
	for i := idx + 1; i <= target; i++ {
		step += stepSeconds
		res, err := Advance(r, Transition{From: r.Phase, To: PhaseOrder[i]}, tokens[i-1], step)
		if err != nil {
			t.Fatalf("advanceTo(%s): %v", want, err)
		}
		r = res.Record
	}
	return r
}

// matchingVerification is a target observation that genuinely matches -
// the only shape DeriveOutcome will turn into a success.
func matchingVerification(at int64) Verification {
	return Verification{
		Observed:     true,
		GuestRunning: true,
		ObservedGUID: "guid-abc",
		ExpectedGUID: "guid-abc",
		ObservedIP:   "10.0.0.5",
		ExpectedIP:   "10.0.0.5",
		ObservedAt:   at,
		ObservedOn:   testTarget,
	}
}

func observedEvidence(at int64) []Evidence {
	return []Evidence{{
		Rule:           "target-reported-running",
		Detail:         "target " + testTarget + " reported the guest running with the expected dataset guid and address",
		ObservedAtUnix: at,
	}}
}

func failureEvidence(detail string, at int64) []Evidence {
	return []Evidence{{Rule: "storage-or-target-failure", Detail: detail, ObservedAtUnix: at}}
}

// quorumSurvives is a valid recovery.QuorumFact where the confirmed
// reachable count alone already meets quorum.
func quorumSurvives(targetIsVoter bool) recovery.QuorumFact {
	return recovery.QuorumFact{
		TargetIsVoter:      targetIsVoter,
		TotalVoters:        3,
		RemainingVoters:    2,
		RemainingReachable: 2,
		RemainingUnknown:   0,
		QuorumSize:         2,
	}
}

// quorumMightClose is a valid fact where the reachable count alone
// falls short but the deficit could close: recovery's QuorumUnknown.
func quorumMightClose() recovery.QuorumFact {
	return recovery.QuorumFact{
		TargetIsVoter:      true,
		TotalVoters:        3,
		RemainingVoters:    2,
		RemainingReachable: 1,
		RemainingUnknown:   1,
		QuorumSize:         2,
	}
}

// quorumGone is a valid fact where even crediting every unknown voter,
// a majority cannot be reached: recovery's QuorumLost.
func quorumGone() recovery.QuorumFact {
	return recovery.QuorumFact{
		TargetIsVoter:      true,
		TotalVoters:        3,
		RemainingVoters:    2,
		RemainingReachable: 0,
		RemainingUnknown:   1,
		QuorumSize:         2,
	}
}

// quorumUnreadable is the concrete real trigger internal/recovery names
// for a silent upstream read failure: a zero-valued fact, which would
// otherwise classify as a genuine and completely fabricated QuorumLost.
func quorumUnreadable() recovery.QuorumFact { return recovery.QuorumFact{} }

// --- small assertion helpers ---------------------------------------------

func requireSentinel(t *testing.T, err error, want error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected %v, got nil error", what, want)
	}
	if !errorsIs(err, want) {
		t.Fatalf("%s: error = %v, want it to wrap %v", what, err, want)
	}
}

func errorsIs(err, target error) bool {
	type unwrapper interface{ Unwrap() error }
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func requireNoError(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", what, err)
	}
}
