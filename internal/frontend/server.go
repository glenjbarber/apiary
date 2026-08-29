package frontend

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/web"
)

// pageData is passed to both the full index page and the vm_rows
// fragment - Error is empty on the normal path; VMs is always the
// current list, even when an action failed, so the UI never shows a
// stale table alongside an error.
type pageData struct {
	Error string
	VMs   []vmView

	// Nodes lists known raft cluster member IDs, for the create-VM form's
	// node picker. Only populated for the full index render - vm_rows
	// doesn't include the create form, so it doesn't need this.
	Nodes []string
}

// Server renders the HTMX web UI, backed by a rpcpb.ManagerServiceClient.
type Server struct {
	client rpcpb.ManagerServiceClient
	tmpl   *template.Template
	mux    *http.ServeMux
}

// NewServer parses the embedded templates and returns a Server that
// answers requests using client.
func NewServer(client rpcpb.ManagerServiceClient) (*Server, error) {
	tmpl, err := template.ParseFS(web.FS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("frontend: parsing templates: %w", err)
	}

	s := &Server{client: client, tmpl: tmpl, mux: http.NewServeMux()}
	s.routes()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.Handle("GET /static/", http.FileServerFS(web.FS))
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("POST /vms", s.handleCreateVM)
	s.mux.HandleFunc("DELETE /vms/{id}", s.handleDeleteVM)
}

// currentVMs fetches the current VM list, returning an empty slice (not
// an error) if the fetch fails - callers fold the failure into the page
// as an error message instead, since a fetch failure shouldn't crash a
// human-facing page render.
func (s *Server) currentVMs(r *http.Request) ([]vmView, string) {
	resp, err := s.client.ListVMs(r.Context(), &rpcpb.ListVMsRequest{})
	if err != nil {
		return nil, err.Error()
	}
	if resp.GetError() != "" {
		msg := resp.GetError()
		if resp.GetLeaderHint() != "" {
			msg += " (leader hint: " + resp.GetLeaderHint() + ")"
		}
		return nil, msg
	}

	vms := make([]vmView, 0, len(resp.GetVms()))
	for _, d := range resp.GetVms() {
		vms = append(vms, fromRPCVM(d))
	}
	return vms, ""
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	vms, errMsg := s.currentVMs(r)
	nodes, err := s.knownNodes(r)
	if err != nil && errMsg == "" {
		errMsg = err.Error()
	}
	s.render(w, "layout", pageData{Error: errMsg, VMs: vms, Nodes: nodes})
}

// knownNodes fetches the current raft cluster membership via Status, for
// the create-VM form's node picker. A failure here doesn't prevent the
// page from rendering - the picker just falls back to a free-text
// default (see index.html).
func (s *Server) knownNodes(r *http.Request) ([]string, error) {
	resp, err := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	if err != nil {
		return nil, err
	}
	return resp.GetKnownNodeIds(), nil
}

func (s *Server) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderRowsWithError(w, r, "invalid form: "+err.Error())
		return
	}

	vcpus, _ := strconv.ParseUint(r.FormValue("vcpus"), 10, 32)
	memoryMB, _ := strconv.ParseUint(r.FormValue("memory_mb"), 10, 64)

	resp, err := s.client.CreateVM(r.Context(), &rpcpb.CreateVMRequest{
		Vm: &rpcpb.VMDefinition{
			Id:           r.FormValue("id"),
			Name:         r.FormValue("name"),
			Vcpus:        uint32(vcpus),
			MemoryMb:     memoryMB,
			NodeId:       r.FormValue("node_id"),
			DesiredState: stateToRPC(r.FormValue("desired_state")),
		},
	})
	if err != nil {
		s.renderRowsWithError(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderRowsWithError(w, r, resp.GetError())
		return
	}

	vms, errMsg := s.currentVMs(r)
	s.render(w, "vm_rows", pageData{Error: errMsg, VMs: vms})
}

func (s *Server) handleDeleteVM(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.DeleteVM(r.Context(), &rpcpb.DeleteVMRequest{Id: r.PathValue("id")})
	if err != nil {
		s.renderRowsWithError(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderRowsWithError(w, r, resp.GetError())
		return
	}

	vms, errMsg := s.currentVMs(r)
	s.render(w, "vm_rows", pageData{Error: errMsg, VMs: vms})
}

// renderRowsWithError re-fetches the current (unchanged) VM list and
// renders it alongside msg, so a failed action still shows the real
// state rather than leaving the UI out of sync.
func (s *Server) renderRowsWithError(w http.ResponseWriter, r *http.Request, msg string) {
	vms, fetchErr := s.currentVMs(r)
	if fetchErr != "" {
		msg = msg + "; additionally failed to refresh list: " + fetchErr
	}
	s.render(w, "vm_rows", pageData{Error: msg, VMs: vms})
}

func (s *Server) render(w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}
