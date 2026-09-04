package frontend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/health"
	"github.com/glenjbarber/apiary/internal/recovery"
)

// recoveryHandbookFormatVersion is embedded in every generated edition
// and in the fingerprint envelope below - CODEX's own "creation time,
// version, checksum" requirement.
const recoveryHandbookFormatVersion = "v1"

// nodeContextLimit caps the number of nodes given detailed Node Context
// checking (see gatherNodeContext) - a defensive bound, not expected to
// matter at this project's actual cluster sizes.
const nodeContextLimit = 10

// nodeContextTimeout/nodeContextOverallTimeout bound the bounded,
// embedded Node Context evidence (see
// docs/adr/0057-offline-recovery-handbook-v1.md) in time, not just node
// count - neither fetchHostStats/nodeHealthSignals nor nodeAssumptions
// wrap their own context in a deadline, so this page adds one at every
// call site rather than risking an unreachable peer hanging the whole
// request. nodeContextTimeout matches internal/manager/server.go's own
// reachabilityCheckTimeout for consistency. Package-level vars, not
// consts, so tests can shrink them to exercise real timeout behavior
// without waiting out the production duration.
var (
	nodeContextTimeout        = 3 * time.Second
	nodeContextOverallTimeout = 10 * time.Second
)

// recoveryStepView is the template-facing shape for one recovery.Step.
type recoveryStepView struct {
	Order         int
	Title, Detail string
	Irreversible  bool
	StopCondition string
}

func fromRecoveryStep(s recovery.Step) recoveryStepView {
	return recoveryStepView{
		Order: s.Order, Title: s.Title, Detail: s.Detail,
		Irreversible: s.Irreversible, StopCondition: s.StopCondition,
	}
}

// recoveryMemberView is the template-facing shape for one raft member -
// the "dated topology snapshot" CODEX asks for, free from the anchor
// Status() call already fetched for leader identity.
type recoveryMemberView struct {
	NodeID   string
	Suffrage string
}

// recoveryAssumptionConcernView mirrors recovery.AssumptionConcern for
// template rendering.
type recoveryAssumptionConcernView struct {
	Kind, SubjectKind, SubjectID, DependencyID, Qualifier string
	ObservedStatus, Status                                string
	Stale                                                 bool
	LastObservedAt                                        string
	ReasonCode, Detail                                    string
}

// recoveryNodeContextView is the template-facing shape for one bounded,
// embedded peer's evidence (see gatherNodeContext below).
type recoveryNodeContextView struct {
	NodeID               string
	Reason               string
	EvidenceLimitReached bool

	HealthStatus       string
	HealthExplanation  string
	HealthObservations []health.Observation

	AssumptionsFetchError string
	Assumptions           []recoveryAssumptionConcernView
	StorageDegraded       bool
	StorageDegradedDetail string
}

func fromRecoveryNodeContext(f recovery.NodeContextFact) recoveryNodeContextView {
	v := recoveryNodeContextView{
		NodeID: f.NodeID, Reason: f.Reason, EvidenceLimitReached: f.EvidenceLimitReached,
		HealthStatus: string(f.Health.Status), HealthExplanation: f.Health.Explanation,
		HealthObservations:    f.Health.Observations,
		AssumptionsFetchError: f.AssumptionsFetchError,
		StorageDegraded:       f.StorageDegraded, StorageDegradedDetail: f.StorageDegradedDetail,
	}
	for _, a := range f.Assumptions {
		v.Assumptions = append(v.Assumptions, recoveryAssumptionConcernView{
			Kind: a.Kind, SubjectKind: a.SubjectKind, SubjectID: a.SubjectID,
			DependencyID: a.DependencyID, Qualifier: a.Qualifier,
			ObservedStatus: a.ObservedStatus, Status: a.Status, Stale: a.Stale,
			LastObservedAt: a.LastObservedAt, ReasonCode: a.ReasonCode, Detail: a.Detail,
		})
	}
	return v
}

