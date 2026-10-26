package manager

// ADR-0170's authorized trust transition: the managerd side.
//
// The three RPCs here stage, activate and abandon a certificate
// transition for an already-pinned Comb. The FSM arms in
// internal/raft/peers.go decide what each command means; this file is
// the part that decides what the CALLER is allowed to say, and what
// the manager does about the values that have to be facts rather than
// claims.
//
// The shape of the manager side follows PinPeerCertificate above, and
// deliberately so: a transition is a pin-shaped act - it decides which
// certificate this Colony will accept from a Comb - so it goes
// through the same leader-forwarding, the same fail-closed role map,
// and the same "the caller does not get to state a fingerprint" rule.
//
// Two things are different from a pin, and both are the point of
// ADR-0170:
//
//   - A transition RE-DERIVES the incoming certificate's fingerprint
//     and expiry before submitting it, so a refusal names the real
//     cause rather than "the FSM said no".
//
//   - The overlap is expressed as a DURATION, not as an absolute
//     deadline, and it is bounded here. The caller asks for "this long
//     with both certificates trusted"; the manager turns that into the
//     absolute instant the FSM wants, because the FSM must not consult
//     a wall clock.

import (
	"context"
	"fmt"
	"strings"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostcert"
)

// DefaultTransitionOverlap is how long both certificates are trusted
// when the caller does not say.
//
// It is long enough to cover a Comb being restarted with a new
// certificate and its peers reconnecting, and short enough that a
// transition that is never activated does not leave a second trust
// anchor standing indefinitely.
const DefaultTransitionOverlap = 15 * time.Minute

// MaxTransitionOverlap is the longest overlap this accepts.
//
// The bound is the point. An unbounded staged certificate is a
// permanent second trust anchor, and a permanent second trust anchor
// is a certificate nobody knows why is still trusted. A caller that
// wants a longer overlap gets a refusal naming this limit rather than
// a quiet acceptance.
const MaxTransitionOverlap = 24 * time.Hour

