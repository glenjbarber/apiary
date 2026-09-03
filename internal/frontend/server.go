package frontend

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/manager"
	"github.com/glenjbarber/apiary/internal/pam"
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

	// Stats is one node's host stats snapshot, for the verbose per-node
	// page ("/host/{id}").
	Stats statsView

	// ClusterNodes is the basic per-node summary row list for the
	// default landing page ("/").
	ClusterNodes []clusterNodeView

	// AuthEnabled reports whether login is required at all, so the nav
	// partial only shows a "Log out" link when there's actually a
	// session to log out of.
	AuthEnabled bool

	// Username/Role identify the current session (ADR-0030) - both
	// empty when login is disabled or (in principle) unreachable
	// states like a request that slipped past ServeHTTP's own gate.
	// The nav partial shows "logged in as <Username> (<Role>)" only
	// when Username is non-empty.
	Username   string
	Role       string
	CanOperate bool
	CanAdmin   bool

	// VM is the single virtual machine rendered by the detail page.
	VM vmView

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

	// SerialLogVMID/SerialLogVMName/SerialLogContent/SerialLogTruncated/
	// SerialLogError are only used by the serial log page
	// (serial_log.go) - see its own doc comments.
	SerialLogVMID      string
	SerialLogVMName    string
	SerialLogContent   string
	SerialLogTruncated bool
	SerialLogError     string

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

	// Jails lists known jails (ADR-0026's jail-orchestration half): for
	// the Jails page's own table.
	Jails []jailView

	// JailFormError reports a create/delete-specific error for the
	// Jails page, rendered the same way NetworkFormError is for
	// Networks.
	JailFormError string

	// Users lists every roleMap entry (ADR-0039), for the Users page's
	// own table - each row's "can I change this account's password"
	// action is computed once here (CanChange), not re-derived in the
	// template, since the authorization rule (canChangePassword) is
	// Go logic a template action can't express inline.
	Users []userView

	// UserFormError/UserFormSuccess report a change-password result,
	// rendered the same way APIKeyFormError/ISOFormSuccess are for
	// their own pages.
	UserFormError   string
	UserFormSuccess string

	// ClusterISOs is the cluster-wide view of every known node's stored
	// images (ADR-0041) - distinct from ISOs (still local-only, used by
	// the Images page's own upload/delete panel), used by the
	// create-VM/create-jail forms' image pickers to show a "will be
	// fetched from a peer" cue: internal/cluster's Reconciler now
	// fetches a missing image automatically at provisioning time, so
	// the picker no longer needs to restrict itself to images already
	// present on the currently-selected node.
	ClusterISOs []isoRowView
}

// userView is one row of the Users page's table.
type userView struct {
	Username  string
	Role      string
	CanChange bool
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

	// auth authenticates a login attempt's username/password (ADR-0030,
	// real PAM by default in cmd/frontend) - nil disables login
	// entirely, the default, matching this project's current single-
	// developer/local-network stage. roleMap resolves an authenticated
	// username to a Role; a username with no entry is rejected at
	// login (default-deny), never silently downgraded to Viewer.
	// sessions tracks logged-in sessions; see session.go. lockouts
	// tracks repeated failed login attempts per username, so an online
	// password-guessing attack against a real PAM account is at least
	// slowed down, not merely reported as "invalid" forever with no
	// consequence - see lockout.go.
	auth     pam.Authenticator
	roleMap  map[string]manager.Role
	sessions *sessionStore
	lockouts *loginAttemptTracker

	// peers is nil-able (see cmd/frontend's -peer-tls/-peer-hostname-suffix/
	// -peer-manager-port) - the cluster overview page ("/") falls back to
	// reporting only this frontend's own colocated node when unset,
	// rather than trying to dial peers with no address-derivation
	// configured.
	peers              peerHostStatsClient
	peerHostnameSuffix string
	peerManagerPort    string

	// passwords implements the actual UNIX-account password change
	// (ADR-0039) - real pw(8) by default in cmd/frontend, faked in
	// tests. See password.go's canChangePassword for the authorization
	// rule gating who may target whose account.
	passwords PasswordSetter
}

