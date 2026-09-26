package pf

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The host baseline: what this node can honestly say about the half of
// the firewall internal/pf does not own. Pure functions over two raw
// pfctl reads, so every state - including every unknown - is testable
// with no pfctl, no root, no network and no FreeBSD.

// realPFInfoEnabled is the shape `pfctl -s info` prints on a host where
// pf is up. Trimmed to the parts this package reads; the real output has
// kernel version, counters, a state table and a per-interface list.
const realPFInfoEnabled = `Status: Enabled for 0 days 00:31:12 Debug: Urp
       Kernel: 16.0-CURRENT #0
       Counters: [ PktIn 4  PktOut 3   PktsIn 0   PktsOut 0 ]
              [  Packets: [ block: 0 pass: 0 match: 0 ] ]
              [ States: [ pps: 0  ] ]
`

const realPFInfoDisabled = "Status: Disabled\n"

// measuredLiveCombsRuleset is the main ruleset of a real, live, four-node
// production cluster node, read-only, copied here verbatim because it is
// the evidence this entire file is built on. One rule, no policy, no
// skip, and an empty apiary anchor - while pf reported Status: Enabled.
//
//	# cat /etc/pf.conf
//	anchor "apiary/*"
//	# pfctl -sr
const measuredLiveCombsRuleset = `anchor "apiary/*" all
`

// stockFreeBSDRuleset is a plausible hand-maintained main ruleset: loopback
// skipped, a default policy that drops, an explicit block rule, and the
// apiary namespace anchored. The contrast with the measured one is the
// whole comparison.
const stockFreeBSDRuleset = `scrub on lo
set skip on lo
set block-policy drop
block return in log from any to any
pass in on em0 proto tcp to port { 17700, 8080 } keep state
anchor "apiary/*" all
`

func reads(info string, infoErr error, rules string, rulesErr error) HostReads {
	return HostReads{Info: info, InfoErr: infoErr, MainRuleset: rules, MainRulesetErr: rulesErr}
}

func TestClassifyHost_MeasuredLiveNodeIsUnfilteredNotFiltering(t *testing.T) {
	// The defect this file exists for, as a test: pf says Enabled, and
	// the host's own ruleset cannot drop anything. A two-valued model
	// would report this as "firewall fine", which is what
	// `pfctl -s info` has been reporting.
	b := ClassifyHost(reads(realPFInfoEnabled, nil, measuredLiveCombsRuleset, nil))

	if b.State != BaselineUnfiltered {
		t.Errorf("State = %q, want %q for a host whose only rule is the apiary anchor", b.State, BaselineUnfiltered)
	}
	if b.State.Filters() {
		t.Error("Filters() = true for a host with no block policy and no block rule")
	}
	if b.PFEnabled != Present {
		t.Errorf("PFEnabled = %q, want %q - pf really was enabled here", b.PFEnabled, Present)
	}
	if b.DefaultPolicy != Absent {
		t.Errorf("DefaultPolicy = %q, want %q", b.DefaultPolicy, Absent)
	}
	if b.BlockRule != Absent {
		t.Errorf("BlockRule = %q, want %q", b.BlockRule, Absent)
	}
	if b.SkipLoopback != Absent {
		t.Errorf("SkipLoopback = %q, want %q - the measured pf.conf had no `set skip on lo`", b.SkipLoopback, Absent)
	}
	if b.ApiaryAnchorReached != Present {
		t.Errorf("ApiaryAnchorReached = %q, want %q - `anchor \"apiary/*\" all` is present", b.ApiaryAnchorReached, Present)
	}
	if b.Detail == "" {
		t.Error("Detail is empty; a verdict with no reason is not reportable")
	}
}

func TestClassifyHost_DefaultBlockPolicyPresentIsFiltering(t *testing.T) {
	b := ClassifyHost(reads(realPFInfoEnabled, nil, stockFreeBSDRuleset, nil))
	if b.State != BaselineFiltering {
		t.Errorf("State = %q, want %q", b.State, BaselineFiltering)
	}
	if !b.State.Filters() {
		t.Error("Filters() = false for a host with a default block policy")
	}
	if b.DefaultPolicy != Present {
		t.Errorf("DefaultPolicy = %q, want %q", b.DefaultPolicy, Present)
	}
	if b.SkipLoopback != Present {
		t.Errorf("SkipLoopback = %q, want %q", b.SkipLoopback, Present)
	}
}

