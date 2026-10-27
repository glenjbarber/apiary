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

// caIssuedPeerCerts builds certificates that ONE certificate
// authority issued, in the shape a Colony's Comb carries once the
// Cloudflare/ACME work (ADR-0133) has moved it onto a publicly
// trusted certificate: a DNS SAN of the Comb's name, and NO loopback
// address, because a Let's Encrypt leaf carries none.
//
// Every leaf shares one CA, because that is the shape a Colony on a
// single public CA has, and because it is what makes the CA - rather
// than any one leaf - the trust anchor.
func caIssuedPeerCerts(t *testing.T, combNames ...string) (caPEM, caFingerprint string, leaves map[string]string, fingerprints map[string]string, notAfters map[string]int64) {
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
	caPEMBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	leaves = map[string]string{}
	fingerprints = map[string]string{}
	notAfters = map[string]int64{}
	for i, combName := range combNames {
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		fqdn := combName + ".lab3.apiary.work"
		leafTmpl := &x509.Certificate{
			SerialNumber:          big.NewInt(int64(i) + 2),
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
		leafPEMBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})

		// The gate a pin applies evaluates the leaf AGAINST the CA
		// that signed it, so the CA travels with the pin
		// (ADR-0185).
		info, err := hostcert.Evaluate(leafPEMBytes, hostcert.EvaluateOptions{
			Now:                pinTestNow,
			RequireDNSSAN:      true,
			RequireLoopbackSAN: true,
			AllowCAIssued:      true,
			CAAnchorPEM:        caPEMBytes,
		})
		if err != nil {
			t.Fatalf("the CA-issued test certificate for %s does not pass the gate the pin applies: %v", combName, err)
		}
		leaves[combName] = string(leafPEMBytes)
		fingerprints[combName] = info.Fingerprint
		notAfters[combName] = info.NotAfter.Unix()
		// The CA's digest is the same one the gate derived, and
		// managerd puts it on the wire before the FSM sees the pin.
		caFingerprint = info.CAFingerprint
	}
	return string(caPEMBytes), caFingerprint, leaves, fingerprints, notAfters
}

// caIssuedPeerCert is caIssuedPeerCerts for a single Comb.
func caIssuedPeerCert(t *testing.T, combName string) (certPEM, caPEM, caFingerprint, fingerprint string, notAfter int64) {
	t.Helper()
	ca, caFP, leaves, fingerprints, notAfters := caIssuedPeerCerts(t, combName)
	return leaves[combName], ca, caFP, fingerprints[combName], notAfters[combName]
}

