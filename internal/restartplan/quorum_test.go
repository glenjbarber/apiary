package restartplan

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/cluster"
	"github.com/glenjbarber/apiary/internal/guardrail"
	"github.com/glenjbarber/apiary/internal/invariant"
)

// TestEvaluateQuorumSafetyQuorumArithmetic is the exhaustive table ADR-0125
// §Verification item 1 calls for: 3-voter (quorum 2) and 5-voter
// (quorum 3) clusters across every combination of other-voter
// reachability, plus the cases where the target is not a voter and where
// the probe could not be read at all.
//
// The expected Survives/TotalVoters/QuorumSize/RemainingReachable values
// are written out by hand in each case rather than recomputed by calling
// the same code the implementation calls, so this test can actually fail
// if the delegation to cluster.ComputeQuorumImpact stops matching what
// ADR-0125 §4 says.
func TestEvaluateQuorumSafetyQuorumArithmetic(t *testing.T) {
	// The OTHER voters, i.e. every voter except the target - which is
	// what QuorumFact.OtherVoters means, and what ComputeQuorumImpact
	// counts. Listing the target here as well would make TotalVoters
	// come out one short, so these lists deliberately never contain
	// "comb-a".
	all3 := []string{"comb-b", "comb-c"}
	all5 := []string{"comb-b", "comb-c", "comb-d", "comb-e"}

	cases := []struct {
		name              string
		fact              QuorumFact
		wantVerdict       guardrail.Verdict
		wantRule          string
		wantTotalVoters   uint32
		wantQuorumSize    uint32
		wantRemainingUp   uint32
		wantRemainingUnk  uint32
		wantLeaderFinding bool
	}{
		{
			// 3 voters, quorum 2: restarting one leaves 2, and both
			// answer. The plain case.
			name: "3-voter/0-of-2-other-voters-up means quorum is lost",
			fact: QuorumFact{
				Service: DefaultService, TargetNodeID: "comb-a",
				IsTargetVoter: true, ProbeReadOK: true,
				OtherVoters: reachableVoters(all3),
			},
			wantVerdict: guardrail.Block, wantRule: RuleQuorumSafety,
			wantTotalVoters: 3, wantQuorumSize: 2, wantRemainingUp: 0,
		},
		{
			// 3 voters, quorum 2: one of the two remaining answers, so
			// the survivors are 1, short of 2. This is the exact case
			// pure arithmetic gets wrong ("3 minus 1 is 2, and 2 is a
			// majority of 3") and which ADR-0052 exists to catch.
			name: "3-voter/1-of-2-other-voters-up is still a loss",
			fact: QuorumFact{
				Service: DefaultService, TargetNodeID: "comb-a",
				IsTargetVoter: true, ProbeReadOK: true,
				OtherVoters: reachableVoters(all3, "comb-b"),
			},
			wantVerdict: guardrail.Block, wantRule: RuleQuorumSafety,
			wantTotalVoters: 3, wantQuorumSize: 2, wantRemainingUp: 1,
		},
		{
			name: "3-voter/2-of-2-other-voters-up survives",
			fact: QuorumFact{
				Service: DefaultService, TargetNodeID: "comb-a",
				IsTargetVoter: true, ProbeReadOK: true,
				OtherVoters: reachableVoters(all3, "comb-b", "comb-c"),
			},
			wantVerdict:     guardrail.Allow,
			wantTotalVoters: 3, wantQuorumSize: 2, wantRemainingUp: 2,
		},
		{
			// Unknown is not up. A voter whose dial never produced a
			// result cannot be credited as surviving, and this is the
			// single most important line in the whole evaluator.
			name: "3-voter/1-up-1-unknown counts the unknown as down",
			fact: QuorumFact{
				Service: DefaultService, TargetNodeID: "comb-a",
				IsTargetVoter: true, ProbeReadOK: true,
				OtherVoters: append(reachableVoters(all3, "comb-b")[:1], unknownVoters("comb-c")...),
			},
			wantVerdict: guardrail.Block, wantRule: RuleQuorumSafety,
			wantTotalVoters: 3, wantQuorumSize: 2, wantRemainingUp: 1, wantRemainingUnk: 1,
		},
		{
			name: "3-voter/2-up survives even with nothing unknown",
			fact: QuorumFact{
				Service: DefaultService, TargetNodeID: "comb-a",
				IsTargetVoter: true, ProbeReadOK: true,
				OtherVoters: reachableVoters(all3, "comb-b", "comb-c"),
			},
			wantVerdict:     guardrail.Allow,
			wantTotalVoters: 3, wantQuorumSize: 2, wantRemainingUp: 2,
		},
		{
			name: "5-voter/2-of-4-up is a loss",
			fact: QuorumFact{
				Service: DefaultService, TargetNodeID: "comb-a",
				IsTargetVoter: true, ProbeReadOK: true,
				OtherVoters: reachableVoters(all5, "comb-b", "comb-c"),
			},
			wantVerdict: guardrail.Block, wantRule: RuleQuorumSafety,
			wantTotalVoters: 5, wantQuorumSize: 3, wantRemainingUp: 2,
		},
		{
			name: "5-voter/3-of-4-up survives",
			fact: QuorumFact{
				Service: DefaultService, TargetNodeID: "comb-a",
				IsTargetVoter: true, ProbeReadOK: true,
				OtherVoters: reachableVoters(all5, "comb-b", "comb-c", "comb-d"),
			},
			wantVerdict:     guardrail.Allow,
			wantTotalVoters: 5, wantQuorumSize: 3, wantRemainingUp: 3,
		},
		{
			// A single-voter cluster: restarting the only voter ends the
			// cluster's ability to reach quorum entirely, and the ADR is
			// explicit that this is not "no other voters, therefore safe".
			name: "single-voter cluster loses the cluster entirely",
			fact: QuorumFact{
				Service: DefaultService, TargetNodeID: "comb-a",
				IsTargetVoter: true, ProbeReadOK: true,
			},
			wantVerdict: guardrail.Block, wantRule: RuleQuorumSafety,
			wantTotalVoters: 1, wantQuorumSize: 1, wantRemainingUp: 0,
		},
		{
			// A non-voter (staging server) restart cannot cost quorum. It
			// is Allow even with no other voter reachable - and even
			// though IsTargetVoter was only believable because the read
			// succeeded.
			name: "non-voter target is allowed with zero reachable others",
			fact: QuorumFact{
				Service: DefaultService, TargetNodeID: "comb-a",
				IsTargetVoter: false, ProbeReadOK: true,
				OtherVoters: reachableVoters([]string{"comb-b"}),
			},
			wantVerdict:     guardrail.Allow,
			wantTotalVoters: 1, wantQuorumSize: 1,
		},
		{
			// Fail closed. ADR-0125 §3/§8 are explicit: a probe that
			// could not run is Unknown, never Allow.
			name: "probe that could not run is Unknown, not Allow",
			fact: QuorumFact{
				Service: DefaultService, TargetNodeID: "comb-a",
				ProbeReadOK: false, ProbeError: "raft status read: context deadline exceeded",
			},
			wantVerdict: guardrail.Unknown, wantRule: RuleQuorumSafety,
		},
		{
			// Even with everything else looking perfect, an unreadable
			// probe wins: we cannot establish the target is a non-voter,
			// so "non-voter restarts are safe" is not a conclusion this
			// call is entitled to reach.
			name: "probe failure beats a non-voter claim",
			fact: QuorumFact{
				Service: DefaultService, TargetNodeID: "comb-a",
				IsTargetVoter: false, ProbeReadOK: false,
				ProbeError: "raft status read: no leader",
			},
			wantVerdict: guardrail.Unknown, wantRule: RuleQuorumSafety,
		},
		{
			// Leader vs follower. A follower restart in a healthy cluster
			// is a plain Allow with no leader finding at all.
			name: "healthy follower restart is allowed with no leader finding",
			fact: QuorumFact{
				Service: DefaultService, TargetNodeID: "comb-b",
				IsTargetVoter: true, IsTargetLeader: false, ProbeReadOK: true,
				OtherVoters: reachableVoters([]string{"comb-a", "comb-c"}, "comb-a", "comb-c"),
			},
			wantVerdict:     guardrail.Allow,
			wantTotalVoters: 3, wantQuorumSize: 2, wantRemainingUp: 2,
		},
		{
			// The leader. Quorum survives, but the election is a real
			// availability cost, so ADR-0125 §4 emits a separate,
			// Block-by-default finding for it.
			name: "leader restart blocks on the leader rule while quorum is fine",
			fact: QuorumFact{
				Service: DefaultService, TargetNodeID: "comb-a",
				IsTargetVoter: true, IsTargetLeader: true, ProbeReadOK: true,
				OtherVoters: reachableVoters([]string{"comb-b", "comb-c"}, "comb-b", "comb-c"),
			},
			wantVerdict: guardrail.Block, wantRule: RuleLeaderRestart,
			wantTotalVoters: 3, wantQuorumSize: 2, wantRemainingUp: 2,
			wantLeaderFinding: true,
		},
		{
			// Both findings at once: quorum is lost AND the target is the
			// leader. Both rules must be present, because an operator
			// overriding the leader election does not also fix quorum.
			name: "leader restart on a quorum-losing cluster reports both rules",
			fact: QuorumFact{
				Service: DefaultService, TargetNodeID: "comb-a",
				IsTargetVoter: true, IsTargetLeader: true, ProbeReadOK: true,
				OtherVoters: reachableVoters([]string{"comb-b", "comb-c"}, "comb-b"),
			},
			wantVerdict: guardrail.Block, wantRule: RuleQuorumSafety,
			wantTotalVoters: 3, wantQuorumSize: 2, wantRemainingUp: 1,
			wantLeaderFinding: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := EvaluateQuorumSafety(tc.fact)

			if report.Verdict != tc.wantVerdict {
				t.Errorf("Verdict = %q, want %q (findings %v)", report.Verdict, tc.wantVerdict, rulesOf(report))
			}
			if tc.wantRule != "" && !hasRule(report, tc.wantRule) {
				t.Errorf("missing finding rule %q; got %v", tc.wantRule, rulesOf(report))
			}
			if tc.wantLeaderFinding && !hasRule(report, RuleLeaderRestart) {
				t.Errorf("expected the leader-restart finding; got %v", rulesOf(report))
			}

			// The arithmetic, verified independently of the verdict.
			impact := ComputeQuorum(tc.fact)
			if impact.TotalVoters != tc.wantTotalVoters {
				t.Errorf("TotalVoters = %d, want %d", impact.TotalVoters, tc.wantTotalVoters)
			}
			if impact.QuorumSize != tc.wantQuorumSize {
				t.Errorf("QuorumSize = %d, want %d", impact.QuorumSize, tc.wantQuorumSize)
			}
			if impact.RemainingReachable != tc.wantRemainingUp {
				t.Errorf("RemainingReachable = %d, want %d", impact.RemainingReachable, tc.wantRemainingUp)
			}
			if impact.RemainingUnknown != tc.wantRemainingUnk {
				t.Errorf("RemainingUnknown = %d, want %d", impact.RemainingUnknown, tc.wantRemainingUnk)
			}
			// Survives must agree with the verdict: a Block for
			// quorum-safety reasons means Survives is false, and an Allow
			// (other than the non-voter shortcut) means it is true.
			if tc.wantVerdict == guardrail.Block && tc.wantRule == RuleQuorumSafety && impact.Survives {
				t.Errorf("verdict is Block on quorum-safety but ComputeQuorum reported Survives=true")
			}
		})
	}
}

