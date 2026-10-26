package frontend

import (
	"net/http"
	"sort"
	"strings"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// The command palette (handoff section 11).
//
// The palette is a way to START something, so its actions are
// role-filtered SERVER-SIDE. A Viewer is shown no mutating action at
// all - not a disabled one, and not one that fails when clicked. The
// palette is not a second, weaker copy of the route table's role
// checks: it asks the same question, in the same place.

// paletteAction is one action the palette can run.
type paletteAction struct {
	Label string
	Href  string
	// Detail is the faint right-hand text (the mockup's shortcut
	// hints are not required in this phase, so this is usually a
	// short description instead).
	Detail string
	// Operator marks an action only an Operator (or better) may run.
	Operator bool
	// Admin marks an action only an Admin may run.
	Admin bool
}

// paletteResult is one "Go to" result.
type paletteResult struct {
	Label  string
	Detail string
	Href   string
}

// paletteView is everything the palette's result list renders.
type paletteView struct {
	Actions []paletteAction
	Results []paletteResult
	// Query is what the operator typed.
	Query string
	// Empty is true when there is nothing to show at all.
	Empty bool
}

// paletteActions is the full, role-gated action list.
//
// EVERY ACTION HERE POINTS AT A PAGE OR FORM THAT ALREADY EXISTS. The
// handoff forbids new mutating endpoints in this phase, so the palette
// navigates to the page where the action lives rather than performing
// it directly.
func paletteActions() []paletteAction {
	return []paletteAction{
		{Label: "Create virtual machine", Href: "/create?kind=vm", Detail: "guided create", Operator: true},
		{Label: "Create jail", Href: "/create?kind=jail", Detail: "guided create", Operator: true},
		{Label: "Open console", Href: "/vms", Detail: "pick a cell", Operator: true},
		{Label: "Snapshot cell", Href: "/vms", Detail: "pick a cell", Operator: true},
		{Label: "Upload ISO image", Href: "/images", Detail: "image library", Operator: true},
		{Label: "Add comb", Href: "/machine#join-colony", Detail: "join a Colony", Operator: true},
		{Label: "Network configuration", Href: "/networks", Detail: "VLAN, bridge, DHCP", Operator: true},
		{Label: "Maintenance waves", Href: "/maintenance", Detail: "rehearse a failure"},
		{Label: "Pending join requests", Href: "/#pending-join-requests", Detail: "approve a join", Admin: true},
		{Label: "API keys", Href: "/apikeys", Detail: "issue or revoke", Admin: true},
	}
}

// paletteActionAllowed reports whether this session may run an action.
//
// The rule is the route table's own: an Operator action needs Operator
// or better, an Admin action needs Admin.
func paletteActionAllowed(action paletteAction, canOperate, canAdmin bool) bool {
	if action.Admin {
		return canAdmin
	}
	if action.Operator {
		return canOperate
	}
	return true
}

// paletteResults builds the palette's content for a query.
//
// The ACTIONS group is always shown - it is a short, stable list of
// things an operator can do, and an empty query showing nothing at all
// would make the palette look broken before the operator has typed.
//
// The "Go to" group is filtered by the query. An empty query shows the
// most useful destinations, so the palette is useful the moment it
// opens.
func (s *Server) paletteResults(r *http.Request, query string, canOperate, canAdmin bool) paletteView {
	view := paletteView{Query: query}

	for _, action := range paletteActions() {
		if !paletteActionAllowed(action, canOperate, canAdmin) {
			continue
		}
		if query != "" && !paletteMatches(query, action.Label, action.Detail) {
			continue
		}
		view.Actions = append(view.Actions, action)
	}

	for _, result := range s.paletteDestinations(r) {
		if query != "" && !paletteMatches(query, result.Label, result.Detail) {
			continue
		}
		view.Results = append(view.Results, result)
	}

	// A capped result list: the palette is a quick jump, not a
	// search-engine results page, and an unbounded list would push the
	// keyboard selection off the screen.
	//
	// THE CAP IS PER GROUP, NOT OVER THE WHOLE LIST. The static page
	// names come first and would otherwise fill every slot, so a
	// cell or a Comb would never appear in an unfiltered palette -
	// which is the palette's most useful state. Each group keeps its
	// own budget, so a search for a cell's name always finds it.
	view.Results = capPaletteResults(view.Results)
	view.Empty = len(view.Actions) == 0 && len(view.Results) == 0
	return view
}

// capPaletteResults trims a "Go to" list that is dominated by one
// KIND of destination.
//
// The list is sorted by label, so without this a Colony with many
// cells would push every Comb and every page name off the end. The
// budget is shared but no single kind may take more than half of it,
// so a cell search never loses the Combs and a page search never
// loses the cells.
func capPaletteResults(results []paletteResult) []paletteResult {
	if len(results) <= paletteMaxResults {
		return results
	}
	perKind := map[string]int{}
	out := make([]paletteResult, 0, paletteMaxResults)
	for _, result := range results {
		kind := paletteResultKind(result)
		if perKind[kind] >= paletteMaxResults/2 {
			continue
		}
		perKind[kind]++
		out = append(out, result)
		if len(out) == paletteMaxResults {
			break
		}
	}
	return out
}

// paletteResultKind names which KIND of destination a result is, so
// the cap can be applied per kind.
//
// The kinds are the handoff's own list of what the palette searches:
// cells, combs, networks, images, and page names. The cell kinds are
// matched on a PREFIX because a cell's detail names its kind and its
// Comb ("VM on brood"), which is more useful to a reader than a bare
// kind word.
func paletteResultKind(result paletteResult) string {
	switch {
	case result.Detail == "Comb":
		return "Comb"
	case strings.HasPrefix(result.Detail, "VM"):
		return "VM"
	case strings.HasPrefix(result.Detail, "Jail"):
		return "Jail"
	case result.Detail == "Network":
		return "Network"
	case result.Detail == "Image":
		return "Image"
	}
	return "Page"
}

// paletteMaxResults caps the "Go to" list.
const paletteMaxResults = 12

// paletteMatches is a case-insensitive substring match over the
// label and the detail.
func paletteMatches(query, label, detail string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return true
	}
	return strings.Contains(strings.ToLower(label), q) || strings.Contains(strings.ToLower(detail), q)
}

