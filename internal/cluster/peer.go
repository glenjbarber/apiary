// Cross-node write forwarding (ADR-0029): a node's own raftd only
// accepts an Apply when it is itself the current raft leader - a real
// gap, found live, when a VM/jail's owning node and the raft leader
// are different nodes (previously true only during MigrateVM/
// MigrateJail testing, but really possible any time leadership moves
// for any reason). Rather than exposing raftd's internal API over the
// network (a real widening of its trust boundary - see CLAUDE.md's
// "no authentication... judged sufficient for now" stance on that
// socket), a rejected local Apply is instead forwarded to the leader
// node's own managerd, over ManagerService - already networked,
// already optionally API-key-gated (ADR-0023), so this reuses an
// existing trust boundary instead of opening a new one.
package cluster

import (
	"context"
	"net"
)

// peerReporter is the subset of *manager.PeerReporter the reconciler
// needs, for the same reason as every other manager interface in this
// package. Phase is passed as this package's own plain-string
// representation (PhaseCreating etc.) rather than an internalpb enum,
// matching VMPlacement/JailPlacement's own decoupling from the wire
// schema - the concrete implementation (internal/manager, which
// already deals in rpcpb types) does its own string<->enum mapping.
type peerReporter interface {
	ReportVMPhase(ctx context.Context, leaderAddr, id, phase, phaseError string) error
	ReportVMTeardownComplete(ctx context.Context, leaderAddr, id string) error
	ReportJailPhase(ctx context.Context, leaderAddr, id, phase, phaseError string) error
	ReportJailTeardownComplete(ctx context.Context, leaderAddr, id string) error
}

// defaultPeerManagerdPort is used when Reconciler.PeerManagerdPort is
// unset - matches managerd's own -rpc-addr default port
// (cmd/managerd's "127.0.0.1:17700"). Every node in a real deployment
// is expected to run managerd's external API on the same port
// (differing only by host), the same symmetric-configuration
// assumption hast.go's own Node.Name/peer-address resolution already
// makes.
const defaultPeerManagerdPort = "17700"

func (r *Reconciler) peerManagerdPort() string {
	if r.PeerManagerdPort == "" {
		return defaultPeerManagerdPort
	}
	return r.PeerManagerdPort
}

// resolvePeerManagerdAddr turns a raft leader_hint (the LEADER's raft
// transport address, e.g. "10.50.0.14:17600" - a different port than
// managerd's own external API) into that same node's managerd address,
// by keeping the host and substituting the configured/default
// managerd port. There is no separate node-address directory to
// consult for this - the raft bind address is the only per-node
// address this project already has on hand at the point a write is
// rejected.
func (r *Reconciler) resolvePeerManagerdAddr(leaderHint string) string {
	host, _, err := net.SplitHostPort(leaderHint)
	if err != nil {
		host = leaderHint
	}
	return net.JoinHostPort(host, r.peerManagerdPort())
}