// pageHeaderData is the argument type the "page_header" template (see
// layout.html) renders - built exclusively by the pageHeader helper
// below, never from request-derived data, so Extra being pre-built
// template.HTML (rather than auto-escaped plain text) is safe: every
// caller is this package's own Go code passing a fixed literal or
// another helper's output, never anything a user typed.
type pageHeaderData struct {
	Title    string
	Subtitle string
	Extra    template.HTML
}

// pageHeader builds a page_header argument. extra, if given, is a
// trusted HTML fragment (e.g. a Cancel/Back link or an action button) -
// most pages have none.
func pageHeader(title, subtitle string, extra ...string) pageHeaderData {
	var e template.HTML
	if len(extra) > 0 {
		e = template.HTML(extra[0])
	}
	return pageHeaderData{Title: title, Subtitle: subtitle, Extra: e}
}

// vmSubtitle/nodeSubtitle exist because the "page_header" template call
// is a single expression - a template action has no if/else available
// inside a function-call argument, so the two pages whose subtitle is
// conditional on request data (serial_log, stats) compute it here in
// Go instead.
func vmSubtitle(name, id string) string {
	if name != "" {
		return name + " · " + id + " · refreshes every 3 seconds"
	}
	return id + " · refreshes every 3 seconds"
}

func nodeSubtitle(nodeID string) string {
	if nodeID != "" {
		return "Node " + nodeID
	}
	return "Local manager node"
}

