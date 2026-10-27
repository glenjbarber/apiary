package raft

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/peerca"
)

// caIssuedPeerCert builds a certificate a certificate authority
// issued, in the shape a Colony's Comb carries once the
// Cloudflare/ACME work (ADR-0133) has moved it onto a publicly
// trusted certificate: a DNS SAN of the Comb's name, and NO loopback
// address, because a Let's Encrypt leaf carries none.
func caIssuedPeerCert(t *testing.T, combName string) (certPEM, fingerprint string, notAfter int64) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test Issuing CA"},
		NotBefore:             pinTestNow.Add(-time.Hour),
		NotAfter:              pinTestNow.Add(3650 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fqdn := combName + ".lab3.apiary.work"
	leafTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: fqdn},
		NotBefore:             pinTestNow.Add(-time.Hour),
		NotAfter:              pinTestNow.Add(90 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{fqdn},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEMBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})

	info, err := hostcert.Evaluate(certPEMBytes, hostcert.EvaluateOptions{
		Now:                pinTestNow,
		RequireDNSSAN:      true,
		RequireLoopbackSAN: true,
		AllowCAIssued:      true,
	})
	if err != nil {
		t.Fatalf("the CA-issued test certificate for %s does not pass the gate the pin applies: %v", combName, err)
	}
	return string(certPEMBytes), info.Fingerprint, info.NotAfter.Unix()
}

// TestPinTrustedPeerAcceptsACertificateIssuedByACAClosesTheTrustStore
// Gap is the whole reason this change exists.
//
// Before it, a Colony whose Combs carry publicly trusted certificates
// could never populate the peer trust store: the pin was refused with
// "not a Comb's own self-signed certificate", the store stayed empty,
// peer-ca.pem was never written, and peer TLS had no derived anchors
// at all. The operator saw the pin preflight fail with a message
// about a certificate it could not change.
func TestPinTrustedPeerAcceptsACertificateIssuedByACAClosesTheTrustStore(t *testing.T) {
	certPEM, fingerprint, notAfter := caIssuedPeerCert(t, "brood")
	peer := &internalpb.TrustedPeer{
		NodeId:       "brood.lab3.apiary.work",
		CombName:     "brood",
		Fingerprint:  fingerprint,
		CertPem:      certPEM,
		NotAfterUnix: notAfter,
		PinnedAtUnix: pinTestNow.Unix(),
		PinnedBy:     "brood",
		IsVoter:      false,
	}

	fsm := newTestFSM(t)
	res := applyPin(t, fsm, 1, pinCmd(peer))
	if res.Error != "" {
		t.Fatalf("a CA-issued certificate was refused: %s", res.Error)
	}

	// The store must actually hold it, and the derived file must be
	// writable from it.
	stored := fsm.trustedPeers[peer.GetNodeId()]
	if stored == nil {
		t.Fatal("the pin was not recorded in the trust store")
	}
	body, err := peerca.Render(storeAsSlice(fsm))
	if err != nil {
		t.Fatalf("rendering the derived peer-ca.pem failed: %v", err)
	}
	if !contains(string(body), peer.GetCertPem()) {
		t.Error("the derived peer-ca.pem does not carry the pinned certificate")
	}
}

// TestTheDerivedPeerCAFileVerifiesACombsOwnCertificate is the property
// the derived file exists for.
//
// The file is the trust anchor a managerd verifies a peer's presented
// certificate against. So the question is not "does the file contain
// the certificate" but "does a handshake with this file as the root
// succeed" - and the answer has to be checked with Go's own verifier,
// not inferred.
func TestTheDerivedPeerCAFileVerifiesACombsOwnCertificate(t *testing.T) {
	certPEM, fingerprint, notAfter := caIssuedPeerCert(t, "brood")
	peer := &internalpb.TrustedPeer{
		NodeId:       "brood.lab3.apiary.work",
		CombName:     "brood",
		Fingerprint:  fingerprint,
		CertPem:      certPEM,
		NotAfterUnix: notAfter,
		PinnedAtUnix: pinTestNow.Unix(),
		PinnedBy:     "brood",
		IsVoter:      false,
	}

	fsm := newTestFSM(t)
	if res := applyPin(t, fsm, 1, pinCmd(peer)); res.Error != "" {
		t.Fatalf("a CA-issued certificate was refused: %s", res.Error)
	}
	body, err := peerca.Render(storeAsSlice(fsm))
	if err != nil {
		t.Fatalf("rendering the derived peer-ca.pem failed: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(body) {
		t.Fatal("the derived file did not parse as certificates")
	}
	leaf, err := x509.ParseCertificate(mustDecodePEM(t, peer.GetCertPem()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   "brood.lab3.apiary.work",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("the derived peer-ca.pem does not verify the certificate it pins: %v", err)
	}
}

// TestPinTrustedPeerStillRefusesACertificateTheGateRefuses is the
// fail-closed property: the new acceptance is narrow. A CA-issued
// certificate is accepted; a certificate that is genuinely broken is
// still refused, by name.
func TestPinTrustedPeerStillRefusesACertificateTheGateRefuses(t *testing.T) {
	certPEM, fingerprint, notAfter := caIssuedPeerCert(t, "brood")
	peer := &internalpb.TrustedPeer{
		NodeId:       "brood.lab3.apiary.work",
		CombName:     "brood",
		Fingerprint:  fingerprint,
		CertPem:      certPEM,
		NotAfterUnix: notAfter,
		PinnedAtUnix: pinTestNow.Unix(),
		PinnedBy:     "brood",
		IsVoter:      false,
	}
	// A fingerprint that does not match the certificate it carries is
	// still refused, CA-issued or not.
	peer.Fingerprint = "SHA256:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00"

	fsm := newTestFSM(t)
	res := applyPin(t, fsm, 1, pinCmd(peer))
	refuseRefusal(t, res, "fingerprint")
}

func mustDecodePEM(t *testing.T, pemText string) []byte {
	t.Helper()
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		t.Fatal("the pinned certificate did not decode as PEM")
	}
	return block.Bytes
}

// storeAsSlice turns the FSM's map of pinned peers into the slice
// the derived-file writer takes. The map is the FSM's own state; the
// writer sorts, so the order here does not matter.
func storeAsSlice(fsm *FSM) []*internalpb.TrustedPeer {
	out := make([]*internalpb.TrustedPeer, 0, len(fsm.trustedPeers))
	for _, peer := range fsm.trustedPeers {
		out = append(out, peer)
	}
	return out
}
