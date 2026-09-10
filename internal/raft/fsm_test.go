package raft

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// fakeSnapshotSink is a minimal in-memory raft.SnapshotSink for testing
// FSMSnapshot.Persist without a real raft.FileSnapshotStore.
type fakeSnapshotSink struct {
	bytes.Buffer
}

func (s *fakeSnapshotSink) ID() string    { return "test" }
func (s *fakeSnapshotSink) Cancel() error { return nil }
func (s *fakeSnapshotSink) Close() error  { return nil }

// mustMarshalCommand marshals cmd, failing the test on error.
func mustMarshalCommand(t *testing.T, cmd *internalpb.Command) []byte {
	t.Helper()
	data, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshaling command: %v", err)
	}
	return data
}

func createVMCmd(id, name string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_CreateVm{
			CreateVm: &internalpb.CreateVM{Vm: &internalpb.VMDefinition{Id: id, Name: name}},
		},
	}
}

func TestFSM_Apply_CreateVM(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})

	applyResult, ok := result.(*FSMApplyResult)
	if !ok {
		t.Fatalf("Apply returned %T, want *FSMApplyResult", result)
	}
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	if applyResult.VM.GetId() != "vm-1" || applyResult.VM.GetName() != "web-1" {
		t.Errorf("VM = %+v, want id=vm-1 name=web-1", applyResult.VM)
	}
	if got := fsm.AppliedIndex(); got != 1 {
		t.Errorf("AppliedIndex() = %d, want 1", got)
	}

	vm, ok := fsm.VM("vm-1")
	if !ok || vm.GetName() != "web-1" {
		t.Errorf("VM(vm-1) = (%+v, %v), want web-1 present", vm, ok)
	}
}

func TestFSM_Apply_CreateVMDuplicateRejected(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})

	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-2"))})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error == "" {
		t.Fatalf("Error = empty, want a duplicate-id rejection")
	}
	// The index still advances even though the command was rejected: the
	// raft-level commit succeeded, only the application-level command did
	// not.
	if got := fsm.AppliedIndex(); got != 2 {
		t.Errorf("AppliedIndex() = %d, want 2", got)
	}
}

// TestFSM_Apply_CreateVMInvalidIDRejected is the regression test for a
// 2026-09-06 security-audit finding: a VM id was only checked for
// non-emptiness/uniqueness, but is later interpolated with no escaping
// as a dnsmasq lease hostname (internal/dhcpd.RenderConfig) and a
// hast.conf resource name (internal/hast.RenderConfig) - a newline in
// the id let an Operator inject an arbitrary dnsmasq directive
// (including dhcp-script=, which dnsmasq runs as root) or hast.conf
// stanza. Proven directly against internal/dhcpd.RenderConfig during
// the audit before this fix existed.
func TestFSM_Apply_CreateVMInvalidIDRejected(t *testing.T) {
	fsm := NewFSM()

	cases := []string{
		"vm-1\ndhcp-script=/tmp/pwn.sh",
		"vm 1",                  // space
		"vm-1/etc",              // slash
		"",                      // handled by the separate empty-id check, included for completeness
		strings.Repeat("a", 65), // over the 64-char limit
	}
	for _, id := range cases {
		result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd(id, "web-1"))})
		if result.(*FSMApplyResult).Error == "" {
			t.Errorf("id %q: Error = empty, want a rejection", id)
		}
	}

	// A valid id must still be accepted.
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})
	if result.(*FSMApplyResult).Error != "" {
		t.Errorf("valid id rejected: %q", result.(*FSMApplyResult).Error)
	}
}

func TestFSM_Apply_DeleteVM(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})

	deleteCmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteVm{DeleteVm: &internalpb.DeleteVM{Id: "vm-1"}},
	}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, deleteCmd)})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	if _, ok := fsm.VM("vm-1"); ok {
		t.Errorf("VM(vm-1) still present after DeleteVM")
	}
}

func TestFSM_Apply_DeleteVM_AssignedVMIsSoftDeleted(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_CreateVm{CreateVm: &internalpb.CreateVM{
			Vm: &internalpb.VMDefinition{Id: "vm-1", NodeId: "node-a"},
		}},
	})})

	deleteCmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteVm{DeleteVm: &internalpb.DeleteVM{Id: "vm-1"}},
	}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, deleteCmd)})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	vm, ok := fsm.VM("vm-1")
	if !ok {
		t.Fatalf("VM(vm-1) not found, want it to still exist as a tombstone")
	}
	if vm.GetDesiredState() != internalpb.VMState_VM_STATE_DELETING {
		t.Errorf("DesiredState = %v, want VM_STATE_DELETING", vm.GetDesiredState())
	}
}

func TestFSM_Apply_UpdateVMPhase(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})

	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateVmPhase{UpdateVmPhase: &internalpb.UpdateVMPhase{
			Id: "vm-1", Phase: internalpb.VMPhase_VM_PHASE_READY,
		}},
	}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, cmd)})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	vm, _ := fsm.VM("vm-1")
	if vm.GetPhase() != internalpb.VMPhase_VM_PHASE_READY {
		t.Errorf("Phase = %v, want VM_PHASE_READY", vm.GetPhase())
	}
}

// TestFSM_Apply_SetVMFirewallPaused_TouchesOnlyThatField guards the
// exact reason this is a dedicated narrow command rather than routed
// through UpdateVM (which fully replaces the record - see ADR-0049):
// every other field, especially FirewallRules, must survive untouched.
func TestFSM_Apply_SetVMFirewallPaused_TouchesOnlyThatField(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_CreateVm{CreateVm: &internalpb.CreateVM{
			Vm: &internalpb.VMDefinition{
				Id: "vm-1", NodeId: "node-a",
				FirewallRules: []*internalpb.FirewallRule{{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "22"}},
			},
		}},
	})})

	cmd := &internalpb.Command{
		Op: &internalpb.Command_SetVmFirewallPaused{SetVmFirewallPaused: &internalpb.SetVMFirewallPaused{
			Id: "vm-1", Paused: true,
		}},
	}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, cmd)})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	vm, _ := fsm.VM("vm-1")
	if !vm.GetFirewallPaused() {
		t.Errorf("FirewallPaused = false, want true")
	}
	if vm.GetNodeId() != "node-a" {
		t.Errorf("NodeId = %q, want node-a (must survive untouched)", vm.GetNodeId())
	}
	if len(vm.GetFirewallRules()) != 1 || vm.GetFirewallRules()[0].GetPortRange() != "22" {
		t.Errorf("FirewallRules = %v, want the original rule to survive untouched", vm.GetFirewallRules())
	}
}

