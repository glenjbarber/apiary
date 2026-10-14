package tlsmigrate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/restshimdconfig"
)

// writeSampleConfigs lays out a fixture Comb whose three config files
// all point their TLS fields at oldDir/cert.pem and oldDir/key.pem,
// the shape every Comb installed before issue #99 actually has on
// disk.
func writeSampleConfigs(t *testing.T, configDir, oldDir string) {
	t.Helper()
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cert := filepath.Join(oldDir, "cert.pem")
	key := filepath.Join(oldDir, "key.pem")

	if err := (&nodeconfig.Manager{Path: filepath.Join(configDir, "managerd.json")}).Save(nodeconfig.Config{
		NodeID: "node1", TLSCert: cert, TLSKey: key,
	}); err != nil {
		t.Fatalf("seeding managerd.json: %v", err)
	}
	if err := (&frontendconfig.Manager{Path: filepath.Join(configDir, "frontend.json")}).Save(frontendconfig.Config{
		ManagerAddr: "127.0.0.1:17700", HTTPAddr: "0.0.0.0:8080",
		ManagerTLS: true, ManagerTLSCA: cert, TLSCert: cert, TLSKey: key,
	}); err != nil {
		t.Fatalf("seeding frontend.json: %v", err)
	}
	if err := (&restshimdconfig.Manager{Path: filepath.Join(configDir, "restshimd.json")}).Save(restshimdconfig.Config{
		ManagerAddr: "127.0.0.1:17700", HTTPAddr: "127.0.0.1:8081",
		ManagerTLS: true, ManagerTLSCA: cert, TLSCert: cert, TLSKey: key,
	}); err != nil {
		t.Fatalf("seeding restshimd.json: %v", err)
	}

	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cert, []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMigratePlanAndApplyMovesFilesAndRewritesConfigs(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc", "apiary")
	oldDir := filepath.Join(root, "etc", "apiary-tls")
	writeSampleConfigs(t, configDir, oldDir)

	paths := Paths{ConfigDir: configDir}
	plan, err := New(paths)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if plan.NoOp {
		t.Fatal("plan reports NoOp, want a migration since every field still names the old directory")
	}
	wantNewDir := filepath.Join(configDir, "tls")
	if plan.NewTLSDir != wantNewDir {
		t.Errorf("NewTLSDir = %q, want %q", plan.NewTLSDir, wantNewDir)
	}
	if plan.OldTLSDir != oldDir {
		t.Errorf("OldTLSDir = %q, want %q", plan.OldTLSDir, oldDir)
	}
	if len(plan.Files()) != 3 {
		t.Errorf("Files() = %v, want all three config files", plan.Files())
	}

	if err := plan.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The old files are gone, and the new ones carry the exact bytes
	// and modes across - this is a move, not a copy left behind.
	if _, err := os.Stat(filepath.Join(oldDir, "cert.pem")); !os.IsNotExist(err) {
		t.Errorf("old cert.pem still exists after Apply (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(oldDir, "key.pem")); !os.IsNotExist(err) {
		t.Errorf("old key.pem still exists after Apply (err=%v)", err)
	}
	newCert := filepath.Join(wantNewDir, "cert.pem")
	newKey := filepath.Join(wantNewDir, "key.pem")
	certInfo, err := os.Stat(newCert)
	if err != nil {
		t.Fatalf("stat new cert.pem: %v", err)
	}
	if certInfo.Mode().Perm() != 0o644 {
		t.Errorf("new cert.pem mode = %04o, want 0644", certInfo.Mode().Perm())
	}
	keyInfo, err := os.Stat(newKey)
	if err != nil {
		t.Fatalf("stat new key.pem: %v", err)
	}
	if keyInfo.Mode().Perm() != 0o600 {
		t.Errorf("new key.pem mode = %04o, want 0600 - the private key's permission must carry over exactly", keyInfo.Mode().Perm())
	}
	dirInfo, err := os.Stat(wantNewDir)
	if err != nil {
		t.Fatalf("stat new tls dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("new tls dir mode = %04o, want 0700 (root-only)", dirInfo.Mode().Perm())
	}

	mgr, err := (&nodeconfig.Manager{Path: filepath.Join(configDir, "managerd.json")}).Load()
	if err != nil {
		t.Fatalf("reloading managerd.json: %v", err)
	}
	if mgr.TLSCert != newCert || mgr.TLSKey != newKey {
		t.Errorf("managerd.json tls_cert/tls_key = %q/%q, want %q/%q", mgr.TLSCert, mgr.TLSKey, newCert, newKey)
	}
	front, err := (&frontendconfig.Manager{Path: filepath.Join(configDir, "frontend.json")}).Load()
	if err != nil {
		t.Fatalf("reloading frontend.json: %v", err)
	}
	if front.TLSCert != newCert || front.TLSKey != newKey || front.ManagerTLSCA != newCert {
		t.Errorf("frontend.json TLS fields did not all move to the new directory: cert=%q key=%q ca=%q", front.TLSCert, front.TLSKey, front.ManagerTLSCA)
	}
	rest, err := (&restshimdconfig.Manager{Path: filepath.Join(configDir, "restshimd.json")}).Load()
	if err != nil {
		t.Fatalf("reloading restshimd.json: %v", err)
	}
	if rest.TLSCert != newCert || rest.TLSKey != newKey || rest.ManagerTLSCA != newCert {
		t.Errorf("restshimd.json TLS fields did not all move to the new directory: cert=%q key=%q ca=%q", rest.TLSCert, rest.TLSKey, rest.ManagerTLSCA)
	}
}

// TestMigrateNoOpWhenAlreadyOnNewLayout is explicit in issue #99: "if
// they already point at the new location, no-ops cleanly."
func TestMigrateNoOpWhenAlreadyOnNewLayout(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc", "apiary")
	newDir := filepath.Join(configDir, "tls")
	// Use the new directory as the "old" location passed to the
	// fixture writer, so every config field already names newDir.
	writeSampleConfigs(t, configDir, newDir)

	plan, err := New(Paths{ConfigDir: configDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !plan.NoOp {
		t.Fatalf("plan.NoOp = false, want true: every field already names the new directory (%s)", newDir)
	}

	// Applying a no-op plan must not touch anything, and must not even
	// require the files it would have moved to still exist.
	if err := os.RemoveAll(newDir); err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatalf("Apply on a no-op plan should do nothing and never fail: %v", err)
	}
}

// TestMigrateNoOpWithNoTLSFieldsSet covers a Comb with no TLS material
// configured at all - nothing to move, and New must not error over
// empty fields.
func TestMigrateNoOpWithNoTLSFieldsSet(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc", "apiary")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (&nodeconfig.Manager{Path: filepath.Join(configDir, "managerd.json")}).Save(nodeconfig.Config{NodeID: "node1"}); err != nil {
		t.Fatal(err)
	}

	plan, err := New(Paths{ConfigDir: configDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !plan.NoOp {
		t.Fatal("plan.NoOp = false, want true: no file sets any TLS path")
	}
}

// TestMigrateNeverTouchesKeyContents is the hard requirement in issue
// #99: nothing printed or returned by Plan ever carries the private
// key's bytes, only paths.
func TestMigrateNeverTouchesKeyContents(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc", "apiary")
	oldDir := filepath.Join(root, "etc", "apiary-tls")
	writeSampleConfigs(t, configDir, oldDir)

	plan, err := New(Paths{ConfigDir: configDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	secret := "-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----"
	for _, m := range plan.Moves() {
		if m == secret {
			t.Fatal("Moves() leaked key file content instead of a basename")
		}
	}
	for _, f := range plan.Files() {
		if f == secret {
			t.Fatal("Files() leaked key file content instead of a config path")
		}
	}
}
