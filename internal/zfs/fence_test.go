package zfs

// The generation fence is the split-brain barrier ADR-0130 §3 calls "the
// actual split-brain fence". These tests are the proof that a stream
// which would move a target backwards or replay a generation it
// already holds is refused, and that the refusal is a refusal — not a
// warning, not a flag on a plan nobody checks.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestReplSnapshotNameMintsAndParses(t *testing.T) {
	// Every generation the package is willing to mint, including both
	// ends of the 8-digit range. A generation past MaxReplGeneration
	// mints a name the parser can never read back, so it is excluded
	// here on purpose and refused by the fence.
	for _, gen := range []uint32{0, 1, 7, 42, 99999999} {
		name := ReplSnapshotName(gen)
		if want := fmt.Sprintf("apiary-repl-%08d", gen); name != want {
			t.Errorf("ReplSnapshotName(%d) = %q, want %q", gen, name, want)
		}
		if !IsReplSnapshot(name) {
			t.Errorf("IsReplSnapshot(%q) = false, want true", name)
		}
		back, ok := ParseReplSnapshotName(name)
		if !ok || back != gen {
			t.Errorf("ParseReplSnapshotName(%q) = (%d, %v), want (%d, true)", name, back, ok, gen)
		}
	}
}

func TestParseReplSnapshotNameRejectsEverythingItDidNotMint(t *testing.T) {
	// Every one of these must be rejected. Accepting a hand-made name
	// as a generation is how a stale snapshot gets mistaken for a
	// replication point, and the fence is the last thing standing
	// between that mistake and a stream sent from the wrong place.
	for _, name := range []string{
		"",
		"-",
		"none",
		"apiary-repl-",
		"apiary-repl-7",         // not zero-padded
		"apiary-repl-000000007", // nine digits
		"apiary-repl-00000007-", // trailing junk
		"apiary-repl-00000007x",
		"apiary-repl-0000000a", // not decimal
		"apiary-repl-0000000-7",
		"apiary-repl-4294967295", // 10 digits: past MaxReplGeneration
		"apiary-repl-100000000",
		"apiary-template",
		"before-migration",
		"pool/vm-1@apiary-repl-00000007", // a full name, not a short one
	} {
		if _, ok := ParseReplSnapshotName(name); ok {
			t.Errorf("ParseReplSnapshotName(%q) accepted a name this package did not mint", name)
		}
		if IsReplSnapshot(name) {
			t.Errorf("IsReplSnapshot(%q) = true, want false", name)
		}
	}
}

func TestParseReplSnapshotNameTrimsPaddingButNotJunk(t *testing.T) {
	// Surrounding whitespace is trimmed, because every line this
	// package reads out of zfs(8) is trimmed, and a padded name is
	// worth more than a pathological dataset whose snapshot name
	// really does begin with a space.
	for _, padded := range []string{"  apiary-repl-00000007", "apiary-repl-00000007  ", "\tapiary-repl-00000007\n"} {
		gen, ok := ParseReplSnapshotName(padded)
		if !ok || gen != 7 {
			t.Errorf("ParseReplSnapshotName(%q) = (%d, %v), want (7, true)", padded, gen, ok)
		}
	}
}

func TestReplSnapshotNamesSortInGenerationOrder(t *testing.T) {
	// Zero padding to 8 digits is what makes a plain string sort agree
	// with the numeric order, which is the property that lets an
	// operator read `zfs list` output and know which generation is
	// newest without running anything.
	gens := []uint32{3, 100, 1, 42, 99999999, 2}
	names := make([]string, 0, len(gens))
	for _, g := range gens {
		names = append(names, ReplSnapshotName(g))
	}
	sort.Strings(names)
	want := []uint32{1, 2, 3, 42, 100, 99999999}
	for i, n := range names {
		got, ok := ParseReplSnapshotName(n)
		if !ok || got != want[i] {
			t.Fatalf("string-sorted names[%d] = %q -> (%d, %v), want generation %d", i, n, got, ok, want[i])
		}
	}
}

