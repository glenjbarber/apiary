package frontend

import (
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/web"
)

// pageData is passed to every full-page and fragment render - Error is
// empty on the normal path; VMs is always the current list, even when
// an action failed, so the UI never shows a stale table alongside an
// error.
type pageData struct {
	Error string
	VMs   []vmView

	// ActivePage names the current page ("vms", "images", or "new_vm"),
	// for the shared nav partial to bold/underline the matching link.
	// Empty for fragment renders, which don't include the nav.
	ActivePage string

	// Nodes lists known raft cluster member IDs, for the create-VM form's
	// node picker. Only populated for the New VM page.
	Nodes []string

	// SortBy/SortDir are the sort currently applied to VMs ("id", "node",
	// or "state"; "asc" or "desc") - only used by the full index render,
	// to link each column header to toggle its own sort and to carry the
	// current sort forward into the polling tbody's own hx-get URL (see
	// vms.html) so live refreshes don't reset it back to the default.
	SortBy  string
	SortDir string

	// ISOs lists stored installer images, for both the Images section's
	// table and the create-VM form's ISO picker.
	ISOs []isoView

	// ISOFormError reports an upload/delete-specific error for the
	// Images section. Rendered inside the same #iso-panel target the
	// upload form's own hx-target points at - not via an out-of-band
	// swap, unlike #create-error's, because htmx's hx-encoding
	// "multipart/form-data" doesn't reliably apply hx-swap-oob elements
	// found in the response (confirmed: the exact same oob markup works
	// for the non-multipart create-VM form but silently no-ops here).
	// Empty on the normal index render.
	ISOFormError string

	// ISOFormSuccess reports a just-completed upload's name, so the
	// Images panel shows an explicit confirmation rather than success
	// being indistinguishable from "nothing happened yet" - a large
	// upload's only other visible change is one more row appearing at
	// the bottom of a table the user may not be looking at.
	ISOFormSuccess string

	// Stats is this node's host stats snapshot, for the Stats page.
	Stats statsView
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
	s.mux.HandleFunc("GET /{$}", s.handleStatsPage)
	s.mux.HandleFunc("GET /vms", s.handleVMsPage)
	s.mux.HandleFunc("GET /vms/rows", s.handleListVMs)
	s.mux.HandleFunc("GET /images", s.handleImagesPage)
	s.mux.HandleFunc("GET /vms/new", s.handleNewVMPage)
	s.mux.HandleFunc("POST /vms", s.handleCreateVM)
	s.mux.HandleFunc("DELETE /vms/{id}", s.handleDeleteVM)
	s.mux.HandleFunc("GET /isos", s.handleListISOs)
	s.mux.HandleFunc("POST /isos", s.handleUploadISO)
	s.mux.HandleFunc("DELETE /isos/{name}", s.handleDeleteISO)
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

// handleStatsPage serves the host stats page - the default landing
// page ("/"), ahead of VMs/Images/New VM, since a node's own health is
// what an operator most likely wants to see first.
func (s *Server) handleStatsPage(w http.ResponseWriter, r *http.Request) {
	stats, errMsg := s.currentStats(r)
	s.render(w, "stats_page", pageData{Error: errMsg, Stats: stats, ActivePage: "stats"})
}

// currentStats fetches a HostStats snapshot, returning a zero-value
// statsView (not an error) if the fetch fails entirely - the same
// fail-soft convention currentVMs/currentISOs follow. A partial
// failure (one subsystem down) is instead carried in statsView.Errors,
// since hoststats.Gather already reports best-effort per subsystem
// rather than failing outright (see internal/hoststats.Snapshot).
func (s *Server) currentStats(r *http.Request) (statsView, string) {
	resp, err := s.client.HostStats(r.Context(), &rpcpb.HostStatsRequest{})
	if err != nil {
		return statsView{}, err.Error()
	}
	return fromRPCStats(resp), ""
}

// handleVMsPage serves the VMs list page ("/vms").
func (s *Server) handleVMsPage(w http.ResponseWriter, r *http.Request) {
	sortBy, dir := parseSort(r)
	vms, errMsg := s.currentVMs(r, sortBy, dir)
	s.render(w, "vms_page", pageData{Error: errMsg, VMs: vms, SortBy: sortBy, SortDir: dir, ActivePage: "vms"})
}

// handleImagesPage serves the Images (ISO upload/list) page ("/images").
func (s *Server) handleImagesPage(w http.ResponseWriter, r *http.Request) {
	isos, errMsg := s.currentISOs(r)
	s.render(w, "images_page", pageData{ISOs: isos, ISOFormError: errMsg, ActivePage: "images"})
}

// handleNewVMPage serves the create-VM form page ("/vms/new"). A failed
// Nodes/ISOs fetch isn't surfaced as an error here - the node picker
// already falls back to a free-text input when Nodes is empty (see
// new_vm.html), and an empty ISO picker just means "(none)" is the only
// option, both harmless degraded states rather than failures worth a
// banner.
func (s *Server) handleNewVMPage(w http.ResponseWriter, r *http.Request) {
	nodes, _ := s.knownNodes(r)
	isos, _ := s.currentISOs(r)
	s.render(w, "new_vm_page", pageData{Nodes: nodes, ISOs: isos, ActivePage: "new_vm"})
}

// currentISOs fetches the current list of stored installer images,
// returning an empty slice (not an error) if the fetch fails - the same
// fail-soft convention currentVMs follows.
func (s *Server) currentISOs(r *http.Request) ([]isoView, string) {
	resp, err := s.client.ListISOs(r.Context(), &rpcpb.ListISOsRequest{})
	if err != nil {
		return nil, err.Error()
	}
	if resp.GetError() != "" {
		return nil, resp.GetError()
	}
	isos := make([]isoView, 0, len(resp.GetIsos()))
	for _, i := range resp.GetIsos() {
		isos = append(isos, fromRPCISO(i))
	}
	return isos, ""
}

// knownNodes fetches the current raft cluster membership via Status, for
// the create-VM form's node picker. A failure here doesn't prevent the
// page from rendering - the picker just falls back to a free-text
// default (see new_vm.html).
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

// handleCreateVM lives on its own page (/vms/new, see new_vm.html) now
// that VMs/Images/New VM are separate pages - there's no VM table on
// this page to refresh, so a validation/application error just renders
// directly into the form's own #create-error target. On success, an
// HX-Redirect tells htmx to navigate the browser to the VMs page
// outright, where the new VM shows up (starting at "pending", per
// ADR-0016's reconciliation phase) via that page's own normal render -
// simpler and more honest than trying to fake a VMs-table view here.
func (s *Server) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderCreateError(w, "invalid form: "+err.Error())
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
			IsoName:      r.FormValue("iso_name"),
		},
	})
	if err != nil {
		s.renderCreateError(w, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderCreateError(w, resp.GetError())
		return
	}
	w.Header().Set("HX-Redirect", "/vms")
}

