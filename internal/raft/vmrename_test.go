package raft

import (
	"strings"
	"testing"

	"github.com/hashicorp/raft"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// ADR-0152's atomic VM identity rename. What matters here is not that
// a key moved, it is what did NOT happen: no deletion tombstone, no
// purge, no teardown, no field lost, and no second VM. Every refusal
// path is checked for leaving state exactly as it was.

// renameCmd builds the dedicated rename command.
func renameCmd(sourceID, destinationID string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_RenameVmIdentity{
			RenameVmIdentity: &internalpb.RenameVMIdentity{
				SourceId:      sourceID,
				DestinationId: destinationID,
			},
		},
	}
}

// applyRename applies a rename and fails the test on any error.
func applyRename(t *testing.T, fsm *FSM, index uint64, sourceID, destinationID string) *FSMApplyResult {
	t.Helper()
	res := fsm.Apply(&raft.Log{Index: index, Data: mustMarshalCommand(t, renameCmd(sourceID, destinationID))}).(*FSMApplyResult)
	if res.Error != "" {
		t.Fatalf("RenameVMIdentity(%q -> %q) at index %d failed unexpectedly: %s", sourceID, destinationID, index, res.Error)
	}
	return res
}

// applyRenameExpectingRefusal applies a rename that must be refused and
// returns the error text.
func applyRenameExpectingRefusal(t *testing.T, fsm *FSM, index uint64, sourceID, destinationID string) string {
	t.Helper()
	res := fsm.Apply(&raft.Log{Index: index, Data: mustMarshalCommand(t, renameCmd(sourceID, destinationID))}).(*FSMApplyResult)
	if res.Error == "" {
		t.Fatalf("RenameVMIdentity(%q -> %q) at index %d was accepted, want a refusal", sourceID, destinationID, index)
	}
	if !strings.Contains(res.Error, "RenameVMIdentity") {
		t.Errorf("refusal does not name the command it refuses: %q", res.Error)
	}
	return res.Error
}

// seedNetwork creates the network a seeded VM names, so the VM's own
// creation is not refused for a missing network.
//
// It is idempotent per FSM: seeding a second VM into the same FSM must
// not fail with "network already exists", and the network's index must
// not collide with the caller's own index sequence.
func seedNetwork(t *testing.T, fsm *FSM) {
	t.Helper()
	if _, exists := fsm.networks["net-1"]; exists {
		return
	}
	res := fsm.Apply(&raft.Log{Index: 900, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_CreateNetwork{
			CreateNetwork: &internalpb.CreateNetwork{Network: &internalpb.NetworkDefinition{
				Id: "net-1", Name: "net-1", Subnet: "10.0.0.0/24", VlanId: 10,
			}},
		},
	})}).(*FSMApplyResult)
	if res.Error != "" {
		t.Fatalf("seeding the network failed unexpectedly: %s", res.Error)
	}
}

// seedVM creates a fully-populated VM, so a rename can be checked for
// preserving every field rather than just an id. The network it names
// is created first, because CreateVM refuses a VM naming a network that
// does not exist.
func seedVM(t *testing.T, fsm *FSM, index uint64, id string) *internalpb.VMDefinition {
	t.Helper()
	seedNetwork(t, fsm)
	vm := &internalpb.VMDefinition{
		Id:                 id,
		Name:               "web-" + id,
		Vcpus:              4,
		MemoryMb:           4096,
		NodeId:             "node-a",
		DesiredState:       internalpb.VMState_VM_STATE_RUNNING,
		Phase:              internalpb.VMPhase_VM_PHASE_READY,
		IsoName:            "installer.iso",
		NetworkId:          "net-1",
		IpAddress:          "10.0.0.5",
		MacAddress:         "02:00:00:00:00:05",
		ReplicaNodeId:      "node-b",
		BaseImageName:      "base.img",
		CloudflareHostname: "vm.example.com",
		CloudflarePort:     8080,
		CloneFromSnapshot:  "",
		FirewallPaused:     true,
		DiskSizeMb:         30720,
		FirewallRules: []*internalpb.FirewallRule{
			{Direction: "in", Action: "allow", Protocol: "tcp", PortRange: "443", Priority: 10},
			{Direction: "out", Action: "block", Protocol: "udp", PortRange: "53", Priority: 5},
		},
	}
	res := fsm.Apply(&raft.Log{Index: index, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_CreateVm{CreateVm: &internalpb.CreateVM{Vm: vm}},
	})}).(*FSMApplyResult)
	if res.Error != "" {
		t.Fatalf("seeding VM %q failed unexpectedly: %s", id, res.Error)
	}
	return res.VM
}