func TestPlanSendRefusesStaleOrReplayedGeneration(t *testing.T) {
	// The single most important test in the file. A source that has
	// lost its authority can still reach the target; what it must not
	// be able to do is produce a stream that moves the target
	// backwards, or replays a generation the target already holds.
	cases := []struct {
		name      string
		lastAcked uint32
		requested uint32
	}{
		{"replay of the acked generation", 7, 7},
		{"one generation behind", 7, 6},
		{"far behind", 100, 3},
		{"replay of the very first acked generation", 1, 1},
		{"replay of the maximum generation", MaxGeneration, MaxGeneration},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := SourceState{
				Dataset:                  "vm-1",
				LastAckedGeneration:      tc.lastAcked,
				HaveLastAcked:            true,
				LastAckedSnapshot:        "vm-1@" + ReplSnapshotName(tc.lastAcked),
				LastAckedSnapshotPresent: true,
			}
			plan, err := PlanSend(state, SendRequest{TargetGeneration: tc.requested})
			if err == nil {
				t.Fatalf("PlanSend(generation %d) with last acked %d returned a plan %+v, want a fence refusal",
					tc.requested, tc.lastAcked, plan)
			}
			if !errors.Is(err, ErrFenceRefused) {
				t.Errorf("errors.Is(err, ErrFenceRefused) = false for %v", err)
			}
			var fe *FenceError
			if !errors.As(err, &fe) {
				t.Fatalf("err is not a *FenceError: %v", err)
			}
			if fe.Reason != FenceRefuseStale {
				t.Errorf("Reason = %q, want %q", fe.Reason, FenceRefuseStale)
			}
			if fe.Requested != tc.requested {
				t.Errorf("Requested = %d, want %d", fe.Requested, tc.requested)
			}
			if fe.LastAcked != tc.lastAcked || !fe.LastAckedValid {
				t.Errorf("LastAcked = (%d, valid=%v), want (%d, true)", fe.LastAcked, fe.LastAckedValid, tc.lastAcked)
			}
			if fe.Dataset != "vm-1" {
				t.Errorf("Dataset = %q, want %q", fe.Dataset, "vm-1")
			}
			// A zero plan must come back with the refusal, so a caller
			// that ignores the error cannot accidentally run an empty
			// `zfs send`.
			if plan.Kind != "" || plan.Args != nil {
				t.Errorf("refused PlanSend returned a non-empty plan: %+v", plan)
			}
			if !strings.Contains(fe.Error(), "strictly forward") {
				t.Errorf("refusal message does not say why: %q", fe.Error())
			}
		})
	}
}

func TestPlanSendRefusesStaleEvenWithAUsableResumeToken(t *testing.T) {
	// A token must never be a way around the fence. If a lapsed source
	// can present a token, it still cannot move the target.
	state := SourceState{
		Dataset:             "jail-7",
		LastAckedGeneration: 12,
		HaveLastAcked:       true,
	}
	_, err := PlanSend(state, SendRequest{TargetGeneration: 9, ObservedResumeToken: "1-abcdef0123456789"})
	if !errors.Is(err, ErrFenceRefused) {
		t.Fatalf("a resume token bypassed the generation fence: %v", err)
	}
}

func TestPlanSendRefusesGenerationZero(t *testing.T) {
	// Generation 0 is this package's "nothing acked" sentinel. A
	// stream minted at 0 would be indistinguishable from "no
	// replication has ever happened" to every later comparison, which
	// silently reopens the fence.
	// With no recorded history, generation 0 reaches the
	// representability check and is refused for being the sentinel.
	plan, err := PlanSend(SourceState{Dataset: "vm-1"}, SendRequest{TargetGeneration: 0})
	if !errors.Is(err, ErrFenceRefused) {
		t.Fatalf("PlanSend(generation 0) = %+v, %v; want a fence refusal", plan, err)
	}
	var fe *FenceError
	if !errors.As(err, &fe) || fe.Reason != FenceRefuseNoGeneration {
		t.Fatalf("err = %v, want FenceRefuseNoGeneration", err)
	}
	if fe.Detail == "" {
		t.Error("the generation-0 refusal carries no Detail")
	}

	// With history at 0 (which RecordAck forbids producing) it is
	// caught even earlier as stale, which is also a refusal: the point
	// is only that no input produces a plan.
	plan, err = PlanSend(SourceState{Dataset: "vm-1", HaveLastAcked: true}, SendRequest{TargetGeneration: 0})
	if !errors.Is(err, ErrFenceRefused) {
		t.Fatalf("PlanSend(generation 0) with history at 0 = %+v, %v; want a fence refusal", plan, err)
	}
}