func TestFSM_Apply_SetVMFirewallPaused_MissingIDIsError(t *testing.T) {
	fsm := NewFSM()

	cmd := &internalpb.Command{
		Op: &internalpb.Command_SetVmFirewallPaused{SetVmFirewallPaused: &internalpb.SetVMFirewallPaused{
			Id: "vm-1", Paused: true,
		}},
	}
	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, cmd)})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a missing-id rejection")
	}
}

func TestFSM_Apply_SetVMFirewallRules_ReplacesRulesTouchesNothingElse(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_CreateVm{CreateVm: &internalpb.CreateVM{
			Vm: &internalpb.VMDefinition{
				Id: "vm-1", NodeId: "node-a",
				FirewallRules: []*internalpb.FirewallRule{{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "22"}},
			},
		}},
	})})

	cmd := &internalpb.Command{
		Op: &internalpb.Command_SetVmFirewallRules{SetVmFirewallRules: &internalpb.SetVMFirewallRules{
			Id: "vm-1",
			FirewallRules: []*internalpb.FirewallRule{
				{Direction: "out", Action: "pass", Protocol: "udp", PortRange: "53", Priority: 5},
			},
		}},
	}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, cmd)})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	vm, _ := fsm.VM("vm-1")
	if vm.GetNodeId() != "node-a" {
		t.Errorf("NodeId = %q, want node-a (must survive untouched)", vm.GetNodeId())
	}
	rules := vm.GetFirewallRules()
	if len(rules) != 1 || rules[0].GetDirection() != "out" || rules[0].GetPortRange() != "53" || rules[0].GetPriority() != 5 {
		t.Errorf("FirewallRules = %v, want just the new rule, old one replaced entirely", rules)
	}
}

func TestFSM_Apply_SetVMFirewallRules_EmptyListClearsRules(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_CreateVm{CreateVm: &internalpb.CreateVM{
			Vm: &internalpb.VMDefinition{
				Id: "vm-1", NodeId: "node-a",
				FirewallRules: []*internalpb.FirewallRule{{Direction: "in", Action: "block"}},
			},
		}},
	})})

	cmd := &internalpb.Command{
		Op: &internalpb.Command_SetVmFirewallRules{SetVmFirewallRules: &internalpb.SetVMFirewallRules{Id: "vm-1"}},
	}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, cmd)})

	if result.(*FSMApplyResult).Error != "" {
		t.Fatalf("Error = %q, want empty", result.(*FSMApplyResult).Error)
	}
	vm, _ := fsm.VM("vm-1")
	if len(vm.GetFirewallRules()) != 0 {
		t.Errorf("FirewallRules = %v, want empty after clearing", vm.GetFirewallRules())
	}
}

func TestFSM_Apply_SetVMFirewallRules_MissingIDIsError(t *testing.T) {
	fsm := NewFSM()

	cmd := &internalpb.Command{
		Op: &internalpb.Command_SetVmFirewallRules{SetVmFirewallRules: &internalpb.SetVMFirewallRules{Id: "vm-1"}},
	}
	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, cmd)})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a missing-id rejection")
	}
}

// TestFSM_Apply_SetVMCloudflareExposure_InvalidHostnameRejected is the
// regression test for a 2026-09-06 security-audit finding: hostname was
// entirely unvalidated, but is interpolated verbatim into cloudflared's
// generated YAML config (internal/cloudflare.RenderConfig) - a newline
// would inject an arbitrary ingress rule routing an attacker-chosen
// public hostname to an arbitrary internal address.
func TestFSM_Apply_SetVMCloudflareExposure_InvalidHostnameRejected(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})

	bad := &internalpb.Command{Op: &internalpb.Command_SetVmCloudflareExposure{SetVmCloudflareExposure: &internalpb.SetVMCloudflareExposure{
		Id: "vm-1", Hostname: "evil.example.com\n  - hostname: internal.local\n    service: http://10.0.0.1:22", Port: 80,
	}}}
	if result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, bad)}); result.(*FSMApplyResult).Error == "" {
		t.Fatal("hostname with a newline: Error = empty, want a rejection")
	}

	good := &internalpb.Command{Op: &internalpb.Command_SetVmCloudflareExposure{SetVmCloudflareExposure: &internalpb.SetVMCloudflareExposure{
		Id: "vm-1", Hostname: "web.example.com", Port: 80,
	}}}
	result := fsm.Apply(&raft.Log{Index: 3, Data: mustMarshalCommand(t, good)})
	if result.(*FSMApplyResult).Error != "" {
		t.Errorf("valid hostname rejected: %q", result.(*FSMApplyResult).Error)
	}
	if got := result.(*FSMApplyResult).VM.GetCloudflareHostname(); got != "web.example.com" {
		t.Errorf("CloudflareHostname = %q, want web.example.com", got)
	}
}

func TestFSM_Apply_SetVMDesiredState_TouchesOnlyLifecycleState(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_CreateVm{CreateVm: &internalpb.CreateVM{Vm: &internalpb.VMDefinition{
			Id: "vm-1", NodeId: "node-a", Vcpus: 2, FirewallPaused: true,
		}}},
	})})
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_SetVmDesiredState{SetVmDesiredState: &internalpb.SetVMDesiredState{
			Id: "vm-1", DesiredState: internalpb.VMState_VM_STATE_STOPPED,
		}},
	})})
	if result.(*FSMApplyResult).Error != "" {
		t.Fatalf("SetVMDesiredState error = %q", result.(*FSMApplyResult).Error)
	}
	vm, _ := fsm.VM("vm-1")
	if vm.GetDesiredState() != internalpb.VMState_VM_STATE_STOPPED || vm.GetVcpus() != 2 || !vm.GetFirewallPaused() {
		t.Errorf("VM = %+v, want only desired state changed", vm)
	}
}