// NewServer parses the embedded templates and returns a Server that
// answers requests using client. auth enables a login page gating the
// whole UI when non-nil; pass nil to disable login entirely (roleMap
// is then unused). peers (nil-able) lets the cluster overview page
// ("/") fetch other nodes' HostStats directly - see cmd/frontend's own
// -peer-tls/-peer-hostname-suffix/-peer-manager-port flags for how
// peerHostnameSuffix/peerManagerPort combine with a node ID to form
// its managerd address. passwords implements the real UNIX-account
// password change (ADR-0039) - pass UnixPasswordSetter{} in production.
func NewServer(client rpcpb.ManagerServiceClient, auth pam.Authenticator, roleMap map[string]manager.Role, peers peerHostStatsClient, peerHostnameSuffix, peerManagerPort string, passwords PasswordSetter) (*Server, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"pageHeader":       pageHeader,
		"vmSubtitle":       vmSubtitle,
		"nodeSubtitle":     nodeSubtitle,
		"isoMissingByNode": isoMissingByNode,
	}).ParseFS(web.FS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("frontend: parsing templates: %w", err)
	}

	s := &Server{
		client:             client,
		tmpl:               tmpl,
		lockouts:           newLoginAttemptTracker(defaultMaxFailedAttempts, defaultAttemptWindow, defaultLockDuration),
		mux:                http.NewServeMux(),
		auth:               auth,
		roleMap:            roleMap,
		sessions:           newSessionStore(),
		peers:              peers,
		peerHostnameSuffix: peerHostnameSuffix,
		peerManagerPort:    peerManagerPort,
		passwords:          passwords,
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
	if s.auth != nil && r.URL.Path != "/login" && !strings.HasPrefix(r.URL.Path, "/static/") {
		if _, ok := s.currentSession(r); !ok {
			s.redirectToLogin(w, r)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// currentSession returns the requesting session's identity, if any.
func (s *Server) currentSession(r *http.Request) (sessionInfo, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return sessionInfo{}, false
	}
	return s.sessions.Valid(c.Value)
}

// withAuthFields fills in pd's AuthEnabled/Username/Role from the
// request's current session (ADR-0030) and returns it - a small
// helper so every full-page render doesn't repeat the same three-line
// lookup. Safe to call even when login is disabled or no session
// exists; both leave Username/Role empty.
func (s *Server) withAuthFields(r *http.Request, pd pageData) pageData {
	pd.AuthEnabled = s.auth != nil
	pd.CanOperate = s.auth == nil
	pd.CanAdmin = s.auth == nil
	if info, ok := s.currentSession(r); ok {
		pd.Username = info.username
		pd.Role = string(info.role)
		pd.CanOperate = info.role.Satisfies(manager.RoleOperator)
		pd.CanAdmin = info.role.Satisfies(manager.RoleAdmin)
	}
	return pd
}

// requireRole wraps handler so it only runs for a session whose role
// satisfies at least want (ADR-0030) - a no-op wrapper when login
// itself is disabled (s.auth == nil), matching this project's existing
// "no login configured" default of leaving every route fully open.
// ServeHTTP's own gate above already guarantees a valid session exists
// by the time any handler runs when login is enabled, so the only new
// outcome here is a role that's too low, not a missing session.
func (s *Server) requireRole(want manager.Role, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.auth == nil {
			handler(w, r)
			return
		}
		info, ok := s.currentSession(r)
		if !ok || !info.role.Satisfies(want) {
			http.Error(w, "forbidden: this account's role does not permit this action", http.StatusForbidden)
			return
		}
		handler(w, r)
	}
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

	// Viewer: every read-only page/route.
	s.mux.HandleFunc("GET /{$}", s.handleClusterOverviewPage)
	s.mux.HandleFunc("GET /host/{id}", s.handleHostPage)
	s.mux.HandleFunc("GET /vms", s.handleVMsPage)
	s.mux.HandleFunc("GET /vms/rows", s.handleListVMs)
	s.mux.HandleFunc("GET /vms/{id}", s.handleVMPage)
	s.mux.HandleFunc("GET /images", s.handleImagesPage)
	s.mux.HandleFunc("GET /isos", s.handleListISOs)
	s.mux.HandleFunc("GET /vms/{id}/console", s.handleConsolePage)
	s.mux.HandleFunc("GET /vms/{id}/console/ws", s.handleConsoleWS)
	s.mux.HandleFunc("GET /vms/{id}/serial", s.handleSerialLogPage)
	s.mux.HandleFunc("GET /vms/{id}/serial/content", s.handleSerialLogContent)
	s.mux.HandleFunc("GET /networks", s.handleNetworksPage)
	s.mux.HandleFunc("GET /jails", s.handleJailsPage)

	// Operator: VM/jail/network/ISO lifecycle - including the create-VM
	// form's own GET, since a Viewer has nothing useful to do with a
	// form whose POST it can't reach anyway.
	s.mux.HandleFunc("GET /vms/new", s.requireRole(manager.RoleOperator, s.handleNewVMPage))
	s.mux.HandleFunc("POST /vms", s.requireRole(manager.RoleOperator, s.handleCreateVM))
	s.mux.HandleFunc("DELETE /vms/{id}", s.requireRole(manager.RoleOperator, s.handleDeleteVM))
	s.mux.HandleFunc("POST /isos", s.requireRole(manager.RoleOperator, s.handleUploadISO))
	s.mux.HandleFunc("DELETE /isos/{name}", s.requireRole(manager.RoleOperator, s.handleDeleteISO))

	// Copy-on-demand ISO replication (ADR-0040) - same RoleOperator gate
	// as upload/delete, matching write blast radius.
	s.mux.HandleFunc("POST /networks", s.requireRole(manager.RoleOperator, s.handleCreateNetwork))
	s.mux.HandleFunc("DELETE /networks/{id}", s.requireRole(manager.RoleOperator, s.handleDeleteNetwork))
	s.mux.HandleFunc("GET /jails/new", s.requireRole(manager.RoleOperator, s.handleNewJailPage))
	s.mux.HandleFunc("POST /jails", s.requireRole(manager.RoleOperator, s.handleCreateJail))
	s.mux.HandleFunc("DELETE /jails/{id}", s.requireRole(manager.RoleOperator, s.handleDeleteJail))

	// Admin: API-key management, entirely - including just viewing the
	// list, unlike every other Viewer-readable page above.
	s.mux.HandleFunc("GET /apikeys", s.requireRole(manager.RoleAdmin, s.handleAPIKeysPage))
	s.mux.HandleFunc("POST /apikeys", s.requireRole(manager.RoleAdmin, s.handleCreateAPIKey))
	s.mux.HandleFunc("DELETE /apikeys/{id}", s.requireRole(manager.RoleAdmin, s.handleRevokeAPIKey))

	// Users (ADR-0039): visible to everyone logged in (RoleViewer, the
	// same "always show, gate the action" convention every other page
	// here follows) - the page itself decides per-row whether a change-
	// password action is even offered. The route-level gate on the
	// actual change is RoleOperator (the coarsest role that can ever
	// change *any* password) - handleChangePassword re-checks the full
	// per-target rule itself, since Operator is let through here but
	// still must not be allowed to target Admin.
	s.mux.HandleFunc("GET /users", s.requireRole(manager.RoleViewer, s.handleUsersPage))
	s.mux.HandleFunc("POST /users/{username}/password", s.requireRole(manager.RoleOperator, s.handleChangePassword))
}

// handleLoginPage serves the login form. If login isn't enabled at all,
// there's nothing to log into - redirect straight to the normal
// landing page rather than showing a form that can't do anything.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.render(w, "login_page", pageData{NextURL: r.URL.Query().Get("next")})
}

