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
	// The certificate is evaluated as of pinned_at_unix, NOT the wall
	// clock, so that this arm reaches the identical decision on every
	// voter and on every replay. The CA anchor, when there is one, is
	// evaluated at the same instant for the same reason.
	now := time.Unix(peer.GetPinnedAtUnix(), 0).UTC()
	info, err := hostcert.Evaluate([]byte(peer.GetCertPem()), hostcert.EvaluateOptions{
		Now:                now,
		RequireDNSSAN:      true,
		RequireLoopbackSAN: true,
		// A Comb's certificate may be self-signed or issued by a
		// public CA. The Cloudflare/ACME work (ADR-0133) moved a
		// Colony's Combs onto publicly trusted certificates, and
		// refusing those here left the trust store permanently empty
		// on such a Colony - which left peer-ca.pem unwritten and
		// peer TLS with no anchors at all.
		//
		// The gate is still fail-closed. A self-signed certificate
		// must still verify its own signature, and the fingerprint
		// and expiry are still RE-DERIVED from the certificate below
		// rather than believed.
		AllowCAIssued: true,
		// The CA anchor, when the pin carries one. It is checked
		// against the certificate - it must be a CA, it must have
		// signed this certificate, and it must be inside its own
		// validity window - and it is the CA that becomes the
		// recorded trust anchor.
		CAAnchorPEM: []byte(peer.GetCaPem()),
	})
	if err != nil {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("PinTrustedPeer: %v", err)}
	}
	// The CA's fingerprint is RE-DERIVED here rather than trusted, for
	// the same reason the leaf's is: the writer is one managerd, and
	// every other voter applies the same log entry without having
	// seen the PEM arrive over a socket. A pin that stated a CA
	// fingerprint which is not the digest of the CA it carries would
	// be a store that advertises one identity and verifies another.
	if peer.GetCaPem() != "" && peer.GetCaFingerprint() != info.CAFingerprint {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"PinTrustedPeer: ca_fingerprint %q does not match the digest of the CA certificate supplied (%s); a pin must record what it actually pins",
			peer.GetCaFingerprint(), info.CAFingerprint)}
	}
	// The stored entry carries the CA anchor as it was evaluated, not
	// as it arrived, so that every replica holds the same bytes for
	// the same trust anchor. The fingerprint and expiry below are
	// re-derived for the same reason.
	stored := proto.Clone(peer).(*internalpb.TrustedPeer)
	stored.CaPem = info.CAAnchor
	stored.CaFingerprint = info.CAFingerprint
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

	f.trustedPeers[peer.GetNodeId()] = stored
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

// applyTransitionTrustedPeer stages a new certificate for a Comb that
// is already pinned.
//
// ADR-0170's decision, and the reason this is a separate command
// rather than an extension of applyPinTrustedPeer: the store's
// create-only rule is narrowed in exactly one place here.
//
// The narrowing is safe because of WHAT this command does not do. It
// does not overwrite the current certificate. It ADDS a staged one,
// and the current one keeps being trusted for the whole overlap. So
// the thing the create-only rule protects against - a certificate
// somebody compared by hand being silently replaced - cannot happen
// here: the old certificate remains in the store, in full, until an
// explicit activation removes it.
//
// Every refusal names itself, because each is a different operator
// problem and they are fixed in different places.
//
// The fingerprint and the expiry are RE-DERIVED here, not trusted,
// for exactly the reason applyPinTrustedPeer re-derives them: the
// writer is one managerd, and every other voter applies the same log
// entry without having seen the PEM arrive over a socket.
//
// And the evaluation is as of overlap_expires_at_unix rather than the
// wall clock, for the same determinism reason applyPinTrustedPeer
// evaluates as of pinned_at_unix: this function runs on every voter
// and again on every replay, so consulting time.Now() here would let
// a replay years later reject an entry that originally succeeded and
// leave replicas disagreeing about the store's contents.
func (f *FSM) applyTransitionTrustedPeer(index uint64, cmd *internalpb.TransitionTrustedPeer) *FSMApplyResult {
	nodeID := cmd.GetNodeId()
	if nodeID == "" {
		return &FSMApplyResult{Index: index, Error: "TransitionTrustedPeer: node_id must be set"}
	}
	next := cmd.GetNext()
	if next == nil {
		return &FSMApplyResult{Index: index, Error: "TransitionTrustedPeer: the incoming certificate must be set"}
	}
	if cmd.GetOverlapExpiresAtUnix() <= 0 {
		return &FSMApplyResult{Index: index, Error: "TransitionTrustedPeer: overlap_expires_at_unix must be set; an unbounded staged certificate is a permanent second trust anchor"}
	}
	existing, exists := f.trustedPeers[nodeID]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"TransitionTrustedPeer: node_id %q is not pinned; a transition replaces a certificate somebody compared, and creating one here would trust a certificate nobody compared",
			nodeID)}
	}
	if existing.GetNext() != nil {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"TransitionTrustedPeer: node_id %q already has a staged certificate; activate or abandon it before staging another, so the store's state for this node is never ambiguous",
			nodeID)}
	}
	// The incoming certificate is evaluated exactly as a pin would be:
	// the same gate, the same fail-closed rules, the same re-derivation.
	// A certificate that would be refused as a pin is refused here.
	info, err := hostcert.Evaluate([]byte(next.GetCertPem()), hostcert.EvaluateOptions{
		Now:                time.Unix(cmd.GetOverlapExpiresAtUnix(), 0).UTC(),
		RequireDNSSAN:      true,
		RequireLoopbackSAN: true,
	})
	if err != nil {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("TransitionTrustedPeer: %v", err)}
	}
	if next.GetFingerprint() != info.Fingerprint {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"TransitionTrustedPeer: fingerprint %q does not match the digest of the certificate supplied (%s); a staged certificate must record what it actually stages",
			next.GetFingerprint(), info.Fingerprint)}
	}
	if next.GetNotAfterUnix() != info.NotAfter.Unix() {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"TransitionTrustedPeer: not_after_unix %d does not match the certificate's own NotAfter %d",
			next.GetNotAfterUnix(), info.NotAfter.Unix())}
	}
	// A staged certificate must not claim to be for a different node.
	// The node_id is the store's key and the raft identity; a
	// certificate for someone else staged here would be trusted under
	// the wrong identity.
	if next.GetNodeId() != "" && next.GetNodeId() != nodeID {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"TransitionTrustedPeer: the staged certificate names node_id %q but is being staged for %q",
			next.GetNodeId(), nodeID)}
	}
	// Membership does not change when a certificate does. is_voter
	// describes whether the Comb is a MEMBER, and a rotation is not a
	// join.
	staged := proto.Clone(next).(*internalpb.TrustedPeer)
	staged.NodeId = nodeID
	// The STAGED certificate is never a voter. is_voter describes
	// whether the Comb is a MEMBER, and it belongs to the CURRENT
	// certificate's entry, not to a certificate that is not yet
	// serving. Carrying the membership flag on the staged entry
	// would make a second, not-yet-trusted certificate look like a
	// member's own.
	//
	// The membership is carried forward on ACTIVATION instead, where
	// the staged certificate becomes the current one and inherits
	// it there.
	staged.IsVoter = false
	if staged.GetPinnedBy() == "" {
		staged.PinnedBy = existing.GetPinnedBy()
	}
	if staged.GetPinnedAtUnix() == 0 {
		staged.PinnedAtUnix = existing.GetPinnedAtUnix()
	}

	updated := proto.Clone(existing).(*internalpb.TrustedPeer)
	updated.Next = staged
	f.trustedPeers[nodeID] = updated
	return &FSMApplyResult{Index: index, TrustedPeer: updated}
}

