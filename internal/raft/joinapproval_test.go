package raft

// ADR-0147 Part 3's replicated half: RecordJoinApproval's slot rules
// and AuthorizationUse's scan.
//
// These are tested against a real FSM with the real Apply path, because
// the properties that matter are all about what several applied log
// entries do to one record in order - a hand-built map poked in the
// right shape would not exercise the ordering that is the whole point.

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// applyOp drives the same switch Apply itself uses, by handing the
// marshalled command to a real FSM.Apply - so a test cannot pass by
// calling an apply function the production path does not. The index
// counter is local to this file rather than read off the FSM because
// the FSM's own lastIndex is part of its state digest, and a test that
// perturbed it to keep a counter would be testing something else.
var applyIndex uint64

func applyOp(t *testing.T, f *FSM, cmd *internalpb.Command) *FSMApplyResult {
	t.Helper()
	applyIndex++
	res, ok := f.Apply(&raft.Log{Index: applyIndex, Data: mustMarshalCommand(t, cmd)}).(*FSMApplyResult)
	if !ok {
		t.Fatalf("FSM.Apply returned %T, want *FSMApplyResult", res)
	}
	return res
}

// newTestFSM is a bare FSM with no raft behind it. The properties under
// test are all about what a sequence of applied log entries does to one
// record, so a real raft group would add a second node without changing
// any of them - and internal/manager's integration tests already cover
// the end-to-end path through a real one.
func newTestFSM(t *testing.T) *FSM {
	t.Helper()
	applyIndex = 0
	return NewFSM()
}

func seedRequest(t *testing.T, f *FSM, requestID, nodeID, fingerprint string) {
	t.Helper()
	res := applyOp(t, f, &internalpb.Command{Op: &internalpb.Command_CreatePendingJoinRequest{
		CreatePendingJoinRequest: &internalpb.CreatePendingJoinRequest{
			Request: &internalpb.PendingJoinRequest{
				RequestId:          requestID,
				NodeId:             nodeID,
				RaftBindAddress:    nodeID + ".example:17600",
				Code:               "123456",
				RequestedAtUnix:    time.Now().Unix(),
				ExpiresAtUnix:      time.Now().Add(time.Hour).Unix(),
				TlsCertFingerprint: fingerprint,
				Status:             internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING,
			},
		},
	}})
	if res.Error != "" {
		t.Fatalf("seeding a pending request: %s", res.Error)
	}
}

func recordApproval(t *testing.T, f *FSM, requestID, keyID, nodeID string) *FSMApplyResult {
	t.Helper()
	return applyOp(t, f, &internalpb.Command{Op: &internalpb.Command_RecordJoinApproval{
		RecordJoinApproval: &internalpb.RecordJoinApproval{
			RequestId: requestID, KeyId: keyID, NodeId: nodeID, AtUnix: time.Now().Unix(),
		},
	}})
}

func TestRecordJoinApprovalFillsTheFirstEmptySlotAndResolvesNothing(t *testing.T) {
	f := newTestFSM(t)
	seedRequest(t, f, "jreq-1", "comb-3", "SHA256:AA")

	first := recordApproval(t, f, "jreq-1", "key-1", "comb-1")
	if first.Error != "" {
		t.Fatalf("first RecordJoinApproval: %s", first.Error)
	}
	if got := first.PendingJoinRequest.GetApproval_1(); got.GetKeyId() != "key-1" || got.GetNodeId() != "comb-1" {
		t.Errorf("approval_1 = %+v, want key-1 on comb-1", got)
	}
	if first.PendingJoinRequest.GetApproval_2() != nil {
		t.Errorf("approval_2 = %+v, want nil after one authorization", first.PendingJoinRequest.GetApproval_2())
	}
	// The critical property: recording an authorizing act admits nobody.
	// A request that carries one authorization must still be PENDING, or
	// an Admin who clicked once would have admitted a Comb while the UI
	// said a second was needed.
	if got := first.PendingJoinRequest.GetStatus(); got != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING {
		t.Errorf("status after one authorization = %s, want PENDING: recording an authorization must not resolve the request", got)
	}
	stored, ok := f.PendingJoinRequest("jreq-1")
	if !ok || stored.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING {
		t.Errorf("stored request status = %v, want PENDING", stored.GetStatus())
	}
	if len(f.ListPendingJoinRequests()) != 1 {
		t.Error("the request left the pending list; a request awaiting a second authorization is still actionable")
	}
}