// pinPeer builds the pin for a Comb from a certificate and its CA.
func pinPeer(combName, certPEM, caPEM, caFingerprint, fingerprint string, notAfter int64) *internalpb.TrustedPeer {
	return &internalpb.TrustedPeer{
		NodeId:        combName + ".lab3.apiary.work",
		CombName:      combName,
		Fingerprint:   fingerprint,
		CertPem:       certPEM,
		CaPem:         caPEM,
		CaFingerprint: caFingerprint,
		NotAfterUnix:  notAfter,
		PinnedAtUnix:  pinTestNow.Unix(),
		PinnedBy:      "brood",
		IsVoter:       false,
	}
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
	certPEM, caPEM, caFP, fingerprint, notAfter := caIssuedPeerCert(t, "brood")
	peer := pinPeer("brood", certPEM, caPEM, caFP, fingerprint, notAfter)

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
	if stored.GetCaPem() == "" {
		t.Error("the pin was recorded without its CA anchor, so there is no trust anchor for it")
	}
	body, err := peerca.Render(storeAsSlice(fsm))
	if err != nil {
		t.Fatalf("rendering the derived peer-ca.pem failed: %v", err)
	}
	if !contains(string(body), stored.GetCaPem()) {
		t.Error("the derived peer-ca.pem does not carry the pinned CA anchor")
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
	certPEM, caPEM, caFP, fingerprint, notAfter := caIssuedPeerCert(t, "brood")
	peer := pinPeer("brood", certPEM, caPEM, caFP, fingerprint, notAfter)

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

// TestTheDerivedPeerCAFileVerifiesEveryLeafItsCAsigned is the reason
// the store records the CA rather than the leaf.
//
// A pin list - one entry per exact leaf - verifies only the exact
// certificate it holds. A trust SET - the CA that signed the leaf -
// verifies every leaf that CA signed. So the derived file must verify
// a DIFFERENT Comb's leaf too, and that is only true when the anchor
// is the CA.
//
// This is checked with Go's own verifier rather than asserted, because
// the whole claim is about what a verifier does.
func TestTheDerivedPeerCAFileVerifiesEveryLeafItsCAsigned(t *testing.T) {
	caPEM, caFP, leaves, fingerprints, notAfters := caIssuedPeerCerts(t, "brood", "drone")

	// Only brood is pinned. The store holds ONE entry, and the
	// derived file holds the CA that signed both leaves.
	fsm := newTestFSM(t)
	brood := pinPeer("brood", leaves["brood"], caPEM, caFP, fingerprints["brood"], notAfters["brood"])
	if res := applyPin(t, fsm, 1, pinCmd(brood)); res.Error != "" {
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
	// Drone is NOT pinned, and its leaf still verifies - because the
	// anchor is the CA, and the CA signed it.
	droneLeaf, err := x509.ParseCertificate(mustDecodePEM(t, leaves["drone"]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := droneLeaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   "drone.lab3.apiary.work",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("the derived peer-ca.pem does not verify a leaf its CA signed: %v", err)
	}
}

// TestPinTrustedPeerStillRefusesACertificateTheGateRefuses is the
// fail-closed property: the new acceptance is narrow. A CA-issued
// certificate is accepted; a certificate that is genuinely broken is
// still refused, by name.
func TestPinTrustedPeerStillRefusesACertificateTheGateRefuses(t *testing.T) {
	certPEM, caPEM, caFP, fingerprint, notAfter := caIssuedPeerCert(t, "brood")
	peer := pinPeer("brood", certPEM, caPEM, caFP, fingerprint, notAfter)
	// A fingerprint that does not match the certificate it carries is
	// still refused, CA-issued or not.
	peer.Fingerprint = "SHA256:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00"

	fsm := newTestFSM(t)
	res := applyPin(t, fsm, 1, pinCmd(peer))
	refuseRefusal(t, res, "fingerprint")
}

// TestPinTrustedPeerRefusesACAFingerprintThatDoesNotMatchItsCA is the
// same fail-closed property for the CA anchor: a pin that states a CA
// digest which is not the digest of the CA it carries is refused, so
// the store cannot advertise one anchor and verify another.
func TestPinTrustedPeerRefusesACAFingerprintThatDoesNotMatchItsCA(t *testing.T) {
	certPEM, caPEM, caFP, fingerprint, notAfter := caIssuedPeerCert(t, "brood")
	peer := pinPeer("brood", certPEM, caPEM, caFP, fingerprint, notAfter)
	peer.CaFingerprint = "SHA256:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00"

	fsm := newTestFSM(t)
	res := applyPin(t, fsm, 1, pinCmd(peer))
	refuseRefusal(t, res, "ca_fingerprint")
}

// TestPinTrustedPeerRefusesACaThatDidNotSignTheCertificate is the
// check that makes a CA anchor honest rather than a claim.
//
// A CA that did not sign the certificate anchors nothing: the derived
// peer-ca.pem would hold a certificate that verifies no peer's leaf,
// and the failure would surface as an opaque TLS handshake error
// against a host nobody could explain.
func TestPinTrustedPeerRefusesACaThatDidNotSignTheCertificate(t *testing.T) {
	certPEM, _, _, fingerprint, notAfter := caIssuedPeerCert(t, "brood")
	// A different CA entirely - the same shape, a different key.
	otherCAPEM, _, _, _, _ := caIssuedPeerCerts(t, "drone")
	peer := pinPeer("brood", certPEM, otherCAPEM, "", fingerprint, notAfter)

	fsm := newTestFSM(t)
	res := applyPin(t, fsm, 1, pinCmd(peer))
	refuseRefusal(t, res, "did not sign")
}

// TestASelfSignedPinStillAnchorsItself is the property that the
// change is not a change for a Colony that never moved onto public
// certificates: a self-signed entry has no CA, and the derived file
// holds the certificate itself, exactly as it always did.
func TestASelfSignedPinStillAnchorsItself(t *testing.T) {
	certPEM, fingerprint, notAfter := generatePinTestCert(t, "brood", 3650*24*time.Hour)
	peer := &internalpb.TrustedPeer{
		NodeId:       "brood.lab3.home.arpa",
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
		t.Fatalf("a self-signed certificate was refused: %s", res.Error)
	}
	stored := fsm.trustedPeers[peer.GetNodeId()]
	if stored == nil {
		t.Fatal("the self-signed pin was not recorded in the trust store")
	}
	if stored.GetCaPem() != "" {
		t.Error("a self-signed pin was recorded with a CA anchor; it anchors itself")
	}
	body, err := peerca.Render(storeAsSlice(fsm))
	if err != nil {
		t.Fatalf("rendering the derived peer-ca.pem failed: %v", err)
	}
	if !contains(string(body), certPEM) {
		t.Error("the derived peer-ca.pem does not carry the self-signed certificate")
	}
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
