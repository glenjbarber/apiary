// Package hostcert issues and evaluates one Comb's own TLS serving
// certificate: the self-signed pair managerd serves its external API
// with, and the same certificate every peer Comb pins.
//
// It exists as its own package, and not as functions inside
// internal/certmgr, for a reason that is worth stating rather than
// leaving to be rediscovered. internal/certmgr is deliberately
// read-only and structurally incapable of holding key material: it
// reads a certificate that already exists, never opens a *.key, and
// has no returned type that could carry one. Generation is the
// opposite of that, and so is the local half of the evaluation gate,
// which has to compare a certificate's public key against the private
// key sitting next to it. Putting either here would undo a boundary
// that was built on purpose. The evaluation gate's *other* half --
// the same checks applied to a certificate a peer presented, where no
// private key is held and none is read -- is exactly the same code, and
// that is why the gate lives here rather than beside the generator
// alone: one implementation, two callers, one set of named failures.
//
// Two things are load-bearing for the rest of the codebase:
//
//   - Every refusal names the check that refused. A join that fails
//     on "not yet valid" and a join that fails on "two certificates in
//     one PEM" are different operator problems, and a message that
//     says "invalid certificate" sends the reader to the wrong file.
//
//   - The gate is fail-closed. Absence of evidence is a refusal, not a
//     pass, and no option makes a missing check into an accepted one.
//     The only knobs are which checks apply (RequireDNSSAN and
//     RequireLoopbackSAN, both true for a Comb serving certificate) and
//     whether a local private key is available to check against.
//
// The fingerprint format is "SHA256:" followed by colon-separated
// uppercase hex pairs of the DER bytes, which is the format
// internal/manager already publishes and the one ADR-0147's join flow
// compares against. It is defined here once; internal/manager
// delegates to it rather than keeping a second copy of the same
// formatting, because two copies of a fingerprint format is a wire
// compatibility problem waiting for the day they disagree.
package hostcert

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// DefaultValidity is the lifetime a generated certificate gets. It
// matches what the Makefile's setup-tls recipe has always issued
// (3650 days), and it is deliberately NOT shortened: a shorter life
// would create an expiry cliff with no renewal path, and
// internal/certmgr already makes an expiry visible without renewing
// it. Shortening the life without building renewal is strictly worse
// than leaving it.
const DefaultValidity = 3650 * 24 * time.Hour

// notBeforeBackdate is how far before "now" a generated certificate
// starts being valid. A small backdate covers the ordinary case of two
// machines whose clocks disagree by a little, which would otherwise
// present as a certificate that is "not yet valid" for reasons that
// have nothing to do with trust.
const notBeforeBackdate = time.Hour

// Fingerprint formats a DER-encoded certificate's SHA-256 digest as
// colon-separated uppercase hex pairs prefixed "SHA256:" - a plain,
// deterministic function of the raw bytes, so it is independently
// testable without a certificate on disk and it is what two Combs
// compare when one pins the other.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	pairs := make([]string, len(sum))
	for i, b := range sum {
		pairs[i] = fmt.Sprintf("%02X", b)
	}
	return "SHA256:" + strings.Join(pairs, ":")
}

// Info is everything that may be said about a certificate without
// revealing anything about its private key. Every field is public
// material or a public digest, which is what makes it safe to render
// into an installer's report.
type Info struct {
	Subject     string
	Issuer      string
	NotBefore   time.Time
	NotAfter    time.Time
	DNSNames    []string
	IPAddresses []string
	Fingerprint string
	KeyType     string
	KeyBits     int
	Signature   string
}

// Expiry renders the certificate's own validity window in the form a
// report prints it, so that one place decides how a date is worded.
func (i Info) Expiry() string {
	return i.NotBefore.UTC().Format("2006-01-02 15:04:05Z") + " through " + i.NotAfter.UTC().Format("2006-01-02 15:04:05Z")
}

