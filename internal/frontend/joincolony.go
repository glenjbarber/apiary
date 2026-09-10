// The mutually-authorized Colony-join flow (ADR-0083): the Machine
// page's "Join a Colony" section is the joining Comb's own side
// (request, then poll for approval); the default landing page's
// "Pending join requests" panel is the existing Colony's side (Admin
// reviews the code, approves/rejects).
package frontend

import (
	"net/http"
	"net/url"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// joinRequestView is the template-facing shape of a PendingJoinRequest.
type joinRequestView struct {
	RequestID       string
	NodeID          string
	RaftBindAddress string
	Code            string
	RequestedAt     string
	Status          string
	Error           string

	// TargetAddress (ADR-0092) is NOT part of the underlying
	// PendingJoinRequest record - it's the existing Colony member's
	// address this request was forwarded to, carried in the page's own
	// query string so a later Refresh/Cancel action on this same
	// request knows where to reach it again.
	TargetAddress string
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
	}
	return joinRequestView{
		RequestID:       r.GetRequestId(),
		NodeID:          r.GetNodeId(),
		RaftBindAddress: r.GetRaftBindAddress(),
		Code:            r.GetCode(),
		RequestedAt:     time.Unix(r.GetRequestedAtUnix(), 0).Local().Format("2006-01-02 15:04 MST"),
		Status:          status,
	}
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
	if err := r.ParseForm(); err != nil {
		s.renderMachinePageWithJoinColonyError(w, r, err.Error())
		return
	}
	targetAddress := r.FormValue("target_address")
	if targetAddress == "" {
		s.renderMachinePageWithJoinColonyError(w, r, "the existing Colony member's address is required")
		return
	}
	resp, err := s.client.RequestJoinColony(r.Context(), &rpcpb.RequestJoinColonyRequest{
		NodeId:          r.FormValue("node_id"),
		RaftBindAddress: r.FormValue("raft_bind_address"),
		TargetAddress:   targetAddress,
	})
	if err != nil {
		s.renderMachinePageWithJoinColonyError(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderMachinePageWithJoinColonyError(w, r, resp.GetError())
		return
	}
	http.Redirect(w, r, "/machine?join_request_id="+url.QueryEscape(resp.GetRequestId())+"&join_target_address="+url.QueryEscape(targetAddress), http.StatusFound)
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
func (s *Server) handleApproveJoinRequest(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.ApproveJoinRequest(r.Context(), &rpcpb.ApproveJoinRequestRequest{RequestId: r.PathValue("id")})
	s.redirectAfterJoinRequestAction(w, r, resp.GetError(), err)
}

func (s *Server) handleRejectJoinRequest(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.RejectJoinRequest(r.Context(), &rpcpb.RejectJoinRequestRequest{RequestId: r.PathValue("id")})
	s.redirectAfterJoinRequestAction(w, r, resp.GetError(), err)
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
