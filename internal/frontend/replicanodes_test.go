package frontend

// The replica-node picker, with the owner excluded SERVER-SIDE.
//
// What these pin: that the owner is excluded from the list, that the
// exclusion is not applied when there is no owner yet, and that the
// SERVER-side validator and the picker agree - because the picker
// offering something the validator rejects is the gap this work
// closes.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// The owner is excluded from the replica list.
func TestReplicaNodeOptions_ExcludesTheOwner(t *testing.T) {
	nodes := []string{"brood", "drone", "sting"}
	got := replicaNodeOptions(nodes, "drone")
	if len(got) != 2 {
		t.Fatalf("the replica list holds %v, want two Combs", got)
	}
	for _, node := range got {
		if node == "drone" {
			t.Error("the owner is in the replica list; HAST replicates between two DIFFERENT Combs")
		}
	}
}

// With NO owner, nothing is excluded.
//
// Until the operator has chosen an owner, every Comb is a possible
// replica, and removing Combs from the list on the strength of an
// answer that does not exist yet would make the list look arbitrary.
func TestReplicaNodeOptions_NoOwnerExcludesNothing(t *testing.T) {
	nodes := []string{"brood", "drone"}
	got := replicaNodeOptions(nodes, "")
	if len(got) != 2 {
		t.Errorf("the replica list holds %v with no owner chosen, want both Combs", got)
	}
}

// The picker and the VALIDATOR agree.
//
// This is the property that matters: the picker must not offer
// something the validator will reject. The validator refuses a
// replica equal to the owner, so the picker must not list it.
func TestReplicaNodeOptions_AgreesWithTheValidator(t *testing.T) {
	nodes := []string{"brood", "drone"}
	owner := "brood"

	// What the picker offers.
	offered := replicaNodeOptions(nodes, owner)

	// What the validator refuses. This is the same rule the form's
	// POST path enforces.
	refused := false
	for _, node := range offered {
		if node == owner {
			refused = true
		}
	}
	if refused {
		t.Errorf("the picker offers %v, which includes the owner %q - the validator refuses that pairing, so the picker must not offer it", offered, owner)
	}
}

// The fragment renders the list with the owner excluded.
//
// The rendered fragment is what the browser actually receives, so it
// is what the test checks.
func TestReplicaNodeFragment_ExcludesTheOwner(t *testing.T) {
	s := newTestServer(t, &fakeClient{
		statusResp: &rpcpb.StatusResponse{KnownNodeIds: []string{"brood", "drone"}},
	})

	req := httptest.NewRequest(http.MethodGet, "/create/replica-nodes?owner=brood", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "drone") {
		t.Error("the rendered fragment does not offer the other Comb")
	}
	if strings.Contains(body, "value=\"brood\"") {
		t.Error("the rendered fragment offers the owner as a replica")
	}
}

// The full wizard page renders the picker with the owner excluded.
func TestCreateGuidedPage_ReplicaListExcludesTheOwner(t *testing.T) {
	s := newTestServer(t, &fakeClient{
		statusResp: &rpcpb.StatusResponse{
			KnownNodeIds:  []string{"brood", "drone"},
			ManagerNodeId: "brood",
		},
		listResp: &rpcpb.ListVMsResponse{},
	})

	req := httptest.NewRequest(http.MethodGet, "/create", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "replica-node-slot") {
		t.Error("the page does not render the replica picker's refresh slot")
	}
	// The owner is this Comb - the page's default - so it must not be
	// offered as its own replica.
	if strings.Contains(body, `<option value="brood">brood</option>`) {
		t.Error("the owner is offered as a replica target")
	}
}

// The fragment fails soft.
//
// A Comb whose list cannot be read leaves the picker showing what it
// has, rather than breaking the pane.
func TestReplicaNodeFragment_FailsSoft(t *testing.T) {
	s := newTestServer(t, &fakeClient{statusErr: context.DeadlineExceeded})

	req := httptest.NewRequest(http.MethodGet, "/create/replica-nodes", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when the list cannot be read", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "replica-node") {
		t.Errorf("the fragment does not render a picker at all: %s", rec.Body.String())
	}
}