// fromRPCQuorumFact converts SimulateNodeFailureResponse's QuorumImpact
// into recovery.QuorumFact's decision-focused shape - the raw counts
// ClassifyQuorum/ValidQuorumFact need, never a pre-collapsed Survives
// bool (see internal/recovery/quorum.go's own doc comments for why).
func fromRPCQuorumFact(q *rpcpb.QuorumImpact) recovery.QuorumFact {
	return recovery.QuorumFact{
		TargetIsVoter:      q.GetTargetIsVoter(),
		TotalVoters:        q.GetTotalVoters(),
		RemainingVoters:    q.GetRemainingVoters(),
		RemainingReachable: q.GetRemainingReachableVoters(),
		RemainingUnknown:   q.GetRemainingUnknownVoters(),
		QuorumSize:         q.GetQuorumSize(),
	}
}

func fromRPCResourceFact(i *rpcpb.OwnedResourceImpact) recovery.ResourceFact {
	return recovery.ResourceFact{
		ID: i.GetId(), Name: i.GetName(), Kind: fromRPCResourceKind(i.GetKind()),
		ReplicaNodeID:     i.GetReplicaNodeId(),
		ReplicaConfigured: i.GetVerdict() == rpcpb.RecoveryVerdict_RECOVERY_VERDICT_UNVERIFIED_REPLICA,
	}
}

func fromRPCReplicaBackedFact(i *rpcpb.ReplicaBackedImpact) recovery.ReplicaBackedFact {
	return recovery.ReplicaBackedFact{
		ID: i.GetId(), Name: i.GetName(), Kind: fromRPCResourceKind(i.GetKind()),
		OwnerNodeID: i.GetOwnerNodeId(),
	}
}

func fromRPCImageFact(i *rpcpb.ImageAvailabilityImpact) recovery.ImageFact {
	verdict := recovery.ImageUnknown
	switch i.GetVerdict() {
	case rpcpb.ImageAvailabilityVerdict_IMAGE_AVAILABILITY_VERDICT_AVAILABLE:
		verdict = recovery.ImageAvailable
	case rpcpb.ImageAvailabilityVerdict_IMAGE_AVAILABILITY_VERDICT_UNAVAILABLE:
		verdict = recovery.ImageUnavailable
	}
	return recovery.ImageFact{
		ResourceID: i.GetResourceId(), ResourceName: i.GetResourceName(),
		ImageName: i.GetImageName(), Verdict: verdict,
	}
}

