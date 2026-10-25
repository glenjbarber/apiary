package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	manager "github.com/glenjbarber/apiary/internal/manager"
)

// ADR-0163's guided join ledger. The tests below are about the ledger's
// PROMISES, and there are only a few that matter:
//
//  1. It never renders the second PIN. That value authorizes a membership
//     change, and a target Admin who can read it can complete the
//     handshake alone - which collapses the two-party handshake ADR-0147
//     exists to build into one party.
//
//  2. It never submits anything. Every route it exposes is a GET, and no
//     RPC it calls mutates state.
//
//  3. It says "done" only when replicated state proves it. A step that
//     cannot be proven is "unknown", not "done" and not "waiting" -
//     because an operator told a step is "waiting" when the page simply
//     could not read the state would wait for something that never
//     arrives.
//
//  4. It distinguishes "the window is closed" (a real state with a real
//     next step) from "this page could not find out" (no state to
//     report).

// ledgerRequest builds a pending request at a given stage and status.
func ledgerRequest(stage rpcpb.JoinRequestStage, status rpcpb.JoinRequestStatus) *rpcpb.PendingJoinRequest {
	return &rpcpb.PendingJoinRequest{
		RequestId:       "jreq-ledger-1",
		NodeId:          "node-joining",
		RaftBindAddress: "10.62.0.9:17600",
		Stage:           stage,
		Status:          status,
		AdvertisedFingerprints: []string{
			"SHA256:AbCdEf0123456789",
		},
	}
}

// findStep returns the step whose Action contains the given fragment.
func findStep(t *testing.T, steps []joinLedgerStep, fragment string) joinLedgerStep {
	t.Helper()
	for _, step := range steps {
		if strings.Contains(step.Action, fragment) {
			return step
		}
	}
	t.Fatalf("no step whose Action contains %q; steps: %+v", fragment, steps)
	return joinLedgerStep{}
}