// TestEvaluateQuorumSafetySurvivesExactlyAtQuorum pins the boundary: the
// verdict must flip exactly at RemainingReachable == QuorumSize, for
// every cluster size from 1 to 7 voters, with no off-by-one in either
// direction. A guardrail that is one voter too generous here is how a
// three-voter cluster ends up with one live node and no quorum.
func TestEvaluateQuorumSafetySurvivesExactlyAtQuorum(t *testing.T) {
	for total := 1; total <= 7; total++ {
		others := make([]string, 0, total-1)
		for i := 0; i < total-1; i++ {
			others = append(others, "peer-"+string(rune('a'+i)))
		}
		quorumSize := uint32(total/2 + 1)
		for up := 0; up <= total-1; up++ {
			t.Run(fmt.Sprintf("voters=%d up=%d quorum=%d", total, up, quorumSize), func(t *testing.T) {
				fact := QuorumFact{
					Service: DefaultService, TargetNodeID: "target",
					IsTargetVoter: true, ProbeReadOK: true,
					OtherVoters: reachableVoters(others, others[:up]...),
				}
				impact := ComputeQuorum(fact)
				if impact.QuorumSize != quorumSize {
					t.Fatalf("QuorumSize = %d, want %d", impact.QuorumSize, quorumSize)
				}
				wantSurvives := uint32(up) >= quorumSize
				if impact.Survives != wantSurvives {
					t.Fatalf("Survives = %v with %d/%d reachable (quorum %d), want %v",
						impact.Survives, up, total-1, quorumSize, wantSurvives)
				}
				report := EvaluateQuorumSafety(fact)
				wantVerdict := guardrail.Allow
				if !wantSurvives {
					wantVerdict = guardrail.Block
				}
				if report.Verdict != wantVerdict {
					t.Errorf("Verdict = %q, want %q", report.Verdict, wantVerdict)
				}
			})
		}
	}
}