func TestFSM_Apply_UpdateVMPhase_MissingIDIsError(t *testing.T) {
	fsm := NewFSM()

	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateVmPhase{UpdateVmPhase: &internalpb.UpdateVMPhase{
			Id: "vm-1", Phase: internalpb.VMPhase_VM_PHASE_READY,
		}},
	}
	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, cmd)})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a missing-id rejection")
	}
}

func TestFSM_Apply_PurgeVM(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})

	cmd := &internalpb.Command{Op: &internalpb.Command_PurgeVm{PurgeVm: &internalpb.PurgeVM{Id: "vm-1"}}}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, cmd)})

	if result.(*FSMApplyResult).Error != "" {
		t.Fatalf("Error = %q, want empty", result.(*FSMApplyResult).Error)
	}
	if _, ok := fsm.VM("vm-1"); ok {
		t.Errorf("VM(vm-1) still present after PurgeVM")
	}
}

func TestFSM_Apply_PurgeVM_IdempotentWhenAlreadyGone(t *testing.T) {
	fsm := NewFSM()

	cmd := &internalpb.Command{Op: &internalpb.Command_PurgeVm{PurgeVm: &internalpb.PurgeVM{Id: "vm-1"}}}
	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, cmd)})

	if result.(*FSMApplyResult).Error != "" {
		t.Errorf("Error = %q, want empty (purging an already-gone id is not an error)", result.(*FSMApplyResult).Error)
	}
}

func TestFSM_Apply_InvalidPayload(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: []byte("not a valid protobuf command")})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error == "" {
		t.Fatalf("Error = empty, want a decoding error for a malformed payload")
	}
}

func TestFSM_SnapshotRestore(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 5, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})
	fsm.Apply(&raft.Log{Index: 6, Data: mustMarshalCommand(t, createVMCmd("vm-2", "web-2"))})

	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}

	sink := &fakeSnapshotSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("Persist() error: %v", err)
	}

	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}

	if got := restored.AppliedIndex(); got != 6 {
		t.Errorf("restored AppliedIndex() = %d, want 6", got)
	}
	if vm, ok := restored.VM("vm-1"); !ok || vm.GetName() != "web-1" {
		t.Errorf("restored VM(vm-1) = (%+v, %v), want web-1 present", vm, ok)
	}
	if vm, ok := restored.VM("vm-2"); !ok || vm.GetName() != "web-2" {
		t.Errorf("restored VM(vm-2) = (%+v, %v), want web-2 present", vm, ok)
	}
}

func TestFSM_ListVMs(t *testing.T) {
	fsm := NewFSM()

	if got := fsm.ListVMs(); len(got) != 0 {
		t.Errorf("ListVMs() on empty FSM = %v, want empty", got)
	}

	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})
	fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMCmd("vm-2", "web-2"))})

	got := fsm.ListVMs()
	if len(got) != 2 {
		t.Fatalf("ListVMs() returned %d entries, want 2", len(got))
	}
	names := map[string]bool{got[0].GetName(): true, got[1].GetName(): true}
	if !names["web-1"] || !names["web-2"] {
		t.Errorf("ListVMs() names = %v, want web-1 and web-2", names)
	}
}

func createNetworkCmd(id, name, subnet string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_CreateNetwork{
			CreateNetwork: &internalpb.CreateNetwork{Network: &internalpb.NetworkDefinition{Id: id, Name: name, Subnet: subnet}},
		},
	}
}

func createVMOnNetworkCmd(id, networkID string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_CreateVm{
			CreateVm: &internalpb.CreateVM{Vm: &internalpb.VMDefinition{Id: id, NetworkId: networkID}},
		},
	}
}

func TestFSM_Apply_CreateNetwork(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	if applyResult.Network.GetId() != "net-1" || applyResult.Network.GetSubnet() != "10.60.0.0/24" {
		t.Errorf("Network = %+v, want id=net-1 subnet=10.60.0.0/24", applyResult.Network)
	}

	network, ok := fsm.Network("net-1")
	if !ok || network.GetName() != "prod" {
		t.Errorf("Network(net-1) = (%+v, %v), want prod present", network, ok)
	}
}

func TestFSM_Apply_CreateNetworkDuplicateRejected(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})

	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "dup", "10.61.0.0/24"))})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a duplicate-id rejection")
	}
}

func TestFSM_Apply_CreateNetworkInvalidSubnetRejected(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "not-a-cidr"))})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want an invalid-subnet rejection")
	}
}

// TestFSM_Apply_CreateNetworkInvalidBridgeNameOrGatewayRejected is the
// regression test for a 2026-09-06 security-audit finding:
// bridge_name/external_gateway were entirely unvalidated, but both are
// interpolated verbatim into generated dnsmasq.conf
// (internal/dhcpd.RenderConfig, as interface=/dhcp-option=...,3,<gw>
// lines) - a newline in either let an Operator inject an arbitrary
// dnsmasq directive. Proven directly against internal/dhcpd.RenderConfig
// during the audit before this fix existed.
func TestFSM_Apply_CreateNetworkInvalidBridgeNameOrGatewayRejected(t *testing.T) {
	fsm := NewFSM()

	badBridge := &internalpb.Command{Op: &internalpb.Command_CreateNetwork{CreateNetwork: &internalpb.CreateNetwork{
		Network: &internalpb.NetworkDefinition{Id: "net-1", Subnet: "10.60.0.0/24", BridgeName: "apnet-x\ndhcp-script=/tmp/pwn.sh"},
	}}}
	if result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, badBridge)}); result.(*FSMApplyResult).Error == "" {
		t.Error("bridge_name with a newline: Error = empty, want a rejection")
	}

	badGateway := &internalpb.Command{Op: &internalpb.Command_CreateNetwork{CreateNetwork: &internalpb.CreateNetwork{
		Network: &internalpb.NetworkDefinition{Id: "net-2", Subnet: "10.61.0.0/24", ExternalGateway: "10.61.0.1\ndhcp-script=/tmp/pwn.sh"},
	}}}
	if result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, badGateway)}); result.(*FSMApplyResult).Error == "" {
		t.Error("external_gateway with a newline: Error = empty, want a rejection")
	}

	badGatewayNotIP := &internalpb.Command{Op: &internalpb.Command_CreateNetwork{CreateNetwork: &internalpb.CreateNetwork{
		Network: &internalpb.NetworkDefinition{Id: "net-3", Subnet: "10.62.0.0/24", ExternalGateway: "not-an-ip"},
	}}}
	if result := fsm.Apply(&raft.Log{Index: 3, Data: mustMarshalCommand(t, badGatewayNotIP)}); result.(*FSMApplyResult).Error == "" {
		t.Error("external_gateway not an IP: Error = empty, want a rejection")
	}

	// A valid bridge_name/external_gateway must still be accepted.
	good := &internalpb.Command{Op: &internalpb.Command_CreateNetwork{CreateNetwork: &internalpb.CreateNetwork{
		Network: &internalpb.NetworkDefinition{Id: "net-4", Subnet: "10.63.0.0/24", BridgeName: "bridge12", ExternalGateway: "10.63.0.1"},
	}}}
	if result := fsm.Apply(&raft.Log{Index: 4, Data: mustMarshalCommand(t, good)}); result.(*FSMApplyResult).Error != "" {
		t.Errorf("valid bridge_name/external_gateway rejected: %q", result.(*FSMApplyResult).Error)
	}
}