func TestRecordJoinApprovalRefusesAMissingOrResolvedRequest(t *testing.T) {
	f := newTestFSM(t)
	if res := recordApproval(t, f, "jreq-nope", "key-1", "comb-1"); res.Error == "" {
		t.Error("RecordJoinApproval on a nonexistent request = no error, want a refusal")
	}
	seedRequest(t, f, "jreq-1", "comb-3", "SHA256:AA")
	approveRes := applyOp(t, f, &internalpb.Command{Op: &internalpb.Command_ApprovePendingJoinRequest{
		ApprovePendingJoinRequest: &internalpb.ApprovePendingJoinRequest{RequestId: "jreq-1"},
	}})
	if approveRes.Error != "" {
		t.Fatalf("approving: %s", approveRes.Error)
	}
	if res := recordApproval(t, f, "jreq-1", "key-1", "comb-1"); res.Error == "" {
		t.Error("RecordJoinApproval on an already-approved request = no error, want a refusal: writing into a closed record is not something a late click may do")
	}
}

func TestRecordJoinApprovalRefusesAnUnattributedAct(t *testing.T) {
	// The "nobody authenticated" case. An empty key id is not a wildcard
	// that two empty ids could satisfy: it is the absence of a
	// credential, and the two-person rule counts credentials.
	f := newTestFSM(t)
	seedRequest(t, f, "jreq-1", "comb-3", "SHA256:AA")
	for _, tc := range []struct{ name, key, node string }{
		{"no key", "", "comb-1"},
		{"no node", "key-1", ""},
		{"neither", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := recordApproval(t, f, "jreq-1", tc.key, tc.node)
			if res.Error == "" {
				t.Fatalf("RecordJoinApproval(%q, %q) = no error, want a refusal", tc.key, tc.node)
			}
		})
	}
	stored, _ := f.PendingJoinRequest("jreq-1")
	if stored.GetApproval_1() != nil {
		t.Errorf("a refused act still wrote approval_1 = %+v", stored.GetApproval_1())
	}
}

func TestRecordJoinApprovalSecondSlotMakesThePairDistinct(t *testing.T) {
	f := newTestFSM(t)
	seedRequest(t, f, "jreq-1", "comb-3", "SHA256:AA")
	recordApproval(t, f, "jreq-1", "key-1", "comb-1")
	second := recordApproval(t, f, "jreq-1", "key-2", "comb-2")
	if second.Error != "" {
		t.Fatalf("second RecordJoinApproval: %s", second.Error)
	}
	a, b := second.PendingJoinRequest.GetApproval_1(), second.PendingJoinRequest.GetApproval_2()
	if !AuthorizationsDistinct(a, b) {
		t.Fatalf("recorded pair (%+v, %+v) is not distinct; two keys on two Combs must satisfy the rule", a, b)
	}
}

func TestRecordJoinApprovalReplacesAPairThatCouldNeverWork(t *testing.T) {
	// The recoverability case. Without it, one Admin clicking approve
	// twice from one Comb would permanently poison a request: both slots
	// would hold the same act, every later call would be refused, and the
	// only way out would be purging the request and starting the join
	// over. The SECOND slot is replaced; the first never is, because the
	// first is the act that made the pair unusable.
	f := newTestFSM(t)
	seedRequest(t, f, "jreq-1", "comb-3", "SHA256:AA")
	recordApproval(t, f, "jreq-1", "key-1", "comb-1")
	recordApproval(t, f, "jreq-1", "key-1", "comb-1") // same key, same Comb

	third := recordApproval(t, f, "jreq-1", "key-2", "comb-2")
	if third.Error != "" {
		t.Fatalf("a third, distinct authorization after two identical ones: %s", third.Error)
	}
	if got := third.PendingJoinRequest.GetApproval_1().GetKeyId(); got != "key-1" {
		t.Errorf("approval_1 = %q, want key-1: the first act is never replaced", got)
	}
	if got := third.PendingJoinRequest.GetApproval_2(); got.GetKeyId() != "key-2" || got.GetNodeId() != "comb-2" {
		t.Errorf("approval_2 = %+v, want key-2 on comb-2", got)
	}
}

