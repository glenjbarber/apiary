package manager

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
	"/apiary.rpc.v1.ManagerService/GetVM":                      RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListVMs":                    RoleViewer,
	"/apiary.rpc.v1.ManagerService/GetJail":                    RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListJails":                  RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListISOs":                   RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListNetworks":               RoleViewer,
	"/apiary.rpc.v1.ManagerService/HostStats":                  RoleViewer,
	"/apiary.rpc.v1.ManagerService/GetLocalHASTResourceStatus": RoleViewer,
	"/apiary.rpc.v1.ManagerService/GetVMSerialLog":             RoleViewer,
	"/apiary.rpc.v1.ManagerService/GetNodeConfig":              RoleViewer,

	// GetFrontendConfig/GetRestshimdConfig/GetRaftdConfig (ADR-0102):
	// read-only reports of a sibling daemon's own config, secrets
	// redacted to a `_set` boolean - same tier as GetNodeConfig above,
	// which the Machine Configuration page already shows to viewers.
	"/apiary.rpc.v1.ManagerService/GetFrontendConfig":         RoleViewer,
	"/apiary.rpc.v1.ManagerService/GetRestshimdConfig":        RoleViewer,
	"/apiary.rpc.v1.ManagerService/GetRaftdConfig":            RoleViewer,
	"/apiary.rpc.v1.ManagerService/GetLocalNodeHealth":        RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListAssumptionClaims":      RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListOriginCertificates":    RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListOrphanedHASTResources": RoleViewer,
	"/apiary.rpc.v1.ManagerService/GetNetworkTeardownStatus":  RoleViewer,

	// Plain reads that were added without an entry and so silently defaulted
	// to Admin. ListVMsLocal/ListJailsLocal are the local-FSM variants of
	// ListVMs/ListJails; ClusterHealth, HostPackages, and ListNodeServices
	// are read-only reports of the same kind as HostStats and
	// GetLocalNodeHealth. TestRequiredRole_CoversEveryRPC now fails if an
	// RPC is added without a deliberate role.
	"/apiary.rpc.v1.ManagerService/ListVMsLocal":     RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListJailsLocal":   RoleViewer,
	"/apiary.rpc.v1.ManagerService/ClusterHealth":    RoleViewer,
	"/apiary.rpc.v1.ManagerService/HostPackages":     RoleViewer,
	"/apiary.rpc.v1.ManagerService/ListNodeServices": RoleViewer,

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
	"/apiary.rpc.v1.ManagerService/CreateVM":                    RoleOperator,
	"/apiary.rpc.v1.ManagerService/UpdateVM":                    RoleOperator,
	"/apiary.rpc.v1.ManagerService/DeleteVM":                    RoleOperator,
	"/apiary.rpc.v1.ManagerService/MigrateVM":                   RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetVMFirewallPaused":         RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetVMCloudflareExposure":     RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetVMDesiredState":           RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetVMFirewallRules":          RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetDatasetQuota":             RoleOperator,
	"/apiary.rpc.v1.ManagerService/CreateVMSnapshot":            RoleOperator,
	"/apiary.rpc.v1.ManagerService/RestoreVMSnapshot":           RoleOperator,
	"/apiary.rpc.v1.ManagerService/DeleteVMSnapshot":            RoleOperator,
	"/apiary.rpc.v1.ManagerService/SaveAssumptionClaim":         RoleOperator,
	"/apiary.rpc.v1.ManagerService/DeleteAssumptionClaim":       RoleOperator,
	"/apiary.rpc.v1.ManagerService/PurgeStaleAssumptionResults": RoleOperator,
	"/apiary.rpc.v1.ManagerService/IssueOriginCertificate":      RoleAdmin,
	"/apiary.rpc.v1.ManagerService/CreateJail":                  RoleOperator,
	"/apiary.rpc.v1.ManagerService/UpdateJail":                  RoleOperator,
	"/apiary.rpc.v1.ManagerService/DeleteJail":                  RoleOperator,
	"/apiary.rpc.v1.ManagerService/MigrateJail":                 RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetJailDesiredState":         RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetJailHostname":             RoleOperator,
	"/apiary.rpc.v1.ManagerService/CreateNetwork":               RoleOperator,
	"/apiary.rpc.v1.ManagerService/DeleteNetwork":               RoleOperator,
	"/apiary.rpc.v1.ManagerService/SetNetworkName":              RoleOperator,
	"/apiary.rpc.v1.ManagerService/UploadISO":                   RoleOperator,
	"/apiary.rpc.v1.ManagerService/DeleteISO":                   RoleOperator,
	"/apiary.rpc.v1.ManagerService/ReportVMPhase":               RoleOperator,
	"/apiary.rpc.v1.ManagerService/ReportVMTeardownComplete":    RoleOperator,
	"/apiary.rpc.v1.ManagerService/ReportJailPhase":             RoleOperator,
	"/apiary.rpc.v1.ManagerService/ReportJailTeardownComplete":  RoleOperator,

	// PushISOTo is peer-only, like the Report* RPCs above and
	// PushJailTemplateTo below: a node asks a peer to push it a file the
	// peer already has. Same Operator tier, for the same reason.
	"/apiary.rpc.v1.ManagerService/PushISOTo": RoleOperator,

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
	"/apiary.rpc.v1.ManagerService/UpdateManagerdBindAddress":   RoleAdmin,
	// ConvertStandaloneToJoiner (ADR-0105) stops/resets/restarts this
	// node's own raftd and submits a Colony join request - a host-wide,
	// only-narrowly-reversible physical change, same tier as
	// UpdateManagerdBindAddress above.
	"/apiary.rpc.v1.ManagerService/ConvertStandaloneToJoiner": RoleAdmin,

	// UpdateFrontendConfig/UpdateRestshimdConfig (ADR-0102): same tier
	// as UpdateNodeConfig above - writes a sibling daemon's own config
	// file (including a real credential for frontend) and schedules a
	// restart. There is no UpdateRaftdConfig - see GetRaftdConfig's own
	// doc comment in internal/manager/server.go.
	"/apiary.rpc.v1.ManagerService/UpdateFrontendConfig":  RoleAdmin,
	"/apiary.rpc.v1.ManagerService/UpdateRestshimdConfig": RoleAdmin,

	// RestartNodeService (ADR-0085 era) restarts an allowlisted rc.d
	// service on this Hive, including managerd/frontend themselves - a
	// host-wide action, same tier as UpdateNodeConfig above. Already
	// RoleAdmin via requiredRoleFor's own fail-closed default even
	// without this entry; listed explicitly anyway (ADR-0096) so this
	// map stays a complete, honest reference of every RPC's tier rather
	// than relying silently on the default for one of them.
	"/apiary.rpc.v1.ManagerService/RestartNodeService": RoleAdmin,

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

	// UpdateVoterAddress (ADR-0106) also calls AddVoter against this
	// node's own raft cluster - the same materially-bigger-consequence
	// reasoning as ApproveJoinRequest above, same tier.
	"/apiary.rpc.v1.ManagerService/UpdateVoterAddress": RoleAdmin,

	// OpenColonyJoinWindow/CloseColonyJoinWindow (ADR-0147 Part 4) open
	// and close the Colony-wide window that decides whether this Colony
	// will accept a new member at all. Admin, like every other entry in
	// this block: the map fails closed to Admin for anything absent, so
	// leaving these out would not open them - it would make the
	// requirement accidental rather than stated, with no compile-time
	// signal, which is exactly what TestRequiredRole_CoversEveryRPC
	// exists to prevent. GetColonyJoinWindow is deliberately NOT here:
	// it is exempt from checkAuth altogether, above.
	"/apiary.rpc.v1.ManagerService/OpenColonyJoinWindow":  RoleAdmin,
	"/apiary.rpc.v1.ManagerService/CloseColonyJoinWindow": RoleAdmin,

	// PreflightApproveJoinRequest (ADR-0103) previews ApproveJoinRequest's
	// own reachability gate - Admin-tier, matching ApproveJoinRequest
	// itself exactly, since it makes managerd dial a caller-selected
	// address (a Viewer could otherwise use it as a reachability oracle).
	"/apiary.rpc.v1.ManagerService/PreflightApproveJoinRequest": RoleAdmin,

	// PreflightRestartNodeService (ADR-0103) is read-only and makes no
	// live dial to a caller-influenced address (only a local raft-
	// internal state read) - Viewer-tier, matching
	// GetLocalNetworkBridgeStatus/SimulateNodeFailure's own posture.
	// ReserveRestartLease/ConfirmRestartCompleted are deliberately absent
	// from this map entirely, the same way RequestJoinColony/
	// GetJoinRequestStatus/CancelJoinRequest are above - they're exempted
	// from checkAuth altogether in AuthUnaryInterceptor and gated by
	// restartGuardrailTokenValid instead, not merely a low tier.
	"/apiary.rpc.v1.ManagerService/PreflightRestartNodeService": RoleViewer,
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
// getColonyJoinWindowMethod (ADR-0147 Part 4) is exempted for the
// same reason requestJoinColonyMethod is, and it is the only other RPC
// in this codebase with no alternative: a joining Comb has no Colony
// API key, and it has to be able to learn who the Colony is before it
// can become a member. What makes that safe is that this RPC is
// read-only and refuses outright once no window is live, so the period
// in which an unauthenticated caller learns anything is a period an
// operator opened on purpose and can see in the UI.
const getColonyJoinWindowMethod = "/apiary.rpc.v1.ManagerService/GetColonyJoinWindow"

