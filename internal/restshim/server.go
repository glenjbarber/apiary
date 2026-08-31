package restshim

import (
	"context"
	"encoding/json"
	"net/http"

	"google.golang.org/grpc/metadata"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// Server translates HTTP/JSON requests into calls against a
// rpcpb.ManagerServiceClient. It implements http.Handler directly (via
// an internal ServeMux) so it can be mounted with http.ListenAndServe or
// embedded under another handler.
type Server struct {
	client rpcpb.ManagerServiceClient
	mux    *http.ServeMux
}

// NewServer returns a Server that answers REST requests using client.
// rpcpb.ManagerServiceClient is the same interface managerd's own gRPC
// client satisfies - restshim is just another client of ManagerService,
// dialed the same way any other gRPC client would be.
func NewServer(client rpcpb.ManagerServiceClient) *Server {
	s := &Server{client: client, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// authContext forwards the caller's own "Authorization" HTTP header
// through to managerd as gRPC metadata (ADR-0023 gates every managerd
// RPC on exactly this metadata key). Unlike cmd/frontend - a single
// application-level identity that attaches one static
// APIARY_MANAGER_API_KEY at dial time - restshim has no identity of
// its own: it's meant to sit in front of external tooling (curl,
// Terraform, CI) where each caller presents their own key, so
// forwarding per-request is the only correct behavior here. A caller
// with no header attaches nothing, which behaves exactly like an
// unauthenticated call today - unchanged if managerd has no keys yet.
func authContext(r *http.Request) context.Context {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return r.Context()
	}
	return metadata.AppendToOutgoingContext(r.Context(), "authorization", auth)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /v1/status", s.handleStatus)
	s.mux.HandleFunc("POST /v1/vms", s.handleCreateVM)
	s.mux.HandleFunc("GET /v1/vms", s.handleListVMs)
	s.mux.HandleFunc("GET /v1/vms/{id}", s.handleGetVM)
	s.mux.HandleFunc("PUT /v1/vms/{id}", s.handleUpdateVM)
	s.mux.HandleFunc("DELETE /v1/vms/{id}", s.handleDeleteVM)
}

// errorBody is the JSON shape returned for any non-2xx response.
type errorBody struct {
	Error      string `json:"error"`
	LeaderHint string `json:"leader_hint,omitempty"`
}

// writeError picks a status code from (rpcErr, appErr, leaderHint):
//   - rpcErr (a transport/gRPC-level failure calling managerd) -> 502
//   - appErr with a leaderHint (this managerd's raftd isn't the leader) -> 503
//   - appErr alone (an application-level rejection, e.g. duplicate/missing
//     VM id) -> 400
//
// This is a deliberate v1 simplification: application-level errors are
// plain strings from the RPC layer, not typed codes, so a "missing id"
// rejection can't be distinguished from other rejections without
// fragile string-matching on the message - a real error-code scheme is
// a separate future improvement, not something to fake by guessing at
// error text.
func writeError(w http.ResponseWriter, rpcErr error, appErr, leaderHint string) {
	switch {
	case rpcErr != nil:
		writeJSON(w, http.StatusBadGateway, errorBody{Error: rpcErr.Error()})
	case leaderHint != "":
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: appErr, LeaderHint: leaderHint})
	default:
		writeJSON(w, http.StatusBadRequest, errorBody{Error: appErr})
	}
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.Status(authContext(r), &rpcpb.StatusRequest{})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"manager_node_id":     resp.GetManagerNodeId(),
		"raft_reachable":      resp.GetRaftReachable(),
		"raft_error":          resp.GetRaftError(),
		"raft_is_leader":      resp.GetRaftIsLeader(),
		"raft_leader_id":      resp.GetRaftLeaderId(),
		"raft_node_id":        resp.GetRaftNodeId(),
		"raft_last_log_index": resp.GetRaftLastLogIndex(),
		"raft_applied_index":  resp.GetRaftAppliedIndex(),
		"raft_state":          resp.GetRaftState(),
	})
}

func (s *Server) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	var body vm
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON body: " + err.Error()})
		return
	}

	resp, err := s.client.CreateVM(authContext(r), &rpcpb.CreateVMRequest{Vm: toRPCVM(body)})
	if err != nil || resp.GetError() != "" {
		writeError(w, err, resp.GetError(), resp.GetLeaderHint())
		return
	}
	writeJSON(w, http.StatusCreated, fromRPCVM(resp.GetVm()))
}

func (s *Server) handleUpdateVM(w http.ResponseWriter, r *http.Request) {
	var body vm
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON body: " + err.Error()})
		return
	}
	body.ID = r.PathValue("id")

	resp, err := s.client.UpdateVM(authContext(r), &rpcpb.UpdateVMRequest{Vm: toRPCVM(body)})
	if err != nil || resp.GetError() != "" {
		writeError(w, err, resp.GetError(), resp.GetLeaderHint())
		return
	}
	writeJSON(w, http.StatusOK, fromRPCVM(resp.GetVm()))
}

func (s *Server) handleDeleteVM(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.DeleteVM(authContext(r), &rpcpb.DeleteVMRequest{Id: r.PathValue("id")})
	if err != nil || resp.GetError() != "" {
		writeError(w, err, resp.GetError(), resp.GetLeaderHint())
		return
	}
	writeJSON(w, http.StatusOK, fromRPCVM(resp.GetVm()))
}

func (s *Server) handleGetVM(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.GetVM(authContext(r), &rpcpb.GetVMRequest{Id: r.PathValue("id")})
	if err != nil || resp.GetError() != "" {
		writeError(w, err, resp.GetError(), resp.GetLeaderHint())
		return
	}
	if !resp.GetFound() {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "VM not found"})
		return
	}
	writeJSON(w, http.StatusOK, fromRPCVM(resp.GetVm()))
}

func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.ListVMs(authContext(r), &rpcpb.ListVMsRequest{})
	if err != nil || resp.GetError() != "" {
		writeError(w, err, resp.GetError(), resp.GetLeaderHint())
		return
	}

	vms := make([]vm, 0, len(resp.GetVms()))
	for _, d := range resp.GetVms() {
		vms = append(vms, fromRPCVM(d))
	}
	writeJSON(w, http.StatusOK, vms)
}
