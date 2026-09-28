package guardrail

import (
	"strings"
	"testing"
)

// The join-log guardrail is the only one here whose dangerous input is the
// ABSENCE of evidence, so most of what follows is about the unobserved
// case. If a future edit ever lets it return Allow for a fact with no
// observation, ApproveJoinRequest would perform the one irreversible
// action in this codebase on the strength of nothing.

// TestEvaluateJoinLogState_ObservedEmptyIsAllow is the one shape that may
// be approved: somebody read the joiner's own raft and it reported no log
// entries at all.
func TestEvaluateJoinLogState_ObservedEmptyIsAllow(t *testing.T) {
	got := EvaluateJoinLogState(JoinerLogStateFact{NodeID: "node02", Observed: true, LastLogIndex: 0})
	if got.Verdict != Allow {
		t.Errorf("Verdict = %v, want Allow for an observed empty log", got.Verdict)
	}
	if len(got.Findings) != 0 {
		t.Errorf("Findings = %v, want none for an allowed verdict", got.Findings)
	}
}

// TestEvaluateJoinLogState_ObservedNonEmptyIsBlock is the case the whole
// guardrail exists for: a Comb that was previously part of a cluster, or
// bootstrapped on its own, carrying log entries into an AddVoter.
func TestEvaluateJoinLogState_ObservedNonEmptyIsBlock(t *testing.T) {
	for _, index := range []uint64{1, 2, 4096} {
		got := EvaluateJoinLogState(JoinerLogStateFact{NodeID: "node02", Observed: true, LastLogIndex: index})
		if got.Verdict != Block {
			t.Errorf("LastLogIndex %d: Verdict = %v, want Block", index, got.Verdict)
		}
		if len(got.Findings) != 1 || got.Findings[0].Rule != "joiner-log-state" {
			t.Errorf("LastLogIndex %d: Findings = %v, want one join-log-state finding", index, got.Findings)
		}
	}
}

// TestEvaluateJoinLogState_UnobservedIsNeverAllow is the fail-closed rule.
// LastLogIndex is left at zero in every case here on purpose: that is
// exactly what a caller with no evidence looks like, and it must not be
// mistaken for a report of zero.
func TestEvaluateJoinLogState_UnobservedIsNeverAllow(t *testing.T) {
	got := EvaluateJoinLogState(JoinerLogStateFact{NodeID: "node02"})
	if got.Verdict == Allow {
		t.Fatal("Verdict = Allow for a fact with no observation, want a refusal - this is the whole fail-closed rule")
	}
	if got.Verdict != Unknown {
		t.Errorf("Verdict = %v, want Unknown (not Block: nothing is known to be wrong)", got.Verdict)
	}
	if len(got.Findings) != 1 || got.Findings[0].Rule != "joiner-log-state" {
		t.Fatalf("Findings = %v, want one join-log-state finding", got.Findings)
	}
}

// A failed read must be distinguishable from a silent one, or an operator
// staring at a refusal has no idea whether to go fix raftd or go wipe the
// joiner.
func TestEvaluateJoinLogState_ReadErrorIsSurfaced(t *testing.T) {
	got := EvaluateJoinLogState(JoinerLogStateFact{NodeID: "node02", ReadError: "raft: dialing raftd: connection refused"})
	if got.Verdict != Unknown {
		t.Fatalf("Verdict = %v, want Unknown", got.Verdict)
	}
	if !strings.Contains(got.Findings[0].Detail, "connection refused") {
		t.Errorf("Detail = %q, want it to carry the underlying read error", got.Findings[0].Detail)
	}
}

// A refusal has to tell the operator what to DO, not merely that something
// is wrong, or it is a dead end.
func TestEvaluateJoinLogState_BlockSaysWhatToDo(t *testing.T) {
	got := EvaluateJoinLogState(JoinerLogStateFact{NodeID: "node02", Observed: true, LastLogIndex: 77})
	if got.Verdict != Block {
		t.Fatalf("Verdict = %v, want Block", got.Verdict)
	}
	for _, want := range []string{"77", "wipe", "data_dir"} {
		if !strings.Contains(strings.ToLower(got.Findings[0].Detail), want) {
			t.Errorf("Detail = %q, want it to mention %q", got.Findings[0].Detail, want)
		}
	}
}

