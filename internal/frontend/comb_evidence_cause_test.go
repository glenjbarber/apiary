package frontend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/health"
)

// This file covers the "why is this Comb unhealthy" evidence surface. The
// properties under test are deliberately about HONESTY rather than layout:
// the three observation states must stay visually and semantically distinct,
// a stale observation must never read as a current one, a Comb nothing was
// ever observed about must never render green, and a failed fetch must
// render the honest "not observed" panel rather than an empty or green one.

// causesPanel extracts just the cause panel from a rendered page. Scoping
// assertions to it is deliberate: the surrounding chrome (the Colony-leader
// badge in layout.html, the host-evidence "Reachable" badge) legitimately
// renders green for reasons that have nothing to do with whether this
// feature observed anything, so a document-wide "no green" assertion would
// be both noisy and wrong.
func causesPanel(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `id="comb-causes"`)
	if start < 0 {
		t.Fatalf("rendered page has no cause panel (id=\"comb-causes\"); body: %s", body)
	}
	end := strings.Index(body[start:], "</section>")
	if end < 0 {
		t.Fatalf("cause panel is not terminated by </section>; body: %s", body)
	}
	return body[start : start+end]
}

// assertNeverGreen is the load-bearing assertion of this whole file: none of
// the green badge classes may appear inside the cause panel unless the row's
// own state is a fresh, genuinely successful observation.
func assertNeverGreen(t *testing.T, panel, except string) {
	t.Helper()
	for _, green := range []string{`class="badge ready"`, `class="badge healthy"`, `class="badge ok"`} {
		if strings.Contains(panel, green) && !strings.Contains(except, green) {
			t.Errorf("cause panel rendered the green badge %s for a Comb with no successful observation; panel: %s", green, panel)
		}
	}
}

func TestCombCauseBadge_ThreeStatesAreVisuallyDistinct(t *testing.T) {
	tests := []struct {
		name      string
		state     combCauseState
		stale     bool
		wantClass string
		wantLabel string
	}{
		// The three required states, each with its own class AND its own
		// label, so the distinction survives both a colour-blind reader
		// and a screen reader (or a text-mode browser).
		{"never observed", combCauseNeverObserved, false, "unknown", "not observed"},
		{"observed healthy", combCauseObservedHealthy, false, "ready", "observed healthy"},
		{"observed failed", combCauseObservedFailed, false, "error", "observed failed"},
		// Staleness is a separate axis, never a fourth state.
		{"stale healthy", combCauseObservedHealthy, true, "stale", "observed healthy (stale)"},
		{"stale failed", combCauseObservedFailed, true, "error", "observed failed (stale)"},
		{"not applicable", combCauseNotApplicable, false, "not-applicable", "not applicable"},
		// An unrecognized state is treated as unobserved, never as
		// healthy: a future typo must not become a green light.
		{"unknown future state", combCauseState("something_new"), false, "unknown", "not observed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			class, label := combCauseBadge(tc.state, tc.stale)
			if class != tc.wantClass || label != tc.wantLabel {
				t.Fatalf("combCauseBadge(%q, stale=%v) = (%q, %q), want (%q, %q)", tc.state, tc.stale, class, label, tc.wantClass, tc.wantLabel)
			}
		})
	}

	// The three states must not collapse onto one another.
	seen := map[string]combCauseState{}
	for _, state := range []combCauseState{combCauseNeverObserved, combCauseObservedHealthy, combCauseObservedFailed} {
		class, label := combCauseBadge(state, false)
		key := class + "/" + label
		if other, dup := seen[key]; dup {
			t.Errorf("states %q and %q render identically as %s", other, state, key)
		}
		seen[key] = state
	}
	// Only observed_healthy may be green, and only while fresh.
	for _, state := range []combCauseState{combCauseNeverObserved, combCauseObservedFailed, combCauseNotApplicable} {
		if class, _ := combCauseBadge(state, false); class == "ready" || class == "healthy" || class == "ok" {
			t.Errorf("state %q renders green as %q; an unobserved or failed observation must never look healthy", state, class)
		}
	}
	if class, _ := combCauseBadge(combCauseObservedHealthy, true); class == "ready" {
		t.Error("a stale successful observation rendered green - a stale success is not a current success")
	}
}

