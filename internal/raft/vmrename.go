package raft

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// Rename outcomes, reported back to the caller so a completed retry is
// distinguishable from a rename this call actually applied.
//
// "renamed" means the FSM moved the record in this apply. "already_renamed"
// means the rename had already been applied and this call PROVED it (the
// destination record carries the physical identity of the requested
// source). There is deliberately no third "success" value: a caller that
// cannot tell those two apart cannot implement ADR-0152's retry contract
// (a caller whose first attempt timed out must be able to re-resolve
// leadership and retry the same source/destination pair without creating
// a second VM).
const (
	RenameOutcomeRenamed        = "renamed"
	RenameOutcomeAlreadyRenamed = "already_renamed"
)

// physicalIDTakenBy reports whether some other VM already holds
// physicalID as its physical identity.
//
// This is the check that keeps the rename's own invariant honest: once
// a VM has been renamed AWAY from an id, that id is free as a LOGICAL
// id, but it is NOT free as a PHYSICAL one - the renamed VM's host
// resources still carry it, and they will for as long as that VM
// exists. A new VM created under that same id would be assigned the
// same resource_id, and would then address (and eventually destroy)
// the FIRST VM's dataset, bhyve VM, HAST resource and pf anchor.
//
// The check is by physical identity, not by logical key, because the
// two are exactly what can drift apart here: the logical key "old-id"
// is gone from the map, while the physical name "old-id" is very much
// still in use.
func (f *FSM) physicalIDTakenBy(physicalID, exceptLogicalID string) bool {
	for _, vm := range f.vms {
		if vm.GetId() == exceptLogicalID {
			continue
		}
		if effectiveResourceID(vm) == physicalID {
			return true
		}
	}
	return false
}

// effectiveResourceID returns a VM's PHYSICAL identity: its persisted
// resource_id when it has one, and its own logical id otherwise.
//
// The fallback is the whole compatibility story of ADR-0152: a record
// written before the field existed has no resource_id, and its host
// resources were named for its id. Resolving the empty value to the id
// preserves every one of those names exactly, so nothing has to be
// renamed on disk and nothing has to be re-provisioned.
//
// The value is read-only: nothing here writes it back. It is
// MATERIALIZED (written into the record) by the rename apply, which is
// the one place the legacy fallback becomes permanent.
func effectiveResourceID(vm *internalpb.VMDefinition) string {
	if vm == nil {
		return ""
	}
	if id := vm.GetResourceId(); id != "" {
		return id
	}
	return vm.GetId()
}

// applyRenameVMIdentity is ADR-0152's atomic logical-identity rename.
//
// It moves a VM's record from one map key to another, changing ONLY
// VMDefinition.id, in a single apply. Every other field is preserved,
// resource_id is preserved (and materialized, if it was still empty),
// and no deletion tombstone, purge, or teardown is triggered.
//
// The order of the checks is the design's, exactly:
//
//  1. Validate both ids with the SAME resource-ID rules creation uses.
//     An empty, malformed, over-long, or equal pair is refused before
//     anything is touched.
//  2. Look up the source. If it is absent, the idempotent-retry rule
//     below decides whether this is an already-applied rename (success)
//     or a missing source (error).
//  3. Verify ownership and migration eligibility against the caller's
//     stated evidence. A mismatch is refused WITHOUT mutation. This is
//     re-checked here, in the FSM, rather than trusted from a managerd
//     pre-check: the FSM apply is the only place that runs in the same
//     order on every replica.
//  4. Check the destination. If another record already occupies it, the
//     request is refused and NEITHER record is changed. Never overwrite.
//  5. Clone the source, materialize its effective resource_id, change
//     only id, and atomically remove the source key and insert the
//     destination key.
//
// Every refusal path leaves state exactly as it was: the clone-and-swap
// happens only after every check has passed.
func (f *FSM) applyRenameVMIdentity(index uint64, req *internalpb.RenameVMIdentity) *FSMApplyResult {
	sourceID := req.GetSourceId()
	destinationID := req.GetDestinationId()

	// 1. Validate both ids with the existing resource-ID rules. These
	// are the same rules CreateVM applies, because both ids end up
	// naming host resources (the destination's id will name a dataset
	// and a bhyve VM on the next reconcile) and neither may carry a
	// character that would be unsafe to interpolate into a generated
	// configuration file.
	if sourceID == "" {
		return &FSMApplyResult{Index: index, Error: "RenameVMIdentity: source_id must be set"}
	}
	if destinationID == "" {
		return &FSMApplyResult{Index: index, Error: "RenameVMIdentity: destination_id must be set"}
	}
	if !validResourceID(sourceID) {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("RenameVMIdentity: invalid source_id %q: only alphanumerics, '-', and '_' are allowed (max 64 chars)", sourceID)}
	}
	if !validResourceID(destinationID) {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("RenameVMIdentity: invalid destination_id %q: only alphanumerics, '-', and '_' are allowed (max 64 chars)", destinationID)}
	}
	if sourceID == destinationID {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("RenameVMIdentity: source_id and destination_id are both %q; a rename must change the logical identity", sourceID)}
	}

	source, exists := f.vms[sourceID]
	if !exists {
		// 2. The source is gone. Either this rename was already applied
		// (the destination exists and PROVES it) or the source never
		// existed (or was deleted). A destination that merely shares the
		// requested NAME is never proof - see the idempotency check
		// below.
		return f.renameRetryOutcome(index, sourceID, destinationID, req)
	}

	// 3. Ownership and migration eligibility. The caller states what it
	// believes the record holds; the FSM compares against what the
	// record ACTUALLY holds. A mismatch is refused without mutation.
	if expected := req.GetExpectedNodeId(); expected != "" && expected != source.GetNodeId() {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"RenameVMIdentity: VM %q is owned by node %q, not %q as this request expected; refusing to rename it",
			sourceID, source.GetNodeId(), expected)}
	}
	if expected := req.GetExpectedReplicaNodeId(); expected != "" && expected != source.GetReplicaNodeId() {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"RenameVMIdentity: VM %q has replica node %q, not %q as this request expected; refusing to rename it",
			sourceID, source.GetReplicaNodeId(), expected)}
	}
	if expected := req.GetExpectedResourceId(); expected != "" && expected != effectiveResourceID(source) {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"RenameVMIdentity: VM %q has physical identity %q, not %q as this request expected; refusing to rename it",
			sourceID, effectiveResourceID(source), expected)}
	}
	// A VM marked for deletion is in the soft-delete lifecycle, where the
	// owning node is tearing its host resources down and will purge the
	// record. Renaming it would race that teardown: the reconciler
	// already holds the old id, and a renamed record would leave that
	// teardown pointing at a key that no longer exists.
	if source.GetDesiredState() == internalpb.VMState_VM_STATE_DELETING {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"RenameVMIdentity: VM %q is marked for deletion; its host resources are being torn down, so it cannot be renamed", sourceID)}
	}
	// A fenced guest (ADR-0128) is held by a migration record that
	// outlives the VM record itself. Renaming it would leave that
	// migration record pointing at an id that no longer exists.
	if source.GetMigrationFenced() {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"RenameVMIdentity: VM %q is fenced by an in-progress migration; it cannot be renamed until that migration settles", sourceID)}
	}

	// 4. Destination collision. Never overwrite another VM's record.
	if _, taken := f.vms[destinationID]; taken {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"RenameVMIdentity: destination id %q is already in use; refusing to overwrite it", destinationID)}
	}

	// 5. The move. Clone the source, materialize the effective
	// resource_id (the legacy fallback becomes permanent here, in the
	// same apply that changes the key - so a LATER logical rename can
	// never change the physical name), change ONLY id, and swap the
	// key.
	renamed := proto.Clone(source).(*internalpb.VMDefinition)
	renamed.ResourceId = effectiveResourceID(source)
	renamed.Id = destinationID
	delete(f.vms, sourceID)
	f.vms[destinationID] = renamed

	return &FSMApplyResult{Index: index, VM: renamed, RenameResult: &internalpb.RenameVMIdentityResult{
		Vm:      renamed,
		Outcome: RenameOutcomeRenamed,
	}}
}

