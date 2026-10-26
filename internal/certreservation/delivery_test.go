package certreservation

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/hostcert"
)

// makeCert generates a self-signed serving certificate for name.
func makeCert(t *testing.T, name string) (certPEM, keyPEM string) {
	t.Helper()
	opts := hostcert.GenerateOptions{CommonName: name}
	if name != "" {
		opts.DNSNames = []string{name}
	}
	cert, key, err := hostcert.Generate(opts)
	if err != nil {
		t.Fatalf("hostcert.Generate: %v", err)
	}
	return string(cert), string(key)
}

func TestSealAndOpenDelivery_RoundTrips(t *testing.T) {
	recipientCert, recipientKeyPEM := makeCert(t, "host2.example.com")
	certPEM, keyPEM := makeCert(t, "host2.example.com")
	fingerprint, err := CertificateFingerprint(certPEM)
	if err != nil {
		t.Fatalf("CertificateFingerprint: %v", err)
	}
	res, err := NewReservation("host2.example.com", certPEM, fingerprint, time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	now := time.Now()
	if err := res.Bind("host2", "req-1", now); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	envelope, err := SealDelivery(res, "req-1", "host2", recipientCert, keyPEM, now)
	if err != nil {
		t.Fatalf("SealDelivery: %v", err)
	}
	bundle, err := OpenDelivery(string(envelope), recipientCert, recipientKeyPEM, res)
	if err != nil {
		t.Fatalf("OpenDelivery: %v", err)
	}
	if bundle.CertificatePEM != certPEM {
		t.Error("the delivered certificate is not the prepared one")
	}
	if bundle.PrivateKeyPEM != keyPEM {
		t.Error("the delivered private key is not the prepared one")
	}
	if bundle.NodeID != "host2" || bundle.RequestID != "req-1" {
		t.Errorf("the bundle names node %q request %q, want host2/req-1", bundle.NodeID, bundle.RequestID)
	}
}

func TestOpenDelivery_RefusesTheWrongRecipient(t *testing.T) {
	recipientCert, _ := makeCert(t, "host2.example.com")
	otherCert, otherKey := makeCert(t, "host3.example.com")
	certPEM, keyPEM := makeCert(t, "host2.example.com")
	fingerprint, err := CertificateFingerprint(certPEM)
	if err != nil {
		t.Fatalf("CertificateFingerprint: %v", err)
	}
	res, err := NewReservation("host2.example.com", certPEM, fingerprint, time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	now := time.Now()
	if err := res.Bind("host2", "req-1", now); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	envelope, err := SealDelivery(res, "req-1", "host2", recipientCert, keyPEM, now)
	if err != nil {
		t.Fatalf("SealDelivery: %v", err)
	}
	// A different Comb's key cannot open it: the envelope is sealed
	// to the recipient's certificate.
	if _, err := OpenDelivery(string(envelope), otherCert, otherKey, res); err == nil {
		t.Error("OpenDelivery succeeded with the wrong recipient's key")
	}
}

func TestOpenDelivery_RefusesADeliveryForAnotherReservation(t *testing.T) {
	recipientCert, recipientKeyPEM := makeCert(t, "host2.example.com")
	certPEM, keyPEM := makeCert(t, "host2.example.com")
	fingerprint, err := CertificateFingerprint(certPEM)
	if err != nil {
		t.Fatalf("CertificateFingerprint: %v", err)
	}
	res, err := NewReservation("host2.example.com", certPEM, fingerprint, time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	now := time.Now()
	if err := res.Bind("host2", "req-1", now); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	envelope, err := SealDelivery(res, "req-1", "host2", recipientCert, keyPEM, now)
	if err != nil {
		t.Fatalf("SealDelivery: %v", err)
	}

	// A second reservation with the same certificate - a different
	// preparation - must not accept the first one's delivery. The
	// associated data binds the envelope to the reservation ID, so
	// this fails to decrypt rather than decrypting into the wrong
	// material.
	other, err := NewReservation("host2.example.com", certPEM, fingerprint, time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	if err := other.Bind("host2", "req-1", now); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if _, err := OpenDelivery(string(envelope), recipientCert, recipientKeyPEM, other); err == nil {
		t.Error("OpenDelivery accepted a delivery sealed for a different reservation")
	}
}

func TestOpenDelivery_RefusesACertificateForTheWrongName(t *testing.T) {
	recipientCert, recipientKeyPEM := makeCert(t, "host2.example.com")
	// The certificate is for a DIFFERENT name than the reservation.
	certPEM, keyPEM := makeCert(t, "someone-else.example.com")
	fingerprint, err := CertificateFingerprint(certPEM)
	if err != nil {
		t.Fatalf("CertificateFingerprint: %v", err)
	}
	res, err := NewReservation("host2.example.com", certPEM, fingerprint, time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	now := time.Now()
	if err := res.Bind("host2", "req-1", now); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	envelope, err := SealDelivery(res, "req-1", "host2", recipientCert, keyPEM, now)
	if err != nil {
		t.Fatalf("SealDelivery: %v", err)
	}
	if _, err := OpenDelivery(string(envelope), recipientCert, recipientKeyPEM, res); !errors.Is(err, ErrNameMismatch) {
		t.Errorf("OpenDelivery for the wrong name = %v, want ErrNameMismatch", err)
	}
}

func TestOpenDelivery_RefusesAMismatchedKeyPair(t *testing.T) {
	recipientCert, recipientKeyPEM := makeCert(t, "host2.example.com")
	certPEM, _ := makeCert(t, "host2.example.com")
	otherCertPEM, otherKeyPEM := makeCert(t, "host2.example.com")
	fingerprint, err := CertificateFingerprint(certPEM)
	if err != nil {
		t.Fatalf("CertificateFingerprint: %v", err)
	}
	res, err := NewReservation("host2.example.com", certPEM, fingerprint, time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	now := time.Now()
	if err := res.Bind("host2", "req-1", now); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	envelope, err := SealDelivery(res, "req-1", "host2", recipientCert, otherKeyPEM, now)
	if err != nil {
		t.Fatalf("SealDelivery: %v", err)
	}
	if _, err := OpenDelivery(string(envelope), recipientCert, recipientKeyPEM, res); !errors.Is(err, ErrKeyMismatch) {
		t.Errorf("OpenDelivery with a mismatched key pair = %v, want ErrKeyMismatch", err)
	}
	_ = otherCertPEM
}

func TestCheckKeyMatchesCertificate_AcceptsAMatchingPair(t *testing.T) {
	certPEM, keyPEM := makeCert(t, "host2.example.com")
	if err := CheckKeyMatchesCertificate(certPEM, keyPEM); err != nil {
		t.Errorf("CheckKeyMatchesCertificate on a matching pair = %v, want nil", err)
	}
}

func TestCertificateFingerprint_MatchesHostcert(t *testing.T) {
	certPEM, _ := makeCert(t, "host2.example.com")
	got, err := CertificateFingerprint(certPEM)
	if err != nil {
		t.Fatalf("CertificateFingerprint: %v", err)
	}
	block, _ := pem.Decode([]byte(certPEM))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	// The reservation's fingerprint and the peer trust store's
	// fingerprint must be the same string, or a prepared certificate
	// could never be pinned.
	if want := hostcert.Fingerprint(cert.Raw); got != want {
		t.Errorf("CertificateFingerprint = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "SHA256:") {
		t.Errorf("CertificateFingerprint = %q, want a SHA256: prefix", got)
	}
}
