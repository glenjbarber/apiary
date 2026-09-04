package frontend

import (
	"context"
	"net/http"
	"sync"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/coverage"
	"github.com/glenjbarber/apiary/internal/invariant"
	"github.com/glenjbarber/apiary/internal/recovery"
)

// coverageBadgeClass maps a coverage.Status onto this project's
// already-defined badge CSS classes (web/templates/layout.html) - no
// new CSS. "simulated" (a real mechanism exists and evaluated this
// scenario) reuses the "true"/healthy styling regardless of the
// scenario's own Result, since Status describes whether evidence
// exists, not whether the evidence is good news. "unsafe_or_impossible"
// and "untested" reuse "false" and "unknown" respectively.
// "physically_rehearsed" is unreachable in v1 but mapped for
// completeness; "stale" already has its own literal CSS class.
func coverageBadgeClass(status coverage.Status) string {
	switch status {
	case coverage.StatusSimulated, coverage.StatusPhysicallyRehearsed:
		return "true"
	case coverage.StatusUnsafeOrImpossible:
		return "false"
	case coverage.StatusStale:
		return "stale"
	default: // StatusUntested
		return "unknown"
	}
}

// coverageScenarioView is the template-facing shape for one
// coverage.Scenario.
type coverageScenarioView struct {
	Kind, Target, Label string
	Status              string
	BadgeClass          string
	Result              string
	Explanation         string
}

func fromCoverageScenario(s coverage.Scenario) coverageScenarioView {
	return coverageScenarioView{
		Kind: s.Kind, Target: s.Target, Label: s.Label,
		Status: string(s.Status), BadgeClass: coverageBadgeClass(s.Status),
		Result: s.Result, Explanation: s.Explanation,
	}
}

// coverageStatusCountView is one row of the plain Counts tally - a
// fixed, ordered list (never a percentage, never a progress bar) so
// the permanently-zero statuses render just as visibly as the
// populated ones.
type coverageStatusCountView struct {
	Status     string
	BadgeClass string
	Count      int
}

// gatherHiveFailureScenarios builds one coverage.Scenario per node in
// simulateNodeChoices, at zero extra RPC cost: quorum verdicts reuse
// the exact []invariant.VoterReachability snapshot gatherQuorumTolerance
// already builds (one bounded HostStats fan-out, already paid for by
// the quorum-tolerance scenario itself), and owned/replica-backed
// counts come from a local loop over the VM/jail lists already fetched
// for cell-recoverability - never a per-node SimulateNodeFailure call,
// which would cost O(candidateNodes x clusterServers) RPCs on the
// leader for what boils down to two counts and one verdict per node.
func (s *Server) gatherHiveFailureScenarios(nodeIDs []string, voterImpacts []invariant.VoterQuorumImpact, vms []vmView, jails []jailView) []coverage.Scenario {
	impactByNode := make(map[string]invariant.VoterQuorumImpact, len(voterImpacts))
	for _, impact := range voterImpacts {
		impactByNode[impact.NodeID] = impact
	}

	scenarios := make([]coverage.Scenario, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		owned, replicaBacked := 0, 0
		for _, vm := range vms {
			if vm.NodeID == id {
				owned++
			}
			if vm.ReplicaNodeID == id {
				replicaBacked++
			}
		}
		for _, j := range jails {
			if j.NodeID == id {
				owned++
			}
			if j.ReplicaNodeID == id {
				replicaBacked++
			}
		}

		impact, isVoter := impactByNode[id]
		verdict := recovery.QuorumSurvives
		valid := true
		if isVoter {
			verdict, valid = impact.Verdict, impact.Valid
		}

		scenarios = append(scenarios, coverage.ClassifyHiveFailure(id, "Hive "+id+" fails", verdict, valid, owned, replicaBacked))
	}
	return scenarios
}

// gatherNetworkFailureScenarios calls SimulateNetworkFailure once per
// managed network (bounded, concurrent) - unlike SimulateNodeFailure,
// this RPC's handler does only two sequential leader-only reads
// (ListNetworks, ListVMs) with no per-server fan-out, so looping it
// over a typically small set of managed networks is cheap.
func (s *Server) gatherNetworkFailureScenarios(ctx context.Context, networks []networkView) []coverage.Scenario {
	scenarios := make([]coverage.Scenario, len(networks))
	overallCtx, cancel := context.WithTimeout(ctx, nodeContextOverallTimeout)
	defer cancel()

	var wg sync.WaitGroup
	limit := len(networks)
	if limit > nodeContextLimit {
		limit = nodeContextLimit
	}
	for i := 0; i < limit; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			net := networks[i]
			label := "Network " + net.Name + " fails"
			checkCtx, checkCancel := context.WithTimeout(overallCtx, nodeContextTimeout)
			defer checkCancel()
			resp, err := s.client.SimulateNetworkFailure(checkCtx, &rpcpb.SimulateNetworkFailureRequest{NetworkId: net.ID})
			switch {
			case err != nil, resp.GetError() != "":
				scenarios[i] = coverage.ClassifyNetworkFailure(net.ID, label, 0)
			default:
				scenarios[i] = coverage.ClassifyNetworkFailure(net.ID, label, len(resp.GetAffectedResources()))
			}
		}(i)
	}
	wg.Wait()
	for i := limit; i < len(networks); i++ {
		net := networks[i]
		scenarios[i] = coverage.ClassifyNetworkFailure(net.ID, "Network "+net.Name+" fails", 0)
	}
	return scenarios
}

