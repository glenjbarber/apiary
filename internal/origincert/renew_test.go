package origincert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeRenewIssuer signs whatever CSR it's given with a fresh throwaway
// key, expiring validityUntil from now - a real, parseable certificate
// matching the CSR's own public key (so WritePair's X509KeyPair check
// passes), mirroring internal/manager's own fakeOriginCAIssuer.
type fakeRenewIssuer struct {
	validityUntil time.Duration
	calls         int
	err           error
}

func (f *fakeRenewIssuer) Issue(_ context.Context, _ string, _ []string, _ int, csrPEM string) (IssuedCertificate, error) {
	f.calls++
	if f.err != nil {
		return IssuedCertificate{}, f.err
	}
	block, _ := pem.Decode([]byte(csrPEM))
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return IssuedCertificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return IssuedCertificate{}, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"apiary-test"}},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(f.validityUntil),
		DNSNames:     csr.DNSNames,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, csr.PublicKey, key)
	if err != nil {
		return IssuedCertificate{}, err
	}
	return IssuedCertificate{ID: "renewed-1", ExpiresAt: tmpl.NotAfter,
		PEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}, nil
}

func writeTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRenewer_UnconfiguredDirectoryOrTokenIsNoop(t *testing.T) {
	r := &Renewer{Config: func() (string, string, error) { return "", "", nil }}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v, want nil", err)
	}
}

