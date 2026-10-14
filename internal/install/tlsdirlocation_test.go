package install

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTLSDirOldLocationCheckFlagsOldPath is issue #99's apiaryinstall
// half: a managerd.json still naming a certificate under the old
// sibling directory is reported, by name, with the migration
// subcommand named as the fix.
func TestTLSDirOldLocationCheckFlagsOldPath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "managerd.json")
	old := managerdConfigPath
	managerdConfigPath = path
	defer func() { managerdConfigPath = old }()

	body := `{"node_id": "node1", "tls_cert": "/usr/local/etc/apiary-tls/cert.pem", "tls_key": "/usr/local/etc/apiary-tls/key.pem"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	res := tlsDirOldLocationCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusMisconfigured {
		t.Fatalf("status = %v, want misconfigured for a tls_cert still under the old directory", res.Status)
	}
	for _, want := range []string{"/usr/local/etc/apiary-tls/cert.pem", path} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("Detail should name %q, got: %s", want, res.Detail)
		}
	}
	if !strings.Contains(res.FixHint, "migrate-tls-dir") {
		t.Errorf("FixHint should name the migrate-tls-dir subcommand as the fix, got: %s", res.FixHint)
	}
	if tlsDirOldLocationCheck.Apply != nil {
		t.Error("this is a report-only hint: Apply must stay nil, the fix is apiaryctl migrate-tls-dir, not this package")
	}
	if tlsDirOldLocationCheck.Risk != RiskManualOnly {
		t.Errorf("Risk = %v, want manual-only", tlsDirOldLocationCheck.Risk)
	}
}

// TestTLSDirOldLocationCheckAcceptsNewPath is the other half: a Comb
// already on the new default, or one that has run `apiaryctl
// migrate-tls-dir`, must not be flagged.
func TestTLSDirOldLocationCheckAcceptsNewPath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "managerd.json")
	old := managerdConfigPath
	managerdConfigPath = path
	defer func() { managerdConfigPath = old }()

	body := `{"node_id": "node1", "tls_cert": "/usr/local/etc/apiary/tls/cert.pem", "tls_key": "/usr/local/etc/apiary/tls/key.pem"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	res := tlsDirOldLocationCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusOK {
		t.Fatalf("status = %v, want ok for a tls_cert already under the new default, detail: %s", res.Status, res.Detail)
	}
}

// TestTLSDirOldLocationCheckNoManagerdJSON mirrors
// TestManagerdNodeIDCheckMissingFile: a host that has not been
// installed yet has nothing to report on.
func TestTLSDirOldLocationCheckNoManagerdJSON(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "managerd.json")
	old := managerdConfigPath
	managerdConfigPath = path
	defer func() { managerdConfigPath = old }()

	res := tlsDirOldLocationCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusOK {
		t.Fatalf("status = %v, want ok when managerd.json does not exist yet", res.Status)
	}
}

// TestTLSDirOldLocationCheckNoTLSCert covers a managerd.json that
// exists but never set tls_cert at all.
func TestTLSDirOldLocationCheckNoTLSCert(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "managerd.json")
	old := managerdConfigPath
	managerdConfigPath = path
	defer func() { managerdConfigPath = old }()

	if err := os.WriteFile(path, []byte(`{"node_id": "node1"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	res := tlsDirOldLocationCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusOK {
		t.Fatalf("status = %v, want ok when tls_cert is unset", res.Status)
	}
}

func TestTLSDirOldLocationCheckRegistered(t *testing.T) {
	for _, c := range All() {
		if c.ID == "tls-dir-old-location" {
			return
		}
	}
	t.Fatal("tls-dir-old-location is not in the registry: it would never run")
}
