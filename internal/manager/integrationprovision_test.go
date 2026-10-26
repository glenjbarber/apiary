package manager

// ADR-0174: the integration is provisioned ONLY to a Comb that was
// prepared for.
//
// That is the whole security argument for the design, so it is the
// property the test exists for: a zone-scoped Cloudflare token can
// change what every host in that zone resolves to, and handing it to
// every Comb would multiply the blast radius of one compromise for
// the benefit of Combs that will never issue a certificate.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
)

// A Comb with NO reservation gets NO integration.
//
// That is the ordinary case, and it is not an error: the bundle simply
// carries none.
func TestIntegrationForPreparedJoin_NoneForAPreparedlessComb(t *testing.T) {
	srv := &Server{}
	pending := &internalpb.PendingJoinRequest{RequestId: "jreq-1", NodeId: "node02"}

	integration, err := srv.integrationForPreparedJoin(t.Context(), pending)
	if err != nil {
		t.Fatalf("integrationForPreparedJoin: %v", err)
	}
	if integration != nil {
		t.Fatalf("a Comb with no reservation was given an integration: %+v", integration)
	}
}

// The token is read from the TOKEN FILE, not from the public record.
//
// The public record is 0644 and may be copied between Combs; the token
// is 0600 and is not. A design that put the token in the public
// record would have made every copy of it a copy of the credential.
func TestReadSavedCloudflareToken_ReadsTheTokenFile(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "cloudflare-token")
	if err := os.WriteFile(tokenPath, []byte("the-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "managerd.json")
	manager := &nodeconfig.Manager{Path: cfgPath}
	if err := manager.Save(nodeconfig.Config{NodeID: "brood", CloudflareTokenFile: tokenPath}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{nodeConfig: manager}

	got, err := srv.readSavedCloudflareToken()
	if err != nil {
		t.Fatalf("readSavedCloudflareToken: %v", err)
	}
	if got != "the-token" {
		t.Errorf("readSavedCloudflareToken = %q, want the-token", got)
	}
}

// A missing token file is NOT an error.
//
// A Comb that has never been set up for certificates has no token,
// and that is the ordinary case rather than a failure.
func TestReadSavedCloudflareToken_MissingIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "managerd.json")
	manager := &nodeconfig.Manager{Path: cfgPath}
	if err := manager.Save(nodeconfig.Config{NodeID: "brood", CloudflareTokenFile: filepath.Join(dir, "absent")}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{nodeConfig: manager}

	got, err := srv.readSavedCloudflareToken()
	if err != nil {
		t.Fatalf("readSavedCloudflareToken: %v", err)
	}
	if got != "" {
		t.Errorf("readSavedCloudflareToken = %q, want empty", got)
	}
}

// The saved integration's PUBLIC record carries no token.
//
// The token is in its own 0600 file, and the public record is the
// thing that may be read, copied and rendered.
func TestSavedIntegrationCarriesNoToken(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "managerd.json")
	tokenPath := filepath.Join(dir, "cloudflare-token")
	if err := os.WriteFile(tokenPath, []byte("the-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &nodeconfig.Manager{Path: cfgPath}
	if err := manager.Save(nodeconfig.Config{NodeID: "brood", CloudflareTokenFile: tokenPath}); err != nil {
		t.Fatal(err)
	}
	integrationPath := filepath.Join(dir, "integration.json")
	if err := os.WriteFile(integrationPath, []byte(`{"zone_id":"zone-1","allowed_suffix":".lab2.apiary.work"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := &Server{nodeConfig: manager}

	saved, err := srv.loadSavedIntegration()
	if err != nil {
		t.Fatalf("loadSavedIntegration: %v", err)
	}
	if saved == nil {
		t.Fatal("the saved integration was not loaded")
	}
	data, err := os.ReadFile(integrationPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "the-token") {
		t.Error("the public integration record carries the token; the token belongs in its own 0600 file")
	}
}
