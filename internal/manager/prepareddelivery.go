package manager

// ADR-0171's prepared-certificate DELIVERY.
//
// The delivery rides the join flow's own GetJoinRequestStatus poll,
// because that is where the joining Comb's certificate is already
// presented and where the Colony already releases a sealed credential
// bundle. A separate channel would have meant a second place for the
// joiner's identity to be checked, and a second thing to get subtly
// wrong.
//
// What is delivered is an ENVELOPE, sealed to the joining Comb's own
// serving certificate, carrying the prepared certificate and the
// bootstrap private key that goes with it. The plaintext never leaves
// this Comb, and neither the certificate nor the key is ever in
// replicated state.
//
// The checks below are the ones that make the delivery safe, and each
// is refused by name:
//
//   - The request must be APPROVED. Before approval the join request
//     is a proposal, and the Colony has not decided this Comb is a
//     member - so it has not decided this Comb's certificate is a
//     trust anchor either.
//
//   - The reservation must exist, must be bound, and must be bound to
//     THIS request and THIS Comb.
//
//   - The joining Comb's certificate must be the one recorded for the
//     request, and the connection must be TLS.
//
//   - The delivery is single-use: a reservation that has been
//     delivered cannot be delivered again.
//
// The bootstrap key is a secret with TWO holders until the joiner
// replaces it with a key of its own. That cost is inherent to the
// prepared mode, and it is why the delivery is bounded, single-use,
// and bound to one named Comb.

import (
	"context"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/certreservation"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/origincert"
	"github.com/glenjbarber/apiary/internal/raft"
)

// sealPreparedCertificateForJoiner seals a prepared certificate and
// its bootstrap key to the joining Comb.
//
// It returns the envelope and the reservation it came from. The
// envelope is empty when there is no prepared certificate for this
// request, which is NOT an error: most joins have no preparation, and
// a joiner that was not given a prepared certificate is a perfectly
// ordinary join.
//
// Every refusal returns an error rather than an empty envelope, so a
// caller cannot mistake "nothing prepared" for "the delivery failed".
func (s *Server) sealPreparedCertificateForJoiner(ctx context.Context, req *rpcpb.GetJoinRequestStatusRequest, pending *internalpb.PendingJoinRequest) ([]byte, string, string, error) {
	reservationID := strings.TrimSpace(req.GetPreparedCertificateReservationId())
	if reservationID == "" {
		// Nothing was presented, so there is nothing to deliver. This
		// is the ordinary case and it is not an error.
		return nil, "", "", nil
	}
	if pending.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED {
		return nil, "", "", fmt.Errorf("a prepared certificate is delivered only to an APPROVED join; this request is %s", pending.GetStatus().String())
	}
	if !statusPeerUsesTLS(ctx) {
		return nil, "", "", fmt.Errorf("prepared certificate delivery requires a TLS connection; rerun apiaryctl join-introduce without --plaintext")
	}

	// The joining Comb's certificate must be the one recorded for this
	// request. This is the same check the credential delivery makes,
	// and it is the check that ties the delivery to the identity the
	// Colony already verified.
	certBlock, _ := pem.Decode([]byte(req.GetRecipientTlsCertificatePem()))
	if certBlock == nil {
		return nil, "", "", fmt.Errorf("the recipient's certificate is missing or malformed")
	}
	fingerprint := hostcert.Fingerprint(certBlock.Bytes)
	wantFingerprint := pending.GetTlsCertFingerprint()
	if wantFingerprint == "" && len(pending.GetAdvertisedFingerprints()) > 0 {
		wantFingerprint = pending.GetAdvertisedFingerprints()[0]
	}
	if wantFingerprint == "" || fingerprint != wantFingerprint {
		return nil, "", "", fmt.Errorf("recipient certificate fingerprint does not match the certificate recorded for this join request")
	}

	// The reservation must exist and must be bound to THIS request and
	// THIS Comb.
	reservations, err := s.raft.ListCertificateReservationsLocal(ctx)
	if err != nil {
		return nil, "", "", fmt.Errorf("reading the prepared certificate reservations: %w", err)
	}
	var reservation *internalpb.CertificateReservation
	for _, res := range reservations.GetReservations() {
		if res.GetReservationId() == reservationID {
			reservation = res
			break
		}
	}
	if reservation == nil {
		return nil, "", "", fmt.Errorf("no prepared certificate reservation %q exists on this Colony", reservationID)
	}
	if reservation.GetNodeId() == "" {
		return nil, "", "", fmt.Errorf("the prepared certificate reservation %q is not bound to a joining Comb yet", reservationID)
	}
	if reservation.GetNodeId() != pending.GetNodeId() {
		return nil, "", "", fmt.Errorf("the prepared certificate reservation %q is bound to node %q, not to %q", reservationID, reservation.GetNodeId(), pending.GetNodeId())
	}
	if reservation.GetRequestId() != pending.GetRequestId() {
		return nil, "", "", fmt.Errorf("the prepared certificate reservation %q is bound to join request %q, not to %q", reservationID, reservation.GetRequestId(), pending.GetRequestId())
	}
	if reservation.GetDeliveredAtUnix() != 0 {
		return nil, "", "", fmt.Errorf("the prepared certificate reservation %q has already been delivered; the prepared pair is single-use", reservationID)
	}
	if raft.CertificateReservationExpired(reservation, time.Now().UTC().Unix()) {
		return nil, "", "", fmt.Errorf("the prepared certificate reservation %q expired at %d", reservationID, reservation.GetExpiresAtUnix())
	}

	// The prepared pair is read from this Comb's own protected store.
	// It is never in replicated state, and it is never in the request.
	certPEM, keyPEM, err := s.readPreparedPair(reservation)
	if err != nil {
		return nil, "", "", err
	}

	// The envelope's associated data binds it to the reservation, the
	// request, the node and the fingerprint. A ciphertext copied from
	// one reservation's delivery therefore fails to decrypt under
	// another's, rather than decrypting into the wrong joiner's key.
	envelope, err := certreservation.SealDelivery(
		&certreservation.Reservation{
			ID:             reservation.GetReservationId(),
			FQDN:           reservation.GetFqdn(),
			Fingerprint:    reservation.GetFingerprint(),
			CertificatePEM: certPEM,
			NodeID:         reservation.GetNodeId(),
			RequestID:      reservation.GetRequestId(),
		},
		pending.GetRequestId(),
		pending.GetNodeId(),
		req.GetRecipientTlsCertificatePem(),
		keyPEM,
		time.Now().UTC(),
	)
	if err != nil {
		return nil, "", "", fmt.Errorf("sealing the prepared certificate: %w", err)
	}
	return envelope, reservationID, reservation.GetFingerprint(), nil
}

