package manager

// ADR-0171's prepared-certificate reservations: the managerd side.
//
// The FSM arms in internal/raft/reservations.go decide what each
// command means. This file is the part that decides what the CALLER
// is allowed to say, and what the manager does about the values that
// have to be facts rather than claims.
//
// The shape follows the trust store's RPCs, and deliberately so: a
// reservation is a standing record about which certificate this
// Colony is holding for a Comb that has not joined, so it goes through
// the same leader-forwarding, the same fail-closed role map, and the
// same "the caller does not get to state a fingerprint" rule.
//
// What does NOT happen here is the delivery. The delivery rides the
// join flow's own GetJoinRequestStatus poll, which is where the
// joining Comb's certificate is already presented and where the
// Colony already releases a sealed credential bundle. Putting the
// prepared certificate on a separate RPC would have meant a second
// channel, a second place for the joiner's identity to be checked,
// and a second thing to get subtly wrong.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostcert"
)

// DefaultReservationValidity is how long a preparation stays
// deliverable when the caller does not say.
//
// It is bounded on purpose. A prepared pair sits on the preparing Comb
// holding a private key for a machine that has not arrived yet, and an
// unbounded preparation is an unbounded wait.
const DefaultReservationValidity = time.Hour

// MaxReservationValidity is the longest preparation this accepts.
//
// The bound is the point: an unbounded preparation holds a prepared key
// indefinitely, and a caller that wants a longer one gets a refusal
// naming this limit rather than a quiet acceptance.
const MaxReservationValidity = 24 * time.Hour

