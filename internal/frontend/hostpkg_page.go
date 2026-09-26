package frontend

// Host package inventory page (internal/hostpkg).
//
// # Why this page reads through an interface rather than a client call
//
// internal/hostpkg is a purely host-local package: it shells out to
// pkg/uname on the Comb it runs on and it is not replicated. Nothing in
// api/rpc/manager.proto carries a package inventory, and the current
// RPC surface has no way to ask a managerd for one, so this handler is
// written against a small injectable source instead of pretending to
// have a client call that does not exist.
//
// The default (unset) source renders the honest "could not be read"
// panel. It does not render an empty inventory, and it does not render
// a reassuring "everything is up to date" - an absent reading is
// reported as an absent reading, which is the same rule
// internal/cluster/simulate.go applies to replica reachability.
//
// # Integration still required (files this change deliberately does not
// touch)
//
//  1. api/rpc/manager.proto: a GetHostPackages RPC (request: node_id;
//     response: the base system, the catalogue observation, the
//     installed packages, the unknowns, observed_at). Plus the
//     generated stubs, via `buf generate`.
//
//  2. internal/manager/server.go: a handler that calls
//     hostpkg.NewCollector(...).Collect(ctx) and translates to proto.
//
//  3. cmd/frontend: wire the page's source to that client call, e.g.
//     SetHostPkgSource(managerdClientSource{client}).
//
//  4. internal/frontend/server.go: one line in routes() -
//     registerHostPkgRoutes(s) - which this file already provides.
//
// Steps 1-3 are a privilege-model decision as much as a plumbing one:
// this page's write half needs a root helper under managerd, and who
// may trigger it is not this change's to decide. See the report notes
// for the precise question.

import (
	"context"
	"net/http"
	"sync"

	"github.com/glenjbarber/apiary/internal/hostpkg"
	"github.com/glenjbarber/apiary/internal/manager"
)

// hostPkgSource supplies one Comb's package inventory.
//
// nodeID names the Comb being asked about. A source MUST answer only
// for the Comb it can actually read - the one it is running on - and
// MUST return an error for any other node, rather than forwarding a
// question it cannot answer or substituting a nearby host's reading.
// Reading another Comb's packages is a different RPC and a different
// trust decision, and inventing it here would be exactly the kind of
// "close enough" substitution this feature is built to avoid.
type hostPkgSource interface {
	HostPackageInventory(ctx context.Context, nodeID string) (hostpkg.Inventory, error)
}

// hostPkgSourceMu guards the process-wide source below.
var (
	hostPkgSourceMu sync.RWMutex
	hostPkgSourceV  hostPkgSource
)

// SetHostPkgSource wires the page's inventory source. Nil (the default)
// leaves the page reporting the inventory as unavailable.
//
// A process-wide value rather than a Server field is a deliberate
// compromise forced by ownership: Server's own struct lives in
// server.go, which this change does not own, so the seam is here. It is
// inert until something calls it, and calling it is the integration step
// described at the top of this file.
func SetHostPkgSource(src hostPkgSource) {
	hostPkgSourceMu.Lock()
	defer hostPkgSourceMu.Unlock()
	hostPkgSourceV = src
}

func currentHostPkgSource() hostPkgSource {
	hostPkgSourceMu.RLock()
	defer hostPkgSourceMu.RUnlock()
	return hostPkgSourceV
}

// hostPkgPageData is this page's template data. It embeds pageData
// rather than adding fields to it, so the shared nav, header and
// footer partials keep working unchanged while this page's own data
// stays in this file.
type hostPkgPageData struct {
	pageData
	HostPkg hostPkgView
}

