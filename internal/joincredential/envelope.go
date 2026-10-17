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
type Bundle struct {
	Version               int    `json:"version"`
	RequestID             string `json:"request_id"`
	NodeID                string `json:"node_id"`
	PeerAPIKey            string `json:"peer_api_key"`
	RestartGuardrailToken string `json:"restart_guardrail_token"`
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