// PrepareCertificateReservation implements rpcpb.ManagerServiceServer -
// Admin-tier.
//
// It records that a certificate has been prepared for a Comb that has
// not joined yet.
//
// The prepared certificate's PRIVATE KEY is not accepted here, and
// never is. It stays in the preparing Comb's protected store and is
// sealed to the joining Comb at delivery time, so no private key
// travels through the RPC surface, through replicated state, or
// through a log line.
func (s *Server) PrepareCertificateReservation(ctx context.Context, req *rpcpb.PrepareCertificateReservationRequest) (*rpcpb.PrepareCertificateReservationResponse, error) {
	fqdn := strings.TrimSpace(req.GetFqdn())
	if fqdn == "" {
		return &rpcpb.PrepareCertificateReservationResponse{Error: "fqdn is required; it is the name the certificate was issued for"}, nil
	}
	if strings.TrimSpace(req.GetCertPem()) == "" {
		return &rpcpb.PrepareCertificateReservationResponse{Error: "cert_pem is required; the prepared certificate is needed so the fingerprint can be re-derived from its bytes"}, nil
	}
	reservationID := strings.TrimSpace(req.GetReservationId())
	if reservationID == "" {
		return &rpcpb.PrepareCertificateReservationResponse{Error: "reservation_id is required; it is what the joining Comb presents to collect the prepared material, and it must be unguessable"}, nil
	}
	if status, statusErr := s.raft.Status(ctx); statusErr == nil && !status.GetIsLeader() {
		if hint := currentLeaderRaftAddress(status); hint != "" && s.peers != nil {
			if fwd, ferr := s.peers.PrepareCertificateReservation(ctx, s.peerManagerdAddr(hint), req); ferr == nil {
				return fwd, nil
			}
		}
	}

	validity := time.Duration(req.GetValiditySeconds()) * time.Second
	if validity <= 0 {
		validity = DefaultReservationValidity
	}
	if validity > MaxReservationValidity {
		return &rpcpb.PrepareCertificateReservationResponse{Error: fmt.Sprintf(
			"the validity of %s is longer than the %s maximum; an unbounded preparation holds a prepared key indefinitely",
			validity, MaxReservationValidity)}, nil
	}

	// The fingerprint is RE-DERIVED from the certificate's bytes, not
	// taken from the caller. The caller may state it, and the stated
	// value is checked against the real one - but the stored value is
	// always the derived one, because the stored value is what the
	// delivery's associated data is bound to.
	info, err := hostcert.Evaluate([]byte(req.GetCertPem()), hostcert.EvaluateOptions{
		Now:                time.Now().UTC(),
		RequireDNSSAN:      true,
		RequireLoopbackSAN: true,
	})
	if err != nil {
		return &rpcpb.PrepareCertificateReservationResponse{Error: fmt.Sprintf("refusing to prepare: %v", err)}, nil
	}
	stated := strings.TrimSpace(req.GetFingerprint())
	if stated != "" && stated != info.Fingerprint {
		return &rpcpb.PrepareCertificateReservationResponse{Error: fmt.Sprintf(
			"refusing to prepare: the stated fingerprint %q does not match the certificate's own digest (%s)",
			stated, info.Fingerprint)}, nil
	}

	now := time.Now().UTC()
	reservation := &internalpb.CertificateReservation{
		ReservationId: reservationID,
		Fqdn:          fqdn,
		Fingerprint:   info.Fingerprint,
		CreatedAtUnix: now.Unix(),
		ExpiresAtUnix: now.Add(validity).Unix(),
		PreparedBy:    s.localNodeID(),
	}
	cmd := &internalpb.Command{
		Op: &internalpb.Command_CreateCertificateReservation{
			CreateCertificateReservation: &internalpb.CreateCertificateReservation{Reservation: reservation},
		},
	}
	result, appErr, leaderHint := s.applyReservationCommand(ctx, cmd)
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.PrepareCertificateReservation(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.PrepareCertificateReservationResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.PrepareCertificateReservationResponse{Reservation: toRPCCertificateReservation(result)}, nil
}

// ListCertificateReservations implements rpcpb.ManagerServiceServer -
// Viewer-tier.
//
// It is a READ: it lists the preparations this Colony is holding. The
// list carries no key material, so a Viewer can see which
// preparations exist without being able to collect any of them.
func (s *Server) ListCertificateReservations(ctx context.Context, _ *rpcpb.ListCertificateReservationsRequest) (*rpcpb.ListCertificateReservationsResponse, error) {
	resp, err := s.raft.ListCertificateReservationsLocal(ctx)
	if err != nil {
		return &rpcpb.ListCertificateReservationsResponse{Error: err.Error()}, nil
	}
	out := &rpcpb.ListCertificateReservationsResponse{}
	for _, res := range resp.GetReservations() {
		out.Reservations = append(out.Reservations, toRPCCertificateReservation(res))
	}
	return out, nil
}

// DropCertificateReservation implements rpcpb.ManagerServiceServer -
// Admin-tier.
//
// It removes a preparation outright. It is how a withdrawn or expired
// preparation is cleared, and how an operator cancels one that was
// prepared for a Comb that will never arrive.
//
// The reason is required and is replicated with the removal, for the
// same reason UnpinPeerCertificate requires one: an incident review
// asking "why was this preparation dropped" can only be answered from
// the log.
func (s *Server) DropCertificateReservation(ctx context.Context, req *rpcpb.DropCertificateReservationRequest) (*rpcpb.DropCertificateReservationResponse, error) {
	if strings.TrimSpace(req.GetReservationId()) == "" {
		return &rpcpb.DropCertificateReservationResponse{Error: "reservation_id is required"}, nil
	}
	if strings.TrimSpace(req.GetReason()) == "" {
		return &rpcpb.DropCertificateReservationResponse{Error: "a reason is required. Dropping a preparation discards a certificate that was issued for a specific Comb, and an empty reason is an answer of \"someone, for no recorded reason\""}, nil
	}
	if status, statusErr := s.raft.Status(ctx); statusErr == nil && !status.GetIsLeader() {
		if hint := currentLeaderRaftAddress(status); hint != "" && s.peers != nil {
			if fwd, ferr := s.peers.DropCertificateReservation(ctx, s.peerManagerdAddr(hint), req); ferr == nil {
				return fwd, nil
			}
		}
	}
	cmd := &internalpb.Command{
		Op: &internalpb.Command_DropCertificateReservation{
			DropCertificateReservation: &internalpb.DropCertificateReservation{
				ReservationId: strings.TrimSpace(req.GetReservationId()),
				Reason:        strings.TrimSpace(req.GetReason()),
			},
		},
	}
	_, appErr, leaderHint := s.applyReservationCommand(ctx, cmd)
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.DropCertificateReservation(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.DropCertificateReservationResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.DropCertificateReservationResponse{}, nil
}

// applyReservationCommand submits one reservation command and returns
// the reservation the FSM recorded.
//
// The result is unmarshalled from the apply response, so the caller's
// answer is the FSM's own record rather than what the caller hoped
// for.
func (s *Server) applyReservationCommand(ctx context.Context, cmd *internalpb.Command) (reservation *internalpb.CertificateReservation, appErr, leaderHint string) {
	payload, err := proto.Marshal(cmd)
	if err != nil {
		return nil, err.Error(), ""
	}
	resp, err := s.raft.Apply(ctx, payload, defaultApplyTimeout)
	if err != nil {
		return nil, err.Error(), ""
	}
	if resp.GetError() != "" {
		return nil, resp.GetError(), resp.GetLeaderHint()
	}
	reservation = &internalpb.CertificateReservation{}
	if err := proto.Unmarshal(resp.GetResult(), reservation); err != nil {
		return nil, err.Error(), ""
	}
	return reservation, "", ""
}