func TestCreateVMAssignsResourceIDFromItsOwnID(t *testing.T) {
	fsm := NewFSM()

	vm := seedVM(t, fsm, 1, "vm-1")

	if got := vm.GetResourceId(); got != "vm-1" {
		t.Errorf("resource_id = %q, want vm-1: a new VM's physical identity is its own initial logical id", got)
	}
}

func TestRenameVMIdentityMovesTheKeyAndPreservesEveryField(t *testing.T) {
	fsm := NewFSM()
	before := seedVM(t, fsm, 1, "legacy-id")

	res := applyRename(t, fsm, 2, "legacy-id", "uid-scoped-id")

	if got := res.RenameResult.GetOutcome(); got != RenameOutcomeRenamed {
		t.Errorf("outcome = %q, want %q", got, RenameOutcomeRenamed)
	}

	// The source key must be GONE and the destination key present: one
	// logical VM, not two.
	if _, found := fsm.VM("legacy-id"); found {
		t.Error("the source key is still present after the rename; exactly one logical VM must remain")
	}
	after, found := fsm.VM("uid-scoped-id")
	if !found {
		t.Fatal("the destination key is missing after the rename")
	}

	// Every field except the id must be preserved, including the
	// physical identity, the address pair, the firewall rules and the
	// per-VM disk size.
	if after.GetId() != "uid-scoped-id" {
		t.Errorf("id = %q, want uid-scoped-id", after.GetId())
	}
	if after.GetResourceId() != before.GetResourceId() {
		t.Errorf("resource_id = %q, want %q: a rename must not change the physical identity", after.GetResourceId(), before.GetResourceId())
	}
	if after.GetName() != before.GetName() {
		t.Errorf("name = %q, want %q", after.GetName(), before.GetName())
	}
	if after.GetVcpus() != before.GetVcpus() {
		t.Errorf("vcpus = %d, want %d", after.GetVcpus(), before.GetVcpus())
	}
	if after.GetMemoryMb() != before.GetMemoryMb() {
		t.Errorf("memory_mb = %d, want %d", after.GetMemoryMb(), before.GetMemoryMb())
	}
	if after.GetNodeId() != before.GetNodeId() {
		t.Errorf("node_id = %q, want %q", after.GetNodeId(), before.GetNodeId())
	}
	if after.GetReplicaNodeId() != before.GetReplicaNodeId() {
		t.Errorf("replica_node_id = %q, want %q", after.GetReplicaNodeId(), before.GetReplicaNodeId())
	}
	if after.GetDesiredState() != before.GetDesiredState() {
		t.Errorf("desired_state = %v, want %v", after.GetDesiredState(), before.GetDesiredState())
	}
	if after.GetPhase() != before.GetPhase() {
		t.Errorf("phase = %v, want %v", after.GetPhase(), before.GetPhase())
	}
	if after.GetPhaseError() != before.GetPhaseError() {
		t.Errorf("phase_error = %q, want %q", after.GetPhaseError(), before.GetPhaseError())
	}
	if after.GetIpAddress() != before.GetIpAddress() {
		t.Errorf("ip_address = %q, want %q", after.GetIpAddress(), before.GetIpAddress())
	}
	if after.GetMacAddress() != before.GetMacAddress() {
		t.Errorf("mac_address = %q, want %q", after.GetMacAddress(), before.GetMacAddress())
	}
	if after.GetIsoName() != before.GetIsoName() {
		t.Errorf("iso_name = %q, want %q", after.GetIsoName(), before.GetIsoName())
	}
	if after.GetNetworkId() != before.GetNetworkId() {
		t.Errorf("network_id = %q, want %q", after.GetNetworkId(), before.GetNetworkId())
	}
	if after.GetBaseImageName() != before.GetBaseImageName() {
		t.Errorf("base_image_name = %q, want %q", after.GetBaseImageName(), before.GetBaseImageName())
	}
	if after.GetCloudflareHostname() != before.GetCloudflareHostname() {
		t.Errorf("cloudflare_hostname = %q, want %q", after.GetCloudflareHostname(), before.GetCloudflareHostname())
	}
	if after.GetCloudflarePort() != before.GetCloudflarePort() {
		t.Errorf("cloudflare_port = %d, want %d", after.GetCloudflarePort(), before.GetCloudflarePort())
	}
	if after.GetFirewallPaused() != before.GetFirewallPaused() {
		t.Errorf("firewall_paused = %v, want %v", after.GetFirewallPaused(), before.GetFirewallPaused())
	}
	if after.GetDiskSizeMb() != before.GetDiskSizeMb() {
		t.Errorf("disk_size_mb = %d, want %d", after.GetDiskSizeMb(), before.GetDiskSizeMb())
	}
	if len(after.GetFirewallRules()) != len(before.GetFirewallRules()) {
		t.Fatalf("firewall_rules count = %d, want %d", len(after.GetFirewallRules()), len(before.GetFirewallRules()))
	}
	for i, rule := range before.GetFirewallRules() {
		if after.GetFirewallRules()[i].GetPortRange() != rule.GetPortRange() || after.GetFirewallRules()[i].GetPriority() != rule.GetPriority() {
			t.Errorf("firewall_rules[%d] = %+v, want %+v", i, after.GetFirewallRules()[i], rule)
		}
	}
}