func TestRenewer_SkipsEntriesWithoutAutoRenew(t *testing.T) {
	dir := t.TempDir()
	if err := SaveInventory(dir, []InventoryEntry{{
		Name: "managerd", Service: "apiary_managerd", Hostnames: []string{"api.example.com"},
		ExpiresAt: time.Now().Add(time.Hour), ValidityDays: 365, AutoRenew: false,
	}}); err != nil {
		t.Fatal(err)
	}
	issuer := &fakeRenewIssuer{validityUntil: 365 * 24 * time.Hour}
	r := &Renewer{
		Config: func() (string, string, error) { return dir, writeTokenFile(t, "tok"), nil },
		Issuer: issuer,
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if issuer.calls != 0 {
		t.Errorf("issuer called %d times, want 0 for a non-auto-renew entry expiring soon", issuer.calls)
	}
}

func TestRenewer_SkipsHealthyAutoRenewEntries(t *testing.T) {
	dir := t.TempDir()
	if err := SaveInventory(dir, []InventoryEntry{{
		Name: "managerd", Service: "apiary_managerd", Hostnames: []string{"api.example.com"},
		ExpiresAt: time.Now().Add(365 * 24 * time.Hour), ValidityDays: 365, AutoRenew: true,
	}}); err != nil {
		t.Fatal(err)
	}
	issuer := &fakeRenewIssuer{validityUntil: 365 * 24 * time.Hour}
	r := &Renewer{
		Config: func() (string, string, error) { return dir, writeTokenFile(t, "tok"), nil },
		Issuer: issuer,
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if issuer.calls != 0 {
		t.Errorf("issuer called %d times, want 0 for a healthy certificate far from expiry", issuer.calls)
	}
}

func TestRenewer_RenewsExpiringAutoRenewEntryAndRestartsItsService(t *testing.T) {
	dir := t.TempDir()
	if err := SaveInventory(dir, []InventoryEntry{{
		Name: "managerd", Service: "apiary_managerd", Hostnames: []string{"api.example.com"},
		ExpiresAt: time.Now().Add(24 * time.Hour), ValidityDays: 365, AutoRenew: true,
	}}); err != nil {
		t.Fatal(err)
	}
	issuer := &fakeRenewIssuer{validityUntil: 365 * 24 * time.Hour}
	var restarted string
	r := &Renewer{
		Config:         func() (string, string, error) { return dir, writeTokenFile(t, "tok"), nil },
		Issuer:         issuer,
		RestartService: func(service string) { restarted = service },
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if issuer.calls != 1 {
		t.Fatalf("issuer called %d times, want 1 for a certificate expiring within the renewal window", issuer.calls)
	}
	if restarted != "apiary_managerd" {
		t.Errorf("restarted service = %q, want apiary_managerd", restarted)
	}
	entries, err := LoadInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Expiry(time.Now()) != ExpiryOK {
		t.Errorf("inventory after renewal = %+v, want a single OK entry", entries)
	}
}

func TestRenewer_RenewsAlreadyExpiredAutoRenewEntry(t *testing.T) {
	dir := t.TempDir()
	if err := SaveInventory(dir, []InventoryEntry{{
		Name: "managerd", Service: "apiary_managerd", Hostnames: []string{"api.example.com"},
		ExpiresAt: time.Now().Add(-time.Hour), ValidityDays: 365, AutoRenew: true,
	}}); err != nil {
		t.Fatal(err)
	}
	issuer := &fakeRenewIssuer{validityUntil: 365 * 24 * time.Hour}
	r := &Renewer{
		Config: func() (string, string, error) { return dir, writeTokenFile(t, "tok"), nil },
		Issuer: issuer,
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if issuer.calls != 1 {
		t.Errorf("issuer called %d times, want 1 for an already-expired certificate", issuer.calls)
	}
}

func TestRenewer_OneFailureDoesNotBlockOtherRenewals(t *testing.T) {
	dir := t.TempDir()
	if err := SaveInventory(dir, []InventoryEntry{
		{Name: "broken", Service: "apiary_managerd", Hostnames: []string{"broken.example.com"},
			ExpiresAt: time.Now().Add(time.Hour), ValidityDays: 365, AutoRenew: true},
		{Name: "ok-one", Service: "apiary_frontend", Hostnames: []string{"ok.example.com"},
			ExpiresAt: time.Now().Add(time.Hour), ValidityDays: 365, AutoRenew: true},
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	restarts := map[string]bool{}
	issuer := issuerFunc(func(ctx context.Context, token string, hostnames []string, days int, csr string) (IssuedCertificate, error) {
		calls++
		if hostnames[0] == "broken.example.com" {
			return IssuedCertificate{}, errors.New("issuer unavailable")
		}
		return (&fakeRenewIssuer{validityUntil: 365 * 24 * time.Hour}).Issue(ctx, token, hostnames, days, csr)
	})
	r := &Renewer{
		Config:         func() (string, string, error) { return dir, writeTokenFile(t, "tok"), nil },
		Issuer:         issuer,
		RestartService: func(service string) { restarts[service] = true },
	}
	err := r.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce() error = nil, want a joined error for the broken entry")
	}
	if calls != 2 {
		t.Errorf("issuer called %d times, want 2 (both entries attempted despite one failing)", calls)
	}
	if !restarts["apiary_frontend"] {
		t.Error("the healthy renewal (ok-one) should still have triggered its own restart")
	}
	if restarts["apiary_managerd"] {
		t.Error("the failed renewal (broken) should not have triggered a restart")
	}
}

// issuerFunc adapts a plain function to the Issuer interface, for tests
// that need per-call branching a fixed fake struct can't express cleanly.
type issuerFunc func(context.Context, string, []string, int, string) (IssuedCertificate, error)

func (f issuerFunc) Issue(ctx context.Context, token string, hostnames []string, days int, csr string) (IssuedCertificate, error) {
	return f(ctx, token, hostnames, days, csr)
}

func TestInventoryEntry_ExpiryClassifiesOKSoonAndExpired(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		expiresAt time.Time
		want      ExpiryStatus
	}{
		{"far future", now.Add(365 * 24 * time.Hour), ExpiryOK},
		{"just inside the renewal window", now.Add(RenewalWindow - time.Hour), ExpirySoon},
		{"already past", now.Add(-time.Hour), ExpiryExpired},
		{"expires exactly now", now, ExpiryExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := InventoryEntry{ExpiresAt: tt.expiresAt}
			if got := entry.Expiry(now); got != tt.want {
				t.Errorf("Expiry() = %q, want %q", got, tt.want)
			}
		})
	}
}
