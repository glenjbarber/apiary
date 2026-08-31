package restshim

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

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
	s.mux.HandleFunc("POST /v1/vms/{id}/migrate", s.handleMigrateVM)

	s.mux.HandleFunc("POST /v1/jails", s.handleCreateJail)
	s.mux.HandleFunc("GET /v1/jails", s.handleListJails)
	s.mux.HandleFunc("GET /v1/jails/{id}", s.handleGetJail)
	s.mux.HandleFunc("PUT /v1/jails/{id}", s.handleUpdateJail)
	s.mux.HandleFunc("DELETE /v1/jails/{id}", s.handleDeleteJail)
	s.mux.HandleFunc("POST /v1/jails/{id}/migrate", s.handleMigrateJail)

	// NetworkDefinition has no external Update/Get-by-id RPC (see
	// api/rpc/manager.proto's ManagerService - only Create/List/Delete
	// exist), so there's no PUT or GET /v1/networks/{id} to add here.
	s.mux.HandleFunc("POST /v1/networks", s.handleCreateNetwork)
	s.mux.HandleFunc("GET /v1/networks", s.handleListNetworks)
	s.mux.HandleFunc("DELETE /v1/networks/{id}", s.handleDeleteNetwork)

	// UploadISO (ADR-0017) previously had no REST equivalent at all -
	// only ManagerService's own client-streaming gRPC and the web UI's
	// multipart form could reach it. A REST-only client (e.g. a
	// Terraform or Cluster API provider - see ADR-0031) needs a way to
	// upload an ISO/base image too, so this mirrors internal/frontend's
	// own handleUploadISO multipart-to-gRPC-stream relay exactly.
	s.mux.HandleFunc("POST /v1/isos", s.handleUploadISO)
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

// migrateRequestBody is the JSON body for both migrate endpoints.
type migrateRequestBody struct {
	TargetNodeID string `json:"target_node_id"`
}

func (s *Server) handleMigrateVM(w http.ResponseWriter, r *http.Request) {
	var body migrateRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON body: " + err.Error()})
		return
	}

	resp, err := s.client.MigrateVM(authContext(r), &rpcpb.MigrateVMRequest{
		Id: r.PathValue("id"), TargetNodeId: body.TargetNodeID,
	})
	if err != nil || resp.GetError() != "" {
		writeError(w, err, resp.GetError(), resp.GetLeaderHint())
		return
	}
	writeJSON(w, http.StatusOK, fromRPCVM(resp.GetVm()))
}

func (s *Server) handleMigrateJail(w http.ResponseWriter, r *http.Request) {
	var body migrateRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON body: " + err.Error()})
		return
	}

	resp, err := s.client.MigrateJail(authContext(r), &rpcpb.MigrateJailRequest{
		Id: r.PathValue("id"), TargetNodeId: body.TargetNodeID,
	})
	if err != nil || resp.GetError() != "" {
		writeError(w, err, resp.GetError(), resp.GetLeaderHint())
		return
	}
	writeJSON(w, http.StatusOK, fromRPCJail(resp.GetJail()))
}

func (s *Server) handleCreateJail(w http.ResponseWriter, r *http.Request) {
	var body jail
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON body: " + err.Error()})
		return
	}

	resp, err := s.client.CreateJail(authContext(r), &rpcpb.CreateJailRequest{Jail: toRPCJail(body)})
	if err != nil || resp.GetError() != "" {
		writeError(w, err, resp.GetError(), resp.GetLeaderHint())
		return
	}
	writeJSON(w, http.StatusCreated, fromRPCJail(resp.GetJail()))
}

func (s *Server) handleUpdateJail(w http.ResponseWriter, r *http.Request) {
	var body jail
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON body: " + err.Error()})
		return
	}
	body.ID = r.PathValue("id")

	resp, err := s.client.UpdateJail(authContext(r), &rpcpb.UpdateJailRequest{Jail: toRPCJail(body)})
	if err != nil || resp.GetError() != "" {
		writeError(w, err, resp.GetError(), resp.GetLeaderHint())
		return
	}
	writeJSON(w, http.StatusOK, fromRPCJail(resp.GetJail()))
}

func (s *Server) handleDeleteJail(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.DeleteJail(authContext(r), &rpcpb.DeleteJailRequest{Id: r.PathValue("id")})
	if err != nil || resp.GetError() != "" {
		writeError(w, err, resp.GetError(), resp.GetLeaderHint())
		return
	}
	writeJSON(w, http.StatusOK, fromRPCJail(resp.GetJail()))
}

func (s *Server) handleGetJail(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.GetJail(authContext(r), &rpcpb.GetJailRequest{Id: r.PathValue("id")})
	if err != nil || resp.GetError() != "" {
		writeError(w, err, resp.GetError(), resp.GetLeaderHint())
		return
	}
	if !resp.GetFound() {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "jail not found"})
		return
	}
	writeJSON(w, http.StatusOK, fromRPCJail(resp.GetJail()))
}

