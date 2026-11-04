package raft

import (
	"fmt"

	"github.com/glenjbarber/apiary/api/internalpb"
	"google.golang.org/protobuf/proto"
)

// ADR-0191's enrollment half, in replicated state.
//
// WHY THIS IS IN RAFT AT ALL, when the enrollment store itself is local:
//
// The ENROLLMENT RECORD is local - it says which external hosts this
// Colony admits, and a provider credential is a per-host secret. But the
// AUTHORIZING ACTS are Colony-wide facts in exactly the way ADR-0147
// Part 3's are: two distinct API keys, presented to two distinct Colony
// members, authorizing the same thing.
//
// Those two acts have to be countable the same way on every replica and
// across a leadership change, or the count is a claim rather than a
// fact. That is the entire reason ADR-0147 Part 3 pins the record of
// use in replicated state rather than leaving it to a root-owned file
// that only the leader reads, and the same reasoning applies here.
//
// So: the two authorizations live in raft, and the resulting enrollment
// record lives in a local store on each member. A member that has not
// seen the resolution yet does not claim the host is enrolled, which is
// the same visible-lag the join flow has.
//
// WHAT THIS DOES NOT DO:
//
// It adds no voter. There is no AddVoter anywhere in this file, and an
// enrolled external node is not a member, holds no vote, and is not
// reachable over Apiary's raft transport. A Proxmox node is a Linux
// host running QEMU/KVM and an Apiary Comb is a FreeBSD host running
// bhyve; a raft membership change would be claiming the second is the
// first.

// maxExternalEnrollmentIDLen bounds an enrollment attempt's request id.
const maxExternalEnrollmentIDLen = 128

// maxExternalEnrollmentFieldLen bounds a provider or node name.
//
// The bound exists because these strings are operator-chosen on the
// provider's side and are written into replicated state, where an
// unbounded value is a way to make every replica carry something
// arbitrarily large.
const maxExternalEnrollmentFieldLen = 256

// applyCreatePendingExternalEnrollment starts an enrollment attempt.
//
// It refuses a duplicate request_id rather than overwriting, the same
// posture CreatePendingJoinRequest takes. An attempt that already exists
// is one an operator may still be reading.
func (f *FSM) applyCreatePendingExternalEnrollment(index uint64, req *internalpb.PendingExternalEnrollment) *FSMApplyResult {
	if req.GetRequestId() == "" || req.GetProvider() == "" || req.GetName() == "" {
		return &FSMApplyResult{Index: index, Error: "CreatePendingExternalEnrollment: request_id, provider, and name must all be set"}
	}
	if len(req.GetRequestId()) > maxExternalEnrollmentIDLen || !printableASCII(req.GetRequestId()) {
		return &FSMApplyResult{Index: index, Error: "CreatePendingExternalEnrollment: request_id is malformed"}
	}
	if len(req.GetProvider()) > maxExternalEnrollmentFieldLen || !printableASCII(req.GetProvider()) {
		return &FSMApplyResult{Index: index, Error: "CreatePendingExternalEnrollment: provider is malformed"}
	}
	if len(req.GetName()) > maxExternalEnrollmentFieldLen || !printableASCII(req.GetName()) {
		return &FSMApplyResult{Index: index, Error: "CreatePendingExternalEnrollment: name is malformed"}
	}
	if len(req.GetSourceId()) > maxExternalEnrollmentFieldLen || !printableASCII(req.GetSourceId()) {
		return &FSMApplyResult{Index: index, Error: "CreatePendingExternalEnrollment: source_id is malformed"}
	}
	if _, exists := f.pendingExternalEnrollments[req.GetRequestId()]; exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreatePendingExternalEnrollment: request_id %q already exists", req.GetRequestId())}
	}
	stored := proto.Clone(req).(*internalpb.PendingExternalEnrollment)
	f.pendingExternalEnrollments[req.GetRequestId()] = stored
	return &FSMApplyResult{Index: index, PendingExternalEnrollment: stored}
}