func TestFSM_Apply_DeleteNetwork(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})

	deleteCmd := &internalpb.Command{Op: &internalpb.Command_DeleteNetwork{DeleteNetwork: &internalpb.DeleteNetwork{Id: "net-1"}}}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, deleteCmd)})

	if result.(*FSMApplyResult).Error != "" {
		t.Fatalf("Error = %q, want empty", result.(*FSMApplyResult).Error)
	}
	if _, ok := fsm.Network("net-1"); ok {
		t.Errorf("Network(net-1) still present after DeleteNetwork")
	}
}

func TestFSM_Apply_SetNetworkNameRenamesTouchesNothingElse(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})

	cmd := &internalpb.Command{Op: &internalpb.Command_SetNetworkName{SetNetworkName: &internalpb.SetNetworkName{Id: "net-1", Name: "production"}}}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, cmd)})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	network, ok := fsm.Network("net-1")
	if !ok {
		t.Fatal("Network(net-1) missing after SetNetworkName")
	}
	if network.GetName() != "production" {
		t.Errorf("Name = %q, want production", network.GetName())
	}
	if network.GetSubnet() != "10.60.0.0/24" {
		t.Errorf("Subnet = %q, want the original subnet to survive untouched", network.GetSubnet())
	}
}

func TestFSM_Apply_SetNetworkNameMissingIDIsError(t *testing.T) {
	fsm := NewFSM()

	cmd := &internalpb.Command{Op: &internalpb.Command_SetNetworkName{SetNetworkName: &internalpb.SetNetworkName{Id: "missing", Name: "x"}}}
	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, cmd)})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a not-found rejection")
	}
}

func TestFSM_Apply_DeleteNetworkMissingIsError(t *testing.T) {
	fsm := NewFSM()

	deleteCmd := &internalpb.Command{Op: &internalpb.Command_DeleteNetwork{DeleteNetwork: &internalpb.DeleteNetwork{Id: "missing"}}}
	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, deleteCmd)})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a not-found rejection")
	}
}

func TestFSM_Apply_DeleteNetworkStillReferencedIsRejected(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})
	fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMOnNetworkCmd("vm-1", "net-1"))})

	deleteCmd := &internalpb.Command{Op: &internalpb.Command_DeleteNetwork{DeleteNetwork: &internalpb.DeleteNetwork{Id: "net-1"}}}
	result := fsm.Apply(&raft.Log{Index: 3, Data: mustMarshalCommand(t, deleteCmd)})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a still-referenced rejection")
	}
	if _, ok := fsm.Network("net-1"); !ok {
		t.Errorf("Network(net-1) removed despite the rejected delete")
	}
}

func TestFSM_Apply_CreateVMOnNetworkAssignsIPAndMAC(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})

	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMOnNetworkCmd("vm-1", "net-1"))})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	if applyResult.VM.GetIpAddress() != "10.60.0.2" {
		t.Errorf("IpAddress = %q, want 10.60.0.2 (skipping .0 network, .1 gateway)", applyResult.VM.GetIpAddress())
	}
	if applyResult.VM.GetMacAddress() == "" {
		t.Errorf("MacAddress = empty, want a derived address")
	}
}

// TestFSM_Apply_CreateVMWithoutNetworkStillGetsMAC confirms a
// flat-bridge VM (no network_id) still gets a real, derived MAC address
// - previously only a network-attached VM did, leaving a flat-bridge
// VM with whatever random MAC bhyve's own virtio-net device generated,
// which made it impossible for an operator to set up a static DHCP
// reservation on their own router ahead of time.
func TestFSM_Apply_CreateVMWithoutNetworkStillGetsMAC(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	if applyResult.VM.GetMacAddress() == "" {
		t.Errorf("MacAddress = empty, want a derived address even without a network_id")
	}
	if applyResult.VM.GetIpAddress() != "" {
		t.Errorf("IpAddress = %q, want empty - no network_id means no Apiary-managed IP allocation", applyResult.VM.GetIpAddress())
	}
}

// TestFSM_Apply_CreateVMWithoutNetworkMACIsDeterministic mirrors
// TestFSM_Apply_CreateVMOnNetworkIsDeterministic for the flat-bridge
// case - the same id must always derive the same MAC, independent of
// FSM instance/networking mode, since an operator's DHCP reservation
// depends on it never changing.
func TestFSM_Apply_CreateVMWithoutNetworkMACIsDeterministic(t *testing.T) {
	fsm1 := NewFSM()
	r1 := fsm1.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))}).(*FSMApplyResult)

	fsm2 := NewFSM()
	r2 := fsm2.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))}).(*FSMApplyResult)

	if r1.VM.GetMacAddress() != r2.VM.GetMacAddress() {
		t.Errorf("two independent FSMs derived different MACs for the same VM id: %q vs %q", r1.VM.GetMacAddress(), r2.VM.GetMacAddress())
	}
}