const cancelJoinRequestMethod = "/apiary.rpc.v1.ManagerService/CancelJoinRequest"

// authenticatePasswordMethod (ADR-0087) is exempted for the same
// reason as requestJoinColonyMethod above: a caller proving their
// identity here has no Colony API key yet, by definition - their
// eventual session role comes from frontend's own role map only after
// this succeeds. The real PAM check inside the handler is the actual
// security boundary, not an API key.
const authenticatePasswordMethod = "/apiary.rpc.v1.ManagerService/AuthenticatePassword"

// reserveRestartLeaseMethod/confirmRestartCompletedMethod (ADR-0103) are
// exempted from checkAuth for a deliberately different reason than every
// other exemption above: these two are NOT meant to be reachable by any
// CreateAPIKey-issued credential at all, Admin or otherwise - two earlier
// designs (a CreateAPIKey-mintable "peer" role; a write-only nodeconfig
// field) both turned out not to actually close that off, since either
// gave an ordinary Admin a real path to mint or set a valid credential
// themselves. The real security boundary here is entirely inside each
// handler: a dedicated comparison (restartGuardrailTokenValid) against
// Server.restartGuardrailToken, a value loaded once from a root-owned
// local file at managerd startup and never exposed through any RPC in
// either direction. Bypassing checkAuth here is not "no authorization" -
// it is "a different, stricter authorization that checkAuth's own
// API-key/role model cannot express."
const reserveRestartLeaseMethod = "/apiary.rpc.v1.ManagerService/ReserveRestartLease"
const confirmRestartCompletedMethod = "/apiary.rpc.v1.ManagerService/ConfirmRestartCompleted"