// applyRecordExternalEnrollment records ONE authorizing act against a
// still-pending enrollment attempt and resolves nothing.
//
// It mirrors applyRecordJoinApproval exactly, including the two refusals
// that matter: a second act that uses the SAME API key or the SAME Comb
// is refused HERE, before it is written anywhere, and an act with an
// empty key or node id is refused rather than recorded as an empty
// string.
//
// The two slots rather than one is the same design as the join flow's:
// one slot would let a third call silently replace a record an operator
// may still be reading.
func (f *FSM) applyRecordExternalEnrollment(index uint64, cmd *internalpb.RecordExternalEnrollment) *FSMApplyResult {
	req, exists := f.pendingExternalEnrollments[cmd.GetRequestId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("RecordExternalEnrollment: request_id %q does not exist", cmd.GetRequestId())}
	}
	if req.GetStatus() != internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_PENDING {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("RecordExternalEnrollment: request_id %q is already %s", cmd.GetRequestId(), req.GetStatus())}
	}
	if cmd.GetKeyId() == "" || cmd.GetNodeId() == "" {
		return &FSMApplyResult{Index: index, Error: "RecordExternalEnrollment: key_id and node_id must both be set - a managerd with no API keys cannot satisfy the two-authorization rule, which is deliberate rather than an oversight"}
	}
	if !printableASCII(cmd.GetKeyId()) || !printableASCII(cmd.GetNodeId()) {
		return &FSMApplyResult{Index: index, Error: "RecordExternalEnrollment: key_id or node_id is malformed"}
	}

	updated := proto.Clone(req).(*internalpb.PendingExternalEnrollment)
	attestation := &internalpb.JoinApprovalAttestation{
		KeyId:  cmd.GetKeyId(),
		NodeId: cmd.GetNodeId(),
		AtUnix: cmd.GetAtUnix(),
	}
	first := updated.GetApproval_1()
	switch {
	case first == nil:
		updated.Approval_1 = attestation

	case updated.GetApproval_2() == nil && AuthorizationsDistinct(first, attestation):
		updated.Approval_2 = attestation

	case updated.GetApproval_2() == nil:
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"RecordExternalEnrollment: this authorizing act cannot satisfy the two-person rule against the one already recorded for %q - the SAME API key or the SAME Comb. Two DISTINCT keys on two DISTINCT Colony members are required, so that one credential cannot admit an external host on its own",
			cmd.GetRequestId())}

	default:
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"RecordExternalEnrollment: %q already has two authorizing acts recorded", cmd.GetRequestId())}
	}

	f.pendingExternalEnrollments[cmd.GetRequestId()] = updated
	return &FSMApplyResult{Index: index, PendingExternalEnrollment: updated}
}

// applyResolveExternalEnrollment is the terminal act on an enrollment
// attempt.
//
// It refuses to resolve an attempt that does not have BOTH authorizing
// acts recorded, which is the whole point of the two-phase shape: a
// single recorded act must never be able to enroll a host on its own.
//
// The enrolment is only ever recorded as ENROLLED or REJECTED. There is
// no path here that adds a voter, because an enrolled external node is
// not a member.
func (f *FSM) applyResolveExternalEnrollment(index uint64, cmd *internalpb.ResolveExternalEnrollment) *FSMApplyResult {
	req, exists := f.pendingExternalEnrollments[cmd.GetRequestId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("ResolveExternalEnrollment: request_id %q does not exist", cmd.GetRequestId())}
	}
	if req.GetStatus() != internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_PENDING {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("ResolveExternalEnrollment: request_id %q is already %s", cmd.GetRequestId(), req.GetStatus())}
	}
	switch cmd.GetStatus() {
	case internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_ENROLLED:
		if req.GetApproval_1() == nil || req.GetApproval_2() == nil {
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
				"ResolveExternalEnrollment: %q has %d of the 2 required authorizing acts recorded, and one act must never be able to enroll an external host on its own",
				cmd.GetRequestId(), countAttestations(req))}
		}
		if !AuthorizationsDistinct(req.GetApproval_1(), req.GetApproval_2()) {
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
				"ResolveExternalEnrollment: %q's two authorizing acts are not distinct - the same API key or the same Colony member - and cannot satisfy the two-person rule",
				cmd.GetRequestId())}
		}
	case internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_REJECTED:
		// A rejection needs no authorizations. Refusing one is an
		// operator's own act and needs no second party.
	default:
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"ResolveExternalEnrollment: %q cannot be resolved to %s - only ENROLLED or REJECTED are terminal",
			cmd.GetRequestId(), cmd.GetStatus())}
	}

	updated := proto.Clone(req).(*internalpb.PendingExternalEnrollment)
	updated.Status = cmd.GetStatus()
	f.pendingExternalEnrollments[cmd.GetRequestId()] = updated
	return &FSMApplyResult{Index: index, PendingExternalEnrollment: updated}
}

// countAttestations counts how many of the two authorizing acts are
// recorded.
func countAttestations(req *internalpb.PendingExternalEnrollment) int {
	n := 0
	if req.GetApproval_1() != nil {
		n++
	}
	if req.GetApproval_2() != nil {
		n++
	}
	return n
}

// ListPendingExternalEnrollments returns every enrollment attempt.
//
// It mirrors ListPendingJoinRequests' own accessor, including the
// mutex.
func (f *FSM) ListPendingExternalEnrollments() []*internalpb.PendingExternalEnrollment {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*internalpb.PendingExternalEnrollment, 0, len(f.pendingExternalEnrollments))
	for _, req := range f.pendingExternalEnrollments {
		out = append(out, req)
	}
	return out
}

// PendingExternalEnrollment returns one enrollment attempt.
//
// It mirrors PendingJoinRequest's own accessor, including the mutex:
// the FSM's maps are read by the read-side RPC handlers concurrently
// with the apply functions.
func (f *FSM) PendingExternalEnrollment(requestID string) (*internalpb.PendingExternalEnrollment, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	req, ok := f.pendingExternalEnrollments[requestID]
	return req, ok
}