func TestAssessCombReconcileEvidence_DistinguishesAllStates(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	limit := 15 * time.Minute // 3x a 5m reconcile interval, as internal/health derives it

	tests := []struct {
		name       string
		configured bool
		attempted  bool
		attempt    time.Time
		succeeded  bool
		success    time.Time
		hasLimit   bool
		wantState  combCauseState
		wantStale  bool
		wantSeen   bool
	}{
		{
			name: "no reconciler configured is not applicable, never healthy",
			// interval 0 means SignalsFrom sets ReconcilerConfigured=false.
			// "Nothing was asked" must not read as "nothing is wrong".
			wantState: combCauseNotApplicable, wantSeen: false,
		},
		{
			name:       "recent success is the only healthy state",
			configured: true, attempted: true, attempt: now.Add(-1 * time.Minute),
			succeeded: true, success: now.Add(-1 * time.Minute), hasLimit: true,
			wantState: combCauseObservedHealthy, wantStale: false, wantSeen: true,
		},
		{
			name:       "recent attempt with no recent success is a recent failure",
			configured: true, attempted: true, attempt: now.Add(-1 * time.Minute),
			hasLimit: true, wantState: combCauseObservedFailed, wantStale: false, wantSeen: true,
		},
		{
			name:       "an old attempt is a stale failure, not a quiet success",
			configured: true, attempted: true, attempt: now.Add(-3 * time.Hour),
			hasLimit: true, wantState: combCauseObservedFailed, wantStale: true, wantSeen: true,
		},
		{
			// attempt == success: the last tick this Comb actually ran was
			// a clean one, three hours ago. That is a real observed
			// success, and calling it a failure would invent one.
			name:       "an old success with no later attempt is a stale success",
			configured: true, attempted: true, attempt: now.Add(-3 * time.Hour),
			succeeded: true, success: now.Add(-3 * time.Hour), hasLimit: true,
			wantState: combCauseObservedHealthy, wantStale: true, wantSeen: true,
		},
		{
			// The strongest thing two timestamps can say: the most recent
			// tick this Comb ran did not succeed.
			name:       "an attempt later than the last success is a stale failure",
			configured: true, attempted: true, attempt: now.Add(-2 * time.Hour),
			succeeded: true, success: now.Add(-3 * time.Hour), hasLimit: true,
			wantState: combCauseObservedFailed, wantStale: true, wantSeen: true,
		},
		{
			name:       "configured but never ticked is never observed",
			configured: true, hasLimit: true,
			wantState: combCauseNeverObserved, wantSeen: false,
		},
		{
			// internal/health stops before deriving a freshness limit when
			// raft itself is unhealthy. With no limit there is nothing to be
			// past, so nothing may be called stale - inventing a threshold
			// here would be the mirror image of inventing an error.
			name:       "no derivable limit means no staleness claim",
			configured: true, attempted: true, attempt: now.Add(-3 * time.Hour),
			hasLimit: false, wantState: combCauseObservedFailed, wantStale: false, wantSeen: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := assessCombReconcileEvidence(tc.configured, tc.attempted, tc.attempt, tc.succeeded, tc.success, limit, tc.hasLimit, now)
			if got.State != tc.wantState {
				t.Errorf("State = %q, want %q (detail: %s)", got.State, tc.wantState, got.Detail)
			}
			if got.Stale != tc.wantStale {
				t.Errorf("Stale = %v, want %v", got.Stale, tc.wantStale)
			}
			if got.HasObservedAt != tc.wantSeen {
				t.Errorf("HasObservedAt = %v, want %v", got.HasObservedAt, tc.wantSeen)
			}
			if got.Detail == "" {
				t.Error("Detail is empty; every state must say what it does and does not mean")
			}
		})
	}
}

func TestReconcileFreshnessLimit_ReusesTheLimitHealthAlreadyComputed(t *testing.T) {
	// Reusing health's own Observation is what keeps this page and the
	// health badge from drifting onto two different definitions of
	// "fresh". The test pins that dependency from both sides.
	if got, ok := reconcileFreshnessLimit([]health.Observation{{Source: "raft_membership"}, {Source: "reconciler_last_success", FreshnessLimit: 15 * time.Minute}}); !ok || got != 15*time.Minute {
		t.Errorf("reconcileFreshnessLimit = (%v, %v), want (15m, true)", got, ok)
	}
	if _, ok := reconcileFreshnessLimit([]health.Observation{{Source: "raft_membership"}}); ok {
		t.Error("reconcileFreshnessLimit reported a limit where health derived none; callers would then mark things stale against a threshold that does not exist")
	}
}