// stepAsideForRestartMethod (ADR-0145) is exempted from checkAuth for
// the identical reason as the two above: it is not meant to be
// reachable by any CreateAPIKey-issued credential at all, Admin or
// otherwise. It exists so a coordinator can make an arbitrary Comb in
// the Colony give up raft leadership, which is a step in a
// cluster-wide restart - exactly the class of operation the dedicated
// root-owned token was introduced for. The real boundary is again
// entirely inside the handler (restartGuardrailTokenValid against
// Server.restartGuardrailToken), and the response carries only what
// this node observed about its own leadership; no Colony API key is
// read, created, or required to reach it.
const stepAsideForRestartMethod = "/apiary.rpc.v1.ManagerService/StepAsideForRestart"

// mutateColonyUpdateMethod (ADR-0145) is exempted from checkAuth for
// the identical reason as the three above: it is not meant to be
// reachable by any CreateAPIKey-issued credential at all, Admin or
// otherwise. Claiming the Colony's single controlled update is a step
// in a cluster-wide restart - the exact class the dedicated root-owned
// token was introduced for - and an ordinary Admin who could reach it
// could stop a sweep other operators are watching. The real boundary is
// again entirely inside the handler (restartGuardrailTokenValid against
// Server.restartGuardrailToken).
const mutateColonyUpdateMethod = "/apiary.rpc.v1.ManagerService/MutateColonyUpdate"

