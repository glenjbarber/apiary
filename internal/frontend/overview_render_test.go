package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/manager"
)

// The Blend overview's render tests.
//
// These cover the acceptance criteria the handoff names explicitly:
// the three comb views, the cookie fallback, and role filtering.
//
// They are RENDER tests, not model tests: the model is in
// overview.go, and what matters is that the right thing reaches the
// page.

// overviewTestClient is a Colony with two Combs and a few cells, so
// the views have something to show.
func overviewTestClient() *fakeClient {
	return &fakeClient{
		statusResp: &rpcpb.StatusResponse{
			ManagerNodeId:    "brood",
			RaftReachable:    true,
			RaftState:        "Follower",
			RaftNodeId:       "brood",
			RaftLeaderId:     "brood",
			RaftAppliedIndex: 181,
			KnownNodeIds:     []string{"brood", "drone"},
			Members: []*rpcpb.RaftMember{
				{NodeId: "brood", Suffrage: "Voter"},
				{NodeId: "drone", Suffrage: "Voter"},
			},
		},
		listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{
			{Id: "web-01", Name: "web", NodeId: "brood", DesiredState: rpcpb.VMState_VM_STATE_RUNNING},
			{Id: "db-01", Name: "db", NodeId: "brood", DesiredState: rpcpb.VMState_VM_STATE_STOPPED},
		}},
		listJailsResp: &rpcpb.ListJailsResponse{Jails: []*rpcpb.JailDefinition{
			{Id: "dns", Name: "dns", NodeId: "drone", DesiredState: rpcpb.JailState_JAIL_STATE_RUNNING},
		}},
		hostStatsResp: &rpcpb.HostStatsResponse{
			NodeId: "brood", Cpu: &rpcpb.CPUStats{Cores: 8, LoadAvg_1: 1.6},
			Mem: &rpcpb.MemStats{TotalBytes: 1 << 30, FreeBytes: 1 << 29},
		},
	}
}

// renderOverviewAt renders the landing page with the given query.
func renderOverviewAt(t *testing.T, client *fakeClient, query string) *httptest.ResponseRecorder {
	t.Helper()
	s := newTestServer(t, client)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	return rec
}

// TestOverviewShowsStatusLineAttentionAndCombsInOrder pins the
// handoff's central acceptance criterion: GET / shows, in order, the
// status line, Needs attention, and Combs.
//
// The ORDER matters and is checked by position, not by presence: three
// sections in the wrong order is a different page from three sections
// in the right one.
func TestOverviewShowsStatusLineAttentionAndCombsInOrder(t *testing.T) {
	body := renderOverviewAt(t, overviewTestClient(), "").Body.String()

	status := strings.Index(body, `id="ov-status"`)
	attention := strings.Index(body, `id="ov-attention"`)
	combs := strings.Index(body, `id="ov-combs-body"`)

	if status < 0 {
		t.Fatal("the overview does not render a status line")
	}
	if attention < 0 {
		t.Fatal("the overview does not render a Needs-attention list")
	}
	if combs < 0 {
		t.Fatal("the overview does not render a Combs section")
	}
	if !(status < attention && attention < combs) {
		t.Errorf("the three sections are out of order: status=%d attention=%d combs=%d", status, attention, combs)
	}
}

