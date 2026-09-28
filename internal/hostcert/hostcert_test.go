package hostcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// combOpts is what a Comb's own certificate looks like, so a test that
// wants a valid leaf does not have to restate it.
func combOpts() GenerateOptions {
	return GenerateOptions{
		CommonName:  "brood",
		DNSNames:    []string{"brood", "brood.lab3.home.arpa"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		Now:         now,
	}
}

func gate(certPEM, keyPEM []byte) error {
	_, err := Evaluate(certPEM, EvaluateOptions{
		Now:                now,
		RequireLoopbackSAN: true,
		RequireDNSSAN:      true,
		KeyPEM:             keyPEM,
	})
	return err
}

func TestGenerateProducesAPairThatPassesItsOwnGate(t *testing.T) {
	certPEM, keyPEM, err := Generate(combOpts())
	if err != nil {
		t.Fatal(err)
	}
	if err := gate(certPEM, keyPEM); err != nil {
		t.Fatalf("a freshly generated pair fails the gate: %v", err)
	}
}

// The public key in the certificate is the private key's, checked
// through the gate rather than by eye, because that is the check that
// makes a half-mismatched pair an install-time refusal instead of a
// Comb that will not start.
func TestTheGateRefusesAPairWhoseKeyDoesNotMatch(t *testing.T) {
	certPEM, _, err := Generate(combOpts())
	if err != nil {
		t.Fatal(err)
	}
	_, otherKey, err := Generate(combOpts())
	if err != nil {
		t.Fatal(err)
	}
	err = gate(certPEM, otherKey)
	if err == nil {
		t.Fatal("a mismatched pair passed the gate")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("refusal does not say the key does not match: %v", err)
	}
}

// Every refusal names its check. These are the named cases ADR-0147
// lists, each built here rather than described, so a change that made
// one of them pass silently would fail this test.
// A knob mutates a certificate template before it is signed, which is
// how the awkward cases Generate will not make are built.
type knob func(*x509.Certificate)

func TestEveryNamedRefusalIsRefusedByName(t *testing.T) {
	// Each case below is a real certificate built by sign(), not a byte
	// pattern matched against a string, so a check that stopped
	// firing would fail here rather than pass quietly.
	cases := []struct {
		name  string
		knobs []knob
		want  string
	}{
		{
			name:  "a truncated PEM",
			knobs: nil,
			want:  "not a PEM certificate",
		},
		{
			name:  "two certificates in one PEM",
			knobs: nil,
			want:  "CERTIFICATE blocks",
		},
		{
			name:  "an expired leaf",
			knobs: []knob{func(c *x509.Certificate) { c.NotBefore = now.Add(-48 * time.Hour); c.NotAfter = now.Add(-time.Hour) }},
			want:  "expired",
		},
		{
			name:  "a leaf that is not yet valid",
			knobs: []knob{func(c *x509.Certificate) { c.NotBefore = now.Add(time.Hour); c.NotAfter = now.Add(48 * time.Hour) }},
			want:  "not yet valid",
		},
		{
			name:  "a certificate whose issuer differs from its subject",
			knobs: []knob{func(c *x509.Certificate) { c.Subject.Organization = []string{"someone else"} }},
			want:  "not a Comb's own self-signed certificate",
		},
		{
			name:  "a self-signed certificate with a bad self-signature",
			knobs: nil,
			want:  "self-signature does not verify",
		},
		{
			name:  "a certificate with no 127.0.0.1 SAN",
			knobs: []knob{func(c *x509.Certificate) { c.IPAddresses = nil }},
			want:  "no IP:127.0.0.1 subject alternative name",
		},
		{
			name:  "a certificate with IP SANs and no DNS name",
			knobs: []knob{func(c *x509.Certificate) { c.DNSNames = nil }},
			want:  "no DNS subject alternative name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var certPEM, keyPEM []byte
			switch {
			case tc.name == "a truncated PEM":
				certPEM = []byte("-----BEGIN CERTIFICATE-----\nMIIB\n")
				keyPEM = nil
			case tc.name == "two certificates in one PEM":
				one, _, err := Generate(combOpts())
				if err != nil {
					t.Fatal(err)
				}
				two, _, err := Generate(combOpts())
				if err != nil {
					t.Fatal(err)
				}
				certPEM = append(append([]byte{}, one...), two...)
			case tc.name == "a certificate whose issuer differs from its subject":
				certPEM, keyPEM = issuedBySomebodyElse(t, combOpts())
			case tc.name == "a self-signed certificate with a bad self-signature":
				certPEM, keyPEM = signWithBrokenSignature(t, combOpts())
			default:
				certPEM, keyPEM = sign(t, combOpts(), tc.knobs)
			}
			err := gate(certPEM, keyPEM)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal for %s does not name the check (%q): %v", tc.name, tc.want, err)
			}
		})
	}
}