// TestEvaluateQuorumSafetyForceSemantics covers ADR-0103/0125's Force
// override exhaustively, because "what does an override actually
// override" is exactly the thing that must not be left to be re-guessed
// per call site.
func TestEvaluateQuorumSafetyForceSemantics(t *testing.T) {
	healthyLeader := QuorumFact{
		Service: DefaultService, TargetNodeID: "comb-a",
		IsTargetVoter: true, IsTargetLeader: true, ProbeReadOK: true,
		OtherVoters: reachableVoters([]string{"comb-b", "comb-c"}, "comb-b", "comb-c"),
	}
	quorumLoss := QuorumFact{
		Service: DefaultService, TargetNodeID: "comb-a",
		IsTargetVoter: true, ProbeReadOK: true,
		OtherVoters: reachableVoters([]string{"comb-b", "comb-c"}, "comb-b"),
	}
	unreadable := QuorumFact{
		Service: DefaultService, TargetNodeID: "comb-a",
		ProbeReadOK: false, ProbeError: "raft status read failed",
	}

	t.Run("force downgrades the leader finding to a cited caveat", func(t *testing.T) {
		fact := healthyLeader
		fact.Force = true
		report := EvaluateQuorumSafety(fact)
		if report.Verdict != guardrail.Allow {
			t.Errorf("Verdict = %q, want allow", report.Verdict)
		}
		if len(report.Findings) != 0 {
			t.Errorf("overridden findings should move to caveats, still present: %v", rulesOf(report))
		}
		if !caveatContaining(report, "operator acknowledged this") {
			t.Errorf("expected an acknowledged-override caveat; got %v", report.Caveats)
		}
		if !caveatContaining(report, "election") {
			t.Errorf("the override caveat must preserve the reason it overrode, not just that it overrode: %v", report.Caveats)
		}
	})

	t.Run("force downgrades a quorum-safety block too", func(t *testing.T) {
		fact := quorumLoss
		fact.Force = true
		report := EvaluateQuorumSafety(fact)
		if report.Verdict != guardrail.Allow {
			t.Errorf("Verdict = %q, want allow - ADR-0103's Force is a blanket override of this guardrail", report.Verdict)
		}
		if !caveatContaining(report, "quorum") {
			t.Errorf("expected the quorum reason preserved as a caveat; got %v", report.Caveats)
		}
	})

	t.Run("force never rescues an unknown", func(t *testing.T) {
		fact := unreadable
		fact.Force = true
		report := EvaluateQuorumSafety(fact)
		if report.Verdict != guardrail.Unknown {
			t.Errorf("Verdict = %q, want unknown - an operator cannot acknowledge their way past a fact nobody established", report.Verdict)
		}
		if !hasRule(report, RuleQuorumSafety) {
			t.Errorf("the unknown must keep its finding, not be softened; got %v", rulesOf(report))
		}
	})

	t.Run("force is a no-op when the verdict is already allow", func(t *testing.T) {
		fact := QuorumFact{
			Service: DefaultService, TargetNodeID: "comb-b",
			IsTargetVoter: true, ProbeReadOK: true,
			OtherVoters: reachableVoters([]string{"comb-a", "comb-c"}, "comb-a", "comb-c"),
		}
		fact.Force = true
		report := EvaluateQuorumSafety(fact)
		if report.Verdict != guardrail.Allow {
			t.Errorf("Verdict = %q, want allow", report.Verdict)
		}
		if len(report.Caveats) != 0 {
			t.Errorf("no findings means no override caveats; got %v", report.Caveats)
		}
	})
}