// TestOverviewCombsViewsRender pins that each of the three views
// renders its own distinct markup, and that the default is Table.
func TestOverviewCombsViewsRender(t *testing.T) {
	for _, tc := range []struct {
		view   string
		wants  []string
		absent []string
	}{
		{
			view:   "table",
			wants:  []string{`class="ov-table"`, "<th>Comb</th>", "<th>Role</th>", "<th>Uptime</th>"},
			absent: []string{`class="ov-cards"`, `class="ov-matrix"`},
		},
		{
			view:   "cards",
			wants:  []string{`class="ov-cards"`, `class="ov-card"`, "Leader"},
			absent: []string{`class="ov-table"`, `class="ov-matrix"`},
		},
		{
			view:   "matrix",
			wants:  []string{`class="ov-matrix"`, "<th>Reachable</th>", "<th>Raft</th>", `class="ov-legend"`},
			absent: []string{`class="ov-table"`, `class="ov-cards"`},
		},
	} {
		t.Run(tc.view, func(t *testing.T) {
			body := renderOverviewAt(t, overviewTestClient(), "?combview="+tc.view).Body.String()
			for _, want := range tc.wants {
				if !strings.Contains(body, want) {
					t.Errorf("the %s view is missing %q", tc.view, want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(body, absent) {
					t.Errorf("the %s view also renders %q, which belongs to another view", tc.view, absent)
				}
			}
		})
	}
}

// TestOverviewDefaultCombViewIsTable pins the fallback: no cookie, no
// parameter, means Table.
func TestOverviewDefaultCombViewIsTable(t *testing.T) {
	body := renderOverviewAt(t, overviewTestClient(), "").Body.String()
	if !strings.Contains(body, `class="ov-table"`) {
		t.Error("the overview did not default to the Table view")
	}
	if !strings.Contains(body, `aria-pressed="true"`) {
		t.Error("no view button is marked pressed, so the segmented control does not show which view is selected")
	}
}

// TestOverviewCombViewCookieFallback pins the cookie's behaviour: a
// valid value is honoured, an invalid one falls back to Table, and a
// missing one falls back to Table.
//
// The invalid-value case matters most: a cookie is attacker- and
// accident-reachable, and an unrecognised value must never render a
// view that does not exist.
func TestOverviewCombViewCookieFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cookie string
		want   string
	}{
		{name: "valid cards cookie", cookie: "cards", want: "cards"},
		{name: "valid matrix cookie", cookie: "matrix", want: "matrix"},
		{name: "invalid cookie falls back to table", cookie: "not-a-view", want: "table"},
		{name: "empty cookie falls back to table", cookie: "", want: "table"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, overviewTestClient())
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: combViewCookieName, Value: tc.cookie})
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			switch tc.want {
			case "cards":
				if !strings.Contains(body, `class="ov-cards"`) {
					t.Error("the cards cookie did not select the cards view")
				}
			case "matrix":
				if !strings.Contains(body, `class="ov-matrix"`) {
					t.Error("the matrix cookie did not select the matrix view")
				}
			default:
				if !strings.Contains(body, `class="ov-table"`) {
					t.Error("the invalid cookie did not fall back to the table view")
				}
			}
		})
	}
}

// TestOverviewCombViewCookieIsSet pins that the server records the
// selection, so the choice survives a reload and a new tab.
func TestOverviewCombViewCookieIsSet(t *testing.T) {
	rec := renderOverviewAt(t, overviewTestClient(), "?combview=cards")

	cookies := rec.Result().Cookies()
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == combViewCookieName {
			found = c
		}
	}
	if found == nil {
		t.Fatalf("the overview did not set the %s cookie; got %v", combViewCookieName, cookies)
	}
	if found.Value != "cards" {
		t.Errorf("cookie value = %q, want %q", found.Value, "cards")
	}
	if found.Path != "/" {
		t.Errorf("cookie path = %q, want %q", found.Path, "/")
	}
	if !found.HttpOnly {
		t.Error("the cookie is not HttpOnly")
	}
	if found.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", found.SameSite)
	}
	if found.MaxAge != combViewCookieMaxAge {
		t.Errorf("cookie MaxAge = %d, want %d (one year)", found.MaxAge, combViewCookieMaxAge)
	}
}

// TestOverviewCombViewFragmentSwapsOnlyTheCombsSection pins that the
// segmented control's hx-get returns the Combs section and NOT a whole
// page - a fragment route that returned a full document would nest a
// page inside the page.
func TestOverviewCombViewFragmentSwapsOnlyTheCombsSection(t *testing.T) {
	s := newTestServer(t, overviewTestClient())
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/overview/combs?view=cards", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="ov-cards"`) {
		t.Error("the Combs fragment did not render the cards view")
	}
	if strings.Contains(body, "<!DOCTYPE html>") {
		t.Error("the Combs fragment returned a whole document, not a fragment")
	}
	if strings.Contains(body, `id="ov-status"`) {
		t.Error("the Combs fragment carried the status line too; it must swap only its own section")
	}
}