func TestRecordJoinApprovalIgnoresAFurtherActOnceSatisfied(t *testing.T) {
	// Two properties in one, because they are the same decision.
	//
	// The recorded pair is the Colony's history of who admitted this
	// Comb, so a later act must not overwrite either half of it.
	//
	// And the act must not be REFUSED either. A request can hold two
	// distinct authorizations and still be turned away by a later check
	// - a duplicate node_id, a reachability dial that failed - and an
	// operator who fixes that and approves again has to get past this
	// gate. A gate that cannot be re-entered after a correctable
	// failure stops every retry at a message about a rule already
	// satisfied, which is a trap wearing the costume of a rule.
	f := newTestFSM(t)
	seedRequest(t, f, "jreq-1", "comb-3", "SHA256:AA")
	recordApproval(t, f, "jreq-1", "key-1", "comb-1")
	recordApproval(t, f, "jreq-1", "key-2", "comb-2")

	for _, tc := range []struct{ name, key, node string }{
		{"the identical act again", "key-2", "comb-2"},
		{"a third, different act", "key-3", "comb-3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := recordApproval(t, f, "jreq-1", tc.key, tc.node)
			if res.Error != "" {
				t.Fatalf("a further act on a satisfied pair was refused: %s", res.Error)
			}
			a, b := res.PendingJoinRequest.GetApproval_1(), res.PendingJoinRequest.GetApproval_2()
			if a.GetKeyId() != "key-1" || a.GetNodeId() != "comb-1" {
				t.Errorf("approval_1 = (%q, %q), want (key-1, comb-1) unchanged", a.GetKeyId(), a.GetNodeId())
			}
			if b.GetKeyId() != "key-2" || b.GetNodeId() != "comb-2" {
				t.Errorf("approval_2 = (%q, %q), want (key-2, comb-2) unchanged", b.GetKeyId(), b.GetNodeId())
			}
		})
	}
}

func TestRecordJoinApprovalRefusesARepeatedFirstActOnceBothSlotsAreUnusable(t *testing.T) {
	f := newTestFSM(t)
	seedRequest(t, f, "jreq-1", "comb-3", "SHA256:AA")
	recordApproval(t, f, "jreq-1", "key-1", "comb-1")
	recordApproval(t, f, "jreq-1", "key-2", "comb-1") // two keys, one Comb
	res := recordApproval(t, f, "jreq-1", "key-3", "comb-1")
	if res.Error == "" {
		t.Fatal("a third act from the same Comb, matching neither slot, = no error, want a refusal")
	}
	if !strings.Contains(res.Error, "comb-1") {
		t.Errorf("refusal %q does not name the Comb, so the operator is not told which side of the rule to change", res.Error)
	}
}

func TestApproveStampsTheRecordOfUseOnTheSameCommand(t *testing.T) {
	// The atomicity property. "This join was approved" and "this entry
	// was spent" have to be one fact: a log entry that approved without
	// recording the spend would leave single use resting on a file only
	// the leader reads.
	f := newTestFSM(t)
	seedRequest(t, f, "jreq-1", "comb-3", "SHA256:AA")
	res := applyOp(t, f, &internalpb.Command{Op: &internalpb.Command_ApprovePendingJoinRequest{
		ApprovePendingJoinRequest: &internalpb.ApprovePendingJoinRequest{
			RequestId: "jreq-1", AuthorizationId: "auth-77", ConsumedAtUnix: 1700000000,
		},
	}})
	if res.Error != "" {
		t.Fatalf("ApprovePendingJoinRequest: %s", res.Error)
	}
	if got := res.PendingJoinRequest.GetAuthorizationId(); got != "auth-77" {
		t.Errorf("authorization_id = %q, want auth-77", got)
	}
	if got := res.PendingJoinRequest.GetConsumedAtUnix(); got != 1700000000 {
		t.Errorf("consumed_at_unix = %d, want 1700000000", got)
	}
	// And it survives on the stored record, which is what a later
	// approval on a DIFFERENT leader reads.
	stored, _ := f.PendingJoinRequest("jreq-1")
	if stored.GetAuthorizationId() != "auth-77" || stored.GetConsumedAtUnix() != 1700000000 {
		t.Errorf("stored record = (%q, %d), want (auth-77, 1700000000)", stored.GetAuthorizationId(), stored.GetConsumedAtUnix())
	}
}

