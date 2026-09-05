package cluster

import (
	"context"
	"testing"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/cloudflare"
)

// fakeCloudflareManager is a fake cloudflareManager: records every
// ReconcileExposures call's desired set and whether StopIfRunning was
// called, with no real outbound HTTP calls or daemon(8)/cloudflared
// processes.
type fakeCloudflareManager struct {
	lastDesired      []cloudflare.DesiredExposure
	reconcileCalls   int
	stopCalls        int
	reconcileErr     error
	lastToken        string
	lastZoneID       string
	lastTunnelTarget string
}

func (f *fakeCloudflareManager) ReconcileExposures(ctx context.Context, token, zoneID, tunnelTarget, tunnelID, credentialsFile string, desired []cloudflare.DesiredExposure) error {
	f.reconcileCalls++
	f.lastDesired = desired
	f.lastToken = token
	f.lastZoneID = zoneID
	f.lastTunnelTarget = tunnelTarget
	return f.reconcileErr
}

func (f *fakeCloudflareManager) StopIfRunning(ctx context.Context) error {
	f.stopCalls++
	return nil
}

func TestReconciler_RunOnce_CloudflareExposesOwnedVMWithIPAddress(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{
			Id: "vm-1", NodeId: "node-a", IpAddress: "10.60.0.5",
			CloudflareHostname: "web.example.com", CloudflarePort: 8080,
		}},
	}}
	zfs := newFakeDatasetManager()
	cf := &fakeCloudflareManager{}

	r := &Reconciler{
		Raft: raft, ZFS: zfs, LocalNodeID: "node-a",
		Cloudflare: cf, CloudflareToken: "tok", CloudflareZoneID: "zone-1", CloudflareTunnelID: "tunnel-1",
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}
	if cf.reconcileCalls != 1 {
		t.Fatalf("ReconcileExposures calls = %d, want 1", cf.reconcileCalls)
	}
	if len(cf.lastDesired) != 1 || cf.lastDesired[0].Hostname != "web.example.com" || cf.lastDesired[0].Address != "10.60.0.5:8080" {
		t.Errorf("desired = %+v, want one entry for web.example.com at 10.60.0.5:8080", cf.lastDesired)
	}
	if cf.lastTunnelTarget != "tunnel-1.cfargotunnel.com" {
		t.Errorf("tunnelTarget = %q, want tunnel-1.cfargotunnel.com", cf.lastTunnelTarget)
	}
}

func TestReconciler_RunOnce_CloudflareSkipsVMWithNoIPAddress(t *testing.T) {
	// ADR-0063 finding 4: network_id/ip_address can drift independently
	// of when SetVMCloudflareExposure last ran - a VM with a hostname
	// set but no assigned IP must be skipped, never built into a broken
	// ingress entry.
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{
			Id: "vm-1", NodeId: "node-a", IpAddress: "",
			CloudflareHostname: "web.example.com", CloudflarePort: 8080,
		}},
	}}
	zfs := newFakeDatasetManager()
	cf := &fakeCloudflareManager{}

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a", Cloudflare: cf}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}
	if len(cf.lastDesired) != 0 {
		t.Fatalf("desired = %+v, want empty - a VM with no IPAddress must never be built into an ingress entry", cf.lastDesired)
	}
}

func TestReconciler_RunOnce_CloudflareIgnoresVMWithNoHostname(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", IpAddress: "10.60.0.5"}},
	}}
	zfs := newFakeDatasetManager()
	cf := &fakeCloudflareManager{}

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a", Cloudflare: cf}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}
	if len(cf.lastDesired) != 0 {
		t.Fatalf("desired = %+v, want empty for a VM with no CloudflareHostname set", cf.lastDesired)
	}
}

func TestReconciler_RunOnce_CloudflareIgnoresVMOwnedByAnotherNode(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{
			Id: "vm-1", NodeId: "node-b", IpAddress: "10.60.0.5",
			CloudflareHostname: "web.example.com", CloudflarePort: 8080,
		}},
	}}
	zfs := newFakeDatasetManager()
	cf := &fakeCloudflareManager{}

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a", Cloudflare: cf}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}
	if len(cf.lastDesired) != 0 {
		t.Fatalf("desired = %+v, want empty - this node must never expose a VM it does not own", cf.lastDesired)
	}
}

func TestReconciler_RunOnce_CloudflareNilCallsStopIfRunning(t *testing.T) {
	// ADR-0063 finding 8: even with the feature disabled entirely
	// (Cloudflare == nil), a leftover process from before it was
	// disabled must still be cleaned up - this works because
	// reconcileCloudflareTunnel falls back to a Manager with fixed
	// default paths, not the (absent) fake. This test only confirms
	// RunOnce doesn't error out when Cloudflare is nil; the real
	// StopIfRunning behavior itself is covered by internal/cloudflare's
	// own TestStopIfRunning_* tests.
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a"}},
	}}
	zfs := newFakeDatasetManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"} // Cloudflare left nil
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}
}
