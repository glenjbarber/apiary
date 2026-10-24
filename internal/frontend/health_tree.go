package frontend

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/health"
)

const healthTreeStaleAfter = 30 * time.Second

type healthTreeNode struct {
	Key               string           `json:"key"`
	Label             string           `json:"label"`
	Kind              string           `json:"kind"`
	Level             int              `json:"level"`
	State             string           `json:"state"`
	Leaf              bool             `json:"leaf"`
	ObservedAtUnix    int64            `json:"observedAtUnix"`
	AgeSeconds        int64            `json:"ageSeconds"`
	DefaultExpanded   bool             `json:"defaultExpanded,omitempty"`
	InventoryComplete bool             `json:"inventoryComplete"`
	Children          []healthTreeNode `json:"children,omitempty"`
}

func (s *Server) handleClusterHealthJSON(w http.ResponseWriter, r *http.Request) {
	statusResp, err := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	if err != nil {
		http.Error(w, "cluster health is unavailable", http.StatusServiceUnavailable)
		return
	}
	localNodeID := statusResp.GetManagerNodeId()
	nodeIDs := statusResp.GetKnownNodeIds()
	if len(nodeIDs) == 0 && localNodeID != "" {
		nodeIDs = []string{localNodeID}
	}

	index := s.gatherCombCauseIndex(r.Context())
	now := time.Now()
	nodes := make([]clusterNodeView, len(nodeIDs))
	var wg sync.WaitGroup
	for i, id := range nodeIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			nodes[i] = s.clusterNodeEvidence(r.Context(), id, localNodeID, statusResp, index, now)
		}(i, id)
	}
	wg.Wait()
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(buildHealthTree(nodes, index, now)); err != nil {
		return
	}
}

func buildHealthTree(nodes []clusterNodeView, index *combCauseIndex, now time.Time) healthTreeNode {
	if index != nil {
		for _, nodeID := range resourceHostIDs(index) {
			found := false
			for _, host := range nodes {
				if sameTreeHost(nodeID, host.NodeID) {
					found = true
					break
				}
			}
			if !found {
				nodes = append(nodes, clusterNodeView{NodeID: nodeID, HealthStatus: health.StatusUnknown})
			}
		}
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
	}
	inventoryUnknown := index == nil || index.VMError != "" || index.JailError != ""
	children := make([]healthTreeNode, 0, len(nodes))
	for _, host := range nodes {
		state := hostTreeState(host, now)
		if inventoryUnknown && state == "HEALTHY" {
			state = "UNKNOWN"
		}
		hostNode := healthTreeNode{
			Key: "host:" + host.NodeID, Label: relativeHostName(host.NodeID), Kind: "host",
			Level: 1, State: state, ObservedAtUnix: host.ReconcileSuccessUnix,
		}
		if host.ReconcileSuccessUnix > 0 {
			age := now.Sub(time.Unix(host.ReconcileSuccessUnix, 0))
			if age >= 0 {
				hostNode.AgeSeconds = int64(age / time.Second)
			}
		}
		if index != nil {
			for _, vm := range index.VMs {
				if sameTreeHost(vm.NodeID, host.NodeID) {
					hostNode.Children = append(hostNode.Children, resourceTreeNode("vm", vm.ID, vm.Name, vm.DesiredState, vm.Phase, vm.ObservedAtUnix, host, now))
				}
			}
			for _, jail := range index.Jails {
				if sameTreeHost(jail.NodeID, host.NodeID) {
					hostNode.Children = append(hostNode.Children, resourceTreeNode("jail", jail.ID, jail.Name, jail.DesiredState, jail.Phase, jail.ObservedAtUnix, host, now))
				}
			}
		}
		sort.Slice(hostNode.Children, func(i, j int) bool {
			if hostNode.Children[i].Label == hostNode.Children[j].Label {
				return hostNode.Children[i].Key < hostNode.Children[j].Key
			}
			return hostNode.Children[i].Label < hostNode.Children[j].Label
		})
		hostNode.Leaf = len(hostNode.Children) == 0
		hostNode.State = aggregateHealthState(hostNode.State, hostNode.Children)
		children = append(children, hostNode)
	}
	root := healthTreeNode{
		Key: "cluster:lab3.apiary.work", Label: "lab3.apiary.work", Kind: "cluster",
		Level: 0, State: aggregateHealthState("HEALTHY", children), DefaultExpanded: true,
		Leaf: len(children) == 0, InventoryComplete: !inventoryUnknown, Children: children,
	}
	for _, child := range children {
		if child.ObservedAtUnix > root.ObservedAtUnix {
			root.ObservedAtUnix = child.ObservedAtUnix
		}
		for _, guest := range child.Children {
			if guest.ObservedAtUnix > root.ObservedAtUnix {
				root.ObservedAtUnix = guest.ObservedAtUnix
			}
		}
	}
	if root.ObservedAtUnix > 0 {
		age := now.Sub(time.Unix(root.ObservedAtUnix, 0))
		if age >= 0 {
			root.AgeSeconds = int64(age / time.Second)
		}
	}
	return root
}