func TestRenameVMIdentityDoesNotSetDeletingStateOrPurge(t *testing.T) {
	fsm := NewFSM()
	seedVM(t, fsm, 1, "legacy-id")

	res := applyRename(t, fsm, 2, "legacy-id", "uid-scoped-id")

	// No deletion tombstone: the renamed VM is still the same running
	// VM, not a soft-deleted one.
	if res.VM.GetDesiredState() == internalpb.VMState_VM_STATE_DELETING {
		t.Error("the rename set VM_STATE_DELETING; a rename must not enter the deletion lifecycle")
	}
	if res.VM.GetPhase() == internalpb.VMPhase_VM_PHASE_DELETING {
		t.Error("the rename set VM_PHASE_DELETING; a rename must not enter the deletion lifecycle")
	}
	// No purge: the record is still there under its new key.
	if _, found := fsm.VM("uid-scoped-id"); !found {
		t.Error("the renamed record is missing; a rename must not purge the record")
	}
	// And no teardown: the total number of VM records is unchanged.
	if got := len(fsm.ListVMs()); got != 1 {
		t.Errorf("VM count = %d, want 1: a rename must leave exactly one logical VM", got)
	}
}

func TestRenameVMIdentityRefusesInvalidIDs(t *testing.T) {
	cases := []struct {
		name        string
		source      string
		destination string
	}{
		{"empty source", "", "new-id"},
		{"empty destination", "legacy-id", ""},
		{"source with a slash", "legacy/id", "new-id"},
		{"destination with a space", "legacy-id", "new id"},
		{"destination with a newline", "legacy-id", "new\nid"},
		{"destination too long", "legacy-id", strings.Repeat("a", 65)},
		{"source equals destination", "legacy-id", "legacy-id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsm := NewFSM()
			seedVM(t, fsm, 1, "legacy-id")

			applyRenameExpectingRefusal(t, fsm, 2, tc.source, tc.destination)

			// The refusal changed nothing.
			if _, found := fsm.VM("legacy-id"); !found {
				t.Error("the source record is gone after a refused rename")
			}
		})
	}
}

func TestRenameVMIdentityRefusesAMissingSource(t *testing.T) {
	fsm := NewFSM()

	err := applyRenameExpectingRefusal(t, fsm, 1, "no-such-vm", "new-id")

	if !strings.Contains(err, "does not exist") {
		t.Errorf("refusal = %q, want it to say the source does not exist", err)
	}
}

func TestRenameVMIdentityRefusesADestinationCollisionWithoutMutating(t *testing.T) {
	fsm := NewFSM()
	seedVM(t, fsm, 1, "legacy-id")
	existing := seedVM(t, fsm, 2, "other-id")

	err := applyRenameExpectingRefusal(t, fsm, 3, "legacy-id", "other-id")

	if !strings.Contains(err, "already in use") {
		t.Errorf("refusal = %q, want it to say the destination is already in use", err)
	}
	// Neither record changed.
	source, found := fsm.VM("legacy-id")
	if !found {
		t.Fatal("the source record is gone after a refused collision")
	}
	if source.GetId() != "legacy-id" {
		t.Errorf("source id = %q, want legacy-id", source.GetId())
	}
	destination, found := fsm.VM("other-id")
	if !found {
		t.Fatal("the destination record is gone after a refused collision")
	}
	if destination.GetName() != existing.GetName() || destination.GetNodeId() != existing.GetNodeId() {
		t.Errorf("the destination record was modified by a refused collision: %+v", destination)
	}
	if got := len(fsm.ListVMs()); got != 2 {
		t.Errorf("VM count = %d, want 2: a refused collision must not remove either record", got)
	}
}