// SANs renders the certificate's subject alternative names as a single
// printable line, in the same order they appear in Info. A refusal
// that prints this is a refusal that can be acted on, because the
// operator can see the name the tool wanted and the names it was given.
func (i Info) SANs() string {
	if len(i.DNSNames) == 0 && len(i.IPAddresses) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(i.DNSNames)+len(i.IPAddresses))
	for _, ip := range i.IPAddresses {
		parts = append(parts, "IP:"+ip)
	}
	for _, dns := range i.DNSNames {
		parts = append(parts, "DNS:"+dns)
	}
	return strings.Join(parts, ", ")
}

// GenerateOptions is what a caller knows about the certificate it
// wants issued. Every field is public: the private key is generated
// here and never leaves as a field a caller can read back by mistake.
type GenerateOptions struct {
	// CommonName is the certificate's subject CN. It is the short
	// hostname on a Comb whose short hostname is its name.
	CommonName string

	// DNSNames are the DNS SANs, in preference order. The first is
	// not special to this package; the caller decides which of them is
	// the resolvable one.
	DNSNames []string

	// IPAddresses are the IP SANs. A Comb serving certificate carries
	// 127.0.0.1 and deliberately nothing else: ADR-0139 is the record
	// of a numeric LAN address being carried into a dial target
	// against a certificate that does not cover it.
	IPAddresses []net.IP

	// Validity overrides DefaultValidity when non-zero.
	Validity time.Duration

	// Now overrides the clock, for tests. Zero means time.Now.
	Now time.Time
}

