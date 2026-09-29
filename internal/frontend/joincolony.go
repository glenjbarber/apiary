// The mutually-authorized Colony-join flow (ADR-0083): the Machine
// page's "Join a Colony" section is the joining Comb's own side
// (request, then poll for approval); the default landing page's
// "Pending join requests" panel is the existing Colony's side (Admin
// reviews the code, approves/rejects).
package frontend

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

// joinRequestView is the template-facing shape of a PendingJoinRequest.
type joinRequestView struct {
	RequestID       string
	NodeID          string
	RaftBindAddress string
	// Code is the FIRST CODE - the REQUESTING Comb's own 6-digit value
	// (ADR-0147 Part 2), and on the target's panel it is what the
	// operator compares a paste against. It is empty on a request that
	// has already been through stage one, because the code is CLEARED
	// from replicated state the moment it is accepted: a spent value is
	// not left lying around for a later reader.
	Code        string
	RequestedAt string
	Status      string
	Stage       string
	Error       string

	// Fingerprints (ADR-0147 Part 2) is every certificate the joining
	// Comb advertised, in the hostcert.Fingerprint format. Rendered as
	// the comparison list next to the first-code form, and rendered
	// ALWAYS - including when it is empty, which is a refusal state
	// rather than a "nothing to see here".
	//
	// The pre-ADR-0147 panel rendered an explicit "no TLS certificate
	// presented" fallback and let approval proceed on it. That fallback
	// was the defect: with nothing to compare, the fingerprint check -
	// the only thing binding a request to a machine - was vacuous.
	Fingerprints []string

	// FirstCodeAttempts / SecondPinAttempts are the replicated
	// per-stage counters, shown so an operator about to use the last of
	// five knows it is the last.
	FirstCodeAttempts uint32
	SecondPinAttempts uint32
	AttemptsMax       uint32

	// SecondPinExpires is when the target's second PIN lapses, as
	// ready-to-render text, or "" when there is no PIN. The VALUE is
	// never on this struct: the second PIN is released to the
	// requesting Comb and only there, and a field here would be one
	// template edit away from rendering it on the page an operator who
	// could complete the handshake alone reads.
	SecondPinExpires string

	// SecondPinReissues is how many of the two permitted re-arms have
	// been used.
	SecondPinReissues uint32

	// TLSFingerprint (ADR-0113) is the joining Comb's own TLS certificate
	// fingerprint, or "" when it presented none (TLS remains opt-in) -
	// the landing page's Approve form always renders this value (or an
	// explicit "no TLS certificate presented" fallback) directly next to
	// the confirm_phrase field, so an Admin cannot reach that field
	// without having seen it.
	TLSFingerprint string

	// JoinerLogState is the joining Comb's own reported raft log state, as
	// ready-to-render text. It is shown in the pending list rather than
	// only in the preflight result because it is the one piece of evidence
	// an Admin needs BEFORE clicking anything: a joiner carrying a
	// non-empty log cannot be approved at all, and finding that out by
	// typing the confirmation phrase is a poor way to learn it.
	//
	// "not reported" is deliberately its own state and not folded into
	// "empty". A request with no evidence is refused at approval, and
	// rendering it as though it were safe would be the UI version of the
	// same fail-open mistake the guardrail exists to prevent.
	JoinerLogState string

	// TargetAddress (ADR-0092) is NOT part of the underlying
	// PendingJoinRequest record - it's the existing Colony member's
	// address this request was forwarded to, carried in the page's own
	// query string so a later Refresh/Cancel action on this same
	// request knows where to reach it again.
	TargetAddress string
}

// joinerLogStateText renders the join-log evidence for the pending list.
func joinerLogStateText(observed bool, lastIndex uint64) string {
	switch {
	case !observed:
		return "not reported - approval will be refused until this Comb re-requests with evidence"
	case lastIndex == 0:
		return "empty (index 0) - safe to approve"
	default:
		return fmt.Sprintf("NOT empty (index %d) - this Comb carries a previous log and cannot be approved until it is wiped", lastIndex)
	}
}