// hostPkgView is the whole contract behind the page. Every field is a
// plain Go value, translated once here, so the template never reasons
// about evidence - only renders a verdict it was handed.
type hostPkgView struct {
	// NodeID is the Comb this reading came from.
	NodeID string

	// Headline is internal/hostpkg's single top-level answer, verbatim
	// ("up_to_date", "updates_available", or "unknown").
	Headline string

	// HeadlineBadge maps Headline onto layout.html's existing badge
	// vocabulary. Unknown gets the "unknown" badge and the muted colour,
	// never the green one.
	HeadlineBadge string

	// HeadlineLabel is a short sentence naming the headline in words.
	HeadlineLabel string

	// HeadlineExplanation says what the headline does and does not
	// cover.
	HeadlineExplanation string

	// HeadlineWarning is set only when the headline is unknown, naming
	// what that costs. It is separate from the explanation so the
	// template can render it in the error register.
	HeadlineWarning string

	ObservedAt    string
	Base          hostPkgBaseView
	Catalogue     hostPkgCatalogueView
	Ports         []hostPkgPackageRow
	OutdatedCount int
	UnknownCount  int
	Unknowns      []hostPkgUnknownView
	RawOutdated   string

	// SourceError is non-empty when no reading was possible at all. The
	// template renders it as an explicit failure panel.
	SourceError string
}

type hostPkgBaseView struct {
	OSRelease      string
	OSVersion      string
	PkgBasePackage bool
	Observed       bool
	Status         string
	Badge          string
	Detail         string
	StatusRaw      string
}

type hostPkgCatalogueView struct {
	State  string
	Badge  string
	Detail string
}

type hostPkgPackageRow struct {
	Name      string
	Origin    string
	Version   string
	Candidate string
	Status    string
	Badge     string
	Detail    string
}

type hostPkgUnknownView struct {
	Subject string
	Reason  string
}

// registerHostPkgRoutes adds the host package page to a Server's mux.
//
// It is a separate function rather than a few lines inlined in routes()
// so that the integration point is one obvious call, and so this file
// owns everything about the page except the one line in server.go that
// references it.
func registerHostPkgRoutes(s *Server) {
	// Viewer, not Operator: the page is pure observation and needs no
	// privilege. Any write half would be a separate, Admin-gated route
	// and a separate decision.
	s.mux.HandleFunc("GET /host/{id}/packages", s.requireRole(manager.RoleViewer, s.handleHostPkgPage))
}

// handleHostPkgPage serves the host package inventory for one Comb.
func (s *Server) handleHostPkgPage(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("id")
	view := hostPkgView{NodeID: nodeID}

	src := currentHostPkgSource()
	if src == nil {
		view.SourceError = "no host package inventory source is configured on this frontend. " +
			"internal/hostpkg reads packages on the Comb it runs on and is not replicated, and no " +
			"Apiary RPC currently carries a package inventory, so there is nothing to read here yet. " +
			"This is not a claim that the host is up to date."
		s.renderHostPkgPage(w, r, hostPkgPageData{HostPkg: view})
		return
	}

	inv, err := src.HostPackageInventory(r.Context(), nodeID)
	if err != nil {
		view.SourceError = "could not read the package inventory for " + nodeID + ": " + err.Error() +
			" - this Comb's packages are unobserved, which is not the same as up to date"
		s.renderHostPkgPage(w, r, hostPkgPageData{HostPkg: view})
		return
	}

	s.renderHostPkgPage(w, r, hostPkgPageData{HostPkg: hostPkgViewFrom(inv, nodeID)})
}

// renderHostPkgPage renders the page template with this page's own data
// type. s.render takes a bare pageData, which cannot carry these
// fields; embedding pageData here is what lets the shared partials keep
// resolving .Username/.ColonyLeader/&c. against a promoted field.
func (s *Server) renderHostPkgPage(w http.ResponseWriter, r *http.Request, data hostPkgPageData) {
	pd := s.withAuthFields(r, data.pageData)
	pd.ActivePage = "host_packages"
	data.pageData = pd
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "hostpkg_packages_page", data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}

// hostPkgObservedAtLayout is how the page renders a reading's age. It
// is a named constant so a test asserts on the format rather than
// re-deriving it, and so a change to the layout is a deliberate edit.
const hostPkgObservedAtLayout = "2006-01-02 15:04:05 MST"

