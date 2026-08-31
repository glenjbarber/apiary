package frontend

import (
	"crypto/subtle"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

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

	// AuthEnabled reports whether login is required at all, so the nav
	// partial only shows a "Log out" link when there's actually a
	// session to log out of.
	AuthEnabled bool

	// LoginError and NextURL are only used by the login page: a failed
	// attempt's message, and the originally-requested path to return to
	// after a successful login (see isSafeRedirectPath).
	LoginError string
	NextURL    string

	// ConsoleVMID/ConsoleVMName/ConsoleWSPath/ConsoleError are only used
	// by the console page (console.go) - see its own doc comments.
	ConsoleVMID   string
	ConsoleVMName string
	ConsoleWSPath string
	ConsoleError  string

	// Networks lists known networks (ADR-0022): for the Networks page's
	// own table, and for the create-VM form's network picker.
	Networks []networkView

	// NetworkFormError reports a create/delete-specific error for the
	// Networks page, rendered the same way ISOFormError is for Images.
	NetworkFormError string

	// APIKeys lists existing API keys (metadata only) for the API Keys
	// page's table (ADR-0023).
	APIKeys []apiKeyView

	// APIKeyFormError reports a create/revoke-specific error, rendered
	// the same way NetworkFormError/ISOFormError are for their pages.
	APIKeyFormError string

	// APIKeyRawName/APIKeyRawValue hold a just-created key's name and
	// one-time raw value, so the panel can show it exactly once with an
	// explicit "you will not see this again" warning - the same
	// one-shot-reveal pattern ISOFormSuccess uses for uploads, just for
	// a secret instead of a filename. Both empty on every render except
	// the one immediately following a successful create.
	APIKeyRawName  string
	APIKeyRawValue string
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

	// authUser/authPass gate every route except /login and /static/ when
	// non-empty (both empty disables login entirely - the default,
	// matching this project's current single-developer/local-network
	// stage). sessions tracks logged-in sessions; see session.go.
	authUser string
	authPass string
	sessions *sessionStore
}

// NewServer parses the embedded templates and returns a Server that
// answers requests using client. authUser/authPass enable a login page
// gating the whole UI when both are non-empty; pass "", "" to disable
// login entirely.
func NewServer(client rpcpb.ManagerServiceClient, authUser, authPass string) (*Server, error) {
	tmpl, err := template.ParseFS(web.FS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("frontend: parsing templates: %w", err)
	}

	s := &Server{
		client:   client,
		tmpl:     tmpl,
		mux:      http.NewServeMux(),
		authUser: authUser,
		authPass: authPass,
		sessions: newSessionStore(),
	}
	s.routes()
	return s, nil
}

// ServeHTTP gates every request behind a valid session when login is
// enabled, except /login itself and /static/ assets (the login page
// needs its own CSS, and obviously can't require a session to reach).
// An htmx request that hits this gate gets HX-Redirect rather than a
// bare 302, matching the pattern already used for the create-VM form's
// own success redirect - a plain Location header on an XHR/fetch
// response doesn't reliably drive the *browser's* navigation the way a
// real page load does.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.authUser != "" && r.URL.Path != "/login" && !strings.HasPrefix(r.URL.Path, "/static/") {
		if !s.hasValidSession(r) {
			s.redirectToLogin(w, r)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) hasValidSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	return s.sessions.Valid(c.Value)
}

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	next := "/login"
	if isSafeRedirectPath(r.URL.RequestURI()) {
		next = "/login?next=" + url.QueryEscape(r.URL.RequestURI())
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", next)
		return
	}
	http.Redirect(w, r, next, http.StatusFound)
}

// isSafeRedirectPath rejects anything that isn't an in-app relative
// path - in particular a protocol-relative "//evil.com" or an absolute
// "https://evil.com" URL, either of which would turn the login form's
// own "next" parameter into an open redirect.
func isSafeRedirectPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.Contains(p, "://")
}

