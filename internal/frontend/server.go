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

	// SortBy/SortDir are the sort currently applied to VMs ("id", "node",
	// or "state"; "asc" or "desc") - only used by the full index render,
	// to link each column header to toggle its own sort and to carry the
	// current sort forward into the polling tbody's own hx-get URL (see
	// index.html) so live refreshes don't reset it back to the default.
	SortBy  string
	SortDir string
}

// parseSort reads sort/dir query parameters, defaulting to ascending by
// ID - which used to be the *only* order (ListVMs's underlying map
// iteration is unordered), hence "keep it sorted alphabetically by
// default" being a real, user-visible bug fix, not just an added
// convenience. Any unrecognized sort value falls back to "id" rather
// than erroring - a stale or hand-edited URL parameter shouldn't break
// the page.
func parseSort(r *http.Request) (sortBy, dir string) {
	switch r.URL.Query().Get("sort") {
	case "node", "state":
		sortBy = r.URL.Query().Get("sort")
	default:
		sortBy = "id"
	}
	if r.URL.Query().Get("dir") == "desc" {
		dir = "desc"
	} else {
		dir = "asc"
	}
	return sortBy, dir
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
	s.mux.HandleFunc("GET /vms", s.handleListVMs)
	s.mux.HandleFunc("POST /vms", s.handleCreateVM)
	s.mux.HandleFunc("DELETE /vms/{id}", s.handleDeleteVM)
}

// currentVMs fetches the current VM list, sorted by sortBy/dir (see
// parseSort), returning an empty slice (not an error) if the fetch
// fails - callers fold the failure into the page as an error message
// instead, since a fetch failure shouldn't crash a human-facing page
// render. ListVMs's own order is unspecified (backed by a Go map on the
// FSM side), so sorting here - never leaving the caller's raw order -
// is what actually makes the table's order stable and predictable.
func (s *Server) currentVMs(r *http.Request, sortBy, dir string) ([]vmView, string) {
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
	sortVMs(vms, sortBy, dir)
	return vms, ""
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	sortBy, dir := parseSort(r)
	vms, errMsg := s.currentVMs(r, sortBy, dir)
	nodes, err := s.knownNodes(r)
	if err != nil && errMsg == "" {
		errMsg = err.Error()
	}
	s.render(w, "layout", pageData{Error: errMsg, VMs: vms, Nodes: nodes, SortBy: sortBy, SortDir: dir})
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

// handleListVMs serves just the vm_rows fragment, for HTMX polling
// (hx-trigger="every ...") to pick up reconciliation progress - e.g. a
// VM's State column moving from "pending" to "creating" to "ready" -
// without a full page reload.
func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	sortBy, dir := parseSort(r)
	vms, errMsg := s.currentVMs(r, sortBy, dir)
	s.render(w, "vm_rows", pageData{Error: errMsg, VMs: vms})
}

func (s *Server) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderVMRowsAndFormError(w, r, "invalid form: "+err.Error())
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
		s.renderVMRowsAndFormError(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderVMRowsAndFormError(w, r, resp.GetError())
		return
	}

	// Empty formErr on success clears any stale error left over from a
	// previous failed attempt, rather than leaving it displayed forever.
	s.renderVMRowsAndFormError(w, r, "")
}

// renderVMRowsAndFormError refreshes the VM table and reports a
// create-form-specific error (if any) as a separate out-of-band swap
// into index.html's #create-error slot, rather than inline in the VMs
// table (vm_rows's own {{if .Error}} row is for errors *about* the
// list itself - e.g. a failed fetch - not about a create attempt that
// never touched the list at all). HTMX processes the hx-swap-oob
// element in the response independently of the request's hx-target, so
// one response can update both the table and the form's error slot.
func (s *Server) renderVMRowsAndFormError(w http.ResponseWriter, r *http.Request, formErr string) {
	sortBy, dir := parseSort(r)
	vms, fetchErr := s.currentVMs(r, sortBy, dir)
	if fetchErr != "" {
		if formErr == "" {
			formErr = fetchErr
		} else {
			formErr += "; additionally failed to refresh list: " + fetchErr
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "vm_rows", pageData{VMs: vms}); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.tmpl.ExecuteTemplate(w, "create_error", formErr); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
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

	sortBy, dir := parseSort(r)
	vms, errMsg := s.currentVMs(r, sortBy, dir)
	s.render(w, "vm_rows", pageData{Error: errMsg, VMs: vms})
}

// renderRowsWithError re-fetches the current (unchanged) VM list and
// renders it alongside msg, so a failed action still shows the real
// state rather than leaving the UI out of sync.
func (s *Server) renderRowsWithError(w http.ResponseWriter, r *http.Request, msg string) {
	sortBy, dir := parseSort(r)
	vms, fetchErr := s.currentVMs(r, sortBy, dir)
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