func (s *Server) handleListJails(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.ListJails(authContext(r), &rpcpb.ListJailsRequest{})
	if err != nil || resp.GetError() != "" {
		writeError(w, err, resp.GetError(), resp.GetLeaderHint())
		return
	}

	jails := make([]jail, 0, len(resp.GetJails()))
	for _, d := range resp.GetJails() {
		jails = append(jails, fromRPCJail(d))
	}
	writeJSON(w, http.StatusOK, jails)
}

func (s *Server) handleCreateNetwork(w http.ResponseWriter, r *http.Request) {
	var body network
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON body: " + err.Error()})
		return
	}

	resp, err := s.client.CreateNetwork(authContext(r), &rpcpb.CreateNetworkRequest{Network: toRPCNetwork(body)})
	if err != nil || resp.GetError() != "" {
		writeError(w, err, resp.GetError(), resp.GetLeaderHint())
		return
	}
	writeJSON(w, http.StatusCreated, fromRPCNetwork(resp.GetNetwork()))
}

func (s *Server) handleDeleteNetwork(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.DeleteNetwork(authContext(r), &rpcpb.DeleteNetworkRequest{Id: r.PathValue("id")})
	if err != nil || resp.GetError() != "" {
		writeError(w, err, resp.GetError(), resp.GetLeaderHint())
		return
	}
	writeJSON(w, http.StatusOK, fromRPCNetwork(resp.GetNetwork()))
}

func (s *Server) handleListNetworks(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.ListNetworks(authContext(r), &rpcpb.ListNetworksRequest{})
	if err != nil || resp.GetError() != "" {
		writeError(w, err, resp.GetError(), resp.GetLeaderHint())
		return
	}

	networks := make([]network, 0, len(resp.GetNetworks()))
	for _, d := range resp.GetNetworks() {
		networks = append(networks, fromRPCNetwork(d))
	}
	writeJSON(w, http.StatusOK, networks)
}

// isoUploadResult is the REST-facing JSON shape for a successful
// UploadISO call.
type isoUploadResult struct {
	Name string `json:"name"`
}

// handleUploadISO streams a multipart file upload directly into
// managerd's UploadISO RPC, chunk by chunk - mirrors
// internal/frontend's own handleUploadISO exactly (same field-order
// requirement: expected_sha256 must arrive before file, since
// MultipartReader processes parts strictly in send order and the
// Metadata message needs the hash already known by the time the file
// part's bytes start arriving).
func (s *Server) handleUploadISO(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid upload: " + err.Error()})
		return
	}

	var expectedHash string
	var result *rpcpb.UploadISOResponse
	var uploadErr error

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			uploadErr = fmt.Errorf("reading upload: %w", err)
			break
		}

		switch part.FormName() {
		case "expected_sha256":
			data, _ := io.ReadAll(part)
			expectedHash = strings.TrimSpace(string(data))
		case "file":
			result, uploadErr = s.uploadISOStream(r, part, expectedHash)
		}
		part.Close()
		if uploadErr != nil {
			break
		}
	}

	switch {
	case uploadErr != nil:
		writeJSON(w, http.StatusBadGateway, errorBody{Error: uploadErr.Error()})
		return
	case result == nil:
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "no file provided"})
		return
	case result.GetError() != "":
		writeJSON(w, http.StatusBadRequest, errorBody{Error: result.GetError()})
		return
	}
	writeJSON(w, http.StatusCreated, isoUploadResult{Name: result.GetName()})
}

// uploadISOStream opens managerd's UploadISO client stream, sends the
// required Metadata message (the file's own name, plus expectedHash
// gathered from the earlier form field), then relays part's bytes as a
// sequence of Chunk messages - see internal/frontend's identical helper.
func (s *Server) uploadISOStream(r *http.Request, part *multipart.Part, expectedHash string) (*rpcpb.UploadISOResponse, error) {
	stream, err := s.client.UploadISO(authContext(r))
	if err != nil {
		return nil, fmt.Errorf("opening upload stream: %w", err)
	}
	if err := stream.Send(&rpcpb.UploadISORequest{
		Data: &rpcpb.UploadISORequest_Metadata{
			Metadata: &rpcpb.ISOUploadMetadata{Name: part.FileName(), ExpectedSha256: expectedHash},
		},
	}); err != nil {
		return nil, fmt.Errorf("sending upload metadata: %w", err)
	}

	buf := make([]byte, 256*1024)
	for {
		n, rerr := part.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if serr := stream.Send(&rpcpb.UploadISORequest{Data: &rpcpb.UploadISORequest_Chunk{Chunk: chunk}}); serr != nil {
				return nil, fmt.Errorf("sending upload data: %w", serr)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("reading upload data: %w", rerr)
		}
	}
	return stream.CloseAndRecv()
}
