// ADR-0147 Part 2's handshake, in the FSM: the stage-one comparison
// against the first code and the fingerprints, the second PIN's
// reissue, and its consumption.
//
// Everything here is a log entry, not a managerd memory, and the reason
// is the one the ADR states: leadership can change at any point in this
// flow. A follower that has not seen the first code verified cannot
// finish the handshake, and a counter held in one managerd's memory
// hands out a fresh five guesses to whoever becomes leader next.
//
// Everything here is also a COMPARISON rather than a boolean the
// caller sent. The values being checked are already in replicated
// state - the request's own code, and the fingerprints it carried - so
// the check is available to the FSM without inventing anything, and
// doing it here means every replica reaches the identical answer from
// the identical entry. A manager-side comparison followed by a "yes it
// matched" command would put the deciding value behind a byte the FSM
// cannot check, and the attempt counter would then be counting
// whatever that manager chose to send.
//
// Expiry is NEVER consulted here. Every deadline in this file arrives
// as a field on the command, and internal/manager checks it once
// against the wall clock before submitting. A wall clock read inside an
// Apply would let a replay years later reject an entry that
// originally succeeded and leave replicas disagreeing about the
// request's stage - the same failure applyResolvePendingJoinRequest's
// own expiry comment describes, and the reason this file exists
// separately from the one that first needed the comment.

package raft

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

const (
	// MaxJoinStageAttempts is the number of wrong values any one stage
	// of the handshake tolerates. Replicated, so a leader change or a
	// managerd restart cannot hand out a fresh allowance, and so two
	// operators cannot each get five guesses by racing across a
	// failover.
	//
	// Exhaustion is TERMINAL. The request becomes FAILED and has to be
	// purged and re-issued; it does not become "pending again with a
	// fresh counter". A request that has attracted five wrong codes is
	// a request something is guessing at, and handing that guesser a
	// new allowance is how a value stops being a gate.
	MaxJoinStageAttempts uint32 = 5

	// MaxJoinSecondPinReissues is how many times the second PIN may be
	// explicitly re-armed. It exists because a failed AddVoter must not
	// restore the PIN that was spent: restoring would be a free retry
	// of a value the operator has already typed once, on a request
	// that has already been acted on. Re-arming is explicit, visible in
	// the log, and capped.
	MaxJoinSecondPinReissues uint32 = 2
)

// ConstantTimeValueEqual compares two human-entered authorization
// values in constant time, on the hash of each.
//
// Hashed first because these are strings of varying length: a raw
// subtle.ConstantTimeCompare returns immediately on a length mismatch,
// which would leak the length of the value being guessed. Hashing makes
// both sides fixed-width, so the comparison below runs the same
// instruction sequence whatever the caller typed.
//
// Used for all THREE values in this flow - the first code, each
// fingerprint, and the second PIN - through one function, so there is
// no fourth place that remembers to be careful.
//
// Exported because internal/manager makes one comparison the FSM cannot
// make (whether this member's own certificate is among the fingerprints
// a request advertised) and must not keep a second copy of the rule.
func ConstantTimeValueEqual(a, b string) bool {
	sumA := sha256.Sum256([]byte(a))
	sumB := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(sumA[:], sumB[:]) == 1
}

