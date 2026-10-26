package certintegration

// ADR-0174's provisioning of the certificate integration.
//
// What these pin: that the token lands in a 0600 file, that an
// EXISTING token is never replaced, that the ACME account key is
// never transferred, and that the public settings are recorded so the
// issuance path can find them.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/joincredential"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
)

// The token lands in a 0600 file.
//
// A zone-scoped Cloudflare token that anyone on the host can read is
// a token anyone on the host can spend.
func TestInstall_WritesTheToken0600(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "managerd.json")
	manager := &nodeconfig.Manager{Path: cfgPath}
	if err := manager.Save(nodeconfig.Config{NodeID: "brood", CloudflareTokenFile: filepath.Join(dir, "cloudflare-token")}); err != nil {
		t.Fatal(err)
	}

	integration := &joincredential.Integration{
		CloudflareToken:  "the-zone-token",
		CloudflareZoneID: "zone-1",
		AllowedSuffix:    ".lab2.apiary.work",
	}
	if err := InstallWith(integration, cfgPath, func() error { return nil }); err != nil {
		t.Fatalf("Install: %v", err)
	}

	cfg, err := manager.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CloudflareTokenFile == "" {
		t.Fatal("the configuration names no token file")
	}
	info, err := os.Stat(cfg.CloudflareTokenFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the token file's mode is %04o, want 0600", mode)
	}
	data, err := os.ReadFile(cfg.CloudflareTokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "the-zone-token" {
		t.Errorf("the token file holds %q, want the token", string(data))
	}
}

// The public settings are recorded, so the issuance path can find the
// zone, the suffix and the ACME account.
func TestInstall_RecordsThePublicSettings(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "managerd.json")
	manager := &nodeconfig.Manager{Path: cfgPath}
	if err := manager.Save(nodeconfig.Config{NodeID: "brood", CloudflareTokenFile: filepath.Join(dir, "cloudflare-token")}); err != nil {
		t.Fatal(err)
	}
	if err := InstallWith(&joincredential.Integration{
		CloudflareToken:  "t",
		CloudflareZoneID: "zone-1",
		AllowedSuffix:    ".lab2.apiary.work",
		ACMEEmail:        "ops@example.com",
		ACMEDirectoryURL: "https://acme-staging.example/dir",
	}, cfgPath, func() error { return nil }); err != nil {
		t.Fatalf("Install: %v", err)
	}
	cfg, err := manager.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CloudflareZoneID != "zone-1" {
		t.Errorf("CloudflareZoneID = %q, want zone-1", cfg.CloudflareZoneID)
	}
	if cfg.PreparedCertificateSuffix != ".lab2.apiary.work" {
		t.Errorf("PreparedCertificateSuffix = %q, want .lab2.apiary.work", cfg.PreparedCertificateSuffix)
	}
	if cfg.AcmeEmail != "ops@example.com" {
		t.Errorf("AcmeEmail = %q, want ops@example.com", cfg.AcmeEmail)
	}
	if cfg.AcmeDirectoryURL != "https://acme-staging.example/dir" {
		t.Errorf("AcmeDirectoryURL = %q, want the staging URL", cfg.AcmeDirectoryURL)
	}
}

// An EXISTING token is never replaced.
//
// A Comb that already has an integration keeps it: a join must not
// silently replace a credential the operator set up. The refusal names
// the file, so the operator can act on it deliberately.
func TestInstall_RefusesToReplaceAnExistingToken(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "managerd.json")
	tokenPath := filepath.Join(dir, "existing-token")
	if err := os.WriteFile(tokenPath, []byte("the-operators-own-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &nodeconfig.Manager{Path: cfgPath}
	if err := manager.Save(nodeconfig.Config{NodeID: "brood", CloudflareTokenFile: tokenPath}); err != nil {
		t.Fatal(err)
	}

	err := InstallWith(&joincredential.Integration{
		CloudflareToken:  "the-provisioned-token",
		CloudflareZoneID: "zone-1",
	}, cfgPath, func() error { return nil })
	if err == nil {
		t.Fatal("an existing token was replaced")
	}
	if !strings.Contains(err.Error(), "already has a Cloudflare token") {
		t.Errorf("the refusal %q does not say the token already exists", err)
	}
	// And the operator's token is untouched.
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "the-operators-own-token" {
		t.Errorf("the existing token is now %q; it was replaced", string(data))
	}
}

// The ACME account key is NEVER transferred.
//
// Two Combs sharing one ACME account key would mean one compromised
// Comb could issue for the other's account. The joining Comb
// GENERATES its own, so the configuration names a path and the key
// itself never travels.
func TestInstall_NeverCarriesAnAccountKey(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "managerd.json")
	manager := &nodeconfig.Manager{Path: cfgPath}
	if err := manager.Save(nodeconfig.Config{NodeID: "brood", CloudflareTokenFile: filepath.Join(dir, "cloudflare-token")}); err != nil {
		t.Fatal(err)
	}
	if err := InstallWith(&joincredential.Integration{
		CloudflareToken:  "t",
		CloudflareZoneID: "zone-1",
	}, cfgPath, func() error { return nil }); err != nil {
		t.Fatalf("Install: %v", err)
	}
	cfg, err := manager.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AcmeAccountKeyFile == "" {
		t.Fatal("the configuration names no ACME account key file")
	}
	// The key file must NOT exist yet: the joiner generates it on
	// first use, and a key that arrived with the bundle would be a
	// shared secret.
	if _, err := os.Stat(cfg.AcmeAccountKeyFile); err == nil {
		t.Error("the ACME account key file exists; the key must be generated on the joiner, never transferred")
	}
	if !strings.Contains(cfg.AcmeAccountKeyFile, DefaultProvisionedACMEAccountKeyName) {
		t.Errorf("the account key file is %q, want it named %q", cfg.AcmeAccountKeyFile, DefaultProvisionedACMEAccountKeyName)
	}
}

// An integration with no token is refused.
//
// A bundle that carries the public settings but no token would leave
// the joiner believing it had an integration it could not use.
func TestInstall_RefusesAnIntegrationWithNoToken(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "managerd.json")
	manager := &nodeconfig.Manager{Path: cfgPath}
	if err := manager.Save(nodeconfig.Config{NodeID: "brood", CloudflareTokenFile: filepath.Join(dir, "cloudflare-token")}); err != nil {
		t.Fatal(err)
	}
	err := InstallWith(&joincredential.Integration{CloudflareZoneID: "zone-1"}, cfgPath, func() error { return nil })
	if err == nil {
		t.Fatal("an integration with no token was installed")
	}
	if !strings.Contains(err.Error(), "no Cloudflare token") {
		t.Errorf("the refusal %q does not say the token is what is missing", err)
	}
}

// A nil integration is a no-op, not a failure.
//
// Most Combs are not prepared for, so most bundles carry no
// integration at all.
func TestInstall_NilIsANoOp(t *testing.T) {
	if err := Install(nil, filepath.Join(t.TempDir(), "managerd.json")); err != nil {
		t.Errorf("Install(nil) = %v, want nil", err)
	}
}