func TestRenameVMIdentityRefusesAnOwnershipMismatchWithoutMutating(t *testing.T) {
	fsm := NewFSM()
	seedVM(t, fsm, 1, "legacy-id")

	res := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_RenameVmIdentity{
			RenameVmIdentity: &internalpb.RenameVMIdentity{
				SourceId:       "legacy-id",
				DestinationId:  "uid-scoped-id",
				ExpectedNodeId: "node-z",
			},
		},
	})}).(*FSMApplyResult)
	if res.Error == "" {
		t.Fatal("a rename with a mismatched expected owner was accepted, want a refusal")
	}
	if !strings.Contains(res.Error, "owned by node") {
		t.Errorf("refusal = %q, want it to say who actually owns the VM", res.Error)
	}
	if _, found := fsm.VM("legacy-id"); !found {
		t.Error("the source record is gone after a refused ownership mismatch")
	}
	if _, found := fsm.VM("uid-scoped-id"); found {
		t.Error("the destination record exists after a refused ownership mismatch")
	}
}

func TestRenameVMIdentityRefusesAReplicaMismatch(t *testing.T) {
	fsm := NewFSM()
	seedVM(t, fsm, 1, "legacy-id")

	res := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_RenameVmIdentity{
			RenameVmIdentity: &internalpb.RenameVMIdentity{
				SourceId:              "legacy-id",
				DestinationId:         "uid-scoped-id",
				ExpectedReplicaNodeId: "node-z",
			},
		},
	})}).(*FSMApplyResult)
	if res.Error == "" {
		t.Fatal("a rename with a mismatched expected replica was accepted, want a refusal")
	}
	if !strings.Contains(res.Error, "replica node") {
		t.Errorf("refusal = %q, want it to name the replica mismatch", res.Error)
	}
}

func TestRenameVMIdentityRefusesADeletingVM(t *testing.T) {
	fsm := NewFSM()
	seedVM(t, fsm, 1, "legacy-id")
	fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_DeleteVm{DeleteVm: &internalpb.DeleteVM{Id: "legacy-id"}},
	})})

	err := applyRenameExpectingRefusal(t, fsm, 3, "legacy-id", "uid-scoped-id")

	if !strings.Contains(err, "marked for deletion") {
		t.Errorf("refusal = %q, want it to say the VM is being deleted", err)
	}
}

func TestRenameVMIdentityIsIdempotentForACompletedRename(t *testing.T) {
	fsm := NewFSM()
	seedVM(t, fsm, 1, "legacy-id")
	applyRename(t, fsm, 2, "legacy-id", "uid-scoped-id")

	// The retry: the source is gone and the destination exists and
	// PROVES the relationship (its physical identity is the source's
	// original id).
	res := applyRename(t, fsm, 3, "legacy-id", "uid-scoped-id")

	if got := res.RenameResult.GetOutcome(); got != RenameOutcomeAlreadyRenamed {
		t.Errorf("outcome = %q, want %q", got, RenameOutcomeAlreadyRenamed)
	}
	if res.VM.GetId() != "uid-scoped-id" {
		t.Errorf("vm.id = %q, want uid-scoped-id", res.VM.GetId())
	}
	if got := len(fsm.ListVMs()); got != 1 {
		t.Errorf("VM count = %d, want 1: a retry must not create a second VM", got)
	}
}

func TestRenameVMIdentityRejectsANonMatchingDestinationAsACollision(t *testing.T) {
	fsm := NewFSM()
	seedVM(t, fsm, 1, "legacy-id")
	// An UNRELATED VM already occupies the destination. Its name
	// matches, but its physical identity does not, so it is a
	// collision and not proof of a completed rename.
	seedVM(t, fsm, 2, "uid-scoped-id")

	err := applyRenameExpectingRefusal(t, fsm, 3, "legacy-id", "uid-scoped-id")

	if !strings.Contains(err, "already in use") {
		t.Errorf("refusal = %q, want a destination-collision error", err)
	}
	// And the unrelated VM is untouched.
	destination, found := fsm.VM("uid-scoped-id")
	if !found {
		t.Fatal("the destination record is gone")
	}
	if destination.GetResourceId() != "uid-scoped-id" {
		t.Errorf("the destination record was modified by a refused collision: resource_id = %q", destination.GetResourceId())
	}
}