func TestFSM_Apply_CreateVMOnNetworkIsDeterministic(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})
	r1 := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMOnNetworkCmd("vm-1", "net-1"))}).(*FSMApplyResult)

	fsm2 := NewFSM()
	fsm2.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})
	r2 := fsm2.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMOnNetworkCmd("vm-1", "net-1"))}).(*FSMApplyResult)

	if r1.VM.GetIpAddress() != r2.VM.GetIpAddress() || r1.VM.GetMacAddress() != r2.VM.GetMacAddress() {
		t.Errorf("two independent FSMs assigned different values for the same commands: %+v vs %+v - not safe under raft replication", r1.VM, r2.VM)
	}
}

func TestFSM_Apply_CreateVMOnNetworkSkipsUsedIPs(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/30"))})
	// A /30 has exactly one usable non-gateway address (10.60.0.2) after
	// skipping .0 (network), .1 (gateway), and .3 (broadcast).
	first := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMOnNetworkCmd("vm-1", "net-1"))}).(*FSMApplyResult)
	if first.Error != "" {
		t.Fatalf("first CreateVM Error = %q, want empty", first.Error)
	}

	second := fsm.Apply(&raft.Log{Index: 3, Data: mustMarshalCommand(t, createVMOnNetworkCmd("vm-2", "net-1"))})
	if second.(*FSMApplyResult).Error == "" {
		t.Fatalf("second CreateVM Error = empty, want an exhausted-network rejection")
	}
}

func TestFSM_Apply_CreateVMUnknownNetworkRejected(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMOnNetworkCmd("vm-1", "missing-network"))})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want an unknown-network rejection")
	}
}

func TestDeriveMAC_IsStableAndLocallyAdministered(t *testing.T) {
	a := deriveMAC("vm-1")
	b := deriveMAC("vm-1")
	c := deriveMAC("vm-2")

	if a != b {
		t.Errorf("deriveMAC(vm-1) = %q then %q, want stable across calls", a, b)
	}
	if a == c {
		t.Errorf("deriveMAC(vm-1) == deriveMAC(vm-2) = %q, want different ids to differ", a)
	}

	var firstOctet int
	if _, err := fmt.Sscanf(a[:2], "%x", &firstOctet); err != nil {
		t.Fatalf("parsing first octet of %q: %v", a, err)
	}
	if firstOctet&0x01 != 0 {
		t.Errorf("deriveMAC(vm-1) = %q, first octet has the multicast bit set", a)
	}
	if firstOctet&0x02 == 0 {
		t.Errorf("deriveMAC(vm-1) = %q, first octet is missing the locally-administered bit", a)
	}
}

func TestFSM_ListNetworks_SortedByID(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-b", "b", "10.61.0.0/24"))})
	fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createNetworkCmd("net-a", "a", "10.60.0.0/24"))})

	got := fsm.ListNetworks()
	if len(got) != 2 || got[0].GetId() != "net-a" || got[1].GetId() != "net-b" {
		t.Errorf("ListNetworks() = %v, want [net-a, net-b] sorted", got)
	}
}

func TestFSM_SnapshotRestore_Networks(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})

	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	sink := &fakeSnapshotSink{}
	if err := snap.(*fsmSnapshot).Persist(sink); err != nil {
		t.Fatalf("Persist() error: %v", err)
	}

	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}

	network, ok := restored.Network("net-1")
	if !ok || network.GetSubnet() != "10.60.0.0/24" {
		t.Errorf("restored Network(net-1) = (%+v, %v), want present with subnet 10.60.0.0/24", network, ok)
	}
}

func createAPIKeyCmd(id, name, hashedKey string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_CreateApiKey{
			CreateApiKey: &internalpb.CreateAPIKey{Key: &internalpb.ApiKey{Id: id, Name: name, HashedKey: hashedKey, CreatedUnix: 1000}},
		},
	}
}

func TestFSM_Apply_CreateAPIKey(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createAPIKeyCmd("key-1", "terraform", "deadbeef"))})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	if applyResult.ApiKey.GetId() != "key-1" || applyResult.ApiKey.GetHashedKey() != "deadbeef" {
		t.Errorf("ApiKey = %+v, want id=key-1 hashed_key=deadbeef", applyResult.ApiKey)
	}
	if !fsm.AuthEnabled() {
		t.Errorf("AuthEnabled() = false, want true after a successful create")
	}
	id, _, valid := fsm.ValidateHash("deadbeef")
	if !valid || id != "key-1" {
		t.Errorf("ValidateHash(deadbeef) = (%q, %v), want (key-1, true)", id, valid)
	}
}

func TestFSM_Apply_CreateAPIKeyDuplicateRejected(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createAPIKeyCmd("key-1", "a", "hash-a"))})

	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createAPIKeyCmd("key-1", "b", "hash-b"))})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a duplicate-id rejection")
	}
}

func TestFSM_Apply_RevokeAPIKey(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createAPIKeyCmd("key-1", "terraform", "deadbeef"))})

	revokeCmd := &internalpb.Command{Op: &internalpb.Command_RevokeApiKey{RevokeApiKey: &internalpb.RevokeAPIKey{Id: "key-1"}}}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, revokeCmd)})

	if result.(*FSMApplyResult).Error != "" {
		t.Fatalf("Error = %q, want empty", result.(*FSMApplyResult).Error)
	}
	if _, _, valid := fsm.ValidateHash("deadbeef"); valid {
		t.Errorf("ValidateHash(deadbeef) = valid after revocation, want invalid")
	}
	if !fsm.AuthEnabled() {
		t.Errorf("AuthEnabled() = false after revoking the only key, want true (auth must stay locked down, not reopen)")
	}
}

func TestFSM_Apply_RevokeAPIKeyMissingIsError(t *testing.T) {
	fsm := NewFSM()

	revokeCmd := &internalpb.Command{Op: &internalpb.Command_RevokeApiKey{RevokeApiKey: &internalpb.RevokeAPIKey{Id: "missing"}}}
	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, revokeCmd)})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a not-found rejection")
	}
}