func TestPlanSendRefusesUnrepresentableGeneration(t *testing.T) {
	// MaxReplGeneration+1 mints a 9-digit snapshot name that
	// ParseReplSnapshotName rejects, so every later check would
	// conclude the snapshot was gone. It is refused at the fence rather
	// than minted.
	for _, gen := range []uint32{MaxReplGeneration + 1, 100000000, MaxGeneration} {
		state := SourceState{
			Dataset:                  "vm-1",
			LastAckedGeneration:      MaxReplGeneration - 1,
			HaveLastAcked:            true,
			LastAckedSnapshot:        "vm-1@" + ReplSnapshotName(MaxReplGeneration-1),
			LastAckedSnapshotPresent: true,
		}
		plan, err := PlanSend(state, SendRequest{TargetGeneration: gen})
		if !errors.Is(err, ErrFenceRefused) {
			t.Fatalf("PlanSend(generation %d) = %+v, %v; want a fence refusal", gen, plan, err)
		}
		var fe *FenceError
		if !errors.As(err, &fe) || fe.Reason != FenceRefuseNoGeneration {
			t.Fatalf("generation %d: err = %v, want FenceRefuseNoGeneration", gen, err)
		}
		if !strings.Contains(fe.Detail, "represent") {
			t.Errorf("generation %d: Detail = %q, want it to say the name is unrepresentable", gen, fe.Detail)
		}
	}
	// The cap itself is the last usable generation.
	plan, err := PlanSend(SourceState{Dataset: "vm-1", LastAckedGeneration: MaxReplGeneration - 1, HaveLastAcked: true,
		LastAckedSnapshot: "vm-1@" + ReplSnapshotName(MaxReplGeneration-1), LastAckedSnapshotPresent: true},
		SendRequest{TargetGeneration: MaxReplGeneration})
	if err != nil {
		t.Fatalf("PlanSend(MaxReplGeneration) = %v, want nil", err)
	}
	if plan.ToSnapshot != "vm-1@apiary-repl-99999999" {
		t.Errorf("ToSnapshot = %q", plan.ToSnapshot)
	}
}

func TestPlanSendAllowsStrictlyNewerIncludingGaps(t *testing.T) {
	// A gap is allowed deliberately: refusing one would wedge a
	// dataset whose runs were skipped or failed, which is far worse
	// than skipping them. Monotonicity is the invariant that matters.
	cases := []struct {
		lastAcked uint32
		requested uint32
	}{
		{7, 8},
		{7, 12},   // a gap
		{7, 4096}, // a big gap
		{1, 2},
		{MaxReplGeneration - 1, MaxReplGeneration}, // the last representable step
	}
	for _, tc := range cases {
		state := SourceState{
			Dataset:                  "vm-1",
			LastAckedGeneration:      tc.lastAcked,
			HaveLastAcked:            true,
			LastAckedSnapshot:        "vm-1@" + ReplSnapshotName(tc.lastAcked),
			LastAckedSnapshotPresent: true,
		}
		plan, err := PlanSend(state, SendRequest{TargetGeneration: tc.requested})
		if err != nil {
			t.Fatalf("PlanSend(%d) with last acked %d: %v", tc.requested, tc.lastAcked, err)
		}
		if plan.ToGeneration != tc.requested {
			t.Errorf("ToGeneration = %d, want %d", plan.ToGeneration, tc.requested)
		}
	}
}