func TestCombine_AllowsOnlyWhenEveryInputAllows(t *testing.T) {
	allow := Report{Intent: "approve-join-request", Verdict: Allow}
	got := Combine(allow, allow)
	if got.Verdict != Allow {
		t.Errorf("Verdict = %v, want Allow when every input allows", got.Verdict)
	}
	if got.Intent != "approve-join-request" {
		t.Errorf("Intent = %q, want the first non-empty intent carried through", got.Intent)
	}
}

func TestCombine_BlockOutranksUnknown(t *testing.T) {
	unknown := Report{Intent: "approve-join-request", Verdict: Unknown, Findings: []Finding{{Rule: "joiner-log-state"}}}
	block := Report{Intent: "approve-join-request", Verdict: Block, Findings: []Finding{{Rule: "join-reachability"}}}

	if got := Combine(block, unknown); got.Verdict != Block {
		t.Errorf("Block+Unknown: Verdict = %v, want Block", got.Verdict)
	}
	if got := Combine(unknown, block); got.Verdict != Block {
		t.Errorf("Unknown+Block: Verdict = %v, want Block - order must not matter", got.Verdict)
	}
	if got := Combine(unknown, unknown); got.Verdict != Unknown {
		t.Errorf("Unknown+Unknown: Verdict = %v, want Unknown", got.Verdict)
	}
	// Allow must never survive alongside anything else.
	allow := Report{Intent: "approve-join-request", Verdict: Allow}
	if got := Combine(allow, unknown); got.Verdict == Allow {
		t.Error("Allow+Unknown: Verdict = Allow, want a refusal")
	}
}

// Combine keeps every finding, so an operator sees everything that is
// wrong rather than only whichever check happened to run first. Blocking
// findings come first, because the join preflight page renders only
// Findings()[0] and that is the one the operator actually reads.
func TestCombine_OrdersBlockingFindingsFirst(t *testing.T) {
	unobserved := Report{Intent: "approve-join-request", Verdict: Unknown, Findings: []Finding{{Rule: "joiner-log-state"}}}
	unreachable := Report{Intent: "approve-join-request", Verdict: Block, Findings: []Finding{{Rule: "join-reachability"}}}

	// Log state first in the input order, reachability second. The blocking
	// one must still lead the output.
	got := Combine(unobserved, unreachable)
	if got.Verdict != Block {
		t.Fatalf("Verdict = %v, want Block", got.Verdict)
	}
	if len(got.Findings) != 2 {
		t.Fatalf("Findings = %v, want both kept", got.Findings)
	}
	if got.Findings[0].Rule != "join-reachability" {
		t.Errorf("Findings[0].Rule = %q, want the blocking join-reachability finding first", got.Findings[0].Rule)
	}
	if got.Findings[1].Rule != "joiner-log-state" {
		t.Errorf("Findings[1].Rule = %q, want the unobserved log finding second", got.Findings[1].Rule)
	}
}

// Combine keeps every finding, so an operator sees everything that is
// wrong rather than only whichever check happened to run first.
func TestCombine_KeepsEveryFinding(t *testing.T) {
	a := Report{Intent: "approve-join-request", Verdict: Allow}
	b := Report{Intent: "approve-join-request", Verdict: Block, Findings: []Finding{{Rule: "joiner-log-state"}}}
	c := Report{Intent: "approve-join-request", Verdict: Unknown, Findings: []Finding{{Rule: "some-other-check"}}}

	got := Combine(a, b, c)
	if len(got.Findings) != 2 {
		t.Fatalf("Findings = %v, want both non-allowing inputs' findings kept", got.Findings)
	}
	if got.Findings[0].Rule != "joiner-log-state" || got.Findings[1].Rule != "some-other-check" {
		t.Errorf("Findings = %v, want the blocking finding ahead of the unobserved one", got.Findings)
	}
}
