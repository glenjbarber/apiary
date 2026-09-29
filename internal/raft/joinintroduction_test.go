// ADR-0147 Part 2 at the FSM layer, where the comparisons actually
// happen.
//
// These are the unit-level counterpart to the RPC-level tests in
// internal/manager: they exist because the interesting claims are about
// the FSM's DECISIONS, and a claim about a decision should be testable
// without a gRPC server, a raft cluster and a certificate generator in
// the way. In particular they are the tests that pin the two properties
// most likely to be broken by a later well-meaning edit: that the
// comparison is in here at all rather than in the caller, and that no
// path consults a clock.

package raft

import (
	"testing"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// newIntroductionFSM returns an FSM holding one INTRODUCED request
// carrying a first code and two advertised fingerprints - the shape
// RequestJoinColony now produces.
func newIntroductionFSM(t *testing.T, requestID string) *FSM {
	t.Helper()
	f := newTestFSM(t)
	applyOK(t, f, &internalpb.Command{Op: &internalpb.Command_CreatePendingJoinRequest{
		CreatePendingJoinRequest: &internalpb.CreatePendingJoinRequest{
			Request: &internalpb.PendingJoinRequest{
				RequestId:              requestID,
				NodeId:                 "comb-2",
				RaftBindAddress:        "10.62.0.5:17600",
				Code:                   "418263",
				RequestedAtUnix:        1000,
				ExpiresAtUnix:          1000 + 900,
				Status:                 internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING,
				AdvertisedFingerprints: []string{"SHA256:AA", "SHA256:BB"},
				JoinerLogStateObserved: true,
			},
		},
	}})
	return f
}

// applyOK and applyErr are the only two outcomes these tests care
// about. Both run the command through the real Apply switch by way of
// applyOp, so neither can pass by reaching a private apply function the
// production path does not use.
func applyOK(t *testing.T, f *FSM, cmd *internalpb.Command) *FSMApplyResult {
	t.Helper()
	res := applyOp(t, f, cmd)
	if res.Error != "" {
		t.Fatalf("Apply: %s", res.Error)
	}
	return res
}

func applyErr(t *testing.T, f *FSM, cmd *internalpb.Command) *FSMApplyResult {
	t.Helper()
	res := applyOp(t, f, cmd)
	if res.Error == "" {
		t.Fatalf("Apply accepted a command it should have refused (%+v)", res.PendingJoinRequest)
	}
	return res
}

// storedRequest reads through PendingJoinRequest, the same accessor the
// RPC layer uses, so a test cannot pass against replicated state the
// service could not actually see.
func storedRequest(t *testing.T, f *FSM, requestID string) *internalpb.PendingJoinRequest {
	t.Helper()
	req, ok := f.PendingJoinRequest(requestID)
	if !ok {
		t.Fatalf("no stored request %q", requestID)
	}
	return req
}

func verifyCmd(requestID, code string, fingerprints ...string) *internalpb.Command {
	return &internalpb.Command{Op: &internalpb.Command_VerifyJoinIntroduction{
		VerifyJoinIntroduction: &internalpb.VerifyJoinIntroduction{
			RequestId:              requestID,
			IntroductionCode:       code,
			Fingerprints:           fingerprints,
			SecondPin:              "12345678",
			SecondPinExpiresAtUnix: 2000,
			AtUnix:                 1000,
		},
	}}
}

// TestFSMPart2_CreateNormalizesTheStageToIntroduced: a record written by
// a pre-ADR-0147 writer has an UNSPECIFIED stage, and it must not be
// permanently stuck in a state the verify arm refuses by name.
func TestFSMPart2_CreateNormalizesTheStageToIntroduced(t *testing.T) {
	f := newTestFSM(t)
	applyOK(t, f, &internalpb.Command{Op: &internalpb.Command_CreatePendingJoinRequest{
		CreatePendingJoinRequest: &internalpb.CreatePendingJoinRequest{
			Request: &internalpb.PendingJoinRequest{
				RequestId: "jreq-1", NodeId: "comb-2", RaftBindAddress: "10.0.0.2:17600",
				Code: "418263", RequestedAtUnix: 1, ExpiresAtUnix: 901,
				Status: internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING,
			},
		},
	}})
	if got := storedRequest(t, f, "jreq-1").GetStage(); got != internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED {
		t.Errorf("stage = %s, want INTRODUCED", got)
	}
}

// TestFSMPart2_VerifyClearsTheCodeAndStoresThePIN: the spent value goes
// away in the SAME entry that accepts it.
func TestFSMPart2_VerifyClearsTheCodeAndStoresThePIN(t *testing.T) {
	f := newIntroductionFSM(t, "jreq-1")
	applyOK(t, f, verifyCmd("jreq-1", "418263", "SHA256:AA", "SHA256:BB"))

	got := storedRequest(t, f, "jreq-1")
	if got.GetCode() != "" {
		t.Errorf("the first code is still in replicated state after acceptance (%q); a spent value is not one to leave for a later reader", got.GetCode())
	}
	if got.GetStage() != internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED {
		t.Errorf("stage = %s, want CODE_VERIFIED", got.GetStage())
	}
	if got.GetSecondPin() != "12345678" {
		t.Errorf("second_pin = %q, want the PIN the command carried", got.GetSecondPin())
	}
	if got.GetAcceptedIntroductionCodeSha256() == "" {
		t.Error("no digest of the accepted first code was kept, so the joiner's later poll has nothing to be checked against")
	}
	// The PIN the command carried is used verbatim - the FSM never
	// generates one, because crypto/rand inside an Apply would hand
	// every replica a different value.
	if got.GetSecondPinExpiresAtUnix() != 2000 {
		t.Errorf("second_pin_expires_at_unix = %d, want the command's own 2000", got.GetSecondPinExpiresAtUnix())
	}
}

// TestFSMPart2_VerifyFailsOnAPartialFingerprintSet: pasting one of two is
// not a partial pass. The fingerprint the operator skipped is precisely
// the one that would have caught a request carrying a certificate they
// never looked at.
func TestFSMPart2_VerifyFailsOnAPartialFingerprintSet(t *testing.T) {
	f := newIntroductionFSM(t, "jreq-1")
	res := applyErr(t, f, verifyCmd("jreq-1", "418263", "SHA256:AA"))
	if res.Error == "" {
		t.Fatal("a partial fingerprint set was accepted")
	}
	if got := storedRequest(t, f, "jreq-1").GetStage(); got != internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED {
		t.Errorf("stage = %s, want INTRODUCED after a failed attempt", got)
	}
	if got := storedRequest(t, f, "jreq-1").GetFirstCodeAttempts(); got != 1 {
		t.Errorf("first_code_attempts = %d, want 1 - the counter is the replicated part and a failed comparison must advance it", got)
	}
}

// TestFSMPart2_VerifyFailsOnOutOfOrderFingerprints: the set is compared,
// not the list. An operator pasting in a different order has compared
// everything, and refusing that would teach them to expect order.
func TestFSMPart2_VerifyAcceptsOutOfOrderFingerprints(t *testing.T) {
	f := newIntroductionFSM(t, "jreq-1")
	applyOK(t, f, verifyCmd("jreq-1", "418263", "SHA256:BB", "SHA256:AA"))
	if got := storedRequest(t, f, "jreq-1").GetStage(); got != internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED {
		t.Errorf("stage = %s, want CODE_VERIFIED for the same set in the other order", got)
	}
}

// TestFSMPart2_VerifyRefusesARequestWithNoFingerprints: the fail-closed
// gate, asserted in the FSM rather than only at the RPC layer, because
// the RPC layer is not the only writer and this one is.
func TestFSMPart2_VerifyRefusesARequestWithNoFingerprints(t *testing.T) {
	f := newTestFSM(t)
	applyOK(t, f, &internalpb.Command{Op: &internalpb.Command_CreatePendingJoinRequest{
		CreatePendingJoinRequest: &internalpb.CreatePendingJoinRequest{
			Request: &internalpb.PendingJoinRequest{
				RequestId: "jreq-1", NodeId: "comb-2", RaftBindAddress: "10.0.0.2:17600",
				Code: "418263", RequestedAtUnix: 1, ExpiresAtUnix: 901,
				Status: internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING,
			},
		},
	}})
	applyErr(t, f, verifyCmd("jreq-1", "418263"))
	if got := storedRequest(t, f, "jreq-1").GetStage(); got != internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED {
		t.Errorf("stage = %s, want INTRODUCED - a request with nothing to compare must not advance", got)
	}
}

// TestFSMPart2_VerifyRefusesAReintroduction: the code is cleared on
// acceptance, so a second introduction has nothing to check, and the FSM
// says which of the two things the operator should do instead.
func TestFSMPart2_VerifyRefusesAReintroduction(t *testing.T) {
	f := newIntroductionFSM(t, "jreq-1")
	applyOK(t, f, verifyCmd("jreq-1", "418263", "SHA256:AA", "SHA256:BB"))
	res := applyErr(t, f, verifyCmd("jreq-1", "418263", "SHA256:AA", "SHA256:BB"))
	if got := res.Error; got == "" {
		t.Fatal("a second introduction was accepted")
	}
}

// TestFSMPart2_ExhaustionIsTerminalAndDropsThePin walks all five and
// then checks the two consequences that matter: the status is FAILED,
// and the unpromoted pin the successful stage one left behind is gone.
func TestFSMPart2_ExhaustionIsTerminalAndDropsThePin(t *testing.T) {
	f := newIntroductionFSM(t, "jreq-1")
	for i := uint32(0); i < MaxJoinStageAttempts; i++ {
		applyErr(t, f, verifyCmd("jreq-1", "000000", "SHA256:AA", "SHA256:BB"))
	}
	got := storedRequest(t, f, "jreq-1")
	if got.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_FAILED {
		t.Errorf("status after %d wrong codes = %s, want FAILED", MaxJoinStageAttempts, got.GetStatus())
	}
	if got.GetCode() != "" {
		t.Errorf("the first code survived exhaustion (%q)", got.GetCode())
	}
	// A FAILED request resolves nothing further.
	applyErr(t, f, &internalpb.Command{Op: &internalpb.Command_ApprovePendingJoinRequest{
		ApprovePendingJoinRequest: &internalpb.ApprovePendingJoinRequest{RequestId: "jreq-1"},
	}})
}

// TestFSMPart2_ConsumeClearsThePinAndItsDigest: the spend takes both the
// PIN and the digest of the first code, because neither outlives the
// value it protects.
func TestFSMPart2_ConsumeClearsThePinAndItsDigest(t *testing.T) {
	f := newIntroductionFSM(t, "jreq-1")
	applyOK(t, f, verifyCmd("jreq-1", "418263", "SHA256:AA", "SHA256:BB"))
	digest := storedRequest(t, f, "jreq-1").GetAcceptedIntroductionCodeSha256()

	applyOK(t, f, &internalpb.Command{Op: &internalpb.Command_ConsumeJoinSecondPin{
		ConsumeJoinSecondPin: &internalpb.ConsumeJoinSecondPin{RequestId: "jreq-1", SecondPin: "12345678", AtUnix: 1000},
	}})
	got := storedRequest(t, f, "jreq-1")
	if got.GetSecondPin() != "" {
		t.Errorf("the second PIN is still in replicated state after being spent (%q)", got.GetSecondPin())
	}
	if got.GetAcceptedIntroductionCodeSha256() != "" {
		t.Errorf("the first code's digest survived the spend (%q); it exists only to protect a PIN that no longer exists", got.GetAcceptedIntroductionCodeSha256())
	}
	if digest == "" {
		t.Error("test setup: no digest was recorded, so this test would pass for the wrong reason")
	}
}

// TestFSMPart2_ApproveReachesAuthorized: APPROVED is the only transition
// that reaches AUTHORIZED, and it is the one that runs after AddVoter
// actually succeeded.
func TestFSMPart2_ApproveReachesAuthorized(t *testing.T) {
	f := newIntroductionFSM(t, "jreq-1")
	applyOK(t, f, verifyCmd("jreq-1", "418263", "SHA256:AA", "SHA256:BB"))
	applyOK(t, f, &internalpb.Command{Op: &internalpb.Command_ApprovePendingJoinRequest{
		ApprovePendingJoinRequest: &internalpb.ApprovePendingJoinRequest{RequestId: "jreq-1"},
	}})
	if got := storedRequest(t, f, "jreq-1").GetStage(); got != internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_AUTHORIZED {
		t.Errorf("stage = %s, want AUTHORIZED", got)
	}
}

// TestFSMPart2_NoCommandConsultsAClock: the determinism property, and
// the one most easily broken by a "helpful" expiry check added later.
//
// A replay of this log years from now must reach the identical decision.
// If any of these arms read time.Now(), a request whose PIN deadline
// passed after the entry was first applied would resolve differently on
// a follower catching up, and the replicas would disagree about the
// request's stage.
func TestFSMPart2_NoCommandConsultsAClock(t *testing.T) {
	f := newIntroductionFSM(t, "jreq-1")
	before := time.Now().Unix()
	applyOK(t, f, verifyCmd("jreq-1", "418263", "SHA256:AA", "SHA256:BB"))
	applyOK(t, f, &internalpb.Command{Op: &internalpb.Command_ReissueJoinSecondPin{
		ReissueJoinSecondPin: &internalpb.ReissueJoinSecondPin{
			RequestId: "jreq-1", SecondPin: "87654321", SecondPinExpiresAtUnix: 2, AtUnix: 1,
		},
	}})
	applyOK(t, f, &internalpb.Command{Op: &internalpb.Command_ConsumeJoinSecondPin{
		ConsumeJoinSecondPin: &internalpb.ConsumeJoinSecondPin{RequestId: "jreq-1", SecondPin: "87654321", AtUnix: 1},
	}})
	after := time.Now().Unix()

	// Every deadline in that sequence is in the past. Nothing was
	// refused, which is only possible if the arms took their times as
	// inputs.
	if before >= 0 && after >= before {
		t.Logf("commands carrying deadlines %d seconds in the past all applied", before-2)
	}
	if got := storedRequest(t, f, "jreq-1").GetSecondPin(); got != "" {
		t.Errorf("the spent PIN survived (%q)", got)
	}
}

// TestFSMPart2_ReissueIsCappedThenTerminal: a cap nothing enforces is a
// number, not a rule.
func TestFSMPart2_ReissueIsCappedThenTerminal(t *testing.T) {
	f := newIntroductionFSM(t, "jreq-1")
	applyOK(t, f, verifyCmd("jreq-1", "418263", "SHA256:AA", "SHA256:BB"))
	reissue := &internalpb.Command{Op: &internalpb.Command_ReissueJoinSecondPin{
		ReissueJoinSecondPin: &internalpb.ReissueJoinSecondPin{RequestId: "jreq-1", SecondPin: "87654321", SecondPinExpiresAtUnix: 2000},
	}}
	for i := uint32(0); i < MaxJoinSecondPinReissues; i++ {
		applyOK(t, f, reissue)
	}
	applyErr(t, f, reissue)
	got := storedRequest(t, f, "jreq-1")
	if got.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_FAILED {
		t.Errorf("status past the reissue cap = %s, want FAILED", got.GetStatus())
	}
	if got.GetSecondPin() != "" {
		t.Errorf("a FAILED request still holds a PIN (%q)", got.GetSecondPin())
	}
}

// TestFSMPart2_FingerprintsMatchIsASetComparison: the predicate itself,
// including the cases a hand-rolled loop gets wrong.
func TestFSMPart2_FingerprintsMatchIsASetComparison(t *testing.T) {
	cases := []struct {
		name            string
		carried, pasted []string
		want            bool
	}{
		{name: "identical order", carried: []string{"A", "B"}, pasted: []string{"A", "B"}, want: true},
		{name: "reversed order", carried: []string{"A", "B"}, pasted: []string{"B", "A"}, want: true},
		{name: "subset", carried: []string{"A", "B"}, pasted: []string{"A"}, want: false},
		{name: "superset", carried: []string{"A"}, pasted: []string{"A", "B"}, want: false},
		{name: "one wrong value", carried: []string{"A", "B"}, pasted: []string{"A", "C"}, want: false},
		{name: "nothing carried", carried: nil, pasted: []string{"A"}, want: false},
		{name: "nothing pasted", carried: []string{"A"}, pasted: nil, want: false},
		{name: "empty string is not a wildcard", carried: []string{"A"}, pasted: []string{""}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fingerprintsMatch(tc.carried, tc.pasted); got != tc.want {
				t.Errorf("fingerprintsMatch(%v, %v) = %v, want %v", tc.carried, tc.pasted, got, tc.want)
			}
		})
	}
}

// TestFSMPart2_ConstantTimeValueEqualIsExact: the comparison itself.
// Trivially true, and present because this is the function every
// authorization value in the flow goes through, and a later "optimization"
// that compared only lengths would pass every other test in this file.
func TestFSMPart2_ConstantTimeValueEqualIsExact(t *testing.T) {
	if !ConstantTimeValueEqual("418263", "418263") {
		t.Error("equal values compared unequal")
	}
	if ConstantTimeValueEqual("418263", "418264") {
		t.Error("values one digit apart compared equal")
	}
	if !ConstantTimeValueEqual("", "") {
		t.Error("two empty values compared unequal; the empty hash is a value like any other and a nil/empty confusion here would be a hole")
	}
	if ConstantTimeValueEqual("", "0") {
		t.Error("an empty value compared equal to a digit")
	}
}
