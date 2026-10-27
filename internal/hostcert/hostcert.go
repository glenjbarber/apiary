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

	// CAAnchor is the certificate of the CA that ISSUED this
	// certificate, when one was supplied, and the empty string when
	// the certificate is self-signed and anchors itself.
	//
	// It is the trust anchor rather than the leaf, because a peer
	// presents a leaf and the thing that verifies it is the CA that
	// signed it. A Colony whose members hold publicly trusted
	// certificates has exactly one CA to record, and recording the
	// leaf instead would make the derived peer-ca.pem a pin list
	// rather than a trust set.
	//
	// It is PEM, normalized, so it can be handed to crypto/x509 as
	// a root directly.
	CAAnchor string

	// CAFingerprint is the digest of CAAnchor, in this package's
	// fingerprint format. It is empty when CAAnchor is empty.
	CAFingerprint string
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

	// AllowCAIssued accepts a certificate that was issued by a
	// certificate authority rather than signed by itself.
	//
	// A Comb's serving certificate used to be always self-signed, so
	// the issuer-is-the-subject check was the whole story. It stopped
	// being true when the Cloudflare/ACME work (ADR-0133, ADR-0169,
	// ADR-0173) moved a Colony's Combs onto publicly trusted
	// certificates - a Let's Encrypt leaf is issued by Let's Encrypt,
	// and refusing it meant the peer trust store could never be
	// populated on such a Colony.
	//
	// The check is still fail-closed, it just changes what it asks. A
	// self-signed certificate must still verify its own signature; a
	// CA-issued one must name an issuer that can anchor trust.
	//
	// Leave it false where a caller genuinely needs a Comb's own
	// self-signed certificate - the local serving certificate, for
	// instance, where a CA-issued one would be a different identity
	// than the one the operator configured.
	AllowCAIssued bool

	// CAAnchorPEM is the certificate of the CA that issued the
	// certificate being evaluated.
	//
	// When it is set, the certificate being evaluated is checked
	// AGAINST it - the CA must actually have signed it, and the CA
	// must be a CA - and the CA becomes the trust anchor reported in
	// Info.CAAnchor. That is the model ADR-0185 decides for a Colony
	// whose members hold publicly trusted certificates: the store
	// records the CA, and the derived peer-ca.pem holds the CA, so
	// every member's leaf verifies against it.
	//
	// When it is empty, a self-signed certificate anchors itself and
	// Info.CAAnchor is empty.
	CAAnchorPEM []byte
}

// checkIssuerIsCA refuses a CA-issued certificate that carries no
// BasicConstraints extension at all.
//
// The ISSUER's CA status is not knowable from the leaf alone - Go
// exposes it on the issuer certificate, which is not here. So what is
// checked is the leaf's own BasicConstraints extension, which is what
// says "this certificate was issued under a CA's rules" rather than
// "this certificate is a root". A leaf with no BasicConstraints at all
// is refused: an issuer that says nothing about the hierarchy it
// belongs to cannot anchor a trust decision, and refusing it is the
// fail-closed choice.
func checkIssuerIsCA(cert *x509.Certificate) error {
	if !cert.BasicConstraintsValid {
		return fmt.Errorf("hostcert: certificate is issued by %q but carries no BasicConstraints extension, so it cannot be a trust anchor", cert.Issuer.String())
	}
	return nil
}

