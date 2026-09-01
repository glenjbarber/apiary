package raft

import (
	"context"
	"crypto/subtle"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TokenUnaryInterceptor/TokenStreamInterceptor gate every RaftInternal
// RPC on a single shared-secret token, checked via gRPC metadata -
// opt-in via cmd/raftd's own -internal-token-file flag. An empty token
// disables the check entirely, preserving today's behavior (the socket
// relies on file permissions alone, per ADR-0023's own "judged
// sufficient for now" - this is the follow-up that adds a real
// credential on top, not a replacement for those permissions).
//
// Deliberately simpler than managerd's own tiered API-key auth
// (internal/manager's ADR-0023): raftd's internal socket has exactly
// one kind of legitimate caller - managerd on the same node, or a peer
// raftd during -join - not a spectrum of human operators needing
// different privilege tiers. A single shared secret is the right
// amount of complexity here, not a scaled-down copy of ADR-0023's role
// system.
func TokenUnaryInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if err := checkToken(ctx, token); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func TokenStreamInterceptor(token string) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := checkToken(ss.Context(), token); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// checkToken compares the caller's presented "authorization" metadata
// (an optional "Bearer " prefix is stripped, matching internal/manager's
// own extractBearerToken convention) against want using a constant-time
// comparison, the same posture ADR-0019 already applies to session
// credential comparison.
func checkToken(ctx context.Context, want string) error {
	if want == "" {
		return nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing internal token")
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return status.Error(codes.Unauthenticated, "missing internal token")
	}
	got := vals[0]
	const prefix = "Bearer "
	if len(got) > len(prefix) && got[:len(prefix)] == prefix {
		got = got[len(prefix):]
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid internal token")
	}
	return nil
}

// TokenCredentials implements credentials.PerRPCCredentials, attaching
// token to every outgoing call as a bearer credential - used by
// managerd (internal/manager.Dial) and by raftd's own -join client
// (cmd/raftd) to present the shared secret TokenUnaryInterceptor/
// TokenStreamInterceptor check above. An empty token is a valid,
// no-op value (GetRequestMetadata returns nothing), matching this
// project's "opt-in, zero-config-compatible" posture for every other
// auth feature.
type TokenCredentials string

func (t TokenCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	if t == "" {
		return nil, nil
	}
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}

func (TokenCredentials) RequireTransportSecurity() bool { return false }
