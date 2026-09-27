package manager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/glenjbarber/apiary/internal/guardrail"
	"github.com/glenjbarber/apiary/internal/restartplan"
)

// This file holds the two tests that assert internal/manager and
// internal/restartplan agree with each other. They live HERE, in the
// manager package, rather than in internal/restartplan's own tests where
// they started, because ADR-0125 made the dependency one-way: internal/
// manager now imports internal/restartplan to evaluate the quorum-safety
// rules. A test in package restartplan that imported manager back to
// check the agreement would close a cycle that Go rejects outright -
// "import cycle not allowed in test" - so the assertions moved to the
// side that is allowed to see both packages.
//
// The assertions themselves are unchanged, and they are still the same
// bidirectional check they always were: each side writes a record and
// the other reads it, so neither can drift from the other in the file
// name, the JSON field names, or the encoding.

// TestPendingStoreMatchesManagerRestartConfirmStore is the check that
// makes having two writers to the pending-restart file safe rather than
// merely convenient.
//
// RestartConfirmStore is the writer in production today;
// restartplan.PendingStore is the reader cmd/raftd uses, and the writer
// restartplan.Engine uses. If those two ever disagree about the file
// name, the JSON field names or the encoding, the restarted raftd
// silently finds nothing to confirm and a raft-replicated lease is
// stranded forever with no TTL to release it - a failure that no unit
// test inside either package could catch, because each would be testing
// only its own half. So this test writes through one type and reads
// through the other, both ways, and compares the bytes on disk as well
// as the decoded values.
func TestPendingStoreMatchesManagerRestartConfirmStore(t *testing.T) {
	dir := t.TempDir()
	theirs := NewRestartConfirmStore(dir)
	mine := restartplan.NewPendingStore(dir)
	want := restartplan.PendingRestart{Service: restartplan.DefaultService, NodeID: "comb-a", LeaseID: 4242}

	t.Run("what I write, managerd reads", func(t *testing.T) {
		if err := mine.Save(want); err != nil {
			t.Fatalf("PendingStore.Save: %v", err)
		}
		got, found, err := theirs.Load(restartplan.DefaultService)
		if err != nil {
			t.Fatalf("manager.RestartConfirmStore.Load: %v", err)
		}
		if !found {
			t.Fatalf("manager.RestartConfirmStore.Load found nothing; the two stores disagree about the path")
		}
		if got.Service != want.Service || got.NodeID != want.NodeID || got.LeaseID != want.LeaseID {
			t.Errorf("managerd read %+v, want %+v", got, want)
		}
	})

	t.Run("what managerd writes, I read", func(t *testing.T) {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		theirRec := PendingRestart{Service: restartplan.DefaultService, NodeID: "comb-b", LeaseID: 99}
		if err := theirs.Save(theirRec); err != nil {
			t.Fatalf("manager.RestartConfirmStore.Save: %v", err)
		}
		got, found, err := mine.Load(restartplan.DefaultService)
		if err != nil {
			t.Fatalf("PendingStore.Load: %v", err)
		}
		if !found {
			t.Fatalf("PendingStore.Load found nothing; the two stores disagree about the path")
		}
		if got.Service != theirRec.Service || got.NodeID != theirRec.NodeID || got.LeaseID != theirRec.LeaseID {
			t.Errorf("I read %+v, want %+v", got, theirRec)
		}
	})

	t.Run("the on-disk encoding is byte-for-byte identical", func(t *testing.T) {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := mine.Save(want); err != nil {
			t.Fatal(err)
		}
		mineBytes, err := os.ReadFile(filepath.Join(dir, "pending-restart-"+restartplan.DefaultService+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := theirs.Save(PendingRestart(want)); err != nil {
			t.Fatal(err)
		}
		theirBytes, err := os.ReadFile(filepath.Join(dir, "pending-restart-"+restartplan.DefaultService+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(mineBytes) != string(theirBytes) {
			t.Errorf("encodings differ:\n  restartplan: %s\n  manager:    %s", mineBytes, theirBytes)
		}
		// A var (not :=) so this stays true even if the encoding above
		// somehow became identical for a different reason.
		var decodedA, decodedB map[string]any
		if err := json.Unmarshal(mineBytes, &decodedA); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(theirBytes, &decodedB); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"service", "node_id", "lease_id"} {
			if _, ok := decodedA[key]; !ok {
				t.Errorf("key %q missing; the on-disk contract cmd/raftd depends on has drifted", key)
			}
		}
	})

	t.Run("clear is symmetric too", func(t *testing.T) {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := mine.Save(want); err != nil {
			t.Fatal(err)
		}
		if err := theirs.Clear(restartplan.DefaultService); err != nil {
			t.Fatalf("manager RestartConfirmStore.Clear: %v", err)
		}
		if _, found, err := mine.Load(restartplan.DefaultService); err != nil || found {
			t.Errorf("after managerd cleared it, Load = (found=%v, err=%v), want found=false", found, err)
		}
	})
}

// TestDefaultServiceMatchesManagerServices keeps restartplan's service
// key honest against the real internal/manager list, which is where the
// rc.d name an operator actually restarts comes from. A rename there
// without a rename here would silently target nothing.
func TestDefaultServiceMatchesManagerServices(t *testing.T) {
	if restartplan.DefaultService != "apiary_raftd" {
		t.Errorf("DefaultService = %q, want %q (ADR-0125 §1: the existing rc.d name, not a new string)", restartplan.DefaultService, "apiary_raftd")
	}
	if restartplan.ManagerService != "apiary_managerd" {
		t.Errorf("ManagerService = %q, want %q", restartplan.ManagerService, "apiary_managerd")
	}
	if restartplan.DefaultService == restartplan.ManagerService {
		t.Errorf("the two lease keys must differ; they gate independent leases")
	}
	// The verdict vocabulary this package reports must be the one
	// internal/guardrail already defines, so a merged Report needs no
	// translation.
	if guardrail.Allow != "allow" || guardrail.Block != "block" || guardrail.Unknown != "unknown" {
		t.Errorf("guardrail's own vocabulary changed underneath this package")
	}
	if restartplan.RuleQuorumSafety != "raftd-quorum-safety" || restartplan.RuleLeaderRestart != "raftd-leader-restart" {
		t.Errorf("the rule ids must stay the strings ADR-0125 §4 and the frontend's copy key off: %q, %q", restartplan.RuleQuorumSafety, restartplan.RuleLeaderRestart)
	}
}

// TestGuardrailServicesMatchTheRealInventory is the check that the
// guardrail's trigger set and the restartable inventory cannot drift
// apart.
//
// These are two separate tables on purpose - one answers "may this be
// restarted at all", the other "does its restart need a cluster-wide
// lease" - but a service appearing in only one of them is always a bug,
// in one direction or the other:
//
//   - restartable but not guardrailed means a cluster-wide consequence
//     (costing quorum, or severing the RPC carrying the restart) with
//     nothing stopping a second node doing the same thing at once.
//   - guardrailed but not restartable means a lease that can be
//     reserved for a service that can then never actually be restarted,
//     so the lease strands and blocks the cluster's next maintenance
//     forever, with no TTL to release it.
//
// apiary_raftd is the case that makes this test worth having: it was
// deliberately absent from the restartable inventory until ADR-0125,
// because exposing a raftd restart before the quorum guardrail existed
// would have been the worst of both worlds at once.
func TestGuardrailServicesMatchTheRealInventory(t *testing.T) {
	for _, entry := range apiaryServices {
		guardrailed := guardrailService(entry.name)
		switch {
		case entry.restartable && !guardrailed:
			// Not automatically an error - frontend and restshimd are
			// legitimately restartable without a lease, because
			// restarting them cannot cost quorum and does not sever the
			// RPC carrying the call. Assert that reasoning explicitly so
			// adding a fourth such service is a deliberate act.
			if entry.name == "apiary_frontend" || entry.name == "apiary_restshimd" {
				continue
			}
			t.Errorf("%s is restartable but not guardrailed; either it is safe (say why here) or it needs a lease", entry.name)
		case guardrailed && !entry.restartable:
			t.Errorf("%s is guardrailed but not restartable; its lease can be reserved and then never released", entry.name)
		}
	}

	// And the specific facts ADR-0125 turned on, asserted directly
	// rather than inferred from the loops above.
	if !restartableService(raftdServiceName) {
		t.Errorf("%s must be restartable now that ADR-0125's guardrail exists", raftdServiceName)
	}
	if !guardrailService(raftdServiceName) {
		t.Errorf("%s must be guardrailed; a raftd restart can cost the cluster its quorum", raftdServiceName)
	}
	if raftdServiceName != restartplan.DefaultService {
		t.Errorf("raftdServiceName = %q but restartplan.DefaultService = %q; cmd/raftd's confirmation hook keys off the latter, so a divergence would strand the lease",
			raftdServiceName, restartplan.DefaultService)
	}
}