// HashJoinValue is the SHA-256 of one human-entered value, hex-encoded.
//
// Exported for the same reason ConstantTimeValueEqual is: internal/manager
// needs it for the post-acceptance status poll, and two definitions of
// "the digest of a pasted value" is two chances for them to disagree
// about which record a poll is being checked against.
//
// Deliberately not salted. There is no secret to salt with - the whole
// design is that no shared key exists - and the value it protects is a
// six-digit number that has already been publicly transcribed between
// two operators. It is a handle for equality, not a password hash.
func HashJoinValue(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// fingerprintsMatch reports whether the fingerprints an operator
// pasted are exactly the ones the request carried.
//
// Both directions matter, and a subset match is NOT a pass. An operator
// who pastes only the first of two advertised fingerprints has not
// compared the second, and the second is precisely the one that would
// have caught a request carrying a certificate the operator never
// looked at. So the lengths must agree and every carried value must
// have been pasted - a set comparison, not a containment check.
//
// The length comparison is not a timing leak in any sense that matters
// here: the operator is pasting a value they can read off the
// requester's screen, and the count of advertised certificates is
// public. What is protected is the CONTENT, and that is compared with
// the same hashed constant-time compare as every other value here.
func fingerprintsMatch(carried, pasted []string) bool {
	if len(carried) == 0 || len(pasted) != len(carried) {
		return false
	}
	// int accumulator rather than a bool: a boolean short-circuits in
	// the generated code, and the point of the scan is that it does
	// not. Every comparison below runs, every time.
	covered := 0
	for _, want := range carried {
		for _, got := range pasted {
			covered |= boolToInt(ConstantTimeValueEqual(want, got))
		}
	}
	return covered == len(carried)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// exhaustedJoinStage names the two caps in one message shape, so the
// two calls that enforce them cannot drift apart in what they tell an
// operator.
func exhaustedJoinStage(requestID string, stage internalpb.JoinRequestStage) string {
	return fmt.Sprintf(
		"request_id %q has used all %d attempts at stage %s and is now FAILED. It is terminal: purge it and have the joining Comb request again. It does not return to pending with a fresh allowance, because a request that has attracted %d wrong values is a request something is guessing at",
		requestID, MaxJoinStageAttempts, stage, MaxJoinStageAttempts)
}

// failJoinStageAttempt advances the per-stage counter and, on
// exhaustion, makes the request terminal.
//
// The counter is written to the map BEFORE the error is returned, and
// that ordering is deliberate: the manager that receives a refusal also
// needs the increment to be durable, or five refusals in a row would
// each be the first. The Apply's Error is a report about the command,
// not a rollback.
//
// On exhaustion it also drops the joiner's unpromoted pin, inside this
// same entry, for the reason applyResolvePendingJoinRequest does it
// there: a request that reached a terminal state without becoming a
// voter must not leave a standing trust anchor behind, and a cleanup
// a caller has to remember to send is a cleanup an interrupted
// approval leaves behind forever.
func (f *FSM) failJoinStageAttempt(updated *internalpb.PendingJoinRequest, stage internalpb.JoinRequestStage) *internalpb.PendingJoinRequest {
	attempts := updated.GetFirstCodeAttempts() + 1
	if stage == internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED {
		attempts = updated.GetSecondPinAttempts() + 1
	}
	updated.SecondPin = ""
	if stage == internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED {
		updated.FirstCodeAttempts = attempts
	} else {
		updated.SecondPinAttempts = attempts
	}
	if attempts < MaxJoinStageAttempts {
		return updated
	}
	// Terminal. The first code is cleared on the way out whatever
	// happens, because a value that has been guessed at five times is
	// not one to leave in replicated state.
	updated.Code = ""
	updated.AcceptedIntroductionCodeSha256 = ""
	updated.Status = internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_FAILED
	f.dropUnpromotedPin(updated.GetNodeId())
	return updated
}

// applyVerifyJoinIntroduction is stage one: the target's operator has
// pasted the first code the requester generated and the fingerprints
// the request carried, and this entry decides whether they match.
//
// On success, in ONE entry: the stage advances to CODE_VERIFIED, the
// first code is CLEARED (a spent value is not left in replicated state
// for a later reader to find), the second PIN supplied on the command
// is stored with its deadline, and the joiner's leaf is evaluated and
// pinned. Those are deliberately not separate commands - see
// VerifyJoinIntroduction's own comment in api/internalpb/state.proto
// for why a window between "the certificate was accepted" and "the
// certificate is pinned" is the state the trust store exists to make
// impossible.
//
// On failure: the attempt counter advances, and the 5th failure makes
// the request FAILED.
func (f *FSM) applyVerifyJoinIntroduction(index uint64, cmd *internalpb.VerifyJoinIntroduction) *FSMApplyResult {
	req, exists := f.pendingJoinRequests[cmd.GetRequestId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("VerifyJoinIntroduction: request_id %q does not exist", cmd.GetRequestId())}
	}
	if req.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("VerifyJoinIntroduction: request_id %q is %s, not pending", cmd.GetRequestId(), req.GetStatus())}
	}
	// Re-introducing a request that is already CODE_VERIFIED is
	// refused by name rather than treated as another attempt. The
	// operator pasted a first code for a request whose first code has
	// already been accepted and cleared, and the answer to that is
	// "paste the second PIN", not "try again". ReissueJoinSecondPin is
	// the other door into the same state and it is a separate, logged
	// act.
	if req.GetStage() != internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"VerifyJoinIntroduction: request_id %q is already at stage %s, so there is no first code left to verify. Paste the second PIN into the Approve form, or reissue the PIN explicitly if it has expired",
			cmd.GetRequestId(), req.GetStage())}
	}
	// FAIL-CLOSED on fingerprints, before anything else is looked at.
	// A request carrying no fingerprints can never be verified: the
	// operator would be comparing against nothing, and the comparison
	// is the only thing binding this request to a machine. The
	// pre-ADR-0147 UI rendered "no TLS certificate presented" and
	// approved on it anyway; this is the refusal that replaced it.
	if len(req.GetAdvertisedFingerprints()) == 0 {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"VerifyJoinIntroduction: request_id %q advertised no certificate fingerprints. A request with nothing to compare cannot be verified and can never be approved - have the joining Comb re-request with `apiaryctl join-introduce`, which reads its own certificate files",
			cmd.GetRequestId())}
	}
	updated := proto.Clone(req).(*internalpb.PendingJoinRequest)
	if !ConstantTimeValueEqual(cmd.GetIntroductionCode(), updated.GetCode()) {
		f.pendingJoinRequests[cmd.GetRequestId()] = f.failJoinStageAttempt(updated, internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED)
		attempts := f.pendingJoinRequests[cmd.GetRequestId()].GetFirstCodeAttempts()
		if updated.GetStatus() == internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_FAILED {
			return &FSMApplyResult{Index: index, Error: exhaustedJoinStage(cmd.GetRequestId(), internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED)}
		}
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"VerifyJoinIntroduction: the first code pasted for request_id %q does not match the one that request carried (that is what the requesting Comb's own screen shows). %d of %d attempts used",
			cmd.GetRequestId(), attempts, MaxJoinStageAttempts)}
	}
	if !fingerprintsMatch(req.GetAdvertisedFingerprints(), cmd.GetFingerprints()) {
		f.pendingJoinRequests[cmd.GetRequestId()] = f.failJoinStageAttempt(updated, internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED)
		attempts := f.pendingJoinRequests[cmd.GetRequestId()].GetFirstCodeAttempts()
		if updated.GetStatus() == internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_FAILED {
			return &FSMApplyResult{Index: index, Error: exhaustedJoinStage(cmd.GetRequestId(), internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED)}
		}
		// BOTH values, because "these do not match" with nothing to
		// compare them against is the message an operator cannot act on
		// - and a fingerprint mismatch is a REFUSAL, not a warning, so
		// it has to say what was carried and what was pasted.
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"VerifyJoinIntroduction: the fingerprints pasted for request_id %q do not match the ones that request carried.\n  the request carried: %v\n  what was pasted:    %v\nRead both off the requesting Comb's own screen (`apiaryctl join-introduce --field fingerprints`). %d of %d attempts used",
			cmd.GetRequestId(), req.GetAdvertisedFingerprints(), cmd.GetFingerprints(), attempts, MaxJoinStageAttempts)}
	}
	// The first code is spent. Cleared here, in the same entry that
	// accepts it, so there is no window in which an accepted code is
	// still sitting in replicated state.
	updated.Code = ""
	// The digest of what was just accepted, so the requester's later
	// status poll can be checked against SOMETHING without the spent
	// value being back in the record. It is cleared in the same log
	// entry that spends the PIN below.
	updated.AcceptedIntroductionCodeSha256 = HashJoinValue(cmd.GetIntroductionCode())
	updated.Stage = internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED
	updated.SecondPin = cmd.GetSecondPin()
	updated.SecondPinExpiresAtUnix = cmd.GetSecondPinExpiresAtUnix()
	// The pin, in this same entry. cmd.Peer is re-evaluated here rather
	// than trusted: applyPinTrustedPeer runs the fail-closed
	// hostcert.Evaluate and re-derives the fingerprint from the PEM, so
	// a peer whose stated fingerprint disagrees with the certificate it
	// carries cannot be stored at all. A peer that fails to evaluate
	// fails the whole entry - a request that is CODE_VERIFIED with no
	// trust anchor behind it is exactly the state this design refuses
	// to create.
	if peer := cmd.GetPeer(); peer != nil {
		pinned := f.applyPinTrustedPeer(index, peer)
		// A pin that already exists for this node_id is a refusal from
		// applyPinTrustedPeer, deliberately: replacing a pinned
		// certificate is meant to be two visible acts in the log rather
		// than one invisible one. It is NOT a refusal here when the
		// existing pin records the SAME certificate and is already a
		// member's - in that case there is nothing to change, and
		// treating it as an error would make a re-joining member
		// permanently unintroducible, which is the kind of unrecoverable
		// gate this codebase has been bitten by before.
		//
		// A pin for a DIFFERENT certificate keeps the create-only
		// refusal, which is the case the rule exists for.
		if pinned.Error != "" && f.alreadyPinnedAsMember(peer.GetNodeId(), peer.GetFingerprint()) {
			pinned = &FSMApplyResult{Index: index}
		}
		if pinned.Error != "" {
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
				"VerifyJoinIntroduction: request_id %q is refused because the certificate it advertised did not pass the trust gate: %s",
				cmd.GetRequestId(), pinned.Error)}
		}
	}
	f.pendingJoinRequests[cmd.GetRequestId()] = updated
	return &FSMApplyResult{Index: index, PendingJoinRequest: updated}
}

