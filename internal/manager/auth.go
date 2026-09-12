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

// Role is a tiered permission level (ADR-0030): Viewer < Operator <
// Admin. Kept as a plain string type (not a proto enum) since it's
// shared across the internal/external schemas and the web UI's own
// role-map config, none of which should couple to a particular wire
// representation.
type Role string

const (
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)

// roleRank orders roles for a single "is this enough" comparison.
// Unrecognized values rank below Viewer - never treated as "no
// restriction" (mirrors internal/raft.normalizeRole's own default-deny
// stance).
func roleRank(r Role) int {
	switch r {
	case RoleViewer:
		return 1
	case RoleOperator:
		return 2
	case RoleAdmin:
		return 3
	default:
		return 0
	}
}

// Satisfies reports whether have's rank meets or exceeds want's.
// Exported: internal/frontend's own role-gated routes (ADR-0030) use
// this same hierarchy for a logged-in session's role, not just an API
// key's.
func (have Role) Satisfies(want Role) bool {
	return roleRank(have) >= roleRank(want)
}

// requiredRole maps each ManagerService RPC's FullMethod to the
// minimum Role a caller needs, checked by checkAuth below once auth is
// enabled. Status is handled separately (a total exemption, not a
// Viewer requirement - see AuthUnaryInterceptor's own doc comment);
// any RPC missing from this map defaults to RoleAdmin in
// requiredRoleFor, not silently open, so a newly added RPC can never
// ship unintentionally under-protected.
var requiredRole = map[string]Role{
	// Viewer: read-only.
	"/apiary.rpc.v1.ManagerService/GetVM":                     RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListVMs":                   RoleViewer,
	"/apiary.rpc.v1.ManagerService/GetJail":                   RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListJails":                 RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListISOs":                  RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListNetworks":              RoleViewer,
	"/apiary.rpc.v1.ManagerService/HostStats":                 RoleViewer,
	"/apiary.rpc.v1.ManagerService/GetVMSerialLog":            RoleViewer,
	"/apiary.rpc.v1.ManagerService/GetNodeConfig":             RoleViewer,
	"/apiary.rpc.v1.ManagerService/GetLocalNodeHealth":        RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListAssumptionClaims":      RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListOriginCertificates":    RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListOrphanedHASTResources": RoleViewer,
	"/apiary.rpc.v1.ManagerService/GetNetworkTeardownStatus":  RoleViewer,

	// The Dependency Graph Simulator RPCs are read-only reports - Viewer,
	// the same tier as every other plain read
	// above. Mandatory, not optional: this map fails closed to
	// RoleAdmin for anything absent, so omitting this entry wouldn't
	// leave the RPC open, it would silently make it Admin-only once
	// auth is enabled, with no compile-time signal.
	"/apiary.rpc.v1.ManagerService/SimulateNodeFailure":    RoleViewer,
	"/apiary.rpc.v1.ManagerService/SimulateNetworkFailure": RoleViewer,
	"/apiary.rpc.v1.ManagerService/TraceCellPath":          RoleViewer,

	// GetLocalNetworkBridgeStatus/ListAssumptionResults (ADR-0055,
	// Automated Assumption Checks v1) are both read-only, local-only
	// reports - Viewer, same tier as HostStats/ListNetworks above. Same
	// "mandatory, not optional" reasoning as SimulateNodeFailure's own
	// comment: this map fails closed to RoleAdmin for anything absent.
	"/apiary.rpc.v1.ManagerService/GetLocalNetworkBridgeStatus": RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListAssumptionResults":       RoleViewer,

	// GetUplinkStatus (ADR-0085) is a read-only report of this node's
	// own uplink interface state - Viewer, same tier as HostStats
	// above. The paired write, SetUplinkState, is Admin-tier - see the
	// Admin block below.
	"/apiary.rpc.v1.ManagerService/GetUplinkStatus": RoleViewer,

	// ListJailTemplateNames (ADR-0089) is a read-only report of this
	// node's own local jail base templates - Viewer, same tier as
	// ListISOs below (its direct analog).
	"/apiary.rpc.v1.ManagerService/ListJailTemplateNames": RoleViewer,

	// ListVMSnapshots (ADR-0090) is a read-only report of a VM's own
	// local ZFS snapshots - Viewer, same tier as GetVMSerialLog above
	// (its direct analog: local, per-node, read-only VM-scoped state).
	// The three writes (Create/Restore/Delete) sit in the Operator
	// block below.
	"/apiary.rpc.v1.ManagerService/ListVMSnapshots": RoleViewer,

	// GetVMConsole/ProxyVMConsole (ADR-0096) sit at Operator, not Viewer:
	// unlike every other RPC in the Viewer block above, a VM console is
	// a full bidirectional VNC/RFB tunnel (internal/frontend's
	// proxyConsole pumps bytes both ways) - keyboard and mouse input
	// reach the guest, not just a framebuffer view. That's real control
	// (reboot the guest, reach single-user mode or a bootloader,
	// interact with an in-guest login prompt), a materially different
	// capability than "read-only" implies for the lowest tier this
	// project defines everywhere else.
	"/apiary.rpc.v1.ManagerService/GetVMConsole":   RoleOperator,
	"/apiary.rpc.v1.ManagerService/ProxyVMConsole": RoleOperator,

	// Operator: VM/jail/network lifecycle, ISO management, and the
	// peer-to-peer reconciler-forwarding RPCs (ADR-0029) - a follower
	// node forwarding its own already-authorized write needs at least
	// Operator, not Admin, to keep -peer-api-key's required role the
	// same tier as the writes it's relaying.
	"/apiary.rpc.v1.ManagerService/CreateVM":                   RoleOperator,
	"/apiary.rpc.v1.ManagerService/UpdateVM":                   RoleOperator,
	"/apiary.rpc.v1.ManagerService/DeleteVM":                   RoleOperator,
	"/apiary.rpc.v1.ManagerService/MigrateVM":                  RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetVMFirewallPaused":        RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetVMCloudflareExposure":    RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetVMDesiredState":          RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetVMFirewallRules":         RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetDatasetQuota":            RoleOperator,
	"/apiary.rpc.v1.ManagerService/CreateVMSnapshot":           RoleOperator,
	"/apiary.rpc.v1.ManagerService/RestoreVMSnapshot":          RoleOperator,
	"/apiary.rpc.v1.ManagerService/DeleteVMSnapshot":           RoleOperator,
	"/apiary.rpc.v1.ManagerService/SaveAssumptionClaim":        RoleOperator,
	"/apiary.rpc.v1.ManagerService/DeleteAssumptionClaim":      RoleOperator,
	"/apiary.rpc.v1.ManagerService/IssueOriginCertificate":     RoleAdmin,
	"/apiary.rpc.v1.ManagerService/CreateJail":                 RoleOperator,
	"/apiary.rpc.v1.ManagerService/UpdateJail":                 RoleOperator,
	"/apiary.rpc.v1.ManagerService/DeleteJail":                 RoleOperator,
	"/apiary.rpc.v1.ManagerService/MigrateJail":                RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetJailDesiredState":        RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetJailHostname":            RoleOperator,
	"/apiary.rpc.v1.ManagerService/CreateNetwork":              RoleOperator,
	"/apiary.rpc.v1.ManagerService/DeleteNetwork":              RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetNetworkName":             RoleOperator,
	"/apiary.rpc.v1.ManagerService/UploadISO":                  RoleOperator,
	"/apiary.rpc.v1.ManagerService/DeleteISO":                  RoleOperator,
	"/apiary.rpc.v1.ManagerService/ReportVMPhase":              RoleOperator,
	"/apiary.rpc.v1.ManagerService/ReportVMTeardownComplete":   RoleOperator,
	"/apiary.rpc.v1.ManagerService/ReportJailPhase":            RoleOperator,
	"/apiary.rpc.v1.ManagerService/ReportJailTeardownComplete": RoleOperator,

	// PushJailTemplateTo/ReceiveJailTemplate (ADR-0089) are the jail
	// base-template equivalents of UploadISO/PushISOTo above - same
	// Operator tier, same peer-to-peer-only reasoning.
	"/apiary.rpc.v1.ManagerService/PushJailTemplateTo":  RoleOperator,
	"/apiary.rpc.v1.ManagerService/ReceiveJailTemplate": RoleOperator,

	// Admin: API-key/administration and the ForcePurge* escape hatches
	// (a human-triggered override of a reconciler's own normal
	// teardown sequence - deliberately not something Operator can do
	// unilaterally).
	"/apiary.rpc.v1.ManagerService/ForcePurgeVM":                RoleAdmin,
	"/apiary.rpc.v1.ManagerService/ForcePurgeJail":              RoleAdmin,
	"/apiary.rpc.v1.ManagerService/CleanupOrphanedHASTResource": RoleAdmin,
	"/apiary.rpc.v1.ManagerService/CreateAPIKey":                RoleAdmin,
	"/apiary.rpc.v1.ManagerService/ListAPIKeys":                 RoleAdmin,
	"/apiary.rpc.v1.ManagerService/RevokeAPIKey":                RoleAdmin,
	"/apiary.rpc.v1.ManagerService/UpdateNodeConfig":            RoleAdmin,

	// RestartNodeService (ADR-0085 era) restarts an allowlisted rc.d
	// service on this Hive, including managerd/frontend themselves - a
	// host-wide action, same tier as UpdateNodeConfig above. Already
	// RoleAdmin via requiredRoleFor's own fail-closed default even
	// without this entry; listed explicitly anyway (ADR-0096) so this
	// map stays a complete, honest reference of every RPC's tier rather
	// than relying silently on the default for one of them.
	"/apiary.rpc.v1.ManagerService/RestartNodeService": RoleAdmin,

	// SetUplinkState (ADR-0085) administratively downs/ups this node's
	// uplink interface - a host-wide physical change with a real risk
	// of severing the node's own network access, the same tier as
	// UpdateNodeConfig above.
	"/apiary.rpc.v1.ManagerService/SetUplinkState": RoleAdmin,

	// ListJoinRequests/ApproveJoinRequest/RejectJoinRequest/
	// PurgeJoinRequest (ADR-0083): approving a request calls AddVoter
	// against this node's own raft cluster - a materially bigger
	// consequence than any Operator-tier write above, so this sits at
	// the same tier as CreateAPIKey/RevokeAPIKey, not the
	// peer-forwarding RPCs' Operator tier. PurgeJoinRequest sits here
	// too even though it never touches raft membership - it's still an
	// existing Colony member unilaterally discarding another Comb's
	// request record. RequestJoinColony/GetJoinRequestStatus/
	// CancelJoinRequest are deliberately absent from this map entirely
	// - they're exempted from checkAuth altogether in
	// AuthUnaryInterceptor, not merely low-tier, since a joining Comb
	// has no API key yet by definition.
	"/apiary.rpc.v1.ManagerService/ListJoinRequests":   RoleAdmin,
	"/apiary.rpc.v1.ManagerService/ApproveJoinRequest": RoleAdmin,
	"/apiary.rpc.v1.ManagerService/RejectJoinRequest":  RoleAdmin,
	"/apiary.rpc.v1.ManagerService/PurgeJoinRequest":   RoleAdmin,
}