// executeNodeRestartPlanMethod (ADR-0145) is exempted from checkAuth for
// the identical reason as the three above. It is the composition of all
// three: it asks a Comb to step aside, reserves the cluster-wide restart
// lease, restarts the service, and waits for the restarted process to
// confirm itself. If any part of that is reachable by an ordinary
// Admin API key, then an ordinary Admin API key can restart a
// quorum-critical daemon in the Colony, which is exactly what ADR-0103's
// dedicated root-owned token exists to prevent.
//
// It is guardrail plumbing, not an operator control. There is no UI for
// it (ADR-0145's UI is deliberately a later step) and no REST route in
// restshimd, so today the only callers are a peer managerd forwarding
// with the guardrail token, and an operator on the node itself
// presenting the root-owned token. That is a narrower reach than
// RestartNodeService's Admin-tier RPC on purpose: this call composes
// every step of a guarded restart rather than one of them.
const executeNodeRestartPlanMethod = "/apiary.rpc.v1.ManagerService/ExecuteNodeRestartPlan"

// requestManagerdRestartMethod and issueManagerdRestartMethod (ADR-0146)
// are exempted from checkAuth for the identical reason as the four above,
// and the stakes here are the highest in this file: between them they
// stop and start the managerd that is the Colony's entire control plane.
// An ordinary Admin API key that could reach either would be able to take
// the management daemon down on any Comb it can name, which is a larger
// consequence than restarting a quorum-critical daemon and is exactly
// what the dedicated root-owned token was introduced to prevent.
//
// Neither is an operator control. There is no UI control, no REST route
// and no template for either, and none is being added: the Machine
// page's managerd row is status-only and stays that way, because
// ADR-0142's refusal is about the ORCHESTRATION, not about who may ask.
// A managerd restart during a controlled update is a step of that
// update, and the update is driven by a coordinator holding this token.
const requestManagerdRestartMethod = "/apiary.rpc.v1.ManagerService/RequestManagerdRestart"
const issueManagerdRestartMethod = "/apiary.rpc.v1.ManagerService/IssueManagerdRestart"

// authExemptMethods is every RPC that skips checkAuth's API-key role check,
// each for the specific reason documented on its constant above. It is the
// single definition AuthUnaryInterceptor consults, and TestRequiredRole_
// CoversEveryRPC treats these as the only RPCs allowed to have no entry in
// requiredRole.
var authExemptMethods = map[string]bool{
	statusMethod:                  true,
	requestJoinColonyMethod:       true,
	getJoinRequestStatusMethod:    true,
	cancelJoinRequestMethod:       true,
	authenticatePasswordMethod:    true,
	reserveRestartLeaseMethod:     true,
	confirmRestartCompletedMethod: true,
	stepAsideForRestartMethod:     true,
	mutateColonyUpdateMethod:      true,
	getColonyJoinWindowMethod:     true,

	executeNodeRestartPlanMethod: true,

	requestManagerdRestartMethod: true,
	issueManagerdRestartMethod:   true,
}

