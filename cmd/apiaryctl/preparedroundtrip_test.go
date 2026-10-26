package main

// ADR-0173's round trip: a prepared pair is sealed to the joiner, the
// joiner opens it, and the pair lands in the joiner's own serving
// paths.
//
// The test drives the REAL seal and the REAL open, so the associated
// data discipline is exercised rather than assumed: an envelope sealed
// for one reservation must not open under another.

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/certreservation"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
)

// makeServingCert generates a serving certificate and its key.
func makeServingCert(t *testing.T, name string) (certPEM, keyPEM, fingerprint string) {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	cert, key, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName:  name + ".lab2.apiary.work",
		DNSNames:    []string{name, name + ".lab2.apiary.work"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		Now:         now,
	})
	if err != nil {
		t.Fatalf("generating the certificate: %v", err)
	}
	info, err := hostcert.Evaluate(cert, hostcert.EvaluateOptions{
		Now:                now,
		RequireDNSSAN:      true,
		RequireLoopbackSAN: true,
	})
	if err != nil {
		t.Fatalf("the certificate does not pass the gate: %v", err)
	}
	return string(cert), string(key), info.Fingerprint
}

// The round trip: the preparing Comb seals the prepared pair to the
// joiner, and the joiner installs it into its own serving paths.
func TestPreparedCertificateRoundTrip(t *testing.T) {
	// The joiner's CURRENT serving certificate - the one the delivery
	// is sealed to.
	joinerCert, joinerKey, _ := makeServingCert(t, "brood")
	// The PREPARED pair: for the joiner's name, but a different key.
	preparedCert, preparedKey, preparedFingerprint := makeServingCert(t, "brood-prepared")

	reservation := &certreservation.Reservation{
		ID:             "res-1",
		RequestID:      "jreq-1",
		NodeID:         "brood",
		FQDN:           "brood-prepared.lab2.apiary.work",
		Fingerprint:    preparedFingerprint,
		CertificatePEM: preparedCert,
	}
	envelope, err := certreservation.SealDelivery(reservation, "jreq-1", "brood", joinerCert, preparedKey, time.Now())
	if err != nil {
		t.Fatalf("SealDelivery: %v", err)
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls", "cert.pem")
	keyPath := filepath.Join(dir, "tls", "key.pem")
	cfgPath := filepath.Join(dir, "managerd.json")

	// The joiner's CURRENT pair is already at the paths it serves
	// from - that is what the delivery is sealed to, and what the
	// installer reads to open it.
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, []byte(joinerCert), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(joinerKey), 0o600); err != nil {
		t.Fatal(err)
	}

	// The joiner's own configuration names the paths it serves from.
	cfg := nodeconfig.Config{
		NodeID:  "brood",
		TLSCert: certPath,
		TLSKey:  keyPath,
	}
	manager := &nodeconfig.Manager{Path: cfgPath}
	if err := manager.Save(cfg); err != nil {
		t.Fatalf("saving the configuration: %v", err)
	}

	servingCert := &localCertificate{label: "managerd", path: certPath, pem: joinerCert}
	result, err := installPreparedCertificateWith(envelope, servingCert, "res-1", "jreq-1", preparedFingerprint, "brood-prepared.lab2.apiary.work", cfgPath, func() error { return nil })
	if err != nil {
		t.Fatalf("installPreparedCertificate: %v", err)
	}
	if result.FQDN != "brood-prepared.lab2.apiary.work" {
		t.Errorf("the installed certificate is for %q, want the prepared name", result.FQDN)
	}
	if result.Fingerprint != preparedFingerprint {
		t.Errorf("the reported fingerprint is %q, want %q", result.Fingerprint, preparedFingerprint)
	}

	// The pair landed in the CONFIGURED paths.
	installedCert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("reading the installed certificate: %v", err)
	}
	if string(installedCert) != preparedCert {
		t.Error("the installed certificate is not the prepared one")
	}
	installedKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("reading the installed key: %v", err)
	}
	if string(installedKey) != preparedKey {
		t.Error("the installed key is not the prepared one")
	}
}

// An envelope sealed for one reservation must not open under another.
//
// This is the property that makes the delivery non-replayable across
// reservations, and it is enforced by the associated data rather than
// by a check each caller has to remember to write.
func TestPreparedCertificateEnvelopeIsBoundToItsReservation(t *testing.T) {
	joinerCert, joinerKey, _ := makeServingCert(t, "brood")
	preparedCert, preparedKey, preparedFingerprint := makeServingCert(t, "brood-prepared")

	reservation := &certreservation.Reservation{
		ID:             "res-1",
		RequestID:      "jreq-1",
		NodeID:         "brood",
		FQDN:           "brood-prepared.lab2.apiary.work",
		Fingerprint:    preparedFingerprint,
		CertificatePEM: preparedCert,
	}
	envelope, err := certreservation.SealDelivery(reservation, "jreq-1", "brood", joinerCert, preparedKey, time.Now())
	if err != nil {
		t.Fatalf("SealDelivery: %v", err)
	}

	// A DIFFERENT reservation, with the same joiner.
	other := &certreservation.Reservation{
		ID:             "res-2",
		RequestID:      "jreq-1",
		NodeID:         "brood",
		FQDN:           "brood-prepared.lab2.apiary.work",
		Fingerprint:    preparedFingerprint,
		CertificatePEM: preparedCert,
	}
	if _, err := certreservation.OpenDelivery(string(envelope), joinerCert, joinerKey, other); err == nil {
		t.Fatal("a delivery sealed for one reservation opened under another")
	}
}
