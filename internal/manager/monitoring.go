package manager

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/health"
)

// ADR-0160's monitoring report RPCs.
//
// These are SIBLINGS of ClusterHealth, not an extension of it.
// ClusterHealth keeps its focused evidence-verdict contract, and several
// consumers already read it for unrelated reasons; a monitoring report
// carries measurements and timestamps instead. Expanding ClusterHealth
// would change the meaning and payload of an established API.
//
// The shape of the whole thing, in one paragraph:
//
// Monitoring is PULL. A caller asks for current observations when it
// wants them, and Apiary never pushes reports to a leader or an
// external service. Monitoring observations are NOT raft-replicated -
// each node's physical observations stay local - so a cluster report
// fans out to peers the way ClusterHealth does, with per-node
// timestamps and explicit unavailable, stale, or unknown states.
//
// The two rules that matter most, and that everything below exists to
// honour:
//
//  1. A MISSING OBSERVATION IS NOT ZERO. A source that could not be
//     read is absent from the report, and the row says so explicitly.
//     Converting "I do not know" into "zero" is how a monitoring system
//     reports a healthy cluster during an outage.
//
//  2. A ROW SAYS WHEN IT WAS OBSERVED. Every row carries the timestamp
//     of the underlying observation, so a consumer can tell a fresh
//     reading from a stale one without guessing from its own clock.

// monitoringObservedUnix is the timestamp stamped on an observation
// that was actually taken.
//
// It is the observation time, not the response time: a consumer polling
// every sixty seconds must be able to tell that the value it is looking
// at is from the previous poll, and a response timestamp would hide
// that.
func monitoringObservedUnix(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.Unix()
}

// monitoringStatus builds the health-status field of a monitoring row.
//
// The five-state vocabulary is internal/health's own, and it is carried
// as a string for the reason ClusterNodeHealth.status is: a newer
// managerd may compute a state an older consumer has never heard of,
// and the string contract cannot enforce anything on the consumer's
// behalf.
//
// available is separate from status on purpose. An UNAVAILABLE row has
// no verdict at all - nothing was observed - where an UNKNOWN row has a
// verdict that says nothing is known. Collapsing them would lose the
// difference between "this Comb is down" and "this Comb is up and
// cannot tell us anything".
func monitoringStatus(status health.Status, available bool, observed time.Time, detail string) *rpcpb.MonitoringHealthStatus {
	if !available {
		return &rpcpb.MonitoringHealthStatus{
			Status:       "",
			ObservedUnix: 0,
			Available:    false,
			Detail:       detail,
		}
	}
	return &rpcpb.MonitoringHealthStatus{
		Status:       string(status),
		ObservedUnix: monitoringObservedUnix(observed),
		Available:    true,
		Detail:       detail,
	}
}

