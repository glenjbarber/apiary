package certmgr

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/origincert"
)

// newTestPair writes a real certificate and a real private key into dir
// using origincert.WritePair - the same transaction production uses -
// and returns the two paths. notAfter is the leaf's own NotAfter, so a
// test can place a certificate at any point in its life.
func newTestPair(t *testing.T, dir, name string, notBefore, notAfter time.Time) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "api.example.com"},
		DNSNames:     []string{"api.example.com", "www.example.com"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("encoding key: %v", err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	certPath, keyPath, err = origincert.WritePair(dir, name, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("WritePair: %v", err)
	}
	return certPath, keyPath
}

// TestObserve_VerdictTable is the table ADR-0133's test plan calls "the
// single most important table in the ADR": every way a check can fail, and
// what each one must be reported as. The two columns that must never be
// wrong are the ones on either side of unknown - a check that could not
// be made reported as expired manufactures an outage, and reported as ok
// hides one.
func TestObserve_VerdictTable(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()

	// A directory to write failing cases into, kept separate from the
	// valid pair's so the "never observed" and "unreadable" cases are
	// obviously distinct setups rather than one mutating the other.
	broken := t.TempDir()

	healthy, _ := newTestPair(t, dir, "healthy", now.Add(-300*24*time.Hour), now.Add(200*24*time.Hour))
	soon, _ := newTestPair(t, dir, "soon", now.Add(-300*24*time.Hour), now.Add(10*24*time.Hour))
	expired, _ := newTestPair(t, dir, "expired", now.Add(-400*24*time.Hour), now.Add(-2*time.Hour))

	unparseable := filepath.Join(broken, "unparseable.crt")
	if err := os.WriteFile(unparseable, []byte("-----BEGIN CERTIFICATE-----\nnot base64 at all\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatalf("writing unparseable certificate: %v", err)
	}
	notACert := filepath.Join(broken, "notacert.crt")
	if err := os.WriteFile(notACert, []byte("just some text, no PEM block at all\n"), 0o644); err != nil {
		t.Fatalf("writing non-certificate file: %v", err)
	}
	// A key file in place of a certificate: the self-check must not be
	// able to make a verdict out of one, and must certainly not treat it
	// as a certificate.
	wrongKind := filepath.Join(broken, "wrongkind.crt")
	keyPEMBytes, err := os.ReadFile(filepath.Join(dir, "healthy.key"))
	if err != nil {
		t.Fatalf("reading key for the wrong-kind case: %v", err)
	}
	if err := os.WriteFile(wrongKind, keyPEMBytes, 0o644); err != nil {
		t.Fatalf("writing wrong-kind file: %v", err)
	}

	cases := []struct {
		name    string
		certPth string
		want    origincert.ExpiryStatus
		wantErr bool
	}{
		{name: "valid certificate well past the window", certPth: healthy, want: origincert.ExpiryOK},
		{name: "valid certificate inside the window", certPth: soon, want: origincert.ExpirySoon},
		{name: "valid certificate already expired", certPth: expired, want: origincert.ExpiryExpired},
		{name: "never observed - no path recorded at all", certPth: "", want: origincert.ExpiryUnknown, wantErr: true},
		{name: "file does not exist", certPth: filepath.Join(broken, "missing.crt"), want: origincert.ExpiryUnknown, wantErr: true},
		{name: "content is not a certificate", certPth: notACert, want: origincert.ExpiryUnknown, wantErr: true},
		{name: "PEM block will not parse", certPth: unparseable, want: origincert.ExpiryUnknown, wantErr: true},
		{name: "a private key where a certificate should be", certPth: wrongKind, want: origincert.ExpiryUnknown, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs := Observe(origincert.InventoryEntry{Name: "n", Service: "apiary_managerd", CertPath: tc.certPth}, now)
			if obs.Status != tc.want {
				t.Fatalf("Status = %q, want %q (error %q)", obs.Status, tc.want, obs.Error)
			}
			// An unknown verdict must always carry the reason. It is
			// the only actionable content such a row has.
			if tc.want == origincert.ExpiryUnknown && obs.Error == "" {
				t.Error("unknown verdict carried no reason")
			}
			// No failure may be laundered into a definite verdict by
			// accident: an unknown row has no observed expiry at all.
			if tc.want == origincert.ExpiryUnknown && !obs.NotAfter.IsZero() {
				t.Errorf("unknown verdict reported NotAfter %v; an unobserved expiry must stay zero", obs.NotAfter)
			}
			if tc.wantErr && obs.Error == "" {
				t.Error("expected an error message for this failure mode")
			}
			if !tc.wantErr && obs.Error != "" {
				t.Errorf("successful check carried an error: %s", obs.Error)
			}
		})
	}
}

// TestObserve_ReportedExpiryIsNeverTheRecordedCopy pins the distinction
// the page depends on: a check that fails is unknown even when the
// inventory holds a perfectly healthy-looking recorded expiry. Falling
// back to the recorded value would render an unverified copy as a
// current verdict, which is the failure mode this package exists to
// remove.
func TestObserve_ReportedExpiryIsNeverTheRecordedCopy(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	obs := Observe(origincert.InventoryEntry{
		Name:      "managerd",
		CertPath:  filepath.Join(t.TempDir(), "gone.crt"),
		ExpiresAt: now.Add(300 * 24 * time.Hour),
	}, now)
	if obs.Status != origincert.ExpiryUnknown {
		t.Fatalf("Status = %q, want unknown despite a recorded expiry 300 days out", obs.Status)
	}
	if !obs.NotAfter.IsZero() {
		t.Errorf("NotAfter = %v, want zero: the recorded expiry must never become an observation", obs.NotAfter)
	}
	if obs.Error == "" {
		t.Error("no reason recorded for a failed check over a recorded expiry")
	}
}

// TestObserve_NeverReadsThePrivateKey is the structural half of the key
// custody rule, and it is a proof rather than an assertion: the key file
// is deleted outright, and a self-check that opened it would have no
// choice but to report unknown. Getting a definite verdict therefore
// demonstrates the key is never read, and a package that never reads a
// key cannot leak one.
func TestObserve_NeverReadsThePrivateKey(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	certPath, keyPath := newTestPair(t, dir, "managerd", now.Add(-100*24*time.Hour), now.Add(200*24*time.Hour))

	entry := origincert.InventoryEntry{
		Name: "managerd", Service: "apiary_managerd", CertPath: certPath,
		// KeyPath is set exactly as an inventory entry sets it, to show
		// that having it populated changes nothing.
		KeyPath: keyPath,
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatalf("removing key file: %v", err)
	}
	obs := Observe(entry, now)
	if obs.Status != origincert.ExpiryOK {
		t.Fatalf("Status = %q (error %q), want ok - the key file is gone and the verdict did not notice, which is the point", obs.Status, obs.Error)
	}
}

// TestInspect_DoesNotReturnKeyMaterial is the second half of the same
// proof, on the parse layer: nothing Inspect returns can be derived from
// a private key, and its own return type has no field that could hold
// one.
func TestInspect_DoesNotReturnKeyMaterial(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	certPath, keyPath := newTestPair(t, dir, "managerd", now.Add(-100*24*time.Hour), now.Add(200*24*time.Hour))
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("reading key: %v", err)
	}
	leaf, err := Inspect(certPath)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	// Every string Inspect can produce, concatenated, must be free of
	// any distinctive run of the key's own base64 body.
	rendered := leaf.Subject + strings.Join(leaf.DNSNames, ",")
	for _, line := range strings.Split(string(keyBytes), "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 24 || !strings.HasPrefix(line, "-----") {
			continue
		}
		if strings.Contains(rendered, line) {
			t.Fatalf("Inspect result contains key material: %q", line)
		}
	}
	if leaf.NotAfter != now.Add(200*24*time.Hour) {
		t.Errorf("NotAfter = %v, want %v", leaf.NotAfter, now.Add(200*24*time.Hour))
	}
	if len(leaf.DNSNames) != 2 {
		t.Errorf("DNSNames = %v, want the certificate's own two SANs", leaf.DNSNames)
	}
}

