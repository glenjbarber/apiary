package extdiscovery

import (
	"testing"
	"time"
)

// A scan that FAILED must not remove anything. This is the rule that
// stops a provider outage from deleting live records.
//
// The test is checked by re-introducing the bug: if Apply deleted on an
// incomplete scan, this fails.
func TestApply_FailedScanPreservesEverything(t *testing.T) {
	store := NewStore()

	store.Apply(ScanResult{
		State:      StateFresh,
		Complete:   true,
		ObservedAt: time.Now(),
		Nodes: []Node{
			{Provider: "proxmox", SourceID: "c1", Name: "n1", Fingerprint: "aa"},
		},
		Resources: []Resource{
			{Provider: "proxmox", SourceID: "c1", NodeName: "n1", Kind: KindVM, SourceResourceID: "100", Name: "web"},
		},
	})

	if len(store.Nodes()) != 1 || len(store.Resources()) != 1 {
		t.Fatalf("seed failed: nodes=%d resources=%d", len(store.Nodes()), len(store.Resources()))
	}

	// Now a failed scan that saw NOTHING.
	store.Apply(ScanResult{
		State:      StateUnavailable,
		Complete:   false,
		ObservedAt: time.Now(),
	})

	if len(store.Nodes()) != 1 {
		t.Errorf("nodes after a failed scan = %d, want 1: a failed scan must not remove records", len(store.Nodes()))
	}
	if len(store.Resources()) != 1 {
		t.Errorf("resources after a failed scan = %d, want 1: a failed scan must not remove records", len(store.Resources()))
	}
	if store.State() != StateUnavailable {
		t.Errorf("state after a failed scan = %q, want %q", store.State(), StateUnavailable)
	}
}

// A PARTIAL scan is not a complete scan either. What it did observe is
// current; what it did not observe is unknown, not absent.
func TestApply_PartialScanPreservesUnobservedRecords(t *testing.T) {
	store := NewStore()

	store.Apply(ScanResult{
		State:      StateFresh,
		Complete:   true,
		ObservedAt: time.Now(),
		Nodes: []Node{
			{Provider: "p", SourceID: "s", Name: "n1"},
			{Provider: "p", SourceID: "s", Name: "n2"},
		},
	})

	// A partial scan that only saw n1.
	store.Apply(ScanResult{
		State:      StatePartial,
		Complete:   false,
		ObservedAt: time.Now(),
		Nodes: []Node{
			{Provider: "p", SourceID: "s", Name: "n1"},
		},
	})

	if len(store.Nodes()) != 2 {
		t.Errorf("nodes after a partial scan = %d, want 2: a partial scan must not remove records", len(store.Nodes()))
	}
	if len(store.AbsentNodes()) != 0 {
		t.Errorf("absent nodes after a partial scan = %d, want 0: absence requires a COMPLETE scan", len(store.AbsentNodes()))
	}
}

// A COMPLETE scan that did not contain a record is evidence of
// absence - and it is recorded as absence, not acted on by deletion.
func TestApply_CompleteScanRecordsAbsence(t *testing.T) {
	store := NewStore()

	store.Apply(ScanResult{
		State:      StateFresh,
		Complete:   true,
		ObservedAt: time.Now(),
		Nodes: []Node{
			{Provider: "p", SourceID: "s", Name: "n1"},
			{Provider: "p", SourceID: "s", Name: "n2"},
		},
	})

	store.Apply(ScanResult{
		State:      StateFresh,
		Complete:   true,
		ObservedAt: time.Now(),
		Nodes: []Node{
			{Provider: "p", SourceID: "s", Name: "n1"},
		},
	})

	absent := store.AbsentNodes()
	if len(absent) != 1 {
		t.Fatalf("absent nodes = %d, want 1", len(absent))
	}
	if absent[0].Name != "n2" {
		t.Errorf("absent node = %q, want n2", absent[0].Name)
	}
	// The record itself is still there. Removal is an explicit
	// operator action, not a side-effect of a scan.
	if len(store.Nodes()) != 2 {
		t.Errorf("nodes = %d, want 2: the record is marked absent, not deleted", len(store.Nodes()))
	}
}

// Repeated identical scans must be idempotent. This is what makes
// polling safe to run every minute forever.
func TestApply_RepeatedScanIsIdempotent(t *testing.T) {
	store := NewStore()

	scan := ScanResult{
		State:      StateFresh,
		Complete:   true,
		ObservedAt: time.Now(),
		Nodes: []Node{
			{Provider: "p", SourceID: "s", Name: "n1", Fingerprint: "aa", Status: "online"},
		},
		Resources: []Resource{
			{Provider: "p", SourceID: "s", NodeName: "n1", Kind: KindVM, SourceResourceID: "100", Name: "web", State: "running"},
		},
	}

	for i := 0; i < 5; i++ {
		store.Apply(scan)
	}

	if len(store.Nodes()) != 1 {
		t.Errorf("nodes = %d, want 1", len(store.Nodes()))
	}
	if len(store.Resources()) != 1 {
		t.Errorf("resources = %d, want 1", len(store.Resources()))
	}
	if len(store.AbsentNodes()) != 0 {
		t.Errorf("absent nodes = %d, want 0", len(store.AbsentNodes()))
	}
}

