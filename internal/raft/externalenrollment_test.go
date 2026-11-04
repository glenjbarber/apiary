package raft

import (
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/api/internalpb"
)

// seedExternalEnrollment creates a pending enrollment attempt and
// returns its request id.
func seedExternalEnrollment(t *testing.T, f *FSM, requestID, provider, sourceID, name string) {
	t.Helper()
	res := applyOp(t, f, &internalpb.Command{Op: &internalpb.Command_CreatePendingExternalEnrollment{
		CreatePendingExternalEnrollment: &internalpb.CreatePendingExternalEnrollment{
			Enrollment: &internalpb.PendingExternalEnrollment{
				RequestId:      requestID,
				Provider:       provider,
				SourceId:       sourceID,
				Name:           name,
				Fingerprint:    "fp-1",
				ProviderStatus: "online",
				Status:         internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_PENDING,
			},
		},
	}})
	if res.Error != "" {
		t.Fatalf("CreatePendingExternalEnrollment: %s", res.Error)
	}
}

// recordEnrollmentAct records one authorizing act.
func recordEnrollmentAct(t *testing.T, f *FSM, requestID, keyID, nodeID string) *FSMApplyResult {
	t.Helper()
	return applyOp(t, f, &internalpb.Command{Op: &internalpb.Command_RecordExternalEnrollment{
		RecordExternalEnrollment: &internalpb.RecordExternalEnrollment{
			RequestId: requestID,
			KeyId:     keyID,
			NodeId:    nodeID,
			AtUnix:    1700000000,
		},
	}})
}

func TestExternalEnrollmentTwoDistinctActsAreRecorded(t *testing.T) {
	f := newTestFSM(t)
	seedExternalEnrollment(t, f, "req-1", "proxmox", "cluster-a", "pve1")

	if res := recordEnrollmentAct(t, f, "req-1", "key-a", "comb-1"); res.Error != "" {
		t.Fatalf("first act: %s", res.Error)
	}
	res := recordEnrollmentAct(t, f, "req-1", "key-b", "comb-2")
	if res.Error != "" {
		t.Fatalf("second act: %s", res.Error)
	}
	if res.PendingExternalEnrollment.GetApproval_1() == nil || res.PendingExternalEnrollment.GetApproval_2() == nil {
		t.Fatalf("both acts were not recorded: %+v", res.PendingExternalEnrollment)
	}
}

func TestExternalEnrollmentRefusesSameKeyTwice(t *testing.T) {
	f := newTestFSM(t)
	seedExternalEnrollment(t, f, "req-1", "proxmox", "cluster-a", "pve1")

	if res := recordEnrollmentAct(t, f, "req-1", "key-a", "comb-1"); res.Error != "" {
		t.Fatalf("first act: %s", res.Error)
	}
	res := recordEnrollmentAct(t, f, "req-1", "key-a", "comb-2")
	if res.Error == "" {
		t.Fatalf("the same API key was accepted twice")
	}
	if !strings.Contains(res.Error, "SAME API key") {
		t.Errorf("error = %q, want it to name the same key", res.Error)
	}
	// The refused act must not have been written anywhere.
	got, ok := f.PendingExternalEnrollment("req-1")
	if !ok {
		t.Fatalf("the attempt disappeared")
	}
	if got.GetApproval_2() != nil {
		t.Errorf("the refused act was recorded")
	}
}

func TestExternalEnrollmentRefusesSameCombTwice(t *testing.T) {
	f := newTestFSM(t)
	seedExternalEnrollment(t, f, "req-1", "proxmox", "cluster-a", "pve1")

	if res := recordEnrollmentAct(t, f, "req-1", "key-a", "comb-1"); res.Error != "" {
		t.Fatalf("first act: %s", res.Error)
	}
	res := recordEnrollmentAct(t, f, "req-1", "key-b", "comb-1")
	if res.Error == "" {
		t.Fatalf("the same Comb was accepted twice")
	}
	if !strings.Contains(res.Error, "SAME Comb") {
		t.Errorf("error = %q, want it to name the same Comb", res.Error)
	}
}