func TestCombResourceLink_OnlyLinksToPagesThatExist(t *testing.T) {
	// VMs and jails have per-resource routes. Networks and images only have
	// list pages, so a deep link to them would resolve to the top of a list
	// while looking exactly like the working links beside it.
	if got := combResourceLink("vm", "abc"); got != "/vms/abc" {
		t.Errorf("vm link = %q, want /vms/abc", got)
	}
	if got := combResourceLink("jail", "abc"); got != "/jails/abc" {
		t.Errorf("jail link = %q, want /jails/abc", got)
	}
	for _, kind := range []string{"network", "iso", "", "something_new"} {
		if got := combResourceLink(kind, "abc"); got != "" {
			t.Errorf("combResourceLink(%q) = %q, want \"\" - there is no per-%s page, so a link here would not resolve", kind, got, kind)
		}
	}
	if got := combResourceLink("vm", ""); got != "" {
		t.Errorf("empty id produced a link %q, want none", got)
	}
}

func TestCombCauses_UnreachableCombIsNeverObservedNotHealthy(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	anchor := &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium", "freebsd-apiary"}, RaftReachable: true}

	causes, state, headline, _ := combCauses("freebsd-apiary", "apiarium", anchor, nil, errors.New("connection refused"), nil, &combCauseIndex{}, now)

	if state != combCauseNeverObserved {
		t.Fatalf("overall state = %q, want %q - nothing was observed about an unreachable Comb", state, combCauseNeverObserved)
	}
	reach := findCause(t, causes, "manager_reachability")
	if reach.State != combCauseNeverObserved {
		t.Errorf("reachability cause state = %q, want %q", reach.State, combCauseNeverObserved)
	}
	if !strings.Contains(reach.Detail, "connection refused") {
		t.Errorf("reachability cause dropped the real fetch error: %q", reach.Detail)
	}
	if class, _ := combCauseBadge(reach.State, reach.Stale); class == "ready" {
		t.Error("an unreachable Comb's cause rendered green")
	}
	if headline == "" {
		t.Error("headline is empty; the command-center card would show nothing about why this Comb is bad")
	}

	// No HostStats means no reconcile cause may claim a tick was seen.
	for _, c := range causes {
		if c.Source == "reconciler_last_tick" {
			t.Errorf("a reconcile tick cause was derived for a Comb whose HostStats never arrived: %+v", c)
		}
	}
}

func TestCombCauses_PeerForwardingAbsentNeverBorrowsTheLocalNodesEvidence(t *testing.T) {
	// clusterNodeEvidence drops an unverified HostStats before it reaches
	// here, so a Comb that could not be dialed can never inherit this
	// frontend's own reconcile history. Pinned here at the seam.
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	anchor := &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium", "freebsd-apiary"}, RaftReachable: true}
	obs := []health.Observation{{Source: "reconciler_last_success", FreshnessLimit: 15 * time.Minute}}

	causes, state, _, _ := combCauses("freebsd-apiary", "apiarium", anchor, nil, nil, obs, &combCauseIndex{}, now)
	if state != combCauseNeverObserved {
		t.Fatalf("overall state = %q, want %q", state, combCauseNeverObserved)
	}
	reach := findCause(t, causes, "manager_reachability")
	if !strings.Contains(reach.Detail, "peer forwarding") {
		t.Errorf("detail does not explain that nothing was read: %q", reach.Detail)
	}
	if findCauseIn(causes, "reconciler_last_tick") != nil {
		t.Error("a reconcile tick cause was derived for a Comb that was never actually read")
	}
}

