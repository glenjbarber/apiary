package frontend

// The sidebar's Colony tree (ADR-0144's `CombTree`).
//
// WHY A TREE. ADR-0059's Follow-up removed Comb-as-sidebar-item
// because it had "no separate destination or useful operator action".
// `/host/{id}` now exists and did not when that Follow-up was
// written, so the objection no longer applies - and a Comb with a
// destination, a state chip, and the cells it owns is genuinely
// useful to have one click away.
//
// WHY THE STATE COMES FROM RAFT MEMBERSHIP, NOT THE COMMAND CENTER'S
// EVIDENCE. The obvious implementation - reuse `clusterNodeEvidence`,
// so the sidebar and the Command Center cannot disagree - costs one
// HostStats probe AND one peer Status per Comb on EVERY page view
// across the whole app.
//
// That cost is not acceptable for a sidebar, and the existing
// `KnownCombs` comment already says so: "that would cost one
// HostStats/GetNodeConfig probe per other Comb on every single page
// view across the whole app, not just the dashboard where that cost
// is already paid today".
//
// So the tree reads what the page render ALREADY fetches: the raft
// membership in the `StatusResponse` that every full-page render
// already makes for the Colony-leader indicator. Membership says
// which Combs exist and what their suffrage is, and it costs nothing
// extra.
//
// What that CANNOT say is a Comb's full health verdict - that needs
// the probes. So the tree's chip is a MEMBERSHIP chip, not a health
// verdict, and it says so. The page that answers "is this Comb
// healthy?" is `/host/{id}`, which is one click away and is where the
// real evidence lives.