// handleCoveragePage serves the Resilience Coverage Map page
// ("/resilience-coverage", ADR-0062): enumerates every meaningful
// failure scenario already computable by other shipped mechanisms (the
// Dependency Graph Simulator, the Operational Invariants catalog) and
// classifies each by whether real evidence exists - never a dump of
// every warning, never a rolled-up percentage. No new RPC/proto beyond
// one additive internal/invariant function.
func (s *Server) handleCoveragePage(w http.ResponseWriter, r *http.Request) {
	statusResp, statusErr := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	localNodeID := statusResp.GetManagerNodeId()

	vms, vmsErr := s.currentVMs(r, "id", "asc")
	jails, jailsErr := s.currentJails(r)
	networks, netErr := s.currentNetworks(r)

	var scenarios []coverage.Scenario

	if statusErr != nil || !statusResp.GetRaftReachable() || statusResp.GetRaftError() != "" {
		reason := "raft status could not be confirmed"
		switch {
		case statusErr != nil:
			reason = statusErr.Error()
		case statusResp.GetRaftError() != "":
			reason = statusResp.GetRaftError()
		}
		scenarios = append(scenarios, coverage.ClassifyQuorumTolerance(invariant.Evaluation{
			Result: invariant.ResultUnknown, Explanation: "Quorum tolerance could not be evaluated: " + reason,
		}))
	} else {
		voterReachability := s.gatherVoterReachability(r.Context(), statusResp, localNodeID)
		leaderID := statusResp.GetRaftLeaderId()

		quorumEval := invariant.EvaluateQuorumTolerance(voterReachability, leaderID)
		scenarios = append(scenarios, coverage.ClassifyQuorumTolerance(quorumEval))

		voterImpacts := invariant.ClassifyVoterQuorumImpacts(voterReachability, leaderID)
		nodeIDs := s.simulateNodeChoices(r)
		scenarios = append(scenarios, s.gatherHiveFailureScenarios(nodeIDs, voterImpacts, vms, jails)...)
	}

	if netErr == "" {
		scenarios = append(scenarios, s.gatherNetworkFailureScenarios(r.Context(), networks)...)
	}

	for _, e := range s.gatherNetworkRoute(r.Context(), localNodeID, r, vms, vmsErr) {
		scenarios = append(scenarios, coverage.ClassifyNetworkConnectivity(e))
	}

	for _, e := range s.gatherCellRecoverability(r.Context(), localNodeID, r) {
		scenarios = append(scenarios, coverage.ClassifyCellRecoverability(e))
	}

	var resourceIDs []string
	if vmsErr == "" {
		for _, vm := range vms {
			if vm.ReplicaNodeID != "" {
				resourceIDs = append(resourceIDs, vm.ID)
			}
		}
	}
	if jailsErr == "" {
		for _, j := range jails {
			if j.ReplicaNodeID != "" {
				resourceIDs = append(resourceIDs, j.ID)
			}
		}
	}
	for _, e := range invariant.EvaluateHASTDualPrimary(resourceIDs) {
		scenarios = append(scenarios, coverage.ClassifyHASTDualPrimary(e))
	}

	report := coverage.BuildReport(scenarios)

	views := make([]coverageScenarioView, 0, len(report.Scenarios))
	for _, sc := range report.Scenarios {
		views = append(views, fromCoverageScenario(sc))
	}

	counts := make([]coverageStatusCountView, 0, len(coverageStatusDisplayOrder))
	for _, st := range coverageStatusDisplayOrder {
		counts = append(counts, coverageStatusCountView{Status: string(st), BadgeClass: coverageBadgeClass(st), Count: report.Counts[st]})
	}

	s.render(w, "coverage_page", s.withAuthFields(r, pageData{
		CoverageScenarios: views,
		CoverageCounts:    counts,
		CoverageGaps:      report.Gaps,
		ActivePage:        "resilience-coverage",
	}))
}

// coverageStatusDisplayOrder fixes the Counts tally's rendering order -
// mirrors internal/coverage's own statusOrder so the permanently-zero
// statuses (physically_rehearsed, stale) always render, not just the
// populated ones.
var coverageStatusDisplayOrder = []coverage.Status{
	coverage.StatusUnsafeOrImpossible,
	coverage.StatusSimulated,
	coverage.StatusUntested,
	coverage.StatusPhysicallyRehearsed,
	coverage.StatusStale,
}