// hostPkgViewFrom translates one inventory into the page's shapes. It is
// pure, so every verdict - including every unknown - is reachable by
// test without a host and without a live RPC.
func hostPkgViewFrom(inv hostpkg.Inventory, nodeID string) hostPkgView {
	view := hostPkgView{
		NodeID:        nodeID,
		ObservedAt:    inv.ObservedAt.Format(hostPkgObservedAtLayout),
		Base:          hostPkgBaseFrom(inv),
		Catalogue:     hostPkgCatalogueView{State: string(inv.Catalogue.State), Badge: catalogueBadge(inv.Catalogue.State), Detail: inv.Catalogue.Detail},
		Ports:         make([]hostPkgPackageRow, 0, len(inv.Ports)),
		OutdatedCount: inv.OutdatedCount(),
		UnknownCount:  inv.UnknownCount(),
		Unknowns:      make([]hostPkgUnknownView, 0, len(inv.Unknown)),
		RawOutdated:   inv.RawOutdatedOutput,
	}
	for _, u := range inv.Unknown {
		view.Unknowns = append(view.Unknowns, hostPkgUnknownView{Subject: u.Subject, Reason: u.Reason})
	}
	for _, p := range inv.Ports {
		view.Ports = append(view.Ports, hostPkgPackageRow{
			Name: p.Name, Origin: p.Origin, Version: p.Version, Candidate: p.Candidate,
			Status: string(p.UpdateStatus), Badge: updateBadge(p.UpdateStatus), Detail: p.Detail,
		})
	}
	view.Headline = string(inv.Headline())
	view.HeadlineBadge = headlineBadge(inv.Headline())
	view.HeadlineLabel, view.HeadlineExplanation, view.HeadlineWarning = headlineProse(inv)
	return view
}

func hostPkgBaseFrom(inv hostpkg.Inventory) hostPkgBaseView {
	return hostPkgBaseView{
		OSRelease:      inv.Base.OSRelease,
		OSVersion:      inv.Base.OSVersion,
		PkgBasePackage: inv.Base.PkgBasePackage,
		Observed:       inv.Base.Observed,
		Status:         string(inv.Base.UpdateStatus),
		Badge:          updateBadge(inv.Base.UpdateStatus),
		Detail:         inv.Base.Detail,
		StatusRaw:      inv.RawBaseVersionOutput,
	}
}

// headlineBadge maps the top-level answer onto layout.html's existing
// badge classes. The unknown case gets "unknown" and nothing else: it
// must never share a colour with the healthy case, and it must never
// borrow the red of the error case either, because "we could not check"
// is not an assertion that something is wrong.
func headlineBadge(h hostpkg.Headline) string {
	switch h {
	case hostpkg.HeadlineUpToDate:
		return "ready"
	case hostpkg.HeadlineUpdatesAvailable:
		return "degraded"
	default:
		return "unknown"
	}
}

func updateBadge(s hostpkg.UpdateStatus) string {
	switch s {
	case hostpkg.UpdateStatusCurrent:
		return "ready"
	case hostpkg.UpdateStatusOutdated:
		return "degraded"
	default:
		return "unknown"
	}
}

func catalogueBadge(s hostpkg.CatalogueState) string {
	switch s {
	case hostpkg.CatalogueFresh:
		return "ready"
	case hostpkg.CatalogueStale:
		return "degraded"
	default:
		return "unknown"
	}
}

// headlineProse renders the headline in words for a reader who should
// not have to know this codebase's vocabulary. The three cases are
// spelled out separately because the difference between them is the
// entire point of the page.
func headlineProse(inv hostpkg.Inventory) (label, explanation, warning string) {
	switch inv.Headline() {
	case hostpkg.HeadlineUpToDate:
		return "Up to date",
			"Every package on this Comb was positively read as current, and the local package catalogue is recent enough to support that claim.",
			""
	case hostpkg.HeadlineUpdatesAvailable:
		return "Updates available",
			"Every question this page asks was answered, and at least one package has a newer version available. See the table below.",
			""
	default:
		return "Package state could not be fully determined",
			"Part of this Comb's package state could not be established, so this page will not claim it is up to date. Anything not listed as an available update is unobserved, not confirmed current.",
			"This page is deliberately not reporting a clean host. The specific gaps are listed below."
	}
}
