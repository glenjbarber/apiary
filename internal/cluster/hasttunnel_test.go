package cluster

import (
	"context"
	"strings"
	"testing"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/hast"
)

// ADR-0158: the encrypted HAST transport.
//
// The tests here cover the two properties that matter and that are easy
// to get wrong: the tunnel address is what goes into hast.conf, and the
// LAN address is NEVER used as a fallback when the tunnel is enabled.

// hastTunnelTestReconciler builds a reconciler with one replicated VM
// and a two-member raft membership, which is the smallest setup that
// produces a hast.conf with a Node.Remote at all.
func hastTunnelTestReconciler(t *testing.T, tunnel HASTTunnelConfig) (*Reconciler, *fakeHASTManager) {
	t.Helper()
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{
			Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", ReplicaNodeId: "node-b"}},
		},
		statusResp: statusResponseWithPeers("node-a", "10.0.0.1:17600", "node-b", "10.0.0.2:17600"),
	}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["hast-vm-vm-1"] = t.TempDir()
	h := newFakeHASTManager()

	r := &Reconciler{
		Raft: raft, ZFS: zfs, HAST: h,
		HASTRestartSettleDelay: time.Millisecond,
		LocalNodeID:            "node-a",
		HASTTunnel:             tunnel,
	}
	return r, h
}

// remoteFor returns the Node.Remote written for the given node name in
// the last written config.
func remoteFor(resources []hast.Resource, nodeName string) string {
	for _, res := range resources {
		for _, n := range res.Nodes {
			if n.Name == nodeName {
				return n.Remote
			}
		}
	}
	return ""
}

func TestHASTTunnel_DisabledKeepsTheLANAddressExactly(t *testing.T) {
	// The regression guard: with the tunnel off, the rendered config
	// must be byte-for-byte what it is today. The LAN address, with no
	// port.
	r, h := hastTunnelTestReconciler(t, HASTTunnelConfig{Enabled: false})

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}
	if len(h.writtenConfigs) == 0 {
		t.Fatal("no hast.conf was written")
	}
	last := h.writtenConfigs[len(h.writtenConfigs)-1]
	if got := remoteFor(last, "node-a"); got != "10.0.0.2" {
		t.Errorf("node-a's remote = %q, want the peer's LAN address 10.0.0.2", got)
	}
	if got := remoteFor(last, "node-b"); got != "10.0.0.1" {
		t.Errorf("node-b's remote = %q, want this node's LAN address 10.0.0.1", got)
	}
}

func TestHASTTunnel_EnabledUsesTheTunnelAddress(t *testing.T) {
	r, h := hastTunnelTestReconciler(t, HASTTunnelConfig{
		Enabled: true,
		Addrs: map[string]string{
			"node-a": "10.90.0.1:8457",
			"node-b": "10.90.0.2:8457",
		},
	})

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}
	last := h.writtenConfigs[len(h.writtenConfigs)-1]
	if got := remoteFor(last, "node-a"); got != "10.90.0.2:8457" {
		t.Errorf("node-a's remote = %q, want the peer's tunnel address 10.90.0.2:8457", got)
	}
	if got := remoteFor(last, "node-b"); got != "10.90.0.1:8457" {
		t.Errorf("node-b's remote = %q, want this node's tunnel address 10.90.0.1:8457", got)
	}
}

func TestHASTTunnel_EnabledNeverFallsBackToTheLANAddress(t *testing.T) {
	// The whole point of the change: with the tunnel on and a peer
	// missing from the tunnel plan, the reconciler must REFUSE rather
	// than write the peer's LAN address. A fallback would mean
	// replication working in plaintext whenever the tunnel is down.
	r, h := hastTunnelTestReconciler(t, HASTTunnelConfig{
		Enabled: true,
		Addrs: map[string]string{
			"node-a": "10.90.0.1:8457",
			// node-b is deliberately absent.
		},
	})

	err := r.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce() succeeded with a peer missing from the tunnel plan, want a refusal")
	}
	if !strings.Contains(err.Error(), "node-b") || !strings.Contains(err.Error(), "tunnel address") {
		t.Errorf("error = %v, want it to name node-b and its missing tunnel address", err)
	}
	// And no config naming the LAN address may have been written.
	for _, cfg := range h.writtenConfigs {
		for _, res := range cfg {
			for _, n := range res.Nodes {
				if n.Remote == "10.0.0.2" || n.Remote == "10.0.0.1" {
					t.Errorf("a config naming the LAN address %q was written; there must be no plaintext fallback", n.Remote)
				}
			}
		}
	}
}

func TestHASTTunnel_MissingLocalTunnelAddressIsRefused(t *testing.T) {
	// The local node's own address is resolved through the same path as
	// the peer's, so a missing LOCAL tunnel address is refused too -
	// otherwise the peer would be told to dial a plaintext address.
	r, _ := hastTunnelTestReconciler(t, HASTTunnelConfig{
		Enabled: true,
		Addrs: map[string]string{
			"node-b": "10.90.0.2:8457",
			// node-a is deliberately absent.
		},
	})

	err := r.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce() succeeded with this node missing from the tunnel plan, want a refusal")
	}
	if !strings.Contains(err.Error(), "tunnel address") {
		t.Errorf("error = %v, want it to name the missing tunnel address", err)
	}
}

