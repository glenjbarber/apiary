// ADR-0147 Part 4's replicated peer trust store: the three command
// arms, the pin lifecycle, and the read side the derived peer-ca.pem
// writer consumes.
//
// Why this is replicated state at all, and not a field on the manager
// that accepted the certificate: a trust anchor held in one managerd's
// memory is a trust anchor that disappears when that Comb steps aside.
// A Colony whose members disagree about which certificates they trust
// is not a Colony with TLS, it is a Colony where a forwarded RPC
// verifies on one node and fails on the next, for no reason an operator
// did anything. So the pins move through the log like everything else
// in this package.
//
// Why the pin is a SEPARATE record from raft membership: the two
// events are not simultaneous, and a single record cannot express
// "this certificate is trusted but this Comb is not yet a member".
// The pin is written when the target accepts the certificate - before
// authorization, before the second PIN, before AddVoter. AddVoter
// promotes it. Conflating them would mean either a refused request
// leaving a permanent pin, or a pin implying membership. See
// TrustedPeer.is_voter's own doc comment in api/internalpb/state.proto.
//
// Why the certificate is re-evaluated HERE rather than trusted from
// whatever wrote the command: the writer is one managerd, and every
// other voter applies the same log entry without having seen the PEM
// arrive over a socket. Validation at the FSM boundary is what makes a
// follower's copy of the store something it actually decided rather
// than something it was told.

package raft

import (
	"fmt"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/hostcert"
)

// applyPinTrustedPeer writes one entry into the trust store.
//
// Create-only, and every check below names itself in the refusal,
// because each one is a different operator problem: a pin of the wrong
// certificate is a typo, a pin of an expired certificate is a Combs
// that has been forgotten, and a pin whose fingerprint disagrees with
// its own PEM is a bug or an attack, never a misconfiguration.
//
// The certificate is evaluated as of pinned_at_unix, NOT the wall
// clock. That is deliberate and it is what makes this arm deterministic:
// this function runs on every voter and again on every replay after a
// restart or a snapshot catch-up, so consulting time.Now() here would
// let a replay years later reject an entry that originally succeeded
// and leave replicas disagreeing about the store's contents - the same
// failure applyResolvePendingJoinRequest's own expiry comment describes.
// The wall clock is consulted once, by the manager that decides to pin,
// and only ever as pinned_at_unix.
func (f *FSM) applyPinTrustedPeer(index uint64, peer *internalpb.TrustedPeer) *FSMApplyResult {
	if peer.GetNodeId() == "" {
		return &FSMApplyResult{Index: index, Error: "PinTrustedPeer: node_id must be set"}
	}
	if peer.GetCombName() == "" {
		return &FSMApplyResult{Index: index, Error: "PinTrustedPeer: comb_name must be set"}
	}
	if peer.GetPinnedBy() == "" {
		return &FSMApplyResult{Index: index, Error: "PinTrustedPeer: pinned_by must be set"}
	}
	if peer.GetPinnedAtUnix() <= 0 {
		return &FSMApplyResult{Index: index, Error: "PinTrustedPeer: pinned_at_unix must be set"}
	}
	if peer.GetFingerprint() == "" {
		return &FSMApplyResult{Index: index, Error: "PinTrustedPeer: fingerprint must be set"}
	}
	// A pin that claims voter membership is refused rather than
	// accepted-and-ignored. This command is the "the operator compared
	// the fingerprints" step, which happens before authorization and
	// before AddVoter; letting it set is_voter would create a pin that
	// implies membership, which is precisely the conflation
	// TrustedPeer.is_voter's own doc comment exists to prevent.
	if peer.GetIsVoter() {
		return &FSMApplyResult{Index: index, Error: "PinTrustedPeer: is_voter must be false - membership is recorded by SetTrustedPeerVoter after AddVoter, never by the act of pinning a certificate"}
	}
	// Checked before the duplicate check so that a caller retrying an
	// identical pin learns why it is refused. A repeat of a pin that
	// already exists is a different problem from a repeat of a pin that
	// was never written, and conflating them would send an operator to
	// the wrong place.
	if _, exists := f.trustedPeers[peer.GetNodeId()]; exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"PinTrustedPeer: node_id %q is already pinned; unpin it first, so replacing a pinned certificate is two visible acts in the log rather than one invisible one",
			peer.GetNodeId())}
	}

	// The same fail-closed gate internal/manager applies to a presented
	// certificate, with KeyPEM left empty because a peer's private key
	// is not here and must not be asked for. RequireDNSSAN and
	// RequireLoopbackSAN are both true because this is a Comb serving
	// certificate, exactly as they are for a Comb's own.
	info, err := hostcert.Evaluate([]byte(peer.GetCertPem()), hostcert.EvaluateOptions{
		Now:                time.Unix(peer.GetPinnedAtUnix(), 0).UTC(),
		RequireDNSSAN:      true,
		RequireLoopbackSAN: true,
	})
	if err != nil {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("PinTrustedPeer: %v", err)}
	}
	// Re-derived here rather than trusted. A pin whose fingerprint does
	// not match the certificate it carries would be a store that
	// advertises one identity and verifies another, which is worse than
	// no store at all: the derived peer-ca.pem is built from these
	// entries, so a mismatch is a trust anchor for an identity nobody
	// compared. The check costs one SHA-256 and removes the whole class.
	if peer.GetFingerprint() != info.Fingerprint {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"PinTrustedPeer: fingerprint %q does not match the digest of the certificate supplied (%s); a pin must record what it actually pins",
			peer.GetFingerprint(), info.Fingerprint)}
	}
	// Same reasoning for the expiry. A pin claiming a longer life than
	// its certificate has is how a store ends up still trusting a leaf
	// after the leaf is dead, and the reader that would notice is a TLS
	// handshake failing with a name nobody recognises.
	if peer.GetNotAfterUnix() != info.NotAfter.Unix() {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"PinTrustedPeer: not_after_unix %d does not match the certificate's own NotAfter %d",
			peer.GetNotAfterUnix(), info.NotAfter.Unix())}
	}

	f.trustedPeers[peer.GetNodeId()] = proto.Clone(peer).(*internalpb.TrustedPeer)
	return &FSMApplyResult{Index: index, TrustedPeer: f.trustedPeers[peer.GetNodeId()]}
}