// monitoringNodeReportFromStats builds a node's monitoring row from a
// HostStats response.
//
// The HostStats response is the source of every MEASUREMENT here. It is
// already a best-effort snapshot - a subsystem that failed to report is
// recorded in its Errors list rather than blanking the rest - so
// reusing it rather than gathering a second time is what keeps a
// monitoring report from disagreeing with the health page about the same
// node at the same moment.
//
// The health VERDICT is not taken from HostStats, because HostStats
// carries no verdict. It comes from the cluster health machinery, which
// is the one place that knows the evidence.
func monitoringNodeReportFromStats(nodeID string, stats *rpcpb.HostStatsResponse, verdict *rpcpb.MonitoringHealthStatus) *rpcpb.MonitoringNodeReport {
	report := &rpcpb.MonitoringNodeReport{NodeId: nodeID, Health: verdict}

	if stats == nil {
		// No stats at all. The row is still present - a node that
		// could not be measured is still a node - and it says so.
		if report.Health == nil {
			report.Health = monitoringStatus(health.StatusUnknown, false, time.Time{}, "this node's measurements could not be read")
		}
		report.Unavailable = []string{"host statistics could not be read from this node"}
		return report
	}

	// A MEASUREMENT THAT WAS NOT TAKEN IS ABSENT, NOT ZERO.
	//
	// This needs saying because HostStats cannot express the
	// difference on its own: it always populates Mem, so a memory
	// reading that failed to be taken arrives as a MemStats of zeros -
	// indistinguishable, to a reader of HostStats, from a machine with
	// no memory at all.
	//
	// What DOES carry the difference is HostStats.errors, which names
	// each subsystem that failed ("memory: ...", "cpu: ...", "zfs:
	// ..."). So a measurement is included only when its subsystem did
	// not fail.
	//
	// This matters because a monitoring system that reads "memory
	// available: 0" during an outage will page an operator about a
	// healthy machine. The whole point of a monitoring report is to
	// tell the difference between "zero" and "I do not know".
	failed := monitoringFailedSubsystems(stats.GetErrors())

	if stats.GetCpu() != nil && !failed["cpu"] {
		report.Cpu = stats.GetCpu()
	}
	if stats.GetMem() != nil && !failed["memory"] {
		report.Mem = stats.GetMem()
	}
	if len(stats.GetPools()) > 0 && !failed["zfs"] {
		report.Pools = stats.GetPools()
	}
	if len(stats.GetDisks()) > 0 && !failed["disks"] {
		report.Disks = stats.GetDisks()
	}
	if len(stats.GetNet()) > 0 && !failed["net"] {
		report.Net = stats.GetNet()
	}
	// The subsystems that failed are named, not zeroed. A consumer
	// reading a present field is reading a real observation; a consumer
	// reading an absent one knows the answer is not known.
	report.Unavailable = stats.GetErrors()
	return report
}

// monitoringFailedSubsystems reads HostStats.errors and returns the set
// of subsystems that failed.
//
// hoststats records each failure as "<subsystem>: <reason>", where the
// subsystem is one of cpu, memory, zfs, disks, net or pf. The prefix is
// what identifies the subsystem, so it is the part this reads.
//
// An error string with no recognised prefix is still reported - it
// appears in the row's unavailable list - but it does not suppress any
// measurement, because nothing here knows which measurement it was
// about. Suppressing the wrong one would be worse than leaving it in.
func monitoringFailedSubsystems(errs []string) map[string]bool {
	failed := make(map[string]bool, len(errs))
	for _, err := range errs {
		prefix, _, found := strings.Cut(err, ":")
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

// gatherMonitoringNodes gathers every Comb's monitoring row.
//
// It is the shared body of the two RPCs, so the per-node view and the
// cluster view can never disagree: the cluster view is the per-node
// rows plus a verdict computed from the same evidence.
//
// The fan-out is concurrent and bounded by the existing
// reachabilityCheckTimeout, exactly as ClusterHealth's is. Each peer
// probe gets its own context, for the reason ClusterHealth's does: a
// shared budget meant a slow-but-alive peer spent all of it on one
// call and handed the other an already-expired context.
func (s *Server) gatherMonitoringNodes(ctx context.Context) ([]*rpcpb.MonitoringNodeReport, string) {
	now := time.Now()
	anchor, err := s.Status(ctx, &rpcpb.StatusRequest{})
	if err != nil {
		return nil, "this node's own status could not be read, so the cluster's node set is unknown"
	}

	// A raftd that answered but failed its own configuration read has
	// not observed membership - it has only answered. Treating that as
	// observed would compute the report against an empty voter set and
	// present it as the Colony.
	membershipObserved := anchor.GetRaftReachable() && anchor.GetRaftMembershipError() == ""

	nodeIDs := anchor.GetKnownNodeIds()
	if len(nodeIDs) == 0 && s.nodeID != "" {
		nodeIDs = []string{s.nodeID}
	}

	memberByNode := make(map[string]*rpcpb.RaftMember, len(anchor.GetMembers()))
	for _, member := range anchor.GetMembers() {
		memberByNode[member.GetNodeId()] = member
	}

	// The health verdicts come from the SAME machinery ClusterHealth
	// uses, so a monitoring report and the health page cannot disagree
	// about a node's state. It is computed once, here, rather than
	// per-RPC, because both RPCs need it.
	healthByNode := s.monitoringHealthVerdicts(ctx, nodeIDs, anchor, memberByNode, membershipObserved, now)

	type nodeResult struct {
		report *rpcpb.MonitoringNodeReport
	}
	results := make([]nodeResult, len(nodeIDs))
	var wg sync.WaitGroup
	for i, nodeID := range nodeIDs {
		wg.Add(1)
		go func(i int, nodeID string) {
			defer wg.Done()
			stats, detail := s.monitoringStatsFor(ctx, nodeID, memberByNode, membershipObserved)
			verdict := healthByNode[nodeID]
			if verdict == nil {
				verdict = monitoringStatus(health.StatusUnknown, false, time.Time{}, detail)
			} else if detail != "" && !verdict.GetAvailable() {
				verdict.Detail = detail
			}
			results[i] = nodeResult{report: monitoringNodeReportFromStats(nodeID, stats, verdict)}
		}(i, nodeID)
	}
	wg.Wait()

	reports := make([]*rpcpb.MonitoringNodeReport, 0, len(results))
	for _, result := range results {
		if result.report != nil {
			reports = append(reports, result.report)
		}
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].GetNodeId() < reports[j].GetNodeId() })

	clusterErr := ""
	if !membershipObserved {
		clusterErr = "raft membership could not be read from this node - every Comb's verdict is capped at unknown"
	}
	return reports, clusterErr
}