// TestEvaluateQuorumSafetyMergesExistingReport checks that ADR-0103's own
// cooldown/concurrent-restart findings survive into the merged report
// untouched, that their rules stay distinct from this package's two, and
// that a foreign Block is never softened by this package's Force.
func TestEvaluateQuorumSafetyMergesExistingReport(t *testing.T) {
	existing := &guardrail.Report{
		Intent:  "restart-apiary_raftd",
		Verdict: guardrail.Block,
		Findings: []guardrail.Finding{{
			Rule:   "concurrent-manager-restart",
			Detail: "a restart lease for this service is already held and unconfirmed",
		}},
		Caveats: []invariant.Evidence{{Source: "concurrent-manager-restart", Detail: "lease held by comb-b"}},
	}

	t.Run("an existing block is never softened", func(t *testing.T) {
		fact := QuorumFact{
			Service: DefaultService, TargetNodeID: "comb-a",
			IsTargetVoter: true, ProbeReadOK: true, Force: true,
			OtherVoters: reachableVoters([]string{"comb-b", "comb-c"}, "comb-b", "comb-c"),
			Existing:    existing,
		}
		report := EvaluateQuorumSafety(fact)
		if report.Verdict != guardrail.Block {
			t.Errorf("Verdict = %q, want block - Force here only covers this package's own rules", report.Verdict)
		}
		if !hasRule(report, "concurrent-manager-restart") {
			t.Errorf("the pre-existing finding must survive; got %v", rulesOf(report))
		}
	})

	t.Run("an existing allow does not mask this check's block", func(t *testing.T) {
		ok := &guardrail.Report{Intent: "restart-apiary_raftd", Verdict: guardrail.Allow}
		fact := QuorumFact{
			Service: DefaultService, TargetNodeID: "comb-a",
			IsTargetVoter: true, ProbeReadOK: true,
			OtherVoters: reachableVoters([]string{"comb-b", "comb-c"}),
			Existing:    ok,
		}
		report := EvaluateQuorumSafety(fact)
		if report.Verdict != guardrail.Block {
			t.Errorf("Verdict = %q, want block - the merged verdict is the worse of the two, not the first", report.Verdict)
		}
	})

	t.Run("an unknown from this check wins over an existing allow", func(t *testing.T) {
		ok := &guardrail.Report{Intent: "restart-apiary_raftd", Verdict: guardrail.Allow}
		fact := QuorumFact{
			Service: DefaultService, TargetNodeID: "comb-a",
			ProbeReadOK: false, ProbeError: "no leader", Existing: ok,
		}
		report := EvaluateQuorumSafety(fact)
		if report.Verdict != guardrail.Unknown {
			t.Errorf("Verdict = %q, want unknown", report.Verdict)
		}
	})

	t.Run("an existing block is not downgraded by an unknown read", func(t *testing.T) {
		fact := QuorumFact{
			Service: DefaultService, TargetNodeID: "comb-a",
			ProbeReadOK: false, ProbeError: "no leader", Existing: existing,
		}
		report := EvaluateQuorumSafety(fact)
		// A Block is at least as restrictive as Unknown and the caller
		// treats them identically, so the pre-existing Block is kept and
		// the Unknown finding is still reported alongside it.
		if report.Verdict != guardrail.Block {
			t.Errorf("Verdict = %q, want the pre-existing block to stand", report.Verdict)
		}
		if !hasRule(report, RuleQuorumSafety) {
			t.Errorf("the unreadable-probe finding must still be reported; got %v", rulesOf(report))
		}
	})
}