func resourceHostIDs(index *combCauseIndex) []string {
	seen := make(map[string]struct{})
	var ids []string
	for _, vm := range index.VMs {
		if vm.NodeID != "" {
			if _, ok := seen[vm.NodeID]; !ok {
				seen[vm.NodeID] = struct{}{}
				ids = append(ids, vm.NodeID)
			}
		}
	}
	for _, jail := range index.Jails {
		if jail.NodeID != "" {
			if _, ok := seen[jail.NodeID]; !ok {
				seen[jail.NodeID] = struct{}{}
				ids = append(ids, jail.NodeID)
			}
		}
	}
	return ids
}

func hostTreeState(host clusterNodeView, now time.Time) string {
	if !host.Reachable {
		return "UNKNOWN"
	}
	switch host.HealthStatus {
	case health.StatusHealthy:
		if host.ReconcileSuccessUnix == 0 {
			return "UNKNOWN"
		}
		if age := now.Sub(time.Unix(host.ReconcileSuccessUnix, 0)); age < 0 {
			return "UNKNOWN"
		} else if age >= healthTreeStaleAfter {
			return "STALE"
		}
		return "HEALTHY"
	case health.StatusDegraded, health.StatusContradictory:
		return "DEGRADED"
	case health.StatusStale:
		return "STALE"
	default:
		return "UNKNOWN"
	}
}

func resourceTreeNode(kind, id, name, desired, phase string, observedAt int64, host clusterNodeView, now time.Time) healthTreeNode {
	if strings.TrimSpace(name) == "" {
		name = id
	}
	state := "UNKNOWN"
	if observedAt > 0 && host.Reachable {
		age := now.Sub(time.Unix(observedAt, 0))
		if age >= 0 {
			switch {
			case phase == "error":
				state = "DEGRADED"
			case desired == "stopped" && phase == "stopped":
				state = "STOPPED"
			case desired == "running" && phase == "ready":
				state = "RUNNING"
			case desired == "running" && phase == "stopped":
				state = "DOWN"
			default:
				state = "UNKNOWN"
			}
			if age >= healthTreeStaleAfter && (state == "RUNNING" || state == "STOPPED") {
				state = "STALE"
			}
		}
	}
	ageSeconds := int64(0)
	if observedAt > 0 {
		age := now.Sub(time.Unix(observedAt, 0))
		if age >= 0 {
			ageSeconds = int64(age / time.Second)
		}
	}
	return healthTreeNode{Key: kind + ":" + id, Label: name, Kind: kind, Level: 2, State: state, Leaf: true, ObservedAtUnix: observedAt, AgeSeconds: ageSeconds}
}

func aggregateHealthState(own string, children []healthTreeNode) string {
	unknown, stale, degraded := own == "UNKNOWN", own == "STALE", own == "DEGRADED"
	for _, child := range children {
		switch child.State {
		case "DOWN", "DEGRADED":
			degraded = true
		case "STALE":
			stale = true
		case "UNKNOWN":
			unknown = true
		}
	}
	switch {
	case degraded:
		return "DEGRADED"
	case stale:
		return "STALE"
	case unknown:
		return "UNKNOWN"
	default:
		return "HEALTHY"
	}
}

func relativeHostName(id string) string {
	const suffix = ".lab3.apiary.work"
	if strings.HasSuffix(strings.ToLower(id), suffix) {
		return id[:len(id)-len(suffix)]
	}
	return id
}

func sameTreeHost(resourceNodeID, hostNodeID string) bool {
	return strings.EqualFold(resourceNodeID, hostNodeID) || strings.EqualFold(relativeHostName(resourceNodeID), relativeHostName(hostNodeID))
}