// restartGuardrailTokenValid reports whether presented matches configured
// exactly, in constant time - and, critically, only when configured is
// non-empty. subtle.ConstantTimeCompare on two empty byte slices returns
// 1 (equal), so an unprovisioned node (no token file, configured == "")
// must reject unconditionally rather than let an equally-empty (or
// entirely absent) presented token pass by accident.
func restartGuardrailTokenValid(presented, configured string) bool {
	if configured == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(configured)) == 1
}

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
	if !authExemptMethods[info.FullMethod] {
		// The key ID is put into the context HERE rather than re-derived
		// by each handler, for one reason: ADR-0147 Part 3's two-person
		// rule records which API key authorized a join, and a handler
		// that read the bearer token itself would be one refactor away
		// from recording the token's hash or nothing at all. The value
		// is whatever raftd's ValidateAPIKeyHash resolved this
		// credential to, and the only thing derived from it is an id.
		//
		// It is set even when auth is DISABLED, in which case it is
		// empty - see callerAPIKeyID's own doc comment for why an empty
		// id is a refusal in Part 3 rather than a wildcard.
		if keyID, err := s.authenticateAndIdentify(ctx, info.FullMethod); err != nil {
			return nil, err
		} else if keyID != "" {
			ctx = context.WithValue(ctx, callerAPIKeyIDContextKey{}, keyID)
		}
	}
	return handler(ctx, req)
}

// callerAPIKeyIDContextKey is the unexported context key the auth
// interceptor stores the caller's validated API key id under. An
// unexported zero-size type rather than a bare string, so nothing
// outside this package can read or forge it: a handler that could set
// this value could authorize a join on its own say-so, which is the
// exact hole the two-person rule exists to close.
type callerAPIKeyIDContextKey struct{}

// callerAPIKeyID returns the id of the API key this call authenticated
// as, or "" when the call was not authenticated by an API key at all.
//
// "" is a real and important case rather than an error: a managerd with
// no API keys has checkAuth as a no-op (ADR-0023), so it authenticates
// everyone and can authenticate no one in particular. ADR-0147 Part 3
// refuses that rather than waving it through, because the whole point
// of the two-person rule is to count two distinct credentials and a
// Colony with no credentials has none to count.
func callerAPIKeyID(ctx context.Context) string {
	id, _ := ctx.Value(callerAPIKeyIDContextKey{}).(string)
	return id
}

// authenticateAndIdentify is checkAuth plus the one fact handlers need
// from it: which key the credential resolved to. checkAuth keeps its
// existing signature and its existing callers because the only other
// caller is the stream interceptor, which has no use for a key id.
func (s *Server) authenticateAndIdentify(ctx context.Context, fullMethod string) (string, error) {
	key, _ := extractBearerToken(ctx)
	hash := ""
	if key != "" {
		hash = hashAPIKey(key)
	}
	valid, authEnabled, keyID, role, err := raftAPIKeyValidator{s.raft}.ValidateAPIKeyHash(ctx, hash)
	if err != nil {
		return "", status.Errorf(codes.Internal, "checking API key: %v", err)
	}
	if !authEnabled {
		return "", nil
	}
	if key == "" || !valid {
		return "", status.Error(codes.Unauthenticated, "missing or invalid API key")
	}
	if !role.Satisfies(requiredRoleFor(fullMethod)) {
		return "", status.Errorf(codes.PermissionDenied, "this API key's role (%s) may not call %s", role, fullMethod)
	}
	return keyID, nil
}

func (s *Server) AuthStreamInterceptor(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := checkAuth(ss.Context(), info.FullMethod, raftAPIKeyValidator{s.raft}); err != nil {
		return err
	}
	return handler(srv, ss)
}