// TestOverviewPaletteIsRoleFiltered is the acceptance criterion for
// the command palette: a Viewer sees no mutating action at all.
//
// The check is on the SERVER's response, not on what the browser
// renders - a hidden action in the markup is still an action the
// palette offered.
func TestOverviewPaletteIsRoleFiltered(t *testing.T) {
	// A Viewer session.
	viewer := &fakeClient{listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{{Id: "vm-1", NodeId: "brood"}}}}
	s, err := NewServer(viewer, fakeAuthenticator{user: "viewer", pass: "secret"},
		map[string]manager.Role{"viewer": manager.RoleViewer}, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, _ := s.sessions.Create("viewer", manager.RoleViewer)
	req := httptest.NewRequest(http.MethodGet, "/palette?q=", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, forbidden := range []string{
		"Create virtual machine",
		"Create jail",
		"Snapshot cell",
		"Upload ISO image",
		"Pending join requests",
		"API keys",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the Viewer's palette offers the mutating action %q", forbidden)
		}
	}
	// And a Viewer still gets the read-only destinations.
	if !strings.Contains(body, "vm-1") {
		t.Error("the Viewer's palette does not offer the cell it may read")
	}
}

// TestOverviewPaletteOffersOperatorActions pins the other half: an
// Operator does get the actions they may perform.
func TestOverviewPaletteOffersOperatorActions(t *testing.T) {
	op := &fakeClient{listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{{Id: "vm-1", NodeId: "brood"}}}}
	s, err := NewServer(op, fakeAuthenticator{user: "op", pass: "secret"},
		map[string]manager.Role{"op": manager.RoleOperator}, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, _ := s.sessions.Create("op", manager.RoleOperator)
	req := httptest.NewRequest(http.MethodGet, "/palette?q=", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body := rec.Body.String()

	for _, want := range []string{"Create virtual machine", "Create jail", "Upload ISO image"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Operator's palette is missing %q", want)
		}
	}
	// But not the Admin-only ones.
	if strings.Contains(body, "Pending join requests") {
		t.Error("the Operator's palette offers an Admin-only action")
	}
}

// TestOverviewNeedsAttentionIsRoleFiltered pins that a Viewer does not
// see the Admin-only pending-join-requests entries.
func TestOverviewNeedsAttentionIsRoleFiltered(t *testing.T) {
	viewer := &fakeClient{}
	s, err := NewServer(viewer, fakeAuthenticator{user: "viewer", pass: "secret"},
		map[string]manager.Role{"viewer": manager.RoleViewer}, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, _ := s.sessions.Create("viewer", manager.RoleViewer)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body := rec.Body.String()

	if strings.Contains(body, "requested to join the Colony") {
		t.Error("a Viewer sees the Admin-only pending-join-requests entries")
	}
}

// TestOverviewShowsUnknownRatherThanZero pins the handoff's hardest
// rule: a value the server cannot compute renders as "unknown", never
// as zero or ok.
//
// The test server's managerd answers, but the Comb's own HostStats
// does not, so its meters have no reading. A meter that rendered "0%"
// would claim the Comb was idle; one that rendered "ok" would claim it
// was healthy.
func TestOverviewShowsUnknownRatherThanZero(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId: "brood", RaftReachable: true, KnownNodeIds: []string{"brood"},
		Members: []*rpcpb.RaftMember{{NodeId: "brood", Suffrage: "Voter"}},
	}}
	// No hostStatsResp, so the Comb's own readings are unobserved.
	body := renderOverviewAt(t, client, "").Body.String()

	if !strings.Contains(body, "unknown") {
		t.Error("an unobserved value did not render as unknown")
	}
	// The assertion is scoped to the rendered CONTENT, not to the
	// stylesheet: the page's own CSS contains "0%" in a rule about
	// a meter track's width, and a document-wide search would
	// match that.
	if strings.Contains(body, ">0%<") {
		t.Error("an unobserved value rendered as 0%, which claims the resource is empty")
	}
	if strings.Contains(body, "ov-num\">0%") {
		t.Error("an unobserved meter rendered as 0% with a bar")
	}
}

// TestOverviewStatusLineOmitsZeroWarnings pins the handoff's rule that
// the warnings count is omitted when it is zero.
func TestOverviewStatusLineOmitsZeroWarnings(t *testing.T) {
	body := renderOverviewAt(t, overviewTestClient(), "").Body.String()
	if strings.Contains(body, ">Warnings<") {
		t.Error("the status line shows a warnings count when there are no warnings")
	}
}

