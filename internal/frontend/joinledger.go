// The guided Colony-join ledger (ADR-0163): a two-column, READ-ONLY
// view of the two-way join handshake, rendered on the TARGET Colony's
// control host.
//
// The left column is the JOINING Comb's steps; the right column is the
// TARGET Colony's. Every step says which Comb the action happens on, what
// the action is, and what value it produces or consumes.
//
// What this file deliberately does NOT do is the important part:
//
//   - It never submits a first code, a fingerprint set, or a second PIN
//     to the target on the operator's behalf. ADR-0147's stage one is an
//     ATTESTATION: it proves a person read the first code off the
//     requesting Comb's own screen. A surface that submitted the value
//     would satisfy the attestation by itself, which is exactly what
//     stage one exists to prevent.
//
//   - It never holds a secret. The first code and the fingerprints are
//     the JOINING Comb's own values and are rendered on the joining
//     column; the second PIN is never rendered on the target's side at
//     all, because a target Admin who can read it can complete the
//     handshake alone and collapse it back into one party.
//
//   - It adds no RPC, no FSM command and no replicated state. Every
//     value it renders comes from state that already exists and is
//     already replicated - the join window, the pending request's stage
//     and status, and the request's own advertised fingerprints. That is
//     what makes the flow survive a leadership change: "done" is a
//     property of replicated state, not of a session on a particular
//     node, so there is no server-side progress to lose and no
//     browser-side progress to lose either.
//
// What it IS is a place where an operator can see, in one screen, which
// side of the handshake they are on, what has already been recorded, and
// what to do next - which is what issue #79 asked for and what neither
// the Machine page nor the landing page could show.

package frontend

import (
	"net/http"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// joinLedgerStepState is a step's state in the ledger.
//
// The four states are deliberately distinct. A flow that collapsed
// "blocked" into "not done" would tell an operator that a closed join
// window and an unverified first code are the same situation, and they
// are not: one is fixed by opening a window, the other by a human
// comparing two values.
type joinLedgerStepState string

const (
	// joinLedgerStepDone means the value the step produces or consumes is
	// recorded, and the ledger can say so from replicated state.
	joinLedgerStepDone joinLedgerStepState = "done"

	// joinLedgerStepWaiting means this step is the next one and nothing
	// has happened yet.
	joinLedgerStepWaiting joinLedgerStepState = "waiting"

	// joinLedgerStepBlocked means a precondition is unmet: the join
	// window is closed, the request was rejected, the second PIN
	// expired, or the request was cancelled.
	joinLedgerStepBlocked joinLedgerStepState = "blocked"

	// joinLedgerStepUnknown means this page cannot tell. It is not an
	// error state - it is the honest answer when the read that would
	// decide the step failed, and it is rendered as such rather than
	// as "not done", which would tell the operator to wait for
	// something that will never arrive.
	joinLedgerStepUnknown joinLedgerStepState = "unknown"
)

// joinLedgerStep is one step of the handshake.
//
// Comb says which machine the action happens on, in the operator's own
// words ("the JOINING Comb" or "this Colony"), because the whole point
// of the ledger is that the operator can no longer be confused about
// which host they are looking at.
type joinLedgerStep struct {
	// Comb names the machine the action happens on.
	Comb string
	// Action is the step itself, in the imperative.
	Action string
	// Detail is the value the step produces or consumes, or what the
	// operator needs to have at hand to perform it.
	Detail string
	// State is this step's state.
	State joinLedgerStepState
	// Note explains a blocked or unknown state in one sentence.
	Note string
}

// joinLedgerRequest identifies the pending request the ledger is about.
//
// A ledger with no request has nothing to show, because the handshake
// does not start until a request exists: the first code is generated on
// the JOINING Comb and travels with that request, so before a request
// there is no first code, no fingerprint set and no stage.
type joinLedgerRequest struct {
	RequestID       string
	NodeID          string
	RaftBindAddress string
	// Fingerprints are the values the JOINING Comb advertised. They
	// are rendered on the JOINING column, because they are that
	// Comb's own values, read by that Comb's operator.
	Fingerprints []string
}

// joinLedgerView is the template-facing shape of the whole ledger.
type joinLedgerView struct {
	// Request is the pending request being ledged, or nil when there
	// is none yet.
	Request *joinLedgerRequest

	// Joining and Target are the two columns.
	Joining []joinLedgerStep
	Target  []joinLedgerStep

	// WindowLive is whether the Colony's admission window is open.
	WindowLive bool
	// WindowError is the window read's own refusal or failure, verbatim.
	WindowError string
	// WindowReadFailed separates "the window is closed" from "this page
	// could not find out".
	WindowReadFailed bool
	// WindowExpiresAt is when the live window lapses, as ready-to-render
	// text, or "" when there is no live window.
	WindowExpiresAt string
	// WindowRemaining is the live window's remaining time as text.
	WindowRemaining string

	// SecondPinExpires is when the target's second PIN lapses, or "".
	// The VALUE of the PIN is never on this struct.
	SecondPinExpires string
	// SecondPinReissues is how many of the two permitted re-arms have
	// been used.
	SecondPinReissues uint32

	// FirstCodeAttempts and AttemptsMax are the replicated per-stage
	// counters, so an operator about to use the last of five knows it.
	FirstCodeAttempts uint32
	AttemptsMax       uint32

	// Stage is the request's current stage, as the RPC spells it.
	Stage string
	// Status is the request's current status, as the RPC spells it.
	Status string
	// Approved is true once the request has been approved.
	Approved bool
	// Rejected is true once the request has been rejected.
	Rejected bool
	// Cancelled is true once the request has been cancelled.
	Cancelled bool
	// Failed is true once the request has failed. FAILED is TERMINAL in
	// ADR-0147 Part 2, like approved and rejected: the record stays so
	// the joiner's own poll resolves, and Purge is how it goes away.
	Failed bool
}

// handleJoinLedger serves the guided join flow at GET /machine/join.
//
// Admin-only, and deliberately so. The ledger renders the Colony's
// pending join requests and the fingerprints a joining Comb advertised,
// and it is served by a Comb a hostile Admin controls - the same
// reasoning ADR-0147 Part 3 applies to the pending-requests panel. A
// Viewer-ticket operator has no business reading the Colony's join
// traffic.
//
// The page is a GET and takes no parameters: every value it shows is
// read from replicated state, and there is nothing an operator can set
// on it. A reload re-reads the same state, so a leadership change or a
// second PIN issued while the page was open is picked up by reloading.
func (s *Server) handleJoinLedger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.renderJoinLedger(w, r, s.buildJoinLedger(r, time.Now()))
}

// renderJoinLedger renders the ledger. The view is built separately from
// the render so a test can assert on the built steps without a template.
//
// The page is rendered with the same Status fetch the other pages use, so
// the nav and the CanAdmin flag come from one place and cannot disagree
// with each other.
func (s *Server) renderJoinLedger(w http.ResponseWriter, r *http.Request, view joinLedgerView) {
	statusResp, statusErr := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	s.render(w, "join_ledger_page", s.withAuthFieldsFrom(r, pageData{
		JoinLedger:  view,
		ActivePage:  "machine",
		LocalNodeID: s.localNodeID(r),
	}, statusResp, statusErr))
}