func (s *Server) routes() {
	s.mux.Handle("GET /static/", http.FileServerFS(web.FS))
	s.mux.HandleFunc("GET /login", s.handleLoginPage)
	s.mux.HandleFunc("POST /login", s.handleLogin)
	s.mux.HandleFunc("POST /logout", s.handleLogout)
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
	s.mux.HandleFunc("GET /vms/{id}/console", s.handleConsolePage)
	s.mux.HandleFunc("GET /vms/{id}/console/ws", s.handleConsoleWS)
	s.mux.HandleFunc("GET /networks", s.handleNetworksPage)
	s.mux.HandleFunc("POST /networks", s.handleCreateNetwork)
	s.mux.HandleFunc("DELETE /networks/{id}", s.handleDeleteNetwork)
	s.mux.HandleFunc("GET /apikeys", s.handleAPIKeysPage)
	s.mux.HandleFunc("POST /apikeys", s.handleCreateAPIKey)
	s.mux.HandleFunc("DELETE /apikeys/{id}", s.handleRevokeAPIKey)
}

// handleLoginPage serves the login form. If login isn't enabled at all,
// there's nothing to log into - redirect straight to the normal
// landing page rather than showing a form that can't do anything.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.authUser == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.render(w, "login_page", pageData{NextURL: r.URL.Query().Get("next")})
}

// handleLogin checks the submitted credentials with a constant-time
// comparison (avoiding a timing side-channel on either field), and on
// success starts a session and redirects to NextURL if it's a safe
// in-app path, or "/" otherwise.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, "login_page", pageData{LoginError: "invalid form: " + err.Error()})
		return
	}
	user := r.FormValue("username")
	pass := r.FormValue("password")
	next := r.FormValue("next")

	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.authUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.authPass)) == 1
	if !userOK || !passOK {
		s.render(w, "login_page", pageData{LoginError: "invalid username or password", NextURL: next})
		return
	}

	token, err := s.sessions.Create()
	if err != nil {
		s.render(w, "login_page", pageData{LoginError: "could not start a session: " + err.Error(), NextURL: next})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})

	dest := "/"
	if next != "" && isSafeRedirectPath(next) {
		dest = next
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		s.sessions.Delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
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
	s.render(w, "stats_page", pageData{Error: errMsg, Stats: stats, ActivePage: "stats", AuthEnabled: s.authUser != ""})
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
	s.render(w, "vms_page", pageData{Error: errMsg, VMs: vms, SortBy: sortBy, SortDir: dir, ActivePage: "vms", AuthEnabled: s.authUser != ""})
}

// handleImagesPage serves the Images (ISO upload/list) page ("/images").
func (s *Server) handleImagesPage(w http.ResponseWriter, r *http.Request) {
	isos, errMsg := s.currentISOs(r)
	s.render(w, "images_page", pageData{ISOs: isos, ISOFormError: errMsg, ActivePage: "images", AuthEnabled: s.authUser != ""})
}

// handleNewVMPage serves the create-VM form page ("/vms/new"). A failed
// Nodes/ISOs/Networks fetch isn't surfaced as an error here - the node
// picker already falls back to a free-text input when Nodes is empty
// (see new_vm.html), and an empty ISO/network picker just means
// "(none)" is the only option, both harmless degraded states rather
// than failures worth a banner.
func (s *Server) handleNewVMPage(w http.ResponseWriter, r *http.Request) {
	nodes, _ := s.knownNodes(r)
	isos, _ := s.currentISOs(r)
	networks, _ := s.currentNetworks(r)
	s.render(w, "new_vm_page", pageData{Nodes: nodes, ISOs: isos, Networks: networks, ActivePage: "new_vm", AuthEnabled: s.authUser != ""})
}

