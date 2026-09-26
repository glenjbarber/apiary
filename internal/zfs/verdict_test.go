package zfs

// ReplicaVerdict classification: the success / stale / unknown
// distinction at the layer an operator actually reads.
//
// The two load-bearing rules, from ADR-0130 §6 and
// internal/cluster/simulate.go, are:
//
//  1. replica_unobserved means a query was attempted and produced
//     nothing usable. It is never folded into current or into stale.
//  2. "The target answered" is not "the target is current". A target
//     that answers promptly and reports an old generation is stale,
//     full stop.

import (
	"strings"
	"testing"
)

func observedTarget(held uint32, haveHeld bool) TargetObservation {
	return TargetObservation{
		Dataset:         "vm-1",
		NodeID:          "comb-2",
		Observed:        true,
		DatasetObserved: true,
		DatasetExists:   true,
		HaveHeld:        haveHeld,
		HeldGeneration:  held,
		Token:           ObservedNoToken("vm-1"),
	}
}

func TestClassifyVerdictTable(t *testing.T) {
	cases := []struct {
		name      string
		obs       TargetObservation
		expected  uint32
		expectOK  bool
		want      ReplicaVerdict
		wantWords []string
	}{
		{
			name:     "the target holds exactly the last acked generation",
			obs:      observedTarget(7, true),
			expected: 7, expectOK: true,
			want: ReplicaCurrent,
			// The evidence text names the generation, because
			// freshness is the generation comparison and only that.
			wantWords: []string{"apiary-repl-00000007", "last acked"},
		},
		{
			name:     "a target behind by one is stale, not 'not current'",
			obs:      observedTarget(6, true),
			expected: 7, expectOK: true,
			want:      ReplicaStale,
			wantWords: []string{"apiary-repl-00000006", "1 generation(s) behind"},
		},
		{
			name:     "a target far behind is stale by the same rule",
			obs:      observedTarget(2, true),
			expected: 99, expectOK: true,
			want:      ReplicaStale,
			wantWords: []string{"97 generation(s) behind"},
		},
		{
			name:     "a target holding nothing while a generation was expected is stale",
			obs:      observedTarget(0, false),
			expected: 7, expectOK: true,
			want:      ReplicaStale,
			wantWords: []string{"no apiary-repl snapshot", "generation 7"},
		},
		{
			name:     "a missing dataset with a generation expected is a confirmed absence",
			obs:      func() TargetObservation { o := observedTarget(0, false); o.DatasetExists = false; return o }(),
			expected: 7, expectOK: true,
			want:      ReplicaStale,
			wantWords: []string{"does not exist", "holds nothing"},
		},
		{
			name:     "a target holding nothing while nothing was expected is current",
			obs:      func() TargetObservation { o := observedTarget(0, false); o.DatasetExists = false; return o }(),
			expected: 0, expectOK: false,
			want:      ReplicaCurrent,
			wantWords: []string{"nothing to be current about"},
		},
		{
			name: "a live resume token is a confirmed interrupted receive",
			obs: func() TargetObservation {
				o := observedTarget(7, true)
				o.Token = ObservedLiveToken("vm-1", "1-abc")
				return o
			}(),
			expected: 7, expectOK: true,
			// A partial receive beats the generation comparison: a
			// destination mid-receive is not holding the last acked
			// generation cleanly, whatever snapshots it lists.
			want:      ReplicaPartialReceive,
			wantWords: []string{"interrupted", "resumable"},
		},
		{
			name: "a partial receive is not current even when the generation matches",
			obs: func() TargetObservation {
				o := observedTarget(99, true)
				o.Token = ObservedLiveToken("vm-1", "1-abc")
				return o
			}(),
			expected: 99, expectOK: true,
			want: ReplicaPartialReceive,
		},
		{
			name:     "a target ahead of the source is neither current nor stale",
			obs:      observedTarget(12, true),
			expected: 7, expectOK: true,
			want:      ReplicaAheadOfSource,
			wantWords: []string{"NEWER", "repoint"},
		},
		{
			name:     "no query at all is unobserved",
			obs:      TargetObservation{Dataset: "vm-1", NodeID: "comb-2", Detail: "the peer never answered"},
			expected: 7, expectOK: true,
			want:      ReplicaUnobserved,
			wantWords: []string{"not queried at all", "could not check"},
		},
		{
			name: "an unanswered resume query is unobserved, not current",
			obs: func() TargetObservation {
				o := observedTarget(7, true)
				o.Token = ObservedUnknownToken("vm-1", "", "the get timed out")
				return o
			}(),
			expected: 7, expectOK: true,
			want:      ReplicaUnobserved,
			wantWords: []string{"could not be established", "could not check"},
		},
		{
			name:     "a resume state that was never asked about is unobserved",
			obs:      func() TargetObservation { o := observedTarget(7, true); o.Token = ResumeTokenObservation{}; return o }(),
			expected: 7, expectOK: true,
			want: ReplicaUnobserved,
			// This is the case that would otherwise let a caller that
			// read only the generation report a clean target.
			wantWords: []string{"never established", "not the same as asking"},
		},
		{
			name: "an undetermined dataset existence is unobserved, not stale",
			obs: func() TargetObservation {
				o := observedTarget(7, true)
				o.DatasetObserved = false
				o.Detail = "the target RPC returned nothing usable"
				return o
			}(),
			expected: 7, expectOK: true,
			want:      ReplicaUnobserved,
			wantWords: []string{"could not be established"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := Classify(tc.obs, tc.expected, tc.expectOK)
			if got != tc.want {
				t.Errorf("Classify() = %q, want %q (why: %s)", got, tc.want, why)
			}
			if why == "" {
				t.Error("every verdict needs an operator-readable reason")
			}
			for _, w := range tc.wantWords {
				if !strings.Contains(why, w) {
					t.Errorf("reason %q does not mention %q", why, w)
				}
			}
		})
	}
}

