package frontend

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/invariant"
)

// Gathering the HAST evidence the two HAST invariants now resolve from.
//
// ADR-0060 shipped hast-dual-primary and cell-recoverability capped at
// Unknown on a stated premise - "live HAST role/sync status has no RPC
// exposure anywhere in this codebase" - and disclosed the cap rather
// than dropping it. ADR-0119 retired that premise by adding
// GetLocalHASTResourceStatus, a node-local read-only report of one
// node's own hastctl view that a peer can reach without leader
// forwarding. This file is the caller side of that: it asks the
// questions, and internal/invariant does the reading.
//
// The gathering is the same shape internal/frontend/replica_freshness.go
// already uses for a single resource's two ends, widened to a bounded
// concurrent fan-out across every replica-backed resource. The rule
// that shapes it is replica_freshness's own: a question that could not
// be asked is recorded as not asked, and a question that was asked and
// did not answer is recorded separately, because the two are different
// facts and neither is evidence that a resource is healthy.

// hastGatherLimit and hastGatherOverallTimeout bound the fan-out. They
// mirror the nodeContextLimit / nodeContextOverallTimeout pair the other
// bounded gathers in this package already use, so one page render cannot
// turn into an unbounded dial storm. peerHASTStatusClient is reused from
// replica_freshness.go rather than redeclared: both files dial the same
// RPC, and two identical interfaces would be two contracts to keep in
// step for no reason.
//
// The overall budget is read through a function rather than captured in
// a var at init: nodeContextOverallTimeout is a var precisely so a
// caller can bound the whole page (recovery_handbook_test.go,
// invariants_test.go), and a snapshot taken once at package init would
// silently ignore that bound and leave the HAST fan-out running at the
// production 10s while the rest of the page runs at 50ms. The HAST
// gather must not be the one gather on the page that escapes the page's
// own deadline.
const hastGatherLimit = nodeContextLimit

// hastGatherOverallTimeout is this fan-out's share of the page's
// overall node-context budget.
func hastGatherOverallTimeout() time.Duration { return nodeContextOverallTimeout }

// hastResource is one replica-backed resource to ask about, with the
// two configured ends named exactly as raft state records them.
type hastResource struct {
	ID            string // "vm-1" / "jail-1", the strict GetLocalHASTResourceStatus resource name
	Name          string // for the explanation text; never load-bearing
	Kind          string // "vm" | "jail"
	OwnerNodeID   string
	ReplicaNodeID string
}

// gatherHASTResource asks both configured ends of one resource for
// their own live view, independently. Neither end's answer is
// inferred from the other's, which is the point of asking both: a
// single node's view cannot rule out a dual primary on its own.
//
// The two ends are asked concurrently and each is bounded separately,
// so one unresponsive node costs its own budget rather than the whole
// resource's. A failure of either is recorded, never raised: the
// invariant's contract is that silence is reported as silence.
func (s *Server) gatherHASTResource(ctx context.Context, res hastResource, localNodeID string) invariant.HASTPrimarySpec {
	spec := invariant.HASTPrimarySpec{
		ID: res.ID, Name: res.Name, Kind: res.Kind,
		OwnerNodeID: res.OwnerNodeID, ReplicaNodeID: res.ReplicaNodeID,
	}
	observations := make([]invariant.HASTObservation, 2)
	ends := []struct {
		nodeID string
		out    *invariant.HASTObservation
	}{
		{res.OwnerNodeID, &spec.Owner},
		{res.ReplicaNodeID, &spec.Replica},
	}

	overallCtx, cancel := context.WithTimeout(ctx, hastGatherOverallTimeout())
	defer cancel()

	var wg sync.WaitGroup
	for i, end := range ends {
		observations[i].NodeID = end.nodeID
		if end.nodeID == "" {
			observations[i].Detail = "this resource names no " + endName(i) + " node, so there is no node to ask"
			*end.out = observations[i]
			continue
		}
		wg.Add(1)
		go func(i int, nodeID string, out *invariant.HASTObservation) {
			defer wg.Done()
			checkCtx, checkCancel := context.WithTimeout(overallCtx, nodeContextTimeout)
			defer checkCancel()
			*out = s.askLocalHASTStatus(checkCtx, res.ID, nodeID, localNodeID)
		}(i, end.nodeID, end.out)
	}
	wg.Wait()
	return spec
}

func endName(i int) string {
	if i == 0 {
		return "owner"
	}
	return "replica"
}