func TestCombCauses_ResourceErrorCarriesRealTextResourceLinkAndStaleness(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	anchor := &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}, RaftReachable: true}
	obs := []health.Observation{{Source: "reconciler_last_success", FreshnessLimit: 15 * time.Minute}}
	// The owner ticked 3 hours ago, so any phase_error it wrote may since
	// have been resolved and nothing has observed it since.
	stats := &rpcpb.HostStatsResponse{
		NodeId: "apiarium", ReconcileIntervalSeconds: 300,
		LastReconcileAttemptUnix: now.Add(-3 * time.Hour).Unix(),
		LastReconcileSuccessUnix: now.Add(-3 * time.Hour).Unix(),
	}
	index := &combCauseIndex{VMs: []vmView{{
		ID: "vm-1", Name: "build", NodeID: "apiarium", Phase: "error",
		PhaseError: "cluster: zfs create failed: no such dataset", ISOName: "freebsd-14.iso", NetworkID: "net-1",
	}}}

	causes, _, _, _ := combCauses("apiarium", "apiarium", anchor, stats, nil, obs, index, now)
	c := findCause(t, causes, "reconcile_phase")
	if c == nil {
		t.Fatalf("no reconcile_phase cause for a VM in phase=error; causes: %+v", causes)
	}
	if c.State != combCauseObservedFailed {
		t.Errorf("State = %q, want %q", c.State, combCauseObservedFailed)
	}
	if c.Detail != "cluster: zfs create failed: no such dataset" {
		t.Errorf("Detail = %q, want the reconciler's own recorded text verbatim", c.Detail)
	}
	if c.ResourceLink != "/vms/vm-1" {
		t.Errorf("ResourceLink = %q, want /vms/vm-1", c.ResourceLink)
	}
	if c.ResourceKind != "vm" || c.ResourceName != "build" {
		t.Errorf("resource identity = %q/%q, want vm/build", c.ResourceKind, c.ResourceName)
	}
	// The Raft field carries no write timestamp, so the page must say the
	// bound rather than imply it knows when the error happened.
	if !strings.Contains(c.WrittenAtUnknown, "at or before") {
		t.Errorf("phase_error has no write timestamp on the wire; the cause must say so, got %q", c.WrittenAtUnknown)
	}
	if !c.Stale {
		t.Error("an error whose owner has not reconciled for 3h was not marked stale")
	}
	if !strings.Contains(c.StaleNote, "may since have been resolved") {
		t.Errorf("stale note does not say the error may already be resolved: %q", c.StaleNote)
	}
	// The image and network are NAMED, never linked: neither has a
	// per-resource page, and a link that resolves to a list would be worse
	// than no link.
	joined := strings.Join(c.RelatedRefs, " | ")
	if !strings.Contains(joined, "freebsd-14.iso") || !strings.Contains(joined, "net-1") {
		t.Errorf("related refs = %q, want the image and network named", joined)
	}
	if strings.Contains(joined, "href") {
		t.Errorf("related refs rendered a link: %q", joined)
	}
}

func TestCombCauses_ResourceErrorWithNoErrorTextSaysSoRatherThanInventingOne(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	anchor := &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}, RaftReachable: true}
	obs := []health.Observation{{Source: "reconciler_last_success", FreshnessLimit: 15 * time.Minute}}
	stats := &rpcpb.HostStatsResponse{NodeId: "apiarium", ReconcileIntervalSeconds: 300, LastReconcileAttemptUnix: now.Add(-time.Minute).Unix(), LastReconcileSuccessUnix: now.Add(-time.Minute).Unix()}
	index := &combCauseIndex{Jails: []jailView{{ID: "j-1", Name: "dns", NodeID: "apiarium", Phase: "error"}}}

	causes, _, _, _ := combCauses("apiarium", "apiarium", anchor, stats, nil, obs, index, now)
	c := findCause(t, causes, "reconcile_phase")
	if c == nil {
		t.Fatal("no reconcile_phase cause for a jail in phase=error")
	}
	if c.Detail == "" {
		t.Fatal("Detail is empty")
	}
	if !strings.Contains(c.Detail, "stored no error message") {
		t.Errorf("Detail = %q, want it to state that no message exists instead of inventing one", c.Detail)
	}
	if c.ResourceLink != "/jails/j-1" {
		t.Errorf("ResourceLink = %q, want /jails/j-1", c.ResourceLink)
	}
	if c.Stale {
		t.Error("cause marked stale although the owning Comb reconciled a minute ago")
	}
}

func TestCombCauses_ResourceOnlyShownOnItsOwnComb(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	anchor := &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium", "freebsd-apiary"}, RaftReachable: true}
	obs := []health.Observation{{Source: "reconciler_last_success", FreshnessLimit: 15 * time.Minute}}
	stats := &rpcpb.HostStatsResponse{NodeId: "freebsd-apiary", ReconcileIntervalSeconds: 300, LastReconcileAttemptUnix: now.Add(-time.Minute).Unix(), LastReconcileSuccessUnix: now.Add(-time.Minute).Unix()}
	index := &combCauseIndex{VMs: []vmView{{ID: "vm-1", Name: "build", NodeID: "apiarium", Phase: "error", PhaseError: "boom"}}}

	causes, _, _, _ := combCauses("freebsd-apiary", "apiarium", anchor, stats, nil, obs, index, now)
	if findCauseIn(causes, "reconcile_phase") != nil {
		t.Error("a VM owned by apiarium was reported as a problem on freebsd-apiary's evidence")
	}
}