// currentNetworks fetches the current list of networks, returning an
// empty slice (not an error) if the fetch fails - the same fail-soft
// convention currentVMs/currentISOs follow.
func (s *Server) currentNetworks(r *http.Request) ([]networkView, string) {
	resp, err := s.client.ListNetworks(r.Context(), &rpcpb.ListNetworksRequest{})
	if err != nil {
		return nil, err.Error()
	}
	if resp.GetError() != "" {
		return nil, resp.GetError()
	}
	networks := make([]networkView, 0, len(resp.GetNetworks()))
	for _, n := range resp.GetNetworks() {
		networks = append(networks, fromRPCNetwork(n))
	}
	return networks, ""
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
			Id:            r.FormValue("id"),
			Name:          r.FormValue("name"),
			Vcpus:         uint32(vcpus),
			MemoryMb:      memoryMB,
			NodeId:        r.FormValue("node_id"),
			DesiredState:  stateToRPC(r.FormValue("desired_state")),
			IsoName:       r.FormValue("iso_name"),
			NetworkId:     r.FormValue("network_id"),
			ReplicaNodeId: r.FormValue("replica_node_id"),
			FirewallRules: parseFirewallRuleRows(r),
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

// parseFirewallRuleRows reads the create-VM form's repeating firewall-
// rule inputs (new_vm.html's client-side "add rule" rows, all sharing
// the same field names so they arrive as parallel slices) into
// FirewallRule messages. A row is skipped if its direction is empty -
// the template always renders one blank starter row, and an unused row
// shouldn't turn into an empty/invalid rule.
func parseFirewallRuleRows(r *http.Request) []*rpcpb.FirewallRule {
	directions := r.PostForm["fw_direction"]
	actions := r.PostForm["fw_action"]
	protocols := r.PostForm["fw_protocol"]
	ports := r.PostForm["fw_port"]

	var rules []*rpcpb.FirewallRule
	for i, direction := range directions {
		if direction == "" {
			continue
		}
		rule := &rpcpb.FirewallRule{Direction: direction}
		if i < len(actions) {
			rule.Action = actions[i]
		}
		if i < len(protocols) {
			rule.Protocol = protocols[i]
		}
		if i < len(ports) {
			rule.PortRange = ports[i]
		}
		rules = append(rules, rule)
	}
	return rules
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

// handleNetworksPage serves the Networks list/create page ("/networks").
func (s *Server) handleNetworksPage(w http.ResponseWriter, r *http.Request) {
	networks, errMsg := s.currentNetworks(r)
	s.render(w, "networks_page", pageData{Networks: networks, NetworkFormError: errMsg, ActivePage: "networks", AuthEnabled: s.authUser != ""})
}

// handleCreateNetwork follows the same combined-panel pattern as
// handleUploadISO/renderISOPanelResult: refresh the whole #network-panel
// (error slot + table together) rather than a separate error target.
func (s *Server) handleCreateNetwork(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderNetworkPanelResult(w, r, "invalid form: "+err.Error())
		return
	}

	vlanID, _ := strconv.ParseUint(r.FormValue("vlan_id"), 10, 32)

	resp, err := s.client.CreateNetwork(r.Context(), &rpcpb.CreateNetworkRequest{
		Network: &rpcpb.NetworkDefinition{
			Id:     r.FormValue("id"),
			Name:   r.FormValue("name"),
			VlanId: uint32(vlanID),
			Subnet: r.FormValue("subnet"),
		},
	})
	if err != nil {
		s.renderNetworkPanelResult(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderNetworkPanelResult(w, r, resp.GetError())
		return
	}
	s.renderNetworkPanelResult(w, r, "")
}

func (s *Server) handleDeleteNetwork(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.DeleteNetwork(r.Context(), &rpcpb.DeleteNetworkRequest{Id: r.PathValue("id")})
	if err != nil {
		s.renderNetworkPanelResult(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderNetworkPanelResult(w, r, resp.GetError())
		return
	}
	s.renderNetworkPanelResult(w, r, "")
}

// renderNetworkPanelResult refreshes the whole #network-panel (error
// slot + table together), the same combined-target pattern
// renderISOPanelResult uses for Images.
func (s *Server) renderNetworkPanelResult(w http.ResponseWriter, r *http.Request, formErr string) {
	networks, fetchErr := s.currentNetworks(r)
	if fetchErr != "" {
		if formErr == "" {
			formErr = fetchErr
		} else {
			formErr += "; additionally failed to refresh list: " + fetchErr
		}
	}
	s.render(w, "network_panel", pageData{NetworkFormError: formErr, Networks: networks})
}

// currentAPIKeys fetches the current list of API keys (metadata only),
// the same fail-soft convention currentNetworks/currentISOs follow.
func (s *Server) currentAPIKeys(r *http.Request) ([]apiKeyView, string) {
	resp, err := s.client.ListAPIKeys(r.Context(), &rpcpb.ListAPIKeysRequest{})
	if err != nil {
		return nil, err.Error()
	}
	if resp.GetError() != "" {
		return nil, resp.GetError()
	}
	keys := make([]apiKeyView, 0, len(resp.GetKeys()))
	for _, k := range resp.GetKeys() {
		keys = append(keys, fromRPCAPIKey(k))
	}
	return keys, ""
}

func (s *Server) handleAPIKeysPage(w http.ResponseWriter, r *http.Request) {
	keys, errMsg := s.currentAPIKeys(r)
	s.render(w, "apikeys_page", pageData{APIKeys: keys, APIKeyFormError: errMsg, ActivePage: "apikeys", AuthEnabled: s.authUser != ""})
}

// handleCreateAPIKey follows the same combined-panel pattern as
// handleCreateNetwork: refresh the whole #apikey-panel (error slot +
// table together). On success, the raw key is shown exactly once via
// APIKeyRawName/APIKeyRawValue - it is never retrievable again after
// this one render (see ADR-0023).
func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderAPIKeyPanelResult(w, r, "invalid form: "+err.Error(), "", "")
		return
	}

	name := r.FormValue("name")
	resp, err := s.client.CreateAPIKey(r.Context(), &rpcpb.CreateAPIKeyRequest{Name: name})
	if err != nil {
		s.renderAPIKeyPanelResult(w, r, err.Error(), "", "")
		return
	}
	if resp.GetError() != "" {
		s.renderAPIKeyPanelResult(w, r, resp.GetError(), "", "")
		return
	}
	s.renderAPIKeyPanelResult(w, r, "", resp.GetKey().GetName(), resp.GetRawKey())
}

func (s *Server) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.RevokeAPIKey(r.Context(), &rpcpb.RevokeAPIKeyRequest{Id: r.PathValue("id")})
	if err != nil {
		s.renderAPIKeyPanelResult(w, r, err.Error(), "", "")
		return
	}
	if resp.GetError() != "" {
		s.renderAPIKeyPanelResult(w, r, resp.GetError(), "", "")
		return
	}
	s.renderAPIKeyPanelResult(w, r, "", "", "")
}

// renderAPIKeyPanelResult refreshes the whole #apikey-panel (error slot
// + table + one-time raw-key reveal together), mirroring
// renderNetworkPanelResult's combined-target pattern.
func (s *Server) renderAPIKeyPanelResult(w http.ResponseWriter, r *http.Request, formErr, rawName, rawValue string) {
	keys, fetchErr := s.currentAPIKeys(r)
	if fetchErr != "" {
		if formErr == "" {
			formErr = fetchErr
		} else {
			formErr += "; additionally failed to refresh list: " + fetchErr
		}
	}
	s.render(w, "apikey_panel", pageData{APIKeyFormError: formErr, APIKeys: keys, APIKeyRawName: rawName, APIKeyRawValue: rawValue})
}

func (s *Server) render(w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}