// requiredRoleFor returns the minimum Role fullMethod needs. An RPC
// absent from the map above is treated as RoleAdmin - a fail-closed
// default so a newly added RPC can never ship silently under-
// protected just because someone forgot to list it.
func requiredRoleFor(fullMethod string) Role {
	if r, ok := requiredRole[fullMethod]; ok {
		return r
	}
	return RoleAdmin
}

// apiKeyValidator is the subset of *RaftClient the auth interceptor
// needs, defined locally so its core logic can be unit-tested with a
// fake, the same reasoning isoManager/VNCLookup/VLANStatus already
// follow elsewhere in this package.
type apiKeyValidator interface {
	ValidateAPIKeyHash(ctx context.Context, hashedKey string) (valid, authEnabled bool, keyID string, role Role, err error)
}

// raftAPIKeyValidator adapts *RaftClient's real ValidateAPIKeyHash
// (which returns the generated proto response type) to the narrower
// apiKeyValidator interface above.
type raftAPIKeyValidator struct{ raft *RaftClient }

func (v raftAPIKeyValidator) ValidateAPIKeyHash(ctx context.Context, hashedKey string) (valid, authEnabled bool, keyID string, role Role, err error) {
	resp, err := v.raft.ValidateAPIKeyHash(ctx, hashedKey)
	if err != nil {
		return false, false, "", "", err
	}
	if resp.GetError() != "" {
		return false, false, "", "", fmt.Errorf("%s", resp.GetError())
	}
	return resp.GetValid(), resp.GetAuthEnabled(), resp.GetKeyId(), Role(resp.GetRole()), nil
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

// checkAuth is the entire authorization decision: until API-key auth
// has ever been enabled (no CreateAPIKey has ever succeeded
// cluster-wide), every call (including the very first CreateAPIKey) is
// allowed through unauthenticated - this is what makes the feature
// entirely opt-in and non-breaking until someone deliberately creates
// a key. The instant that first create succeeds, authEnabled becomes
// true forever - even if every key is later revoked - and every
// subsequent call (on any node) requires a valid key whose role
// satisfies fullMethod's own requirement (ADR-0030) - a wrong-role key
// is a distinct, more specific rejection (codes.PermissionDenied) from
// a missing/invalid key entirely (codes.Unauthenticated), so an
// operator can tell the two apart from the error alone. See ADR-0023.
func checkAuth(ctx context.Context, fullMethod string, v apiKeyValidator) error {
	key, _ := extractBearerToken(ctx)
	hash := ""
	if key != "" {
		hash = hashAPIKey(key)
	}
	valid, authEnabled, _, role, err := v.ValidateAPIKeyHash(ctx, hash)
	if err != nil {
		return status.Errorf(codes.Internal, "checking API key: %v", err)
	}
	if !authEnabled {
		return nil
	}
	if key == "" || !valid {
		return status.Error(codes.Unauthenticated, "missing or invalid API key")
	}
	if !role.Satisfies(requiredRoleFor(fullMethod)) {
		return status.Errorf(codes.PermissionDenied, "this API key's role (%s) may not call %s", role, fullMethod)
	}
	return nil
}

// statusMethod is exempted from checkAuth below - see AuthUnaryInterceptor.
const statusMethod = "/apiary.rpc.v1.ManagerService/Status"

// requestJoinColonyMethod/getJoinRequestStatusMethod (ADR-0083) are
// exempted from checkAuth for a genuinely different reason than
// statusMethod above, not a casual extension of it: a joining Comb
// calling either has no Colony API key at all yet, by definition - it
// isn't a member. RequestJoinColony's own real security boundary is
// entirely downstream of this exemption: the request only ever creates
// a raft-replicated, plainly-visible PendingJoinRequest record, and the
// actual cluster-membership change (ApproveJoinRequest, which is what
// calls AddVoter) stays RoleAdmin-gated below, same as every other
// consequential write. GetJoinRequestStatus is scoped to exactly one
// caller-supplied request_id and returns nothing an unauthenticated
// caller couldn't already see via the (also unauthenticated)
// RequestJoinColony response that created it.
const requestJoinColonyMethod = "/apiary.rpc.v1.ManagerService/RequestJoinColony"
const getJoinRequestStatusMethod = "/apiary.rpc.v1.ManagerService/GetJoinRequestStatus"

// cancelJoinRequestMethod (ADR-0083) is exempted for the identical
// reason as requestJoinColonyMethod/getJoinRequestStatusMethod above:
// the caller is that same joining Comb, withdrawing its own request,
// still with no Colony API key by definition. Knowledge of
// request_id - already the only thing GetJoinRequestStatus requires
// - is the sole credential this needs too.
const cancelJoinRequestMethod = "/apiary.rpc.v1.ManagerService/CancelJoinRequest"

// authenticatePasswordMethod (ADR-0087) is exempted for the same
// reason as requestJoinColonyMethod above: a caller proving their
// identity here has no Colony API key yet, by definition - their
// eventual session role comes from frontend's own role map only after
// this succeeds. The real PAM check inside the handler is the actual
// security boundary, not an API key.
const authenticatePasswordMethod = "/apiary.rpc.v1.ManagerService/AuthenticatePassword"

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
	if info.FullMethod != statusMethod && info.FullMethod != requestJoinColonyMethod && info.FullMethod != getJoinRequestStatusMethod && info.FullMethod != cancelJoinRequestMethod && info.FullMethod != authenticatePasswordMethod {
		if err := checkAuth(ctx, info.FullMethod, raftAPIKeyValidator{s.raft}); err != nil {
			return nil, err
		}
	}
	return handler(ctx, req)
}

func (s *Server) AuthStreamInterceptor(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := checkAuth(ss.Context(), info.FullMethod, raftAPIKeyValidator{s.raft}); err != nil {
		return err
	}
	return handler(srv, ss)
}