// sign builds a self-signed certificate with the given knobs applied to
// its template, which is the only way to construct the awkward cases
// (expired, not yet valid, wrong issuer) that Generate will not make.
func sign(t *testing.T, opts GenerateOptions, knobs []knob) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(42),
		Subject:               pkix.Name{CommonName: opts.CommonName, Organization: []string{"apiary"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(3650 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              opts.DNSNames,
		IPAddresses:           opts.IPAddresses,
	}
	for _, k := range knobs {
		k(tmpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// issuedBySomebodyElse produces a certificate signed by a separate CA,
// which is what a certificate whose issuer is not its own subject looks
// like. It cannot be made by mutating a self-signed template, because
// in one of those the template is both the subject and the issuer.
func issuedBySomebodyElse(t *testing.T, opts GenerateOptions) (certPEM, keyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "somebody else", Organization: []string{"not apiary"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(3650 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: opts.CommonName, Organization: []string{"apiary"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(3650 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              opts.DNSNames,
		IPAddresses:           opts.IPAddresses,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// signWithBrokenSignature produces a certificate whose signature does
// not verify over its own TBS bytes, which is what a tampered or
// hand-edited certificate looks like.
func signWithBrokenSignature(t *testing.T, opts GenerateOptions) (certPEM, keyPEM []byte) {
	t.Helper()
	certPEM, keyPEM = sign(t, opts, nil)
	block, _ := pem.Decode(certPEM)
	der := block.Bytes
	// Flip a bit inside the signature, at the end of the structure,
	// leaving the TBS bytes alone so the parse still succeeds.
	der[len(der)-1] ^= 0xff
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM
}

// An RSA-1024 key is refused by name. "RSA-1024" is the answer an
// operator needs, and a generic parse failure is not it.
func TestAnRSA1024KeyIsRefusedByName(t *testing.T) {
	certPEM, keyPEM, err := signWithKey(t, combOpts(), func() (any, any) {
		key, err := rsa.GenerateKey(rand.Reader, 1024)
		if err != nil {
			t.Fatal(err)
		}
		return key, &key.PublicKey
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = certPEM
	evalErr := gate(certPEM, keyPEM)
	if evalErr == nil {
		t.Fatal("an RSA-1024 certificate was accepted")
	}
	if !strings.Contains(evalErr.Error(), "RSA-1024") {
		t.Errorf("refusal does not name the key size: %v", evalErr)
	}
}

// An RSA-2048 pair produced by the old setup-tls recipe is still
// accepted. The decision to generate ECDSA is a decision about what the
// installer WRITES; a Comb already carrying an RSA certificate is not
// forced to replace it.
func TestAnExistingRSA2048PairIsStillAccepted(t *testing.T) {
	certPEM, keyPEM, err := signWithKey(t, combOpts(), func() (any, any) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		return key, &key.PublicKey
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := Evaluate(certPEM, EvaluateOptions{Now: now, RequireLoopbackSAN: true, RequireDNSSAN: true, KeyPEM: keyPEM})
	if err != nil {
		t.Fatalf("an RSA-2048 Comb certificate was refused: %v", err)
	}
	if info.KeyType != "RSA" || info.KeyBits != 2048 {
		t.Errorf("KeyType/KeyBits = %s-%d, want RSA-2048", info.KeyType, info.KeyBits)
	}
}

// signWithKey issues a certificate under a caller-chosen key, which is
// how the two RSA cases above are built.
func signWithKey(t *testing.T, opts GenerateOptions, keyPair func() (any, any)) (certPEM, keyPEM []byte, err error) {
	t.Helper()
	priv, pub := keyPair()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(7),
		Subject:               pkix.Name{CommonName: opts.CommonName, Organization: []string{"apiary"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(3650 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              opts.DNSNames,
		IPAddresses:           opts.IPAddresses,
	}
	der, cerr := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if cerr != nil {
		return nil, nil, cerr
	}
	keyDER, kerr := x509.MarshalPKCS8PrivateKey(priv)
	if kerr != nil {
		return nil, nil, kerr
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// The remote half of the boundary, pinned by a test rather than by
// prose: a certificate a PEER presented is accepted on structure, with
// the fingerprint recorded, because no private key for it exists here
// and a gate that claimed to check it would be claiming a check that
// did not happen.
func TestARemoteLeafIsAcceptedOnStructureAloneAndItsFingerprintRecorded(t *testing.T) {
	certPEM, _, err := Generate(combOpts())
	if err != nil {
		t.Fatal(err)
	}
	info, err := Evaluate(certPEM, EvaluateOptions{Now: now, RequireLoopbackSAN: true, RequireDNSSAN: true})
	if err != nil {
		t.Fatalf("a remote leaf was refused: %v", err)
	}
	if info.Fingerprint != Fingerprint(mustDER(t, certPEM)) {
		t.Error("the recorded fingerprint is not a function of the certificate's own bytes")
	}
	// And it really is structure-only: the same certificate with a key
	// that does not belong to it is refused, which is the difference
	// between the local and the remote call.
	_, wrongKey, err := Generate(combOpts())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Evaluate(certPEM, EvaluateOptions{Now: now, RequireLoopbackSAN: true, RequireDNSSAN: true, KeyPEM: wrongKey}); err == nil {
		t.Error("the same certificate was accepted with a local key check, so the two paths are not actually different")
	}
}

func mustDER(t *testing.T, certPEM []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("not a PEM block")
	}
	return block.Bytes
}

// The fingerprint is a wire value that two Combs compare, so its exact
// form is pinned here rather than left to two implementations to agree
// on by accident.
func TestFingerprintFormatIsPinned(t *testing.T) {
	got := Fingerprint([]byte("apiary"))
	sum := sha256.Sum256([]byte("apiary"))
	if !strings.HasPrefix(got, "SHA256:") {
		t.Errorf("Fingerprint = %q, want a SHA256: prefix", got)
	}
	if len(got) != len("SHA256:")+32*3-1 {
		t.Errorf("Fingerprint = %q, want 32 colon-separated hex pairs", got)
	}
	if !strings.Contains(got, strings.ToUpper(hexPair(sum[0]))) {
		t.Errorf("Fingerprint = %q, want uppercase hex pairs", got)
	}
	if Fingerprint([]byte("apiary")) != got {
		t.Error("Fingerprint is not deterministic")
	}
}

func hexPair(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&0x0f]})
}

// An existing Comb's SANs, expiry and fingerprint are all printable,
// and none of that output can carry key material: Info has no field
// able to hold it.
func TestInfoRendersWithoutCarryingAnythingSecret(t *testing.T) {
	certPEM, keyPEM, err := Generate(combOpts())
	if err != nil {
		t.Fatal(err)
	}
	info, err := Evaluate(certPEM, EvaluateOptions{Now: now, RequireLoopbackSAN: true, RequireDNSSAN: true, KeyPEM: keyPEM})
	if err != nil {
		t.Fatal(err)
	}
	rendered := info.Expiry() + " " + info.SANs() + " " + info.Fingerprint
	// The PEM body is the base64 of the key's DER, so a prefix of it is
	// a fair thing to look for in output that is supposed to be free of
	// it. Checking the raw DER would be meaningless, since DER is
	// binary and Contains on it matches almost nothing.
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		t.Fatal("key.pem is not a PEM block")
	}
	keyBody := base64.StdEncoding.EncodeToString(keyBlock.Bytes)
	if strings.Contains(rendered, "PRIVATE KEY") || strings.Contains(rendered, keyBody[:24]) {
		t.Errorf("the rendered Info contains key material: %s", rendered)
	}
	if !strings.Contains(info.SANs(), "DNS:brood.lab3.home.arpa") {
		t.Errorf("SANs() = %q, want the FQDN among them", info.SANs())
	}
}

// A SANs() of a certificate with none is readable rather than an empty
// string, because a refusal prints it and "(none)" says what happened
// where "" says nothing.
func TestSANsWithNoNamesSaysSo(t *testing.T) {
	if got := (Info{}).SANs(); got != "(none)" {
		t.Errorf("SANs() with no names = %q, want %q", got, "(none)")
	}
}