// TestEvaluateQuorumSafetyEvidence checks the two things an operator (and
// a test) must be able to rely on about the cited evidence: it is stamped
// only when the fact was actually observed, and its text is checkable by
// hand rather than merely asserted.
func TestEvaluateQuorumSafetyEvidence(t *testing.T) {
	observed := time.Date(2026, 9, 26, 15, 4, 5, 0, time.UTC)

	t.Run("a blocked quorum cites the arithmetic and the observation time", func(t *testing.T) {
		fact := QuorumFact{
			Service: DefaultService, TargetNodeID: "comb-a",
			IsTargetVoter: true, ProbeReadOK: true, ObservedAt: observed,
			OtherVoters: reachableVoters([]string{"comb-b", "comb-c"}, "comb-b"),
		}
		report := EvaluateQuorumSafety(fact)
		f, ok := findingFor(report, RuleQuorumSafety)
		if !ok {
			t.Fatalf("no quorum-safety finding; got %v", rulesOf(report))
		}
		if len(f.Evidence) != 1 {
			t.Fatalf("Evidence = %v, want exactly one citation", f.Evidence)
		}
		ev := f.Evidence[0]
		if ev.Source != RuleQuorumSafety {
			t.Errorf("Evidence.Source = %q, want %q", ev.Source, RuleQuorumSafety)
		}
		if !ev.ObservedAt.Equal(observed) {
			t.Errorf("Evidence.ObservedAt = %v, want %v", ev.ObservedAt, observed)
		}
		// "3 voter(s) configured, quorum is 2, 1 of the remaining 2
		// voter(s) confirmed reachable right now" - checkable by hand.
		if !strings.Contains(ev.Detail, "3 voter(s) configured") ||
			!strings.Contains(ev.Detail, "quorum is 2") ||
			!strings.Contains(ev.Detail, "1 of the remaining 2") {
			t.Errorf("arithmetic is not stated in checkable form: %q", ev.Detail)
		}
	})

	t.Run("an unobserved evaluation carries no invented timestamp", func(t *testing.T) {
		fact := QuorumFact{
			Service: DefaultService, TargetNodeID: "comb-a",
			ProbeReadOK: false, ProbeError: "raft status read failed",
			// ObservedAt deliberately unset.
		}
		report := EvaluateQuorumSafety(fact)
		// Guards against a future edit defaulting ObservedAt to
		// time.Now() somewhere in this path: an evidence stamp is a
		// claim about when something was seen.
		for _, c := range report.Caveats {
			if !c.ObservedAt.IsZero() {
				t.Errorf("caveat carries a fabricated observation time: %v", c.ObservedAt)
			}
		}
		f, _ := findingFor(report, RuleQuorumSafety)
		for _, ev := range f.Evidence {
			if !ev.ObservedAt.IsZero() {
				t.Errorf("finding evidence carries a fabricated observation time: %v", ev.ObservedAt)
			}
		}
	})
}

