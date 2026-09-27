package raft

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/raft"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

func createJoinReq(t *testing.T, fsm *FSM, index uint64, id, node string, requestedAt, expiresAt int64) *FSMApplyResult {
	t.Helper()
	cmd := &internalpb.Command{Op: &internalpb.Command_CreatePendingJoinRequest{CreatePendingJoinRequest: &internalpb.CreatePendingJoinRequest{
		Request: &internalpb.PendingJoinRequest{
			RequestId: id, NodeId: node, RaftBindAddress: "10.0.0.2:17600", Code: "123456",
			RequestedAtUnix: requestedAt, ExpiresAtUnix: expiresAt,
			Status: internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING,
		},
	}}}
	return fsm.Apply(&raft.Log{Index: index, Data: mustMarshalCommand(t, cmd)}).(*FSMApplyResult)
}

func TestValidateJoinRequestFields(t *testing.T) {
	long := strings.Repeat("a", 300)
	cases := []struct {
		name, node, addr, fp string
		ok                   bool
	}{
		{"typical", "brood.lab3.home.arpa", "brood.lab3.home.arpa:17600", "SHA256:AA:BB", true},
		{"ip and empty fingerprint", "node-1", "10.90.0.94:17600", "", true},
		{"ipv6 bind", "node_1", "[fd00::1]:17600", "", true},
		{"empty node", "", "10.0.0.1:1", "", false},
		{"node too long", long, "10.0.0.1:1", "", false},
		{"node with space", "a b", "10.0.0.1:1", "", false},
		{"node with newline", "a\nb", "10.0.0.1:1", "", false},
		{"addr no port", "n", "10.0.0.1", "", false},
		{"addr empty host", "n", ":17600", "", false},
		{"addr port zero", "n", "h:0", "", false},
		{"addr port too big", "n", "h:70000", "", false},
		{"addr not numeric port", "n", "h:http", "", false},
		{"addr too long", "n", long + ":1", "", false},
		{"fingerprint too long", "n", "h:1", strings.Repeat("F", 300), false},
		{"fingerprint control char", "n", "h:1", "SHA256:\x00", false},
	}
	for _, c := range cases {
		err := ValidateJoinRequestFields(c.node, c.addr, c.fp)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestFSM_CreatePendingJoinRequest_RejectsOversizedFields(t *testing.T) {
	fsm := NewFSM()
	res := createJoinReq(t, fsm, 1, "r1", strings.Repeat("x", 4096), 100, 1000)
	if res.Error == "" {
		t.Fatalf("Error = empty, want a rejection of a 4096 byte node_id")
	}
	if len(fsm.pendingJoinRequests) != 0 {
		t.Errorf("a rejected request was recorded")
	}
}

func TestFSM_CreatePendingJoinRequest_CapsRecordedRequests(t *testing.T) {
	fsm := NewFSM()
	for i := 0; i < MaxJoinRequests; i++ {
		if res := createJoinReq(t, fsm, uint64(i+1), fmt.Sprintf("r%d", i), fmt.Sprintf("node-%d", i), 1000, 1900); res.Error != "" {
			t.Fatalf("request %d rejected below the cap: %s", i, res.Error)
		}
	}
	res := createJoinReq(t, fsm, 999, "one-too-many", "node-x", 1001, 1901)
	if res.Error == "" {
		t.Fatalf("Error = empty, want a rejection once %d requests are recorded", MaxJoinRequests)
	}
	if len(fsm.pendingJoinRequests) != MaxJoinRequests {
		t.Errorf("len = %d, want %d", len(fsm.pendingJoinRequests), MaxJoinRequests)
	}
}

// Expired records are evicted when a later create arrives, decided from the
// new request's own requested_at (log data), so every replica agrees.
func TestFSM_CreatePendingJoinRequest_EvictsRecordsExpiredPastRetention(t *testing.T) {
	fsm := NewFSM()
	for i := 0; i < MaxJoinRequests; i++ {
		createJoinReq(t, fsm, uint64(i+1), fmt.Sprintf("old%d", i), fmt.Sprintf("node-%d", i), 1000, 1900)
	}

	// Still inside the retention window after expiry: nothing is evicted, so
	// a poll for a just-expired request still finds it and the cap still holds.
	if res := createJoinReq(t, fsm, 500, "early", "node-e", 1900+joinRequestRetentionAfterExpiry-1, 9999); res.Error == "" {
		t.Fatalf("Error = empty, want the cap to hold while records are inside the retention window")
	}

	// Past retention: the stale records are evicted and the new one fits.
	if res := createJoinReq(t, fsm, 501, "late", "node-l", 1900+joinRequestRetentionAfterExpiry+1, 9999); res.Error != "" {
		t.Fatalf("Error = %q, want the new request accepted after old records were evicted", res.Error)
	}
	if len(fsm.pendingJoinRequests) != 1 {
		t.Errorf("len = %d, want only the new request left", len(fsm.pendingJoinRequests))
	}
}
