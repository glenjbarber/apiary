package frontend

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/health"
	"github.com/glenjbarber/apiary/web"
)

func TestNewHealthCardView(t *testing.T) {
	v := newHealthCardView(3, 4, map[string]int{
		"ok":            2,
		"contradictory": 1,
		// "critical", "warn", "stale", "unknown", "not-applicable" left
		// absent on purpose - must render as zero, not be dropped.
	}, 1, 2)

	if v.ReachableCombs != 3 || v.TotalCombs != 4 {
		t.Fatalf("got ReachableCombs=%d TotalCombs=%d, want 3/4", v.ReachableCombs, v.TotalCombs)
	}
	if v.GuardrailHolds != 1 || v.PendingJoins != 2 {
		t.Fatalf("got GuardrailHolds=%d PendingJoins=%d, want 1/2", v.GuardrailHolds, v.PendingJoins)
	}
	if len(v.Stats) != len(healthCardStateOrder) {
		t.Fatalf("got %d stats, want all %d vocabulary states present", len(v.Stats), len(healthCardStateOrder))
	}

	byState := map[string]healthCardStat{}
	for _, s := range v.Stats {
		byState[s.State] = s
	}
	wantCounts := map[string]int{
		"ok": 2, "contradictory": 1,
		"critical": 0, "warn": 0, "stale": 0, "unknown": 0, "not-applicable": 0,
	}
	for state, want := range wantCounts {
		got, ok := byState[state]
		if !ok {
			t.Errorf("state %q missing from Stats - an absent-from-input state must still appear as zero", state)
			continue
		}
		if got.Count != want {
			t.Errorf("state %q Count = %d, want %d", state, got.Count, want)
		}
		if got.Label == "" {
			t.Errorf("state %q has an empty Label", state)
		}
	}

	// Worst-first order: critical before ok, wherever both appear.
	criticalIdx, okIdx := -1, -1
	for i, s := range v.Stats {
		if s.State == "critical" {
			criticalIdx = i
		}
		if s.State == "ok" {
			okIdx = i
		}
	}
	if criticalIdx == -1 || okIdx == -1 || criticalIdx > okIdx {
		t.Errorf("expected critical (index %d) to sort before ok (index %d)", criticalIdx, okIdx)
	}
}

func TestHealthCardVocabFor(t *testing.T) {
	cases := []struct {
		status health.Status
		want   string
	}{
		{health.StatusHealthy, "ok"},
		{health.StatusDegraded, "warn"},
		{health.StatusUnknown, "unknown"},
		{health.StatusStale, "stale"},
		{health.StatusContradictory, "contradictory"},
	}
	for _, c := range cases {
		if got := healthCardVocabFor(c.status); got != c.want {
			t.Errorf("healthCardVocabFor(%q) = %q, want %q", c.status, got, c.want)
		}
	}
}

func TestNewHealthCardViewFromNodes(t *testing.T) {
	nodes := []clusterNodeView{
		{NodeID: "a", Reachable: true, HealthStatus: health.StatusHealthy},
		{NodeID: "b", Reachable: true, HealthStatus: health.StatusHealthy},
		{NodeID: "c", Reachable: false, HealthStatus: health.StatusUnknown},
		{NodeID: "d", Reachable: true, HealthStatus: health.StatusContradictory},
	}
	v := newHealthCardViewFromNodes(nodes, 0, 3)

	if v.ReachableCombs != 3 || v.TotalCombs != 4 {
		t.Fatalf("got ReachableCombs=%d TotalCombs=%d, want 3/4", v.ReachableCombs, v.TotalCombs)
	}
	if v.PendingJoins != 3 {
		t.Errorf("PendingJoins = %d, want 3 (passed through unchanged)", v.PendingJoins)
	}
	byState := map[string]int{}
	for _, s := range v.Stats {
		byState[s.State] = s.Count
	}
	want := map[string]int{"ok": 2, "unknown": 1, "contradictory": 1, "critical": 0, "warn": 0, "stale": 0, "not-applicable": 0}
	for state, count := range want {
		if byState[state] != count {
			t.Errorf("state %q count = %d, want %d", state, byState[state], count)
		}
	}
	// The unreachable node (c) still counts toward TotalCombs and still
	// contributes its own HealthStatus (unknown) to the grid - being
	// unreachable does not erase it from the count of what was seen.
	if v.TotalCombs-v.ReachableCombs != 1 {
		t.Errorf("expected exactly one unreachable Comb accounted for")
	}
	// A Comb that did not answer is the one fact reserved for red, and it
	// wins here even though node (d) is also contradictory - the outline
	// must not spend its one loudest signal on anything else.
	if v.Colony.State != colonyStateCombDown {
		t.Errorf("Colony.State = %q, want %q (a Comb is down)", v.Colony.State, colonyStateCombDown)
	}
}

