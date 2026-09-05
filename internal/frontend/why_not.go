package frontend

import (
	"context"
	"net/http"
	"sort"
	"strings"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/invariant"
	"github.com/glenjbarber/apiary/internal/whynot"
)

// whyNotEvidenceView/whyNotBlockerView/whyNotRemedyView/whyNotAnswerView
// are the template-facing shapes for whynot.Evidence/Blocker/Remedy/
// Answer - the same "translate once, never expose raw package types to
// templates" convention convert.go/simulate.go/invariants.go already
// follow.
type whyNotEvidenceView struct {
	Source        string
	Detail        string
	ObservedAt    string
	NeverObserved bool
}

func fromWhyNotEvidence(ev invariant.Evidence) whyNotEvidenceView {
	v := whyNotEvidenceView{Source: ev.Source, Detail: ev.Detail}
	if ev.ObservedAt.IsZero() {
		v.NeverObserved = true
	} else {
		v.ObservedAt = ev.ObservedAt.Format("2006-01-02 15:04:05 MST")
	}
	return v
}

type whyNotBlockerView struct {
	Invariant string
	Detail    string
	Evidence  []whyNotEvidenceView
}

type whyNotRemedyView struct {
	Detail string
	Proven bool
}

type whyNotAnswerView struct {
	Question string
	Scope    string
	Verdict  string
	Blockers []whyNotBlockerView
	Remedies []whyNotRemedyView
	Caveats  []whyNotEvidenceView
}

type whyNotCellChoiceView struct {
	ID     string
	Name   string
	Kind   string
	NodeID string
}

func fromWhyNotAnswer(a whynot.Answer) whyNotAnswerView {
	v := whyNotAnswerView{Question: a.Question, Scope: a.Scope, Verdict: string(a.Verdict)}
	for _, b := range a.Blockers {
		bv := whyNotBlockerView{Invariant: b.Invariant, Detail: b.Detail}
		for _, ev := range b.Evidence {
			bv.Evidence = append(bv.Evidence, fromWhyNotEvidence(ev))
		}
		v.Blockers = append(v.Blockers, bv)
	}
	for _, r := range a.Remedies {
		v.Remedies = append(v.Remedies, whyNotRemedyView{Detail: r.Detail, Proven: r.Proven})
	}
	for _, c := range a.Caveats {
		v.Caveats = append(v.Caveats, fromWhyNotEvidence(c))
	}
	return v
}

// lookupCell resolves a single cell (VM or jail) by ID via GetVM then
// GetJail - the same one-shot single-target pattern console.go/
// serial_log.go already use - and, only for a VM with a configured
// ReplicaNodeID, one bounded HostStats call to that one node (never a
// cluster-wide fan-out to every replica target, which
// gatherCellRecoverability does and a single-question lookup has no
// reason to pay for). A jail always gets DestinationCapable ==
// ResultUnknown, matching gatherCellRecoverability's own established
// handling: no capability signal exists for jails anywhere in this
// codebase.
func (s *Server) lookupCell(ctx context.Context, id, localNodeID string) (whynot.CellFact, bool) {
	if resp, err := s.client.GetVM(ctx, &rpcpb.GetVMRequest{Id: id}); err == nil && resp.GetFound() {
		v := fromRPCVM(resp.GetVm())
		f := whynot.CellFact{
			ID: v.ID, Name: v.Name, Kind: "vm",
			DesiredState: v.DesiredState, NodeID: v.NodeID, ReplicaNodeID: v.ReplicaNodeID,
		}
		if v.ReplicaNodeID != "" {
			checkCtx, cancel := context.WithTimeout(ctx, nodeContextTimeout)
			defer cancel()
			switch hs, err := s.fetchHostStats(checkCtx, v.ReplicaNodeID, localNodeID); {
			case err != nil:
				f.DestinationCapable = invariant.ResultUnknown
				f.DestinationCapableDetail = v.ReplicaNodeID + ": HostStats fetch failed - " + err.Error()
			case hs.GetBhyveConfigured():
				f.DestinationCapable = invariant.ResultTrue
				f.DestinationCapableDetail = v.ReplicaNodeID + ": bhyve configured"
			default:
				f.DestinationCapable = invariant.ResultFalse
				f.DestinationCapableDetail = v.ReplicaNodeID + ": bhyve NOT configured"
			}
		}
		return f, true
	}
	if resp, err := s.client.GetJail(ctx, &rpcpb.GetJailRequest{Id: id}); err == nil && resp.GetFound() {
		j := fromRPCJail(resp.GetJail())
		return whynot.CellFact{
			ID: j.ID, Name: j.Name, Kind: "jail",
			DesiredState: j.DesiredState, NodeID: j.NodeID, ReplicaNodeID: j.ReplicaNodeID,
			DestinationCapable:       invariant.ResultUnknown,
			DestinationCapableDetail: "no capability signal exists for jails in this codebase",
		}, true
	}
	return whynot.CellFact{}, false
}

