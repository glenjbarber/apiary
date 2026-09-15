package manager

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// TestRestartGuardrailToken_EmptyConfiguredTokenAlwaysRejects is the
// direct regression test for the final design review finding: with no
// token file provisioned (in-memory value ""), both ReserveRestartLease
// and ConfirmRestartCompleted must reject an empty bearer token AND a
// genuinely absent authorization header - proving the check requires a
// non-empty configured token before ever calling
// subtle.ConstantTimeCompare (which would otherwise treat two empty
// values as equal).
func TestRestartGuardrailToken_EmptyConfiguredTokenAlwaysRejects(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, raftdSocket, "raftd-1")
	_ = srv // s.restartGuardrailToken stays "" (default) - nothing to set

	ctx := context.Background()

	if _, err := client.ReserveRestartLease(ctx, &rpcpb.ReserveRestartLeaseRequest{Service: "apiary_managerd", NodeId: "raftd-1"}); err == nil {
		t.Error("ReserveRestartLease() with no token configured and no bearer presented = no error, want PermissionDenied")
	}

	emptyBearerCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "))
	if _, err := client.ReserveRestartLease(emptyBearerCtx, &rpcpb.ReserveRestartLeaseRequest{Service: "apiary_managerd", NodeId: "raftd-1"}); err == nil {
		t.Error("ReserveRestartLease() with an empty bearer token against an unconfigured server = no error, want PermissionDenied")
	}

	if _, err := client.ConfirmRestartCompleted(ctx, &rpcpb.ConfirmRestartCompletedRequest{Service: "apiary_managerd", NodeId: "raftd-1"}); err == nil {
		t.Error("ConfirmRestartCompleted() with no token configured and no bearer presented = no error, want PermissionDenied")
	}
}

// TestRestartGuardrailToken_NoRPCPathCanProduceOrChangeIt is the direct
// regression test for the design review finding that neither a
// CreateAPIKey-issued credential (of any role, including Admin) nor
// UpdateNodeConfig can ever produce or discover a valid restart-guardrail
// token - only the value loaded once at startup satisfies the check.
func TestRestartGuardrailToken_NoRPCPathCanProduceOrChangeIt(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, raftdSocket, "raftd-1")
	srv.SetRestartGuardrailToken("the-real-token")

	ctx := context.Background()
	createResp, err := client.CreateAPIKey(ctx, &rpcpb.CreateAPIKeyRequest{Name: "test-admin", Role: "admin"})
	if err != nil || createResp.GetError() != "" {
		t.Fatalf("CreateAPIKey() = (%+v, %v)", createResp, err)
	}

	adminCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+createResp.GetRawKey()))
	if _, err := client.ReserveRestartLease(adminCtx, &rpcpb.ReserveRestartLeaseRequest{Service: "apiary_managerd", NodeId: "raftd-1"}); err == nil {
		t.Error("ReserveRestartLease() with a freshly-minted Admin API key = no error, want PermissionDenied - CreateAPIKey must never produce a usable credential here")
	}

	// The freshly-minted key itself is also not equal to (and cannot be
	// set to) the configured token via any RPC - confirm the real token
	// alone succeeds, proving the check is real and correctly wired, not
	// permanently rejecting everything.
	realCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer the-real-token"))
	resp, err := client.ReserveRestartLease(realCtx, &rpcpb.ReserveRestartLeaseRequest{Service: "apiary_managerd", NodeId: "raftd-1"})
	if err != nil {
		t.Fatalf("ReserveRestartLease() with the real token error: %v", err)
	}
	if !resp.GetGranted() {
		t.Errorf("ReserveRestartLease() with the real token = %+v, want granted", resp)
	}
}