func TestFSM_AuthEnabled_FalseWhenEmpty(t *testing.T) {
	fsm := NewFSM()
	if fsm.AuthEnabled() {
		t.Errorf("AuthEnabled() = true on an empty FSM, want false")
	}
}

func TestFSM_ValidateHash_UnknownHashIsInvalid(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createAPIKeyCmd("key-1", "terraform", "deadbeef"))})

	if _, _, valid := fsm.ValidateHash("wrong-hash"); valid {
		t.Errorf("ValidateHash(wrong-hash) = valid, want invalid")
	}
}

func TestFSM_ListAPIKeys_SortedByID(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createAPIKeyCmd("key-b", "b", "hash-b"))})
	fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createAPIKeyCmd("key-a", "a", "hash-a"))})

	got := fsm.ListAPIKeys()
	if len(got) != 2 || got[0].GetId() != "key-a" || got[1].GetId() != "key-b" {
		t.Errorf("ListAPIKeys() = %v, want [key-a, key-b] sorted", got)
	}
}

func TestFSM_SnapshotRestore_APIKeys(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createAPIKeyCmd("key-1", "terraform", "deadbeef"))})

	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	sink := &fakeSnapshotSink{}
	if err := snap.(*fsmSnapshot).Persist(sink); err != nil {
		t.Fatalf("Persist() error: %v", err)
	}

	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}

	if id, _, valid := restored.ValidateHash("deadbeef"); !valid || id != "key-1" {
		t.Errorf("restored ValidateHash(deadbeef) = (%q, %v), want (key-1, true)", id, valid)
	}
	if !restored.AuthEnabled() {
		t.Errorf("restored AuthEnabled() = false, want true (auth_enabled must survive snapshot/restore)")
	}
}

func createJailCmd(id, name string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_CreateJail{
			CreateJail: &internalpb.CreateJail{Jail: &internalpb.JailDefinition{Id: id, Name: name}},
		},
	}
}

func TestFSM_Apply_CreateJail(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createJailCmd("jail-1", "web-1"))})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	if applyResult.Jail.GetId() != "jail-1" || applyResult.Jail.GetName() != "web-1" {
		t.Errorf("Jail = %+v, want id=jail-1 name=web-1", applyResult.Jail)
	}

	jail, ok := fsm.Jail("jail-1")
	if !ok || jail.GetName() != "web-1" {
		t.Errorf("Jail(jail-1) = (%+v, %v), want web-1 present", jail, ok)
	}
}

func TestFSM_Apply_CreateJailDuplicateRejected(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createJailCmd("jail-1", "a"))})

	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createJailCmd("jail-1", "b"))})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a duplicate-id rejection")
	}
}

func TestFSM_Apply_CreateJailMissingIDRejected(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createJailCmd("", "a"))})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a missing-id rejection")
	}
}

// TestFSM_Apply_CreateJailInvalidIDRejected mirrors
// TestFSM_Apply_CreateVMInvalidIDRejected - a jail id is interpolated
// as a hast.conf resource name the same way a VM id is.
func TestFSM_Apply_CreateJailInvalidIDRejected(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createJailCmd("jail-1\nresource evil {}", "a"))})
	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a rejection for a newline in the id")
	}
}

func TestFSM_Apply_UpdateJail(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createJailCmd("jail-1", "a"))})

	updateCmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateJail{UpdateJail: &internalpb.UpdateJail{
			Jail: &internalpb.JailDefinition{Id: "jail-1", Name: "b"},
		}},
	}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, updateCmd)})

	if result.(*FSMApplyResult).Error != "" {
		t.Fatalf("Error = %q, want empty", result.(*FSMApplyResult).Error)
	}
	jail, _ := fsm.Jail("jail-1")
	if jail.GetName() != "b" {
		t.Errorf("Jail(jail-1).Name = %q, want b", jail.GetName())
	}
}

func TestFSM_Apply_UpdateJailMissingIsError(t *testing.T) {
	fsm := NewFSM()

	updateCmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateJail{UpdateJail: &internalpb.UpdateJail{
			Jail: &internalpb.JailDefinition{Id: "missing"},
		}},
	}
	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, updateCmd)})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a not-found rejection")
	}
}

func TestFSM_Apply_DeleteJail(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createJailCmd("jail-1", "a"))})

	deleteCmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteJail{DeleteJail: &internalpb.DeleteJail{Id: "jail-1"}},
	}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, deleteCmd)})

	if result.(*FSMApplyResult).Error != "" {
		t.Fatalf("Error = %q, want empty", result.(*FSMApplyResult).Error)
	}
	if _, ok := fsm.Jail("jail-1"); ok {
		t.Errorf("Jail(jail-1) still present after DeleteJail")
	}
}

func TestFSM_Apply_SetJailHostname_TouchesOnlyThatField(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_CreateJail{CreateJail: &internalpb.CreateJail{
			Jail: &internalpb.JailDefinition{Id: "jail-1", NodeId: "node-a", Hostname: "old.example.com", ReplicaNodeId: "node-b"},
		}},
	})})

	cmd := &internalpb.Command{
		Op: &internalpb.Command_SetJailHostname{SetJailHostname: &internalpb.SetJailHostname{
			Id: "jail-1", Hostname: "new.example.com",
		}},
	}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, cmd)})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	jail, _ := fsm.Jail("jail-1")
	if jail.GetHostname() != "new.example.com" {
		t.Errorf("Hostname = %q, want new.example.com", jail.GetHostname())
	}
	if jail.GetNodeId() != "node-a" {
		t.Errorf("NodeId = %q, want node-a (must survive untouched)", jail.GetNodeId())
	}
	if jail.GetReplicaNodeId() != "node-b" {
		t.Errorf("ReplicaNodeId = %q, want node-b (must survive untouched)", jail.GetReplicaNodeId())
	}
}

