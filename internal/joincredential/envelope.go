// Package joincredential seals a small join credential bundle to the
// certificate held by the joining Comb. The ciphertext may be replicated
// with a pending join request or returned by an unauthenticated status poll;
// only the holder of the matching private key can recover the contents.
package joincredential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
)

const envelopeVersion = 1

// Envelope contains only public metadata and authenticated ciphertext.
// The format is versioned so a future key-agreement change can fail closed
// instead of guessing how old bytes should be interpreted.
type Envelope struct {
	Version    int    `json:"version"`
	Algorithm  string `json:"algorithm"`
	Ephemeral  string `json:"ephemeral,omitempty"`
	WrappedKey string `json:"wrapped_key,omitempty"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// Bundle is the plaintext payload sealed inside an Envelope. Callers should
// keep it only in memory and must never log or persist it as a Raft value.
//
// The version is 1, 2 or 3. Version 1 carries the Colony's peer
// credentials and nothing else. Version 2 adds the certificate
// INTEGRATION - the Cloudflare token and the public settings that go
// with it - for a joining Comb that was prepared for. Version 3 adds
// the COLONY CA, for a joining Comb that must issue raft transport
// certificates and does not yet trust the Colony's CA.
//
// A joiner that only understands version 1 reads what it understands
// and ignores the rest, which is what the version is for. A joiner
// that understands only version 2 and meets a version 3 bundle
// REFUSES it rather than joining without the CA it needs - the
// installer is where that check lives, and it is the installer's
// judgement whether a Comb can join without one.
type Bundle struct {
	Version               int    `json:"version"`
	RequestID             string `json:"request_id"`
	NodeID                string `json:"node_id"`
	PeerAPIKey            string `json:"peer_api_key"`
	RestartGuardrailToken string `json:"restart_guardrail_token"`

	// Integration is the certificate integration provisioned to this
	// Comb (ADR-0174).
	//
	// It is EMPTY for a Comb that was not prepared for. Most Combs
	// never issue a certificate, and provisioning a zone-scoped
	// Cloudflare credential to every one of them would multiply the
	// blast radius of a single compromise for no benefit.
	Integration *Integration `json:"integration,omitempty"`

	// ColonyCA is the Colony's certificate authority, for a joining
	// Comb that needs one (ADR-0155).
	//
	// It is EMPTY for a joining Comb whose Colony already issues its
	// own leaves from a CA the Comb holds, and it is PRESENT when the
	// joining Comb does not yet trust the Colony CA - which is the
	// normal case for a Comb that has not joined before.
	//
	// The CA CERTIFICATE is public and travels in the sealed bundle
	// because the bundle is already an authenticated channel to this
	// exact Comb, sealed to the certificate it presented. The CA
	// PRIVATE KEY never travels, and never could: it is the Colony's
	// root of trust and it stays on the Comb that created it.
	//
	// A joiner does not trust this certificate because it was handed
	// one. It trusts it because its FINGERPRINT matches the one the
	// operator pinned in the join authorization - so the fingerprint
	// is the part that carries the authority, and the certificate is
	// only the thing the fingerprint is checked against.
	ColonyCA *ColonyCA `json:"colony_ca,omitempty"`
}

// ColonyCA is the Colony certificate authority, carried to a joining
// Comb so it can issue and verify raft transport certificates.
//
// It carries the certificate and the fingerprint the operator pinned.
// The two are carried TOGETHER and the joiner checks them against
// each other, because a certificate without the fingerprint would be
// an anchor nobody compared, and a fingerprint without the
// certificate would be a value nothing can be checked against.
type ColonyCA struct {
	// CertificatePEM is the CA certificate (public).
	CertificatePEM string `json:"certificate_pem"`

	// Fingerprint is the CA certificate's SHA-256, in
	// internal/colonyca's own format: the lowercase hex of the DER
	// bytes, with no prefix and no separators.
	//
	// It is NOT internal/hostcert's format ("SHA256:" followed by
	// uppercase colon-separated pairs). ADR-0155 records that the two
	// formats must never be compared directly, and this field is the
	// colonyca one - so a caller cannot compare them by accident.
	Fingerprint string `json:"fingerprint"`

	// PinnedFingerprint is the fingerprint the OPERATOR pinned in the
	// join authorization, which is the value the joiner checks
	// Fingerprint against.
	//
	// The two are carried together deliberately. Fingerprint is the
	// Colony's own computation of what it is sending; PinnedFingerprint
	// is the value a human compared by hand, out of band, and recorded
	// in a root-owned file.
	//
	// A joiner that only had Fingerprint would be checking the
	// Colony's certificate against the Colony's own arithmetic, which
	// proves nothing. A joiner that only had PinnedFingerprint would
	// have nothing to check it against. The joiner needs BOTH, and it
	// needs them to agree with each other AND with the certificate -
	// which is what makes the join a pinned one rather than a
	// supplied one.
	PinnedFingerprint string `json:"pinned_fingerprint,omitempty"`
}

// Integration is the certificate integration provisioned to a joining
// Comb.
//
// The token is a SHARED SECRET: a zone-scoped Cloudflare token can
// change what every host in that zone resolves to. It is carried only
// to a Comb that was prepared for, and it is written to a 0600
// root-owned file on arrival.
//
// The rest is public: the zone, the suffix, and the ACME account's
// settings. They are what the joining Comb's own configuration needs
// to reference, and they are not secret.
type Integration struct {
	// CloudflareToken is the zone-scoped API token. It is the secret
	// half.
	CloudflareToken string `json:"cloudflare_token,omitempty"`
	// CloudflareZoneID is the zone the token is scoped to.
	CloudflareZoneID string `json:"cloudflare_zone_id,omitempty"`
	// CloudflareZoneName is that zone's human name, for an operator
	// reading the joiner's configuration.
	CloudflareZoneName string `json:"cloudflare_zone_name,omitempty"`
	// AllowedSuffix is the naming suffix this integration may issue
	// for. It is what stops the integration issuing for a name the
	// operator did not mean to give this Colony.
	AllowedSuffix string `json:"allowed_suffix,omitempty"`
	// ACMEDirectory is where the joining Comb keeps its certificates.
	ACMEDirectory string `json:"acme_directory,omitempty"`
	// ACMEAccountKeyFile is where the joining Comb's own ACME account
	// key lives. It names a file; the key itself is generated on the
	// joiner and never travels.
	ACMEAccountKeyFile string `json:"acme_account_key_file,omitempty"`
	// ACMEDirectoryURL is the ACME server. Empty means Let's
	// Encrypt's production directory.
	ACMEDirectoryURL string `json:"acme_directory_url,omitempty"`
	// ACMEEmail is the contact address registered with the ACME
	// account.
	ACMEEmail string `json:"acme_email,omitempty"`
}

// Seal encrypts plaintext to the public key in certificatePEM. aad must bind
// the ciphertext to its use, normally the request ID and node ID. ECDSA P-256
// certificates use ephemeral ECDH; legacy RSA certificates use RSA-OAEP to
// wrap a random AES key. Neither mode writes plaintext key material to the
// envelope.
func Seal(certificatePEM, plaintext, aad []byte) ([]byte, error) {
	cert, err := parseCertificate(certificatePEM)
	if err != nil {
		return nil, err
	}
	var envelope Envelope
	switch pub := cert.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if pub.Curve.Params().Name != "P-256" {
			return nil, fmt.Errorf("joincredential: unsupported ECDSA curve %q", pub.Curve.Params().Name)
		}
		recipient, err := pub.ECDH()
		if err != nil {
			return nil, fmt.Errorf("joincredential: converting ECDSA public key: %w", err)
		}
		ephemeral, err := ecdh.P256().GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("joincredential: generating ephemeral key: %w", err)
		}
		shared, err := ephemeral.ECDH(recipient)
		if err != nil {
			return nil, fmt.Errorf("joincredential: deriving shared key: %w", err)
		}
		salt := make([]byte, sha256.Size)
		if _, err := rand.Read(salt); err != nil {
			return nil, fmt.Errorf("joincredential: generating salt: %w", err)
		}
		key := hkdfSHA256(shared, salt, aad, 32)
		sealed, nonce, err := sealAESGCM(key, plaintext, aad)
		if err != nil {
			return nil, err
		}
		eph := append(salt, ephemeral.PublicKey().Bytes()...)
		envelope = Envelope{Version: envelopeVersion, Algorithm: "p256-hkdf-sha256-aes256gcm", Ephemeral: base64.RawStdEncoding.EncodeToString(eph), Nonce: base64.RawStdEncoding.EncodeToString(nonce), Ciphertext: base64.RawStdEncoding.EncodeToString(sealed)}
	case *rsa.PublicKey:
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("joincredential: generating content key: %w", err)
		}
		wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, key, aad)
		if err != nil {
			return nil, fmt.Errorf("joincredential: wrapping content key: %w", err)
		}
		sealed, nonce, err := sealAESGCM(key, plaintext, aad)
		if err != nil {
			return nil, err
		}
		envelope = Envelope{Version: envelopeVersion, Algorithm: "rsa-oaep-sha256-aes256gcm", WrappedKey: base64.RawStdEncoding.EncodeToString(wrapped), Nonce: base64.RawStdEncoding.EncodeToString(nonce), Ciphertext: base64.RawStdEncoding.EncodeToString(sealed)}
	default:
		return nil, fmt.Errorf("joincredential: unsupported certificate public key type %T", cert.PublicKey)
	}
	wire, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("joincredential: encoding envelope: %w", err)
	}
	return wire, nil
}

// DerivePeerAPIKey returns the stable, per-request peer API key for an
// approved join. The Colony's existing restart-guardrail token is the
// HMAC key; the raw API key is never placed in Raft state, while its hash
// is persisted there as an ordinary Operator key. Repeated status polls
// therefore recover the same key without persisting its plaintext or
// ciphertext in the pending request.
func DerivePeerAPIKey(colonyToken, requestID, nodeID string) (string, error) {
	if colonyToken == "" || requestID == "" || nodeID == "" {
		return "", errors.New("joincredential: token, request ID and node ID are required")
	}
	mac := hmac.New(sha256.New, []byte(colonyToken))
	_, _ = mac.Write([]byte("apiary-join-peer-key/v1\x00"))
	_, _ = mac.Write([]byte(requestID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(nodeID))
	return "apk_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// DeriveColonyPeerAPIKey returns the shared Operator key used to backfill
// existing members that joined before peer credentials were installed
// automatically. It is deterministic from the Colony's shared restart token,
// and domain-separated from request-bound join keys.
func DeriveColonyPeerAPIKey(colonyToken string) (string, error) {
	if colonyToken == "" {
		return "", errors.New("joincredential: Colony restart token is required")
	}
	mac := hmac.New(sha256.New, []byte(colonyToken))
	_, _ = mac.Write([]byte("apiary-colony-peer-key/v1\x00"))
	return "apk_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// AssociatedData binds an envelope to one approved request and its exact
// advertised serving certificate. A ciphertext copied to another request,
// Comb identity or certificate cannot be opened successfully.
func AssociatedData(requestID, nodeID, fingerprint string) []byte {
	return []byte("apiary-join-credentials/v1\x00" + requestID + "\x00" + nodeID + "\x00" + fingerprint)
}

// MemberTokenAssociatedData binds an existing-member token envelope to its
// Raft node identity, pinned serving certificate, and one-time challenge.
func MemberTokenAssociatedData(nodeID, fingerprint, challenge string) []byte {
	return []byte("apiary-member-restart-token/v1\x00" + nodeID + "\x00" + fingerprint + "\x00" + challenge)
}

// Open decrypts an envelope using the PEM certificate and private key held
// by the receiving Comb. The private key must correspond to the certificate;
// this is checked before attempting to decrypt.
func Open(envelopeJSON, certificatePEM, privateKeyPEM, aad []byte) ([]byte, error) {
	var envelope Envelope
	if err := json.Unmarshal(envelopeJSON, &envelope); err != nil {
		return nil, fmt.Errorf("joincredential: decoding envelope: %w", err)
	}
	if envelope.Version != envelopeVersion {
		return nil, fmt.Errorf("joincredential: unsupported envelope version %d", envelope.Version)
	}
	cert, err := parseCertificate(certificatePEM)
	if err != nil {
		return nil, err
	}
	priv, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	if err := checkKeyPair(cert, priv); err != nil {
		return nil, err
	}
	var key []byte
	switch envelope.Algorithm {
	case "p256-hkdf-sha256-aes256gcm":
		private, ok := priv.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("joincredential: envelope requires an ECDSA private key")
		}
		packed, err := base64.RawStdEncoding.DecodeString(envelope.Ephemeral)
		if err != nil || len(packed) != sha256.Size+65 {
			return nil, errors.New("joincredential: malformed ECDH envelope key")
		}
		peer, err := ecdh.P256().NewPublicKey(packed[sha256.Size:])
		if err != nil {
			return nil, fmt.Errorf("joincredential: parsing ephemeral key: %w", err)
		}
		self, err := private.ECDH()
		if err != nil {
			return nil, fmt.Errorf("joincredential: converting ECDSA private key: %w", err)
		}
		shared, err := self.ECDH(peer)
		if err != nil {
			return nil, fmt.Errorf("joincredential: deriving shared key: %w", err)
		}
		key = hkdfSHA256(shared, packed[:sha256.Size], aad, 32)
	case "rsa-oaep-sha256-aes256gcm":
		private, ok := priv.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("joincredential: envelope requires an RSA private key")
		}
		wrapped, err := base64.RawStdEncoding.DecodeString(envelope.WrappedKey)
		if err != nil {
			return nil, fmt.Errorf("joincredential: decoding wrapped key: %w", err)
		}
		key, err = rsa.DecryptOAEP(sha256.New(), rand.Reader, private, wrapped, aad)
		if err != nil {
			return nil, errors.New("joincredential: unable to unwrap content key")
		}
	default:
		return nil, fmt.Errorf("joincredential: unsupported envelope algorithm %q", envelope.Algorithm)
	}
	nonce, err := base64.RawStdEncoding.DecodeString(envelope.Nonce)
	if err != nil {
		return nil, errors.New("joincredential: malformed nonce")
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		return nil, errors.New("joincredential: malformed ciphertext")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("joincredential: creating AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("joincredential: creating GCM: %w", err)
	}
	plain, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, errors.New("joincredential: authentication failed")
	}
	return plain, nil
}

func parseCertificate(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("joincredential: PEM certificate is missing")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("joincredential: parsing certificate: %w", err)
	}
	return cert, nil
}

func parsePrivateKey(data []byte) (any, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("joincredential: PEM private key is missing")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, errors.New("joincredential: parsing private key")
}

func checkKeyPair(cert *x509.Certificate, private any) error {
	var public any
	switch key := private.(type) {
	case *ecdsa.PrivateKey:
		public = &key.PublicKey
	case *rsa.PrivateKey:
		public = &key.PublicKey
	default:
		return fmt.Errorf("joincredential: unsupported private key type %T", private)
	}
	want, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return fmt.Errorf("joincredential: encoding certificate key: %w", err)
	}
	have, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return fmt.Errorf("joincredential: encoding private key's public half: %w", err)
	}
	if !hmac.Equal(want, have) {
		return errors.New("joincredential: certificate and private key do not match")
	}
	return nil
}

func sealAESGCM(key, plaintext, aad []byte) ([]byte, []byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("joincredential: creating AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("joincredential: creating GCM: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("joincredential: generating nonce: %w", err)
	}
	return aead.Seal(nil, nonce, plaintext, aad), nonce, nil
}

// hkdfSHA256 implements RFC 5869's extract-and-expand steps for this
// package's fixed SHA-256 use, avoiding a dependency on a second KDF API.
func hkdfSHA256(secret, salt, info []byte, length int) []byte {
	extract := hmac.New(sha256.New, salt)
	_, _ = extract.Write(secret)
	prk := extract.Sum(nil)
	out := make([]byte, 0, length)
	var previous []byte
	for counter := byte(1); len(out) < length; counter++ {
		expand := hmac.New(sha256.New, prk)
		_, _ = expand.Write(previous)
		_, _ = expand.Write(info)
		_, _ = expand.Write([]byte{counter})
		previous = expand.Sum(nil)
		out = append(out, previous...)
	}
	return out[:length]
}