// alreadyPinnedAsMember reports whether nodeID is already pinned for
// exactly this fingerprint AND that pin has been promoted to a member.
//
// Both conditions, and the second is the load-bearing one. An existing
// member's certificate is already trusted by definition, so a request
// naming it has nothing to add. A pin that is NOT a member's belongs to
// a request still in progress, and a second request for the same
// node_id is a collision this flow has no way to interpret - so the
// create-only refusal stands and the operator is told.
func (f *FSM) alreadyPinnedAsMember(nodeID, fingerprint string) bool {
	existing, ok := f.trustedPeers[nodeID]
	if !ok || !existing.GetIsVoter() {
		return false
	}
	return ConstantTimeValueEqual(existing.GetFingerprint(), fingerprint)
}

// applyReissueJoinSecondPin generates a new second PIN and counts the
// re-arm, capped at MaxJoinSecondPinReissues.
//
// The PIN supplied on a command that is REFUSED is discarded rather
// than stored, so a caller that generated one speculatively has leaked
// nothing - the same property the failure path of
// applyVerifyJoinIntroduction has.
func (f *FSM) applyReissueJoinSecondPin(index uint64, cmd *internalpb.ReissueJoinSecondPin) *FSMApplyResult {
	req, exists := f.pendingJoinRequests[cmd.GetRequestId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("ReissueJoinSecondPin: request_id %q does not exist", cmd.GetRequestId())}
	}
	if req.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("ReissueJoinSecondPin: request_id %q is %s, not pending", cmd.GetRequestId(), req.GetStatus())}
	}
	if req.GetStage() != internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"ReissueJoinSecondPin: request_id %q is at stage %s, so there is no second PIN to reissue. The first code and fingerprints have to be verified first",
			cmd.GetRequestId(), req.GetStage())}
	}
	updated := proto.Clone(req).(*internalpb.PendingJoinRequest)
	reissues := updated.GetSecondPinReissues() + 1
	if reissues > MaxJoinSecondPinReissues {
		// Past the cap: terminal, not a refusal with the request still
		// pending. A request that has had its PIN re-armed the maximum
		// number of times and asked again is one whose operator is not
		// completing the handshake, and leaving it actionable would make
		// the cap advisory.
		updated.SecondPin = ""
		updated.AcceptedIntroductionCodeSha256 = ""
		updated.SecondPinReissues = reissues
		updated.Status = internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_FAILED
		f.pendingJoinRequests[cmd.GetRequestId()] = updated
		f.dropUnpromotedPin(updated.GetNodeId())
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"ReissueJoinSecondPin: request_id %q has been re-issued %d times, which is the cap, and is now FAILED. A PIN that keeps needing re-arming is a request nobody is completing - purge it and have the joining Comb request again",
			cmd.GetRequestId(), reissues)}
	}
	updated.SecondPinReissues = reissues
	updated.SecondPin = cmd.GetSecondPin()
	updated.SecondPinExpiresAtUnix = cmd.GetSecondPinExpiresAtUnix()
	f.pendingJoinRequests[cmd.GetRequestId()] = updated
	return &FSMApplyResult{Index: index, PendingJoinRequest: updated}
}