func TestFSM_Apply_SetJailHostname_MissingIDIsError(t *testing.T) {
	fsm := NewFSM()

	cmd := &internalpb.Command{
		Op: &internalpb.Command_SetJailHostname{SetJailHostname: &internalpb.SetJailHostname{
			Id: "jail-1", Hostname: "new.example.com",
		}},
	}
	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, cmd)})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a missing-id rejection")
	}
}

func TestFSM_Apply_DeleteJail_AssignedJailIsSoftDeleted(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_CreateJail{CreateJail: &internalpb.CreateJail{
			Jail: &internalpb.JailDefinition{Id: "jail-1", NodeId: "node-a"},
		}},
	})})

	deleteCmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteJail{DeleteJail: &internalpb.DeleteJail{Id: "jail-1"}},
	}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, deleteCmd)})

	if result.(*FSMApplyResult).Error != "" {
		t.Fatalf("Error = %q, want empty", result.(*FSMApplyResult).Error)
	}
	jail, ok := fsm.Jail("jail-1")
	if !ok {
		t.Fatalf("Jail(jail-1) not found, want it to still exist as a tombstone")
	}
	if jail.GetDesiredState() != internalpb.JailState_JAIL_STATE_DELETING {
		t.Errorf("DesiredState = %v, want JAIL_STATE_DELETING", jail.GetDesiredState())
	}
}

func TestFSM_Apply_DeleteJailMissingIsError(t *testing.T) {
	fsm := NewFSM()

	deleteCmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteJail{DeleteJail: &internalpb.DeleteJail{Id: "missing"}},
	}
	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, deleteCmd)})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a not-found rejection")
	}
}

func TestFSM_Apply_UpdateJailPhase(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createJailCmd("jail-1", "a"))})

	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateJailPhase{UpdateJailPhase: &internalpb.UpdateJailPhase{
			Id: "jail-1", Phase: internalpb.JailPhase_JAIL_PHASE_READY,
		}},
	}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, cmd)})

	if result.(*FSMApplyResult).Error != "" {
		t.Fatalf("Error = %q, want empty", result.(*FSMApplyResult).Error)
	}
	jail, _ := fsm.Jail("jail-1")
	if jail.GetPhase() != internalpb.JailPhase_JAIL_PHASE_READY {
		t.Errorf("Phase = %v, want JAIL_PHASE_READY", jail.GetPhase())
	}
}

func TestFSM_Apply_UpdateJailPhaseMissingIsError(t *testing.T) {
	fsm := NewFSM()

	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateJailPhase{UpdateJailPhase: &internalpb.UpdateJailPhase{
			Id: "missing", Phase: internalpb.JailPhase_JAIL_PHASE_READY,
		}},
	}
	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, cmd)})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a not-found rejection")
	}
}

func TestFSM_Apply_PurgeJail(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_CreateJail{CreateJail: &internalpb.CreateJail{
			Jail: &internalpb.JailDefinition{Id: "jail-1", NodeId: "node-a"},
		}},
	})})
	fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_DeleteJail{DeleteJail: &internalpb.DeleteJail{Id: "jail-1"}},
	})})

	purgeCmd := &internalpb.Command{Op: &internalpb.Command_PurgeJail{PurgeJail: &internalpb.PurgeJail{Id: "jail-1"}}}
	result := fsm.Apply(&raft.Log{Index: 3, Data: mustMarshalCommand(t, purgeCmd)})

	if result.(*FSMApplyResult).Error != "" {
		t.Fatalf("Error = %q, want empty", result.(*FSMApplyResult).Error)
	}
	if _, ok := fsm.Jail("jail-1"); ok {
		t.Errorf("Jail(jail-1) still present after PurgeJail")
	}
}

func TestFSM_Apply_PurgeJailAlreadyGoneIsIdempotent(t *testing.T) {
	fsm := NewFSM()

	purgeCmd := &internalpb.Command{Op: &internalpb.Command_PurgeJail{PurgeJail: &internalpb.PurgeJail{Id: "missing"}}}
	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, purgeCmd)})

	if result.(*FSMApplyResult).Error != "" {
		t.Fatalf("Error = %q, want empty (purging an already-gone id is not an error)", result.(*FSMApplyResult).Error)
	}
}

func TestFSM_SnapshotRestore_Jails(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createJailCmd("jail-1", "web-1"))})

	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	sink := &fakeSnapshotSink{}
	if err := snap.(*fsmSnapshot).Persist(sink); err != nil {
		t.Fatalf("Persist() error: %v", err)
	}

	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}

	jail, ok := restored.Jail("jail-1")
	if !ok || jail.GetName() != "web-1" {
		t.Errorf("restored Jail(jail-1) = (%+v, %v), want present with name web-1", jail, ok)
	}
}

func createPendingJoinRequestCmd(requestID, nodeID, raftBindAddress, code string, expiresAtUnix int64) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_CreatePendingJoinRequest{
			CreatePendingJoinRequest: &internalpb.CreatePendingJoinRequest{
				Request: &internalpb.PendingJoinRequest{
					RequestId:       requestID,
					NodeId:          nodeID,
					RaftBindAddress: raftBindAddress,
					Code:            code,
					ExpiresAtUnix:   expiresAtUnix,
					Status:          internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING,
				},
			},
		},
	}
}

func approvePendingJoinRequestCmd(requestID string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_ApprovePendingJoinRequest{
			ApprovePendingJoinRequest: &internalpb.ApprovePendingJoinRequest{RequestId: requestID},
		},
	}
}

func rejectPendingJoinRequestCmd(requestID string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_RejectPendingJoinRequest{
			RejectPendingJoinRequest: &internalpb.RejectPendingJoinRequest{RequestId: requestID},
		},
	}
}

func TestFSM_Apply_CreatePendingJoinRequest(t *testing.T) {
	fsm := NewFSM()
	expires := time.Now().Add(15 * time.Minute).Unix()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createPendingJoinRequestCmd("jreq-1", "node02", "10.62.0.5:17600", "482913", expires))})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	if applyResult.PendingJoinRequest.GetCode() != "482913" {
		t.Errorf("PendingJoinRequest.Code = %q, want 482913", applyResult.PendingJoinRequest.GetCode())
	}

	req, ok := fsm.PendingJoinRequest("jreq-1")
	if !ok || req.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING {
		t.Errorf("PendingJoinRequest(jreq-1) = (%+v, %v), want Pending", req, ok)
	}
}

