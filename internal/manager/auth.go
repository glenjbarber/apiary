package manager

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// apiKeyValidator is the subset of *RaftClient the auth interceptor
// needs, defined locally so its core logic can be unit-tested with a
// fake, the same reasoning isoManager/VNCLookup/VLANStatus already
// follow elsewhere in this package.
type apiKeyValidator interface {
	ValidateAPIKeyHash(ctx context.Context, hashedKey string) (valid, authEnabled bool, keyID string, err error)
}

// raftAPIKeyValidator adapts *RaftClient's real ValidateAPIKeyHash
// (which returns the generated proto response type) to the narrower
// apiKeyValidator interface above.
type raftAPIKeyValidator struct{ raft *RaftClient }

func (v raftAPIKeyValidator) ValidateAPIKeyHash(ctx context.Context, hashedKey string) (valid, authEnabled bool, keyID string, err error) {
	resp, err := v.raft.ValidateAPIKeyHash(ctx, hashedKey)
	if err != nil {
		return false, false, "", err
	}
	if resp.GetError() != "" {
		return false, false, "", fmt.Errorf("%s", resp.GetError())
	}
	return resp.GetValid(), resp.GetAuthEnabled(), resp.GetKeyId(), nil
}

// generateAPIKey returns a new random raw API key (never stored
// anywhere in this form - see checkAuth/hashAPIKey) and its SHA-256
// hash. 32 random bytes + base64.RawURLEncoding mirrors
// internal/frontend/session.go's existing session-token convention;
// the "apk_" prefix is purely for human recognizability in logs/UIs,
// the same idea as GitHub's/Stripe's own prefixed API tokens.
func generateAPIKey() (raw, hashed string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	raw = "apk_" + base64.RawURLEncoding.EncodeToString(buf)
	return raw, hashAPIKey(raw), nil
}

// hashAPIKey returns the hex-encoded SHA-256 digest of raw - the only
// form of a key ever stored in ephemeral state (see ADR-0023).
func hashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// generateAPIKeyID returns a random, non-secret identifier for a new
// ApiKey record - distinct from the key material itself (this value is
// shown freely in the list view; the key is not).
func generateAPIKeyID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "key-" + hex.EncodeToString(buf), nil
}

// extractBearerToken reads the "authorization" gRPC metadata key and
// strips a "Bearer " prefix if present. ok is false if the metadata key
// is entirely absent - an empty presented key is still a real (invalid)
// value, distinct from "no attempt to authenticate at all", though
// checkAuth treats both the same way once any key exists.
func extractBearerToken(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return "", false
	}
	const prefix = "Bearer "
	v := vals[0]
	if len(v) > len(prefix) && v[:len(prefix)] == prefix {
		return v[len(prefix):], true
	}
	return v, true
}

// checkAuth is the entire authorization decision, deliberately with no
// per-method special-casing: until API-key auth has ever been enabled
// (no CreateAPIKey has ever succeeded cluster-wide), every call
// (including the very first CreateAPIKey) is allowed through
// unauthenticated - this is what makes the feature entirely opt-in and
// non-breaking until someone deliberately creates a key. The instant
// that first create succeeds, authEnabled becomes true forever - even
// if every key is later revoked - and every subsequent call (on any
// node) requires a valid one. See ADR-0023.
func checkAuth(ctx context.Context, v apiKeyValidator) error {
	key, _ := extractBearerToken(ctx)
	hash := ""
	if key != "" {
		hash = hashAPIKey(key)
	}
	valid, authEnabled, _, err := v.ValidateAPIKeyHash(ctx, hash)
	if err != nil {
		return status.Errorf(codes.Internal, "checking API key: %v", err)
	}
	if !authEnabled {
		return nil
	}
	if key == "" || !valid {
		return status.Error(codes.Unauthenticated, "missing or invalid API key")
	}
	return nil
}

// statusMethod is exempted from checkAuth below - see AuthUnaryInterceptor.
const statusMethod = "/apiary.rpc.v1.ManagerService/Status"

// AuthUnaryInterceptor/AuthStreamInterceptor gate every RPC on
// ManagerService via checkAuth - this project's first use of gRPC
// interceptors anywhere. UploadISO (the one streaming RPC) is checked
// once at stream-open, same as every unary call.
//
// Status is the one deliberate exception: checkAuth itself needs to
// reach raftd (ValidateAPIKeyHash), but Status's entire purpose is to
// report whether raftd is reachable as a diagnostic, degrading
// gracefully (RaftReachable=false, RaftError set) instead of erroring
// when it isn't - see Server.Status. Gating it on checkAuth would mean
// the one call meant to work when raftd is down starts failing with an
// opaque "checking API key" error instead, masking the very thing it
// exists to report. StatusResponse carries no secrets (raft
// reachability/leader info only), so letting it bypass auth entirely
// is an acceptable, narrow carve-out - not a precedent for adding more.
func (s *Server) AuthUnaryInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	if info.FullMethod != statusMethod {
		if err := checkAuth(ctx, raftAPIKeyValidator{s.raft}); err != nil {
			return nil, err
		}
	}
	return handler(ctx, req)
}

func (s *Server) AuthStreamInterceptor(srv interface{}, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := checkAuth(ss.Context(), raftAPIKeyValidator{s.raft}); err != nil {
		return err
	}
	return handler(srv, ss)
}