func TestClassifyHost_BlockRuleAloneIsFilteringButADifferentReason(t *testing.T) {
	// A block rule with no default policy still drops what it matches.
	// It is not the same finding as a default policy, and the two
	// observations are reported separately so a reader can tell them
	// apart.
	b := ClassifyHost(reads(realPFInfoEnabled, nil, "block drop in log\npass in on em0 keep state\n", nil))
	if b.State != BaselineFiltering {
		t.Errorf("State = %q, want %q - one block rule can drop the traffic it matches", b.State, BaselineFiltering)
	}
	if b.BlockRule != Present || b.DefaultPolicy != Absent {
		t.Errorf("BlockRule = %q, DefaultPolicy = %q; want present/absent", b.BlockRule, b.DefaultPolicy)
	}
}

func TestClassifyHost_DisabledPF(t *testing.T) {
	b := ClassifyHost(reads(realPFInfoDisabled, nil, stockFreeBSDRuleset, nil))
	if b.State != BaselineDisabled {
		t.Errorf("State = %q, want %q", b.State, BaselineDisabled)
	}
	if b.State.Filters() {
		t.Error("Filters() = true with pf disabled")
	}
	// The ruleset is still readable, and still says what it says: the
	// verdict is about pf being off, not about the text.
	if b.BlockRule != Present {
		t.Errorf("BlockRule = %q, want %q", b.BlockRule, Present)
	}
}

func TestClassifyHost_UnreadableIsUnknownAndEveryObservationSaysSo(t *testing.T) {
	// The failure direction, chosen deliberately: everything unknown,
	// and the verdict unknown. Never the reassuring answer.
	for _, tc := range []struct {
		name  string
		reads HostReads
	}{
		{"no pfctl at all", HostReads{InfoErr: errors.New("exec: \"pfctl\": executable file not found in $PATH"), MainRulesetErr: errors.New("exec: \"pfctl\": executable file not found in $PATH")}},
		{"not permitted", HostReads{InfoErr: errors.New("pfctl: Permission denied"), MainRulesetErr: errors.New("pfctl: Permission denied")}},
		{"main ruleset unreadable, info fine", reads(realPFInfoEnabled, nil, "", errors.New("pfctl: Permission denied"))},
		{"info unreadable, main ruleset fine", reads("", errors.New("pfctl: Permission denied"), stockFreeBSDRuleset, nil)},
		{"unrecognised status word", reads("Status: Emergency\n", nil, stockFreeBSDRuleset, nil)},
		{"no status line at all", reads("some unexpected output\n", nil, stockFreeBSDRuleset, nil)},
		{"empty output, no error", reads("", nil, "", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := ClassifyHost(tc.reads)
			if b.State == BaselineFiltering {
				t.Fatalf("State = %q, want something other than filtering on unreadable evidence", b.State)
			}
			if b.State.Filters() {
				t.Error("Filters() = true on unreadable evidence")
			}
			if b.Detail == "" {
				t.Error("Detail is empty; an unknown with no reason is not reportable")
			}
		})
	}
}

func TestClassifyHost_UnreadableMainRulesetLeavesEveryRulesetFactUnknown(t *testing.T) {
	b := ClassifyHost(reads(realPFInfoEnabled, nil, "", errors.New("pfctl: Permission denied")))
	if b.PFEnabled != Present {
		t.Errorf("PFEnabled = %q, want %q - `pfctl -s info` did answer", b.PFEnabled, Present)
	}
	for name, got := range map[string]Observation{
		"DefaultPolicy":       b.DefaultPolicy,
		"BlockRule":           b.BlockRule,
		"SkipLoopback":        b.SkipLoopback,
		"ApiaryAnchorReached": b.ApiaryAnchorReached,
	} {
		if got != Unknown {
			t.Errorf("%s = %q, want %q - a failed read is silence, not absence", name, got, Unknown)
		}
		if got.Bool() {
			t.Errorf("%s.Bool() = true for an unknown", name)
		}
	}
}