// paletteDestinations gathers everything the palette can jump to.
//
// The "Go to" group searches across cells, combs, networks, images,
// and page names - the handoff's list. Every one of those is a real
// list the palette reads here, fail-soft: a list that could not be
// read contributes no destinations rather than an empty one that looks
// like a Colony with no cells.
func (s *Server) paletteDestinations(r *http.Request) []paletteResult {
	var out []paletteResult

	// The page names. These are the static part of the navigation and
	// they cost nothing to list.
	for _, section := range navSections {
		for _, tab := range section.Tabs {
			if tab.Admin || tab.Operator {
				continue
			}
			out = append(out, paletteResult{Label: tab.Label, Detail: section.Label, Href: tab.Href})
		}
	}

	// The Combs. The page's own Comb tree already gathers membership
	// from the Status response; the palette reads the same thing.
	if status, err := s.client.Status(r.Context(), &rpcpb.StatusRequest{}); err == nil {
		for _, nodeID := range status.GetKnownNodeIds() {
			if nodeID == "" {
				continue
			}
			out = append(out, paletteResult{Label: nodeID, Detail: "Comb", Href: "/host/" + nodeID})
		}
	}

	// The cells.
	if resp, err := s.client.ListVMs(r.Context(), &rpcpb.ListVMsRequest{}); err == nil && resp.GetError() == "" {
		for _, vm := range resp.GetVms() {
			out = append(out, paletteResult{
				Label:  vm.GetId(),
				Detail: "VM on " + vm.GetNodeId(),
				Href:   "/vms/" + vm.GetId(),
			})
		}
	}
	if resp, err := s.client.ListJails(r.Context(), &rpcpb.ListJailsRequest{}); err == nil && resp.GetError() == "" {
		for _, jail := range resp.GetJails() {
			out = append(out, paletteResult{
				Label:  jail.GetId(),
				Detail: "Jail on " + jail.GetNodeId(),
				Href:   "/jails/" + jail.GetId(),
			})
		}
	}

	// The networks.
	if resp, err := s.client.ListNetworks(r.Context(), &rpcpb.ListNetworksRequest{}); err == nil && resp.GetError() == "" {
		for _, network := range resp.GetNetworks() {
			out = append(out, paletteResult{
				Label:  network.GetId(),
				Detail: "Network",
				Href:   "/networks",
			})
		}
	}

	// The images.
	if rows, err := s.currentClusterISOs(r); err == "" {
		for _, row := range rows {
			out = append(out, paletteResult{
				Label:  row.Name,
				Detail: "Image",
				Href:   "/images",
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}