func TestPlanSendArgumentConstructionPerForm(t *testing.T) {
	// The argument vector IS the contract with zfs(8). Each row is a
	// different kind of send, and a wrong flag here is a wrong stream
	// sent under a healthy-looking status.
	cases := []struct {
		name      string
		state     SourceState
		req       SendRequest
		wantKind  SendKind
		wantFence FenceReason
		wantFrom  string
		wantToken string
		wantArgs  []string
		wantEsc   bool
	}{
		{
			name:      "first ever run is a full send from origin",
			state:     SourceState{Dataset: "vm-1"},
			req:       SendRequest{TargetGeneration: 1},
			wantKind:  SendFull,
			wantFence: FenceAllowNoHistory,
			wantArgs:  []string{"send", "vm-1@apiary-repl-00000001"},
		},
		{
			name: "steady state is an incremental from the last acked snapshot",
			state: SourceState{
				Dataset:                  "jail-7",
				LastAckedGeneration:      12,
				HaveLastAcked:            true,
				LastAckedSnapshot:        "jail-7@apiary-repl-00000012",
				LastAckedSnapshotPresent: true,
			},
			req:       SendRequest{TargetGeneration: 13},
			wantKind:  SendIncremental,
			wantFence: FenceAllowIncremental,
			wantFrom:  "jail-7@apiary-repl-00000012",
			wantArgs:  []string{"send", "-i", "jail-7@apiary-repl-00000012", "jail-7@apiary-repl-00000013"},
		},
		{
			name: "a gap still sends one delta from the last acked snapshot",
			state: SourceState{
				Dataset:                  "jail-7",
				LastAckedGeneration:      12,
				HaveLastAcked:            true,
				LastAckedSnapshot:        "jail-7@apiary-repl-00000012",
				LastAckedSnapshotPresent: true,
			},
			req:       SendRequest{TargetGeneration: 20},
			wantKind:  SendIncremental,
			wantFence: FenceAllowIncremental,
			wantFrom:  "jail-7@apiary-repl-00000012",
			wantArgs:  []string{"send", "-i", "jail-7@apiary-repl-00000012", "jail-7@apiary-repl-00000020"},
		},
		{
			name: "an interrupted first receive is resumed with its token",
			state: SourceState{
				Dataset: "vm-2",
			},
			req:       SendRequest{TargetGeneration: 1, ObservedResumeToken: "1-9a8b7c6d5e4f3a2b"},
			wantKind:  SendResume,
			wantFence: FenceAllowResume,
			wantToken: "1-9a8b7c6d5e4f3a2b",
			wantArgs:  []string{"send", "-t", "1-9a8b7c6d5e4f3a2b"},
		},
		{
			name: "an interrupted incremental is resumed with its token",
			state: SourceState{
				Dataset:             "vm-2",
				LastAckedGeneration: 4,
				HaveLastAcked:       true,
			},
			req:       SendRequest{TargetGeneration: 5, ObservedResumeToken: "1-ffeeddccbbaa9988"},
			wantKind:  SendResume,
			wantFence: FenceAllowResume,
			wantToken: "1-ffeeddccbbaa9988",
			wantArgs:  []string{"send", "-t", "1-ffeeddccbbaa9988"},
		},
		{
			name: "the full resync carries the whole chain from the fallback origin",
			state: SourceState{
				Dataset:                  "vm-3",
				LastAckedGeneration:      9,
				HaveLastAcked:            true,
				LastAckedSnapshot:        "vm-3@apiary-repl-00000009",
				LastAckedSnapshotPresent: false, // it was destroyed
				FallbackFromSnapshot:     "vm-3@apiary-repl-00000007",
			},
			req:       SendRequest{TargetGeneration: 10},
			wantKind:  SendFullIncremental,
			wantFence: FenceRefuseNoSnapshot,
			wantFrom:  "vm-3@apiary-repl-00000007",
			wantArgs:  []string{"send", "-I", "vm-3@apiary-repl-00000007", "vm-3@apiary-repl-00000010"},
			wantEsc:   true,
		},
		{
			name: "a token for an unrelated generation is discarded, not used",
			state: SourceState{
				Dataset:                  "vm-4",
				LastAckedGeneration:      9,
				HaveLastAcked:            true,
				LastAckedSnapshot:        "vm-4@apiary-repl-00000009",
				LastAckedSnapshotPresent: true,
			},
			// A token observed for a stream that is not this run's.
			req:       SendRequest{TargetGeneration: 12, ObservedResumeToken: "1-aaaaaaaaaaaaaaaa"},
			wantKind:  SendIncremental,
			wantFence: FenceAllowIncremental,
			wantFrom:  "vm-4@apiary-repl-00000009",
			wantArgs:  []string{"send", "-i", "vm-4@apiary-repl-00000009", "vm-4@apiary-repl-00000012"},
		},
		{
			name: "a token on a policy with no history is only used for generation 1",
			state: SourceState{
				Dataset: "vm-5",
			},
			req:      SendRequest{TargetGeneration: 3, ObservedResumeToken: "1-bbbbbbbbbbbbbbbb"},
			wantKind: SendFull,
			// FenceAllowNoHistory: with nothing ever acked there is no
			// history for a token to belong to, so the first send is
			// full and the token is reported as discarded.
			wantFence: FenceAllowNoHistory,
			wantArgs:  []string{"send", "vm-5@apiary-repl-00000003"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanSend(tc.state, tc.req)
			if err != nil {
				t.Fatalf("PlanSend() error: %v", err)
			}
			if plan.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", plan.Kind, tc.wantKind)
			}
			if plan.Fence != tc.wantFence {
				t.Errorf("Fence = %q, want %q", plan.Fence, tc.wantFence)
			}
			if plan.FromSnapshot != tc.wantFrom {
				t.Errorf("FromSnapshot = %q, want %q", plan.FromSnapshot, tc.wantFrom)
			}
			if plan.Token != tc.wantToken {
				t.Errorf("Token = %q, want %q", plan.Token, tc.wantToken)
			}
			if plan.ResumedFromToken != (tc.wantKind == SendResume) {
				t.Errorf("ResumedFromToken = %v, want %v", plan.ResumedFromToken, tc.wantKind == SendResume)
			}
			if plan.EscalatedFromIncremental != tc.wantEsc {
				t.Errorf("EscalatedFromIncremental = %v, want %v", plan.EscalatedFromIncremental, tc.wantEsc)
			}
			if !equalArgs(plan.Args, tc.wantArgs...) {
				t.Errorf("Args = %v, want %v", plan.Args, tc.wantArgs)
			}
			if tc.wantKind == SendResume {
				// A resumed plan must never also carry a dataset name:
				// `zfs send -t <token> <dataset>` is not a command.
				for _, a := range plan.Args {
					if strings.Contains(a, "@") {
						t.Errorf("resume plan carries a snapshot name %q in %v", a, plan.Args)
					}
				}
			}
			if plan.ToSnapshot != tc.state.Dataset+"@"+ReplSnapshotName(tc.req.TargetGeneration) {
				t.Errorf("ToSnapshot = %q", plan.ToSnapshot)
			}
		})
	}
}