// TestOverviewStatusLineCarriesEveryMetric pins that the status line
// carries all of the handoff's items.
func TestOverviewStatusLineCarriesEveryMetric(t *testing.T) {
	body := renderOverviewAt(t, overviewTestClient(), "").Body.String()
	for _, want := range []string{
		"Combs", "Cells", "Raft", "HAST", "ZFS",
	} {
		if !strings.Contains(body, `class="ov-metric-label">`+want+`<`) {
			t.Errorf("the status line is missing its %q metric", want)
		}
	}
}

// TestOverviewRefreshesEveryTenSecondsWhileVisible pins the refresh
// contract: each of the three sections polls on the same interval, and
// each poll is gated on the browser tab being visible.
func TestOverviewRefreshesEveryTenSecondsWhileVisible(t *testing.T) {
	body := renderOverviewAt(t, overviewTestClient(), "").Body.String()
	for _, want := range []string{
		`hx-get="/overview/status" hx-trigger="every 10s [document.visibilityState === 'visible']"`,
		`hx-get="/overview/attention" hx-trigger="every 10s [document.visibilityState === 'visible']"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the overview is missing its %q refresh trigger", want)
		}
	}
	// The Combs section's own poll must carry the SELECTED view, so a
	// refresh keeps the view the operator chose.
	if !strings.Contains(body, `hx-get="/overview/combs?view=table"`) {
		t.Error("the Combs section's refresh does not carry the selected view")
	}
}

// TestOverviewNeverShowsAMissingValueAsOK pins the rule at the level
// of the Comb's own state: a Comb that could not be reached is
// unknown, never ok.
func TestOverviewNeverShowsAMissingValueAsOK(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId: "brood", RaftReachable: true, KnownNodeIds: []string{"brood"},
		Members: []*rpcpb.RaftMember{{NodeId: "brood", Suffrage: "Voter"}},
	}}
	body := renderOverviewAt(t, client, "").Body.String()
	if strings.Contains(body, `class="dot ok"`) {
		t.Error("a Comb with no valid observation is rendered as ok")
	}
	if !strings.Contains(body, `class="dot unknown"`) {
		t.Error("a Comb with no valid observation does not render the unknown state")
	}
}

// TestOverviewEveryStatusStateRendersDistinctly pins that all seven
// states of the vocabulary are drawn from the SAME set of CSS classes,
// so no page can invent a colour.
//
// The matrix's legend is where all seven appear, always - a legend
// that only listed the states present would change shape as the Colony
// changed.
func TestOverviewEveryStatusStateRendersDistinctly(t *testing.T) {
	body := renderOverviewAt(t, overviewTestClient(), "?combview=matrix").Body.String()
	for _, state := range []string{"ok", "warn", "critical", "stale", "unknown", "not-applicable", "contradictory"} {
		if !strings.Contains(body, `class="dot `+state+`"`) {
			t.Errorf("the status vocabulary's %q state has no distinct rendering", state)
		}
	}
}

// TestOverviewStatusLineShowsObservedValues pins that an OBSERVED
// value renders as its value and not as "unknown".
//
// This is the mirror of the "never show a missing value as zero" rule:
// a value that WAS observed must not be hidden behind the unknown
// style, or the status line would be equally untrustworthy in the
// other direction - an operator would learn to ignore it either way.
func TestOverviewStatusLineShowsObservedValues(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{
			ManagerNodeId: "brood", RaftReachable: true, RaftState: "Follower",
			RaftNodeId: "brood", RaftLeaderId: "brood",
			KnownNodeIds: []string{"brood"},
			Members:      []*rpcpb.RaftMember{{NodeId: "brood", Suffrage: "Voter"}},
		},
		hostStatsResp: &rpcpb.HostStatsResponse{
			NodeId: "brood", Cpu: &rpcpb.CPUStats{Cores: 8, LoadAvg_1: 2.5},
			Mem:   &rpcpb.MemStats{TotalBytes: 1 << 30, FreeBytes: 1 << 29},
			Pools: []*rpcpb.PoolStats{{Name: "tank", SizeBytes: 1 << 40, CapacityPct: 52, Health: "ONLINE"}},
		},
	}
	body := renderOverviewAt(t, client, "").Body.String()

	// The raft leader was observed, so the line names it.
	if !strings.Contains(body, ">brood</b>") {
		t.Errorf("the status line does not name the observed raft leader: %s", body)
	}
	// The ZFS pool was read, so the line shows its percentage.
	if !strings.Contains(body, "52%") {
		t.Errorf("the status line does not show the observed ZFS pool usage: %s", body)
	}
	// And the unknown style is reserved for what really was unknown.
	if strings.Contains(body, "is-unknown\" title=\"leader brood") {
		t.Error("the observed raft leader is rendered as unknown")
	}
}

// TestOverviewCombViewButtonsAreNamed pins that the segmented control's
// buttons carry their NAMES, not their internal identifiers. "table" is
// a value; "Table" is a label an operator reads.
func TestOverviewCombViewButtonsAreNamed(t *testing.T) {
	body := renderOverviewAt(t, overviewTestClient(), "").Body.String()
	for _, want := range []string{">Table</a>", ">Cards</a>", ">Matrix</a>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the comb view switch is missing its %q button", want)
		}
	}
}

// TestOverviewCombViewWorksWithoutJavaScript pins the handoff's
// "with JavaScript disabled, the switch still works through links"
// criterion.
//
// The check is that the segmented control's buttons ARE links - each
// carries a real href to the full page with that view - so a browser
// with scripting off still navigates to the right view. The htmx
// attributes are an ENHANCEMENT on top of that, not the mechanism.
func TestOverviewCombViewWorksWithoutJavaScript(t *testing.T) {
	body := renderOverviewAt(t, overviewTestClient(), "").Body.String()
	for _, view := range []string{"table", "cards", "matrix"} {
		if !strings.Contains(body, `href="/?combview=`+view+`"`) {
			t.Errorf("the comb view switch has no plain link for the %q view, so it cannot work without JavaScript", view)
		}
	}
	// And following one of those links really does render that view.
	for _, view := range []string{"table", "cards", "matrix"} {
		body := renderOverviewAt(t, overviewTestClient(), "?combview="+view).Body.String()
		switch view {
		case "table":
			if !strings.Contains(body, `class="ov-table"`) {
				t.Error("the no-JS link for the table view did not render the table view")
			}
		case "cards":
			if !strings.Contains(body, `class="ov-cards"`) {
				t.Error("the no-JS link for the cards view did not render the cards view")
			}
		case "matrix":
			if !strings.Contains(body, `class="ov-matrix"`) {
				t.Error("the no-JS link for the matrix view did not render the matrix view")
			}
		}
	}
}

// TestOverviewAccessibilityContract pins the handoff's section 12
// requirements that are checkable in the markup.
func TestOverviewAccessibilityContract(t *testing.T) {
	body := renderOverviewAt(t, overviewTestClient(), "").Body.String()
	for _, want := range []string{
		// The rail is a nav with an aria-label.
		`<nav class="rail" aria-label="Primary navigation">`,
		// The active rail item carries aria-current.
		`aria-current="page"`,
		// The palette is a modal dialog.
		`role="dialog"`,
		`aria-modal="true"`,
		// The segmented control is a group of buttons with
		// aria-pressed.
		`role="group" aria-label="Comb view"`,
		`aria-pressed="true"`,
		`aria-pressed="false"`,
		// Every status dot has a text equivalent.
		`class="visually-hidden"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the overview is missing its accessibility contract %q", want)
		}
	}
}