func TestColonyVerdictFor(t *testing.T) {
	// A state keyed "unknown" that is not one of the seven vocabulary
	// names - the reachable-box rule counts anything that is not
	// affirmatively healthy, so an unforeseen key must not be dropped.
	healthy := map[string]int{"ok": 3, "not-applicable": 1}
	cases := []struct {
		name                         string
		reachable, total             int
		counts                       map[string]int
		guardrailHolds, pendingJoins int
		want                         healthCardColonyState
		wantBadge                    string
		wantLabel                    string
		wantDetailContains           []string
		notWantDetailContains        []string
	}{
		{
			name:      "everything good is green",
			reachable: 3, total: 3,
			counts: map[string]int{"ok": 3},
			want:   colonyStateAllClear, wantBadge: "ok", wantLabel: "All clear",
			wantDetailContains: []string{"all 3 Combs reachable", "reporting healthy"},
		},
		{
			name:      "not-applicable is not an outstanding issue",
			reachable: 4, total: 4,
			counts: healthy,
			want:   colonyStateAllClear, wantBadge: "ok", wantLabel: "All clear",
		},
		{
			name:      "a Comb that is down is red, whatever else is true",
			reachable: 2, total: 3,
			counts: map[string]int{"ok": 2, "unknown": 1},
			want:   colonyStateCombDown, wantBadge: "critical", wantLabel: "1 down",
			wantDetailContains:    []string{"1 Comb of 3 known Combs did not answer", "reserves red for"},
			notWantDetailContains: []string{"not reporting healthy"},
		},
		{
			name:      "every Comb down is still red, and the count is plural",
			reachable: 0, total: 2,
			counts: map[string]int{"unknown": 2},
			want:   colonyStateCombDown, wantBadge: "critical", wantLabel: "2 down",
			wantDetailContains: []string{"2 Combs of 2 known Combs did not answer"},
		},
		{
			name:      "an unobserved Comb is yellow, not green",
			reachable: 2, total: 2,
			counts: map[string]int{"ok": 1, "unknown": 1},
			want:   colonyStateAttention, wantBadge: "warn", wantLabel: "Needs attention",
			wantDetailContains: []string{"1 of 2 Combs report a health verdict other than healthy", "no Comb is down"},
		},
		{
			name:      "a stale Comb is yellow",
			reachable: 1, total: 1,
			counts: map[string]int{"stale": 1},
			want:   colonyStateAttention, wantBadge: "warn", wantLabel: "Needs attention",
		},
		{
			name:      "a contradictory Comb is yellow, not red",
			reachable: 1, total: 1,
			counts: map[string]int{"contradictory": 1},
			want:   colonyStateAttention, wantBadge: "warn", wantLabel: "Needs attention",
			// Red stays reserved for a Comb that does not answer, so a
			// contradictory-but-reachable Comb must not claim it.
			notWantDetailContains: []string{"did not answer"},
		},
		{
			name:      "an unforeseen state key is counted, not dropped",
			reachable: 1, total: 1,
			counts: map[string]int{"ok": 0, "": 1},
			want:   colonyStateAttention, wantBadge: "warn", wantLabel: "Needs attention",
		},
		{
			name:      "a held restart guardrail is yellow on its own",
			reachable: 1, total: 1,
			counts:         map[string]int{"ok": 1},
			guardrailHolds: 1,
			want:           colonyStateAttention, wantBadge: "warn", wantLabel: "Needs attention",
			wantDetailContains: []string{"1 unconfirmed restart guardrail hold"},
		},
		{
			name:      "a pending join request is yellow, and pluralizes",
			reachable: 1, total: 1,
			counts:       map[string]int{"ok": 1},
			pendingJoins: 2,
			want:         colonyStateAttention, wantBadge: "warn", wantLabel: "Needs attention",
			wantDetailContains: []string{"2 pending join requests"},
		},
		{
			name:      "no known Combs is yellow - nothing was checked",
			reachable: 0, total: 0,
			counts: map[string]int{},
			want:   colonyStateAttention, wantBadge: "warn", wantLabel: "Needs attention",
			wantDetailContains:    []string{"no Comb was observed"},
			notWantDetailContains: []string{"0 Combs reachable"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := colonyVerdictFor(c.reachable, c.total, c.counts, c.guardrailHolds, c.pendingJoins)
			if got.State != c.want {
				t.Errorf("State = %q, want %q", got.State, c.want)
			}
			if got.BadgeClass != c.wantBadge {
				t.Errorf("BadgeClass = %q, want %q", got.BadgeClass, c.wantBadge)
			}
			if got.Label != c.wantLabel {
				t.Errorf("Label = %q, want %q", got.Label, c.wantLabel)
			}
			if got.Detail == "" {
				t.Fatal("Detail is empty - the outline's state must be stated in words, not by color alone")
			}
			for _, want := range c.wantDetailContains {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("Detail = %q, want it to contain %q", got.Detail, want)
				}
			}
			for _, unwanted := range c.notWantDetailContains {
				if strings.Contains(got.Detail, unwanted) {
					t.Errorf("Detail = %q, want it NOT to contain %q", got.Detail, unwanted)
				}
			}
		})
	}
}