func TestPlanSendEscalationIsAlwaysReported(t *testing.T) {
	// ADR-0130: a full resend "is an event the operator should see,
	// not a quiet recovery". Every path that falls back to a full send
	// after a generation was already acked must say so, in a field and
	// in words.
	escalated := []struct {
		name  string
		state SourceState
	}{
		{
			name: "last acked snapshot destroyed, older origin available",
			state: SourceState{
				Dataset:                  "vm-1",
				LastAckedGeneration:      9,
				HaveLastAcked:            true,
				LastAckedSnapshot:        "vm-1@apiary-repl-00000009",
				LastAckedSnapshotPresent: false,
				FallbackFromSnapshot:     "vm-1@apiary-repl-00000007",
			},
		},
		{
			name: "no usable origin at all",
			state: SourceState{
				Dataset:             "vm-1",
				LastAckedGeneration: 9,
				HaveLastAcked:       true,
			},
		},
		{
			name: "generation known but snapshot name unknown",
			state: SourceState{
				Dataset:             "vm-1",
				LastAckedGeneration: 9,
				HaveLastAcked:       true,
				LastAckedSnapshot:   "",
			},
		},
	}
	for _, tc := range escalated {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanSend(tc.state, SendRequest{TargetGeneration: 10})
			if err != nil {
				t.Fatalf("PlanSend() error: %v", err)
			}
			if !plan.EscalatedFromIncremental {
				t.Errorf("EscalatedFromIncremental = false; an incremental was refused and became %q without saying so", plan.Kind)
			}
			if plan.Fence == FenceAllowIncremental || plan.Fence == FenceAllowNoHistory {
				t.Errorf("Fence = %q; an escalation must not be reported as an ordinary allow", plan.Fence)
			}
			if plan.Note == "" {
				t.Errorf("Note is empty; a full resend must carry an operator-readable explanation")
			}
			if plan.Kind != SendFull && plan.Kind != SendFullIncremental {
				t.Errorf("Kind = %q, want a full send form", plan.Kind)
			}
		})
	}

	// A first run has no incremental to escalate from, so it must not
	// claim one.
	first, err := PlanSend(SourceState{Dataset: "vm-1"}, SendRequest{TargetGeneration: 1})
	if err != nil {
		t.Fatalf("PlanSend() error: %v", err)
	}
	if first.EscalatedFromIncremental {
		t.Errorf("first run reported EscalatedFromIncremental = true")
	}
	if first.Note != "" {
		t.Errorf("first run carried a note about escalation it did not perform: %q", first.Note)
	}
}