// applyActivateTrustedPeerTransition promotes a staged certificate to
// current and drops the one it replaces.
//
// The OLD certificate is dropped here rather than at staging, so the
// overlap covers the whole period in which the Comb might be serving
// either certificate. Dropping it earlier would leave a window in
// which the Comb serves the new certificate and its peers trust only
// the old one.
//
// The new certificate's fingerprint and expiry are re-derived once
// more, from the staged PEM, rather than carried forward from the
// staging command. That is not redundant: the staged entry is
// replicated state that a replica may have adopted from a snapshot
// rather than from the original log entry, and this is the last point
// at which the certificate that becomes the Comb's trust anchor is
// checked against its own bytes.
func (f *FSM) applyActivateTrustedPeerTransition(index uint64, cmd *internalpb.ActivateTrustedPeerTransition) *FSMApplyResult {
	nodeID := cmd.GetNodeId()
	if nodeID == "" {
		return &FSMApplyResult{Index: index, Error: "ActivateTrustedPeerTransition: node_id must be set"}
	}
	existing, exists := f.trustedPeers[nodeID]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("ActivateTrustedPeerTransition: node_id %q is not pinned", nodeID)}
	}
	staged := existing.GetNext()
	if staged == nil {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"ActivateTrustedPeerTransition: node_id %q has no staged certificate to activate", nodeID)}
	}
	info, err := hostcert.Evaluate([]byte(staged.GetCertPem()), hostcert.EvaluateOptions{
		Now:                time.Unix(staged.GetNotAfterUnix(), 0).UTC(),
		RequireDNSSAN:      true,
		RequireLoopbackSAN: true,
	})
	if err != nil {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("ActivateTrustedPeerTransition: %v", err)}
	}
	if staged.GetFingerprint() != info.Fingerprint {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"ActivateTrustedPeerTransition: the staged certificate's fingerprint (%s) does not match its own digest (%s)",
			staged.GetFingerprint(), info.Fingerprint)}
	}
	activated := proto.Clone(staged).(*internalpb.TrustedPeer)
	activated.NodeId = nodeID
	// Membership carries forward untouched: activating a certificate
	// is not a join, and a Comb that was a voter stays one.
	activated.IsVoter = existing.GetIsVoter()
	activated.Next = nil
	f.trustedPeers[nodeID] = activated
	return &FSMApplyResult{Index: index, TrustedPeer: activated}
}

// applyAbandonTrustedPeerTransition drops a staged certificate and
// keeps the current one.
//
// It exists for the failure case: a staged certificate that turns out
// to be wrong is dropped, and the Comb carries on with the certificate
// its peers already trust. Without this, the only way to clear a bad
// staged certificate would be to activate it.
func (f *FSM) applyAbandonTrustedPeerTransition(index uint64, cmd *internalpb.AbandonTrustedPeerTransition) *FSMApplyResult {
	nodeID := cmd.GetNodeId()
	if nodeID == "" {
		return &FSMApplyResult{Index: index, Error: "AbandonTrustedPeerTransition: node_id must be set"}
	}
	existing, exists := f.trustedPeers[nodeID]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("AbandonTrustedPeerTransition: node_id %q is not pinned", nodeID)}
	}
	if existing.GetNext() == nil {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"AbandonTrustedPeerTransition: node_id %q has no staged certificate to abandon", nodeID)}
	}
	updated := proto.Clone(existing).(*internalpb.TrustedPeer)
	updated.Next = nil
	f.trustedPeers[nodeID] = updated
	return &FSMApplyResult{Index: index, TrustedPeer: updated}
}