// A fingerprint change under a matched key is a CONFLICT, not an
// update. The stored record is left exactly as it was.
//
// This is the check that makes a renamed or re-keyed node visible
// instead of silently accepted.
func TestApply_FingerprintChangeIsAConflict(t *testing.T) {
	store := NewStore()

	store.Apply(ScanResult{
		State:      StateFresh,
		Complete:   true,
		ObservedAt: time.Now(),
		Nodes: []Node{
			{Provider: "p", SourceID: "s", Name: "n1", Fingerprint: "aa", Status: "online"},
		},
	})

	store.Apply(ScanResult{
		State:      StateFresh,
		Complete:   true,
		ObservedAt: time.Now(),
		Nodes: []Node{
			{Provider: "p", SourceID: "s", Name: "n1", Fingerprint: "bb", Status: "online"},
		},
	})

	nodes := store.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(nodes))
	}
	if nodes[0].Fingerprint != "aa" {
		t.Errorf("fingerprint = %q, want aa: a fingerprint change must not overwrite the record", nodes[0].Fingerprint)
	}

	conflicts := store.Conflicts()
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %d, want 1", len(conflicts))
	}
	if conflicts[0].ObservedFingerprint != "bb" || conflicts[0].KnownFingerprint != "aa" {
		t.Errorf("conflict fingerprints = %q/%q, want bb/aa", conflicts[0].ObservedFingerprint, conflicts[0].KnownFingerprint)
	}
}

// A node seen under a NEW key but with a fingerprint already known
// under an OLD key is a RENAME. It must be reported, not filed as a
// second record - which would fork the node's history and orphan the
// old record.
func TestApply_RenameIsAConflictNotANewRecord(t *testing.T) {
	store := NewStore()

	store.Apply(ScanResult{
		State:      StateFresh,
		Complete:   true,
		ObservedAt: time.Now(),
		Nodes: []Node{
			{Provider: "p", SourceID: "s", Name: "old-name", Fingerprint: "aa"},
		},
	})

	store.Apply(ScanResult{
		State:      StateFresh,
		Complete:   true,
		ObservedAt: time.Now(),
		Nodes: []Node{
			{Provider: "p", SourceID: "s", Name: "new-name", Fingerprint: "aa"},
		},
	})

	if len(store.Nodes()) != 1 {
		t.Errorf("nodes = %d, want 1: a rename must not create a second record", len(store.Nodes()))
	}
	if len(store.Conflicts()) != 1 {
		t.Errorf("conflicts = %d, want 1: a rename must be reported", len(store.Conflicts()))
	}
}

// Two different nodes in the same provider must not merge, even if
// they share a name, because their fingerprints differ.
func TestApply_SameNameDifferentFingerprintKeepsBoth(t *testing.T) {
	store := NewStore()

	store.Apply(ScanResult{
		State:      StateFresh,
		Complete:   true,
		ObservedAt: time.Now(),
		Nodes: []Node{
			{Provider: "p", SourceID: "s1", Name: "n1", Fingerprint: "aa"},
		},
	})

	// A DIFFERENT source id, so a different key entirely.
	store.Apply(ScanResult{
		State:      StateFresh,
		Complete:   true,
		ObservedAt: time.Now(),
		Nodes: []Node{
			{Provider: "p", SourceID: "s2", Name: "n1", Fingerprint: "bb"},
		},
	})

	if len(store.Nodes()) != 2 {
		t.Errorf("nodes = %d, want 2: two installations' nodes must not merge on a shared name", len(store.Nodes()))
	}
}

// An empty-but-complete scan is a FACT and must be distinguishable
// from a failed one.
func TestApply_EmptyCompleteScanIsStateEmpty(t *testing.T) {
	store := NewStore()

	store.Apply(ScanResult{
		State:      StateEmpty,
		Complete:   true,
		ObservedAt: time.Now(),
	})

	if store.State() != StateEmpty {
		t.Errorf("state = %q, want %q", store.State(), StateEmpty)
	}
	if len(store.Nodes()) != 0 {
		t.Errorf("nodes = %d, want 0", len(store.Nodes()))
	}
}

// A state and its completeness flag must agree. A fresh state with
// Complete=false is a contradiction, and the store must not report the
// state as fresh.
func TestApply_IncompleteScanNeverReportsFresh(t *testing.T) {
	store := NewStore()

	store.Apply(ScanResult{
		State:      StateFresh,
		Complete:   false,
		ObservedAt: time.Now(),
	})

	if store.State() == StateFresh {
		t.Errorf("state = %q after an incomplete scan; an incomplete scan must not report fresh", store.State())
	}
}

// The source id is recorded from a complete scan, and NOT from a
// failed one - a failed scan did not establish which installation it
// was talking to.
func TestApply_SourceIDRecordedOnlyOnCompleteScan(t *testing.T) {
	store := NewStore()

	store.Apply(ScanResult{
		State:      StateFresh,
		Complete:   true,
		SourceID:   "prod-cluster",
		ObservedAt: time.Now(),
	})
	if store.SourceID() != "prod-cluster" {
		t.Errorf("source id = %q, want prod-cluster", store.SourceID())
	}

	store.Apply(ScanResult{
		State:      StateUnavailable,
		Complete:   false,
		SourceID:   "other-cluster",
		ObservedAt: time.Now(),
	})
	if store.SourceID() != "prod-cluster" {
		t.Errorf("source id = %q after a failed scan, want prod-cluster: a failed scan does not establish a source", store.SourceID())
	}
}