func TestCombCauses_FailedResourceFetchRendersTheHonestGapNotAnEmptyPanel(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	anchor := &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}, RaftReachable: true}
	obs := []health.Observation{{Source: "reconciler_last_success", FreshnessLimit: 15 * time.Minute}}
	stats := &rpcpb.HostStatsResponse{NodeId: "apiarium", ReconcileIntervalSeconds: 300, LastReconcileAttemptUnix: now.Add(-time.Minute).Unix(), LastReconcileSuccessUnix: now.Add(-time.Minute).Unix()}
	index := &combCauseIndex{VMError: "raft: no leader", JailError: "raft: no leader"}

	causes, _, _, _ := combCauses("apiarium", "apiarium", anchor, stats, nil, obs, index, now)
	var gaps int
	for _, c := range causes {
		if c.Source == "reconcile_phase_gap" {
			gaps++
			if c.State != combCauseNeverObserved {
				t.Errorf("gap state = %q, want %q", c.State, combCauseNeverObserved)
			}
			if !strings.Contains(c.Detail, "raft: no leader") {
				t.Errorf("gap dropped the real fetch error: %q", c.Detail)
			}
		}
	}
	if gaps != 2 {
		t.Errorf("got %d resource-read gaps, want 2 (one per independently failing read)", gaps)
	}
	// A failed resource read must never surface as "nothing is wrong".
	for _, c := range causes {
		if c.Source == "reconcile_phase" {
			t.Errorf("a resource cause was invented from a failed read: %+v", c)
		}
	}
}

func TestCombCauses_HealthyCombStillNamesTheUnplumbedTickError(t *testing.T) {
	// A Comb whose only problem is one Apiary cannot yet explain is not a
	// Comb with nothing to say. The gap must be reported even when every
	// real observation is green, so the page never implies "this Comb is
	// fully explained".
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	anchor := &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}, RaftReachable: true}
	obs := []health.Observation{{Source: "reconciler_last_success", FreshnessLimit: 15 * time.Minute}}
	stats := &rpcpb.HostStatsResponse{NodeId: "apiarium", ReconcileIntervalSeconds: 300, LastReconcileAttemptUnix: now.Add(-time.Minute).Unix(), LastReconcileSuccessUnix: now.Add(-time.Minute).Unix()}

	causes, _, _, _ := combCauses("apiarium", "apiarium", anchor, stats, nil, obs, &combCauseIndex{}, now)
	tick := findCause(t, causes, "reconciler_last_tick")
	if tick == nil || tick.State != combCauseObservedHealthy {
		t.Fatalf("reconciler_last_tick = %+v, want a fresh observed_healthy", tick)
	}
	if class, _ := combCauseBadge(tick.State, tick.Stale); class != "ready" {
		t.Errorf("a genuinely fresh success rendered as %q, want ready", class)
	}
}

func TestFormatObservationAge(t *testing.T) {
	tests := []struct {
		age  time.Duration
		want string
	}{
		{3 * time.Second, "3s ago"},
		{90 * time.Second, "1m ago"},
		{3*time.Hour + 12*time.Minute, "3h 12m ago"},
		{50 * time.Hour, "2d 2h ago"},
		// A disagreeing clock is reported as unreadable rather than
		// clamped to a plausible-looking zero.
		{-time.Minute, "unknown (this Comb's clock is ahead of this frontend's)"},
	}
	for _, tc := range tests {
		if got := formatObservationAge(tc.age); got != tc.want {
			t.Errorf("formatObservationAge(%v) = %q, want %q", tc.age, got, tc.want)
		}
	}
}

func TestWorstCombCause_EmptyListIsNeverHealthy(t *testing.T) {
	// "No causes" and "no gaps" must not be reported as healthy: this
	// function has no evidence to support that claim.
	state, _, headline := worstCombCause(nil)
	if state == combCauseObservedHealthy {
		t.Fatal("an empty cause list was reported as observed_healthy")
	}
	if !strings.Contains(headline, "before treating that as healthy") {
		t.Errorf("headline = %q, want it to warn that this is not health", headline)
	}
	if class, _ := combCauseBadge(state, false); class == "ready" {
		t.Error("an empty cause list rendered green")
	}

	// A real failure must outrank a never-observed gap: a confirmed fault
	// is the more actionable finding of the two.
	causes := []combCauseView{
		{Source: "reconcile_phase_gap", State: combCauseNeverObserved, Summary: "gap"},
		{Source: "reconcile_phase", State: combCauseObservedFailed, Summary: "real failure"},
	}
	if state, _, _ = worstCombCause(causes); state != combCauseObservedFailed {
		t.Errorf("worst state = %q, want %q - a confirmed failure outranks a missing read", state, combCauseObservedFailed)
	}
}

func findCause(t *testing.T, causes []combCauseView, source string) *combCauseView {
	t.Helper()
	return findCauseIn(causes, source)
}

func findCauseIn(causes []combCauseView, source string) *combCauseView {
	for i := range causes {
		if causes[i].Source == source {
			return &causes[i]
		}
	}
	return nil
}