// The box's outline state must reach the browser as the data-state token
// the stylesheet has a rule for - an unstyled state would fall back to the
// neutral --line border, which reads as "no news" for what the badge
// beside it calls an outage.
func TestColonyStateTokensReachTheRenderedBox(t *testing.T) {
	partial, err := web.FS.ReadFile("templates/_health_card.html")
	if err != nil {
		t.Fatalf("read _health_card.html: %v", err)
	}
	layout, err := web.FS.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatalf("read layout.html: %v", err)
	}
	tmpl, err := template.New("health_card").Parse(string(partial))
	if err != nil {
		t.Fatalf("parse _health_card.html: %v", err)
	}

	cases := []struct {
		reachable, total int
		counts           map[string]int
		want             healthCardColonyState
	}{
		{3, 3, map[string]int{"ok": 3}, colonyStateAllClear},
		{2, 2, map[string]int{"ok": 1, "unknown": 1}, colonyStateAttention},
		{2, 3, map[string]int{"ok": 2, "unknown": 1}, colonyStateCombDown},
	}
	// Every state the verdict can return must be in this set, so a new
	// state cannot be added without a rule and a case here.
	covered := map[healthCardColonyState]bool{}
	for _, c := range cases {
		covered[c.want] = true
		var buf bytes.Buffer
		view := newHealthCardView(c.reachable, c.total, c.counts, 0, 0)
		if err := tmpl.ExecuteTemplate(&buf, "health_card", view); err != nil {
			t.Fatalf("render health_card: %v", err)
		}
		html := buf.String()
		wantAttr := `class="health-card-reachable" data-state="` + string(c.want) + `"`
		if !strings.Contains(html, wantAttr) {
			t.Errorf("rendered box is missing %s:\n%s", wantAttr, html)
		}
		// The state must also be in words, not carried by the outline
		// alone - the same reason the leader Comb pairs its outline with
		// a badge.
		if !strings.Contains(html, view.Colony.Label) {
			t.Errorf("rendered box does not state the state in words (%q):\n%s", view.Colony.Label, html)
		}
		if !strings.Contains(string(layout), `.health-card-reachable[data-state="`+string(c.want)+`"]`) {
			t.Errorf("no layout.html rule for .health-card-reachable[data-state=%q]", c.want)
		}
	}
	if len(covered) != 3 {
		t.Errorf("covered %d distinct states, want all 3", len(covered))
	}
}