// whyNotCellChoices returns every operator-selectable Cell for the
// Why Not Engine's Cell question. It is best-effort by design: a
// transient VM or jail list failure must not break the page or prevent
// the existing typed-ID fallback from being usable.
func (s *Server) whyNotCellChoices(r *http.Request) []whyNotCellChoiceView {
	var cells []whyNotCellChoiceView
	if vms, errMsg := s.currentVMs(r, "id", "asc"); errMsg == "" {
		for _, vm := range vms {
			cells = append(cells, whyNotCellChoiceView{
				ID:     vm.ID,
				Name:   vm.Name,
				Kind:   "VM",
				NodeID: vm.NodeID,
			})
		}
	}
	if jails, errMsg := s.currentJails(r); errMsg == "" {
		for _, jail := range jails {
			if strings.EqualFold(jail.ID, "timemachine") || strings.EqualFold(jail.Name, "timemachine") {
				continue
			}
			cells = append(cells, whyNotCellChoiceView{
				ID:     jail.ID,
				Name:   jail.Name,
				Kind:   "Jail",
				NodeID: jail.NodeID,
			})
		}
	}
	sort.SliceStable(cells, func(i, j int) bool {
		if cells[i].Kind != cells[j].Kind {
			return cells[i].Kind < cells[j].Kind
		}
		if cells[i].Name != cells[j].Name {
			return cells[i].Name < cells[j].Name
		}
		return cells[i].ID < cells[j].ID
	})
	return cells
}

// answerNetworkConnectivity fans GetLocalNetworkBridgeStatus out to the
// one requested network's actual VM-owning nodes only (never every
// network - gatherNetworkRoute's own cluster-wide cost has no reason to
// be paid for a single-target question, and never ListNetworks's own
// bridge_status field, the ADR-0055/0060-documented bug class where
// that field is populated by whichever node answers a leader-only,
// forwarding RPC and would silently mislabel the LEADER's bridge state
// as another node's own).
func (s *Server) answerNetworkConnectivity(ctx context.Context, localNodeID string, r *http.Request, net networkView) whynot.Answer {
	vms, vmsErr := s.currentVMs(r, "id", "asc")
	owners := map[string]struct{}{}
	if vmsErr == "" {
		for _, vm := range vms {
			if vm.NetworkID == net.ID && vm.NodeID != "" {
				owners[vm.NodeID] = struct{}{}
			}
		}
	}
	nodeIDs := make([]string, 0, len(owners))
	for id := range owners {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)

	fact := invariant.NetworkFact{ID: net.ID, Name: net.Name}
	checkCtx, cancel := context.WithTimeout(ctx, nodeContextOverallTimeout)
	defer cancel()
	for _, nodeID := range nodeIDs {
		var resp *rpcpb.GetLocalNetworkBridgeStatusResponse
		var err error
		fetchCtx, fetchCancel := context.WithTimeout(checkCtx, nodeContextTimeout)
		if nodeID == localNodeID || s.peers == nil {
			resp, err = s.client.GetLocalNetworkBridgeStatus(fetchCtx, &rpcpb.GetLocalNetworkBridgeStatusRequest{NetworkId: net.ID})
		} else {
			resp, err = s.peers.GetLocalNetworkBridgeStatus(fetchCtx, s.peerAddr(nodeID), net.ID)
		}
		fetchCancel()
		switch {
		case err != nil:
			fact.Observations = append(fact.Observations, invariant.BridgeObservation{NodeID: nodeID, Err: err.Error()})
		case resp.GetError() != "":
			fact.Observations = append(fact.Observations, invariant.BridgeObservation{NodeID: nodeID, Err: resp.GetError()})
		default:
			fact.Observations = append(fact.Observations, invariant.BridgeObservation{NodeID: nodeID, Status: resp.GetBridgeStatus()})
		}
	}

	return whynot.AnswerNetworkConnectivity(fact)
}