// --- rendered-page tests -------------------------------------------------

func TestClusterEvidencePage_ThreeStatesRenderDistinctly(t *testing.T) {
	// The evidence page for a Comb whose own reconcile tick is failing
	// because a VM is in phase=error: the real recorded error text, the
	// resource it concerns, and a link that resolves, must all be on the
	// page. Nothing is invented and nothing is green.
	now := time.Now()
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}, RaftReachable: true, Members: []*rpcpb.RaftMember{{NodeId: "apiarium", Suffrage: "Voter"}}},
		hostStatsResp: &rpcpb.HostStatsResponse{
			NodeId: "apiarium", ReconcileIntervalSeconds: 300,
			LastReconcileAttemptUnix: now.Add(-time.Minute).Unix(),
			Errors:                   []string{"zfs: exec: zpool status: exit status 1"},
		},
		listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{{
			Id: "vm-1", Name: "build", NodeId: "apiarium",
			Phase: rpcpb.VMPhase_VM_PHASE_ERROR, PhaseError: "cluster: dataset create failed: no such pool",
		}}},
	}
	s, err := NewServer(client, nil, nil, &fakePeerHostStatsClient{}, ".apiary.work", "17700", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/host/apiarium/evidence", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	panel := causesPanel(t, body)

	// The reconciler's own text, verbatim.
	if !strings.Contains(panel, "dataset create failed: no such pool") {
		t.Errorf("cause panel missing the reconciler's real error text: %s", panel)
	}
	// The affected resource, named and linked to a page that exists.
	if !strings.Contains(panel, `href="/vms/vm-1"`) {
		t.Errorf("cause panel missing a resolvable link to the failing VM: %s", panel)
	}
	// A real host-subsystem failure, verbatim.
	if !strings.Contains(panel, "zpool status") {
		t.Errorf("cause panel missing the real host-subsystem failure text: %s", panel)
	}
	// Failed and unobserved must both appear, and must not be the same
	// rendering.
	if !strings.Contains(panel, `data-cause-state="observed_failed"`) {
		t.Errorf("cause panel has no observed_failed row: %s", panel)
	}
	if !strings.Contains(panel, `data-cause-state="never_observed"`) {
		t.Errorf("cause panel has no never_observed row (a gap was expected): %s", panel)
	}
	if strings.Count(panel, `data-cause-state="observed_healthy"`) != 0 {
		t.Errorf("a Comb whose last tick failed rendered an observed_healthy row: %s", panel)
	}
	// The failing Comb must not render a green cause anywhere.
	assertNeverGreen(t, panel, "")
}

func TestClusterEvidencePage_StaleSuccessIsNeverRenderedAsCurrent(t *testing.T) {
	// A Comb whose last successful tick was three hours ago, against its
	// own 5m interval (so a 15m freshness limit). "Observed healthy" would
	// be a lie here; the page must say the success is stale.
	now := time.Now()
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}, RaftReachable: true, Members: []*rpcpb.RaftMember{{NodeId: "apiarium", Suffrage: "Voter"}}},
		hostStatsResp: &rpcpb.HostStatsResponse{
			NodeId: "apiarium", ReconcileIntervalSeconds: 300,
			LastReconcileAttemptUnix: now.Add(-3 * time.Hour).Unix(),
			LastReconcileSuccessUnix: now.Add(-3 * time.Hour).Unix(),
		},
	}
	s, err := NewServer(client, nil, nil, &fakePeerHostStatsClient{}, ".apiary.work", "17700", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/host/apiarium/evidence", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	panel := causesPanel(t, rec.Body.String())
	if !strings.Contains(panel, "observed healthy (stale)") {
		t.Errorf("a 3-hour-old success was not marked stale: %s", panel)
	}
	if !strings.Contains(panel, `<span class="badge stale">stale</span>`) {
		t.Errorf("stale row carries no stale badge: %s", panel)
	}
	if !strings.Contains(panel, "3h") {
		t.Errorf("stale row does not show how old the observation is: %s", panel)
	}
	// The row's own state is still observed_healthy (staleness is a
	// separate axis, not a fourth state), but its badge must be the dashed
	// `stale` class, never green.
	assertNeverGreen(t, panel, "")
	if class, _ := combCauseBadge(combCauseObservedHealthy, true); class == "ready" {
		t.Fatal("guard test is wrong: a stale healthy state must not be green")
	}
}