// handleLogin authenticates the submitted credentials against s.auth
// (real PAM by default - ADR-0030) and, on success, resolves the
// username's Role via s.roleMap. A username with no role-map entry is
// rejected outright here - a valid PAM login that isn't a mistake
// (real account, real password) but that nobody has explicitly granted
// an Apiary role to is treated as "no access", never silently
// downgraded to Viewer, matching this project's established
// default-deny stance (ADR-0025/26/27/28/29's own reasoning, applied
// here to authorization instead of reconciliation).
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.render(w, "login_page", pageData{LoginError: "invalid form: " + err.Error()})
		return
	}
	user := r.FormValue("username")
	pass := r.FormValue("password")
	next := r.FormValue("next")

	// Checked before ever calling s.auth.Authenticate - a locked-out
	// username shouldn't cost a real PAM round-trip on every retry, and
	// short-circuiting here also avoids the auth backend itself being
	// used as an amplification/timing oracle while locked.
	if locked, remaining := s.lockouts.Locked(user); locked {
		s.render(w, "login_page", pageData{
			LoginError: fmt.Sprintf("too many failed attempts for this account - try again in %s", remaining.Round(time.Second)),
			NextURL:    next,
		})
		return
	}

	ok, err := s.auth.Authenticate(user, pass)
	if err != nil {
		s.render(w, "login_page", pageData{LoginError: "authentication backend error: " + err.Error(), NextURL: next})
		return
	}
	if !ok {
		s.lockouts.RecordFailure(user)
		s.render(w, "login_page", pageData{LoginError: "invalid username or password", NextURL: next})
		return
	}
	s.lockouts.RecordSuccess(user)
	role, hasRole := s.roleMap[user]
	if !hasRole {
		s.render(w, "login_page", pageData{LoginError: "no Apiary role is assigned to this account - contact an administrator", NextURL: next})
		return
	}

	token, err := s.sessions.Create(user, role)
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

// handleClusterOverviewPage/handleHostPage/nodeHostStats live in
// cluster_overview.go - the cluster-wide basic-status landing page
// ("/") and the verbose per-node page ("/host/{id}") it links to.

// handleVMsPage serves the VMs list page ("/vms").
func (s *Server) handleVMsPage(w http.ResponseWriter, r *http.Request) {
	sortBy, dir := parseSort(r)
	vms, errMsg := s.currentVMs(r, sortBy, dir)
	s.render(w, "vms_page", s.withAuthFields(r, pageData{Error: errMsg, VMs: vms, SortBy: sortBy, SortDir: dir, ActivePage: "vms"}))
}

// handleVMPage renders one VM as an operational summary. It deliberately
// uses the existing GetVM read path, so the page has the same leader-forwarded
// consistency semantics as the list without introducing another API surface.
func (s *Server) handleVMPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	resp, err := s.client.GetVM(r.Context(), &rpcpb.GetVMRequest{Id: id})
	if err != nil {
		s.render(w, "vm_page", s.withAuthFields(r, pageData{Error: err.Error(), ActivePage: "vms"}))
		return
	}
	if resp.GetError() != "" {
		s.render(w, "vm_page", s.withAuthFields(r, pageData{Error: resp.GetError(), ActivePage: "vms"}))
		return
	}
	if !resp.GetFound() {
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "vm_page", s.withAuthFields(r, pageData{Error: "virtual machine not found", ActivePage: "vms"}))
		return
	}
	s.render(w, "vm_page", s.withAuthFields(r, pageData{VM: fromRPCVM(resp.GetVm()), ActivePage: "vms"}))
}