// readPreparedPair reads the prepared certificate and its bootstrap key
// from this Comb's own certificate store.
//
// The pair is looked up by the reservation's FQDN, which is the name
// the inventory entry was written under. A missing pair is a refusal
// naming what is missing, because "the preparation is recorded but
// the certificate is gone" is a different operator problem from
// "there is no preparation".
func (s *Server) readPreparedPair(reservation *internalpb.CertificateReservation) (certPEM, keyPEM string, err error) {
	if s.nodeConfig == nil {
		return "", "", fmt.Errorf("this node has no node-config store configured, so a prepared certificate cannot be read from it")
	}
	cfg, err := s.nodeConfig.Load()
	if err != nil {
		return "", "", fmt.Errorf("reading this Comb's node configuration: %w", err)
	}
	if cfg.AcmeDirectory == "" {
		return "", "", fmt.Errorf("this Comb has no local certificate directory configured, so a prepared certificate cannot be read from it")
	}
	entries, err := origincert.LoadInventory(cfg.AcmeDirectory)
	if err != nil {
		return "", "", fmt.Errorf("reading the local certificate inventory: %w", err)
	}
	for _, entry := range entries {
		if entry.Name != reservation.GetFqdn() {
			continue
		}
		certPEM, err := os.ReadFile(entry.CertPath)
		if err != nil {
			return "", "", fmt.Errorf("reading the prepared certificate %q: %w", entry.Name, err)
		}
		keyPEM, err := os.ReadFile(entry.KeyPath)
		if err != nil {
			return "", "", fmt.Errorf("reading the prepared private key %q: %w", entry.Name, err)
		}
		return string(certPEM), string(keyPEM), nil
	}
	return "", "", fmt.Errorf("the prepared certificate %q is recorded in the reservations but is not in this Comb's local certificate inventory", reservation.GetFqdn())
}

// markPreparedCertificateDelivered marks a reservation as delivered, so
// a second collection is refused.
//
// It is applied AFTER the envelope has been produced, because the
// envelope is what the joiner needs and a failed mark must not destroy
// it. The window this leaves - an envelope produced but the mark not
// written - is a window in which the same reservation could be
// delivered twice, which is why the mark is written immediately and
// why the reservation's own delivery window is short.
func (s *Server) markPreparedCertificateDelivered(ctx context.Context, reservationID string) error {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_DeliverCertificateReservation{
			DeliverCertificateReservation: &internalpb.DeliverCertificateReservation{
				ReservationId:   reservationID,
				DeliveredAtUnix: time.Now().UTC().Unix(),
			},
		},
	}
	_, appErr, leaderHint := s.applyReservationCommand(ctx, cmd)
	if appErr != "" {
		return fmt.Errorf("recording the prepared certificate delivery: %s", appErr)
	}
	if leaderHint != "" {
		return fmt.Errorf("recording the prepared certificate delivery reached a follower (leader %s)", leaderHint)
	}
	return nil
}
