package frontend

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

type topologyMapView struct {
	Networks   []topologyNetworkView
	Unassigned []topologyCellView
	Unmapped   []topologyCellView
	LoadError  string
	CombCount  int
	CellCount  int
}

type topologyNetworkView struct {
	ID        string
	Name      string
	Subnet    string
	VLAN      string
	Bridge    string
	Combs     []topologyCombView
	CellCount int
}

type topologyCombView struct {
	NodeID       string
	BridgeLabel  string
	BridgeClass  string
	BridgeDetail string
	Cells        []topologyCellView
}

type topologyCellView struct {
	Kind       string
	ID         string
	Name       string
	NodeID     string
	Phase      string
	PhaseClass string
	Address    string
	DetailURL  string
}

func (s *Server) handleTopologyPage(w http.ResponseWriter, r *http.Request) {
	status, statusErr := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	var nodeIDs []string
	localNodeID := ""
	if statusErr == nil {
		localNodeID = status.GetManagerNodeId()
		nodeIDs = append(nodeIDs, status.GetKnownNodeIds()...)
		if len(nodeIDs) == 0 && localNodeID != "" {
			nodeIDs = append(nodeIDs, localNodeID)
		}
	}

	networks, networkErr := s.currentNetworks(r)
	vms, vmErr := s.currentVMs(r, "", "")
	jails, jailErr := s.currentJails(r)
	view := buildTopologyMap(networks, vms, jails, nodeIDs)
	if statusErr != nil {
		view.LoadError = statusErr.Error()
	}
	for _, message := range []string{networkErr, vmErr, jailErr} {
		if message == "" {
			continue
		}
		if view.LoadError != "" {
			view.LoadError += "; "
		}
		view.LoadError += message
	}
	s.render(w, "topology_page", s.withAuthFieldsFrom(r, pageData{
		TopologyMap: view,
		ActivePage:  "topology",
	}, status, statusErr))
}

func buildTopologyMap(networks []networkView, vms []vmView, jails []jailView, nodeIDs []string) topologyMapView {
	sort.Strings(nodeIDs)
	known := make(map[string]struct{}, len(nodeIDs))
	uniqueNodeIDs := nodeIDs[:0]
	for _, id := range nodeIDs {
		if id == "" {
			continue
		}
		if _, exists := known[id]; exists {
			continue
		}
		known[id] = struct{}{}
		uniqueNodeIDs = append(uniqueNodeIDs, id)
	}
	nodeIDs = uniqueNodeIDs
	view := topologyMapView{CombCount: len(uniqueNodeIDs)}
	networkIndex := make(map[string]int, len(networks))
	for _, network := range networks {
		networkIndex[network.ID] = len(view.Networks)
		name := network.Name
		if name == "" {
			name = network.ID
		}
		vlan := "Untagged"
		if network.VLANID != 0 {
			vlan = "VLAN " + uintString(network.VLANID)
		}
		view.Networks = append(view.Networks, topologyNetworkView{
			ID: network.ID, Name: name, Subnet: network.Subnet,
			VLAN: vlan, Bridge: network.BridgeName,
		})
	}

	for i := range view.Networks {
		network := networks[i]
		statusByNode := make(map[string]string, len(network.BridgeStatuses))
		for _, status := range network.BridgeStatuses {
			statusByNode[status.NodeID] = status.Status
		}
		for _, nodeID := range nodeIDs {
			label, class := topologyBridgeStatus(statusByNode[nodeID])
			view.Networks[i].Combs = append(view.Networks[i].Combs, topologyCombView{
				NodeID: nodeID, BridgeLabel: label, BridgeClass: class,
				BridgeDetail: "Bridge status reported for this Comb's copy of the network.",
			})
		}
	}

	addCell := func(cell topologyCellView, networkID string) {
		view.CellCount++
		if networkID == "" {
			view.Unassigned = append(view.Unassigned, cell)
			return
		}
		networkIndex, ok := networkIndex[networkID]
		if !ok {
			view.Unmapped = append(view.Unmapped, cell)
			return
		}
		combIndex := -1
		for i := range view.Networks[networkIndex].Combs {
			if view.Networks[networkIndex].Combs[i].NodeID == cell.NodeID {
				combIndex = i
				break
			}
		}
		if combIndex < 0 {
			label, class := topologyBridgeStatus("")
			view.Networks[networkIndex].Combs = append(view.Networks[networkIndex].Combs, topologyCombView{
				NodeID: cell.NodeID, BridgeLabel: label, BridgeClass: class,
				BridgeDetail: "No bridge observation is available for this Comb.",
			})
			combIndex = len(view.Networks[networkIndex].Combs) - 1
		}
		view.Networks[networkIndex].Combs[combIndex].Cells = append(view.Networks[networkIndex].Combs[combIndex].Cells, cell)
		view.Networks[networkIndex].CellCount++
	}

	for _, vm := range vms {
		addCell(topologyCellView{
			Kind: "VM", ID: vm.ID, Name: topologyCellName(vm.Name, vm.ID), NodeID: vm.NodeID,
			Phase: vm.Phase, PhaseClass: topologyPhaseClass(vm.Phase), Address: vm.IPAddress,
			DetailURL: "/vms/" + url.PathEscape(vm.ID),
		}, vm.NetworkID)
	}
	for _, jail := range jails {
		addCell(topologyCellView{
			Kind: "Jail", ID: jail.ID, Name: topologyCellName(jail.Name, jail.ID), NodeID: jail.NodeID,
			Phase: jail.Phase, PhaseClass: topologyPhaseClass(jail.Phase), Address: jail.IPAddress,
			DetailURL: "/jails/" + url.PathEscape(jail.ID),
		}, jail.NetworkID)
	}
	for i := range view.Networks {
		sort.Slice(view.Networks[i].Combs, func(a, b int) bool { return view.Networks[i].Combs[a].NodeID < view.Networks[i].Combs[b].NodeID })
		for j := range view.Networks[i].Combs {
			sort.Slice(view.Networks[i].Combs[j].Cells, func(a, b int) bool {
				return view.Networks[i].Combs[j].Cells[a].Name < view.Networks[i].Combs[j].Cells[b].Name
			})
		}
	}
	sort.Slice(view.Unassigned, func(i, j int) bool { return view.Unassigned[i].Name < view.Unassigned[j].Name })
	sort.Slice(view.Unmapped, func(i, j int) bool { return view.Unmapped[i].Name < view.Unmapped[j].Name })
	return view
}

func topologyBridgeStatus(status string) (string, string) {
	switch strings.ToLower(status) {
	case "up":
		return "Bridge up", "healthy"
	case "down":
		return "Bridge down", "error"
	default:
		return "Bridge unknown", "unknown"
	}
}

func topologyPhaseClass(phase string) string {
	switch strings.ToLower(phase) {
	case "ready":
		return "healthy"
	case "error":
		return "error"
	case "creating", "deleting", "restarting", "pending":
		return "warn"
	default:
		return "unknown"
	}
}

func topologyCellName(name, id string) string {
	if strings.TrimSpace(name) == "" {
		return id
	}
	return name
}

func uintString(value uint32) string {
	return strconv.FormatUint(uint64(value), 10)
}