// renderCreateError writes formErr as the create form's #create-error
// contents (its hx-target points directly at that div - no oob swap,
// no separate table to refresh alongside it).
func (s *Server) renderCreateError(w http.ResponseWriter, formErr string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, template.HTMLEscapeString(formErr))
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

// handleListISOs serves just the iso_rows fragment, for refreshing the
// Images table after an upload or delete without a full page reload -
// same pattern as handleListVMs/vm_rows.
func (s *Server) handleListISOs(w http.ResponseWriter, r *http.Request) {
	isos, errMsg := s.currentISOs(r)
	s.render(w, "iso_rows", pageData{Error: errMsg, ISOs: isos})
}

// handleUploadISO streams a multipart file upload directly into
// managerd's UploadISO RPC, chunk by chunk, without ever buffering the
// whole file in this process - an installer image can be several
// gigabytes. This requires the form's hash field to be encoded before
// its file field (see images.html's field order), since MultipartReader
// processes parts strictly in the order the client sent them - by the
// time the file part arrives, the hash needed for its Metadata message
// must already be known.
func (s *Server) handleUploadISO(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		s.renderISOPanelResult(w, r, "invalid upload: "+err.Error(), "")
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
		// fall through to the error render below
	case result == nil:
		uploadErr = fmt.Errorf("no file provided")
	case result.GetError() != "":
		uploadErr = fmt.Errorf("%s", result.GetError())
	}
	if uploadErr != nil {
		s.renderISOPanelResult(w, r, uploadErr.Error(), "")
		return
	}
	s.renderISOPanelResult(w, r, "", result.GetName())
}

// uploadISOStream opens managerd's UploadISO client stream, sends the
// required Metadata message (the file's own name, plus expectedHash
// gathered from an earlier form field), then relays part's bytes as a
// sequence of Chunk messages.
func (s *Server) uploadISOStream(r *http.Request, part *multipart.Part, expectedHash string) (*rpcpb.UploadISOResponse, error) {
	stream, err := s.client.UploadISO(r.Context())
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

// renderISOPanelResult refreshes the whole #iso-panel (error/success
// slot + images table together) as one target, rather than the out-of-
// band-swap pattern used elsewhere (renderVMRowsAndFormError/
// #create-error). That pattern doesn't work here: htmx's hx-encoding
// "multipart/form-data" doesn't reliably apply hx-swap-oob elements
// found in the response - confirmed directly, since the identical oob
// markup works fine for the non-multipart create-VM form. Targeting one
// combined element sidesteps the issue entirely instead of depending on
// an htmx behavior that doesn't hold for this request type.
//
// formErr and successName are mutually exclusive - callers pass exactly
// one non-empty. successName exists because a successful upload's only
// other visible change is one more row appearing in a table the user
// may not be scrolled to; without an explicit confirmation, "it
// finished with no error" and "it silently didn't happen" look
// identical.
func (s *Server) renderISOPanelResult(w http.ResponseWriter, r *http.Request, formErr, successName string) {
	isos, fetchErr := s.currentISOs(r)
	if fetchErr != "" {
		if formErr == "" {
			formErr = fetchErr
		} else {
			formErr += "; additionally failed to refresh list: " + fetchErr
		}
	}
	var success string
	if formErr == "" && successName != "" {
		success = fmt.Sprintf("Uploaded %s successfully.", successName)
	}
	s.render(w, "iso_panel", pageData{ISOFormError: formErr, ISOFormSuccess: success, ISOs: isos})
}

func (s *Server) handleDeleteISO(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.DeleteISO(r.Context(), &rpcpb.DeleteISORequest{Name: r.PathValue("name")})
	if err != nil {
		s.renderISOPanelResult(w, r, err.Error(), "")
		return
	}
	if resp.GetError() != "" {
		s.renderISOPanelResult(w, r, resp.GetError(), "")
		return
	}
	s.renderISOPanelResult(w, r, "", "")
}

func (s *Server) render(w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}