func TestAuthorizationUseFindsTheRequestThatSpentAnEntry(t *testing.T) {
	f := newTestFSM(t)
	if _, _, found := f.AuthorizationUse("auth-77"); found {
		t.Error("AuthorizationUse on an untouched store reported a use")
	}
	seedRequest(t, f, "jreq-1", "comb-3", "SHA256:AA")
	applyOp(t, f, &internalpb.Command{Op: &internalpb.Command_ApprovePendingJoinRequest{
		ApprovePendingJoinRequest: &internalpb.ApprovePendingJoinRequest{
			RequestId: "jreq-1", AuthorizationId: "auth-77", ConsumedAtUnix: 1700000000,
		},
	}})
	requestID, consumedAt, found := f.AuthorizationUse("auth-77")
	if !found {
		t.Fatal("AuthorizationUse(auth-77) = not found after an approval that spent it")
	}
	if requestID != "jreq-1" || consumedAt != 1700000000 {
		t.Errorf("AuthorizationUse(auth-77) = (%q, %d), want (jreq-1, 1700000000)", requestID, consumedAt)
	}
	// A different entry is a different entry.
	if _, _, found := f.AuthorizationUse("auth-78"); found {
		t.Error("AuthorizationUse(auth-78) reported a use; entries must not be conflated")
	}
	// An empty id is a caller bug, not a question about everything.
	if _, _, found := f.AuthorizationUse(""); found {
		t.Error("AuthorizationUse(\"\") reported a use; an empty id must never match")
	}
}

func TestAuthorizationUseFindsAnEntrySpentByARequestThatIsStillPending(t *testing.T) {
	// The replication test the ADR singles out: the spend is recorded
	// when the entry is authorized, not when the request resolves, so a
	// leader that takes over between the two still sees it. A version of
	// this that keyed off the request's STATUS would report a spent
	// entry as available for the whole of the approval.
	f := newTestFSM(t)
	seedRequest(t, f, "jreq-1", "comb-3", "SHA256:AA")
	recordApproval(t, f, "jreq-1", "key-1", "comb-1")
	recordApproval(t, f, "jreq-1", "key-2", "comb-2")
	applyOp(t, f, &internalpb.Command{Op: &internalpb.Command_ApprovePendingJoinRequest{
		ApprovePendingJoinRequest: &internalpb.ApprovePendingJoinRequest{
			RequestId: "jreq-1", AuthorizationId: "auth-77", ConsumedAtUnix: 1700000000,
		},
	}})
	stored, _ := f.PendingJoinRequest("jreq-1")
	if stored.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED {
		t.Fatalf("test setup: status = %s, want APPROVED", stored.GetStatus())
	}
	if _, _, found := f.AuthorizationUse("auth-77"); !found {
		t.Error("AuthorizationUse did not find a spend recorded on a request that is no longer actionable; the record of use outlives the record of the request")
	}
}