// renameRetryOutcome implements ADR-0152's idempotency rule for the
// case where the source key is already gone.
//
// The rule has three outcomes, and only one of them is a success:
//
//   - The destination exists AND proves it is the completed result of
//     renaming THIS source: its logical id is the requested
//     destination, its physical identity (resource_id) matches the
//     requested legacy source identity or the supplied expected
//     resource_id, and its ownership evidence matches. Then the
//     rename is reported as "already_renamed" - a SUCCESS, so a
//     caller whose first attempt timed out can retry safely.
//
//   - The destination exists but does NOT prove that relationship: a
//     destination-collision error. A matching destination by NAME
//     alone is never proof, because an unrelated VM could have been
//     created under that id after this rename failed.
//
//   - Neither exists: a missing-source error.
//
// The third case is the common one for a genuine mistake (the caller
// named a VM that does not exist), and it is reported as an error
// rather than a silent success so a caller cannot mistake "there is
// nothing here" for "the rename is done".
func (f *FSM) renameRetryOutcome(index uint64, sourceID, destinationID string, req *internalpb.RenameVMIdentity) *FSMApplyResult {
	destination, exists := f.vms[destinationID]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"RenameVMIdentity: source VM %q does not exist, and no VM %q exists either", sourceID, destinationID)}
	}

	// Proof requires the destination's PHYSICAL identity to be the
	// source's. The expected_resource_id, when stated, is what the
	// caller believes the source's physical identity was. When it is
	// not stated, the legacy fallback applies: a source whose
	// resource_id was empty resolved to its own id, so a completed
	// rename of that source left the destination carrying the source's
	// id as its resource_id.
	expectedPhysical := req.GetExpectedResourceId()
	if expectedPhysical == "" {
		expectedPhysical = sourceID
	}
	if destination.GetResourceId() != expectedPhysical {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"RenameVMIdentity: destination id %q is already in use by a different VM (physical identity %q, not %q); refusing to overwrite it",
			destinationID, destination.GetResourceId(), expectedPhysical)}
	}

	// Ownership evidence must match too: a destination with the right
	// physical identity but a different owner is still not proof of
	// THIS rename.
	if expected := req.GetExpectedNodeId(); expected != "" && expected != destination.GetNodeId() {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"RenameVMIdentity: destination id %q is owned by node %q, not %q as this request expected; refusing to overwrite it",
			destinationID, destination.GetNodeId(), expected)}
	}
	if expected := req.GetExpectedReplicaNodeId(); expected != "" && expected != destination.GetReplicaNodeId() {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"RenameVMIdentity: destination id %q has replica node %q, not %q as this request expected; refusing to overwrite it",
			destinationID, destination.GetReplicaNodeId(), expected)}
	}

	return &FSMApplyResult{Index: index, VM: destination, RenameResult: &internalpb.RenameVMIdentityResult{
		Vm:      destination,
		Outcome: RenameOutcomeAlreadyRenamed,
	}}
}