// Generate issues a self-signed ECDSA P-256 serving certificate and
// its private key, returned as two PEM blocks.
//
// ECDSA P-256 rather than RSA-2048 is a change from what the Makefile's
// setup-tls recipe produced, and it is deliberate: the key is smaller,
// generation is instant, and every consumer here is Go's crypto/tls,
// which has no preference. It is a statement about what this function
// *writes*. Evaluate accepts an RSA-2048 pair unchanged, so a Comb
// carrying one from the old recipe is not forced to replace it.
func Generate(opts GenerateOptions) (certPEM, keyPEM []byte, err error) {
	if opts.CommonName == "" {
		return nil, nil, errors.New("hostcert: CommonName is required")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	validity := opts.Validity
	if validity == 0 {
		validity = DefaultValidity
	}
	if validity <= 0 {
		return nil, nil, fmt.Errorf("hostcert: refusing to issue a certificate with validity %s", validity)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("hostcert: generating key: %w", err)
	}
	serialMax := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialMax)
	if err != nil {
		return nil, nil, fmt.Errorf("hostcert: generating serial: %w", err)
	}
	// Backdated, so two machines whose clocks disagree by a little do
	// not see a certificate that is not yet valid.
	notBefore := now.Add(-notBeforeBackdate)
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   opts.CommonName,
			Organization: []string{"apiary"},
		},
		// Self-signed: the issuer is the subject, set explicitly
		// below by making the template its own parent. A Comb
		// certificate is self-signed by design, and the evaluation
		// gate checks the self-signature rather than trusting the
		// issuer field alone.
		NotBefore:             notBefore,
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dedupeStrings(opts.DNSNames),
	}
	for _, ip := range opts.IPAddresses {
		if ip == nil {
			continue
		}
		template.IPAddresses = append(template.IPAddresses, ip)
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("hostcert: creating certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("hostcert: marshalling key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// EvaluateOptions says which checks apply to a certificate being
// evaluated, and whether a local private key is available to check the
// certificate's public key against.
type EvaluateOptions struct {
	// Now overrides the clock, for tests. Zero means time.Now.
	Now time.Time

	// RequireLoopbackSAN demands an IP:127.0.0.1 subject
	// alternative name. True for a Comb serving certificate: every
	// daemon that dials managerd defaults to loopback, and ADR-0139's
	// first live failure was exactly a certificate missing it.
	RequireLoopbackSAN bool

	// RequireDNSSAN demands at least one DNS subject alternative name.
	// True for a Comb serving certificate, because a multi-Comb Colony
	// has to dial a name (Part 1's address selection refuses to write
	// an rpc_addr the certificate does not cover). False only where a
	// caller is genuinely asking "what names does this carry", which
	// is how a caller reaches the address-selection refusal rather
	// than this gate's own.
	RequireDNSSAN bool

	// KeyPEM, when non-empty, is the local private key the
	// certificate must belong to. Leave it empty for a certificate a
	// peer presented: the public key cannot be checked against a
	// private key nobody here holds, and pretending otherwise would
	// claim a check that did not happen. This is the boundary
	// ADR-0147 Part 4 pins with a test rather than with prose.
	KeyPEM []byte
}

// Evaluate applies the fail-closed certificate gate and returns what
// may be said about the certificate. Every refusal names its check.
func Evaluate(certPEM []byte, opts EvaluateOptions) (Info, error) {
	if len(certPEM) == 0 {
		return Info{}, errors.New("hostcert: certificate is empty")
	}
	der, err := singleCertificateDER(certPEM)
	if err != nil {
		return Info{}, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return Info{}, fmt.Errorf("hostcert: parsing certificate: %w", err)
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	if now.Before(cert.NotBefore) {
		return Info{}, fmt.Errorf("hostcert: certificate is not yet valid (not before %s, now %s)", cert.NotBefore.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	if now.After(cert.NotAfter) {
		return Info{}, fmt.Errorf("hostcert: certificate expired (not after %s, now %s)", cert.NotAfter.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}

	// The issuer is compared to the subject separately from the
	// signature, so that a certificate whose issuer differs is
	// refused for the reason an operator can act on, and a genuinely
	// self-signed certificate with a broken signature is refused for
	// the different reason that it is broken.
	if !bytes.Equal(cert.RawIssuer, cert.RawSubject) {
		return Info{}, fmt.Errorf("hostcert: certificate is issued by %q but its subject is %q, so it is not a Comb's own self-signed certificate", cert.Issuer.String(), cert.Subject.String())
	}
	// CheckSignature, not CheckSignatureFrom: the latter is a CA
	// check, and a Comb's certificate is a self-signed LEAF, which is
	// exactly what CheckSignatureFrom refuses ("parent certificate
	// cannot sign this kind of certificate"). What is being verified
	// here is that the bytes somebody signed are the bytes in the
	// certificate, by the key named in the certificate.
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		return Info{}, fmt.Errorf("hostcert: self-signature does not verify: %w", err)
	}

	keyType, keyBits, err := describeKey(cert.PublicKey)
	if err != nil {
		return Info{}, err
	}
	if err := checkSignatureAlgorithm(cert.SignatureAlgorithm); err != nil {
		return Info{}, err
	}

	if opts.RequireLoopbackSAN && !hasLoopbackSAN(cert) {
		return Info{}, fmt.Errorf("hostcert: certificate has no IP:127.0.0.1 subject alternative name (it has: %s)", Info{DNSNames: cert.DNSNames, IPAddresses: ipStrings(cert.IPAddresses)}.SANs())
	}
	if opts.RequireDNSSAN && len(cert.DNSNames) == 0 {
		return Info{}, fmt.Errorf("hostcert: certificate has no DNS subject alternative name, so no resolvable name to dial (it has: %s)", Info{IPAddresses: ipStrings(cert.IPAddresses)}.SANs())
	}

	if len(opts.KeyPEM) > 0 {
		if err := checkKeyMatches(cert, opts.KeyPEM); err != nil {
			return Info{}, err
		}
	}

	return Info{
		Subject:     cert.Subject.String(),
		Issuer:      cert.Issuer.String(),
		NotBefore:   cert.NotBefore,
		NotAfter:    cert.NotAfter,
		DNSNames:    append([]string(nil), cert.DNSNames...),
		IPAddresses: ipStrings(cert.IPAddresses),
		Fingerprint: Fingerprint(cert.Raw),
		KeyType:     keyType,
		KeyBits:     keyBits,
		Signature:   cert.SignatureAlgorithm.String(),
	}, nil
}

// singleCertificateDER extracts exactly one CERTIFICATE block. A
// truncated PEM and a PEM holding two certificates are different
// operator problems, so they are refused separately rather than both
// arriving as "parse error".
func singleCertificateDER(certPEM []byte) ([]byte, error) {
	var ders [][]byte
	rest := certPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			ders = append(ders, block.Bytes)
		}
	}
	switch {
	case len(ders) == 0:
		return nil, errors.New("hostcert: no CERTIFICATE block found: the file is not a PEM certificate, or it is truncated")
	case len(ders) > 1:
		return nil, fmt.Errorf("hostcert: %d CERTIFICATE blocks in one file, expected exactly 1", len(ders))
	}
	return ders[0], nil
}

// describeKey reports the key's type and size, refusing the ones this
// Colony must not be asked to trust. RSA below 2048 is refused by name
// rather than by a generic parse failure, because "RSA-1024" is the
// answer an operator needs.
func describeKey(pub any) (string, int, error) {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		bits := k.Curve.Params().BitSize
		switch k.Curve {
		case elliptic.P256(), elliptic.P384(), elliptic.P521():
			return "ECDSA", bits, nil
		default:
			return "", 0, fmt.Errorf("hostcert: refusing an ECDSA key on an unsupported curve (%d bits)", bits)
		}
	case *rsa.PublicKey:
		bits := k.N.BitLen()
		if bits < 2048 {
			return "", 0, fmt.Errorf("hostcert: refusing an RSA-%d key: at least RSA-2048 is required", bits)
		}
		return "RSA", bits, nil
	default:
		return "", 0, fmt.Errorf("hostcert: refusing a public key of unsupported type %T", pub)
	}
}

// checkSignatureAlgorithm refuses the signature algorithms that are
// broken rather than merely old. SHA-1 is refused on sight; SHA-256
// and SHA-384 and SHA-512 are accepted, and an unknown algorithm is
// refused rather than assumed fine.
func checkSignatureAlgorithm(alg x509.SignatureAlgorithm) error {
	switch alg {
	case x509.SHA256WithRSA, x509.ECDSAWithSHA256,
		x509.SHA384WithRSA, x509.ECDSAWithSHA384,
		x509.SHA512WithRSA, x509.ECDSAWithSHA512:
		return nil
	case x509.MD5WithRSA, x509.SHA1WithRSA, x509.DSAWithSHA1, x509.ECDSAWithSHA1:
		return fmt.Errorf("hostcert: refusing a %s signature: it is not a signature algorithm this Colony will accept", alg)
	default:
		return fmt.Errorf("hostcert: refusing an unrecognised signature algorithm %s", alg)
	}
}

func hasLoopbackSAN(cert *x509.Certificate) bool {
	for _, ip := range cert.IPAddresses {
		if ip.IsLoopback() {
			return true
		}
	}
	return false
}

// checkKeyMatches refuses a certificate that does not belong to the
// private key beside it. A half-mismatched pair is a Comb that
// cannot start, discovered late; refusing it at install time is the
// whole reason this function exists.
func checkKeyMatches(cert *x509.Certificate, keyPEM []byte) error {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return errors.New("hostcert: private key is not a PEM block: the key file is unreadable or truncated")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Fall back to the SEC 1 form, which a hand-placed or
		// openssl-generated key may be in. A key this Colony wrote is
		// PKCS#8, but the file on a Comb may be either.
		if sec1, sec1err := x509.ParseECPrivateKey(block.Bytes); sec1err == nil {
			key = sec1
		} else {
			return fmt.Errorf("hostcert: parsing private key: %w", err)
		}
	}
	// Compare through the concrete types rather than an interface, so
	// a future key type is a compile error here rather than a silent
	// "not equal" below.
	switch certKey := cert.PublicKey.(type) {
	case *ecdsa.PublicKey:
		privKey, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return fmt.Errorf("hostcert: certificate carries an ECDSA public key but the private key is %T", key)
		}
		if !privKey.PublicKey.Equal(certKey) {
			return errors.New("hostcert: the private key does not match this certificate: the pair belongs to different keys")
		}
		return nil
	case *rsa.PublicKey:
		privKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return fmt.Errorf("hostcert: certificate carries an RSA public key but the private key is %T", key)
		}
		if !privKey.PublicKey.Equal(certKey) {
			return errors.New("hostcert: the private key does not match this certificate: the pair belongs to different keys")
		}
		return nil
	default:
		return fmt.Errorf("hostcert: refusing to check a certificate whose public key is %T", cert.PublicKey)
	}
}

func ipStrings(ips []net.IP) []string {
	if len(ips) == 0 {
		return nil
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