// monitoringStatsFor reads one Comb's host statistics, locally or from
// the peer.
//
// It returns the stats and, when there were none, a one-sentence
// reason. The reason never contains a credential.
func (s *Server) monitoringStatsFor(ctx context.Context, nodeID string, memberByNode map[string]*rpcpb.RaftMember, membershipObserved bool) (*rpcpb.HostStatsResponse, string) {
	if nodeID == s.nodeID {
		// No dial is needed or wanted: this node is answering now, and
		// self-dialing would be a pointless extra hop.
		stats, err := s.HostStats(ctx, &rpcpb.HostStatsRequest{})
		if err != nil {
			return nil, "this node's own host statistics could not be read: " + err.Error()
		}
		return stats, ""
	}

	if s.peers == nil || !membershipObserved {
		return nil, "this Comb's host statistics could not be read: no peer forwarding is configured, or raft membership could not be read"
	}
	member, known := memberByNode[nodeID]
	if !known || member.GetAddress() == "" {
		return nil, "this Comb's host statistics could not be read: its address is not known from raft membership"
	}

	addr := s.peerManagerdAddr(member.GetAddress())
	statsCtx, statsCancel := context.WithTimeout(ctx, reachabilityCheckTimeout)
	defer statsCancel()
	stats, err := s.peers.HostStats(statsCtx, addr)
	if err != nil {
		return nil, "this Comb's host statistics could not be read: " + err.Error()
	}
	if stats == nil {
		return nil, "this Comb's host statistics could not be read: the peer returned no response"
	}
	return stats, ""
}