func TestPlanSendNeverResumesATokenForANonSuccessorGeneration(t *testing.T) {
	// resumeBindsToPending is the whole of the token/generation
	// binding, so it is worth asserting directly across the boundary.
	const token = "1-0123456789abcdef"
	cases := []struct {
		name      string
		have      bool
		lastAcked uint32
		toGen     uint32
		want      bool
	}{
		{"no history, first generation", false, 0, 1, true},
		{"no history, later generation", false, 0, 2, false},
		{"immediate successor", true, 7, 8, true},
		{"a gap of one", true, 7, 9, false},
		{"a large gap", true, 7, 100, false},
		{"the same generation", true, 7, 7, false},
		{"an older generation", true, 7, 3, false},
	}
	for _, tc := range cases {
		state := SourceState{Dataset: "vm-1", HaveLastAcked: tc.have, LastAckedGeneration: tc.lastAcked}
		plan, err := PlanSend(state, SendRequest{TargetGeneration: tc.toGen, ObservedResumeToken: token})
		if tc.want {
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if plan.Kind != SendResume {
				t.Errorf("%s: Kind = %q, want %q", tc.name, plan.Kind, SendResume)
			}
			if plan.Note != "" {
				t.Errorf("%s: a used token also reported a discard: %q", tc.name, plan.Note)
			}
			continue
		}
		if err != nil {
			// A generation at or below the last acked one is refused
			// outright by the fence, which is a stronger answer than
			// "not this token". Both are acceptable here; a plan at
			// all is not.
			if !errors.Is(err, ErrFenceRefused) {
				t.Fatalf("%s: %v", tc.name, err)
			}
			continue
		}
		if plan.Kind == SendResume {
			t.Errorf("%s: a token was used for a stream it does not belong to", tc.name)
		}
		if !strings.Contains(plan.Note, "discarded resume token") {
			t.Errorf("%s: the discarded token was not reported: Note = %q", tc.name, plan.Note)
		}
	}
}

func TestPlanSendRequiresADataset(t *testing.T) {
	if _, err := PlanSend(SourceState{}, SendRequest{TargetGeneration: 1}); err == nil {
		t.Fatal("PlanSend with no dataset returned no error")
	}
}