// evaluateCAAnchor checks the supplied CA certificate against the
// certificate being evaluated, and returns the CA's normalized PEM
// and fingerprint.
//
// It is the check that makes a CA anchor HONEST. A CA that did not
// sign the certificate would anchor nothing: the derived peer-ca.pem
// would hold a certificate that verifies no peer's leaf, and the
// failure would surface as an opaque TLS handshake error against a
// host nobody could explain.
//
// Three things are checked, and each is a different operator problem:
//
//   - the CA certificate parses and is exactly one certificate;
//   - the CA is a CA (BasicConstraints CA:true, and permitted to
//     sign), because a leaf's issuer that is not a CA cannot anchor
//     anything; and
//   - the CA actually SIGNED this certificate, verified with
//     CheckSignatureFrom.
//
// The CA's OWN chain is not walked here, and deliberately so. A
// publicly trusted CA is a leaf-holder's chain up to a root in the
// SYSTEM trust store, and the system store is not replicated state -
// so requiring it to be verifiable here would make the pin depend on
// a machine's certificate store rather than on the Colony's record.
// The CA certificate in the store IS the root of trust by
// construction, exactly as a pinned self-signed certificate is; what
// is verified here is that it is the CA that signed this certificate,
// which is the fact the store is making a claim about.
func evaluateCAAnchor(cert *x509.Certificate, caPEM []byte, now time.Time) (string, string, error) {
	der, err := singleCertificateDER(caPEM)
	if err != nil {
		return "", "", fmt.Errorf("hostcert: the issuing CA certificate: %w", err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		return "", "", fmt.Errorf("hostcert: parsing the issuing CA certificate: %w", err)
	}
	// The CA must be a CA, and must be permitted to sign. Go's
	// CheckSignatureFrom refuses a parent that is not a CA with a
	// bare ConstraintViolationError, so this is checked here where
	// the refusal can NAME what was wrong.
	if !ca.BasicConstraintsValid || !ca.IsCA {
		return "", "", fmt.Errorf("hostcert: the issuing CA certificate %q is not a certificate authority (it has no CA:true BasicConstraints assertion), so it cannot anchor a trust decision", ca.Subject.String())
	}
	if ca.KeyUsage != 0 && ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		return "", "", fmt.Errorf("hostcert: the issuing CA certificate %q is not permitted to sign certificates, so it cannot anchor a trust decision", ca.Subject.String())
	}
	// The CA must have actually signed THIS certificate. Without
	// this, a CA anchor is a claim rather than a fact.
	if err := cert.CheckSignatureFrom(ca); err != nil {
		return "", "", fmt.Errorf("hostcert: the issuing CA certificate %q did not sign this certificate: %w", ca.Subject.String(), err)
	}
	// The CA's own validity window is checked here, and it is a
	// separate check from the leaf's: a CA that has expired leaves
	// every leaf it signed untrustworthy, and the operator needs to
	// be told WHICH one expired.
	//
	// The caller's clock, not the wall clock: this function runs on
	// every voter and again on every replay, so consulting time.Now()
	// here would let a replay years later reach a different decision
	// than the original and leave replicas disagreeing about the
	// store's contents.
	if now.Before(ca.NotBefore) {
		return "", "", fmt.Errorf("hostcert: the issuing CA certificate %q is not yet valid (not before %s, now %s)", ca.Subject.String(), ca.NotBefore.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	if now.After(ca.NotAfter) {
		return "", "", fmt.Errorf("hostcert: the issuing CA certificate %q expired (not after %s, now %s)", ca.Subject.String(), ca.NotAfter.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})), Fingerprint(ca.Raw), nil
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
	//
	// A CA-issued certificate takes a different branch, and only when
	// the caller asked for one. What is checked there is that the
	// issuer is a real CA: a certificate whose issuer is not a CA
	// cannot anchor anything, and accepting it would put a trust
	// anchor in peer-ca.pem that verifies nothing.
	selfSigned := bytes.Equal(cert.RawIssuer, cert.RawSubject)
	// caAnchor and caFingerprint are the trust anchor this
	// certificate is recorded under, and they are empty for a
	// self-signed certificate, which anchors itself.
	caAnchor, caFingerprint := "", ""
	switch {
	case selfSigned:
		// CheckSignature, not CheckSignatureFrom: the latter is a CA
		// check, and a Comb's certificate is a self-signed LEAF, which
		// is exactly what CheckSignatureFrom refuses ("parent
		// certificate cannot sign this kind of certificate"). What is
		// being verified here is that the bytes somebody signed are
		// the bytes in the certificate, by the key named in the
		// certificate.
		if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
			return Info{}, fmt.Errorf("hostcert: self-signature does not verify: %w", err)
		}
	case opts.AllowCAIssued:
		if err := checkIssuerIsCA(cert); err != nil {
			return Info{}, err
		}
		// A CA anchor is what makes a CA-issued certificate
		// TRUSTWORTHY rather than merely tolerated. The CA supplied
		// is checked against this certificate here - it must be a CA,
		// it must have signed this certificate, and it must be
		// inside its own validity window - and it is the CA, not the
		// leaf, that becomes the recorded trust anchor.
		//
		// A CA-issued certificate with NO CA supplied is refused
		// rather than accepted on the leaf's own say-so. Accepting
		// it would record a leaf as a trust anchor, which is the pin
		// list ADR-0185 replaces, and it would do so without
		// anything ever having checked that the certificate chains
		// to anything at all.
		if len(opts.CAAnchorPEM) == 0 {
			return Info{}, fmt.Errorf("hostcert: certificate is issued by %q but no issuing CA was supplied, so there is no trust anchor to record it under. A CA-issued certificate is recorded under the CA that signed it, not under itself", cert.Issuer.String())
		}
		caAnchor, caFingerprint, err = evaluateCAAnchor(cert, opts.CAAnchorPEM, now)
		if err != nil {
			return Info{}, err
		}
	default:
		return Info{}, fmt.Errorf("hostcert: certificate is issued by %q but its subject is %q, so it is not a Comb's own self-signed certificate", cert.Issuer.String(), cert.Subject.String())
	}

	keyType, keyBits, err := describeKey(cert.PublicKey)
	if err != nil {
		return Info{}, err
	}
	if err := checkSignatureAlgorithm(cert.SignatureAlgorithm); err != nil {
		return Info{}, err
	}

	// The loopback SAN is required of a Comb's OWN self-signed
	// serving certificate, because every daemon that dials managerd
	// defaults to loopback and ADR-0139's first live failure was
	// exactly a certificate missing it.
	//
	// It is NOT required of a CA-issued certificate. A publicly
	// trusted certificate is dialled BY NAME - the DNS SAN is what
	// carries the identity, and a Let's Encrypt leaf carries no
	// IP:127.0.0.1 address at all. Requiring one of it would refuse
	// every such certificate, which is what made the peer trust
	// store impossible to populate on a Colony that had moved onto
	// public certificates.
	if opts.RequireLoopbackSAN && selfSigned && !hasLoopbackSAN(cert) {
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
		Subject:       cert.Subject.String(),
		Issuer:        cert.Issuer.String(),
		NotBefore:     cert.NotBefore,
		NotAfter:      cert.NotAfter,
		DNSNames:      append([]string(nil), cert.DNSNames...),
		IPAddresses:   ipStrings(cert.IPAddresses),
		Fingerprint:   Fingerprint(cert.Raw),
		KeyType:       keyType,
		KeyBits:       keyBits,
		Signature:     cert.SignatureAlgorithm.String(),
		CAAnchor:      caAnchor,
		CAFingerprint: caFingerprint,
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

// IsSelfSigned reports whether a certificate signed itself - which is
// what a Comb's own certificate did before the Cloudflare/ACME work
// moved a Colony onto publicly trusted certificates.
//
// A self-signed certificate anchors ITSELF: it is its own trust
// anchor, and needs no issuing CA. That is the distinction ADR-0185's
// model rests on, and a caller that has to tell the two apart needs
// it stated rather than inferred.
func IsSelfSigned(certPEM []byte) (bool, error) {
	der, err := singleCertificateDER(certPEM)
	if err != nil {
		return false, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return false, fmt.Errorf("hostcert: parsing certificate: %w", err)
	}
	return bytes.Equal(cert.RawIssuer, cert.RawSubject), nil
}

// IsCA reports whether a PEM file holds a certificate that is itself a
// certificate authority, and is therefore a candidate trust anchor
// rather than a Comb's own serving certificate.
//
// It is what lets a reader of a bundle of certificates tell a leaf from
// the CA that signed it, which is the distinction ADR-0185's model
// rests on: the store records the CA, and the derived peer-ca.pem
// holds the CA.
//
// A file that holds more than one certificate is refused rather than
// judged on its first block, because a bundle's shape is an operator's
// mistake and guessing which block was meant would be a worse one.
func IsCA(certPEM []byte) (bool, error) {
	der, err := singleCertificateDER(certPEM)
	if err != nil {
		return false, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return false, fmt.Errorf("hostcert: parsing certificate: %w", err)
	}
	return cert.BasicConstraintsValid && cert.IsCA, nil
}

// SigningCA returns the candidate CA certificate that signed the
// certificate in certPEM, as normalized PEM.
//
// It is the search half of ADR-0185: a bundle that holds a Colony's
// members' leaves and the CA that signed them, and the trust anchor
// for each leaf is the CA in that bundle that actually signed it.
//
// The candidates are tried in the order given, and the FIRST one that
// signed the certificate wins. The check is a real signature check -
// CheckSignatureFrom - and not a name comparison, because two CAs can
// share a name and a name is not an identity.
//
// When no candidate signed the certificate, the refusal says so and
// says how many candidates were tried, because the operator's next
// step is to collect the CA rather than to guess at a flag.
func SigningCA(certPEM []byte, candidates [][]byte) ([]byte, error) {
	der, err := singleCertificateDER(certPEM)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("hostcert: parsing certificate: %w", err)
	}
	if bytes.Equal(cert.RawIssuer, cert.RawSubject) {
		return nil, fmt.Errorf("hostcert: the certificate is self-signed, so it anchors itself and needs no issuing CA")
	}
	for _, candidate := range candidates {
		caDER, err := singleCertificateDER(candidate)
		if err != nil {
			return nil, err
		}
		ca, err := x509.ParseCertificate(caDER)
		if err != nil {
			return nil, fmt.Errorf("hostcert: parsing the issuing CA certificate: %w", err)
		}
		if err := cert.CheckSignatureFrom(ca); err != nil {
			continue
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}), nil
	}
	return nil, fmt.Errorf("hostcert: none of the %d candidate CA certificates signed this certificate, so there is no trust anchor to record it under. The certificate is issued by %q", len(candidates), cert.Issuer.String())
}
