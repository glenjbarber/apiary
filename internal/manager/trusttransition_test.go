package manager

// ADR-0170's trust transition, at the managerd boundary.
//
// The FSM arms decide what a transition MEANS. These tests cover the
// manager's own half: the values the caller does not get to state, the
// bounds the manager enforces before anything reaches the log, and the
// refusals that must name their cause.

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostcert"
)

// transitionTestCert generates a serving certificate the trust gate
// accepts.
func transitionTestCert(t *testing.T, name string) (certPEM, fingerprint string, notAfter int64) {
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
	return string(pem), info.Fingerprint, info.NotAfter.Unix()
}

// The caller does not get to state the fingerprint. The manager
// RE-DERIVES it from the certificate it carries, so a caller that
// supplies a wrong one has it corrected rather than stored.
//
// The correction is the point. Pinning and transitioning both re-derive
// the two fields that have to be facts - the fingerprint and the
// expiry - because the writer is one managerd and every other voter
// applies the same log entry without having seen the PEM arrive over
// a socket. A stated fingerprint that survived into the store would be
// a trust anchor for an identity nobody compared.
func TestTransitionPeerCertificate_RederivesTheFingerprintRatherThanTrustingIt(t *testing.T) {
	client, _ := newManagerdRPCClientAndServer(t, newRaftdUDSSocket(t), "comb-a")

	// Pin the node first: a transition replaces a certificate.
	currentPEM, currentFingerprint, currentNotAfter := transitionTestCert(t, "drone")
	pin, err := client.PinPeerCertificate(context.Background(), &rpcpb.PinPeerCertificateRequest{
		Peer: &rpcpb.TrustedPeer{
			NodeId:       "node02",
			CombName:     "drone",
			Fingerprint:  currentFingerprint,
			CertPem:      currentPEM,
			NotAfterUnix: currentNotAfter,
			// The pin evaluates the certificate as of pinned_at_unix
			// rather than the wall clock, so that every replica
			// decides the same thing. The caller supplies it.
			PinnedAtUnix: time.Now().UTC().Unix(),
		},
	})
	if err != nil {
		t.Fatalf("PinPeerCertificate: %v", err)
	}
	if pin.GetError() != "" {
		t.Fatalf("pinning: %s", pin.GetError())
	}

	incomingPEM, derivedFingerprint, incomingNotAfter := transitionTestCert(t, "drone-rotated")
	resp, err := client.TransitionPeerCertificate(context.Background(), &rpcpb.TransitionPeerCertificateRequest{
		NodeId: "node02",
		Next: &rpcpb.TrustedPeer{
			CertPem:      incomingPEM,
			NotAfterUnix: incomingNotAfter,
			// A lie about the fingerprint.
			Fingerprint: "SHA256:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00",
		},
		Reason: "rotation",
	})
	if err != nil {
		t.Fatalf("TransitionPeerCertificate: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("transition: %s", resp.GetError())
	}
	staged := resp.GetPeer().GetNext()
	if staged == nil {
		t.Fatal("no certificate was staged")
	}
	if staged.GetFingerprint() != derivedFingerprint {
		t.Errorf("the staged fingerprint is %q, want the DERIVED %q; the caller's stated value must not reach the store",
			staged.GetFingerprint(), derivedFingerprint)
	}
}

// A transition with no reason is refused, for the same reason an
// unpin with no reason is: an incident review asking "why did this
// Comb's identity change" can only be answered from the log.
func TestTransitionPeerCertificate_RequiresAReason(t *testing.T) {
	client, _ := newManagerdRPCClientAndServer(t, newRaftdUDSSocket(t), "comb-a")

	certPEM, fingerprint, notAfter := transitionTestCert(t, "drone")
	resp, err := client.TransitionPeerCertificate(context.Background(), &rpcpb.TransitionPeerCertificateRequest{
		NodeId: "node02",
		Next: &rpcpb.TrustedPeer{
			CertPem:      certPEM,
			Fingerprint:  fingerprint,
			NotAfterUnix: notAfter,
		},
	})
	if err != nil {
		t.Fatalf("TransitionPeerCertificate: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("a transition with no reason was accepted")
	}
	if !strings.Contains(resp.GetError(), "reason is required") {
		t.Errorf("the refusal %q does not say that the reason is what is missing", resp.GetError())
	}
}

// The overlap is bounded at the manager too, not only in the FSM. A
// caller asking for an unbounded overlap gets a refusal naming the
// limit, because an unbounded staged certificate is a permanent
// second trust anchor.
func TestTransitionPeerCertificate_RefusesAnUnboundedOverlap(t *testing.T) {
	client, _ := newManagerdRPCClientAndServer(t, newRaftdUDSSocket(t), "comb-a")

	certPEM, fingerprint, notAfter := transitionTestCert(t, "drone")
	resp, err := client.TransitionPeerCertificate(context.Background(), &rpcpb.TransitionPeerCertificateRequest{
		NodeId: "node02",
		Next: &rpcpb.TrustedPeer{
			CertPem:      certPEM,
			Fingerprint:  fingerprint,
			NotAfterUnix: notAfter,
		},
		OverlapSeconds: int64(MaxTransitionOverlap/time.Second) + 1,
		Reason:         "rotation",
	})
	if err != nil {
		t.Fatalf("TransitionPeerCertificate: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("an over-long overlap was accepted")
	}
	if !strings.Contains(resp.GetError(), "maximum") {
		t.Errorf("the refusal %q does not name the maximum", resp.GetError())
	}
}

// A transition for a node that is not pinned is refused, and the
// refusal names the reason rather than reporting a generic failure.
func TestTransitionPeerCertificate_RefusesAnUnpinnedNode(t *testing.T) {
	client, _ := newManagerdRPCClientAndServer(t, newRaftdUDSSocket(t), "comb-a")

	certPEM, fingerprint, notAfter := transitionTestCert(t, "drone")
	resp, err := client.TransitionPeerCertificate(context.Background(), &rpcpb.TransitionPeerCertificateRequest{
		NodeId: "node02",
		Next: &rpcpb.TrustedPeer{
			CertPem:      certPEM,
			Fingerprint:  fingerprint,
			NotAfterUnix: notAfter,
		},
		Reason: "rotation",
	})
	if err != nil {
		t.Fatalf("TransitionPeerCertificate: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("a transition for an unpinned node was accepted")
	}
	if !strings.Contains(resp.GetError(), "not pinned") {
		t.Errorf("the refusal %q does not say that the node is not pinned", resp.GetError())
	}
}

// Activation and abandonment both require a reason, for the same
// reason the transition does.
func TestActivateAndAbandonRequireAReason(t *testing.T) {
	client, _ := newManagerdRPCClientAndServer(t, newRaftdUDSSocket(t), "comb-a")

	activate, err := client.ActivatePeerCertificateTransition(context.Background(), &rpcpb.ActivatePeerCertificateTransitionRequest{
		NodeId: "node02",
	})
	if err != nil {
		t.Fatalf("ActivatePeerCertificateTransition: %v", err)
	}
	if activate.GetError() == "" {
		t.Error("an activation with no reason was accepted")
	}

	abandon, err := client.AbandonPeerCertificateTransition(context.Background(), &rpcpb.AbandonPeerCertificateTransitionRequest{
		NodeId: "node02",
	})
	if err != nil {
		t.Fatalf("AbandonPeerCertificateTransition: %v", err)
	}
	if abandon.GetError() == "" {
		t.Error("an abandonment with no reason was accepted")
	}
}

// A pin that SUCCEEDS reports the peer it wrote.
//
// This is a regression test for a bug that predates ADR-0170: the
// raft server's apply-result payload chain had no TrustedPeer arm, so
// a successful pin answered with an EMPTY peer - no node_id, no
// fingerprint, no certificate. A caller checking the response for the
// pin it just made read it as nothing having been pinned.
//
// The pin is the trust store's only writer, so an empty answer to a
// successful pin is an operator being told their trust anchor was not
// recorded.
func TestPinPeerCertificate_ReportsThePeerItWrote(t *testing.T) {
	client, _ := newManagerdRPCClientAndServer(t, newRaftdUDSSocket(t), "comb-a")

	certPEM, fingerprint, notAfter := transitionTestCert(t, "drone")
	resp, err := client.PinPeerCertificate(context.Background(), &rpcpb.PinPeerCertificateRequest{
		Peer: &rpcpb.TrustedPeer{
			NodeId:       "node02",
			CombName:     "drone",
			Fingerprint:  fingerprint,
			CertPem:      certPEM,
			NotAfterUnix: notAfter,
			PinnedAtUnix: time.Now().UTC().Unix(),
		},
	})
	if err != nil {
		t.Fatalf("PinPeerCertificate: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("pinning: %s", resp.GetError())
	}
	peer := resp.GetPeer()
	if peer == nil {
		t.Fatal("the response carries no peer; a successful pin must report what it wrote")
	}
	if peer.GetNodeId() != "node02" {
		t.Errorf("the reported peer names node %q, want node02", peer.GetNodeId())
	}
	if peer.GetFingerprint() != fingerprint {
		t.Errorf("the reported peer has fingerprint %q, want %q", peer.GetFingerprint(), fingerprint)
	}
	if peer.GetCertPem() == "" {
		t.Error("the reported peer carries no certificate")
	}
}

// A pin must not PANIC.
//
// This is a regression test for a second bug that predates ADR-0170:
// the RPC cloned an rpcpb.TrustedPeer and asserted it as an
// internalpb.TrustedPeer, which panics on EVERY call because the two
// are different protobuf types. PinPeerCertificate is the trust
// store's only writer, so the peer trust store could not be written
// at all through the RPC.
func TestPinPeerCertificate_DoesNotPanic(t *testing.T) {
	client, _ := newManagerdRPCClientAndServer(t, newRaftdUDSSocket(t), "comb-a")

	certPEM, fingerprint, notAfter := transitionTestCert(t, "drone")
	// If the RPC panicked, the test binary would die here. A clean
	// response is the assertion.
	resp, err := client.PinPeerCertificate(context.Background(), &rpcpb.PinPeerCertificateRequest{
		Peer: &rpcpb.TrustedPeer{
			NodeId:       "node02",
			CombName:     "drone",
			Fingerprint:  fingerprint,
			CertPem:      certPEM,
			NotAfterUnix: notAfter,
			PinnedAtUnix: time.Now().UTC().Unix(),
		},
	})
	if err != nil {
		t.Fatalf("PinPeerCertificate: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("pinning: %s", resp.GetError())
	}
}
