package manager

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type fakeAPIKeyValidator struct {
	valid, authEnabled bool
	keyID              string
	role               Role
	err                error
}

func (f *fakeAPIKeyValidator) ValidateAPIKeyHash(context.Context, string) (bool, bool, string, Role, error) {
	return f.valid, f.authEnabled, f.keyID, f.role, f.err
}

func ctxWithAuth(value string) context.Context {
	if value == "" {
		return context.Background()
	}
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", value))
}

const testMethod = "/apiary.rpc.v1.ManagerService/ListVMs"

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
	if err := checkAuth(ctxWithAuth(""), testMethod, v); err != nil {
		t.Errorf("checkAuth() error = %v, want nil when no keys exist at all", err)
	}
}

func TestCheckAuth_ValidKeyWithSufficientRoleAccepted(t *testing.T) {
	v := &fakeAPIKeyValidator{authEnabled: true, valid: true, keyID: "key-1", role: RoleViewer}
	if err := checkAuth(ctxWithAuth("Bearer apk_good"), testMethod, v); err != nil {
		t.Errorf("checkAuth() error = %v, want nil for a valid Viewer key calling a Viewer-level RPC", err)
	}
}

func TestCheckAuth_MissingKeyRejectedOnceKeysExist(t *testing.T) {
	v := &fakeAPIKeyValidator{authEnabled: true}
	if err := checkAuth(ctxWithAuth(""), testMethod, v); err == nil {
		t.Errorf("checkAuth() = nil error, want a rejection when a key is required but none was presented")
	}
}

func TestCheckAuth_InvalidKeyRejected(t *testing.T) {
	v := &fakeAPIKeyValidator{authEnabled: true, valid: false}
	if err := checkAuth(ctxWithAuth("Bearer apk_wrong"), testMethod, v); err == nil {
		t.Errorf("checkAuth() = nil error, want a rejection for an invalid key")
	}
}

func TestCheckAuth_ValidatorErrorPropagates(t *testing.T) {
	v := &fakeAPIKeyValidator{err: errors.New("raftd unreachable")}
	if err := checkAuth(ctxWithAuth("Bearer apk_x"), testMethod, v); err == nil {
		t.Errorf("checkAuth() = nil error, want a real error surfaced when the validator itself fails")
	}
}

func TestCheckAuth_InsufficientRoleIsPermissionDenied(t *testing.T) {
	v := &fakeAPIKeyValidator{authEnabled: true, valid: true, role: RoleViewer}
	err := checkAuth(ctxWithAuth("Bearer apk_viewer"), "/apiary.rpc.v1.ManagerService/CreateVM", v)
	if err == nil {
		t.Fatalf("checkAuth() = nil error, want a rejection for a Viewer key calling an Operator-level RPC")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("checkAuth() code = %v, want PermissionDenied", status.Code(err))
	}
}

func TestCheckAuth_HigherRoleSatisfiesLowerRequirement(t *testing.T) {
	v := &fakeAPIKeyValidator{authEnabled: true, valid: true, role: RoleAdmin}
	if err := checkAuth(ctxWithAuth("Bearer apk_admin"), "/apiary.rpc.v1.ManagerService/CreateVM", v); err != nil {
		t.Errorf("checkAuth() error = %v, want nil - an Admin key must satisfy an Operator-level requirement too", err)
	}
}

func TestCheckAuth_UnlistedMethodDefaultsToAdmin(t *testing.T) {
	v := &fakeAPIKeyValidator{authEnabled: true, valid: true, role: RoleOperator}
	err := checkAuth(ctxWithAuth("Bearer apk_operator"), "/apiary.rpc.v1.ManagerService/SomeNewUnlistedRPC", v)
	if err == nil {
		t.Fatalf("checkAuth() = nil error, want a rejection - an RPC missing from requiredRole must fail closed (require Admin), not open")
	}
}

func TestRoleSatisfies_Hierarchy(t *testing.T) {
	cases := []struct {
		have, want Role
		want_ok    bool
	}{
		{RoleViewer, RoleViewer, true},
		{RoleViewer, RoleOperator, false},
		{RoleViewer, RoleAdmin, false},
		{RoleOperator, RoleViewer, true},
		{RoleOperator, RoleOperator, true},
		{RoleOperator, RoleAdmin, false},
		{RoleAdmin, RoleViewer, true},
		{RoleAdmin, RoleOperator, true},
		{RoleAdmin, RoleAdmin, true},
		{Role("bogus"), RoleViewer, false},
	}
	for _, c := range cases {
		if got := c.have.Satisfies(c.want); got != c.want_ok {
			t.Errorf("Role(%q).Satisfies(%q) = %v, want %v", c.have, c.want, got, c.want_ok)
		}
	}
}

// TestRequiredRoleFor_SimulateNodeFailureIsViewer guards against the
// easy-to-miss failure mode requiredRoleFor's own doc comment warns
// about: an RPC accidentally left out of requiredRole silently falls
// back to RoleAdmin, with no compile-time signal - see ADR-0052.
func TestRequiredRoleFor_SimulateNodeFailureIsViewer(t *testing.T) {
	const method = "/apiary.rpc.v1.ManagerService/SimulateNodeFailure"
	if got := requiredRoleFor(method); got != RoleViewer {
		t.Errorf("requiredRoleFor(%q) = %q, want %q", method, got, RoleViewer)
	}
}

func TestRequiredRoleFor_SimulateNetworkFailureIsViewer(t *testing.T) {
	const method = "/apiary.rpc.v1.ManagerService/SimulateNetworkFailure"
	if got := requiredRoleFor(method); got != RoleViewer {
		t.Errorf("requiredRoleFor(%q) = %q, want %q", method, got, RoleViewer)
	}
}