func TestLastAckedRecordAckIsMonotonic(t *testing.T) {
	// ADR-0130: "RecordReplicationAck with a generation lower than
	// last_acked_generation is rejected, so a late-arriving ack from a
	// run whose lease lapsed can never roll the counter backwards."
	la := NewLastAcked("policy-1", "vm-1")
	if la.HaveGeneration {
		t.Errorf("a fresh LastAcked claims a generation is known")
	}
	next, err := la.RecordAck(3)
	if err != nil {
		t.Fatalf("RecordAck(3): %v", err)
	}
	if !next.HaveGeneration || next.Generation != 3 {
		t.Fatalf("RecordAck(3) = %+v", next)
	}
	if next.Snapshot != "vm-1@apiary-repl-00000003" {
		t.Errorf("Snapshot = %q, want the generation's own snapshot name", next.Snapshot)
	}
	for _, gen := range []uint32{1, 2, 3} {
		rolled, err := next.RecordAck(gen)
		if !errors.Is(err, ErrGenerationRollback) {
			t.Errorf("RecordAck(%d) on a record already at 3 = %+v, %v; want ErrGenerationRollback", gen, rolled, err)
		}
		if rolled.Generation != 3 {
			t.Errorf("a refused RecordAck(%d) mutated the record to %d", gen, rolled.Generation)
		}
	}
	if _, err := NewLastAcked("policy-1", "vm-1").RecordAck(0); err == nil {
		t.Error("RecordAck(0) on a record with no history was accepted; 0 is the 'nothing acked' sentinel")
	}
}

func TestLastAckedNextGenerationNeverWraps(t *testing.T) {
	empty := NewLastAcked("policy-1", "vm-1")
	if g, err := empty.NextGeneration(); err != nil || g != 1 {
		t.Errorf("NextGeneration() on an empty record = (%d, %v), want (1, nil)", g, err)
	}
	atMax := NewLastAckedAt("policy-1", "vm-1", MaxReplGeneration, "vm-1@apiary-repl-99999999")
	if _, err := atMax.NextGeneration(); !errors.Is(err, ErrGenerationExhausted) {
		t.Errorf("NextGeneration() at the cap = %v, want ErrGenerationExhausted", err)
	}
	// Both wrapping past uint32 and running past the 8-digit snapshot
	// name would make a later generation look older than an earlier
	// one, which is exactly the divergence the fence exists to
	// prevent, so the counter stops at the name's limit.
	below := NewLastAckedAt("policy-1", "vm-1", MaxReplGeneration-1, "vm-1@apiary-repl-99999998")
	if g, err := below.NextGeneration(); err != nil || g != MaxReplGeneration {
		t.Errorf("NextGeneration() just below the cap = (%d, %v), want (%d, nil)", g, err, MaxReplGeneration)
	}
}

func TestLastAckedFenceCheckIsTheTargetSideOfTheSameFence(t *testing.T) {
	// The receiving node refuses a stale stream with exactly the
	// decision the source-side planner uses, so the two ends cannot
	// drift apart. One function, two call sites.
	la := NewLastAckedAt("policy-1", "vm-1", 7, "vm-1@apiary-repl-00000007")
	if err := la.FenceCheck(7); !errors.Is(err, ErrFenceRefused) {
		t.Errorf("FenceCheck(7) on a target holding 7 = %v, want a fence refusal", err)
	} else {
		var fe *FenceError
		if !errors.As(err, &fe) || fe.Reason != FenceRefuseStale {
			t.Errorf("FenceCheck(7) err = %v, want a FenceRefuseStale *FenceError", err)
		}
	}
	if err := la.FenceCheck(3); !errors.Is(err, ErrFenceRefused) {
		t.Errorf("FenceCheck(3) on a target holding 7 = %v, want a fence refusal", err)
	}
	if err := la.FenceCheck(8); err != nil {
		t.Errorf("FenceCheck(8) on a target holding 7 = %v, want nil", err)
	}
	// With no history, the first generation is legitimate.
	empty := NewLastAcked("policy-1", "vm-1")
	if err := empty.FenceCheck(1); err != nil {
		t.Errorf("FenceCheck(1) on a target with no history = %v, want nil", err)
	}
}

