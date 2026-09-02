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
	"fmt"
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

	// ListISONames/RequestISOPush implement on-demand image fetching
	// (ADR-0041): when a VM/jail names an ISO/base image this node's
	// own isostore doesn't have yet, the reconciler asks every other
	// known node's ListISONames whether it has the file, then asks the
	// first one that does to push it here via RequestISOPush - reusing
	// exactly the same peer-to-peer UploadISO/PushISOTo mechanism
	// ADR-0040 originally built for a manual, browser-triggered copy,
	// just triggered automatically by the reconciler instead of a
	// button click. Names, not full rpcpb.ISOInfo, for the same
	// decoupling-from-wire-types reason ReportVMPhase's plain-string
	// phase already establishes.
	ListISONames(ctx context.Context, addr string) ([]string, error)
	RequestISOPush(ctx context.Context, addr, name, targetNodeID string) error
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

// fetchImageFromPeer looks for name across every other known cluster
// node (via resolvePeerAddresses, the same raft-membership-derived
// address list hast.go's own HAST role resolution already reuses) and,
// on the first node reporting it, asks that node to push it here
// (ADR-0041) - the reconciler's own automatic counterpart to ADR-0040's
// original browser-triggered copy, invoked when a VM/jail names an
// ISO/base image this node's own isostore doesn't have yet. Returns an
// error if Peers isn't configured, no peer reports having the file, or
// the push itself fails; every unreachable/errored peer along the way
// is simply skipped rather than aborting the whole search.
func (r *Reconciler) fetchImageFromPeer(ctx context.Context, name string) error {
	if r.Peers == nil {
		return fmt.Errorf("image %q not found locally and no peer forwarding is configured on this node", name)
	}
	addrs, err := r.resolvePeerAddresses(ctx)
	if err != nil {
		return fmt.Errorf("resolving peer addresses: %w", err)
	}
	port := r.peerManagerdPort()
	for nodeID, host := range addrs {
		if nodeID == r.LocalNodeID {
			continue
		}
		addr := net.JoinHostPort(host, port)
		names, err := r.Peers.ListISONames(ctx, addr)
		if err != nil {
			continue
		}
		for _, n := range names {
			if n != name {
				continue
			}
			if err := r.Peers.RequestISOPush(ctx, addr, name, r.LocalNodeID); err != nil {
				return fmt.Errorf("fetching %q from %s: %w", name, nodeID, err)
			}
			return nil
		}
	}
	return fmt.Errorf("image %q not found on any known cluster node", name)
}