func TestAuthorizationsDistinctIsTheWholeRule(t *testing.T) {
	att := func(key, node string) *internalpb.JoinApprovalAttestation {
		return &internalpb.JoinApprovalAttestation{KeyId: key, NodeId: node}
	}
	for _, tc := range []struct {
		name string
		a, b *internalpb.JoinApprovalAttestation
		want bool
	}{
		{"two keys on two Combs", att("k1", "c1"), att("k2", "c2"), true},
		{"one key twice", att("k1", "c1"), att("k1", "c2"), false},
		{"two keys on one Comb", att("k1", "c1"), att("k2", "c1"), false},
		{"the same act twice", att("k1", "c1"), att("k1", "c1"), false},
		{"a missing act is never distinct", att("k1", "c1"), nil, false},
		{"both missing", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AuthorizationsDistinct(tc.a, tc.b); got != tc.want {
				t.Errorf("AuthorizationsDistinct(%+v, %+v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestRecordJoinApprovalSurvivesASnapshotRestore(t *testing.T) {
	// A raft snapshot restore or a seeded -restore must not silently
	// drop the two-person rule's record, or a restored Colony would
	// accept a join that had already been authorized - or refuse one
	// that had not.
	f := newTestFSM(t)
	seedRequest(t, f, "jreq-1", "comb-3", "SHA256:AA")
	recordApproval(t, f, "jreq-1", "key-1", "comb-1")
	recordApproval(t, f, "jreq-1", "key-2", "comb-2")
	applyOp(t, f, &internalpb.Command{Op: &internalpb.Command_ApprovePendingJoinRequest{
		ApprovePendingJoinRequest: &internalpb.ApprovePendingJoinRequest{
			RequestId: "jreq-1", AuthorizationId: "auth-77", ConsumedAtUnix: 1700000000,
		},
	}})

	snap, err := f.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	// Persist through the real sink and restore through the real
	// reader, rather than handing one FSM's internals to another: a
	// snapshot test that skipped encoding would not notice a field
	// missing from FSMSnapshotState, which is the entire risk here.
	sink := &fakeSnapshotSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("Persist() error: %v", err)
	}
	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}
	got, ok := restored.PendingJoinRequest("jreq-1")
	if !ok {
		t.Fatal("the approved request did not survive a snapshot restore")
	}
	if got.GetApproval_1().GetKeyId() != "key-1" || got.GetApproval_2().GetKeyId() != "key-2" {
		t.Errorf("recorded pair after restore = (%q, %q), want (key-1, key-2)", got.GetApproval_1().GetKeyId(), got.GetApproval_2().GetKeyId())
	}
	if got.GetAuthorizationId() != "auth-77" || got.GetConsumedAtUnix() != 1700000000 {
		t.Errorf("record of use after restore = (%q, %d), want (auth-77, 1700000000)", got.GetAuthorizationId(), got.GetConsumedAtUnix())
	}
	requestID, _, found := restored.AuthorizationUse("auth-77")
	if !found || requestID != "jreq-1" {
		t.Errorf("AuthorizationUse after restore = (%q, %v), want (jreq-1, true): a restored Colony must not be able to spend the same entry twice", requestID, found)
	}
}

func TestRecordJoinApprovalIsDeterministicUnderReplay(t *testing.T) {
	// Every replica applies the identical command stream and must reach
	// the identical state. The whole two-person rule rests on "first" and
	// "second" meaning the same thing everywhere, so replaying the same
	// entries into a fresh FSM has to reproduce the same pair.
	cmd := func(key, node string) *internalpb.Command {
		return &internalpb.Command{Op: &internalpb.Command_RecordJoinApproval{
			RecordJoinApproval: &internalpb.RecordJoinApproval{RequestId: "jreq-1", KeyId: key, NodeId: node, AtUnix: 5},
		}}
	}
	commands := []*internalpb.Command{cmd("key-1", "comb-1"), cmd("key-2", "comb-2")}

	first := newTestFSM(t)
	seedRequest(t, first, "jreq-1", "comb-3", "SHA256:AA")
	for _, c := range commands {
		if res := applyOp(t, first, c); res.Error != "" {
			t.Fatalf("applying: %s", res.Error)
		}
	}
	second := newTestFSM(t)
	seedRequest(t, second, "jreq-1", "comb-3", "SHA256:AA")
	for i, c := range commands {
		if res := applyOp(t, second, c); res.Error != "" {
			t.Fatalf("replaying entry %d: %s", i, res.Error)
		}
	}

	a, _ := first.PendingJoinRequest("jreq-1")
	b, _ := second.PendingJoinRequest("jreq-1")
	if !proto.Equal(a, b) {
		t.Errorf("two replicas reached different states from the same log:\n%+v\n%+v", a, b)
	}
}