func TestExternalEnrollmentRefusesEmptyKeyOrNode(t *testing.T) {
	f := newTestFSM(t)
	seedExternalEnrollment(t, f, "req-1", "proxmox", "cluster-a", "pve1")

	for _, tc := range []struct{ keyID, nodeID string }{
		{"", "comb-1"},
		{"key-a", ""},
	} {
		res := recordEnrollmentAct(t, f, "req-1", tc.keyID, tc.nodeID)
		if res.Error == "" {
			t.Errorf("key=%q node=%q: accepted, want a refusal", tc.keyID, tc.nodeID)
		}
	}
}

func TestExternalEnrollmentRefusesResolveWithOneAct(t *testing.T) {
	f := newTestFSM(t)
	seedExternalEnrollment(t, f, "req-1", "proxmox", "cluster-a", "pve1")

	if res := recordEnrollmentAct(t, f, "req-1", "key-a", "comb-1"); res.Error != "" {
		t.Fatalf("first act: %s", res.Error)
	}
	res := applyOp(t, f, &internalpb.Command{Op: &internalpb.Command_ResolveExternalEnrollment{
		ResolveExternalEnrollment: &internalpb.ResolveExternalEnrollment{
			RequestId: "req-1",
			Status:    internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_ENROLLED,
		},
	}})
	if res.Error == "" {
		t.Fatalf("one act was enough to enroll a host")
	}
	if !strings.Contains(res.Error, "one act must never be able to enroll") {
		t.Errorf("error = %q, want it to explain that one act is not enough", res.Error)
	}
}

func TestExternalEnrollmentRefusesDuplicateRequestID(t *testing.T) {
	f := newTestFSM(t)
	seedExternalEnrollment(t, f, "req-1", "proxmox", "cluster-a", "pve1")

	res := applyOp(t, f, &internalpb.Command{Op: &internalpb.Command_CreatePendingExternalEnrollment{
		CreatePendingExternalEnrollment: &internalpb.CreatePendingExternalEnrollment{
			Enrollment: &internalpb.PendingExternalEnrollment{
				RequestId: "req-1",
				Provider:  "proxmox",
				Name:      "pve2",
				Status:    internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_PENDING,
			},
		},
	}})
	if res.Error == "" {
		t.Fatalf("a duplicate request_id was accepted")
	}
	if !strings.Contains(res.Error, "already exists") {
		t.Errorf("error = %q, want it to name the duplicate", res.Error)
	}
}

func TestExternalEnrollmentRefusesMalformedFields(t *testing.T) {
	f := newTestFSM(t)
	for _, tc := range []struct {
		name      string
		requestID string
		provider  string
		nodeName  string
	}{
		{"no request id", "", "proxmox", "pve1"},
		{"no provider", "req-1", "", "pve1"},
		{"no name", "req-1", "proxmox", ""},
	} {
		res := applyOp(t, f, &internalpb.Command{Op: &internalpb.Command_CreatePendingExternalEnrollment{
			CreatePendingExternalEnrollment: &internalpb.CreatePendingExternalEnrollment{
				Enrollment: &internalpb.PendingExternalEnrollment{
					RequestId: tc.requestID,
					Provider:  tc.provider,
					Name:      tc.nodeName,
					Status:    internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_PENDING,
				},
			},
		}})
		if res.Error == "" {
			t.Errorf("%s: accepted, want a refusal", tc.name)
		}
	}
}

func TestExternalEnrollmentResolveRequiresDistinctActs(t *testing.T) {
	f := newTestFSM(t)
	seedExternalEnrollment(t, f, "req-1", "proxmox", "cluster-a", "pve1")

	// Record two acts that are NOT distinct by writing directly into
	// the FSM's own map, which the apply path refuses to produce. The
	// resolve function must still refuse the pair: a defence that only
	// works when every other layer behaves is not a defence.
	f.pendingExternalEnrollments["req-1"].Approval_1 = &internalpb.JoinApprovalAttestation{KeyId: "key-a", NodeId: "comb-1"}
	f.pendingExternalEnrollments["req-1"].Approval_2 = &internalpb.JoinApprovalAttestation{KeyId: "key-a", NodeId: "comb-2"}

	res := applyOp(t, f, &internalpb.Command{Op: &internalpb.Command_ResolveExternalEnrollment{
		ResolveExternalEnrollment: &internalpb.ResolveExternalEnrollment{
			RequestId: "req-1",
			Status:    internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_ENROLLED,
		},
	}})
	if res.Error == "" {
		t.Fatalf("a non-distinct pair was allowed to enroll a host")
	}
	if !strings.Contains(res.Error, "not distinct") {
		t.Errorf("error = %q, want it to name the non-distinct pair", res.Error)
	}
}