// recoveryContextNodeIDs returns the deduped, sorted set of nodes
// relevant to targetNodeID's own hive-loss scenario: every distinct
// ReplicaNodeID among its owned resources (a candidate migration
// target) plus every distinct OwnerNodeID among resources it backs -
// never the whole cluster.
func recoveryContextNodeIDs(owned []recovery.ResourceFact, replicaBacked []recovery.ReplicaBackedFact) []string {
	seen := make(map[string]struct{})
	for _, r := range owned {
		if r.ReplicaNodeID != "" {
			seen[r.ReplicaNodeID] = struct{}{}
		}
	}
	for _, r := range replicaBacked {
		if r.OwnerNodeID != "" {
			seen[r.OwnerNodeID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// recoveryContextReason explains why nodeID is relevant, listing every
// owned/replica-backed resource that points at it - a node can be
// relevant for more than one reason at once.
func recoveryContextReason(nodeID string, owned []recovery.ResourceFact, replicaBacked []recovery.ReplicaBackedFact) string {
	var reasons []string
	for _, r := range owned {
		if r.ReplicaNodeID == nodeID {
			reasons = append(reasons, fmt.Sprintf("replica target for %s %q", r.Kind, r.Name))
		}
	}
	for _, r := range replicaBacked {
		if r.OwnerNodeID == nodeID {
			reasons = append(reasons, fmt.Sprintf("owner of replica-backed %s %q", r.Kind, r.Name))
		}
	}
	return strings.Join(reasons, "; ")
}

// gatherNodeContext fetches bounded, embedded evidence for the (at most
// nodeContextLimit) nodes relevant to this hive-loss scenario, so the
// printed page is self-contained during the exact outage it targets
// rather than linking to "/"/"/assumptions" (unusable, and dead text
// once printed). Every relevant node ID is always returned, even beyond
// the limit - a node beyond it comes back with EvidenceLimitReached true
// and every other field zero-valued, never silently dropped from the
// list altogether. Detail-checked nodes are fetched concurrently, each
// under its own timeout plus one overall timeout for the whole phase -
// a failure or timeout is disclosed (Health.Status/AssumptionsFetchError),
// never a silent hang.
func (s *Server) gatherNodeContext(ctx context.Context, anchor *rpcpb.StatusResponse, localNodeID string, owned []recovery.ResourceFact, replicaBacked []recovery.ReplicaBackedFact) []recovery.NodeContextFact {
	ids := recoveryContextNodeIDs(owned, replicaBacked)
	facts := make([]recovery.NodeContextFact, len(ids))

	overallCtx, cancel := context.WithTimeout(ctx, nodeContextOverallTimeout)
	defer cancel()

	var wg sync.WaitGroup
	for i, id := range ids {
		reason := recoveryContextReason(id, owned, replicaBacked)
		if i >= nodeContextLimit {
			facts[i] = recovery.NodeContextFact{NodeID: id, Reason: reason, EvidenceLimitReached: true}
			continue
		}
		wg.Add(1)
		go func(i int, id, reason string) {
			defer wg.Done()
			facts[i] = s.fetchNodeContext(overallCtx, id, localNodeID, anchor, reason)
		}(i, id, reason)
	}
	wg.Wait()

	return facts
}

// fetchNodeContext gathers one node's health verdict and environmental
// assumption results. Health reuses the existing private
// fetchHostStats/nodeHealthSignals + health.ComputeNodeHealth pipeline
// unmodified - a failed or timed-out peer call already flows through as
// health.StatusUnknown/StatusContradictory with a citing Observation, so
// no separate boolean is needed here (nodeHealthSignals has no error to
// report in the first place). Assumptions reuse nodeAssumptions, which
// does return a real fetch error, carried through as AssumptionsFetchError.
func (s *Server) fetchNodeContext(ctx context.Context, nodeID, localNodeID string, anchor *rpcpb.StatusResponse, reason string) recovery.NodeContextFact {
	fact := recovery.NodeContextFact{NodeID: nodeID, Reason: reason}

	healthCtx, healthCancel := context.WithTimeout(ctx, nodeContextTimeout)
	defer healthCancel()
	now := time.Now()
	hostStats, hostStatsErr := s.fetchHostStats(healthCtx, nodeID, localNodeID)
	signals := s.nodeHealthSignals(healthCtx, nodeID, localNodeID, anchor, hostStats, hostStatsErr, now)
	fact.Health = health.ComputeNodeHealth(signals, now)

	assumeCtx, assumeCancel := context.WithTimeout(ctx, nodeContextTimeout)
	defer assumeCancel()
	assumptions := s.nodeAssumptions(assumeCtx, nodeID, localNodeID)
	if assumptions.Error != "" {
		fact.AssumptionsFetchError = assumptions.Error
		return fact
	}
	fact.StorageDegraded = assumptions.StorageDegraded
	fact.StorageDegradedDetail = assumptions.StorageDegradedDetail
	for _, res := range assumptions.Results {
		if res.Status == "true" {
			continue // only concerns are carried into the handbook - a healthy "true" result isn't one
		}
		fact.Assumptions = append(fact.Assumptions, recovery.AssumptionConcern{
			Kind: res.Kind, SubjectKind: res.SubjectKind, SubjectID: res.SubjectID,
			DependencyID: res.DependencyID, Qualifier: res.Qualifier,
			ObservedStatus: res.ObservedStatus, Status: res.Status, Stale: res.Stale,
			LastObservedAt: res.LastObservedAt, ReasonCode: res.ReasonCode, Detail: res.Detail,
		})
	}
	return fact
}

// recoveryFingerprintEnvelope is what the Snapshot fingerprint actually
// hashes: every CORE, stable fact the page presents (topology, quorum,
// ownership, images, the generated Steps themselves) - deliberately
// excluding both presentation timestamps (GeneratedAt, the evidence-
// gathering window) and the bounded Node Context section, whose own
// evidence (health.Observation.ObservedAt, each AssumptionConcern's
// LastObservedAt) is itself inherently time-varying observational data,
// not a stable fact the way membership/ownership/quorum are - including
// it would make the fingerprint change on every regeneration even when
// nothing about the cluster's actual topology changed, defeating the
// one purpose a fingerprint here serves (detecting whether a printed
// copy still matches what would be generated right now).
type recoveryFingerprintEnvelope struct {
	FormatVersion   string
	TargetNodeID    string
	IsCurrentLeader bool
	Quorum          recovery.QuorumFact
	OwnedResources  []recovery.ResourceFact
	ReplicaBacked   []recovery.ReplicaBackedFact
	Images          []recovery.ImageFact
	Steps           []recovery.Step
	RaftMembers     []recoveryMemberView
}

func computeRecoveryFingerprint(in recovery.Inputs, steps []recovery.Step, members []recoveryMemberView) string {
	sortedMembers := append([]recoveryMemberView(nil), members...)
	sort.Slice(sortedMembers, func(i, j int) bool { return sortedMembers[i].NodeID < sortedMembers[j].NodeID })

	envelope := recoveryFingerprintEnvelope{
		FormatVersion:   recoveryHandbookFormatVersion,
		TargetNodeID:    in.TargetNodeID,
		IsCurrentLeader: in.IsCurrentLeader,
		Quorum:          in.Quorum,
		OwnedResources:  in.OwnedResources,
		ReplicaBacked:   in.ReplicaBacked,
		Images:          in.Images,
		Steps:           steps,
		RaftMembers:     sortedMembers,
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

// handleRecoveryHandbookPage serves the Offline Recovery Handbook page
// ("/recovery-handbook", ADR-0057) - a printable, on-demand-generated
// edition for exactly one scenario, loss of one hive, parameterized by
// ?node_id= exactly like /simulate. No new RPC: every fact comes from
// SimulateNodeFailure (ADR-0052/53/54) plus one Status() call for raft
// leader identity/membership.
func (s *Server) handleRecoveryHandbookPage(w http.ResponseWriter, r *http.Request) {
	evidenceStart := time.Now()
	nodes := s.simulateNodeChoices(r)
	nodeID := r.URL.Query().Get("node_id")

	data := pageData{RecoveryNodes: nodes, RecoveryTargetNodeID: nodeID, ActivePage: "recovery-handbook"}

	// The anchor Status() call is fetched regardless of whether node_id
	// is set, exactly like simulateNodeChoices's own best-effort posture -
	// the free topology snapshot is a convenience on the empty-picker
	// page, not something that should hard-error the page if it fails.
	statusResp, statusErr := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	if statusErr == nil {
		for _, m := range statusResp.GetMembers() {
			data.RecoveryMembers = append(data.RecoveryMembers, recoveryMemberView{NodeID: m.GetNodeId(), Suffrage: m.GetSuffrage()})
		}
	}

	if nodeID == "" {
		s.render(w, "recovery_handbook_page", s.withAuthFields(r, data))
		return
	}

	// Generation-state gating: unlike the empty-picker case above, once a
	// specific node_id is chosen the anchor Status() call becomes load-
	// bearing (IsCurrentLeader, ValidQuorumFact both depend on it reading
	// real raft state) - a transport error OR a normal-looking response
	// with RaftReachable=false must both refuse to generate rather than
	// silently guessing IsCurrentLeader=false.
	if statusErr != nil || !statusResp.GetRaftReachable() || statusResp.GetRaftError() != "" {
		reason := "raft status could not be confirmed"
		switch {
		case statusErr != nil:
			reason = statusErr.Error()
		case statusResp.GetRaftError() != "":
			reason = statusResp.GetRaftError()
		}
		data.RecoveryError = "Handbook could not be generated: " + reason
		s.render(w, "recovery_handbook_page", s.withAuthFields(r, data))
		return
	}

	resp, err := s.client.SimulateNodeFailure(r.Context(), &rpcpb.SimulateNodeFailureRequest{NodeId: nodeID})
	if err != nil || resp.GetError() != "" {
		reason := resp.GetError()
		if err != nil {
			reason = err.Error()
		}
		data.RecoveryError = "Handbook could not be generated: " + reason
		s.render(w, "recovery_handbook_page", s.withAuthFields(r, data))
		return
	}

	quorumFact := fromRPCQuorumFact(resp.GetQuorum())
	if resp.GetQuorum() == nil || !recovery.ValidQuorumFact(quorumFact) {
		data.RecoveryError = "Handbook could not be generated: cluster raft configuration appears empty or " +
			"internally inconsistent - this may indicate raftd's own configuration query failed silently " +
			"(a known limitation, see ADR-0056) rather than a genuine finding."
		s.render(w, "recovery_handbook_page", s.withAuthFields(r, data))
		return
	}

	var owned []recovery.ResourceFact
	for _, res := range resp.GetOwnedResources() {
		owned = append(owned, fromRPCResourceFact(res))
	}
	var replicaBacked []recovery.ReplicaBackedFact
	for _, res := range resp.GetReplicaBackedResources() {
		replicaBacked = append(replicaBacked, fromRPCReplicaBackedFact(res))
	}
	var images []recovery.ImageFact
	for _, img := range resp.GetImageAvailability() {
		images = append(images, fromRPCImageFact(img))
	}

	localNodeID := statusResp.GetManagerNodeId()
	nodeContext := s.gatherNodeContext(r.Context(), statusResp, localNodeID, owned, replicaBacked)
	evidenceEnd := time.Now()

	inputs := recovery.Inputs{
		TargetNodeID:    nodeID,
		IsCurrentLeader: statusResp.GetRaftLeaderId() == nodeID,
		Quorum:          quorumFact,
		OwnedResources:  owned,
		ReplicaBacked:   replicaBacked,
		Images:          images,
		NodeContext:     nodeContext,
	}
	hb := recovery.BuildHandbook(inputs)

	data.RecoveryFingerprint = computeRecoveryFingerprint(inputs, hb.Steps, data.RecoveryMembers)
	data.RecoveryGeneratedAt = evidenceEnd.Format("2006-01-02 15:04:05 MST")
	data.RecoveryEvidenceStart = evidenceStart.Format("2006-01-02 15:04:05 MST")
	data.RecoveryEvidenceEnd = evidenceEnd.Format("2006-01-02 15:04:05 MST")
	for _, st := range hb.Steps {
		data.RecoverySteps = append(data.RecoverySteps, fromRecoveryStep(st))
	}
	for _, nc := range nodeContext {
		data.RecoveryNodeContext = append(data.RecoveryNodeContext, fromRecoveryNodeContext(nc))
	}

	// Evidence appendix: the exact same already-fetched resp, converted
	// via simulate.go's own existing view types/fromRPCXxx functions -
	// zero duplication, zero extra RPC calls.
	data.RecoveryQuorum = fromRPCQuorumImpact(resp.GetQuorum())
	for _, res := range resp.GetOwnedResources() {
		data.RecoveryOwnedResources = append(data.RecoveryOwnedResources, fromRPCOwnedResourceImpact(res))
	}
	for _, res := range resp.GetReplicaBackedResources() {
		data.RecoveryReplicaBacked = append(data.RecoveryReplicaBacked, fromRPCReplicaBackedImpact(res))
	}
	for _, img := range resp.GetImageAvailability() {
		data.RecoveryImageAvailability = append(data.RecoveryImageAvailability, fromRPCImageAvailability(img))
	}

	s.render(w, "recovery_handbook_page", s.withAuthFields(r, data))
}