func TestClusterEvidencePage_UnreachableCombRendersTheHonestPanelNeverGreen(t *testing.T) {
	// The Comb is a known member but its own managerd cannot be dialed.
	// The panel must say "not observed" and explain why - never render an
	// empty panel, and never render a green one.
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium", "freebsd-apiary"},
		RaftReachable: true, Members: []*rpcpb.RaftMember{{NodeId: "apiarium", Suffrage: "Voter"}},
	}}
	peers := &fakePeerHostStatsClient{err: errors.New("connection refused")}
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/host/freebsd-apiary/evidence", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 - an unreachable Comb is a finding, not a page error", rec.Code)
	}
	panel := causesPanel(t, rec.Body.String())
	if !strings.Contains(panel, `data-evidence-state="never_observed"`) {
		t.Errorf("panel overall state is not never_observed: %s", panel)
	}
	if !strings.Contains(panel, "not observed") {
		t.Errorf("panel does not say the Comb was not observed: %s", panel)
	}
	if !strings.Contains(panel, "connection refused") {
		t.Errorf("panel dropped the real dial error: %s", panel)
	}
	// No row may claim a successful observation, and none may be green.
	if strings.Contains(panel, `data-cause-state="observed_healthy"`) {
		t.Errorf("an unreachable Comb produced an observed_healthy row: %s", panel)
	}
	assertNeverGreen(t, panel, "")
	// The honest panel is populated, not empty.
	if strings.Count(panel, "<tr data-cause-state=") < 2 {
		t.Errorf("expected both the reachability cause and the tick-error gap; got: %s", panel)
	}
	if !strings.Contains(panel, "firstErr") {
		t.Errorf("panel does not name the plumbing that would explain a failing tick: %s", panel)
	}
}

func TestClusterEvidencePage_ResourceListFetchFailureRendersAGapNotAnEmptyPanel(t *testing.T) {
	now := time.Now()
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}, RaftReachable: true, Members: []*rpcpb.RaftMember{{NodeId: "apiarium", Suffrage: "Voter"}}},
		hostStatsResp: &rpcpb.HostStatsResponse{
			NodeId: "apiarium", ReconcileIntervalSeconds: 300,
			LastReconcileAttemptUnix: now.Add(-time.Minute).Unix(),
			LastReconcileSuccessUnix: now.Add(-time.Minute).Unix(),
		},
		listResp: &rpcpb.ListVMsResponse{Error: "raft: no leader"},
	}
	s, err := NewServer(client, nil, nil, &fakePeerHostStatsClient{}, ".apiary.work", "17700", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/host/apiarium/evidence", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 - a failed resource read is fail-soft, never a 500", rec.Code)
	}
	panel := causesPanel(t, rec.Body.String())
	if !strings.Contains(panel, "raft: no leader") {
		t.Errorf("panel does not report the real resource-read failure: %s", panel)
	}
	if !strings.Contains(panel, `data-cause-state="never_observed"`) {
		t.Errorf("a failed resource read did not produce a never_observed row: %s", panel)
	}
	// The VM list read failed, so no VM may be blamed.
	if strings.Contains(panel, "/vms/") {
		t.Errorf("panel linked a VM despite the VM list never being read: %s", panel)
	}
}

func TestClusterOverviewPage_CardShowsTheCauseLine(t *testing.T) {
	// The command center is where an operator looks first, so the cause
	// line has to be there - including the wording that distinguishes
	// "never observed" from "healthy".
	now := time.Now()
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}, RaftReachable: true, Members: []*rpcpb.RaftMember{{NodeId: "apiarium", Suffrage: "Voter"}}},
		hostStatsResp: &rpcpb.HostStatsResponse{
			NodeId: "apiarium", ReconcileIntervalSeconds: 300,
			LastReconcileAttemptUnix: now.Add(-time.Minute).Unix(),
		},
	}
	s, err := NewServer(client, nil, nil, &fakePeerHostStatsClient{}, ".apiary.work", "17700", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	start := strings.Index(body, `class="cockpit-node-cause"`)
	if start < 0 {
		t.Fatalf("overview card has no cause line; body: %s", body)
	}
	line := body[start : start+400]
	// Ticking but never having succeeded is a failure, not a healthy
	// result, and the card must say so.
	if !strings.Contains(line, "observed failed") {
		t.Errorf("cause line does not report the failed tick: %s", line)
	}
	if !strings.Contains(line, `href="/host/apiarium/evidence"`) {
		t.Errorf("cause line does not link to the full evidence: %s", line)
	}
}