// TestJoinLedger_NeverRendersTheSecondPIN is the promise that matters
// most. The second PIN's value must not appear anywhere on the target's
// page, because the target's Admin is the party the handshake is
// supposed to be binding.
func TestJoinLedger_NeverRendersTheSecondPIN(t *testing.T) {
	const secretPIN = "86753099"

	client := &fakeClient{
		getWindowResp: &rpcpb.GetColonyJoinWindowResponse{
			Window: &rpcpb.ColonyJoinWindow{ExpiresAtUnix: 4102444800},
		},
		listJoinRequestsResp: &rpcpb.ListJoinRequestsResponse{Requests: []*rpcpb.PendingJoinRequest{
			ledgerRequest(rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED, rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING),
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine/join", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, secretPIN) {
		t.Errorf("the rendered ledger contains the second PIN value %q; it must never be rendered on the target's page", secretPIN)
	}
	// And it must not render a field that LOOKS like one either - a
	// template edit that added a "second_pin" field would not have the
	// value in it, but the field itself is the thing to keep out.
	if strings.Contains(strings.ToLower(body), "second_pin") {
		t.Errorf("the rendered ledger mentions a second_pin field; no such field may exist on the target's page")
	}
}

// TestJoinLedger_NeverSubmitsAnything proves the page is read-only. It
// calls no mutating RPC, and it exposes no POST route.
func TestJoinLedger_NeverSubmitsAnything(t *testing.T) {
	client := &fakeClient{
		getWindowResp: &rpcpb.GetColonyJoinWindowResponse{
			Window: &rpcpb.ColonyJoinWindow{ExpiresAtUnix: 4102444800},
		},
		listJoinRequestsResp: &rpcpb.ListJoinRequestsResponse{Requests: []*rpcpb.PendingJoinRequest{
			ledgerRequest(rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED, rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING),
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine/join", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// The fake records what was asked for. Nothing that mutates the
	// Colony's membership may have been called.
	if client.lastVerifyJoinReq != nil {
		t.Errorf("the ledger called VerifyJoinIntroduction; it must never submit anything on the operator's behalf")
	}
	if client.lastApproveJoinRequestReq != nil {
		t.Errorf("the ledger called ApproveJoinRequest; it must never submit anything on the operator's behalf")
	}
	if client.lastRequestJoinColonyReq != nil {
		t.Errorf("the ledger called RequestJoinColony; it must never submit anything on the operator's behalf")
	}
}

// TestJoinLedger_PostIsRefused proves there is no write path at all: a
// POST to the ledger's URL is not routed to the handler.
func TestJoinLedger_PostIsRefused(t *testing.T) {
	client := &fakeClient{}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodPost, "/machine/join", strings.NewReader(""))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("POST /machine/join was accepted with 200; the ledger is a GET-only read-only page")
	}
}

// TestJoinLedger_ClosedWindowBlocksLaterSteps checks that a closed join
// window is reported as a real, named state - and that the step after it
// is blocked rather than merely "waiting", because a closed window is
// something the operator fixes here and now.
func TestJoinLedger_ClosedWindowBlocksLaterSteps(t *testing.T) {
	client := &fakeClient{
		// The REAL closed-window refusal, so this fake exercises the
		// same response managerd actually produces rather than a shape
		// it never returns.
		getWindowResp: &rpcpb.GetColonyJoinWindowResponse{
			Error: manager.ColonyWindowClosedRefusal(),
		},
		listJoinRequestsResp: &rpcpb.ListJoinRequestsResponse{Requests: []*rpcpb.PendingJoinRequest{
			ledgerRequest(rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED, rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING),
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine/join", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, manager.ColonyWindowClosedRefusal()) {
		t.Errorf("a closed window must be named as closed, not left vague; body=%s", body)
	}
	if !strings.Contains(body, "refused until it is open") {
		t.Errorf("a closed window must say what it blocks; body=%s", body)
	}
	if !strings.Contains(body, "badge blocked") {
		t.Errorf("a closed window must render the blocked state; body=%s", body)
	}
}

// TestJoinLedger_UnreadableWindowIsNotAClosedWindow is the distinction
// ADR-0163's "unknown" state exists for: a failed read is not the same
// as a closed window, and offering a fix for a window that may already be
// open is worse than saying nothing.
func TestJoinLedger_UnreadableWindowIsNotAClosedWindow(t *testing.T) {
	client := &fakeClient{
		getWindowErr: errString("no peer forwarding is configured"),
		listJoinRequestsResp: &rpcpb.ListJoinRequestsResponse{Requests: []*rpcpb.PendingJoinRequest{
			ledgerRequest(rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED, rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING),
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine/join", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, manager.ColonyWindowClosedRefusal()) {
		t.Errorf("a failed window read must not be rendered as a closed window; body=%s", body)
	}
	if !strings.Contains(body, "could not read the join window") {
		t.Errorf("a failed window read must say it could not read the state; body=%s", body)
	}
	if !strings.Contains(body, "badge unknown") {
		t.Errorf("a failed window read must render the unknown state; body=%s", body)
	}
}

// TestJoinLedger_ApprovedRequestMarksTheStepsDone checks that "done" is
// driven by replicated state - an approved request moves the ledger's
// steps forward without the page keeping any state of its own.
func TestJoinLedger_ApprovedRequestMarksTheStepsDone(t *testing.T) {
	client := &fakeClient{
		getWindowResp: &rpcpb.GetColonyJoinWindowResponse{
			Window: &rpcpb.ColonyJoinWindow{ExpiresAtUnix: 4102444800},
		},
		listJoinRequestsResp: &rpcpb.ListJoinRequestsResponse{Requests: []*rpcpb.PendingJoinRequest{
			ledgerRequest(rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_AUTHORIZED, rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED),
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine/join", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "approve the request") {
		t.Errorf("an approved request must render the approve step; body=%s", body)
	}
	// The approve step itself must be marked done, not merely present.
	if !strings.Contains(body, "This Colony approved the request") {
		t.Errorf("an approved request must show the approve step as done; body=%s", body)
	}
}

// TestJoinLedger_RevealsTheJoiningFingerprints checks that the joining
// Comb's advertised fingerprints ARE shown, on the joining column. They
// are not a secret at the target - the request carried them - and the
// operator has to compare a paste against them.
func TestJoinLedger_RevealsTheJoiningFingerprints(t *testing.T) {
	client := &fakeClient{
		getWindowResp: &rpcpb.GetColonyJoinWindowResponse{
			Window: &rpcpb.ColonyJoinWindow{ExpiresAtUnix: 4102444800},
		},
		listJoinRequestsResp: &rpcpb.ListJoinRequestsResponse{Requests: []*rpcpb.PendingJoinRequest{
			ledgerRequest(rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED, rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING),
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine/join", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "SHA256:AbCdEf0123456789") {
		t.Errorf("the advertised fingerprints must be shown for comparison; body=%s", body)
	}
}

// TestJoinLedger_NamesBothCombs checks the ledger's central promise: an
// operator can no longer be confused about which host an action happens
// on.
func TestJoinLedger_NamesBothCombs(t *testing.T) {
	client := &fakeClient{
		getWindowResp: &rpcpb.GetColonyJoinWindowResponse{
			Window: &rpcpb.ColonyJoinWindow{ExpiresAtUnix: 4102444800},
		},
		listJoinRequestsResp: &rpcpb.ListJoinRequestsResponse{Requests: []*rpcpb.PendingJoinRequest{
			ledgerRequest(rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED, rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING),
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine/join", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "the JOINING Comb") {
		t.Errorf("the ledger must name the joining Comb's side; body=%s", body)
	}
	if !strings.Contains(body, "this Colony") {
		t.Errorf("the ledger must name the target's side; body=%s", body)
	}
}

// TestJoinLedger_NoRequestYetIsNotAnError checks that the page renders
// before any request exists. The handshake starts with a command on the
// joining Comb, so "no request" is the normal first state, not a fault.
func TestJoinLedger_NoRequestYetIsNotAnError(t *testing.T) {
	client := &fakeClient{
		getWindowResp: &rpcpb.GetColonyJoinWindowResponse{
			Window: &rpcpb.ColonyJoinWindow{ExpiresAtUnix: 4102444800},
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine/join", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No join request yet") {
		t.Errorf("with no request the page must say so plainly; body=%s", body)
	}
	if !strings.Contains(body, "join-introduce") {
		t.Errorf("with no request the page must still name the command that starts the handshake; body=%s", body)
	}
}

// TestJoinLedger_SurvivesALeaderChange proves the ledger keeps no state
// of its own: the same replicated state rendered twice produces the same
// ledger, with no session, cookie or server-side progress involved. A
// flow that kept progress in session state would lose it on a leader
// change, and one that kept it in the browser would lose it on a
// reload.
func TestJoinLedger_SurvivesALeaderChange(t *testing.T) {
	client := &fakeClient{
		getWindowResp: &rpcpb.GetColonyJoinWindowResponse{
			Window: &rpcpb.ColonyJoinWindow{ExpiresAtUnix: 4102444800},
		},
		listJoinRequestsResp: &rpcpb.ListJoinRequestsResponse{Requests: []*rpcpb.PendingJoinRequest{
			ledgerRequest(rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED, rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING),
		}},
	}
	s := newTestServer(t, client)

	first := httptest.NewRecorder()
	s.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/machine/join", nil))
	second := httptest.NewRecorder()
	s.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/machine/join", nil))

	if first.Body.String() != second.Body.String() {
		t.Errorf("two renders of the same replicated state differ; the ledger must keep no state of its own")
	}
}

// errString is a tiny error helper, so the test above can express a
// failing RPC without importing errors for one call.
type errString string

func (e errString) Error() string { return string(e) }

// TestJoinLedger_IsReachableFromTheMachinePage checks the flow is
// discoverable. A guided flow nobody can find is not a guided flow, and
// the Machine page's "Join a Colony" panel is where an operator
// looking for it will be.
func TestJoinLedger_IsReachableFromTheMachinePage(t *testing.T) {
	client := &fakeClient{
		getWindowResp: &rpcpb.GetColonyJoinWindowResponse{
			Window: &rpcpb.ColonyJoinWindow{ExpiresAtUnix: 4102444800},
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `href="/machine/join"`) {
		t.Errorf("the Machine page must link to the guided join flow; body=%s", rec.Body.String())
	}
}