func TestRenameVMIdentityRefusesWhenNeitherSourceNorDestinationExists(t *testing.T) {
	fsm := NewFSM()

	err := applyRenameExpectingRefusal(t, fsm, 1, "legacy-id", "uid-scoped-id")

	if !strings.Contains(err, "does not exist") {
		t.Errorf("refusal = %q, want a missing-source error", err)
	}
}

func TestRenameVMIdentityMaterializesTheLegacyResourceID(t *testing.T) {
	fsm := NewFSM()
	// A record written BEFORE resource_id existed: the field is empty
	// and the host resources are named for the id.
	res := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_CreateVm{
			CreateVm: &internalpb.CreateVM{Vm: &internalpb.VMDefinition{Id: "legacy-id", Name: "legacy"}},
		},
	})}).(*FSMApplyResult)
	if res.Error != "" {
		t.Fatalf("seeding the legacy VM failed: %s", res.Error)
	}
	// Simulate the legacy state by clearing the field the way an old
	// log entry would have.
	fsm.vms["legacy-id"] = &internalpb.VMDefinition{Id: "legacy-id", Name: "legacy"}

	applyRename(t, fsm, 2, "legacy-id", "uid-scoped-id")

	renamed, found := fsm.VM("uid-scoped-id")
	if !found {
		t.Fatal("the renamed record is missing")
	}
	// The legacy fallback is materialized: the physical identity
	// becomes the ORIGINAL id, so a later logical rename can never
	// change the physical name.
	if got := renamed.GetResourceId(); got != "legacy-id" {
		t.Errorf("resource_id = %q, want legacy-id: the legacy fallback must be materialized during the rename", got)
	}
}

func TestUpdateVMKeepsTheResourceIDImmutable(t *testing.T) {
	fsm := NewFSM()
	seedVM(t, fsm, 1, "legacy-id")

	// A full-record replace that tries to point the VM at a different
	// physical identity.
	res := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_UpdateVm{
			UpdateVm: &internalpb.UpdateVM{Vm: &internalpb.VMDefinition{
				Id:         "legacy-id",
				Name:       "renamed-by-hand",
				NetworkId:  "net-1",
				ResourceId: "something-else",
			}},
		},
	})}).(*FSMApplyResult)
	if res.Error != "" {
		t.Fatalf("UpdateVM failed unexpectedly: %s", res.Error)
	}

	if got := res.VM.GetResourceId(); got != "legacy-id" {
		t.Errorf("resource_id = %q, want legacy-id: an ordinary update must never change the physical identity", got)
	}
}

func TestCreateVMRefusesAnIDAlreadyUsedAsAnotherVMsPhysicalIdentity(t *testing.T) {
	fsm := NewFSM()
	seedVM(t, fsm, 1, "legacy-id")
	applyRename(t, fsm, 2, "legacy-id", "uid-scoped-id")

	// The logical key "legacy-id" is free, but its PHYSICAL name is
	// still in use by the renamed VM's host resources. Creating a
	// second VM under it would give the two VMs the same physical
	// identity, and they would address (and eventually destroy) each
	// other's resources.
	res := fsm.Apply(&raft.Log{Index: 3, Data: mustMarshalCommand(t, createVMCmd("legacy-id", "second"))}).(*FSMApplyResult)
	if res.Error == "" {
		t.Fatal("a VM was created under an id already used as another VM's physical identity, want a refusal")
	}
	if !strings.Contains(res.Error, "physical identity") {
		t.Errorf("refusal = %q, want it to name the physical-identity conflict", res.Error)
	}
	if got := len(fsm.ListVMs()); got != 1 {
		t.Errorf("VM count = %d, want 1", got)
	}
}

func TestRenameVMIdentitySurvivesSnapshotAndRestore(t *testing.T) {
	fsm := NewFSM()
	seedVM(t, fsm, 1, "legacy-id")
	applyRename(t, fsm, 2, "legacy-id", "uid-scoped-id")

	snapshot, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	restored := NewFSM()
	if err := restored.Restore(snapshotReader(t, snapshot)); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}

	renamed, found := restored.VM("uid-scoped-id")
	if !found {
		t.Fatal("the renamed record is missing after a snapshot/restore")
	}
	if got := renamed.GetResourceId(); got != "legacy-id" {
		t.Errorf("resource_id = %q, want legacy-id: the physical identity must survive a restore", got)
	}
}
