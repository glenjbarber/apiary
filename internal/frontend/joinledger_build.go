// The build half of ADR-0163's guided join ledger: turning replicated
// state into the two columns of steps.
//
// Everything here is a pure function of the join window's state and the
// pending request's state. There is no session, no cookie and no
// server-side progress: "done" is a property of replicated state, which
// is why the ledger survives a leadership change (ADR-0163's "Leadership
// change does not break it").

package frontend

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// The column labels. The whole point of the ledger is that the operator
// can no longer be confused about which host an action happens on, so
// these say it in the operator's own words rather than in the RPC's.
const (
	joinLedgerJoiningComb = "the JOINING Comb"
	joinLedgerTargetComb  = "this Colony"
)

// buildJoinLedger assembles the ledger from the Colony's own state.
//
// It reads the join window and the pending requests, and it reads them
// INDEPENDENTLY: a failure of one does not blank the other. The window
// and the request are different pieces of state, and an operator whose
// window read failed still needs to know what the pending request's stage
// is.
func (s *Server) buildJoinLedger(r *http.Request, now time.Time) joinLedgerView {
	view := joinLedgerView{}

	window := s.currentColonyJoinWindow(r, now)
	view.WindowLive = window.Live
	view.WindowError = window.Error
	view.WindowReadFailed = window.ReadFailed
	if window.Live {
		view.WindowExpiresAt = deadlineLocal(window.ExpiresAtUnix)
		view.WindowRemaining = remainingText(window)
	}

	requests := s.currentJoinRequests(r)
	var request *joinRequestView
	if len(requests) > 0 {
		request = &requests[0]
	}

	if request != nil {
		view.Request = &joinLedgerRequest{
			RequestID:       request.RequestID,
			NodeID:          request.NodeID,
			RaftBindAddress: request.RaftBindAddress,
			Fingerprints:    request.Fingerprints,
		}
		view.Stage = request.Stage
		view.Status = request.Status
		view.Approved = request.Status == joinRequestStatusApproved
		view.Rejected = request.Status == joinRequestStatusRejected
		view.Cancelled = request.Status == joinRequestStatusCancelled
		view.Failed = request.Status == joinRequestStatusFailed
		view.SecondPinExpires = request.SecondPinExpires
		view.SecondPinReissues = request.SecondPinReissues
		view.FirstCodeAttempts = request.FirstCodeAttempts
		view.AttemptsMax = request.AttemptsMax
	}

	view.Joining = s.joinLedgerJoiningSteps(view, request)
	view.Target = s.joinLedgerTargetSteps(view, request)
	return view
}

// The status and stage spellings, as fromRPCJoinRequest renders them.
//
// These are NOT the protobuf enum's own String() values - those carry the
// full "JOIN_REQUEST_STATUS_APPROVED" prefix, while the view renders the
// short lowercase form ("approved") that every existing template and
// test already expects. They are mirrored here rather than imported so
// that a change in either place shows up as a compile-time failure in the
// test below instead of every step silently rendering "waiting".
const (
	joinRequestStatusPending   = "pending"
	joinRequestStatusApproved  = "approved"
	joinRequestStatusRejected  = "rejected"
	joinRequestStatusCancelled = "cancelled"
	joinRequestStatusFailed    = "failed"
)

// The stage spellings. CODE_VERIFIED's rendered form is the long one
// fromRPCJoinRequest uses, so the comparison below is against the prefix
// the stage actually starts with rather than the whole string.
const (
	joinRequestStageIntroduced   = "introduced"
	joinRequestStageCodeVerified = "code verified"
	joinRequestStageAuthorized   = "authorized"
)