func TestClassifyHost_PresenceAndAbsenceOfEachFact(t *testing.T) {
	// The value-present / value-absent / value-unreadable triple for
	// every single observation, spelled out rather than left to the
	// table tests above.
	for _, tc := range []struct {
		name    string
		rules   string
		info    string
		policy  Observation
		block   Observation
		skipLo  Observation
		anchor  Observation
		verdict BaselineState
	}{
		{
			name:   "nothing but the apiary anchor",
			rules:  measuredLiveCombsRuleset,
			info:   realPFInfoEnabled,
			policy: Absent, block: Absent, skipLo: Absent, anchor: Present,
			verdict: BaselineUnfiltered,
		},
		{
			name:   "a bare parent anchor does not reach apiary/*",
			rules:  "anchor \"apiary\" all\n",
			info:   realPFInfoEnabled,
			policy: Absent, block: Absent, skipLo: Absent, anchor: Absent,
			verdict: BaselineUnfiltered,
		},
		{
			name:   "skip on lo0 counts as skip on lo",
			rules:  "set skip on lo0\nanchor \"apiary/*\" all\n",
			info:   realPFInfoEnabled,
			policy: Absent, block: Absent, skipLo: Present, anchor: Present,
			verdict: BaselineUnfiltered,
		},
		{
			name:   "set block-policy is a policy, not a block rule",
			rules:  "set block-policy drop\nanchor \"apiary/*\" all\n",
			info:   realPFInfoEnabled,
			policy: Present, block: Absent, skipLo: Absent, anchor: Present,
			verdict: BaselineFiltering,
		},
		{
			name:   "a block rule in the main ruleset",
			rules:  "block return\nanchor \"apiary/*\" all\n",
			info:   realPFInfoEnabled,
			policy: Absent, block: Present, skipLo: Absent, anchor: Present,
			verdict: BaselineFiltering,
		},
		{
			name:   "a comment mentioning block is not a block rule",
			rules:  "# block everything except this\nanchor \"apiary/*\" all\n",
			info:   realPFInfoEnabled,
			policy: Absent, block: Absent, skipLo: Absent, anchor: Present,
			verdict: BaselineUnfiltered,
		},
		{
			name:   "the apiary namespace is absent from a filtered host",
			rules:  "set block-policy drop\n",
			info:   realPFInfoEnabled,
			policy: Present, block: Absent, skipLo: Absent, anchor: Absent,
			verdict: BaselineFiltering,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := ClassifyHost(reads(tc.info, nil, tc.rules, nil))
			if b.DefaultPolicy != tc.policy {
				t.Errorf("DefaultPolicy = %q, want %q", b.DefaultPolicy, tc.policy)
			}
			if b.BlockRule != tc.block {
				t.Errorf("BlockRule = %q, want %q", b.BlockRule, tc.block)
			}
			if b.SkipLoopback != tc.skipLo {
				t.Errorf("SkipLoopback = %q, want %q", b.SkipLoopback, tc.skipLo)
			}
			if b.ApiaryAnchorReached != tc.anchor {
				t.Errorf("ApiaryAnchorReached = %q, want %q", b.ApiaryAnchorReached, tc.anchor)
			}
			if b.State != tc.verdict {
				t.Errorf("State = %q, want %q", b.State, tc.verdict)
			}
		})
	}
}

func TestClassifyHost_MainRulesetIsKeptAsEvidence(t *testing.T) {
	b := ClassifyHost(reads(realPFInfoEnabled, nil, measuredLiveCombsRuleset, nil))
	if b.MainRuleset != measuredLiveCombsRuleset {
		t.Errorf("MainRuleset = %q, want the ruleset verbatim - it is the evidence an operator needs to see", b.MainRuleset)
	}
}