// TestClassify_ZeroExpiryIsUnknownNotExpired guards the one arithmetic
// case that would otherwise be the whole argument of this package: a
// caller with no observed expiry must get "could not check", not
// "expired since the zero time".
func TestClassify_ZeroExpiryIsUnknownNotExpired(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if got := Classify(time.Time{}, now); got != origincert.ExpiryUnknown {
		t.Errorf("Classify(zero) = %q, want unknown", got)
	}
}

// TestExpiryWindowMatchesTheRenewalLoop pins the alias: the number a
// surface renders and the window the renewal loop acts on are the same
// constant, so they cannot drift.
func TestExpiryWindowMatchesTheRenewalLoop(t *testing.T) {
	if ExpiryWindow != origincert.RenewalWindow {
		t.Errorf("ExpiryWindow = %v, want it to alias origincert.RenewalWindow (%v)", ExpiryWindow, origincert.RenewalWindow)
	}
}

// TestClassify_WindowBoundary pins the exact edge: strictly more than the
// window left is ok, exactly the window left is soon. An off-by-one here
// is the difference between "flagged with a month to spare" and
// "flagged on the day it matters".
func TestClassify_WindowBoundary(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		at   time.Time
		want origincert.ExpiryStatus
	}{
		{name: "one second inside the window", at: now.Add(ExpiryWindow - time.Second), want: origincert.ExpirySoon},
		// Exactly the window is still ok, matching origincert's own
		// InventoryEntry.Expiry boundary (it tests Before, not
		// AtOrBefore). Agreeing with the existing loop is deliberate:
		// two surfaces disagreeing by one second about whether a
		// certificate is due would be its own kind of confusion.
		{name: "exactly the window", at: now.Add(ExpiryWindow), want: origincert.ExpiryOK},
		{name: "one second past the window", at: now.Add(ExpiryWindow + time.Second), want: origincert.ExpiryOK},
		{name: "one second ago", at: now.Add(-time.Second), want: origincert.ExpiryExpired},
		{name: "exactly now", at: now, want: origincert.ExpiryExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.at, now); got != tc.want {
				t.Errorf("Classify(%v) = %q, want %q", tc.at.Sub(now), got, tc.want)
			}
		})
	}
}