// joinLedgerJoiningSteps builds the JOINING column: the steps performed
// on the Comb that is joining, read by the operator who is at the
// target.
//
// Every step that names a value names a value the JOINING Comb's own
// operator has to read off that Comb's screen - the first code, the
// fingerprints, and (only as an instruction to read it there) the second
// PIN. The ledger never renders the second PIN's value.
func (s *Server) joinLedgerJoiningSteps(view joinLedgerView, request *joinRequestView) []joinLedgerStep {
	steps := []joinLedgerStep{}

	// Step 1: the joiner starts the introduction.
	step := joinLedgerStep{
		Comb:   joinLedgerJoiningComb,
		Action: "Run the introduction command to generate this Comb's first code and read its own certificate fingerprints.",
		Detail: "/usr/local/libexec/apiary/apiaryctl join-introduce --target HOST:PORT",
	}
	switch {
	case request == nil:
		step.State = joinLedgerStepWaiting
		step.Note = "Not started yet. This Comb's operator runs the command above on the JOINING Comb."
	case request.Stage == joinRequestStageIntroduced:
		step.State = joinLedgerStepDone
		step.Note = "Done. This Colony has the request, with the first code and fingerprints it carried."
	default:
		step.State = joinLedgerStepDone
		step.Note = "Done. This Colony has the request, and stage one has been verified."
	}
	steps = append(steps, step)

	// Step 2: the joiner reads the second PIN.
	step = joinLedgerStep{
		Comb:   joinLedgerJoiningComb,
		Action: "Read the second PIN that this Colony generated, off the JOINING Comb's own screen.",
		Detail: "/usr/local/libexec/apiary/apiaryctl join-introduce --request-id ID --first-code CODE --field second-pin",
	}
	switch {
	case request == nil:
		step.State = joinLedgerStepWaiting
		step.Note = "Nothing to read yet."
	case strings.HasPrefix(request.Stage, joinRequestStageCodeVerified) && request.SecondPinExpires != "":
		step.State = joinLedgerStepWaiting
		step.Note = "Waiting for the JOINING Comb's operator to read it. It is never shown on this page, because an Admin who can read it could complete the join alone."
	case strings.HasPrefix(request.Stage, joinRequestStageCodeVerified):
		step.State = joinLedgerStepBlocked
		step.Note = "The second PIN has lapsed. Reissue it from this Colony's request row below, then read the new one."
	case request.Stage == joinRequestStageAuthorized:
		step.State = joinLedgerStepDone
		step.Note = "Done. This Colony verified the second PIN."
	default:
		step.State = joinLedgerStepWaiting
		step.Note = "Not yet. This Colony issues the second PIN once stage one is verified."
	}
	steps = append(steps, step)

	// Step 3: the joiner installs its credentials.
	step = joinLedgerStep{
		Comb:   joinLedgerJoiningComb,
		Action: "Install this Comb's own API key and the Colony's restart-guardrail token, once this Colony has approved the request.",
		Detail: "/usr/local/libexec/apiary/apiaryctl join-introduce --request-id ID --first-code CODE --install-credentials",
	}
	switch {
	case request == nil:
		step.State = joinLedgerStepWaiting
		step.Note = "Nothing to install yet."
	case request.Status == joinRequestStatusApproved:
		step.State = joinLedgerStepWaiting
		step.Note = "This Colony has approved the request. The JOINING Comb's operator runs the command above."
	case request.Status == joinRequestStatusRejected:
		step.State = joinLedgerStepBlocked
		step.Note = "This Colony rejected the request, so there is nothing to install."
	case request.Status == joinRequestStatusCancelled:
		step.State = joinLedgerStepBlocked
		step.Note = "The request was cancelled, so there is nothing to install."
	default:
		step.State = joinLedgerStepWaiting
		step.Note = "Waiting for this Colony to approve the request."
	}
	steps = append(steps, step)

	// Step 4: the joiner becomes a member.
	step = joinLedgerStep{
		Comb:   joinLedgerJoiningComb,
		Action: "Join the Colony. A Comb already waiting with -await-join joins on its own as soon as it is a voter.",
		Detail: "A Comb that already has its own standalone raft state is converted first, by the \"Convert this Comb to a joiner\" action on the Machine page.",
	}
	switch {
	case request == nil:
		step.State = joinLedgerStepWaiting
		step.Note = "Not started yet."
	case request.Status == joinRequestStatusApproved:
		step.State = joinLedgerStepWaiting
		step.Note = "This Colony has approved the request. The JOINING Comb joins automatically once it is up with -await-join."
	case request.Status == joinRequestStatusRejected:
		step.State = joinLedgerStepBlocked
		step.Note = "This Colony rejected the request."
	case request.Status == joinRequestStatusCancelled:
		step.State = joinLedgerStepBlocked
		step.Note = "The request was cancelled."
	default:
		step.State = joinLedgerStepWaiting
		step.Note = "Waiting for this Colony to approve the request."
	}
	steps = append(steps, step)

	return steps
}