func TestBaselineString_NamesTheSubjectAndTheSilence(t *testing.T) {
	for _, tc := range []struct {
		state BaselineState
		reads HostReads
		want  string
	}{
		{BaselineFiltering, reads(realPFInfoEnabled, nil, stockFreeBSDRuleset, nil), "host ruleset: filtering"},
		{BaselineUnfiltered, reads(realPFInfoEnabled, nil, measuredLiveCombsRuleset, nil), "host ruleset: UNFILTERED"},
		{BaselineDisabled, reads(realPFInfoDisabled, nil, measuredLiveCombsRuleset, nil), "host ruleset: pf DISABLED"},
		{BaselineUnknown, HostReads{InfoErr: errors.New("no pfctl")}, "host ruleset: UNKNOWN"},
	} {
		got := ClassifyHost(tc.reads).String()
		if tc.state != ClassifyHost(tc.reads).State {
			t.Fatalf("State = %q, want %q", ClassifyHost(tc.reads).State, tc.state)
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("String() = %q, want it to contain %q", got, tc.want)
		}
	}
}

func TestUnfilteredSaysSoEvenWhenTheAnchorIsReached(t *testing.T) {
	// The measured live host, whose apiary anchor *was* referenced. The
	// detail must still not read as a firewall: an empty anchor drops
	// nothing.
	b := ClassifyHost(reads(realPFInfoEnabled, nil, measuredLiveCombsRuleset, nil))
	if !strings.Contains(b.Detail, "passed") {
		t.Errorf("Detail = %q, want it to say that unmatched packets are passed", b.Detail)
	}
	if !strings.Contains(b.Detail, "apiary/*") {
		t.Errorf("Detail = %q, want it to name the only thing that could still drop a packet", b.Detail)
	}
}

// ---- AssessHost through the exec seam ----

// managerWithHostReads returns a Manager whose exec answers the two
// read-only host queries. This is the seam that makes the whole
// assessment testable on a machine with no pf in it, exactly as
// managerWithReadback does for drift.
func managerWithHostReads(t *testing.T, info string, infoErr error, rules string, rulesErr error) *Manager {
	t.Helper()
	m, _ := newTestManager(t)
	m.exec = func(_ context.Context, _ string, _ string, args ...string) (string, error) {
		if len(args) == 2 && args[0] == "-s" && args[1] == "info" {
			return info, infoErr
		}
		if len(args) == 1 && args[0] == "-sr" {
			return rules, rulesErr
		}
		return "", errors.New("unexpected pfctl invocation: " + strings.Join(args, " "))
	}
	return m
}

func TestAssessHost_ReadsTheMainRulesetNotAnAnchor(t *testing.T) {
	m := managerWithHostReads(t, realPFInfoEnabled, nil, stockFreeBSDRuleset, nil)
	b := m.AssessHost(context.Background())
	if b.State != BaselineFiltering {
		t.Errorf("State = %q, want %q", b.State, BaselineFiltering)
	}
}

// TestAssessHost_NeverWritesToPF is the guard rail for the whole file: the
// assessment must be incapable of changing what pf is enforcing. If a
// future edit adds a -f, this test is what notices.
func TestAssessHost_NeverWritesToPF(t *testing.T) {
	var seen [][]string
	m, _ := newTestManager(t)
	m.exec = func(_ context.Context, _ string, name string, args ...string) (string, error) {
		seen = append(seen, append([]string{name}, args...))
		return realPFInfoEnabled, nil
	}
	_ = m.AssessHost(context.Background())
	if len(seen) == 0 {
		t.Fatal("AssessHost() made no pfctl call")
	}
	for _, call := range seen {
		for _, arg := range call[1:] {
			if arg == "-a" || arg == "-f" || arg == "-F" {
				t.Errorf("AssessHost() invoked pfctl with %v; a read-only assessment must not load, flush or address an anchor", call)
			}
		}
	}
}

func TestAssessHost_MissingPFctlIsUnknown(t *testing.T) {
	m := managerWithHostReads(t, "", &execError{name: "pfctl", msg: "pfctl: not found"}, "", &execError{name: "pfctl", msg: "pfctl: not found"})
	b := m.AssessHost(context.Background())
	if b.State != BaselineUnknown {
		t.Errorf("State = %q, want %q when pfctl is missing", b.State, BaselineUnknown)
	}
}