// applyConsumeJoinSecondPin spends the second PIN, and is a SEPARATE
// command from ApprovePendingJoinRequest for the reason the ADR is
// explicit about: the PIN is cleared before AddVoter is called, so a
// membership change that then fails does not leave a live PIN behind on
// a request that has already been acted on once. A failed AddVoter does
// not put it back; re-arming is ReissueJoinSecondPin, visible and
// capped.
//
// The stage does NOT advance to AUTHORIZED here. Authorization is
// APPROVED - a membership change that actually happened - and conflating
// the two would let a request claim to be authorized after an AddVoter
// that failed.
func (f *FSM) applyConsumeJoinSecondPin(index uint64, cmd *internalpb.ConsumeJoinSecondPin) *FSMApplyResult {
	req, exists := f.pendingJoinRequests[cmd.GetRequestId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("ConsumeJoinSecondPin: request_id %q does not exist", cmd.GetRequestId())}
	}
	if req.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("ConsumeJoinSecondPin: request_id %q is %s, not pending", cmd.GetRequestId(), req.GetStatus())}
	}
	updated := proto.Clone(req).(*internalpb.PendingJoinRequest)
	if !ConstantTimeValueEqual(cmd.GetSecondPin(), updated.GetSecondPin()) || updated.GetSecondPin() == "" {
		f.pendingJoinRequests[cmd.GetRequestId()] = f.failJoinStageAttempt(updated, internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED)
		attempts := f.pendingJoinRequests[cmd.GetRequestId()].GetSecondPinAttempts()
		if updated.GetStatus() == internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_FAILED {
			return &FSMApplyResult{Index: index, Error: exhaustedJoinStage(cmd.GetRequestId(), internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED)}
		}
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"ConsumeJoinSecondPin: the second PIN pasted for request_id %q does not match the one this Colony generated, or that PIN has already been spent. It is the value the requesting Comb's own screen shows. %d of %d attempts used",
			cmd.GetRequestId(), attempts, MaxJoinStageAttempts)}
	}
	// Spent. Cleared in this same entry, together with the digest of the
	// first code - both exist only to protect a PIN that no longer
	// exists, so neither outlives it.
	updated.SecondPin = ""
	updated.AcceptedIntroductionCodeSha256 = ""
	updated.SecondPinAttempts = updated.GetSecondPinAttempts() + 1
	f.pendingJoinRequests[cmd.GetRequestId()] = updated
	return &FSMApplyResult{Index: index, PendingJoinRequest: updated}
}
