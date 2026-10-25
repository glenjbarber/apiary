package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/frontendconfig"
)

// The live API key (issue #75). These tests are about the ONE property
// that matters: a key written into frontend.json takes effect on a
// frontend that is already running, with no restart.
//
// Without that, the API Keys page can write the key and the frontend
// still fails every call with Unauthenticated - which is the lockout
// the issue describes.

// writeKey writes a frontend.json holding the given manager_api_key.
func writeKey(t *testing.T, path, key string) {
	t.Helper()
	cfg := frontendconfig.Config{ManagerAPIKey: key}
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestLiveKeyPicksUpAKeyWrittenAfterStartup is the property the whole
// change exists for.
func TestLiveKeyPicksUpAKeyWrittenAfterStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frontend.json")

	// The frontend starts with NO key - which is exactly the state of a
	// Comb whose first key has not been created yet.
	k := newLiveKey(path)

	md, err := k.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata: %v", err)
	}
	if len(md) != 0 {
		t.Errorf("with no key configured the credential presented %v, want no metadata at all", md)
	}

	// The API Keys page now writes the key. Nothing restarts.
	writeKey(t, path, "apk_the_new_key")

	// The cache would hide it for up to liveKeyTTL, so the test waits
	// for the TTL rather than reaching into the cache - which is the
	// point: the behaviour under test is the one an operator sees.
	deadline := time.Now().Add(5 * liveKeyTTL)
	var got map[string]string
	for time.Now().Before(deadline) {
		got, err = k.GetRequestMetadata(context.Background())
		if err != nil {
			t.Fatalf("GetRequestMetadata: %v", err)
		}
		if got["authorization"] != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got["authorization"] != "Bearer apk_the_new_key" {
		t.Errorf("authorization = %q, want %q - a key written after startup must take effect without a restart", got["authorization"], "Bearer apk_the_new_key")
	}
}

// TestLiveKeyPresentsNoCredentialWithoutAKey checks that a frontend
// with no key presents NO credential, rather than an empty or malformed
// one.
//
// This matters because managerd's interceptor decides what an
// unauthenticated call means: it ALLOWS one while auth is not yet
// enabled and REFUSES one once it is. Returning an error from the
// credential instead would fail every call locally, with a message
// about the frontend's own configuration rather than about
// authentication.
func TestLiveKeyPresentsNoCredentialWithoutAKey(t *testing.T) {
	k := newLiveKey(filepath.Join(t.TempDir(), "frontend.json"))
	md, err := k.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata returned an error with no key configured: %v", err)
	}
	if _, present := md["authorization"]; present {
		t.Errorf("authorization = %q, want no authorization header at all", md["authorization"])
	}
}

// TestLiveKeyKeepsAWorkingKeyAcrossAReadFailure checks that a
// transient read failure does not strip a working credential.
//
// The file is written atomically, so a torn read is unlikely, but a
// permission change or a filesystem blip is not. Stripping the
// credential in that moment would lock the operator out of a frontend
// that was working a moment ago - the opposite of what this change is
// for.
func TestLiveKeyKeepsAWorkingKeyAcrossAReadFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frontend.json")
	writeKey(t, path, "apk_good_key")

	k := newLiveKey(path)
	md, err := k.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata: %v", err)
	}
	if md["authorization"] != "Bearer apk_good_key" {
		t.Fatalf("authorization = %q, want Bearer apk_good_key", md["authorization"])
	}

	// The file becomes unreadable. The cached key must survive.
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read a 0000-mode file")
	}

	// Force the cache to expire so the read is attempted.
	k.mu.Lock()
	k.readAt = time.Now().Add(-2 * liveKeyTTL)
	k.mu.Unlock()

	md, err = k.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata: %v", err)
	}
	if md["authorization"] != "Bearer apk_good_key" {
		t.Errorf("authorization = %q, want the cached key to survive a read failure", md["authorization"])
	}
}

// TestLiveKeyRequireTransportSecurityIsFalse pins the transport
// property: this is a credential for a LOCAL connection to this Comb's
// own managerd, and the transport it travels over is the one the
// operator configured.
func TestLiveKeyRequireTransportSecurityIsFalse(t *testing.T) {
	if newLiveKey("").RequireTransportSecurity() {
		t.Error("RequireTransportSecurity = true, want false to match this project's local transport")
	}
}