// TransitionPeerCertificate implements rpcpb.ManagerServiceServer -
// Admin-tier.
//
// It STAGES an incoming certificate for an already-pinned Comb. Both
// certificates are trusted from this point until the transition is
// activated or abandoned, so the Comb may serve either one while the
// transition is in flight.
func (s *Server) TransitionPeerCertificate(ctx context.Context, req *rpcpb.TransitionPeerCertificateRequest) (*rpcpb.TransitionPeerCertificateResponse, error) {
	nodeID := strings.TrimSpace(req.GetNodeId())
	if nodeID == "" {
		return &rpcpb.TransitionPeerCertificateResponse{Error: "node_id is required"}, nil
	}
	if req.GetNext() == nil {
		return &rpcpb.TransitionPeerCertificateResponse{Error: "the incoming certificate is required"}, nil
	}
	if strings.TrimSpace(req.GetReason()) == "" {
		return &rpcpb.TransitionPeerCertificateResponse{Error: "a reason is required. Replacing a Comb's certificate is an act an incident review asks about, and an empty reason is an answer of \"someone, for no recorded reason\""}, nil
	}
	if status, statusErr := s.raft.Status(ctx); statusErr == nil && !status.GetIsLeader() {
		if hint := currentLeaderRaftAddress(status); hint != "" && s.peers != nil {
			if fwd, ferr := s.peers.TransitionPeerCertificate(ctx, s.peerManagerdAddr(hint), req); ferr == nil {
				return fwd, nil
			}
		}
	}

	overlap := time.Duration(req.GetOverlapSeconds()) * time.Second
	if overlap <= 0 {
		overlap = DefaultTransitionOverlap
	}
	if overlap > MaxTransitionOverlap {
		return &rpcpb.TransitionPeerCertificateResponse{Error: fmt.Sprintf(
			"the overlap of %s is longer than the %s maximum; an unbounded staged certificate is a permanent second trust anchor",
			overlap, MaxTransitionOverlap)}, nil
	}

	// The caller's own values are not trusted for the two fields that
	// have to be facts: the fingerprint is re-derived from the
	// certificate it carries, and the expiry is re-read from it. The
	// FSM refuses a mismatch too, but doing it here means the refusal
	// names the real cause.
	next := toInternalTrustedPeer(req.GetNext())
	// The staged certificate is never a voter: is_voter describes
	// whether the Comb is a MEMBER, and the staged certificate is not
	// yet the one being served.
	next.NodeId = nodeID
	next.IsVoter = false
	info, err := hostcert.Evaluate([]byte(next.GetCertPem()), hostcert.EvaluateOptions{
		Now:                time.Now().UTC(),
		RequireDNSSAN:      true,
		RequireLoopbackSAN: true,
	})
	if err != nil {
		return &rpcpb.TransitionPeerCertificateResponse{Error: fmt.Sprintf("refusing to transition: %v", err)}, nil
	}
	next.Fingerprint = info.Fingerprint
	next.NotAfterUnix = info.NotAfter.Unix()
	if next.GetPinnedBy() == "" {
		next.PinnedBy = s.localNodeID()
	}
	if next.GetPinnedAtUnix() == 0 {
		next.PinnedAtUnix = time.Now().UTC().Unix()
	}

	cmd := &internalpb.Command{
		Op: &internalpb.Command_TransitionTrustedPeer{TransitionTrustedPeer: &internalpb.TransitionTrustedPeer{
			NodeId:               nodeID,
			Next:                 next,
			OverlapExpiresAtUnix: time.Now().UTC().Add(overlap).Unix(),
			Reason:               strings.TrimSpace(req.GetReason()),
		}},
	}
	result, appErr, leaderHint := s.applyTrustedPeerCommand(ctx, cmd, 0)
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.TransitionPeerCertificate(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.TransitionPeerCertificateResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.TransitionPeerCertificateResponse{Peer: toRPCTustedPeer(result)}, nil
}

// ActivatePeerCertificateTransition implements
// rpcpb.ManagerServiceServer - Admin-tier.
//
// It promotes the staged certificate to current and drops the one it
// replaced. The old certificate is dropped HERE rather than at
// staging, so the overlap covers the whole period in which the Comb
// might be serving either certificate.
func (s *Server) ActivatePeerCertificateTransition(ctx context.Context, req *rpcpb.ActivatePeerCertificateTransitionRequest) (*rpcpb.ActivatePeerCertificateTransitionResponse, error) {
	nodeID := strings.TrimSpace(req.GetNodeId())
	if nodeID == "" {
		return &rpcpb.ActivatePeerCertificateTransitionResponse{Error: "node_id is required"}, nil
	}
	if strings.TrimSpace(req.GetReason()) == "" {
		return &rpcpb.ActivatePeerCertificateTransitionResponse{Error: "a reason is required"}, nil
	}
	if status, statusErr := s.raft.Status(ctx); statusErr == nil && !status.GetIsLeader() {
		if hint := currentLeaderRaftAddress(status); hint != "" && s.peers != nil {
			if fwd, ferr := s.peers.ActivatePeerCertificateTransition(ctx, s.peerManagerdAddr(hint), req); ferr == nil {
				return fwd, nil
			}
		}
	}
	cmd := &internalpb.Command{
		Op: &internalpb.Command_ActivateTrustedPeerTransition{ActivateTrustedPeerTransition: &internalpb.ActivateTrustedPeerTransition{
			NodeId: nodeID,
			Reason: strings.TrimSpace(req.GetReason()),
		}},
	}
	result, appErr, leaderHint := s.applyTrustedPeerCommand(ctx, cmd, 0)
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.ActivatePeerCertificateTransition(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.ActivatePeerCertificateTransitionResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.ActivatePeerCertificateTransitionResponse{Peer: toRPCTustedPeer(result)}, nil
}

// AbandonPeerCertificateTransition implements
// rpcpb.ManagerServiceServer - Admin-tier.
//
// It drops a staged certificate and keeps the current one. It exists
// for the failure case: a staged certificate that turns out to be
// wrong is dropped, and the Comb carries on with the certificate its
// peers already trust.
func (s *Server) AbandonPeerCertificateTransition(ctx context.Context, req *rpcpb.AbandonPeerCertificateTransitionRequest) (*rpcpb.AbandonPeerCertificateTransitionResponse, error) {
	nodeID := strings.TrimSpace(req.GetNodeId())
	if nodeID == "" {
		return &rpcpb.AbandonPeerCertificateTransitionResponse{Error: "node_id is required"}, nil
	}
	if strings.TrimSpace(req.GetReason()) == "" {
		return &rpcpb.AbandonPeerCertificateTransitionResponse{Error: "a reason is required"}, nil
	}
	if status, statusErr := s.raft.Status(ctx); statusErr == nil && !status.GetIsLeader() {
		if hint := currentLeaderRaftAddress(status); hint != "" && s.peers != nil {
			if fwd, ferr := s.peers.AbandonPeerCertificateTransition(ctx, s.peerManagerdAddr(hint), req); ferr == nil {
				return fwd, nil
			}
		}
	}
	cmd := &internalpb.Command{
		Op: &internalpb.Command_AbandonTrustedPeerTransition{AbandonTrustedPeerTransition: &internalpb.AbandonTrustedPeerTransition{
			NodeId: nodeID,
			Reason: strings.TrimSpace(req.GetReason()),
		}},
	}
	result, appErr, leaderHint := s.applyTrustedPeerCommand(ctx, cmd, 0)
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.AbandonPeerCertificateTransition(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.AbandonPeerCertificateTransitionResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.AbandonPeerCertificateTransitionResponse{Peer: toRPCTustedPeer(result)}, nil
}