// ---- In sync is not enforcing ----

// driftFor builds the in-sync drift observation for a ruleset body,
// without touching pf or the disk.
func driftFor(anchor, body string) Drift {
	return Drift{Anchor: anchor, State: DriftInSync, Expected: canonicalRules(body), Observed: canonicalRules(body), KnownGood: KnownGoodLoaded}
}

// TestDrift_InSyncIsNotTheSameAsEnforcing is the second distinction this
// branch exists to make: an anchor whose text pf is enforcing byte for
// byte, on a host whose main ruleset makes that text irrelevant.
func TestDrift_InSyncIsNotTheSameAsEnforcing(t *testing.T) {
	passOnly := "pass in on vtnet0 from any to any port = 22\n"
	blockRule := "block in on vtnet0 from any to any\n"
	liveBaseline := ClassifyHost(reads(realPFInfoEnabled, nil, measuredLiveCombsRuleset, nil))

	for _, tc := range []struct {
		name       string
		body       string
		baseline   Baseline
		want       bool
		wantCaveat string
	}{
		{
			name: "pass-only anchor on a host that permits everything anyway",
			body: passOnly, baseline: liveBaseline,
			want: false, wantCaveat: "would have permitted the traffic anyway",
		},
		{
			name: "empty anchor: in sync, drops nothing",
			body: "", baseline: liveBaseline,
			want: false, wantCaveat: "anchor is empty",
		},
		{
			name: "a block rule is decisive even on a permissive host",
			body: blockRule, baseline: liveBaseline,
			want: true, wantCaveat: "",
		},
		{
			name:     "block rule, but the main ruleset never reaches apiary/*",
			body:     blockRule,
			baseline: ClassifyHost(reads(realPFInfoEnabled, nil, "set block-policy drop\n", nil)),
			want:     false, wantCaveat: "does not reference the apiary/* namespace",
		},
		{
			name:     "block rule, but pf is not known to be enabled",
			body:     blockRule,
			baseline: ClassifyHost(reads("", errors.New("pfctl: Permission denied"), measuredLiveCombsRuleset, nil)),
			want:     false, wantCaveat: "not confirmed enabled",
		},
		{
			name:     "block rule, but the host baseline could not be read at all",
			body:     blockRule,
			baseline: ClassifyHost(HostReads{InfoErr: errors.New("no pfctl"), MainRulesetErr: errors.New("no pfctl")}),
			want:     false, wantCaveat: "not confirmed enabled",
		},
		{
			name: "pass-only anchor becomes decisive under a block policy",
			body: passOnly, baseline: ClassifyHost(reads(realPFInfoEnabled, nil, stockFreeBSDRuleset, nil)),
			want: true, wantCaveat: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := driftFor("apiary/vm-1", tc.body)
			if !d.InSync() {
				t.Fatal("fixture is not in sync; the comparison would be meaningless")
			}
			if got := d.Decisive(tc.baseline); got != tc.want {
				t.Errorf("Decisive() = %v, want %v", got, tc.want)
			}
			caveat := d.Qualify(tc.baseline)
			if tc.want {
				if caveat != "" {
					t.Errorf("Qualify() = %q, want empty for a decisive anchor", caveat)
				}
				return
			}
			if !strings.Contains(caveat, tc.wantCaveat) {
				t.Errorf("Qualify() = %q, want it to mention %q", caveat, tc.wantCaveat)
			}
		})
	}
}

func TestDrift_QualifyIsSilentWhenDriftIsTheWorseNews(t *testing.T) {
	drifted := Drift{Anchor: "apiary/vm-1", State: DriftDetected, Detail: "line 1 differs"}
	if got := drifted.Qualify(ClassifyHost(reads(realPFInfoEnabled, nil, measuredLiveCombsRuleset, nil))); got != "" {
		t.Errorf("Qualify() = %q, want empty - a detected drift already says something worse", got)
	}
}
