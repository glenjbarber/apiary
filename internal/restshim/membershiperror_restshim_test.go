package restshim

import (
	"encoding/json"
	"net/http"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// TestServer_StatusCarriesRaftMembershipError pins the one thing this
// branch can assert without the field existing yet: the status body
// keeps a raft_membership_error key, so a JSON consumer is never left
// to infer a healthy cluster from raft_reachable alone. The key is
// empty when the membership read succeeded, which is the same shape
// raft_error already has.
//
// It cannot assert the non-empty case here because doing so would mean
// naming the proto field, and the field arrives from a parallel branch.
// The assertion is about the key surviving, not about its value.
func TestServer_StatusCarriesRaftMembershipError(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId: "manager-1",
		RaftReachable: true,
	}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodGet, "/v1/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if _, ok := got["raft_membership_error"]; !ok {
		t.Errorf("raft_membership_error absent from the status body; a consumer would see raft_reachable alone: %s", rec.Body.String())
	}
	// Adding the key must not disturb the fields already there.
	if got["raft_reachable"] != true {
		t.Errorf("raft_reachable = %v, want true", got["raft_reachable"])
	}
	if got["raft_error"] != "" {
		t.Errorf("raft_error = %v, want empty", got["raft_error"])
	}
}