// handleImagesPage serves the Images (ISO upload/list) page ("/images").
func (s *Server) handleImagesPage(w http.ResponseWriter, r *http.Request) {
	isos, errMsg := s.currentISOs(r)
	s.render(w, "images_page", s.withAuthFields(r, pageData{
		ISOs: isos, ISOFormError: errMsg,
		ActivePage: "images",
	}))
}

// handleNewVMPage serves the create-VM form page ("/vms/new"). A failed
// Nodes/ClusterISOs/Networks fetch isn't surfaced as an error here - the
// node picker already falls back to a free-text input when Nodes is
// empty (see new_vm.html), and an empty ISO/network picker just means
// "(none)" is the only option, both harmless degraded states rather
// than failures worth a banner. ClusterISOs (not the local-only ISOs)
// populates the image pickers so an operator can pick an image that's
// only present on some other node - internal/cluster's Reconciler
// fetches it automatically at provisioning time (ADR-0041) - and the
// page's own JS uses each row's MissingNodes to show a "will be fetched
// from a peer" cue as the Node ID picker changes.
func (s *Server) handleNewVMPage(w http.ResponseWriter, r *http.Request) {
	nodes, _ := s.knownNodes(r)
	clusterISOs, _ := s.currentClusterISOs(r)
	networks, _ := s.currentNetworks(r)
	s.render(w, "new_vm_page", s.withAuthFields(r, pageData{Nodes: nodes, ClusterISOs: clusterISOs, Networks: networks, ActivePage: "vms"}))
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
	s.renderVMRows(w, errMsg, vms, s.withAuthFields(r, pageData{}).CanOperate)
}

// renderVMRows renders the vm_rows fragment for a swap into #vm-rows -
// used by handleListVMs (htmx polling), handleDeleteVM, and
// renderRowsWithError. errMsg, when non-empty, is delivered via an
// HX-Trigger response header (a "vmError" event, picked up by vms.html's
// listener script to update the page's persistent #vm-error banner)
// rather than embedded in the response body - see vm_rows.html's own
// comment for why mixing an out-of-band <div> into a <tbody>-targeted
// response corrupted the table on every poll.
func (s *Server) renderVMRows(w http.ResponseWriter, errMsg string, vms []vmView, canOperate bool) {
	if errMsg != "" {
		if b, err := json.Marshal(map[string]string{"vmError": errMsg}); err == nil {
			w.Header().Set("HX-Trigger", string(b))
		}
	}
	s.render(w, "vm_rows", pageData{Error: errMsg, VMs: vms, CanOperate: canOperate})
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
			BaseImageName: r.FormValue("base_image_name"),
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
		if r.Header.Get("HX-Target") == "vm-detail-error" {
			s.renderCreateError(w, err.Error())
			return
		}
		s.renderRowsWithError(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		if r.Header.Get("HX-Target") == "vm-detail-error" {
			s.renderCreateError(w, resp.GetError())
			return
		}
		s.renderRowsWithError(w, r, resp.GetError())
		return
	}
	if r.Header.Get("HX-Target") == "vm-detail-error" {
		w.Header().Set("HX-Redirect", "/vms")
		return
	}

	sortBy, dir := parseSort(r)
	vms, errMsg := s.currentVMs(r, sortBy, dir)
	s.renderVMRows(w, errMsg, vms, s.withAuthFields(r, pageData{}).CanOperate)
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
	s.renderVMRows(w, msg, vms, s.withAuthFields(r, pageData{}).CanOperate)
}

// handleListISOs serves just the iso_rows fragment, for refreshing the
// Images table after an upload or delete without a full page reload -
// same pattern as handleListVMs/vm_rows.
func (s *Server) handleListISOs(w http.ResponseWriter, r *http.Request) {
	isos, errMsg := s.currentISOs(r)
	s.render(w, "iso_rows", s.withAuthFields(r, pageData{Error: errMsg, ISOs: isos}))
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
	s.render(w, "iso_panel", s.withAuthFields(r, pageData{ISOFormError: formErr, ISOFormSuccess: success, ISOs: isos}))
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
	s.render(w, "networks_page", s.withAuthFields(r, pageData{Networks: networks, NetworkFormError: errMsg, ActivePage: "networks"}))
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
	s.render(w, "network_panel", s.withAuthFields(r, pageData{NetworkFormError: formErr, Networks: networks}))
}

