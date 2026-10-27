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
func caIssued(t *testing.T, commonName string, dnsNames []string, ipAddresses []net.IP) (certPEM []byte) {
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
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
}

// TestACertificateIssuedByACACanBeEvaluated covers the case the peer
// trust store was blocked on: a Comb's serving certificate that a
// certificate authority issued, rather than one it signed itself.
//
// The live case was a Let's Encrypt leaf with a DNS SAN of
// brood.lab3.apiary.work and NO IP:127.0.0.1 address, so this test
// reproduces both properties.
func TestACertificateIssuedByACACanBeEvaluated(t *testing.T) {
	certPEM := caIssued(t, "brood.lab3.apiary.work", []string{"brood.lab3.apiary.work"}, nil)

	info, err := Evaluate(certPEM, EvaluateOptions{
		RequireDNSSAN: true, RequireLoopbackSAN: true, AllowCAIssued: true,
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
	certPEM := caIssued(t, "brood.lab3.apiary.work", []string{"brood.lab3.apiary.work"}, nil)

	if _, err := Evaluate(certPEM, EvaluateOptions{
		RequireDNSSAN: true, RequireLoopbackSAN: true, AllowCAIssued: true,
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
		noDNS := caIssued(t, "brood.lab3.apiary.work", nil, nil)
		_, err := Evaluate(noDNS, EvaluateOptions{
			RequireDNSSAN: true, RequireLoopbackSAN: true, AllowCAIssued: true,
		})
		if err == nil || !strings.Contains(err.Error(), "no DNS subject alternative name") {
			t.Fatalf("Evaluate() error = %v, want the DNS-SAN refusal", err)
		}
	})

	t.Run("an expired CA-issued certificate is still refused", func(t *testing.T) {
		expired := caIssued(t, "brood.lab3.apiary.work", []string{"brood.lab3.apiary.work"}, nil)
		_ = expired
		// Rebuild with an expired window by evaluating as of a later
		// clock: the gate's Now is the only lever a caller has, and
		// the pin path uses it to stay deterministic.
		_, err := Evaluate(expired, EvaluateOptions{
			Now:           now.Add(100 * 365 * 24 * time.Hour),
			RequireDNSSAN: true, AllowCAIssued: true,
		})
		if err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("Evaluate() error = %v, want the expiry refusal", err)
		}
	})

	t.Run("a CA-issued certificate with no BasicConstraints is refused", func(t *testing.T) {
		noBC := caIssuedWithoutBasicConstraints(t, "brood.lab3.apiary.work")
		_, err := Evaluate(noBC, EvaluateOptions{
			RequireDNSSAN: true, AllowCAIssued: true,
		})
		if err == nil || !strings.Contains(err.Error(), "BasicConstraints") {
			t.Fatalf("Evaluate() error = %v, want the BasicConstraints refusal", err)
		}
	})
}

// caIssuedWithoutBasicConstraints builds a CA-issued certificate
// whose leaf carries no BasicConstraints extension at all.
func caIssuedWithoutBasicConstraints(t *testing.T, commonName string) []byte {
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
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
}

// TestACertificateIssuedByACACanBeRefusedWhenTheCallerDoesNotAskForIt
// is the fail-closed property that matters most: the new behaviour is
// opt-in. A caller that needs a Comb's own self-signed certificate
// still gets the old refusal, so nothing that depended on the old
// gate has silently changed.
func TestACertificateIssuedByACACanBeRefusedWhenTheCallerDoesNotAskForIt(t *testing.T) {
	certPEM := caIssued(t, "brood.lab3.apiary.work", []string{"brood.lab3.apiary.work"}, nil)

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