import (
	"context"
	"sort"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// combTreeView is the sidebar's Colony tree: one entry per Comb, each
// with the cells it owns.
type combTreeView struct {
	// Combs is the tree, sorted by node ID.
	Combs []combTreeNodeView
	// Voters is the number of Comb that are raft voters.
	Voters int
}

// combTreeNodeView is one Comb in the tree.
type combTreeNodeView struct {
	// NodeID is the Comb's raft identity.
	NodeID string
	// Address is the Comb's raft bind address, for an operator
	// identifying it on the network.
	Address string
	// Suffrage is "Voter", "Nonvoter", "Staging" or "Unknown".
	//
	// It is a MEMBERSHIP fact, not a health verdict, and the template
	// renders it as such. "Unknown" is a real, live value - any raft
	// suffrage this build does not recognise - and must never be
	// rendered as though it were a Voter.
	Suffrage string
	// IsLeader marks the Comb that currently holds leadership.
	//
	// It is set only for an OBSERVED leader (ADR-0123): an unknown,
	// electing, or unobserved reading marks nothing, so the tree can
	// never imply a leader that was not seen.
	IsLeader bool
	// Cells are the workloads this Comb owns, sorted by type then ID.
	Cells []combTreeCellView
}

// combTreeCellView is one cell under a Comb.
type combTreeCellView struct {
	// Type is "vm" or "jail".
	Type string
	// ID is the cell's ID, which is unique only within its type -
	// which is why the type is carried alongside it.
	ID string
	// Name is the operator's name for the cell, if it has one.
	Name string
	// Href is the page that exists for this cell.
	Href string
}

// gatherCombTree builds the sidebar's Colony tree from what the page
// render already knows.
//
// It fails SOFT. A managerd that cannot reach raft renders no tree
// rather than a broken one, because a tree with a Comb in it that
// cannot be verified is worse than no tree.
func (s *Server) gatherCombTree(ctx context.Context, status *rpcpb.StatusResponse) combTreeView {
	view := combTreeView{}
	if status == nil {
		return view
	}
	// The leader is marked only when raft's state was ACTUALLY READ.
	// An unknown, electing, or unobserved reading marks nothing, so
	// the tree can never imply a leader that was not seen.
	leader := colonyLeaderFromStatus(status, nil, time.Now())
	leaderID := ""
	if leader.Observed && leader.State == colonyLeaderKnown {
		leaderID = leader.NodeID
	}

	// The cells each Comb owns, grouped by the Comb that owns them.
	//
	// ONE read of each list, shared across every Comb, so every
	// Comb's cells come from the same moment. Two reads would put
	// one Comb's cells from before a change and another's from after
	// it.
	cellsByComb := map[string][]combTreeCellView{}
	for _, vm := range s.listVMsForTree(ctx) {
		cellsByComb[vm.NodeID] = append(cellsByComb[vm.NodeID], combTreeCellView{
			Type: "vm",
			ID:   vm.ID,
			Name: vm.Name,
			Href: "/vms/" + vm.ID,
		})
	}
	for _, jail := range s.listJailsForTree(ctx) {
		cellsByComb[jail.NodeID] = append(cellsByComb[jail.NodeID], combTreeCellView{
			Type: "jail",
			ID:   jail.ID,
			Name: jail.Name,
			Href: "/jails/" + jail.ID,
		})
	}

	for _, member := range status.GetMembers() {
		nodeID := member.GetNodeId()
		if nodeID == "" {
			continue
		}
		entry := combTreeNodeView{
			NodeID:   nodeID,
			Address:  member.GetAddress(),
			Suffrage: member.GetSuffrage(),
			Cells:    cellsByComb[nodeID],
		}
		// A leader is marked only when it was OBSERVED, never when it
		// was merely inferred.
		if entry.Suffrage == "Voter" && leaderID != "" && nodeID == leaderID {
			entry.IsLeader = true
		}
		if entry.Suffrage == "Voter" {
			view.Voters++
		}
		sort.Slice(entry.Cells, func(i, j int) bool {
			if entry.Cells[i].Type != entry.Cells[j].Type {
				return entry.Cells[i].Type < entry.Cells[j].Type
			}
			return entry.Cells[i].ID < entry.Cells[j].ID
		})
		view.Combs = append(view.Combs, entry)
	}

	// A member list that is empty but a known-node list that is not
	// means the membership view is unavailable while the roster is
	// known. The roster still names Combs worth linking to, so they
	// are rendered - without a suffrage, because a Comb whose
	// suffrage was not observed must not be shown as a voter.
	if len(view.Combs) == 0 {
		for _, nodeID := range status.GetKnownNodeIds() {
			if nodeID == "" {
				continue
			}
			entry := combTreeNodeView{NodeID: nodeID, Cells: cellsByComb[nodeID]}
			sort.Slice(entry.Cells, func(i, j int) bool {
				if entry.Cells[i].Type != entry.Cells[j].Type {
					return entry.Cells[i].Type < entry.Cells[j].Type
				}
				return entry.Cells[i].ID < entry.Cells[j].ID
			})
			view.Combs = append(view.Combs, entry)
		}
	}
	sort.Slice(view.Combs, func(i, j int) bool { return view.Combs[i].NodeID < view.Combs[j].NodeID })
	return view
}

// treeCell is the minimal shape the tree needs from a cell.
type treeCell struct {
	ID     string
	NodeID string
	Name   string
}

// listVMsForTree reads the VM list for the tree, fail-soft.
func (s *Server) listVMsForTree(ctx context.Context) []treeCell {
	resp, err := s.client.ListVMs(ctx, &rpcpb.ListVMsRequest{})
	if err != nil || resp.GetError() != "" {
		return nil
	}
	out := make([]treeCell, 0, len(resp.GetVms()))
	for _, vm := range resp.GetVms() {
		out = append(out, treeCell{ID: vm.GetId(), NodeID: vm.GetNodeId(), Name: vm.GetName()})
	}
	return out
}

// listJailsForTree reads the jail list for the tree, fail-soft.
func (s *Server) listJailsForTree(ctx context.Context) []treeCell {
	resp, err := s.client.ListJails(ctx, &rpcpb.ListJailsRequest{})
	if err != nil || resp.GetError() != "" {
		return nil
	}
	out := make([]treeCell, 0, len(resp.GetJails()))
	for _, jail := range resp.GetJails() {
		out = append(out, treeCell{ID: jail.GetId(), NodeID: jail.GetNodeId(), Name: jail.GetName()})
	}
	return out
}
