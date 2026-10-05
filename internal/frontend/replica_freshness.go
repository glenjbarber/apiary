package frontend

import (
	"context"
	"fmt"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

type peerHASTStatusClient interface {
	GetLocalHASTResourceStatus(ctx context.Context, addr, resourceName string) (*rpcpb.GetLocalHASTResourceStatusResponse, error)
}

type hastObservationView struct {
	NodeID      string
	Role        string
	Status      string
	Replication string
	Dirty       string
	ObservedAt  string

	// ObservedAgo is the same instant as ObservedAt, rendered as the
	// relative age an operator actually acts on. It is deliberately a
	// second field rather than a re-parse of ObservedAt in the template:
	// the two are formatted from one clock reading and one duration, and
	// a template that recomputed either could not tell a genuine age
	// from a mis-parsed timestamp.
	//
	// It is empty when there is no observation to age - a transport
	// error, a node that reported none, or a response carrying no
	// timestamp at all. That is the unknown case and the template renders
	// it as unknown; it is never a zero age, which would read as "just
	// observed" for a node that was never reached.
	ObservedAgo string

	Error string
}

type replicaFreshnessView struct {
	Configured   bool
	Verdict      string
	Explanation  string
	Observations []hastObservationView
}

// replicaFreshness queries both configured HAST endpoints directly. It does
// not infer replication health from Raft's replicated configuration, and
// does not convert hastctl counters into an RPO estimate.
func (s *Server) replicaFreshness(ctx context.Context, resourceType, id, owner, replica string) replicaFreshnessView {
	if replica == "" {
		return replicaFreshnessView{Verdict: "unprotected", Explanation: "No HAST replica is configured; Apiary cannot provide a replica freshness or recovery-point claim."}
	}
	resourceName := resourceType + "-" + id
	view := replicaFreshnessView{Configured: true, Verdict: "unknown", Explanation: "Freshness is unknown until both nodes return matching, complete HAST observations."}
	status, err := s.client.Status(ctx, &rpcpb.StatusRequest{})
	localNodeID := ""
	if err == nil {
		localNodeID = status.GetManagerNodeId()
	}
	for _, nodeID := range []string{owner, replica} {
		observation := hastObservationView{NodeID: nodeID}
		checkCtx, cancel := context.WithTimeout(ctx, nodeContextTimeout)
		var response *rpcpb.GetLocalHASTResourceStatusResponse
		var callErr error
		switch {
		case nodeID == "" || localNodeID == "":
			callErr = fmt.Errorf("node identity is unavailable")
		case nodeID == localNodeID:
			response, callErr = s.client.GetLocalHASTResourceStatus(checkCtx, &rpcpb.GetLocalHASTResourceStatusRequest{ResourceName: resourceName})
		default:
			peer, ok := s.peers.(peerHASTStatusClient)
			if !ok || peer == nil {
				callErr = fmt.Errorf("peer HAST status forwarding is unavailable")
			} else {
				callCtx, callCancel := context.WithTimeout(checkCtx, nodeContextTimeout)
				response, callErr = peer.GetLocalHASTResourceStatus(callCtx, s.peerAddr(nodeID), resourceName)
				callCancel()
			}
		}
		cancel()
		if callErr != nil {
			observation.Error = callErr.Error()
			view.Observations = append(view.Observations, observation)
			continue
		}
		if response == nil {
			observation.Error = "node returned no HAST observation"
		} else {
			observation.Role = response.GetRole()
			observation.Status = response.GetResourceStatus()
			observation.Replication = response.GetReplication()
			observation.Dirty = response.GetDirty()
			if response.GetObservedAtUnix() > 0 {
				observed := time.Unix(response.GetObservedAtUnix(), 0).UTC()
				observation.ObservedAt = observed.Format(time.RFC3339)
				// Relative to the moment both ends have been asked and
				// answered, not to the start of the gather: an operator
				// reads this as "how old is what is on this page", and
				// the page finishes rendering after the slow end does.
				observation.ObservedAgo = formatObservationAge(time.Since(observed))
			}
			observation.Error = response.GetError()
		}
		view.Observations = append(view.Observations, observation)
	}
	view.Verdict, view.Explanation = assessReplicaFreshness(view.Observations)
	return view
}

func assessReplicaFreshness(observations []hastObservationView) (string, string) {
	if len(observations) != 2 || observations[0].Error != "" || observations[1].Error != "" || observations[0].ObservedAt == "" || observations[1].ObservedAt == "" {
		return "unknown", "At least one node could not provide fresh local HAST evidence. Unknown is not treated as healthy."
	}
	primary, secondary := observations[0], observations[1]
	if primary.Role != "primary" || secondary.Role != "secondary" {
		return "degraded", "The observed HAST roles do not match the configured owner and replica."
	}
	if primary.Status != "complete" || secondary.Status != "complete" {
		return "degraded", "At least one node reports HAST status other than complete."
	}
	if primary.Replication == "" || secondary.Replication == "" || primary.Replication != secondary.Replication {
		return "unknown", "HAST status is complete on both nodes, but replication mode is missing or inconsistent."
	}
	return "complete-observed", "Both nodes recently reported matching HAST roles, complete status, and the same replication mode. This is a point-in-time observation, not an RPO measurement or failover guarantee."
}