func fromRPCJoinRequest(r *rpcpb.PendingJoinRequest) joinRequestView {
	status := "pending"
	switch r.GetStatus() {
	case rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED:
		status = "approved"
	case rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_REJECTED:
		status = "rejected"
	case rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_CANCELLED:
		status = "cancelled"
	case rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_FAILED:
		// ADR-0147 Part 2. FAILED is TERMINAL, like approved and
		// rejected: the record stays so the joiner's own poll resolves,
		// and Purge is how it goes away. It is deliberately not
		// rendered as "pending with 5 attempts used", because a request
		// something has been guessing at is not one more paste away
		// from being approved.
		status = "failed"
	}
	stage := "introduced"
	switch r.GetStage() {
	case rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED:
		stage = "code verified - the second PIN is waiting to be read off the requesting Comb"
	case rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_AUTHORIZED:
		stage = "authorized"
	}
	view := joinRequestView{
		RequestID:         r.GetRequestId(),
		NodeID:            r.GetNodeId(),
		RaftBindAddress:   r.GetRaftBindAddress(),
		Code:              r.GetCode(),
		RequestedAt:       time.Unix(r.GetRequestedAtUnix(), 0).Local().Format("2006-01-02 15:04 MST"),
		Status:            status,
		Stage:             stage,
		Fingerprints:      r.GetAdvertisedFingerprints(),
		FirstCodeAttempts: r.GetFirstCodeAttempts(),
		SecondPinAttempts: r.GetSecondPinAttempts(),
		AttemptsMax:       raftnode.MaxJoinStageAttempts,
		SecondPinReissues: r.GetSecondPinReissues(),
		TLSFingerprint:    r.GetTlsCertFingerprint(),
		JoinerLogState:    joinerLogStateText(r.GetJoinerLogStateObserved(), r.GetJoinerLastLogIndex()),
	}
	if r.GetSecondPinExpiresAtUnix() > 0 && r.GetStage() == rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED {
		view.SecondPinExpires = time.Unix(r.GetSecondPinExpiresAtUnix(), 0).Local().Format("15:04:05 MST")
	}
	return view
}

// currentJoinRequests fetches the Admin-only pending-request list for
// the landing page's panel - a PermissionDenied here (a non-Admin
// session) is expected, not a real page error, so it's silently
// treated as "nothing to show" rather than surfaced.
func (s *Server) currentJoinRequests(r *http.Request) []joinRequestView {
	resp, err := s.client.ListJoinRequests(r.Context(), &rpcpb.ListJoinRequestsRequest{})
	if err != nil || resp.GetError() != "" {
		return nil
	}
	views := make([]joinRequestView, 0, len(resp.GetRequests()))
	for _, req := range resp.GetRequests() {
		views = append(views, fromRPCJoinRequest(req))
	}
	return views
}

// handleRequestJoinColony implements the joining Comb's side of
// ADR-0083 - POST /machine/join-colony, Admin-only (joining a Colony
// changes this Comb's own cluster identity, the same consequence tier
// as UpdateNodeConfig). Redirects back to the Machine page with the
// new request's id AND the target address in the query string, so a
// page reload keeps polling the same request (via the same target,
// ADR-0092) rather than starting a new one or failing "not found"
// against this node's own unrelated local raft.
//
// target_address is required here (not just at the RPC layer, which
// keeps it optional for direct callers/tests): submitting this form
// without one is exactly the confusing trap ADR-0092 closes - it would
// silently record a request on THIS Comb's own Colony instead of the
// one actually being joined.
func (s *Server) handleRequestJoinColony(w http.ResponseWriter, r *http.Request) {
	s.renderMachinePageWithJoinColonyError(w, r, retiredJoinFormMessage)
}

// retiredJoinFormMessage is what the retired form says, in the place an
// operator who has a stale browser tab or a bookmark will actually see
// it. It names the command and says why the page is gone, because
// "this form no longer works" with no alternative is how an operator
// concludes the whole feature was removed.
const retiredJoinFormMessage = "This page no longer starts a Colony join. ADR-0147 Part 2 requires the first code to be generated by the JOINING Comb and read by the target's Admin off the joiner's own screen, which is something a form on the joiner cannot honestly do - it would satisfy the check without a person. Run `apiaryctl join-introduce --target HOST:PORT` on this Comb instead; it prints the first code and this Comb's certificate fingerprints for the target's Admin to paste, and it submits nothing on your behalf."