// TestOverviewEveryStatusDotHasATextEquivalent pins that a status dot
// is never the only cue.
//
// The dot is aria-hidden, and a visually-hidden span beside it carries
// the state's name. A dot alone would be invisible to a screen reader
// and meaningless to a colour-blind reader.
func TestOverviewEveryStatusDotHasATextEquivalent(t *testing.T) {
	body := renderOverviewAt(t, overviewTestClient(), "").Body.String()
	dots := strings.Count(body, `class="dot `)
	hidden := strings.Count(body, `class="visually-hidden"`)
	if dots == 0 {
		t.Fatal("the overview renders no status dots at all")
	}
	if hidden < dots {
		t.Errorf("the overview renders %d status dots but only %d text equivalents; a dot must never be the only cue", dots, hidden)
	}
}

// TestOverviewShowsUnknownForValuesWithNoEvidenceSource pins the
// handoff's rule against the two columns that have no evidence source
// in this build.
//
// The UPTIME and the BASE SYSTEM are both in the design, and neither
// can be read: no RPC in this codebase carries a host's uptime, and
// the base system can only be read for the Comb this frontend is
// colocated with - there is no peer-forwarded read for the others.
//
// So both render as "unknown" with a reason, and neither is ever
// filled with a plausible-looking number. A uptime column that showed
// "41d" for every Comb would look right and be wrong for every Comb
// but one.
func TestOverviewShowsUnknownForValuesWithNoEvidenceSource(t *testing.T) {
	body := renderOverviewAt(t, overviewTestClient(), "").Body.String()

	// The table's Uptime cell.
	if !strings.Contains(body, "unknown") {
		t.Error("the overview renders no unknown values at all")
	}
	// The card's base-system line says so in words. The cards view is
	// a different render of the same data, so it is asked for
	// separately.
	cards := renderOverviewAt(t, overviewTestClient(), "?combview=cards").Body.String()
	if !strings.Contains(cards, "base system unknown") {
		t.Error("the cards view does not say the base system is unknown")
	}
	// And no invented uptime.
	for _, invented := range []string{"up 41d", "up 3d", "up 12d"} {
		if strings.Contains(body, invented) {
			t.Errorf("the overview shows an invented uptime %q", invented)
		}
	}
}

