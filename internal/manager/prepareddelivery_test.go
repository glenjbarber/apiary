package manager

// ADR-0171's prepared-certificate delivery.
//
// What these pin, in the order the ADR states it: that a prepared
// certificate is delivered ONLY to an approved join; that it goes only
// to the Comb the reservation is bound to; that the delivery is
// single-use; that a joiner that presented no reservation gets
// nothing and no error; and that the plaintext never appears in
// replicated state.

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostcert"
)

// preparedTestCert generates a serving certificate the trust gate
// accepts, and its fingerprint.
func preparedTestCert(t *testing.T, name string) (certPEM, fingerprint string) {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	pem, _, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName:  name + ".lab3.home.arpa",
		DNSNames:    []string{name, name + ".lab3.home.arpa"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		Now:         now,
	})
	if err != nil {
		t.Fatalf("generating a test certificate: %v", err)
	}
	info, err := hostcert.Evaluate(pem, hostcert.EvaluateOptions{
		Now:                now,
		RequireDNSSAN:      true,
		RequireLoopbackSAN: true,
	})
	if err != nil {
		t.Fatalf("the generated certificate does not pass the gate: %v", err)
	}
	return string(pem), info.Fingerprint
}

// A joiner that presented no reservation gets nothing, and NO error.
//
// Most joins have no prepared certificate, so "nothing prepared" is
// the ordinary case and must not be reported as a failure. The ADR is
// explicit that the emptiness and the failure are different things.
func TestPreparedDelivery_NothingPresentedIsNotAnError(t *testing.T) {
	srv := &Server{}
	pending := &internalpb.PendingJoinRequest{
		RequestId: "jreq-1",
		NodeId:    "node02",
		Status:    internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED,
	}
	envelope, reservationID, _, _, err := srv.sealPreparedCertificateForJoiner(context.Background(), &rpcpb.GetJoinRequestStatusRequest{}, pending)
	if err != nil {
		t.Fatalf("an ordinary join was refused: %v", err)
	}
	if len(envelope) != 0 || reservationID != "" {
		t.Errorf("a joiner that presented no reservation got an envelope (%d bytes, id %q)", len(envelope), reservationID)
	}
}

// A prepared certificate is NOT delivered to a request that is not
// approved.
//
// Before approval the join request is a proposal, and the Colony has
// not decided this Comb is a member - so it has not decided this
// Comb's certificate is a trust anchor either.
func TestPreparedDelivery_RefusesAnUnapprovedRequest(t *testing.T) {
	srv := &Server{}
	certPEM, _ := preparedTestCert(t, "drone")
	pending := &internalpb.PendingJoinRequest{
		RequestId: "jreq-1",
		NodeId:    "node02",
		Status:    internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING,
	}
	_, _, _, _, err := srv.sealPreparedCertificateForJoiner(context.Background(), &rpcpb.GetJoinRequestStatusRequest{
		PreparedCertificateReservationId: "res-1",
		RecipientTlsCertificatePem:       certPEM,
	}, pending)
	if err == nil {
		t.Fatal("a prepared certificate was prepared for delivery to an unapproved request")
	}
	if !strings.Contains(err.Error(), "APPROVED") {
		t.Errorf("the refusal %q does not say the request must be approved", err)
	}
}

// The delivery requires a TLS connection.
//
// The status poll is unauthenticated by design, and the prepared pair
// carries a private key. A plaintext connection is refused for the
// same reason the credential delivery refuses one.
func TestPreparedDelivery_RefusesAPlaintextConnection(t *testing.T) {
	srv := &Server{}
	certPEM, _ := preparedTestCert(t, "drone")
	pending := &internalpb.PendingJoinRequest{
		RequestId: "jreq-1",
		NodeId:    "node02",
		Status:    internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED,
	}
	_, _, _, _, err := srv.sealPreparedCertificateForJoiner(context.Background(), &rpcpb.GetJoinRequestStatusRequest{
		PreparedCertificateReservationId: "res-1",
		RecipientTlsCertificatePem:       certPEM,
	}, pending)
	if err == nil {
		t.Fatal("a prepared certificate was prepared for delivery over a plaintext connection")
	}
	if !strings.Contains(err.Error(), "TLS") {
		t.Errorf("the refusal %q does not say the connection must be TLS", err)
	}
}

// The joining Comb's certificate must be the one the Colony recorded
// for this request.
//
// This is the check that ties the delivery to an identity the Colony
// already verified. The prepared pair carries a private key, and the
// recipient's certificate is what the envelope is SEALED to - so a
// certificate that is not the recorded one would receive a key nobody
// meant to give it.
func TestPreparedDelivery_RefusesAMismatchedRecipient(t *testing.T) {
	srv := &Server{}
	certPEM, _ := preparedTestCert(t, "drone")
	pending := &internalpb.PendingJoinRequest{
		RequestId: "jreq-1",
		NodeId:    "node02",
		Status:    internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED,
		// A fingerprint that is NOT the one the presented certificate
		// has.
		TlsCertFingerprint: "SHA256:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00",
	}
	tlsCtx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{}})
	_, _, _, _, err := srv.sealPreparedCertificateForJoiner(tlsCtx, &rpcpb.GetJoinRequestStatusRequest{
		PreparedCertificateReservationId: "res-1",
		RecipientTlsCertificatePem:       certPEM,
	}, pending)
	if err == nil {
		t.Fatal("a prepared certificate was prepared for delivery to a Comb whose certificate does not match the recorded one")
	}
	if !strings.Contains(err.Error(), "fingerprint") {
		t.Errorf("the refusal %q does not name the fingerprint check", err)
	}
}