func TestExternalEnrollmentRejectionNeedsNoAuthorizations(t *testing.T) {
	f := newTestFSM(t)
	seedExternalEnrollment(t, f, "req-1", "proxmox", "cluster-a", "pve1")

	res := applyOp(t, f, &internalpb.Command{Op: &internalpb.Command_ResolveExternalEnrollment{
		ResolveExternalEnrollment: &internalpb.ResolveExternalEnrollment{
			RequestId: "req-1",
			Status:    internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_REJECTED,
		},
	}})
	if res.Error != "" {
		t.Fatalf("a rejection was refused: %s", res.Error)
	}
	if res.PendingExternalEnrollment.GetStatus() != internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_REJECTED {
		t.Errorf("status = %v, want REJECTED", res.PendingExternalEnrollment.GetStatus())
	}
}

func TestExternalEnrollmentRefusesResolveTwice(t *testing.T) {
	f := newTestFSM(t)
	seedExternalEnrollment(t, f, "req-1", "proxmox", "cluster-a", "pve1")
	if res := recordEnrollmentAct(t, f, "req-1", "key-a", "comb-1"); res.Error != "" {
		t.Fatalf("first act: %s", res.Error)
	}
	if res := recordEnrollmentAct(t, f, "req-1", "key-b", "comb-2"); res.Error != "" {
		t.Fatalf("second act: %s", res.Error)
	}
	resolve := &internalpb.Command{Op: &internalpb.Command_ResolveExternalEnrollment{
		ResolveExternalEnrollment: &internalpb.ResolveExternalEnrollment{
			RequestId: "req-1",
			Status:    internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_ENROLLED,
		},
	}}
	if res := applyOp(t, f, resolve); res.Error != "" {
		t.Fatalf("resolve: %s", res.Error)
	}
	res := applyOp(t, f, resolve)
	if res.Error == "" {
		t.Fatalf("a resolved attempt was resolved again")
	}
	if !strings.Contains(res.Error, "already") {
		t.Errorf("error = %q, want it to say the attempt is already resolved", res.Error)
	}
}

// TestExternalEnrollmentAddsNoVoter is the load-bearing negative test.
//
// An enrolled external node is NOT a member and holds no vote, and the
// whole point of ADR-0191's decision is that it cannot become one. This
// asserts the property at the level it can actually be asserted at: the
// FSM's membership is unchanged by a complete enrollment.
func TestExternalEnrollmentAddsNoVoter(t *testing.T) {
	f := newTestFSM(t)

	before := len(f.vms)
	seedExternalEnrollment(t, f, "req-1", "proxmox", "cluster-a", "pve1")
	if res := recordEnrollmentAct(t, f, "req-1", "key-a", "comb-1"); res.Error != "" {
		t.Fatalf("first act: %s", res.Error)
	}
	if res := recordEnrollmentAct(t, f, "req-1", "key-b", "comb-2"); res.Error != "" {
		t.Fatalf("second act: %s", res.Error)
	}
	res := applyOp(t, f, &internalpb.Command{Op: &internalpb.Command_ResolveExternalEnrollment{
		ResolveExternalEnrollment: &internalpb.ResolveExternalEnrollment{
			RequestId: "req-1",
			Status:    internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_ENROLLED,
		},
	}})
	if res.Error != "" {
		t.Fatalf("resolve: %s", res.Error)
	}

	if len(f.vms) != before {
		t.Errorf("the FSM's membership changed by an enrollment: %d -> %d", before, len(f.vms))
	}
	// The FSM exposes no AddVoter at all - membership is raftd's own
	// configuration, not a command - so an enrollment cannot have
	// reached one. The absence of any such call in this package is the
	// enforcement.
	if _, ok := f.pendingExternalEnrollments["req-1"]; !ok {
		t.Errorf("the attempt was not recorded")
	}
}
