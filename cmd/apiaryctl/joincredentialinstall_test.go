package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/joincredential"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
)

func TestInstallJoinCredentialBundleWritesLocalSecretsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	nodePath := filepath.Join(dir, "managerd.json")
	frontPath := filepath.Join(dir, "frontend.json")
	tokenPath := filepath.Join(dir, "restart-guardrail-token")
	bundle := joincredential.Bundle{Version: 1, RequestID: "jreq-0123456789abcdef", NodeID: "comb-7", PeerAPIKey: "apk-peer-key", RestartGuardrailToken: "colony-restart-token"}
	if err := installJoinCredentialBundle(bundle, nodePath, frontPath, tokenPath); err != nil {
		t.Fatal(err)
	}
	if err := installJoinCredentialBundle(bundle, nodePath, frontPath, tokenPath); err != nil {
		t.Fatalf("second installation should be safe and idempotent: %v", err)
	}
	nodeCfg, err := (&nodeconfig.Manager{Path: nodePath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if nodeCfg.PeerAPIKey != bundle.PeerAPIKey {
		t.Fatal("managerd peer key was not installed")
	}
	frontCfg, err := (&frontendconfig.Manager{Path: frontPath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if frontCfg.ManagerAPIKey != bundle.PeerAPIKey {
		t.Fatal("frontend manager key was not installed")
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(token)) != bundle.RestartGuardrailToken {
		t.Fatal("restart-guardrail token was not installed")
	}
	for _, path := range []string{nodePath, frontPath, tokenPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", filepath.Base(path), info.Mode().Perm())
		}
	}
}

func TestInstallJoinCredentialBundleRefusesMismatchedExistingToken(t *testing.T) {
	dir := t.TempDir()
	nodePath := filepath.Join(dir, "managerd.json")
	frontPath := filepath.Join(dir, "frontend.json")
	tokenPath := filepath.Join(dir, "restart-guardrail-token")
	if err := os.WriteFile(tokenPath, []byte("different-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := joincredential.Bundle{Version: 1, RequestID: "jreq-0123456789abcdef", NodeID: "comb-7", PeerAPIKey: "apk-peer-key", RestartGuardrailToken: "colony-restart-token"}
	if err := installJoinCredentialBundle(bundle, nodePath, frontPath, tokenPath); err == nil || !strings.Contains(err.Error(), "different restart-guardrail token") {
		t.Fatalf("mismatched token error = %v, want refusal", err)
	}
	if _, err := os.Stat(nodePath); !os.IsNotExist(err) {
		t.Fatalf("managerd config was modified before token mismatch refusal: %v", err)
	}
	if _, err := os.Stat(frontPath); !os.IsNotExist(err) {
		t.Fatalf("frontend config was modified before token mismatch refusal: %v", err)
	}
}