// monitoringHealthVerdicts computes each Comb's health verdict, using
// the same machinery ClusterHealth uses.
//
// It is asked of internal/health rather than reimplemented here, because
// a monitoring report and the health page MUST agree about a node's
// state. Two implementations of the same aggregation would eventually
// disagree, and a monitoring system that contradicts the UI it is
// supposed to mirror is worse than one that has no opinion.
func (s *Server) monitoringHealthVerdicts(ctx context.Context, nodeIDs []string, anchor *rpcpb.StatusResponse, memberByNode map[string]*rpcpb.RaftMember, membershipObserved bool, now time.Time) map[string]*rpcpb.MonitoringHealthStatus {
	verdicts := make(map[string]*rpcpb.MonitoringHealthStatus, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		// The anchor is this node's own Status, and it is passed through
		// rather than a fresh read: it is the same anchor the node set
		// was derived from, so the membership the report is built on and
		// the reachability the verdict is computed from are the SAME
		// observation. A nil anchor here would make every local node
		// look like its own raft was down.
		dialed, verdict, _, _, _ := s.clusterNodeHealth(ctx, nodeID, s.nodeID, anchor, membershipObserved, memberByNode, now)
		status := monitoringStatus(verdict.Status, true, now, verdict.Explanation)
		if !dialed {
			// The node could not be reached. That is UNAVAILABLE, not
			// an unknown verdict: there is no observation at all, which
			// is a different thing from a verdict that says nothing is
			// known.
			status = monitoringStatus(health.StatusUnknown, false, time.Time{}, verdict.Explanation)
		}
		verdicts[nodeID] = status
	}
	return verdicts
}

// monitoringClusterVerdict reduces the per-node rows to one verdict for
// the Colony.
//
// It uses internal/health's AggregateStatus, which is the SAME
// implementation the Command Center's health tree uses. A monitoring
// report and the health page therefore cannot disagree about the
// Colony's state, which is the whole reason the rule lives in one
// place rather than two.
//
// A row that could not be observed at all counts as unknown here, not as
// healthy: an unreachable Comb is not evidence of a healthy Colony.
func (s *Server) monitoringClusterVerdict(reports []*rpcpb.MonitoringNodeReport) *rpcpb.MonitoringHealthStatus {
	statuses := make([]health.Status, 0, len(reports))
	observed := time.Time{}
	for _, report := range reports {
		if report.GetHealth() == nil || !report.GetHealth().GetAvailable() {
			statuses = append(statuses, health.StatusUnknown)
			continue
		}
		statuses = append(statuses, health.Status(report.GetHealth().GetStatus()))
		if at := report.GetHealth().GetObservedUnix(); at > 0 {
			if t := time.Unix(at, 0); observed.IsZero() || t.Before(observed) {
				observed = t
			}
		}
	}
	return monitoringStatus(health.AggregateStatus(statuses), true, observed, "")
}

// GetMonitoringNodes implements rpcpb.ManagerServiceServer: the
// per-node monitoring summaries.
//
// Viewer-tier, and read-only in the strongest sense - it observes. It
// changes nothing, and it is safe to call at any time.
func (s *Server) GetMonitoringNodes(ctx context.Context, _ *rpcpb.GetMonitoringNodesRequest) (*rpcpb.GetMonitoringNodesResponse, error) {
	reports, clusterErr := s.gatherMonitoringNodes(ctx)
	return &rpcpb.GetMonitoringNodesResponse{
		LocalNodeId: s.nodeID,
		Nodes:       reports,
		Error:       clusterErr,
	}, nil
}

// GetMonitoringCluster implements rpcpb.ManagerServiceServer: the
// cluster-wide monitoring view.
//
// It carries the per-node rows AND the Colony's own verdict, so a
// consumer that only wants the cluster verdict does not have to compute
// one from the rows - which would mean reimplementing internal/health's
// aggregation, which is exactly what ADR-0160 says must not happen on
// the consumer's side.
func (s *Server) GetMonitoringCluster(ctx context.Context, _ *rpcpb.GetMonitoringClusterRequest) (*rpcpb.GetMonitoringClusterResponse, error) {
	reports, clusterErr := s.gatherMonitoringNodes(ctx)

	// The cluster verdict is the AGGREGATION of the rows, by the same
	// rule the Command Center's health tree applies - internal/health's
	// AggregateStatus, which is the one implementation of it.
	cluster := s.monitoringClusterVerdict(reports)

	return &rpcpb.GetMonitoringClusterResponse{
		LocalNodeId: s.nodeID,
		Health:      cluster,
		Nodes:       reports,
		Error:       clusterErr,
	}, nil
}