// handleWhyNotPage serves the Why Not Engine page ("/why-not",
// ADR-0061): a read-only, deterministic answer to concrete operator
// questions, citing the smallest actual blocker set plus the evidence
// and invariant behind each conclusion. No new RPC/proto - every fact
// comes from RPCs/gathering helpers that already exist for other
// pages. One ?cell_id= answers both cell-migrate and cell-recoverable
// together, since they're answered from the exact same lookup and
// CODEX's own four example questions map to three distinct inputs
// (Cell/Hive/Network), not four.
func (s *Server) handleWhyNotPage(w http.ResponseWriter, r *http.Request) {
	statusResp, _ := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	localNodeID := statusResp.GetManagerNodeId()

	var (
		cellID            = r.URL.Query().Get("cell_id")
		cellErr           string
		cellMigrateAnswer whyNotAnswerView
		cellRecoverAnswer whyNotAnswerView
	)
	if cellID != "" {
		if fact, found := s.lookupCell(r.Context(), cellID, localNodeID); found {
			cellMigrateAnswer = fromWhyNotAnswer(whynot.AnswerCellMigrate(fact))
			cellRecoverAnswer = fromWhyNotAnswer(whynot.AnswerCellRecoverable(fact))
		} else {
			cellErr = "Cell " + cellID + " was not found as either a VM or a jail."
		}
	}

	var (
		nodeID     = r.URL.Query().Get("node_id")
		hiveErr    string
		hiveAnswer whyNotAnswerView
	)
	if nodeID != "" {
		resp, err := s.client.SimulateNodeFailure(r.Context(), &rpcpb.SimulateNodeFailureRequest{NodeId: nodeID})
		switch {
		case err != nil:
			hiveErr = err.Error()
		case resp.GetError() != "":
			hiveErr = resp.GetError()
		default:
			quorum := whynot.QuorumFact{Survives: resp.GetQuorum().GetSurvives(), Note: resp.GetQuorum().GetNote()}
			var owned []whynot.OwnedResourceFact
			for _, res := range resp.GetOwnedResources() {
				view := fromRPCOwnedResourceImpact(res)
				owned = append(owned, whynot.OwnedResourceFact{
					ID: view.ID, Name: view.Name, Kind: view.Kind,
					ReplicaNodeID: view.ReplicaNodeID, Verdict: view.Verdict, Explanation: view.Explanation,
				})
			}
			var replicaBacked []whynot.ReplicaBackedFact
			for _, res := range resp.GetReplicaBackedResources() {
				view := fromRPCReplicaBackedImpact(res)
				replicaBacked = append(replicaBacked, whynot.ReplicaBackedFact{
					ID: view.ID, Name: view.Name, Kind: view.Kind,
					OwnerNodeID: view.OwnerNodeID, Explanation: view.Explanation,
				})
			}
			hiveAnswer = fromWhyNotAnswer(whynot.AnswerHiveReboot(nodeID, quorum, owned, replicaBacked))
		}
	}

	var (
		networkID     = r.URL.Query().Get("network_id")
		networkErr    string
		networkAnswer whyNotAnswerView
	)
	if networkID != "" {
		networks, netErr := s.currentNetworks(r)
		var target *networkView
		if netErr == "" {
			for i := range networks {
				if networks[i].ID == networkID {
					target = &networks[i]
					break
				}
			}
		}
		switch {
		case netErr != "":
			networkErr = netErr
		case target == nil:
			networkErr = "Network " + networkID + " was not found."
		default:
			networkAnswer = fromWhyNotAnswer(s.answerNetworkConnectivity(r.Context(), localNodeID, r, *target))
		}
	}

	nodes := s.simulateNodeChoices(r)
	networks, _ := s.currentNetworks(r)
	cells := s.whyNotCellChoices(r)

	s.render(w, "why_not_page", s.withAuthFields(r, pageData{
		WhyNotCellID:          cellID,
		WhyNotCellError:       cellErr,
		WhyNotCells:           cells,
		WhyNotCellMigrate:     cellMigrateAnswer,
		WhyNotCellRecoverable: cellRecoverAnswer,
		WhyNotNodes:           nodes,
		WhyNotNodeID:          nodeID,
		WhyNotHiveError:       hiveErr,
		WhyNotHiveReboot:      hiveAnswer,
		WhyNotNetworks:        networks,
		WhyNotNetworkID:       networkID,
		WhyNotNetworkError:    networkErr,
		WhyNotNetworkConnect:  networkAnswer,
		ActivePage:            "why-not",
	}))
}