func TestHASTTunnel_RoleFlipDoesNotChangeTheAddresses(t *testing.T) {
	// A role flip inverts which side dials, but the addresses in the
	// config must not change: the tunnel is symmetric, so the same
	// rendered config is correct in either role. This is checked by
	// rendering the same pair twice, once with the local node as the
	// owner and once as the replica, and comparing.
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{
			Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", ReplicaNodeId: "node-b"}},
		},
		statusResp: statusResponseWithPeers("node-a", "10.0.0.1:17600", "node-b", "10.0.0.2:17600"),
	}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["hast-vm-vm-1"] = t.TempDir()
	h := newFakeHASTManager()
	tunnel := HASTTunnelConfig{
		Enabled: true,
		Addrs: map[string]string{
			"node-a": "10.90.0.1:8457",
			"node-b": "10.90.0.2:8457",
		},
	}

	r := &Reconciler{
		Raft: raft, ZFS: zfs, HAST: h,
		HASTRestartSettleDelay: time.Millisecond,
		LocalNodeID:            "node-a",
		HASTTunnel:             tunnel,
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}
	asOwner := h.writtenConfigs[len(h.writtenConfigs)-1]

	// Now the same pair with the roles the other way round.
	raft2 := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{
			Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-b", ReplicaNodeId: "node-a"}},
		},
		statusResp: statusResponseWithPeers("node-a", "10.0.0.1:17600", "node-b", "10.0.0.2:17600"),
	}
	h2 := newFakeHASTManager()
	r2 := &Reconciler{
		Raft: raft2, ZFS: zfs, HAST: h2,
		HASTRestartSettleDelay: time.Millisecond,
		LocalNodeID:            "node-b",
		HASTTunnel:             tunnel,
	}
	if err := r2.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() (flipped roles) error: %v", err)
	}
	asReplica := h2.writtenConfigs[len(h2.writtenConfigs)-1]

	// Each node's own remote must name the same address in both
	// renderings: node-a's remote is node-b's tunnel address whether
	// node-a is the owner or the replica.
	if got, want := remoteFor(asOwner, "node-a"), remoteFor(asReplica, "node-a"); got != want {
		t.Errorf("node-a's remote changed with the role flip: %q then %q", got, want)
	}
	if got, want := remoteFor(asOwner, "node-b"), remoteFor(asReplica, "node-b"); got != want {
		t.Errorf("node-b's remote changed with the role flip: %q then %q", got, want)
	}
}

func TestHASTTunnel_PlanParsing(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		plan    string
		wantErr bool
		want    map[string]string
	}{
		{name: "disabled ignores the plan entirely", enabled: false, plan: "garbage"},
		{name: "single entry", enabled: true, plan: "node-a=10.90.0.1:8457", want: map[string]string{"node-a": "10.90.0.1:8457"}},
		{name: "two entries", enabled: true, plan: "node-a=10.90.0.1:8457,node-b=10.90.0.2:8457", want: map[string]string{"node-a": "10.90.0.1:8457", "node-b": "10.90.0.2:8457"}},
		{name: "whitespace tolerated", enabled: true, plan: " node-a = 10.90.0.1:8457 , node-b = 10.90.0.2:8457 ", want: map[string]string{"node-a": "10.90.0.1:8457", "node-b": "10.90.0.2:8457"}},
		{name: "missing equals sign is rejected", enabled: true, plan: "node-a", wantErr: true},
		{name: "empty node id is rejected", enabled: true, plan: "=10.90.0.1:8457", wantErr: true},
		{name: "empty address is rejected", enabled: true, plan: "node-a=", wantErr: true},
		{name: "address without a port is rejected", enabled: true, plan: "node-a=10.90.0.1", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := HASTTunnelConfigFromPlan(tc.enabled, tc.plan)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("HASTTunnelConfigFromPlan() = nil error, want rejection")
				}
				return
			}
			if err != nil {
				t.Fatalf("HASTTunnelConfigFromPlan() error: %v", err)
			}
			if len(cfg.Addrs) != len(tc.want) {
				t.Fatalf("Addrs = %v, want %v", cfg.Addrs, tc.want)
			}
			for k, v := range tc.want {
				if cfg.Addrs[k] != v {
					t.Errorf("Addrs[%q] = %q, want %q", k, cfg.Addrs[k], v)
				}
			}
		})
	}
}

func TestHASTTunnel_DisabledConfigHasNoAddressesAtAll(t *testing.T) {
	// With the tunnel disabled the plan is not even parsed, so a
	// malformed plan cannot break a node that does not use the tunnel.
	cfg, err := HASTTunnelConfigFromPlan(false, "this is not a valid plan at all")
	if err != nil {
		t.Fatalf("HASTTunnelConfigFromPlan() error: %v", err)
	}
	if len(cfg.Addrs) != 0 {
		t.Errorf("Addrs = %v, want none for a disabled tunnel", cfg.Addrs)
	}
}
