package frontend

import (
	"net/http"
	"sort"
)

// sidebarTreeItem is one VM or jail row under a Comb in the sidebar's
// live resource tree.
type sidebarTreeItem struct {
	Kind string // "vm" or "jail" - selects the icon/link prefix in the template
	ID   string
	Name string

	// DotClass mirrors the existing badge color conventions (see
	// layout.html's .badge rules) so a single small status dot uses the
	// exact same color a reader already associates with that state from
	// every list/detail page, rather than inventing a second palette.
	DotClass string
}

// sidebarTreeNode is one Comb's section in the tree - present even with
// zero Cells, so an operator can see every known Comb at a glance, the
// same as the Colony overview's own topology list.
type sidebarTreeNode struct {
	NodeID string
	Items  []sidebarTreeItem
}

// vmPhaseDotClass/jailPhaseDotClass map a Cell's Phase to the same
// badge color classes layout.html already defines for status badges
// elsewhere (ready/running=success, error=danger, creating/deleting/
// pending=in-progress, stopped=muted) - see the .badge rules.
func vmPhaseDotClass(phase string) string {
	switch phase {
	case "ready":
		return "ready"
	case "error":
		return "error"
	case "creating", "deleting":
		return "creating"
	case "stopped":
		return "stopped"
	default:
		return "unknown"
	}
}

// buildSidebarTree groups every known Comb's VMs/jails for the sidebar
// tree - reuses currentVMs/currentJails/knownNodes exactly as the full
// /vms, /jails, and cluster overview pages already do, since VM/jail
// listing is already Colony-wide (ADR-0106), not scoped to the
// responding Comb.
func (s *Server) buildSidebarTree(r *http.Request) ([]sidebarTreeNode, string) {
	nodeIDs, err := s.knownNodes(r)
	if err != nil {
		return nil, err.Error()
	}
	localNodeID := s.currentLocalNodeID(r)
	if len(nodeIDs) == 0 && localNodeID != "" {
		nodeIDs = []string{localNodeID}
	}

	byNode := make(map[string][]sidebarTreeItem, len(nodeIDs))
	for _, id := range nodeIDs {
		byNode[id] = nil
	}

	vms, vmErr := s.currentVMs(r, "", "")
	if vmErr == "" {
		for _, vm := range vms {
			byNode[vm.NodeID] = append(byNode[vm.NodeID], sidebarTreeItem{
				Kind: "vm", ID: vm.ID, Name: vm.Name, DotClass: vmPhaseDotClass(vm.Phase),
			})
		}
	}

	jails, jailErr := s.currentJails(r)
	if jailErr == "" {
		for _, jail := range jails {
			byNode[jail.NodeID] = append(byNode[jail.NodeID], sidebarTreeItem{
				Kind: "jail", ID: jail.ID, Name: jail.Name, DotClass: vmPhaseDotClass(jail.Phase),
			})
		}
	}

	nodes := make([]sidebarTreeNode, 0, len(byNode))
	for id, items := range byNode {
		sort.Slice(items, func(i, j int) bool {
			if items[i].Kind != items[j].Kind {
				return items[i].Kind < items[j].Kind
			}
			return items[i].Name < items[j].Name
		})
		nodes = append(nodes, sidebarTreeNode{NodeID: id, Items: items})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })

	errMsg := vmErr
	if errMsg == "" {
		errMsg = jailErr
	}
	return nodes, errMsg
}

// handleSidebarTreePanel serves the sidebar's live resource tree as an
// htmx-loaded fragment ("GET /nav/tree") - kept independent of every
// other page's own render so adding this doesn't require touching each
// page handler's own data-fetching, and so it can refresh on its own
// interval without reloading the page around it.
func (s *Server) handleSidebarTreePanel(w http.ResponseWriter, r *http.Request) {
	nodes, errMsg := s.buildSidebarTree(r)
	s.render(w, "sidebar_tree", pageData{SidebarTreeNodes: nodes, SidebarTreeError: errMsg})
}