// applySetTrustedPeerVoter records that a pinned Comb is (or is no
// longer) a raft member. Only AddVoter calls it with true - raft has
// no demotion, so the false case exists to make the promotion an
// explicit recorded act rather than a side effect of a pin being
// written.
func (f *FSM) applySetTrustedPeerVoter(index uint64, req *internalpb.SetTrustedPeerVoter) *FSMApplyResult {
	if req.GetNodeId() == "" {
		return &FSMApplyResult{Index: index, Error: "SetTrustedPeerVoter: node_id must be set"}
	}
	existing, exists := f.trustedPeers[req.GetNodeId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"SetTrustedPeerVoter: node_id %q is not pinned; a Comb is made a member before its certificate is ever evaluated, and recording that here would mean trusting a certificate nobody compared",
			req.GetNodeId())}
	}
	updated := proto.Clone(existing).(*internalpb.TrustedPeer)
	updated.IsVoter = req.GetIsVoter()
	f.trustedPeers[req.GetNodeId()] = updated
	return &FSMApplyResult{Index: index, TrustedPeer: updated}
}

// applyUnpinTrustedPeer drops the entry - the removal half of "a store
// that can only grow accumulates dead certificates until one expires by
// accident".
//
// Refuses an unknown node_id rather than treating it as a no-op. A
// silent success here would read in the operator's terminal as "it is
// gone" for an entry that was never there, which is the one case where
// the operator most needs to be told.
func (f *FSM) applyUnpinTrustedPeer(index uint64, cmd *internalpb.UnpinTrustedPeer) *FSMApplyResult {
	nodeID := cmd.GetNodeId()
	if nodeID == "" {
		return &FSMApplyResult{Index: index, Error: "UnpinTrustedPeer: node_id must be set"}
	}
	existing, exists := f.trustedPeers[nodeID]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("UnpinTrustedPeer: node_id %q is not pinned", nodeID)}
	}
	delete(f.trustedPeers, nodeID)
	return &FSMApplyResult{Index: index, TrustedPeer: existing}
}

// promotePin marks an existing pin as belonging to a member. A no-op
// when there is no pin, which is a real and common case: a Colony
// upgraded from a pre-ADR-0147 build already has members, and their
// certificates were never pinned. Refusing their approvals would make
// the upgrade path worse than the bug, and silently creating a pin
// would trust a certificate nobody compared. So the approval stands and
// the pin is absent, and that absence is visible in the store.
func (f *FSM) promotePin(nodeID string) {
	existing, ok := f.trustedPeers[nodeID]
	if !ok {
		return
	}
	updated := proto.Clone(existing).(*internalpb.TrustedPeer)
	updated.IsVoter = true
	f.trustedPeers[nodeID] = updated
}

// dropUnpromotedPin removes a pin that never became a member's, so
// the store does not accumulate one entry per Comb that ever asked to
// join. A promoted pin is deliberately left alone: a member's trust
// anchor is not contingent on a join request still existing.
func (f *FSM) dropUnpromotedPin(nodeID string) {
	existing, ok := f.trustedPeers[nodeID]
	if !ok || existing.GetIsVoter() {
		return
	}
	delete(f.trustedPeers, nodeID)
}

// TrustedPeerExpired reports whether a pin's own certificate has
// lapsed at now.
//
// A predicate, not a sweep, for the same reason ColonyJoinWindowLive
// is one: an expired pin is still IN the store, and every reader
// decides the same way from the same replicated field. Nothing has to
// run for the store to be correct.
//
// The entry is kept after this returns true. Sweeping it would make a
// clear "this peer's certificate expired" become an anonymous TLS
// handshake failure against a host that is no longer in any trust
// store at all - a strictly worse diagnostic for the same underlying
// fact, and the ADR records expiry as something an operator has to see.
func TrustedPeerExpired(p *internalpb.TrustedPeer, now int64) bool {
	return p.GetNotAfterUnix() <= now
}

// ListTrustedPeers returns every pinned peer, sorted by node ID.
//
// Sorted rather than in map order because both callers are byte
// comparators: the derived peer-ca.pem writer has to produce the same
// file on every Comb, and a page that reorders itself between renders
// looks like a store that is mutating under the operator.
func (f *FSM) ListTrustedPeers() []*internalpb.TrustedPeer {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.listTrustedPeersLocked()
}

// listTrustedPeersLocked is ListTrustedPeers without the locking. The
// caller must hold f.mu.
func (f *FSM) listTrustedPeersLocked() []*internalpb.TrustedPeer {
	ids := make([]string, 0, len(f.trustedPeers))
	for id := range f.trustedPeers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	peers := make([]*internalpb.TrustedPeer, 0, len(ids))
	for _, id := range ids {
		peers = append(peers, proto.Clone(f.trustedPeers[id]).(*internalpb.TrustedPeer))
	}
	return peers
}
