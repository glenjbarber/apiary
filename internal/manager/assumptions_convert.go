package manager

import (
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/assumptions"
)

// assumptionKindToRPC/rpcKindToAssumption translate between
// internal/assumptions' plain-string Kind vocabulary and the
// AssumptionKind proto enum. An unrecognized value on either side maps
// to UNSPECIFIED/"" rather than panicking - defensive against a future
// mismatch between this map and the proto file, never silently wrong.
var assumptionKindToRPC = map[string]rpcpb.AssumptionKind{
	assumptions.KindPeerManagerRPCSucceeded:  rpcpb.AssumptionKind_ASSUMPTION_KIND_PEER_MANAGER_RPC_SUCCEEDED,
	assumptions.KindPeerSecurityPathAccepted: rpcpb.AssumptionKind_ASSUMPTION_KIND_PEER_SECURITY_PATH_ACCEPTED,
	assumptions.KindNATUplinkDefaultRoute:    rpcpb.AssumptionKind_ASSUMPTION_KIND_NAT_UPLINK_DEFAULT_ROUTE,
	assumptions.KindReplicaBhyveConfigured:   rpcpb.AssumptionKind_ASSUMPTION_KIND_REPLICA_BHYVE_CONFIGURED,
	assumptions.KindReplicaNetworkBridgeUp:   rpcpb.AssumptionKind_ASSUMPTION_KIND_REPLICA_NETWORK_BRIDGE_UP,
}

var rpcKindToAssumption = func() map[rpcpb.AssumptionKind]string {
	out := make(map[rpcpb.AssumptionKind]string, len(assumptionKindToRPC))
	for k, v := range assumptionKindToRPC {
		out[v] = k
	}
	return out
}()

func toRPCAssumptionKind(k string) rpcpb.AssumptionKind {
	return assumptionKindToRPC[k] // zero value (UNSPECIFIED) on a miss
}

func fromRPCAssumptionKind(k rpcpb.AssumptionKind) string {
	return rpcKindToAssumption[k] // zero value ("") on a miss
}

func toRPCSubjectKind(k assumptions.SubjectKind) rpcpb.AssumptionSubjectKind {
	switch k {
	case assumptions.SubjectKindNode:
		return rpcpb.AssumptionSubjectKind_ASSUMPTION_SUBJECT_KIND_NODE
	case assumptions.SubjectKindVM:
		return rpcpb.AssumptionSubjectKind_ASSUMPTION_SUBJECT_KIND_VM
	case assumptions.SubjectKindJail:
		return rpcpb.AssumptionSubjectKind_ASSUMPTION_SUBJECT_KIND_JAIL
	default:
		return rpcpb.AssumptionSubjectKind_ASSUMPTION_SUBJECT_KIND_UNSPECIFIED
	}
}

func fromRPCSubjectKind(k rpcpb.AssumptionSubjectKind) assumptions.SubjectKind {
	switch k {
	case rpcpb.AssumptionSubjectKind_ASSUMPTION_SUBJECT_KIND_NODE:
		return assumptions.SubjectKindNode
	case rpcpb.AssumptionSubjectKind_ASSUMPTION_SUBJECT_KIND_VM:
		return assumptions.SubjectKindVM
	case rpcpb.AssumptionSubjectKind_ASSUMPTION_SUBJECT_KIND_JAIL:
		return assumptions.SubjectKindJail
	default:
		return ""
	}
}

func toRPCAssumptionStatus(s assumptions.Status) rpcpb.AssumptionStatus {
	switch s {
	case assumptions.StatusTrue:
		return rpcpb.AssumptionStatus_ASSUMPTION_STATUS_TRUE
	case assumptions.StatusFalse:
		return rpcpb.AssumptionStatus_ASSUMPTION_STATUS_FALSE
	case assumptions.StatusUnknown:
		return rpcpb.AssumptionStatus_ASSUMPTION_STATUS_UNKNOWN
	case assumptions.StatusNotApplicable:
		return rpcpb.AssumptionStatus_ASSUMPTION_STATUS_NOT_APPLICABLE
	default:
		return rpcpb.AssumptionStatus_ASSUMPTION_STATUS_UNSPECIFIED
	}
}

func toRPCAssumptionKey(k assumptions.Key) *rpcpb.AssumptionKey {
	return &rpcpb.AssumptionKey{
		Kind:             toRPCAssumptionKind(k.Kind),
		SubjectKind:      toRPCSubjectKind(k.SubjectKind),
		SubjectId:        k.SubjectID,
		DependencyId:     k.DependencyID,
		Qualifier:        k.Qualifier,
		ObservedByNodeId: k.ObservedByNodeID,
	}
}

func fromRPCAssumptionKey(k *rpcpb.AssumptionKey) assumptions.Key {
	return assumptions.Key{
		Kind:             fromRPCAssumptionKind(k.GetKind()),
		SubjectKind:      fromRPCSubjectKind(k.GetSubjectKind()),
		SubjectID:        k.GetSubjectId(),
		DependencyID:     k.GetDependencyId(),
		Qualifier:        k.GetQualifier(),
		ObservedByNodeID: k.GetObservedByNodeId(),
	}
}

// toRPCAssumptionResult computes the EFFECTIVE status fresh, from
// observed_status + staleness - see ListAssumptionResults' own doc
// comment for why this structural split (not a `stale` flag alone) is
// what actually keeps stale evidence from silently counting as true.
// ANY stored status, including NOT_APPLICABLE, collapses to UNKNOWN once
// stale.
func toRPCAssumptionResult(r assumptions.Result, now time.Time, staleAfter time.Duration) *rpcpb.AssumptionResult {
	observed := toRPCAssumptionStatus(r.ObservedStatus)
	stale := staleAfter > 0 && now.Sub(r.LastObservedAt) > staleAfter
	effective := observed
	if stale {
		effective = rpcpb.AssumptionStatus_ASSUMPTION_STATUS_UNKNOWN
	}
	return &rpcpb.AssumptionResult{
		Key:                toRPCAssumptionKey(r.Key),
		ObservedStatus:     observed,
		Status:             effective,
		ReasonCode:         r.ReasonCode,
		Detail:             r.Detail,
		LastObservedAtUnix: r.LastObservedAt.Unix(),
		Stale:              stale,
	}
}

func toRPCAssumptionHistoryEntry(h assumptions.HistoryEntry) *rpcpb.AssumptionHistoryEntry {
	return &rpcpb.AssumptionHistoryEntry{
		Key:            toRPCAssumptionKey(h.Key),
		ObservedStatus: toRPCAssumptionStatus(h.ObservedStatus),
		ReasonCode:     h.ReasonCode,
		Detail:         h.Detail,
		RecordedAtUnix: h.RecordedAt.Unix(),
	}
}
