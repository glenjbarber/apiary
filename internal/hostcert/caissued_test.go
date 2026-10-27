package hostcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// caIssued builds a certificate that a real certificate authority
// issued: a CA signs a leaf, so the leaf's issuer is the CA and its
// subject is the Comb.
//
// This is the shape a Colony's Combs take once the Cloudflare/ACME
// work (ADR-0133) has moved them onto publicly trusted certificates.
// A Let's Encrypt leaf is issued by Let's Encrypt, and refusing it
// meant the peer trust store could never be populated on such a
// Colony.
func caIssued(t *testing.T, commonName string, dnsNames []string, ipAddresses []net.IP) (certPEM, caPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test Issuing CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(3650 * 24 * time.Hour),
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
	leafTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"apiary"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(3650 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddresses,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

// TestACertificateIssuedByACACanBeEvaluated covers the case the peer
// trust store was blocked on: a Comb's serving certificate that a
// certificate authority issued, rather than one it signed itself.
//
// The live case was a Let's Encrypt leaf with a DNS SAN of
// brood.lab3.apiary.work and NO IP:127.0.0.1 address, so this test
// reproduces both properties.
func TestACertificateIssuedByACACanBeEvaluated(t *testing.T) {
	certPEM, caPEM := caIssued(t, "brood.lab3.apiary.work", []string{"brood.lab3.apiary.work"}, nil)

	info, err := Evaluate(certPEM, EvaluateOptions{
		RequireDNSSAN: true, RequireLoopbackSAN: true, AllowCAIssued: true,
		CAAnchorPEM: caPEM,
	})
	if err != nil {
		t.Fatalf("Evaluate() refused a CA-issued certificate: %v", err)
	}
	if info.Subject != "CN=brood.lab3.apiary.work,O=apiary" {
		t.Errorf("Subject = %q, want the Comb's own name", info.Subject)
	}
	if info.Fingerprint == "" {
		t.Error("Fingerprint is empty; a pin re-derives it from the certificate")
	}
	if !strings.Contains(info.Issuer, "Test Issuing CA") {
		t.Errorf("Issuer = %q, want the issuing CA's name", info.Issuer)
	}
}

// TestACertificateIssuedByACACanBeEvaluatedWithoutALoopbackSAN is
// the part that is easy to get wrong.
//
// The loopback SAN is required of a Comb's OWN self-signed serving
// certificate, because the daemons that dial managerd default to
// loopback (ADR-0139). A publicly trusted certificate is dialled BY
// NAME, and carries no loopback address at all - so requiring one of
// it would refuse every Let's Encrypt leaf, which is exactly what
// left the trust store empty.
func TestACertificateIssuedByACACanBeEvaluatedWithoutALoopbackSAN(t *testing.T) {
	certPEM, caPEM := caIssued(t, "brood.lab3.apiary.work", []string{"brood.lab3.apiary.work"}, nil)

	if _, err := Evaluate(certPEM, EvaluateOptions{
		RequireDNSSAN: true, RequireLoopbackSAN: true, AllowCAIssued: true,
		CAAnchorPEM: caPEM,
	}); err != nil {
		t.Fatalf("Evaluate() refused a CA-issued certificate with no loopback SAN: %v", err)
	}
}

// TestACertificateIssuedByACACanStillBeRefusedByItsOwnChecks proves
// the new branch is not a blanket pass. The CA path relaxes exactly
// two things - the issuer-is-the-subject check and the loopback SAN -
// and nothing else. Every other gate still refuses.
func TestACertificateIssuedByACACanStillBeRefusedByItsOwnChecks(t *testing.T) {
	t.Run("a CA-issued certificate with no DNS name is still refused", func(t *testing.T) {
		noDNS, noDNSCA := caIssued(t, "brood.lab3.apiary.work", nil, nil)
		_, err := Evaluate(noDNS, EvaluateOptions{
			RequireDNSSAN: true, RequireLoopbackSAN: true, AllowCAIssued: true,
			CAAnchorPEM: noDNSCA,
		})
		if err == nil || !strings.Contains(err.Error(), "no DNS subject alternative name") {
			t.Fatalf("Evaluate() error = %v, want the DNS-SAN refusal", err)
		}
	})

	t.Run("an expired CA-issued certificate is still refused", func(t *testing.T) {
		expired, expiredCA := caIssued(t, "brood.lab3.apiary.work", []string{"brood.lab3.apiary.work"}, nil)
		_ = expired
		// Rebuild with an expired window by evaluating as of a later
		// clock: the gate's Now is the only lever a caller has, and
		// the pin path uses it to stay deterministic.
		_, err := Evaluate(expired, EvaluateOptions{
			Now:           now.Add(100 * 365 * 24 * time.Hour),
			RequireDNSSAN: true, AllowCAIssued: true,
			CAAnchorPEM: expiredCA,
		})
		if err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("Evaluate() error = %v, want the expiry refusal", err)
		}
	})

	t.Run("a CA-issued certificate with no BasicConstraints is refused", func(t *testing.T) {
		noBC, noBCA := caIssuedWithoutBasicConstraints(t, "brood.lab3.apiary.work")
		_, err := Evaluate(noBC, EvaluateOptions{
			RequireDNSSAN: true, AllowCAIssued: true,
			CAAnchorPEM: noBCA,
		})
		if err == nil || !strings.Contains(err.Error(), "BasicConstraints") {
			t.Fatalf("Evaluate() error = %v, want the BasicConstraints refusal", err)
		}
	})
}

// caIssuedWithoutBasicConstraints builds a CA-issued certificate
// whose leaf carries no BasicConstraints extension at all.
func caIssuedWithoutBasicConstraints(t *testing.T, commonName string) (certPEM, caPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test Issuing CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(3650 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: commonName, Organization: []string{"apiary"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(3650 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{commonName},
		// BasicConstraintsValid deliberately left false.
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

// TestACertificateIssuedByACACanBeRefusedWhenTheCallerDoesNotAskForIt
// is the fail-closed property that matters most: the new behaviour is
// opt-in. A caller that needs a Comb's own self-signed certificate
// still gets the old refusal, so nothing that depended on the old
// gate has silently changed.
func TestACertificateIssuedByACACanBeRefusedWhenTheCallerDoesNotAskForIt(t *testing.T) {
	certPEM, _ := caIssued(t, "brood.lab3.apiary.work", []string{"brood.lab3.apiary.work"}, nil)

	_, err := Evaluate(certPEM, EvaluateOptions{
		RequireDNSSAN: true, RequireLoopbackSAN: true,
	})
	if err == nil {
		t.Fatal("Evaluate() accepted a CA-issued certificate without AllowCAIssued; the new path must be opt-in")
	}
	if !strings.Contains(err.Error(), "not a Comb's own self-signed certificate") {
		t.Errorf("Evaluate() error = %v, want the self-signed refusal", err)
	}
}

// TestASelfSignedCertificateStillPassesItsOwnGate is the other half of
// the fail-closed property: the self-signed path is untouched, and a
// certificate that signs itself still verifies its own signature.
func TestASelfSignedCertificateStillPassesItsOwnGate(t *testing.T) {
	certPEM, _ := sign(t, GenerateOptions{
		CommonName:  "brood.lab3.apiary.work",
		DNSNames:    []string{"brood.lab3.apiary.work"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}, nil)

	if _, err := Evaluate(certPEM, EvaluateOptions{
		RequireDNSSAN: true, RequireLoopbackSAN: true, AllowCAIssued: true,
	}); err != nil {
		t.Fatalf("Evaluate() refused a self-signed certificate when AllowCAIssued was set: %v", err)
	}
}

// TestTheCAAnchorIsReportedAndItsDigestDerived covers the reporting
// half of ADR-0185: the CA that signed the certificate becomes the
// recorded trust anchor, and its digest is derived rather than
// stated.
//
// The anchor is reported as normalized PEM, so it can be handed to
// crypto/x509 as a root directly and so that every replica holds the
// same bytes for the same anchor.
func TestTheCAAnchorIsReportedAndItsDigestDerived(t *testing.T) {
	certPEM, caPEM := caIssued(t, "brood.lab3.apiary.work", []string{"brood.lab3.apiary.work"}, nil)

	info, err := Evaluate(certPEM, EvaluateOptions{
		RequireDNSSAN: true, AllowCAIssued: true,
		CAAnchorPEM: caPEM,
	})
	if err != nil {
		t.Fatalf("Evaluate() refused a CA-issued certificate: %v", err)
	}
	if info.CAAnchor == "" {
		t.Fatal("Info.CAAnchor is empty; the CA is the recorded trust anchor")
	}
	block, _ := pem.Decode([]byte(info.CAAnchor))
	if block == nil {
		t.Fatal("Info.CAAnchor is not PEM")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("Info.CAAnchor does not parse as a certificate: %v", err)
	}
	if !caCert.IsCA {
		t.Error("Info.CAAnchor is not a certificate authority")
	}
	if info.CAFingerprint != Fingerprint(caCert.Raw) {
		t.Errorf("Info.CAFingerprint = %q, want the digest of the anchor %q", info.CAFingerprint, Fingerprint(caCert.Raw))
	}
}

// TestASelfSignedCertificateReportsNoCAAnchor is the other half: a
// self-signed certificate anchors itself, so it has no CA anchor and
// its Info says so. This is the shape every entry written before
// ADR-0185 has.
func TestASelfSignedCertificateReportsNoCAAnchor(t *testing.T) {
	certPEM, _, _ := Generate(combOpts())
	info, err := Evaluate(certPEM, EvaluateOptions{
		Now:                now,
		RequireDNSSAN:      true,
		RequireLoopbackSAN: true,
	})
	if err != nil {
		t.Fatalf("Evaluate() refused a self-signed certificate: %v", err)
	}
	if info.CAAnchor != "" || info.CAFingerprint != "" {
		t.Errorf("a self-signed certificate reported a CA anchor (%q / %q); it anchors itself", info.CAAnchor, info.CAFingerprint)
	}
}

// TestACertificateIssuedByACACanBeRefusedWithNoCAAnchor is the
// fail-closed property that matters most about the CA-anchor model.
//
// A CA-issued certificate with NO CA supplied is refused rather than
// accepted on the leaf's own say-so. Accepting it would record a
// leaf as its own trust anchor - the pin list ADR-0185 replaces -
// and it would do so without anything ever having checked that the
// certificate chains to anything at all.
func TestACertificateIssuedByACACanBeRefusedWithNoCAAnchor(t *testing.T) {
	certPEM, _ := caIssued(t, "brood.lab3.apiary.work", []string{"brood.lab3.apiary.work"}, nil)

	_, err := Evaluate(certPEM, EvaluateOptions{
		RequireDNSSAN: true, AllowCAIssued: true,
	})
	if err == nil {
		t.Fatal("Evaluate() accepted a CA-issued certificate with no CA anchor")
	}
	if !strings.Contains(err.Error(), "no issuing CA was supplied") {
		t.Errorf("Evaluate() error = %v, want the refusal that names the missing anchor", err)
	}
}

// TestACaThatDidNotSignTheCertificateIsRefused is the check that makes
// a CA anchor honest rather than a claim.
//
// A CA that did not sign the certificate anchors nothing: the derived
// peer-ca.pem would hold a certificate that verifies no peer's leaf,
// and the failure would surface as an opaque TLS handshake error
// against a host nobody could explain.
func TestACaThatDidNotSignTheCertificateIsRefused(t *testing.T) {
	certPEM, _ := caIssued(t, "brood.lab3.apiary.work", []string{"brood.lab3.apiary.work"}, nil)
	// A different CA: the same shape, a different key.
	_, otherCA := caIssued(t, "drone.lab3.apiary.work", []string{"drone.lab3.apiary.work"}, nil)

	_, err := Evaluate(certPEM, EvaluateOptions{
		RequireDNSSAN: true, AllowCAIssued: true,
		CAAnchorPEM: otherCA,
	})
	if err == nil {
		t.Fatal("Evaluate() accepted a CA that did not sign the certificate")
	}
	if !strings.Contains(err.Error(), "did not sign") {
		t.Errorf("Evaluate() error = %v, want the refusal that names the mismatch", err)
	}
}

// TestACaThatIsNotACertificateAuthorityIsRefused is the other
// fail-closed check: a leaf's issuer that is not a CA cannot anchor
// a trust decision, and putting it in the derived file would publish
// an anchor that verifies nothing.
func TestACaThatIsNotACertificateAuthorityIsRefused(t *testing.T) {
	// A self-signed LEAF is not a CA, so it cannot be an anchor.
	leafPEM, _, _ := Generate(combOpts())
	certPEM, _ := caIssued(t, "brood.lab3.apiary.work", []string{"brood.lab3.apiary.work"}, nil)

	_, err := Evaluate(certPEM, EvaluateOptions{
		RequireDNSSAN: true, AllowCAIssued: true,
		CAAnchorPEM: leafPEM,
	})
	if err == nil {
		t.Fatal("Evaluate() accepted a non-CA as the trust anchor")
	}
	if !strings.Contains(err.Error(), "not a certificate authority") {
		t.Errorf("Evaluate() error = %v, want the refusal that names the non-CA anchor", err)
	}
}

// TestTheTrustAnchorVerifiesTheLeafItSigned is the property the whole
// CA-anchor model rests on, checked with Go's own verifier rather
// than asserted.
//
// The CA is recorded as the trust anchor, and a peer presents a
// LEAF. So the anchor must verify the leaf - and, because the anchor
// is the CA rather than the leaf, it must verify EVERY leaf that CA
// signed, which is what makes the derived file a trust set rather
// than a pin list.
func TestTheTrustAnchorVerifiesTheLeafItSigned(t *testing.T) {
	certPEM, caPEM := caIssued(t, "brood.lab3.apiary.work", []string{"brood.lab3.apiary.work"}, nil)

	info, err := Evaluate(certPEM, EvaluateOptions{
		RequireDNSSAN: true, AllowCAIssued: true,
		CAAnchorPEM: caPEM,
	})
	if err != nil {
		t.Fatalf("Evaluate() refused a CA-issued certificate: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(info.CAAnchor)) {
		t.Fatal("the reported CA anchor did not parse as a certificate")
	}
	leafBlock, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   "brood.lab3.apiary.work",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("the CA anchor does not verify the leaf it signed: %v", err)
	}
}