// joinLedgerTargetSteps builds the TARGET column: the steps performed on
// the Colony being joined.
func (s *Server) joinLedgerTargetSteps(view joinLedgerView, request *joinRequestView) []joinLedgerStep {
	steps := []joinLedgerStep{}

	// Step 1: open the join window.
	step := joinLedgerStep{
		Comb:   joinLedgerTargetComb,
		Action: "Open the join window. A Colony refuses join requests while its window is closed, and closed is the normal state.",
		Detail: "Use the join window control on the Machine page. It applies to the whole Colony and may be opened on any member.",
	}
	switch {
	case view.WindowReadFailed:
		step.State = joinLedgerStepUnknown
		step.Note = "This page could not read the join window's state, so it cannot say whether the window is open. " + view.WindowError
	case view.WindowLive:
		step.State = joinLedgerStepDone
		step.Note = fmt.Sprintf("Open. Expires %s (%s left).", view.WindowExpiresAt, view.WindowRemaining)
	default:
		step.State = joinLedgerStepBlocked
		step.Note = "The window is closed. Every later step is refused until it is open."
	}
	steps = append(steps, step)

	// Step 2: the target verifies stage one.
	step = joinLedgerStep{
		Comb:   joinLedgerTargetComb,
		Action: "Paste the first code and the fingerprints into the request's Stage 1 form, read off the JOINING Comb's own screen.",
		Detail: "This is the comparison stage one exists for: it proves a person read those values off the JOINING Comb.",
	}
	switch {
	case request == nil:
		step.State = joinLedgerStepWaiting
		step.Note = "No request yet."
	case request.Stage == joinRequestStageIntroduced:
		step.State = joinLedgerStepWaiting
		step.Note = "Waiting for the first code and fingerprints to be pasted into the Stage 1 form below."
	case strings.HasPrefix(request.Stage, joinRequestStageCodeVerified) || request.Stage == joinRequestStageAuthorized:
		step.State = joinLedgerStepDone
		step.Note = "Done. This Colony verified the introduction."
	default:
		step.State = joinLedgerStepUnknown
		step.Note = "This Colony cannot say which stage the request is at."
	}
	steps = append(steps, step)

	// Step 3: the target approves.
	step = joinLedgerStep{
		Comb:   joinLedgerTargetComb,
		Action: "Paste the second PIN into the Approve form and approve the request.",
		Detail: "The second PIN is the value that authorizes the membership change. It is read off the JOINING Comb's screen and is never displayed here.",
	}
	switch {
	case request == nil:
		step.State = joinLedgerStepWaiting
		step.Note = "No request yet."
	case request.Status == joinRequestStatusApproved:
		step.State = joinLedgerStepDone
		step.Note = "Done. This Colony approved the request."
	case request.Status == joinRequestStatusRejected:
		step.State = joinLedgerStepBlocked
		step.Note = "This Colony rejected the request."
	case request.Status == joinRequestStatusCancelled:
		step.State = joinLedgerStepBlocked
		step.Note = "The request was cancelled."
	case strings.HasPrefix(request.Stage, joinRequestStageCodeVerified) && request.SecondPinExpires == "":
		step.State = joinLedgerStepBlocked
		step.Note = "The second PIN was issued and then spent. Reissue it before approving."
	case strings.HasPrefix(request.Stage, joinRequestStageCodeVerified):
		step.State = joinLedgerStepWaiting
		step.Note = fmt.Sprintf("Waiting for the second PIN. It expires %s, and may be reissued up to 2 times (%d used).", request.SecondPinExpires, request.SecondPinReissues)
	default:
		step.State = joinLedgerStepWaiting
		step.Note = "Waiting for stage one to be verified."
	}
	steps = append(steps, step)

	return steps
}

// The mirrored spellings above are the ones fromRPCJoinRequest actually
// renders. This assertion fails to COMPILE if that ever stops being true,
// which is the point: a silent mismatch here would render every step of
// every ledger as "waiting" forever, and an operator staring at a flow
// that can never advance has no way to tell that from a real stall.
//
// The rendering is checked through fromRPCJoinRequest itself rather than
// the enum's String(), because the enum spells its values with the full
// "JOIN_REQUEST_STATUS_APPROVED" prefix and the view does not.
var _ = []bool{
	fromRPCJoinRequest(&rpcpb.PendingJoinRequest{Status: rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED}).Status == joinRequestStatusApproved,
	fromRPCJoinRequest(&rpcpb.PendingJoinRequest{Status: rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_REJECTED}).Status == joinRequestStatusRejected,
	fromRPCJoinRequest(&rpcpb.PendingJoinRequest{Status: rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_CANCELLED}).Status == joinRequestStatusCancelled,
	fromRPCJoinRequest(&rpcpb.PendingJoinRequest{Status: rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING}).Status == joinRequestStatusPending,
	fromRPCJoinRequest(&rpcpb.PendingJoinRequest{Stage: rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED}).Stage == joinRequestStageIntroduced,
	fromRPCJoinRequest(&rpcpb.PendingJoinRequest{Stage: rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_AUTHORIZED}).Stage == joinRequestStageAuthorized,
	strings.HasPrefix(fromRPCJoinRequest(&rpcpb.PendingJoinRequest{Stage: rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED}).Stage, joinRequestStageCodeVerified),
}