// currentJails fetches the current list of jails, returning an error
// string ("" on success) the same fail-soft convention currentNetworks
// follows.
func (s *Server) currentJails(r *http.Request) ([]jailView, string) {
	resp, err := s.client.ListJails(r.Context(), &rpcpb.ListJailsRequest{})
	if err != nil {
		return nil, err.Error()
	}
	if resp.GetError() != "" {
		return nil, resp.GetError()
	}
	jails := make([]jailView, 0, len(resp.GetJails()))
	for _, j := range resp.GetJails() {
		jails = append(jails, fromRPCJail(j))
	}
	return jails, ""
}

// handleJailsPage serves the Jails list/create page ("/jails").
func (s *Server) handleJailsPage(w http.ResponseWriter, r *http.Request) {
	jails, errMsg := s.currentJails(r)
	nodes, _ := s.knownNodes(r)
	s.render(w, "jails_page", s.withAuthFields(r, pageData{Jails: jails, Nodes: nodes, JailFormError: errMsg, ActivePage: "jails"}))
}

// handleNewJailPage serves the create-jail form page ("/jails/new"),
// mirroring handleNewVMPage's own separate-page pattern exactly - see
// ADR-0018 for why a create form lives on its own page rather than
// inline on the list.
func (s *Server) handleNewJailPage(w http.ResponseWriter, r *http.Request) {
	nodes, _ := s.knownNodes(r)
	s.render(w, "new_jail_page", s.withAuthFields(r, pageData{Nodes: nodes, ActivePage: "jails"}))
}