// askLocalHASTStatus queries exactly one node's own hastctl view,
// locally or over the peer link, and renders the answer - or the reason
// there is no answer - as an invariant.HASTObservation.
//
// Attempted is the load-bearing distinction here. It is set as soon as
// a query is genuinely issued, and left false when none could be: no
// local node identity, no peer forwarding configured, or the node being
// asked is the same one the RPC is local to. A caller that cannot tell
// those apart would report "we checked and it was fine" for a node it
// never reached.
func (s *Server) askLocalHASTStatus(ctx context.Context, resourceName, nodeID, localNodeID string) invariant.HASTObservation {
	obs := invariant.HASTObservation{NodeID: nodeID}

	// A local read is only local if this node knows it is local. Without
	// a local node id the caller cannot tell the two paths apart, and
	// dialling itself would be a guess.
	if localNodeID == "" {
		obs.Detail = "this node's own id is unknown, so a local HAST read cannot be distinguished from a peer one"
		return obs
	}
	if nodeID == localNodeID {
		obs.Attempted = true
		obs.Observed = true
		resp, err := s.client.GetLocalHASTResourceStatus(ctx, &rpcpb.GetLocalHASTResourceStatusRequest{ResourceName: resourceName})
		return observationFromResponse(obs, resp, err)
	}

	peer, ok := s.peers.(peerHASTStatusClient)
	if !ok || peer == nil || s.peers == nil {
		obs.Detail = "peer forwarding is not configured, so " + nodeID + "'s own HAST view cannot be reached from here"
		return obs
	}

	obs.Attempted = true
	obs.Observed = true
	peerCtx, peerCancel := context.WithTimeout(ctx, nodeContextTimeout)
	defer peerCancel()
	resp, err := peer.GetLocalHASTResourceStatus(peerCtx, s.peerAddr(nodeID), resourceName)
	return observationFromResponse(obs, resp, err)
}

// observationFromResponse turns a wire answer into the observation
// internal/invariant reads. Observed is cleared whenever the answer did
// not actually carry a hastctl view - a transport error, a manager-side
// refusal, or a response whose only content was that error - so that
// Observed never means merely "a reply arrived".
func observationFromResponse(obs invariant.HASTObservation, resp *rpcpb.GetLocalHASTResourceStatusResponse, err error) invariant.HASTObservation {
	switch {
	case err != nil:
		obs.Observed = false
		obs.Detail = err.Error()
		return obs
	case resp == nil:
		obs.Observed = false
		obs.Detail = "the node returned no HAST observation"
		return obs
	case resp.GetError() != "":
		obs.Observed = false
		obs.Detail = resp.GetError()
		return obs
	}
	obs.Observed = true
	obs.Role = resp.GetRole()
	obs.Status = resp.GetResourceStatus()
	obs.Replication = resp.GetReplication()
	return obs
}

// gatherHASTInvariants gathers both HAST invariants' evidence in one
// bounded, concurrent pass and returns the evaluations
// internal/invariant computes from it.
//
// Both invariants read the same two ends of the same resources, so
// gathering them together is one fan-out rather than two. dualPrimary
// and recoverability receive the same specs; each keeps its own zero
// value for any resource that could not be gathered at all, which is
// what keeps a silent resource Unknown in both rather than absent from
// one of them.
//
// The returned error is non-nil only when the gathering itself could
// not be set up - no local node identity, say. Individual resource
// failures are reported inside the evaluations, which is the whole
// point of the three-state vocabulary.
func (s *Server) gatherHASTInvariants(ctx context.Context, resources []hastResource, localNodeID string) ([]invariant.Evaluation, []invariant.Evaluation, []invariant.ResourceFact, error) {
	if len(resources) == 0 {
		return nil, nil, nil, nil
	}
	if localNodeID == "" {
		return nil, nil, nil, fmt.Errorf("this node's own id is unknown, so no HAST evidence can be gathered")
	}

	// Sorted so two renders over the same membership ask in the same
	// order, and truncated at the same bound every other gather in this
	// package uses, so one page render cannot become a dial storm.
	sorted := make([]hastResource, len(resources))
	copy(sorted, resources)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	truncated := false
	if len(sorted) > hastGatherLimit {
		sorted = sorted[:hastGatherLimit]
		truncated = true
	}

	overallCtx, cancel := context.WithTimeout(ctx, hastGatherOverallTimeout())
	defer cancel()

	specs := make([]invariant.HASTPrimarySpec, len(sorted))
	var wg sync.WaitGroup
	for i, res := range sorted {
		wg.Add(1)
		go func(i int, res hastResource) {
			defer wg.Done()
			specs[i] = s.gatherHASTResource(overallCtx, res, localNodeID)
		}(i, res)
	}
	wg.Wait()

	// Every resource that could not be gathered is still named to the
	// dual-primary invariant, so the invariant reports a reason for
	// every replica-backed resource the Colony has rather than silently
	// omitting the ones past the bound.
	unobserved := make([]string, 0, len(resources))
	if truncated {
		for _, res := range resources {
			unobserved = append(unobserved, res.ID)
		}
	} else {
		for _, spec := range specs {
			if spec.ID != "" {
				unobserved = append(unobserved, spec.ID)
			}
		}
	}

	// Cell recoverability is conjunctive, and its sync half is now
	// observable. Only a VM has a destination-capability signal anywhere
	// in this codebase, so a jail's fact carries no DestinationCapable
	// value and falls to Unknown on its own - which is the existing
	// behaviour, not a new restriction.
	facts := make([]invariant.ResourceFact, 0, len(specs))
	for _, spec := range specs {
		facts = append(facts, invariant.ResourceFact{
			ID: spec.ID, Name: spec.Name, Kind: spec.Kind, ReplicaNodeID: spec.ReplicaNodeID,
			ReplicaSync: spec.Replica,
		})
	}

	dualPrimary := invariant.EvaluateHASTDualPrimary(unobserved, specs...)
	recoverability := invariant.EvaluateCellRecoverability(facts)
	return dualPrimary, recoverability, facts, nil
}