// TestQuorumImpactArithmetic covers the one-liner's own wording, since it
// is operator-facing copy that a test is the only guard on.
func TestQuorumImpactArithmetic(t *testing.T) {
	i := QuorumImpact{TotalVoters: 5, QuorumSize: 3, RemainingReachable: 3, RemainingVoters: 4}
	want := "5 voter(s) configured, quorum is 3, 3 of the remaining 4 voter(s) confirmed reachable right now"
	if got := i.Arithmetic(); got != want {
		t.Errorf("Arithmetic() = %q, want %q", got, want)
	}
}

// TestComputeQuorumWithoutAReadableProbe checks the documented contract
// that a zero-valued impact comes back when there are no facts to compute
// from, rather than a plausible-looking wrong one.
func TestComputeQuorumWithoutAReadableProbe(t *testing.T) {
	fact := QuorumFact{Service: DefaultService, TargetNodeID: "comb-a", ProbeReadOK: false}
	impact := ComputeQuorum(fact)
	if impact.TotalVoters != 0 || impact.QuorumSize != 0 || impact.Survives {
		t.Errorf("ComputeQuorum with ProbeReadOK=false = %+v, want the zero value", impact)
	}
	if impact.Note != "" {
		t.Errorf("an unreadable probe should produce no note at all, got %q", impact.Note)
	}
}

