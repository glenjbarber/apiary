package frontend

import (
	"net/http"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// This file is the HTTP half of the guided creation flow: the one
// wizard page (web/templates/create_guided.html) that replaced the
// separate new-VM and new-Jail pages, plus the two validation calls
// its existing POST endpoints make before touching managerd. The rules
// themselves live in guided_create.go, so this file stays plumbing.
//
// Compatibility is the reason the flow is reached the way it is. The
// create endpoints were never renamed and the RPC contract never moved
// - CreateVM/CreateJail and POST /vms, POST /jails are exactly as they
// were, so every existing script, bookmark and test keeps working.
// /vms/new and /jails/new still exist and now render the same wizard
// with step 1 already answered, which is why vms.html, jails.html and
// cluster_overview.html needed no change.

// handleCreateGuidedPage serves the wizard at its own shared entry
// point (/create). The two kind-specific entry points are thin
// wrappers over renderGuidedCreatePage, so there is exactly one page
// implementation and no way for the two to drift apart.
func (s *Server) handleCreateGuidedPage(w http.ResponseWriter, r *http.Request) {
	s.renderGuidedCreatePage(w, r, parseGuidedKind(r.URL.Query().Get("kind")))
}

// renderGuidedCreatePage renders the wizard with kind pre-answered.
// An unrecognised or absent kind falls back to a VM rather than
// guessing: the wizard's own step 1 lets the operator change it, and
// the two legacy entry points (/vms/new, /jails/new) are the only
// callers that pass a kind explicitly.
//
// A failed Nodes/ClusterISOs/Networks fetch is deliberately not
// surfaced as a page error, matching what both create pages have
// always done: the node picker falls back to a free-text input when
// Nodes is empty, and an empty image or network picker just means
// "(none)" is the only choice - both degraded states, not failures.
func (s *Server) renderGuidedCreatePage(w http.ResponseWriter, r *http.Request, kind guidedKind) {
	if kind != guidedKindVM && kind != guidedKindJail {
		kind = guidedKindVM
	}

	var nodes []string
	var localNodeID string
	statusResp, statusErr := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	if statusErr == nil {
		nodes = statusResp.GetKnownNodeIds()
		localNodeID = statusResp.GetManagerNodeId()
	}
	clusterISOs, _ := s.currentClusterISOs(r)
	networks, _ := s.currentNetworks(r)

	// Clone sources are only offered on the VM path, and each one costs
	// a per-VM snapshot fetch, so they are not gathered for a jail.
	var cloneSources []cloneSourceView
	if kind == guidedKindVM {
		existingVMs, _ := s.currentVMs(r, "id", "asc")
		cloneSources = s.currentCloneSources(r, existingVMs)
	}

	activePage := "vms"
	if kind == guidedKindJail {
		activePage = "jails"
	}

	s.render(w, "create_guided_page", s.withAuthFieldsFrom(r, pageData{
		Nodes:          nodes,
		LocalNodeID:    localNodeID,
		ClusterISOs:    clusterISOs,
		Networks:       networks,
		PlacementHives: s.currentPlacementHives(r, nodes, localNodeID),
		CloneSources:   cloneSources,
		Guided: &guidedPageView{
			Kind:         kind,
			Action:       kind.createAction(),
			Installers:   guidedImageOptions(clusterISOs, guidedRoleBootMedia),
			BaseImages:   guidedImageOptions(clusterISOs, guidedRoleBaseImage),
			BaseArchives: guidedImageOptions(clusterISOs, guidedRoleBaseArchive),
		},
		ActivePage: activePage,
	}, statusResp, statusErr))
}

// guidedPageView is the wizard's own data, kept in one nested field
// rather than spread across pageData so it is obvious which fields
// belong to this page and which are shared with every other page.
type guidedPageView struct {
	// Kind is step 1's pre-answered choice, and Action the existing
	// POST endpoint this page submits to for it - the wizard changed
	// the form, not the RPC surface behind it.
	Kind   guidedKind
	Action string

	// Installers/BaseImages/BaseArchives are the three image pickers,
	// each already filtered by its own role so the page and the
	// server-side validators cannot disagree about which image may go
	// where. Every stored image is still listed; the ones that cannot
	// fill the role are rendered disabled with their reason, rather
	// than hidden - an operator who can see an image deserves to be
	// told why they cannot pick it here.
	Installers   []guidedImageOption
	BaseImages   []guidedImageOption
	BaseArchives []guidedImageOption
}

// validateGuidedCreateVM applies every VM availability rule to a
// parsed POST /vms, before any CreateVM RPC is made. Called by
// handleCreateVM; see guided_create.go for the rules and the code each
// one restates.
func (s *Server) validateGuidedCreateVM(r *http.Request) error {
	form := vmCreateFormFromRequest(r)
	return form.validateVMCreateForm(s.cloneSourceNodeID(r, form.CloneSourceVMID, form.CloneSnapshotName))
}

// validateGuidedCreateJail is validateGuidedCreateVM for POST /jails.
// The jail rules need nothing beyond the form itself, so there is no
// extra lookup to pay for.
func (s *Server) validateGuidedCreateJail(r *http.Request) error {
	return jailCreateFormFromRequest(r).validateJailCreateForm()
}

// placementUnavailable reports whether owner-node option hive should be
// rendered disabled for a create of kind, and is the one piece of
// placement evidence the wizard turns into a hard control rather than
// a line of prose.
//
// It is deliberately a *disabled control with a stated reason*, not a
// server-side rejection: this evidence is a live probe of one node's
// configuration (GetNodeConfig's bhyve/jail settings), and a stale,
// throttled or wrong probe must never be able to block a deliberate
// placement - internal/cluster re-checks the real capability at
// provisioning time and fails loudly there (ensureJail's own
// "jail provisioning is disabled" error; a VM on a node without bhyve
// simply never provisions). An unreachable node is never treated as
// incapable either: that is missing evidence, not evidence of
// absence, and the existing per-Comb list above the picker says so.
func placementUnavailable(hive placementHiveView, kind guidedKind) bool {
	if hive.ProbeError != "" {
		return false
	}
	if kind == guidedKindJail {
		return hive.JailKnown && !hive.JailCapable
	}
	return !hive.VMCapable
}