// TestOverviewRemoteCombMetersAreNotTheLocalHosts pins a real bug the
// redesign surfaced.
//
// fetchHostStats falls back to this frontend's OWN colocated managerd
// when there is no peer forwarding configured. For a Comb that is NOT
// local, that answer is this frontend's own host data wearing another
// Comb's name. The health machinery already refused to treat it as
// evidence; the overview's METERS must refuse it too, or every row
// would show this Comb's CPU and memory.
func TestOverviewRemoteCombMetersAreNotTheLocalHosts(t *testing.T) {
	// Two Combs: the local one, and a remote one. The fake's
	// hostStatsResp answers for whichever is asked - and with no peer
	// forwarding, the remote one is answered with the LOCAL host's
	// data.
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{
			ManagerNodeId: "brood", RaftReachable: true,
			KnownNodeIds: []string{"brood", "drone"},
			Members: []*rpcpb.RaftMember{
				{NodeId: "brood", Suffrage: "Voter"},
				{NodeId: "drone", Suffrage: "Voter"},
			},
		},
		hostStatsResp: &rpcpb.HostStatsResponse{
			NodeId: "brood", Cpu: &rpcpb.CPUStats{Cores: 8, LoadAvg_1: 2.5},
			Mem: &rpcpb.MemStats{TotalBytes: 1 << 30, FreeBytes: 1 << 29},
		},
	}
	body := renderOverviewAt(t, client, "").Body.String()

	// The remote Comb's ROW must not carry the local host's readings.
	//
	// The row is scoped by the Combs TABLE, not by the first link to
	// the Comb on the page - the Needs-attention list links to the
	// same Comb, and a document-wide slice from the first link would
	// measure the wrong element.
	table := body[strings.Index(body, `class="ov-table"`):]
	i := strings.Index(table, `href="/host/drone"`)
	if i < 0 {
		t.Fatal("the remote Comb is not in the Combs table")
	}
	row := table[i : i+2500]
	if strings.Contains(row, "2.50 load, 8 cores") {
		t.Error("the remote Comb's row shows this frontend's own CPU reading")
	}
	if strings.Contains(row, "512.00 MB free") {
		t.Error("the remote Comb's row shows this frontend's own memory reading")
	}
	// And the LOCAL Comb's row does carry them - so the test is
	// measuring the right thing and not a page that simply lost its
	// readings.
	localTable := table[:i]
	if !strings.Contains(localTable, "2.50 load, 8 cores") {
		t.Error("the local Comb's row lost its own CPU reading; this test is not measuring what it means to")
	}
}