func TestWorstCombCause_CarriesStalenessToTheOverallBadge(t *testing.T) {
	// The card/panel header badge is derived from (state, stale) together,
	// not from state alone. Collapsing the pair is precisely how a
	// three-hour-old success ends up with a green header above a correctly
	// greyed row, so the coupling is pinned here.
	causes := []combCauseView{
		{Source: "reconciler_last_tick", State: combCauseObservedHealthy, Stale: true, Summary: "stale success"},
	}
	state, stale, _ := worstCombCause(causes)
	if state != combCauseObservedHealthy || !stale {
		t.Fatalf("worstCombCause = (%q, stale=%v), want (observed_healthy, stale=true)", state, stale)
	}
	if class, _ := combCauseBadge(state, stale); class == "ready" {
		t.Error("a stale worst cause produced a green overall badge")
	}
	if class, _ := combCauseBadge(state, false); class != "ready" {
		t.Error("guard test is wrong: a fresh success must be green, or this test proves nothing")
	}

	// A missing read outranks a stale success on the overall badge, because
	// an incomplete panel is the more important thing to say - and neither
	// reading is green.
	causes = append(causes, combCauseView{Source: "reconcile_phase_gap", State: combCauseNeverObserved, Summary: "gap"})
	if state, _, _ = worstCombCause(causes); state != combCauseNeverObserved {
		t.Errorf("worst state = %q, want %q", state, combCauseNeverObserved)
	}

	// A real failure still outranks both.
	causes = append(causes, combCauseView{Source: "reconcile_phase", State: combCauseObservedFailed, Summary: "real failure"})
	if state, _, _ = worstCombCause(causes); state != combCauseObservedFailed {
		t.Errorf("worst state = %q, want %q", state, combCauseObservedFailed)
	}
}

func TestClusterEvidencePage_ReconcilerNeverTickedIsNotObservedNotHealthy(t *testing.T) {
	// The case most likely to be conflated with health: the Comb answers
	// every call, raft is fine, and the reconciler simply has not run yet.
	// "Nothing has gone wrong" and "nothing has been observed" are
	// different sentences, and only one of them is a green light.
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}, RaftReachable: true, Members: []*rpcpb.RaftMember{{NodeId: "apiarium", Suffrage: "Voter"}}},
		hostStatsResp: &rpcpb.HostStatsResponse{
			NodeId: "apiarium", ReconcileIntervalSeconds: 300,
			// Both timestamps zero: a Reconciler is configured, no tick yet.
		},
	}
	s, err := NewServer(client, nil, nil, &fakePeerHostStatsClient{}, ".apiary.work", "17700", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/host/apiarium/evidence", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	panel := causesPanel(t, rec.Body.String())
	if !strings.Contains(panel, `data-cause-state="never_observed"`) {
		t.Errorf("a reconciler that has never ticked produced no never_observed row: %s", panel)
	}
	if !strings.Contains(panel, "not observed") {
		t.Errorf("panel does not say the Comb was not observed: %s", panel)
	}
	if strings.Contains(panel, `data-cause-state="observed_healthy"`) {
		t.Errorf("a never-ticked reconciler produced an observed_healthy row: %s", panel)
	}
	// Nothing may be green: not the overall badge, not the row.
	assertNeverGreen(t, panel, "")
	// The "Observed" cell must not invent a timestamp for something that
	// was never observed.
	if strings.Contains(panel, "1970") || strings.Contains(panel, "year 1") {
		t.Errorf("panel rendered a bogus timestamp for a never-observed Comb: %s", panel)
	}
}

func TestClusterEvidencePage_NoReconcilerIsNotApplicableNotHealthy(t *testing.T) {
	// reconcile_interval_seconds == 0 means "no Reconciler configured" -
	// internal/health calls that healthy because nothing was asked. The
	// cause panel must still refuse to call it an observed success.
	client := &fakeClient{
		statusResp:    &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}, RaftReachable: true, Members: []*rpcpb.RaftMember{{NodeId: "apiarium", Suffrage: "Voter"}}},
		hostStatsResp: &rpcpb.HostStatsResponse{NodeId: "apiarium"},
	}
	s, err := NewServer(client, nil, nil, &fakePeerHostStatsClient{}, ".apiary.work", "17700", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/host/apiarium/evidence", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	panel := causesPanel(t, rec.Body.String())
	if !strings.Contains(panel, `data-cause-state="not_applicable"`) {
		t.Errorf("a Comb with no Reconciler produced no not_applicable row: %s", panel)
	}
	if strings.Contains(panel, `data-cause-state="observed_healthy"`) {
		t.Errorf("a Comb with no Reconciler produced an observed_healthy row: %s", panel)
	}
	if !strings.Contains(panel, "not the same as healthy") && !strings.Contains(panel, "not a statement about") {
		t.Errorf("panel does not distinguish 'nothing was asked' from 'nothing is wrong': %s", panel)
	}
}
