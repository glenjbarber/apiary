package restshim

import (
	"net/http"
	"strings"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// ADR-0160's REST scrape representation.
//
// The ManagerService RPCs are the canonical typed contract. These two
// endpoints expose the SAME observations to an external monitoring
// system, which is what ADR-0161's Zabbix template consumes.
//
// They reuse the RPC's semantics and timestamps rather than
// re-deriving anything: a REST consumer and a ManagerService consumer
// must see the same numbers at the same moment, and a second
// implementation of the mapping would eventually disagree with the
// first.
//
// The two rules that shape every field here:
//
//  1. A MISSING OBSERVATION IS NOT ZERO. A measurement that could not
//     be taken is ABSENT from the JSON, not present as zero. A
//     monitoring system that reads "memory available: 0" during an
//     outage will page an operator about a healthy machine.
//
//  2. THE CALLER IS TOLD WHICH VOCABULARY IT IS READING.
//     known_statuses travels with every response, listing the health
//     states this build recognizes. A consumer that meets a state it
//     does not know must treat it as NOT healthy - the same obligation
//     ADR-0122 puts on the health endpoint, and the same failure
//     Evidence-Aware Health exists to prevent.

// monitoringHealth is the REST shape of one health verdict.
//
// available is separate from status for the same reason it is on the
// wire: an UNAVAILABLE row has no verdict at all, where an UNKNOWN row
// has a verdict that says nothing is known.
type monitoringHealth struct {
	Status string `json:"status"`
	// observed_unix is when the underlying observation was taken. It is
	// 0 when there was none, which is what tells a consumer the row is
	// stale rather than merely old.
	ObservedUnix int64 `json:"observed_unix"`
	// available is false when the observation could not be taken.
	Available bool `json:"available"`
	// detail explains an unavailable or unknown row for a human.
	Detail string `json:"detail,omitempty"`
}

// monitoringNode is the REST shape of one Comb's monitoring summary.
//
// Every measurement is OPTIONAL, and its absence means "not observed",
// never "zero".
type monitoringNode struct {
	NodeID string           `json:"node_id"`
	Health monitoringHealth `json:"health"`
	// cpu, mem, pools, disks and net are omitted entirely when the
	// source could not be read.
	CPU   *monitoringCPU   `json:"cpu,omitempty"`
	Mem   *monitoringMem   `json:"mem,omitempty"`
	Pools []monitoringPool `json:"pools,omitempty"`
	Disks []monitoringDisk `json:"disks,omitempty"`
	Net   []monitoringNet  `json:"net,omitempty"`
	// raft_role and raft_applied_index are empty or zero when they
	// could not be read.
	RaftRole         string `json:"raft_role,omitempty"`
	RaftAppliedIndex uint64 `json:"raft_applied_index,omitempty"`
	// unavailable names the subsystems that could not be reported.
	Unavailable []string `json:"unavailable,omitempty"`
}

type monitoringCPU struct {
	Cores     int32   `json:"cores"`
	LoadAvg1  float64 `json:"load_avg_1"`
	LoadAvg5  float64 `json:"load_avg_5"`
	LoadAvg15 float64 `json:"load_avg_15"`
}

type monitoringMem struct {
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
}

type monitoringPool struct {
	Name        string `json:"name"`
	SizeBytes   uint64 `json:"size_bytes"`
	AllocBytes  uint64 `json:"alloc_bytes"`
	FreeBytes   uint64 `json:"free_bytes"`
	CapacityPct uint32 `json:"capacity_pct"`
	Health      string `json:"health"`
}

type monitoringDisk struct {
	Name    string `json:"name"`
	Model   string `json:"model,omitempty"`
	Serial  string `json:"serial,omitempty"`
	Healthy bool   `json:"healthy"`
	// error is set when the SMART query failed - healthy is
	// meaningless in that case, not a false "known bad" signal.
	Error string `json:"error,omitempty"`
}

type monitoringNet struct {
	Name    string `json:"name"`
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
	Up      bool   `json:"up"`
}

// monitoringCluster is the REST shape of the cluster-wide monitoring
// view.
type monitoringCluster struct {
	Error         string           `json:"error,omitempty"`
	LocalNodeID   string           `json:"local_node_id"`
	KnownStatuses []string         `json:"known_statuses"`
	Health        monitoringHealth `json:"health"`
	Nodes         []monitoringNode `json:"nodes"`
}

// monitoringNodes is the REST shape of the per-node monitoring view.
type monitoringNodes struct {
	Error         string           `json:"error,omitempty"`
	LocalNodeID   string           `json:"local_node_id"`
	KnownStatuses []string         `json:"known_statuses"`
	Nodes         []monitoringNode `json:"nodes"`
}

// monitoringFailedSubsystems reads the unavailable list and returns the
// set of subsystems that failed.
//
// managerd names each failure "<subsystem>: <reason>", where the
// subsystem is one of cpu, memory, zfs, disks, net or pf. The prefix is
// what identifies the subsystem, so it is the part this reads.
//
// An entry with no recognised prefix is still reported in the row's
// unavailable list, but it does not suppress any measurement - nothing
// here knows which measurement it was about, and suppressing the wrong
// one would be worse than leaving it in.
func monitoringFailedSubsystems(unavailable []string) map[string]bool {
	failed := make(map[string]bool, len(unavailable))
	for _, entry := range unavailable {
		prefix, _, found := strings.Cut(entry, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(prefix) {
		case "cpu", "memory", "zfs", "disks", "net", "pf":
			failed[strings.TrimSpace(prefix)] = true
		}
	}
	return failed
}

// fromRPCMonitoringHealth renders one health verdict.
func fromRPCMonitoringHealth(h *rpcpb.MonitoringHealthStatus) monitoringHealth {
	if h == nil {
		return monitoringHealth{Available: false}
	}
	return monitoringHealth{
		Status:       h.GetStatus(),
		ObservedUnix: h.GetObservedUnix(),
		Available:    h.GetAvailable(),
		Detail:       h.GetDetail(),
	}
}

// fromRPCMonitoringNode renders one Comb's monitoring row.
//
// A measurement that was not observed is OMITTED, not rendered as a
// zero-valued object. That distinction is the whole point of the
// shape.
func fromRPCMonitoringNode(n *rpcpb.MonitoringNodeReport) monitoringNode {
	out := monitoringNode{
		NodeID:           n.GetNodeId(),
		Health:           fromRPCMonitoringHealth(n.GetHealth()),
		RaftRole:         n.GetRaftRole(),
		RaftAppliedIndex: n.GetRaftAppliedIndex(),
		Unavailable:      n.GetUnavailable(),
	}
	// A measurement that was NOT TAKEN is omitted below, even when
	// managerd sent a zero-valued struct for it. See the note at the
	// memory branch.
	failed := monitoringFailedSubsystems(n.GetUnavailable())
	if c := n.GetCpu(); c != nil && !failed["cpu"] {
		out.CPU = &monitoringCPU{
			Cores:     c.GetCores(),
			LoadAvg1:  c.GetLoadAvg_1(),
			LoadAvg5:  c.GetLoadAvg_5(),
			LoadAvg15: c.GetLoadAvg_15(),
		}
	}
	// A measurement that was NOT TAKEN is omitted here, even when
	// managerd sent a zero-valued struct for it.
	//
	// This is not paranoia: managerd's HostStats always populates Mem,
	// so a memory reading that failed arrives as a MemStats of zeros. A
	// REST consumer that renders that as "memory available: 0" will
	// page an operator about a healthy machine during an outage.
	//
	if m := n.GetMem(); m != nil && !failed["memory"] {
		out.Mem = &monitoringMem{TotalBytes: m.GetTotalBytes(), FreeBytes: m.GetFreeBytes()}
	}
	if pools := n.GetPools(); len(pools) > 0 && !failed["zfs"] {
		out.Pools = make([]monitoringPool, 0, len(pools))
		for _, p := range pools {
			out.Pools = append(out.Pools, monitoringPool{
				Name: p.GetName(), SizeBytes: p.GetSizeBytes(), AllocBytes: p.GetAllocBytes(),
				FreeBytes: p.GetFreeBytes(), CapacityPct: p.GetCapacityPct(), Health: p.GetHealth(),
			})
		}
	}
	if disks := n.GetDisks(); len(disks) > 0 && !failed["disks"] {
		out.Disks = make([]monitoringDisk, 0, len(disks))
		for _, d := range disks {
			out.Disks = append(out.Disks, monitoringDisk{
				Name: d.GetName(), Model: d.GetModel(), Serial: d.GetSerial(),
				Healthy: d.GetHealthy(), Error: d.GetError(),
			})
		}
	}
	if net := n.GetNet(); len(net) > 0 && !failed["net"] {
		out.Net = make([]monitoringNet, 0, len(net))
		for _, i := range net {
			out.Net = append(out.Net, monitoringNet{
				Name: i.GetName(), RxBytes: i.GetRxBytes(), TxBytes: i.GetTxBytes(), Up: i.GetUp(),
			})
		}
	}
	return out
}

// fromRPCMonitoringNodes renders the per-node rows.
func fromRPCMonitoringNodes(nodes []*rpcpb.MonitoringNodeReport) []monitoringNode {
	out := make([]monitoringNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, fromRPCMonitoringNode(n))
	}
	return out
}

// handleMonitoringCluster implements GET /v1/monitoring/health.
//
// It is a pass-through: managerd's verdict and rows are rendered
// without reinterpretation.
//
// The in-band Error is deliberately NOT an error status code, for the
// same reason clusterHealth's is not: the RPC succeeded and the rows
// below it are real observations, merely capped at unknown because
// raft membership could not be read. Turning that into a 4xx would
// discard the very rows that make the failure legible.
func (s *Server) handleMonitoringCluster(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.GetMonitoringCluster(authContext(r), &rpcpb.GetMonitoringClusterRequest{})
	if err != nil {
		s.writeUpstream(w, err)
		return
	}
	writeJSON(w, http.StatusOK, monitoringCluster{
		Error:         resp.GetError(),
		LocalNodeID:   resp.GetLocalNodeId(),
		KnownStatuses: knownHealthStatuses,
		Health:        fromRPCMonitoringHealth(resp.GetHealth()),
		Nodes:         fromRPCMonitoringNodes(resp.GetNodes()),
	})
}

// handleMonitoringNodes implements GET /v1/monitoring/nodes.
func (s *Server) handleMonitoringNodes(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.GetMonitoringNodes(authContext(r), &rpcpb.GetMonitoringNodesRequest{})
	if err != nil {
		s.writeUpstream(w, err)
		return
	}
	writeJSON(w, http.StatusOK, monitoringNodes{
		Error:         resp.GetError(),
		LocalNodeID:   resp.GetLocalNodeId(),
		KnownStatuses: knownHealthStatuses,
		Nodes:         fromRPCMonitoringNodes(resp.GetNodes()),
	})
}
