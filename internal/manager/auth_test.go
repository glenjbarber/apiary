package manager

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/metadata"
)

type fakeAPIKeyValidator struct {
	valid, authEnabled bool
	keyID              string
	err                error
}

func (f *fakeAPIKeyValidator) ValidateAPIKeyHash(context.Context, string) (bool, bool, string, error) {
	return f.valid, f.authEnabled, f.keyID, f.err
}

func ctxWithAuth(value string) context.Context {
	if value == "" {
		return context.Background()
	}
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", value))
}

func TestExtractBearerToken_StripsPrefix(t *testing.T) {
	got, ok := extractBearerToken(ctxWithAuth("Bearer apk_abc123"))
	if !ok || got != "apk_abc123" {
		t.Errorf("extractBearerToken() = (%q, %v), want (apk_abc123, true)", got, ok)
	}
}

func TestExtractBearerToken_NoPrefixReturnsRaw(t *testing.T) {
	got, ok := extractBearerToken(ctxWithAuth("apk_abc123"))
	if !ok || got != "apk_abc123" {
		t.Errorf("extractBearerToken() = (%q, %v), want (apk_abc123, true) even without a Bearer prefix", got, ok)
	}
}

func TestExtractBearerToken_MissingMetadataIsNotOK(t *testing.T) {
	_, ok := extractBearerToken(context.Background())
	if ok {
		t.Errorf("extractBearerToken() ok = true with no metadata at all, want false")
	}
}

func TestHashAPIKey_DeterministicAndDistinct(t *testing.T) {
	a := hashAPIKey("apk_one")
	b := hashAPIKey("apk_one")
	c := hashAPIKey("apk_two")
	if a != b {
		t.Errorf("hashAPIKey() not deterministic: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("hashAPIKey() collided for different inputs: %q", a)
	}
	if a == "apk_one" {
		t.Errorf("hashAPIKey() returned the raw input unchanged")
	}
}

func TestGenerateAPIKey_RawAndHashAreConsistentAndDistinct(t *testing.T) {
	raw1, hash1, err := generateAPIKey()
	if err != nil {
		t.Fatalf("generateAPIKey() error: %v", err)
	}
	if hash1 != hashAPIKey(raw1) {
		t.Errorf("generateAPIKey() hash %q does not match hashAPIKey(raw) %q", hash1, hashAPIKey(raw1))
	}
	raw2, _, err := generateAPIKey()
	if err != nil {
		t.Fatalf("generateAPIKey() (2nd call) error: %v", err)
	}
	if raw1 == raw2 {
		t.Errorf("generateAPIKey() produced the same raw key twice: %q", raw1)
	}
}

func TestCheckAuth_ZeroKeysIsFullyOpen(t *testing.T) {
	v := &fakeAPIKeyValidator{authEnabled: false}
	if err := checkAuth(ctxWithAuth(""), v); err != nil {
		t.Errorf("checkAuth() error = %v, want nil when no keys exist at all", err)
	}
}

func TestCheckAuth_ValidKeyAccepted(t *testing.T) {
	v := &fakeAPIKeyValidator{authEnabled: true, valid: true, keyID: "key-1"}
	if err := checkAuth(ctxWithAuth("Bearer apk_good"), v); err != nil {
		t.Errorf("checkAuth() error = %v, want nil for a valid key", err)
	}
}

func TestCheckAuth_MissingKeyRejectedOnceKeysExist(t *testing.T) {
	v := &fakeAPIKeyValidator{authEnabled: true}
	if err := checkAuth(ctxWithAuth(""), v); err == nil {
		t.Errorf("checkAuth() = nil error, want a rejection when a key is required but none was presented")
	}
}

func TestCheckAuth_InvalidKeyRejected(t *testing.T) {
	v := &fakeAPIKeyValidator{authEnabled: true, valid: false}
	if err := checkAuth(ctxWithAuth("Bearer apk_wrong"), v); err == nil {
		t.Errorf("checkAuth() = nil error, want a rejection for an invalid key")
	}
}

func TestCheckAuth_ValidatorErrorPropagates(t *testing.T) {
	v := &fakeAPIKeyValidator{err: errors.New("raftd unreachable")}
	if err := checkAuth(ctxWithAuth("Bearer apk_x"), v); err == nil {
		t.Errorf("checkAuth() = nil error, want a real error surfaced when the validator itself fails")
	}
}