func TestFSM_Apply_CreatePendingJoinRequestDuplicateRejected(t *testing.T) {
	fsm := NewFSM()
	expires := time.Now().Add(15 * time.Minute).Unix()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createPendingJoinRequestCmd("jreq-1", "node02", "10.62.0.5:17600", "482913", expires))})

	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createPendingJoinRequestCmd("jreq-1", "node03", "10.62.0.6:17600", "111111", expires))})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a duplicate-request_id rejection")
	}
}

func TestFSM_Apply_CreatePendingJoinRequestMissingFieldsRejected(t *testing.T) {
	fsm := NewFSM()
	expires := time.Now().Add(15 * time.Minute).Unix()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createPendingJoinRequestCmd("", "node02", "10.62.0.5:17600", "482913", expires))})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a missing-request_id rejection")
	}
}

func TestFSM_Apply_ApprovePendingJoinRequest(t *testing.T) {
	fsm := NewFSM()
	expires := time.Now().Add(15 * time.Minute).Unix()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createPendingJoinRequestCmd("jreq-1", "node02", "10.62.0.5:17600", "482913", expires))})

	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, approvePendingJoinRequestCmd("jreq-1"))})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	if applyResult.PendingJoinRequest.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED {
		t.Errorf("Status = %v, want Approved", applyResult.PendingJoinRequest.GetStatus())
	}

	// Approved requests are excluded from the actionable list...
	if list := fsm.ListPendingJoinRequests(); len(list) != 0 {
		t.Errorf("ListPendingJoinRequests() = %v, want empty once approved", list)
	}
	// ...but the record itself is retained, not deleted, so a joining
	// Comb's own poll can still observe the terminal outcome.
	req, ok := fsm.PendingJoinRequest("jreq-1")
	if !ok || req.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED {
		t.Errorf("PendingJoinRequest(jreq-1) = (%+v, %v), want retained as Approved", req, ok)
	}
}

func TestFSM_Apply_RejectPendingJoinRequest(t *testing.T) {
	fsm := NewFSM()
	expires := time.Now().Add(15 * time.Minute).Unix()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createPendingJoinRequestCmd("jreq-1", "node02", "10.62.0.5:17600", "482913", expires))})

	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, rejectPendingJoinRequestCmd("jreq-1"))})

	if result.(*FSMApplyResult).PendingJoinRequest.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_REJECTED {
		t.Errorf("Status = %v, want Rejected", result.(*FSMApplyResult).PendingJoinRequest.GetStatus())
	}
}

// TestFSM_Apply_ApprovePendingJoinRequestTwiceRejected is the direct
// regression test for a stale-browser-tab double-click: approving (or
// rejecting) an already-resolved request must fail loudly, not
// silently no-op or double-apply.
func TestFSM_Apply_ApprovePendingJoinRequestTwiceRejected(t *testing.T) {
	fsm := NewFSM()
	expires := time.Now().Add(15 * time.Minute).Unix()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createPendingJoinRequestCmd("jreq-1", "node02", "10.62.0.5:17600", "482913", expires))})
	fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, approvePendingJoinRequestCmd("jreq-1"))})

	result := fsm.Apply(&raft.Log{Index: 3, Data: mustMarshalCommand(t, approvePendingJoinRequestCmd("jreq-1"))})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a rejection for approving an already-resolved request")
	}
}

func TestFSM_Apply_ApprovePendingJoinRequestMissingIsError(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, approvePendingJoinRequestCmd("no-such-request"))})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a missing-request rejection")
	}
}

// TestFSM_Apply_ApproveExpiredPendingJoinRequestRejected is the direct
// regression test for lazy expiry: an Admin approving a request that
// expired since it was listed must fail, not silently approve a stale
// join.
func TestFSM_Apply_ApproveExpiredPendingJoinRequestRejected(t *testing.T) {
	fsm := NewFSM()
	alreadyExpired := time.Now().Add(-1 * time.Minute).Unix()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createPendingJoinRequestCmd("jreq-1", "node02", "10.62.0.5:17600", "482913", alreadyExpired))})

	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, approvePendingJoinRequestCmd("jreq-1"))})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a rejection for approving an expired request")
	}
}

// TestFSM_ListPendingJoinRequestsExcludesExpired is the direct
// regression test for ListPendingJoinRequests's own lazy-expiry filter -
// an expired request must not appear in the actionable list an Admin
// reviews, even though (like Approved/Rejected ones) it's never deleted.
func TestFSM_ListPendingJoinRequestsExcludesExpired(t *testing.T) {
	fsm := NewFSM()
	alreadyExpired := time.Now().Add(-1 * time.Minute).Unix()
	notExpired := time.Now().Add(15 * time.Minute).Unix()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createPendingJoinRequestCmd("jreq-old", "node02", "10.62.0.5:17600", "111111", alreadyExpired))})
	fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createPendingJoinRequestCmd("jreq-new", "node03", "10.62.0.6:17600", "222222", notExpired))})

	list := fsm.ListPendingJoinRequests()

	if len(list) != 1 || list[0].GetRequestId() != "jreq-new" {
		t.Errorf("ListPendingJoinRequests() = %v, want only jreq-new", list)
	}
}

func TestFSM_SnapshotRestore_PendingJoinRequests(t *testing.T) {
	fsm := NewFSM()
	expires := time.Now().Add(15 * time.Minute).Unix()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createPendingJoinRequestCmd("jreq-1", "node02", "10.62.0.5:17600", "482913", expires))})

	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	sink := &fakeSnapshotSink{}
	if err := snap.(*fsmSnapshot).Persist(sink); err != nil {
		t.Fatalf("Persist() error: %v", err)
	}

	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}

	req, ok := restored.PendingJoinRequest("jreq-1")
	if !ok || req.GetCode() != "482913" {
		t.Errorf("restored PendingJoinRequest(jreq-1) = (%+v, %v), want present with code 482913", req, ok)
	}
}