// TestComputeQuorumDefaultsBlankReachabilityToUnknown is a direct test of
// the most dangerous line in the package: a VoterReachability whose
// Reachability field was never set (the zero value, "") must be treated
// as unknown - never as reachable.
func TestComputeQuorumDefaultsBlankReachabilityToUnknown(t *testing.T) {
	fact := QuorumFact{
		Service: DefaultService, TargetNodeID: "comb-a",
		IsTargetVoter: true, ProbeReadOK: true,
		OtherVoters: []VoterReachability{
			{NodeID: "comb-b", RaftBindAddress: "comb-b:19999"},
			{NodeID: "comb-c", RaftBindAddress: "comb-c:19999", Reachability: cluster.ReachabilityReachable},
		},
	}
	impact := ComputeQuorum(fact)
	if impact.RemainingUnknown != 1 {
		t.Errorf("RemainingUnknown = %d, want 1 - a blank reachability is unknown", impact.RemainingUnknown)
	}
	if impact.RemainingReachable != 1 {
		t.Errorf("RemainingReachable = %d, want 1", impact.RemainingReachable)
	}
	if impact.Survives {
		t.Errorf("Survives = true with 1 reachable against a quorum of 2; a blank reachability must never be credited as survival")
	}
}

// TestComputeQuorumReportsUnreachableNodesSorted covers the Unreachable
// field, which exists so an operator can be told which peers drove the
// verdict without re-deriving anything.
func TestComputeQuorumReportsUnreachableNodesSorted(t *testing.T) {
	fact := QuorumFact{
		Service: DefaultService, TargetNodeID: "comb-a",
		IsTargetVoter: true, ProbeReadOK: true,
		OtherVoters: reachableVoters([]string{"comb-z", "comb-b", "comb-m"}, "comb-m"),
	}
	impact := ComputeQuorum(fact)
	if len(impact.Unreachable) != 2 || impact.Unreachable[0] != "comb-b" || impact.Unreachable[1] != "comb-z" {
		t.Errorf("Unreachable = %v, want [comb-b comb-z] sorted", impact.Unreachable)
	}
}