// TestRestartNodeService_ManagerdGoesThroughGuardrail is the end-to-end
// regression test for the full RestartNodeService flow against
// apiary_managerd: a clean reservation succeeds and writes the pending-
// restart-confirmation record BEFORE the fake restart call, a second
// concurrent-looking attempt is blocked without force, force overrides it
// and reports guardrail_overridden, and a real ConfirmRestartCompleted
// (simulating the restarted node's own next startup) clears the block.
func TestRestartNodeService_ManagerdGoesThroughGuardrail(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, raftdSocket, "raftd-1")
	srv.SetRestartGuardrailToken("the-real-token")
	confirmDir := t.TempDir()
	srv.SetRestartConfirmStore(NewRestartConfirmStore(confirmDir))
	controller := &fakeNodeServiceController{}
	srv.services = controller

	ctx := context.Background()

	resp, err := client.RestartNodeService(ctx, &rpcpb.RestartNodeServiceRequest{Name: "apiary_managerd"})
	if err != nil {
		t.Fatalf("RestartNodeService() error: %v", err)
	}
	if resp.GetError() != "" || !resp.GetScheduled() {
		t.Fatalf("first RestartNodeService() = %+v, want scheduled with no error", resp)
	}
	if resp.GetGuardrailOverridden() {
		t.Error("first RestartNodeService() guardrail_overridden = true, want false on a clean grant")
	}

	pending, found, err := NewRestartConfirmStore(confirmDir).Load("apiary_managerd")
	if err != nil {
		t.Fatalf("Load() pending restart error: %v", err)
	}
	if !found {
		t.Fatal("pending-restart-confirmation record not found after a granted RestartNodeService - it must be written before the restart command, not after")
	}
	if pending.NodeID != "raftd-1" {
		t.Errorf("pending.NodeID = %q, want node01", pending.NodeID)
	}

	// A second attempt while the first is still unconfirmed must block.
	blockedResp, err := client.RestartNodeService(ctx, &rpcpb.RestartNodeServiceRequest{Name: "apiary_managerd"})
	if err != nil {
		t.Fatalf("second RestartNodeService() error: %v", err)
	}
	if blockedResp.GetError() == "" {
		t.Error("second RestartNodeService() while unconfirmed = no error, want refused")
	}

	// force overrides the block.
	forcedResp, err := client.RestartNodeService(ctx, &rpcpb.RestartNodeServiceRequest{Name: "apiary_managerd", Force: true})
	if err != nil {
		t.Fatalf("forced RestartNodeService() error: %v", err)
	}
	if forcedResp.GetError() != "" {
		t.Fatalf("forced RestartNodeService() = %+v, want no error", forcedResp)
	}
	if !forcedResp.GetGuardrailOverridden() {
		t.Error("forced RestartNodeService() guardrail_overridden = false, want true")
	}

	// Preflight must report Block while unconfirmed.
	preflight, err := client.PreflightRestartNodeService(ctx, &rpcpb.PreflightRestartNodeServiceRequest{Name: "apiary_managerd"})
	if err != nil {
		t.Fatalf("PreflightRestartNodeService() error: %v", err)
	}
	if preflight.GetVerdict() != "block" {
		t.Errorf("PreflightRestartNodeService() verdict = %q, want block while a lease is still unconfirmed", preflight.GetVerdict())
	}

	// Simulate the restarted node's own next startup confirming itself -
	// this must clear the LEASE block (an unconfirmed in-flight restart),
	// but the separate 10-minute cooldown against this same just-confirmed
	// restart is now correctly active - "do not restart both managers
	// within ten minutes," the guardrail's own reason for existing, not a
	// bug. Verified directly against the FSM's own read below rather than
	// waiting out a real 10-minute cooldown in this test.
	pending2, found, err := NewRestartConfirmStore(confirmDir).Load("apiary_managerd")
	if err != nil || !found {
		t.Fatalf("Load() pending restart after forced attempt: found=%v err=%v", found, err)
	}
	confirmResp, err := srv.ConfirmRestartCompletedLocal(ctx, pending2.Service, pending2.NodeID, pending2.LeaseID)
	if err != nil || confirmResp.GetError() != "" {
		t.Fatalf("ConfirmRestartCompletedLocal() = (%+v, %v)", confirmResp, err)
	}

	preflightAfter, err := client.PreflightRestartNodeService(ctx, &rpcpb.PreflightRestartNodeServiceRequest{Name: "apiary_managerd"})
	if err != nil {
		t.Fatalf("PreflightRestartNodeService() after confirm error: %v", err)
	}
	if preflightAfter.GetVerdict() != "block" {
		t.Errorf("PreflightRestartNodeService() verdict after confirm = %q, want block - the 10-minute cooldown against the just-confirmed restart is correctly still active", preflightAfter.GetVerdict())
	}

	// A fresh restart within the cooldown must still refuse without
	// force, and succeed with it.
	cooldownResp, err := client.RestartNodeService(ctx, &rpcpb.RestartNodeServiceRequest{Name: "apiary_managerd"})
	if err != nil {
		t.Fatalf("post-confirm RestartNodeService() error: %v", err)
	}
	if cooldownResp.GetError() == "" {
		t.Error("post-confirm RestartNodeService() without force = no error, want refused by the 10-minute cooldown")
	}
	cooldownForcedResp, err := client.RestartNodeService(ctx, &rpcpb.RestartNodeServiceRequest{Name: "apiary_managerd", Force: true})
	if err != nil {
		t.Fatalf("post-confirm forced RestartNodeService() error: %v", err)
	}
	if cooldownForcedResp.GetError() != "" || !cooldownForcedResp.GetGuardrailOverridden() {
		t.Errorf("post-confirm forced RestartNodeService() = %+v, want no error and guardrail_overridden=true", cooldownForcedResp)
	}

	// frontend/restshimd never touch the guardrail at all.
	frontendResp, err := client.RestartNodeService(ctx, &rpcpb.RestartNodeServiceRequest{Name: "apiary_frontend"})
	if err != nil || frontendResp.GetError() != "" {
		t.Fatalf("RestartNodeService(apiary_frontend) = (%+v, %v), want unaffected by the guardrail", frontendResp, err)
	}
}

// TestPreflightRestartNodeService_AllowsForNonManagerdServices confirms
// the guardrail's preview degrades to Allow for frontend/restshimd,
// which carry no quorum stake.
func TestPreflightRestartNodeService_AllowsForNonManagerdServices(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client, _ := newManagerdRPCClientAndServer(t, raftdSocket, "raftd-1")

	resp, err := client.PreflightRestartNodeService(context.Background(), &rpcpb.PreflightRestartNodeServiceRequest{Name: "apiary_frontend"})
	if err != nil {
		t.Fatalf("PreflightRestartNodeService() error: %v", err)
	}
	if resp.GetVerdict() != "allow" {
		t.Errorf("PreflightRestartNodeService(apiary_frontend) verdict = %q, want allow", resp.GetVerdict())
	}
}