func TestLastAckedLagReportsUnknownRatherThanZero(t *testing.T) {
	la := NewLastAckedAt("policy-1", "vm-1", 10, "vm-1@apiary-repl-00000010")
	lag, known := la.Lag(7, true)
	if !known || lag != 3 {
		t.Errorf("Lag(7, true) = (%d, %v), want (3, true)", lag, known)
	}
	if lag, known := la.Lag(10, true); !known || lag != 0 {
		t.Errorf("Lag(10, true) = (%d, %v), want (0, true): an in-sync target is known to be in sync", lag, known)
	}
	// The important one: an unobserved target generation is unknown,
	// not zero lag. Reporting 0 for "we could not check" is how a
	// backup system tells an operator it is fine.
	if _, known := la.Lag(0, false); known {
		t.Error("Lag(_, false) reported a known lag; an unobserved target generation must be unknown")
	}
	if _, known := la.Lag(11, true); known {
		t.Error("Lag(11, true) reported a known lag for a target ahead of the source")
	}
	empty := NewLastAcked("policy-1", "vm-1")
	if _, known := empty.Lag(0, true); known {
		t.Error("Lag on a record with no acked generation reported a known lag")
	}
}

func TestLastAckedSourceStateCarriesPresence(t *testing.T) {
	la := NewLastAckedAt("policy-1", "vm-1", 7, "vm-1@apiary-repl-00000007")
	state := la.SourceState(true)
	if state.Dataset != "vm-1" || !state.HaveLastAcked || state.LastAckedGeneration != 7 {
		t.Fatalf("SourceState(true) = %+v", state)
	}
	if !state.LastAckedSnapshotPresent {
		t.Error("SourceState(true) did not carry snapshot presence through")
	}
	if (la.SourceState(false)).LastAckedSnapshotPresent {
		t.Error("SourceState(false) claimed the snapshot is present")
	}
	if (NewLastAcked("p", "vm-1").SourceState(true)).HaveLastAcked {
		t.Error("an empty LastAcked produced a source state claiming a generation is known")
	}
}

func TestStagingDatasetName(t *testing.T) {
	name, err := StagingDatasetName("policy-1")
	if err != nil {
		t.Fatalf("StagingDatasetName: %v", err)
	}
	if want := StagingDatasetPrefix + "/policy-1"; name != want {
		t.Errorf("StagingDatasetName = %q, want %q", name, want)
	}
	// The name is a path component under a root-owned directory, so a
	// traversal here is a filesystem escape, not a cosmetic problem.
	for _, bad := range []string{"", "..", "../etc/passwd", "a/b", ".hidden", "with space", "nul\x00", strings.Repeat("x", 129)} {
		if _, err := StagingDatasetName(bad); !errors.Is(err, ErrTokenPolicyID) {
			t.Errorf("StagingDatasetName(%q) = %v, want ErrTokenPolicyID", bad, err)
		}
	}
}

func TestReplStateFieldNoteIsNotAFreeFloatingClaim(t *testing.T) {
	// The package refuses to allocate a state.proto field number, so it
	// documents the requirement instead. If the note is ever deleted
	// the requirement is lost, and a field number collision across
	// concurrent workers is the kind of bug that only shows up as a
	// silently wrong raft field.
	if ReplStateFieldNote == "" {
		t.Error("ReplStateFieldNote is empty; the required state.proto field must stay documented in the package")
	}
	for _, want := range []string{"ReplicationProgress", "replication_progress", "FSMSnapshotState", "last_acked_known", "last_acked_snapshot", "policy_id"} {
		if !strings.Contains(ReplStateFieldNote, want) {
			t.Errorf("ReplStateFieldNote does not mention %q", want)
		}
	}
}
