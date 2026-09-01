package raft

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func ctxWithAuth(value string) context.Context {
	if value == "" {
		return context.Background()
	}
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", value))
}

func TestCheckToken_EmptyWantDisablesCheckEntirely(t *testing.T) {
	// No "authorization" metadata at all - if a configured token were
	// required, this would fail. An empty want means the feature is off
	// (today's unauthenticated-by-file-permissions behavior), so it must
	// pass regardless of what the caller did or didn't present.
	if err := checkToken(context.Background(), ""); err != nil {
		t.Errorf("checkToken(empty want) = %v, want nil (auth disabled)", err)
	}
}

func TestCheckToken_MissingMetadataIsUnauthenticated(t *testing.T) {
	err := checkToken(context.Background(), "secret")
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("checkToken() = %v, want codes.Unauthenticated", err)
	}
}

func TestCheckToken_WrongTokenIsUnauthenticated(t *testing.T) {
	err := checkToken(ctxWithAuth("Bearer wrong"), "secret")
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("checkToken() = %v, want codes.Unauthenticated", err)
	}
}

func TestCheckToken_CorrectTokenWithBearerPrefixSucceeds(t *testing.T) {
	if err := checkToken(ctxWithAuth("Bearer secret"), "secret"); err != nil {
		t.Errorf("checkToken() = %v, want nil", err)
	}
}

func TestCheckToken_CorrectTokenWithoutBearerPrefixSucceeds(t *testing.T) {
	// TokenCredentials always sends "Bearer <token>", but checkToken
	// shouldn't require that exact form - mirrors
	// internal/manager's own extractBearerToken tolerance.
	if err := checkToken(ctxWithAuth("secret"), "secret"); err != nil {
		t.Errorf("checkToken() = %v, want nil", err)
	}
}

func TestTokenCredentials_EmptyTokenAttachesNothing(t *testing.T) {
	md, err := TokenCredentials("").GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata() error: %v", err)
	}
	if len(md) != 0 {
		t.Errorf("GetRequestMetadata() = %v, want empty for an empty token", md)
	}
}

func TestTokenCredentials_NonEmptyTokenAttachesBearerHeader(t *testing.T) {
	md, err := TokenCredentials("secret").GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata() error: %v", err)
	}
	if md["authorization"] != "Bearer secret" {
		t.Errorf("GetRequestMetadata()[authorization] = %q, want %q", md["authorization"], "Bearer secret")
	}
}