// handleCreateJail mirrors handleCreateVM exactly: redirect back to the
// list on success, render just the error message on failure - the
// list page never sees this response directly (the form lives on its
// own page now, not inline on the list).
func (s *Server) handleCreateJail(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderCreateError(w, "invalid form: "+err.Error())
		return
	}

	resp, err := s.client.CreateJail(r.Context(), &rpcpb.CreateJailRequest{
		Jail: &rpcpb.JailDefinition{
			Id:            r.FormValue("id"),
			Name:          r.FormValue("name"),
			Hostname:      r.FormValue("hostname"),
			NodeId:        r.FormValue("node_id"),
			ReplicaNodeId: r.FormValue("replica_node_id"),
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
	w.Header().Set("HX-Redirect", "/jails")
}

func (s *Server) handleDeleteJail(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.DeleteJail(r.Context(), &rpcpb.DeleteJailRequest{Id: r.PathValue("id")})
	if err != nil {
		s.renderJailPanelResult(w, r, err.Error())
		return
	}
	if resp.GetError() != "" {
		s.renderJailPanelResult(w, r, resp.GetError())
		return
	}
	s.renderJailPanelResult(w, r, "")
}

// renderJailPanelResult mirrors renderNetworkPanelResult's combined-
// target pattern.
func (s *Server) renderJailPanelResult(w http.ResponseWriter, r *http.Request, formErr string) {
	jails, fetchErr := s.currentJails(r)
	if fetchErr != "" {
		if formErr == "" {
			formErr = fetchErr
		} else {
			formErr += "; additionally failed to refresh list: " + fetchErr
		}
	}
	s.render(w, "jail_panel", s.withAuthFields(r, pageData{JailFormError: formErr, Jails: jails}))
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
	s.render(w, "apikeys_page", s.withAuthFields(r, pageData{APIKeys: keys, APIKeyFormError: errMsg, ActivePage: "apikeys"}))
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
	role := r.FormValue("role")
	resp, err := s.client.CreateAPIKey(r.Context(), &rpcpb.CreateAPIKeyRequest{Name: name, Role: role})
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

// currentUsers builds the Users page's row list from s.roleMap, sorted
// by username for deterministic output (roleMap's own iteration order
// is a plain Go map, unordered) - CanChange is computed once here per
// row against the acting session's own role, via canChangePassword
// (password.go), rather than re-derived in the template.
func (s *Server) currentUsers(actorRole manager.Role) []userView {
	usernames := make([]string, 0, len(s.roleMap))
	for u := range s.roleMap {
		usernames = append(usernames, u)
	}
	sort.Strings(usernames)

	users := make([]userView, 0, len(usernames))
	for _, u := range usernames {
		role := s.roleMap[u]
		users = append(users, userView{Username: u, Role: string(role), CanChange: canChangePassword(actorRole, role)})
	}
	return users
}

// handleUsersPage serves the Users page ("/users") - visible to every
// logged-in role (see routes()'s own doc comment), listing every known
// account with a per-row change-password action gated by
// canChangePassword.
func (s *Server) handleUsersPage(w http.ResponseWriter, r *http.Request) {
	info, ok := s.currentSession(r)
	if !ok {
		s.render(w, "users_page", s.withAuthFields(r, pageData{UserFormError: "no active session", ActivePage: "users"}))
		return
	}
	s.render(w, "users_page", s.withAuthFields(r, pageData{Users: s.currentUsers(info.role), ActivePage: "users"}))
}

// handleChangePassword implements the actual password change (ADR-0039).
// The route's own requireRole(RoleOperator, ...) already keeps Viewer
// out entirely; this handler re-derives the target's role from roleMap
// and re-checks the full canChangePassword rule, since Operator is let
// through by the route gate but must still be blocked from targeting
// Admin. The acting user's own current password is re-verified via
// s.auth (the same check login itself performs) before anything is
// actually changed - proof it's really them, not just an unattended
// open session.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	info, ok := s.currentSession(r)
	if !ok {
		s.renderUserPanelResult(w, r, "", manager.RoleViewer, "no active session")
		return
	}

	target := r.PathValue("username")
	targetRole, known := s.roleMap[target]
	if !known {
		s.renderUserPanelResult(w, r, info.username, info.role, fmt.Sprintf("unknown account %q", target))
		return
	}
	if !canChangePassword(info.role, targetRole) {
		s.renderUserPanelResult(w, r, info.username, info.role, fmt.Sprintf("this account's role does not permit changing %q's password", target))
		return
	}

	if err := r.ParseForm(); err != nil {
		s.renderUserPanelResult(w, r, info.username, info.role, "invalid form: "+err.Error())
		return
	}
	currentPassword := r.FormValue("current_password")
	newPassword := r.FormValue("new_password")
	confirmPassword := r.FormValue("confirm_password")

	if len(newPassword) < minPasswordLength {
		s.renderUserPanelResult(w, r, info.username, info.role, fmt.Sprintf("new password must be at least %d characters", minPasswordLength))
		return
	}
	if newPassword != confirmPassword {
		s.renderUserPanelResult(w, r, info.username, info.role, "new password and confirmation do not match")
		return
	}

	ok, err := s.auth.Authenticate(info.username, currentPassword)
	if err != nil {
		s.renderUserPanelResult(w, r, info.username, info.role, "authentication backend error: "+err.Error())
		return
	}
	if !ok {
		s.renderUserPanelResult(w, r, info.username, info.role, fmt.Sprintf("incorrect password for %q - enter *your own* login password here, not %q's", info.username, target))
		return
	}

	if err := s.passwords.SetPassword(target, newPassword); err != nil {
		s.renderUserPanelResult(w, r, info.username, info.role, "setting password: "+err.Error())
		return
	}
	s.renderUserPanelSuccess(w, info.username, info.role, fmt.Sprintf("password for %q changed successfully", target))
}

// renderUserPanelResult/renderUserPanelSuccess re-render the Users
// page's own list (recomputed against actorRole, so CanChange stays
// correct) alongside a result message - mirroring
// renderAPIKeyPanelResult's combined-target pattern. actorUsername
// flows through to the template so each row's own password field can
// be labeled unambiguously as "your own password, not <target>'s" -
// see users.html.
func (s *Server) renderUserPanelResult(w http.ResponseWriter, r *http.Request, actorUsername string, actorRole manager.Role, formErr string) {
	s.render(w, "user_panel", pageData{Users: s.currentUsers(actorRole), Username: actorUsername, UserFormError: formErr})
}

func (s *Server) renderUserPanelSuccess(w http.ResponseWriter, actorUsername string, actorRole manager.Role, msg string) {
	s.render(w, "user_panel", pageData{Users: s.currentUsers(actorRole), Username: actorUsername, UserFormSuccess: msg})
}

func (s *Server) render(w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}