func TestClassifyNeverCollapsesUnobservedIntoEitherNeighbour(t *testing.T) {
	// The property, asserted over every partially-filled observation
	// rather than a handful of hand-picked ones: any observation with
	// a hole in it is unobserved, and no combination of holes produces
	// a healthy-looking verdict.
	unobserved := []TargetObservation{
		{},
		{Dataset: "vm-1"},
		{Observed: true},
		{Observed: true, DatasetObserved: true, DatasetExists: true},
		{Observed: false, DatasetObserved: true, DatasetExists: true, HaveHeld: true, HeldGeneration: 7, Token: ObservedNoToken("vm-1")},
		{Observed: true, DatasetObserved: false, DatasetExists: true, HaveHeld: true, HeldGeneration: 7, Token: ObservedNoToken("vm-1")},
		{Observed: true, DatasetObserved: true, Token: ResumeTokenObservation{}},
		{Observed: true, DatasetObserved: true, Token: ObservedUnknownToken("vm-1", "x", "y")},
	}
	for i, obs := range unobserved {
		got, why := Classify(obs, 7, true)
		if got == ReplicaCurrent || got == ReplicaStale {
			t.Errorf("observation %d produced %q; a hole in an observation must be unobserved, not a verdict about the copy", i, got)
		}
		_ = why
	}
}

func TestClassifyDoesNotInventAFenceFromSilence(t *testing.T) {
	// "not observed" must not be a fence error either: nothing was
	// compared, so nothing was refused. The reason text has to say
	// which, and unobserved is the honest one.
	got, why := Classify(TargetObservation{Dataset: "vm-1", NodeID: "comb-2"}, 7, true)
	if got != ReplicaUnobserved {
		t.Fatalf("Classify = %q, want %q", got, ReplicaUnobserved)
	}
	if !strings.Contains(why, "comb-2") {
		t.Errorf("the reason does not name the node that could not be observed: %q", why)
	}
}

func TestReplicaVerdictSpellingMatchesTheADR(t *testing.T) {
	// These strings are wire values in a status line and in an
	// evidence list. They are ADR-0130's table verbatim, and the one
	// addition is a separate value rather than a re-spelling of an
	// existing one.
	want := map[ReplicaVerdict]string{
		ReplicaCurrent:        "replica_current",
		ReplicaStale:          "replica_stale",
		ReplicaPartialReceive: "replica_partial_receive",
		ReplicaUnobserved:     "replica_unobserved",
		ReplicaUnprotected:    "replica_unprotected",
		ReplicaDisabled:       "replica_disabled",
		ReplicaAheadOfSource:  "replica_ahead_of_source",
	}
	for got, spelling := range want {
		if string(got) != spelling {
			t.Errorf("verdict %q spelled as %q, want %q", got, string(got), spelling)
		}
	}
	// replica_unobserved is the one that must match
	// internal/cluster/simulate.go's spelling exactly, or a HAST
	// answer and a replication answer could be read as the same
	// thing.
	if string(ReplicaUnobserved) != "replica_unobserved" {
		t.Errorf("the unobserved verdict does not match internal/cluster's spelling: %q", ReplicaUnobserved)
	}
}

func TestHighestReplGenerationIgnoresNonReplicationSnapshots(t *testing.T) {
	// A VM's own rollback snapshots are not replication history. If
	// they counted, a target holding no replicated data at all would
	// report as holding the newest snapshot it owns.
	cases := []struct {
		name  string
		snaps []string
		want  uint32
		ok    bool
	}{
		{"nothing at all", nil, 0, false},
		{"only foreign snapshots", []string{"before-migration", "apiary-template", "manual-1"}, 0, false},
		{"one replication snapshot", []string{"apiary-repl-00000003"}, 3, true},
		{
			name:  "the highest replication generation wins regardless of order",
			snaps: []string{"apiary-repl-00000009", "before-migration", "apiary-repl-00000012", "apiary-repl-00000010"},
			want:  12, ok: true,
		},
		{
			name:  "zero is a real generation, not an absence",
			snaps: []string{"apiary-repl-00000000"},
			want:  0, ok: true,
		},
		{
			name:  "a malformed replication-looking name is not history",
			snaps: []string{"apiary-repl-7", "apiary-repl-00000007x"},
			want:  0, ok: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := HighestReplGeneration(tc.snaps)
			if got != tc.want || ok != tc.ok {
				t.Errorf("HighestReplGeneration(%v) = (%d, %v), want (%d, %v)", tc.snaps, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestReplGenerationsIsSortedAndDeduplicated(t *testing.T) {
	got := ReplGenerations([]string{
		"apiary-repl-00000009", "before-migration", "apiary-repl-00000002",
		"apiary-repl-00000009", "apiary-repl-00000012", "apiary-repl-00000002",
	})
	want := []uint32{2, 9, 12}
	if len(got) != len(want) {
		t.Fatalf("ReplGenerations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ReplGenerations = %v, want %v", got, want)
		}
	}
	if len(ReplGenerations(nil)) != 0 {
		t.Error("ReplGenerations(nil) is non-empty")
	}
}