// hastResourcesFromVMsJails turns the raft-replicated VM/jail metadata
// into the replica-backed resource list gatherHASTInvariants asks about.
//
// It is the structural half only: ReplicaNodeID decides whether a
// resource is in scope at all (a resource with no replica has no HAST
// pair and is not this invariant's business), and NodeID names the end
// believed to hold the writable role. Neither is evidence - both are
// what the Colony was told, not what any node's hastd currently says.
// The evidence half is what askLocalHASTStatus goes and fetches.
//
// A list that could not be fetched contributes no resources at all,
// rather than contributing the resources it might have: a VM list this
// call never received is not an empty VM list, and treating it as one
// would let a fetch failure look like a colony with nothing to check.
func hastResourcesFromVMsJails(vms []vmView, vmsErr string, jails []jailView, jailsErr string) []hastResource {
	var resources []hastResource
	if vmsErr == "" {
		for _, vm := range vms {
			if vm.ReplicaNodeID == "" {
				continue
			}
			resources = append(resources, hastResource{
				// The RPC's resource name is the kind prefix plus the
				// bare id (internal/manager's hastResourceName), not
				// the bare id, and it is matched strictly.
				ID: "vm-" + vm.ID, Name: vm.Name, Kind: "vm",
				OwnerNodeID: vm.NodeID, ReplicaNodeID: vm.ReplicaNodeID,
			})
		}
	}
	if jailsErr == "" {
		for _, j := range jails {
			if j.ReplicaNodeID == "" {
				continue
			}
			resources = append(resources, hastResource{
				ID: "jail-" + j.ID, Name: j.Name, Kind: "jail",
				OwnerNodeID: j.NodeID, ReplicaNodeID: j.ReplicaNodeID,
			})
		}
	}
	return resources
}

// hastResourceIDs names the same resources for the no-evidence path, so
// a caller that cannot gather at all still reports one Unknown per
// replica-backed resource rather than quietly showing nothing.
func hastResourceIDs(resources []hastResource) []string {
	ids := make([]string, 0, len(resources))
	for _, res := range resources {
		ids = append(ids, res.ID)
	}
	return ids
}

// hastSyncByID indexes the facts gatherHASTInvariants returned by
// resource, so cell-recoverability's sync half can be filled in from the
// evidence the HAST gather already fetched rather than from a second
// fan-out of the identical RPCs to the identical nodes.
//
// The map is keyed by the same resource name those facts carry, and a
// missing key is not an error: it means this resource was past the
// fan-out bound, and the zero HASTObservation it stands for is silence,
// which is what that resource's recoverability verdict must see.
func hastSyncByID(facts []invariant.ResourceFact) map[string]invariant.HASTObservation {
	if len(facts) == 0 {
		return nil
	}
	byID := make(map[string]invariant.HASTObservation, len(facts))
	for _, f := range facts {
		byID[f.ID] = f.ReplicaSync
	}
	return byID
}