// handleVerifyJoinIntroduction implements the TARGET's side of stage
// one - POST /join-requests/{id}/verify, Admin-only, same tier as
// Approve/Reject/Purge.
//
// It forwards exactly what the operator pasted and derives nothing: the
// first code and every fingerprint go on the wire as typed, and the
// comparison happens in the FSM against what the request actually
// carried. A page that computed a verdict itself would be a second
// implementation of the one check that matters, and the two would
// eventually disagree.
//
// The response does not carry, and this handler does not render, the
// second PIN. The PIN goes to the requesting Comb and only there.
func (s *Server) handleVerifyJoinRequest(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectAfterJoinRequestAction(w, r, err.Error(), nil)
		return
	}
	fingerprints := fingerprintListFromForm(r.FormValue("fingerprints"))
	resp, err := s.client.VerifyJoinIntroduction(r.Context(), &rpcpb.VerifyJoinIntroductionRequest{
		RequestId:        r.PathValue("id"),
		IntroductionCode: strings.TrimSpace(r.FormValue("introduction_code")),
		Fingerprints:     fingerprints,
	})
	s.redirectAfterJoinRequestAction(w, r, resp.GetError(), err)
}

// fingerprintListFromForm splits a pasted fingerprint block into one
// value per line, dropping blanks.
//
// Line-separated rather than comma-separated because that is what
// `apiaryctl join-introduce --field fingerprints` prints and what a
// terminal selection pastes. Whitespace inside a line is left alone:
// a fingerprint is a fixed-format string, so silently "tidying" one
// would mean the operator is no longer comparing what they read.
func fingerprintListFromForm(raw string) []string {
	out := []string{}
	for _, line := range strings.Split(raw, "\n") {
		if v := strings.TrimSpace(line); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// handleReissueJoinSecondPin re-arms an expired or spent second PIN -
// POST /join-requests/{id}/reissue-pin, Admin-only.
//
// It is a separate button rather than something the verify form does
// automatically because re-arming invalidates a PIN the requesting
// Comb may be reading right now, and because it is capped at 2. An
// operator should have to say so.
func (s *Server) handleReissueJoinSecondPin(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.ReissueJoinSecondPin(r.Context(), &rpcpb.ReissueJoinSecondPinRequest{RequestId: r.PathValue("id")})
	s.redirectAfterJoinRequestAction(w, r, resp.GetError(), err)
}

func (s *Server) renderMachinePageWithJoinColonyError(w http.ResponseWriter, r *http.Request, formErr string) {
	nodeID := s.localNodeID(r)
	cfg, cfgErr := s.currentNodeConfig(r)
	vms, vmErr := s.currentMachineVMs(r, nodeID)
	cloudflareConfigured, _ := s.currentCloudflareStatus(r)
	services, serviceErr := s.currentNodeServices(r)
	originCerts, originErr := s.currentOriginCertificates(r)
	s.render(w, "machine_page", s.withAuthFields(r, pageData{
		NodeConfig: cfg, NodeConfigFormError: cfgErr, MachineVMs: vms, MachineFirewallError: vmErr,
		CloudflareConfigured: cloudflareConfigured, NodeServices: services, ServiceFormError: serviceErr,
		OriginCertificates: originCerts, OriginCAError: originErr,
		JoinColonyFormError: formErr, ActivePage: "machine",
	}))
}

// currentJoinColonyResult reads ?join_request_id=/?join_target_address=
// (set by handleRequestJoinColony's own redirect, ADR-0092) and polls
// the request's current status via that same target - GetJoinRequestStatus
// needs no credential, matching RequestJoinColony itself, since this is
// this Comb's own outstanding request.
func (s *Server) currentJoinColonyResult(r *http.Request) *joinRequestView {
	requestID := r.URL.Query().Get("join_request_id")
	if requestID == "" {
		return nil
	}
	targetAddress := r.URL.Query().Get("join_target_address")
	resp, err := s.client.GetJoinRequestStatus(r.Context(), &rpcpb.GetJoinRequestStatusRequest{RequestId: requestID, TargetAddress: targetAddress})
	if err != nil {
		return &joinRequestView{RequestID: requestID, TargetAddress: targetAddress, Error: err.Error()}
	}
	if resp.GetError() != "" {
		return &joinRequestView{RequestID: requestID, TargetAddress: targetAddress, Error: resp.GetError()}
	}
	view := fromRPCJoinRequest(resp.GetRequest())
	view.TargetAddress = targetAddress
	return &view
}

// handleApproveJoinRequest/handleRejectJoinRequest implement the
// existing Colony's side of ADR-0083 - both Admin-only, both simply
// redirect back to the landing page afterward so the panel reflects the
// new state (or the failure, surfaced via ?join_request_error=).
//
// handleApproveJoinRequest additionally requires confirm_phrase (ADR-0113)
// on the submitted form - the landing page's Approve form always renders
// the pending request's tls_cert_fingerprint (or its absence) right next
// to this field, so an Admin cannot reach Approve's own guardrail without
// having seen it first. A missing or wrong phrase is rejected by
// ApproveJoinRequest itself before any raft/AddVoter action is attempted;
// this handler does not re-check it, it just forwards whatever the form
// carried and lets the RPC's own fail-closed behavior do the work.
func (s *Server) handleApproveJoinRequest(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectAfterJoinRequestAction(w, r, "", err)
		return
	}
	// second_pin is the 8-digit value the TARGET generated, which the
	// operator read off the REQUESTING Comb's own screen. It travels
	// with the confirmation phrase and is checked by the same call; the
	// phrase alone is a constant anyone can read out of this repository,
	// which is why Part 2 kept it but stopped letting it be the whole
	// of the gate.
	resp, err := s.client.ApproveJoinRequest(r.Context(), &rpcpb.ApproveJoinRequestRequest{
		RequestId:     r.PathValue("id"),
		ConfirmPhrase: r.FormValue("confirm_phrase"),
		SecondPin:     strings.TrimSpace(r.FormValue("second_pin")),
	})
	s.redirectAfterJoinRequestAction(w, r, resp.GetError(), err)
}

func (s *Server) handleRejectJoinRequest(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.RejectJoinRequest(r.Context(), &rpcpb.RejectJoinRequestRequest{RequestId: r.PathValue("id")})
	s.redirectAfterJoinRequestAction(w, r, resp.GetError(), err)
}

// handlePreflightJoinRequest previews ApproveJoinRequest's own
// reachability gate without ever approving anything (ADR-0103) - Admin-
// only, matching Approve/Reject/Purge's own tier exactly, since it makes
// managerd dial a caller-selected address. Redirects back to the landing
// page carrying the verdict/detail in the query string, the same
// pattern (if more fully wired up for actual display) as
// redirectAfterJoinRequestAction's own ?join_request_error=. Never gates
// or disables the real Approve button - the operator checks themselves.
func (s *Server) handlePreflightJoinRequest(w http.ResponseWriter, r *http.Request) {
	requestID := r.PathValue("id")
	resp, err := s.client.PreflightApproveJoinRequest(r.Context(), &rpcpb.PreflightApproveJoinRequestRequest{RequestId: requestID})
	verdict := "unknown"
	detail := ""
	if err != nil {
		detail = err.Error()
	} else if resp.GetError() != "" {
		detail = resp.GetError()
	} else {
		verdict = resp.GetVerdict()
		if len(resp.GetFindings()) > 0 {
			detail = resp.GetFindings()[0].GetDetail()
		}
	}
	http.Redirect(w, r, "/?preflight_request_id="+url.QueryEscape(requestID)+
		"&preflight_verdict="+url.QueryEscape(verdict)+
		"&preflight_detail="+url.QueryEscape(detail), http.StatusFound)
}

// handlePurgeJoinRequest implements the existing Colony's other
// available action on a request - Admin-only, same redirect-back
// pattern as Approve/Reject, but deletes the record outright rather
// than marking it (see PurgeJoinRequest's own doc comment).
func (s *Server) handlePurgeJoinRequest(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.PurgeJoinRequest(r.Context(), &rpcpb.PurgeJoinRequestRequest{RequestId: r.PathValue("id")})
	s.redirectAfterJoinRequestAction(w, r, resp.GetError(), err)
}

// handleCancelJoinRequest implements the joining Comb's own side -
// POST /machine/join-colony/cancel, Admin-gated like every other
// Machine page action. Redirects back to a bare /machine (no
// ?join_request_id=) either way, so the "Request to join" form shows
// again rather than continuing to poll a request that no longer needs
// it - a cancel failure is rare enough (the request would have to have
// already been resolved or expired) that showing the same fresh form
// is a reasonable outcome for that case too, not just the success one.
func (s *Server) handleCancelJoinRequest(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/machine", http.StatusFound)
		return
	}
	s.client.CancelJoinRequest(r.Context(), &rpcpb.CancelJoinRequestRequest{
		RequestId:     r.FormValue("request_id"),
		TargetAddress: r.FormValue("target_address"),
	})
	http.Redirect(w, r, "/machine", http.StatusFound)
}

func (s *Server) redirectAfterJoinRequestAction(w http.ResponseWriter, r *http.Request, rpcErr string, err error) {
	if err != nil {
		http.Redirect(w, r, "/?join_request_error="+url.QueryEscape(err.Error()), http.StatusFound)
		return
	}
	if rpcErr != "" {
		http.Redirect(w, r, "/?join_request_error="+url.QueryEscape(rpcErr), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}
