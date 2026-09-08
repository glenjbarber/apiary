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
}

func fromRPCJoinRequest(r *rpcpb.PendingJoinRequest) joinRequestView {
	status := "pending"
	switch r.GetStatus() {
	case rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED:
		status = "approved"
	case rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_REJECTED:
		status = "rejected"
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
// new request's id in the query string so a page reload keeps polling
// the same request rather than starting a new one.
func (s *Server) handleRequestJoinColony(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderMachinePageWithJoinColonyError(w, r, err.Error())
		return
	}
	resp, err := s.client.RequestJoinColony(r.Context(), &rpcpb.RequestJoinColonyRequest{
		NodeId:          r.FormValue("node_id"),
		RaftBindAddress: r.FormValue("raft_bind_address"),
	})
	if err != nil {
		s.renderMachinePageWithJoinColonyError(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderMachinePageWithJoinColonyError(w, r, resp.GetError())
		return
	}
	http.Redirect(w, r, "/machine?join_request_id="+url.QueryEscape(resp.GetRequestId()), http.StatusFound)
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

// currentJoinColonyResult reads ?join_request_id= (set by
// handleRequestJoinColony's own redirect) and polls its current status
// - GetJoinRequestStatus needs no credential, matching RequestJoinColony
// itself, since this is this Comb's own outstanding request.
func (s *Server) currentJoinColonyResult(r *http.Request) *joinRequestView {
	requestID := r.URL.Query().Get("join_request_id")
	if requestID == "" {
		return nil
	}
	resp, err := s.client.GetJoinRequestStatus(r.Context(), &rpcpb.GetJoinRequestStatusRequest{RequestId: requestID})
	if err != nil {
		return &joinRequestView{RequestID: requestID, Error: err.Error()}
	}
	if resp.GetError() != "" {
		return &joinRequestView{RequestID: requestID, Error: resp.GetError()}
	}
	view := fromRPCJoinRequest(resp.GetRequest())
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
