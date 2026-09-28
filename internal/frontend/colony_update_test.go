package frontend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/colonyupdate"
	"github.com/glenjbarber/apiary/internal/manager"
)

// Tests for the controlled Colony update page (ADR-0145).
//
// What is being tested here, stated plainly because the project's own
// "Reading quiet results" rule makes silence dangerous:
//
//   - The page renders every Comb, with honest per-Comb state.
//   - At most ONE Update control is ever enabled, and which one is a
//     fact the server derived, not a choice the page made.
//   - The six outcomes render as six distinct things, and the two
//     inconclusive ones never look like success or like failure.
//   - A backend refusal is rendered as a refusal, from the backend's
//     own words.
//   - A stale or forged client cannot make the page claim success, name
//     a target the server did not name, or enable a control the server
//     did not enable.
//
// What is NOT tested here, and must not be read as tested: that anything
// is actually enforced. Nothing in this branch enforces colony-wide
// single-flight. The only implementation of colonyupdate.Controller that
// exists is colonyupdate.Inert, which refuses everything. The tests below
// drive a FAKE, so they prove the page renders refusals and states
// correctly; they prove nothing about whether a real system would refuse
// anything.

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// fakeColonyUpdate is a colonyupdate.Controller whose answers a test sets
// directly, and which records what it was asked.
//
// It exists to make every state on the page reachable without a live
// update system, which does not exist. It is a test double and nothing
// else; it is deliberately NOT in internal/colonyupdate, so it can never
// be mistaken for the inert implementation a binary would actually ship.
type fakeColonyUpdate struct {
	mu sync.Mutex

	// state is what ColonyUpdateState returns. stateErr, when non-nil,
	// makes the read fail instead.
	state    colonyupdate.State
	stateErr error

	// requestErr is what RequestColonyUpdate returns. requestState, when
	// set, is what it returns alongside a nil error.
	requestErr   error
	requestState *colonyupdate.State

	requests []colonyupdate.Request
	reads    int
}

var _ colonyupdate.Controller = (*fakeColonyUpdate)(nil)

func (f *fakeColonyUpdate) ColonyUpdateState(context.Context) (colonyupdate.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.stateErr != nil {
		return colonyupdate.State{}, f.stateErr
	}
	return f.state, nil
}

func (f *fakeColonyUpdate) RequestColonyUpdate(_ context.Context, req colonyupdate.Request) (colonyupdate.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.requestErr != nil {
		return colonyupdate.State{}, f.requestErr
	}
	if f.requestState != nil {
		return *f.requestState, nil
	}
	return f.state, nil
}

func (f *fakeColonyUpdate) recorded() []colonyupdate.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]colonyupdate.Request(nil), f.requests...)
}

func (f *fakeColonyUpdate) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// updateTestCombs is the four-voter membership every render test uses, so
// the page is exercised against the shape the real Colony has (ADR-0145
// reasons about four voters and a majority of three throughout).
func updateTestCombs() []string {
	return []string{"brood.lab3.home.arpa", "buzz.lab3.home.arpa", "drone.lab3.home.arpa", "sting.lab3.home.arpa"}
}

// updateTestStatus is a StatusResponse for that membership, anchored on
// the first Comb as leader so the per-row role evidence is real rather
// than uniformly unobserved.
func updateTestStatus() *rpcpb.StatusResponse {
	return &rpcpb.StatusResponse{
		ManagerNodeId:    "brood.lab3.home.arpa",
		RaftReachable:    true,
		RaftState:        "Follower",
		RaftNodeId:       "brood.lab3.home.arpa",
		RaftLeaderId:     "brood.lab3.home.arpa",
		RaftAppliedIndex: 181,
		KnownNodeIds:     updateTestCombs(),
	}
}

// idleNominatedState is the healthy idle case: the update system answered,
// enforces single-flight, and is offering exactly one Comb.
func idleNominatedState(nodeID string) colonyupdate.State {
	return colonyupdate.State{
		Observed:        true,
		Phase:           colonyupdate.OutcomeUnobserved,
		Detail:          "no update is in progress",
		NominatedNodeID: nodeID,
		NominatedDetail: "the system derived this Comb as the one to step on, re-reading leadership now",
		BackendEnforced: true,
		EnforcedDetail:  "the update system refuses a second request while one is held",
		Steps:           nil,
	}
}

// newColonyUpdateTestServer builds a Server wired to a fake controller.
// Login is disabled (s.auth == nil) so the session is treated as full
// privilege, which is how every other page test in this package sets up;
// the role-gated cases below use a server with a real authenticator.
func newColonyUpdateTestServer(t *testing.T, controller colonyupdate.Controller) *Server {
	t.Helper()
	client := &fakeClient{statusResp: updateTestStatus()}
	s := newTestServer(t, client)
	s.SetColonyUpdateController(controller)
	return s
}

// renderColonyUpdate GETs the update page and returns the body, failing
// the test on any non-200.
func renderColonyUpdate(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/colony-update", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /colony-update status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// postColonyUpdate POSTs an update request for nodeID and returns the
// recorder, without asserting on the status - several tests need to
// inspect a non-200.
func postColonyUpdate(t *testing.T, s *Server, nodeID string) *httptest.ResponseRecorder {
	t.Helper()
	return postColonyUpdateWith(t, s, nodeID, nil)
}

// postColonyUpdateWith is postColonyUpdate with cookies, for the role
// cases. The Content-Type header is set on the REQUEST, not the recorder:
// r.FormValue parses a url-encoded body only when the request declares it,
// and setting it on the response afterwards changes nothing - a bug this
// exact helper shape is prone to, so the header is set in one place here.
func postColonyUpdateWith(t *testing.T, s *Server, nodeID string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"node_id": {nodeID}}
	req := httptest.NewRequest(http.MethodPost, "/colony-update/request", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// loginAs drives a real login through the real handler and returns the
// session cookies, so a test can exercise a role rather than the absence
// of a session.
func loginAs(t *testing.T, s *Server, username, password string) []*http.Cookie {
	t.Helper()
	form := url.Values{"username": {username}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("login as %s status = %d, want 302; body=%s", username, rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("login as %s set no session cookie", username)
	}
	return cookies
}

// ---------------------------------------------------------------------------
// Every Comb is listed
// ---------------------------------------------------------------------------

// TestColonyUpdateListsEveryComb is the first requirement and the easiest
// one to get wrong quietly: a page that renders three of four Combs, or
// that drops the one it cannot reach, still looks completely plausible.
// The test asserts on every member of the membership raft reported, and
// it does so by asking for a data-comb attribute the template emits on
// each row rather than by counting rows.
func TestColonyUpdateListsEveryComb(t *testing.T) {
	body := renderColonyUpdate(t, newColonyUpdateTestServer(t, &fakeColonyUpdate{state: idleNominatedState("drone.lab3.home.arpa")}))

	for _, nodeID := range updateTestCombs() {
		if !strings.Contains(body, `data-comb="`+nodeID+`"`) {
			t.Errorf("Comb %s is missing from the update page; a page that claims to list every Comb must", nodeID)
		}
		if !strings.Contains(body, ">Update this Comb</button>") {
			t.Fatalf("no update control rendered at all; body=%s", body)
		}
	}
	if got := strings.Count(body, `data-comb="`); got != len(updateTestCombs()) {
		t.Errorf("rendered %d Comb rows, want %d (one per member of the reported membership)", got, len(updateTestCombs()))
	}
}

// TestColonyUpdateRowsCarryHonestPerCombState checks each row says
// something true and specific rather than showing empty cells. The role
// column is the interesting one: it must be a word, never blank, because
// a blank role cell reads as "no role", which is a claim this page has no
// evidence for.
func TestColonyUpdateRowsCarryHonestPerCombState(t *testing.T) {
	body := renderColonyUpdate(t, newColonyUpdateTestServer(t, &fakeColonyUpdate{state: idleNominatedState("drone.lab3.home.arpa")}))

	// The leader, as read from the anchor Status call, is marked - and
	// marked as a reading rather than as a choice.
	if !strings.Contains(body, "Colony leader") {
		t.Errorf("the page does not show which Comb this raftd reports as leader; body excerpt: %s", colonyUpdateExcerpt(body, "Live control"))
	}
	for _, want := range []string{"Leader", "Follower", "Reachable", "No update recorded"} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing the per-Comb verdict %q", want)
		}
	}
	// A row that is not the target must say so in words, so an operator
	// cannot read a stale page as "this one is done".
	if !strings.Contains(body, "it is not the Comb currently being updated") {
		t.Error("a non-target row does not say that it is not being updated")
	}
}

// ---------------------------------------------------------------------------
// Exactly one control enabled
// ---------------------------------------------------------------------------

// TestColonyUpdateEnablesExactlyTheNominatedComb is the load-bearing
// control-count test. Four Combs, one nomination: exactly one control
// must be enabled, it must be the nominated Comb's, and the other three
// must be disabled AND must say why.
//
// The enabled count is measured from the rendered `disabled` attribute,
// because that is the attribute the browser acts on. A count taken from a
// CSS class would prove nothing about what an operator could click.
func TestColonyUpdateEnablesExactlyTheNominatedComb(t *testing.T) {
	body := renderColonyUpdate(t, newColonyUpdateTestServer(t, &fakeColonyUpdate{state: idleNominatedState("drone.lab3.home.arpa")}))

	if got := colonyUpdateEnabledControls(body); got != 1 {
		t.Errorf("page has %d enabled update controls, want exactly 1", got)
	}

	controls := colonyUpdateControls(body)
	if len(controls) != len(updateTestCombs()) {
		t.Fatalf("page rendered %d update controls, want %d (one per Comb)", len(controls), len(updateTestCombs()))
	}
	enabledComb := ""
	for _, control := range controls {
		disabled := strings.Contains(control, " disabled")
		nodeID := updateControlNodeID(control)
		if nodeID == "drone.lab3.home.arpa" {
			if disabled {
				t.Errorf("the nominated Comb's control is disabled: %s", control)
			}
			enabledComb = nodeID
			continue
		}
		if !disabled {
			t.Errorf("non-nominated Comb %s has an enabled control: %s", nodeID, control)
		}
	}
	if enabledComb != "drone.lab3.home.arpa" {
		t.Errorf("the enabled control belongs to %q, want the nominated Comb drone.lab3.home.arpa", enabledComb)
	}

	// Every disabled control carries its reason, and that reason names
	// the Comb the system is offering rather than merely saying "no".
	for _, nodeID := range []string{"brood.lab3.home.arpa", "buzz.lab3.home.arpa", "sting.lab3.home.arpa"} {
		want := "the update system is offering drone.lab3.home.arpa right now"
		if !strings.Contains(body, want) {
			t.Errorf("no row explains that the system is offering another Comb; expected %q", want)
		}
		_ = nodeID
	}
}

// TestColonyUpdateEnablesNothingWhenNoCombIsOffered covers the other half
// of the invariant: zero is a legal answer, and it must be zero rather
// than four. A page that falls back to "enable them all when the system
// has not said" would be handing the operator back the order choice that
// ADR-0145 exists to remove.
func TestColonyUpdateEnablesNothingWhenNoCombIsOffered(t *testing.T) {
	state := idleNominatedState("")
	state.NominatedDetail = "the health gate is not met, so no Comb is on offer"

	body := renderColonyUpdate(t, newColonyUpdateTestServer(t, &fakeColonyUpdate{state: state}))

	if got := colonyUpdateEnabledControls(body); got != 0 {
		t.Errorf("page has %d enabled update controls with no nomination, want 0", got)
	}
	if !strings.Contains(body, "No Comb is on offer right now") {
		t.Error("page does not say that no Comb is on offer")
	}
	if !strings.Contains(body, "the health gate is not met") {
		t.Error("page does not carry the update system's own reason for offering nothing")
	}
}

// TestColonyUpdateEnablesNothingForAReadOnlyRole is the role case. A
// Viewer may read the page - every role may read the truth - and may
// click nothing at all.
func TestColonyUpdateEnablesNothingForAReadOnlyRole(t *testing.T) {
	combs := make([]clusterNodeView, 0, 4)
	for _, id := range updateTestCombs() {
		combs = append(combs, clusterNodeView{NodeID: id, Reachable: true})
	}
	rows := colonyUpdateCombRows(combs, idleNominatedState("drone.lab3.home.arpa"), false)
	for _, row := range rows {
		if row.CanUpdate {
			t.Errorf("Comb %s has an enabled control for a read-only session", row.NodeID)
		}
		if !strings.Contains(row.DisabledReason, "read-only") {
			t.Errorf("Comb %s's disabled reason does not mention the role: %q", row.NodeID, row.DisabledReason)
		}
	}
}

// TestColonyUpdateDisablesNominatedCombItCannotSee is the honesty case for
// the one client-side evidence check that exists. The page will not
// enable a control for a Comb whose managerd did not answer a moment ago,
// and it says so in those words rather than refusing without a reason.
// It is still only a courtesy - the backend's reading wins - and the test
// says as much by checking the wording.
func TestColonyUpdateDisablesNominatedCombItCannotSee(t *testing.T) {
	combs := []clusterNodeView{
		{NodeID: "brood.lab3.home.arpa", Reachable: true},
		{NodeID: "drone.lab3.home.arpa", Reachable: false, Error: "connection refused"},
	}
	rows := colonyUpdateCombRows(combs, idleNominatedState("drone.lab3.home.arpa"), true)

	var drone colonyUpdateCombView
	for _, row := range rows {
		if row.NodeID == "drone.lab3.home.arpa" {
			drone = row
		}
	}
	if drone.CanUpdate {
		t.Error("a control was enabled for a Comb this page has no evidence is reachable")
	}
	if !strings.Contains(drone.DisabledReason, "managerd did not answer") {
		t.Errorf("disabled reason does not name the evidence that is missing: %q", drone.DisabledReason)
	}
}

// ---------------------------------------------------------------------------
// The in-flight state
// ---------------------------------------------------------------------------

// inFlightState is one Comb mid-update, with three steps, two of them
// settled and the current one still moving.
func inFlightState(target string) colonyupdate.State {
	now := time.Now()
	return colonyupdate.State{
		Observed:           true,
		Phase:              colonyupdate.OutcomeInProgress,
		Detail:             "restarting managerd and raftd on " + target + ", one at a time",
		OperationID:        "op-2026-09-27-001",
		TargetNodeID:       target,
		RequestedBy:        "gjb",
		StartedAt:          now.Add(-40 * time.Second),
		NominatedNodeID:    "",
		NominatedDetail:    "a Comb is being updated, so the rest of the Colony is not on offer",
		SingleFlightHolder: target,
		BackendEnforced:    true,
		EnforcedDetail:     "the update system holds a colony-wide lease and refuses a second request",
		Steps: []colonyupdate.Step{
			{Name: "Health gate", Outcome: colonyupdate.OutcomeConfirmedComplete, Detail: "the Colony digest matched before this step", Evidence: "digest 9f2c... applied index 181", ObservedAt: now.Add(-45 * time.Second)},
			{Name: "Leadership step-aside", Outcome: colonyupdate.OutcomeConfirmedComplete, Detail: "not leader; no transfer was needed", Evidence: "raft_state Follower", ObservedAt: now.Add(-41 * time.Second)},
			{Name: "Restart managerd and raftd", Outcome: colonyupdate.OutcomeInProgress, Detail: "issued; awaiting the process to come back", ObservedAt: now.Add(-40 * time.Second)},
		},
	}
}

// TestColonyUpdateInFlightEnablesNothing checks the state ADR-0145 is
// actually about: while one Comb is being updated, every other Comb is
// visibly disabled, the target is visibly marked, and the page says the
// disabling is a courtesy rather than the rule.
func TestColonyUpdateInFlightEnablesNothing(t *testing.T) {
	body := renderColonyUpdate(t, newColonyUpdateTestServer(t, &fakeColonyUpdate{state: inFlightState("sting.lab3.home.arpa")}))

	if got := colonyUpdateEnabledControls(body); got != 0 {
		t.Errorf("page has %d enabled update controls while an update is in flight, want 0", got)
	}
	if !strings.Contains(body, `data-comb="sting.lab3.home.arpa" data-target="true"`) {
		t.Error("the Comb being updated is not marked as the target")
	}
	for _, want := range []string{
		"another Comb is being updated right now",
		"courtesy",
		"being updated now",
		"op-2026-09-27-001",
		"gjb",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("in-flight page is missing %q", want)
		}
	}
	// The in-flight Comb's own control must not be re-clickable: a
	// second update of the Comb currently being updated is exactly the
	// double-click ADR-0145's single-flight is there to refuse.
	if !strings.Contains(body, `data-comb-update="sting.lab3.home.arpa" disabled`) {
		t.Error("the Comb currently being updated has a clickable control")
	}
}

// TestColonyUpdateInFlightReportsTheHolder is the second-tab case. An
// operator watching a second tab must be able to see WHO is holding the
// update, so a refusal is comprehensible rather than a bare denial.
func TestColonyUpdateInFlightReportsTheHolder(t *testing.T) {
	body := renderColonyUpdate(t, newColonyUpdateTestServer(t, &fakeColonyUpdate{state: inFlightState("sting.lab3.home.arpa")}))

	if !strings.Contains(body, "held by <strong>sting.lab3.home.arpa</strong>") {
		t.Error("page does not name what is holding the colony-wide update")
	}
	if !strings.Contains(body, "by the server&#39;s decision, not this page&#39;s") && !strings.Contains(body, "by the server's decision, not this page's") {
		t.Errorf("page does not say the hold is the server's decision; got: %s", colonyUpdateExcerpt(body, "Single-flight"))
	}
}

// TestColonyUpdatePanelPollRendersTheSameView checks the htmx poll target
// and the full page agree. They render from the same function on purpose,
// and this is the test that keeps that true - a poll that drifted from
// the page would show an operator two different truths four seconds
// apart.
func TestColonyUpdatePanelPollRendersTheSameView(t *testing.T) {
	s := newColonyUpdateTestServer(t, &fakeColonyUpdate{state: inFlightState("sting.lab3.home.arpa")})

	page := renderColonyUpdate(t, s)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/colony-update/panel", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /colony-update/panel status = %d, want 200", rec.Code)
	}
	panel := rec.Body.String()

	if !strings.Contains(panel, "Restart managerd and raftd") || !strings.Contains(page, "Restart managerd and raftd") {
		t.Error("the step list does not appear on both the page and the poll fragment")
	}
	if got := colonyUpdateEnabledControls(panel); got != 0 {
		t.Errorf("the poll fragment has %d enabled controls while an update is in flight, want 0", got)
	}
	// The fragment is a fragment: it must not carry the document chrome,
	// or a swap would nest a whole page inside a panel.
	if strings.Contains(panel, "<html") || strings.Contains(panel, "</body>") {
		t.Error("the poll fragment contains a full document, which would corrupt the swap")
	}
}

// TestColonyUpdatePanelPollGetsHXRedirectOnExpiredSession is the HTMX-aware
// gate, required for anything this page adds that polls. A bare 302 on an
// XHR leaves the operator staring at a blank panel forever; HX-Redirect
// navigates the browser to the login page, which is the pattern
// redirectToLogin already implements for the rest of the app.
//
// The request carries no session cookie, which is exactly what a browser
// with an expired session sends, and the only difference from a live
// session is that the cookie is no longer valid.
func TestColonyUpdatePanelPollGetsHXRedirectOnExpiredSession(t *testing.T) {
	s := newTestServerWithAuth(t, &fakeClient{statusResp: updateTestStatus()}, "admin", "secret")
	s.SetColonyUpdateController(&fakeColonyUpdate{state: idleNominatedState("brood.lab3.home.arpa")})

	req := httptest.NewRequest(http.MethodGet, "/colony-update/panel", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if got := rec.Header().Get("HX-Redirect"); !strings.HasPrefix(got, "/login") {
		t.Errorf("HX-Redirect = %q, want a /login prefix; the panel poll would silently stall without it", got)
	}
}

// ---------------------------------------------------------------------------
// The six outcomes
// ---------------------------------------------------------------------------

// allOutcomes is every Outcome this package can produce, in one place, so
// the "each renders distinctly" test below cannot quietly miss one.
func allOutcomes() []colonyupdate.Outcome {
	return []colonyupdate.Outcome{
		colonyupdate.OutcomeInProgress,
		colonyupdate.OutcomeConfirmedComplete,
		colonyupdate.OutcomeBlocked,
		colonyupdate.OutcomeFailed,
		colonyupdate.OutcomeUnknown,
		colonyupdate.OutcomeUnobserved,
	}
}

// TestColonyUpdateOutcomesAreDistinctByLabelAndClass is the structural
// half of the outcome requirement: six outcomes, six different words, six
// different badge classes. It is checked directly against the mapping
// rather than against rendered HTML, because a collision in the mapping
// would be the cause of any collision in the page.
func TestColonyUpdateOutcomesAreDistinctByLabelAndClass(t *testing.T) {
	labels := map[string]colonyupdate.Outcome{}
	classes := map[string]colonyupdate.Outcome{}
	for _, outcome := range allOutcomes() {
		label := colonyUpdateOutcomeLabel(outcome)
		class := colonyUpdateOutcomeClass(outcome)
		if previous, clash := labels[label]; clash {
			t.Errorf("outcomes %q and %q share the label %q; they would render identically", previous, outcome, label)
		}
		labels[label] = outcome
		if previous, clash := classes[class]; clash {
			t.Errorf("outcomes %q and %q share the badge class %q; they would be indistinguishable by colour", previous, outcome, class)
		}
		classes[class] = outcome
	}

	// The two inconclusive outcomes must not be the success colour, and
	// must not be the failure colour. This is the check the whole
	// requirement exists for.
	for _, outcome := range []colonyupdate.Outcome{colonyupdate.OutcomeUnknown, colonyupdate.OutcomeUnobserved} {
		class := colonyUpdateOutcomeClass(outcome)
		if class == "ready" {
			t.Errorf("inconclusive outcome %q renders as ready, i.e. as success", outcome)
		}
		if class == "error" {
			t.Errorf("inconclusive outcome %q renders as error, i.e. as failure", outcome)
		}
		if !outcome.IsInconclusive() {
			t.Errorf("outcome %q is not reported as inconclusive by its own predicate", outcome)
		}
		if outcome.IsSuccess() {
			t.Errorf("inconclusive outcome %q reports itself as a success", outcome)
		}
	}
	// Blocked and failed are both "not fine" but are different facts:
	// one is a refusal, the other is positive evidence of breakage. They
	// must not share a rendering.
	if colonyUpdateOutcomeClass(colonyupdate.OutcomeBlocked) == colonyUpdateOutcomeClass(colonyupdate.OutcomeFailed) {
		t.Error("blocked and failed render identically; nothing was attempted in one of them")
	}
	// Only the confirmed outcome may claim success.
	for _, outcome := range allOutcomes() {
		if outcome.IsSuccess() != (outcome == colonyupdate.OutcomeConfirmedComplete) {
			t.Errorf("outcome %q disagrees with the rule that only confirmed-complete is a success", outcome)
		}
	}
}

// TestColonyUpdateEachOutcomeRendersInThePage is the render half: each of
// the six outcomes, used as a step outcome, reaches the page with its own
// words and its own data-outcome attribute, and no outcome is ever
// rendered as another.
//
// Each case renders a whole page with a single step carrying that
// outcome, so this exercises the real template rather than the mapping in
// isolation.
func TestColonyUpdateEachOutcomeRendersInThePage(t *testing.T) {
	now := time.Now()
	for _, outcome := range allOutcomes() {
		t.Run(string(outcome), func(t *testing.T) {
			state := inFlightState("sting.lab3.home.arpa")
			state.Steps = []colonyupdate.Step{{
				Name:       "restart",
				Outcome:    outcome,
				Detail:     "step detail for " + string(outcome),
				Evidence:   "evidence for " + string(outcome),
				ObservedAt: now,
			}}
			body := renderColonyUpdate(t, newColonyUpdateTestServer(t, &fakeColonyUpdate{state: state}))

			want := colonyUpdateOutcomeLabel(outcome)
			if !strings.Contains(body, ">"+want+"</span>") {
				t.Errorf("page does not render the outcome label %q; got: %s", want, colonyUpdateExcerpt(body, "update-step-head"))
			}
			if !strings.Contains(body, `data-step-outcome="`+string(outcome)+`"`) {
				t.Errorf("page does not carry data-step-outcome=%q, so the exact vocabulary is not verifiable in the markup", outcome)
			}
			// And no OTHER outcome's badge may be present, which is the
			// "renders distinctly" claim stated as an exclusion rather
			// than an inclusion: with one step on the page, a second
			// outcome's badge can only mean the two are rendering alike.
			for _, other := range allOutcomes() {
				if other == outcome {
					continue
				}
				if stepCountFor(body, other) != 0 {
					t.Errorf("outcome %q also rendered a badge for %q; they are not distinct",
						outcome, other)
				}
			}
		})
	}
}

// TestColonyUpdateInconclusiveStepsAreNotRenderedAsSuccessOrFailure is the
// requirement stated as a direct assertion: for each of the two
// inconclusive outcomes, the rendered step badge must not be the success
// badge, must not be the error badge, and must carry a distinct word.
func TestColonyUpdateInconclusiveStepsAreNotRenderedAsSuccessOrFailure(t *testing.T) {
	for _, outcome := range []colonyupdate.Outcome{colonyupdate.OutcomeUnknown, colonyupdate.OutcomeUnobserved} {
		t.Run(string(outcome), func(t *testing.T) {
			state := inFlightState("sting.lab3.home.arpa")
			state.Steps = []colonyupdate.Step{{Name: "restart", Outcome: outcome, Detail: "no usable answer"}}
			body := renderColonyUpdate(t, newColonyUpdateTestServer(t, &fakeColonyUpdate{state: state}))

			badge := stepBadgeTag(body, outcome)
			if badge == "" {
				t.Fatalf("no step badge rendered for %q; got: %s", outcome, colonyUpdateExcerpt(body, "update-step-head"))
			}
			if strings.Contains(badge, `class="badge ready"`) {
				t.Errorf("inconclusive outcome %q rendered as ready: %s", outcome, badge)
			}
			if strings.Contains(badge, `class="badge error"`) {
				t.Errorf("inconclusive outcome %q rendered as error: %s", outcome, badge)
			}
			if !strings.Contains(badge, colonyUpdateOutcomeLabel(outcome)) {
				t.Errorf("inconclusive outcome %q did not name itself: %s", outcome, badge)
			}
			// The whole page must not claim the update succeeded.
			if strings.Contains(body, colonyUpdateOutcomeLabel(colonyupdate.OutcomeConfirmedComplete)) {
				t.Errorf("page claims a confirmed complete update while a step is %q", outcome)
			}
		})
	}
}

// TestColonyUpdateUnknownOutcomeValueRendersAsUnobserved is the
// forward-compatibility case. A newer update system may report an outcome
// this build has never heard of, and ADR-0056's rule applies unchanged: an
// unrecognised observation is never read as a healthy one. It must render
// as the unobserved bucket, not as success and not as a blank badge.
func TestColonyUpdateUnknownOutcomeValueRendersAsUnobserved(t *testing.T) {
	state := inFlightState("sting.lab3.home.arpa")
	state.Steps = []colonyupdate.Step{{Name: "a step from the future", Outcome: colonyupdate.Outcome("quantum-superposition"), Detail: "this build has never heard of this outcome"}}
	body := renderColonyUpdate(t, newColonyUpdateTestServer(t, &fakeColonyUpdate{state: state}))

	if !strings.Contains(body, colonyUpdateOutcomeLabel(colonyupdate.OutcomeUnobserved)) {
		t.Errorf("an unrecognised outcome did not render as unobserved: %s", colonyUpdateExcerpt(body, "update-step-head"))
	}
	if !strings.Contains(body, "this build has never heard of this outcome") {
		t.Error("the unrecognised step's own detail was dropped")
	}
	if strings.Contains(body, colonyUpdateOutcomeLabel(colonyupdate.OutcomeConfirmedComplete)) {
		t.Error("an unrecognised outcome was rendered as confirmed complete")
	}
}

// ---------------------------------------------------------------------------
// Backend refusal
// ---------------------------------------------------------------------------

// TestColonyUpdateRendersBackendRefusal is the case the whole ADR hinges
// on. A second tab, a curl, or a stale page that forces through the
// disabled control must be refused by the backend, and this page must
// render that refusal as a refusal - in the backend's own words, with the
// holder named, and with the operator's own request echoed back as their
// request rather than as a target.
func TestColonyUpdateRendersBackendRefusal(t *testing.T) {
	fake := &fakeColonyUpdate{
		state:      inFlightState("sting.lab3.home.arpa"),
		requestErr: colonyupdate.Refusal("sting.lab3.home.arpa", "an update of sting.lab3.home.arpa is already in progress"),
	}
	rec := postColonyUpdate(t, newColonyUpdateTestServer(t, fake), "brood.lab3.home.arpa")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200 (a refusal is re-rendered, not an error page); body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{
		"refused by the server, not by this page",
		"an update of sting.lab3.home.arpa is already in progress",
		"Already holding the Colony-wide update",
		"sting.lab3.home.arpa",
		"You asked to update <strong>brood.lab3.home.arpa</strong>",
		"it is not a target and nothing on this page will treat it as one",
		"would not have prevented it",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal page is missing %q; got: %s", want, colonyUpdateExcerpt(body, "refused by the server"))
		}
	}
}

// TestColonyUpdateRefusalIsNotRenderedAsAnErrorOrSuccess separates the
// three outcomes an action can have, which are genuinely three different
// things: refused (the server said no), failed to deliver (the request
// never got there), and accepted. The first two must be visibly
// different messages and neither may claim the update started.
func TestColonyUpdateRefusalIsNotRenderedAsAnErrorOrSuccess(t *testing.T) {
	t.Run("refusal", func(t *testing.T) {
		fake := &fakeColonyUpdate{
			state:      inFlightState("sting.lab3.home.arpa"),
			requestErr: colonyupdate.Refusal("sting.lab3.home.arpa", "already held"),
		}
		body := postColonyUpdate(t, newColonyUpdateTestServer(t, fake), "brood.lab3.home.arpa").Body.String()
		if strings.Contains(body, "could not be delivered") {
			t.Error("a refusal was rendered as a delivery failure; they are different things")
		}
	})

	t.Run("delivery failure", func(t *testing.T) {
		fake := &fakeColonyUpdate{
			state:      inFlightState("sting.lab3.home.arpa"),
			requestErr: errors.New("connection reset by peer"),
		}
		body := postColonyUpdate(t, newColonyUpdateTestServer(t, fake), "brood.lab3.home.arpa").Body.String()
		if !strings.Contains(body, "could not be delivered") {
			t.Error("a delivery failure was not rendered as a delivery failure")
		}
		if strings.Contains(body, "refused by the server, not by this page") {
			t.Error("a delivery failure was rendered as a backend refusal; the server never saw the request")
		}
	})
}

// TestColonyUpdateInertBackendRequestIsRefused covers the actual state of
// this branch. With no adapter wired, the page's own POST reaches
// colonyupdate.Inert, which refuses, and the page must say the request
// could not be recorded and that nothing was attempted.
func TestColonyUpdateInertBackendRequestIsRefused(t *testing.T) {
	// No SetColonyUpdateController call at all: this is how the binary
	// actually builds today.
	s := newTestServer(t, &fakeClient{statusResp: updateTestStatus()})
	rec := postColonyUpdate(t, s, "brood.lab3.home.arpa")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no Colony update system is attached to this frontend") {
		t.Errorf("the inert backend's refusal is not rendered: %s", colonyUpdateExcerpt(body, "Colony update"))
	}
	if !strings.Contains(body, "Nothing was attempted against any Comb") {
		t.Error("the page does not say that nothing was attempted")
	}
}

// ---------------------------------------------------------------------------
// A stale or forged client cannot make the page lie
// ---------------------------------------------------------------------------

// TestColonyUpdateForgedRequestCannotClaimSuccess is the most important
// test in this file.
//
// The fake's post-request state says the operation is confirmed complete
// on brood, while the operator's form claimed a different Comb entirely
// (a stale page) and the page is then rendered from a FRESH read. What
// must be true of the resulting markup:
//
//   - the target named is the one the SERVER said, not the one the
//     browser asked for;
//   - the requested Comb is shown as the operator's request, and is
//     explicitly not shown as a target;
//   - the enable/disable state comes from the fresh read, not from
//     anything the browser did.
//
// A page that trusted its own form here would show "confirmed complete"
// for a Comb the server never mentioned, which is the exact failure the
// ADR's "UI state is advisory" clause exists to prevent.
func TestColonyUpdateForgedRequestCannotClaimSuccess(t *testing.T) {
	completed := colonyupdate.State{
		Observed:        true,
		Phase:           colonyupdate.OutcomeConfirmedComplete,
		Detail:          "managerd and raftd on brood are running the new build",
		OperationID:     "op-2026-09-27-002",
		TargetNodeID:    "brood.lab3.home.arpa",
		RequestedBy:     "gjb",
		StartedAt:       time.Now().Add(-3 * time.Minute),
		NominatedNodeID: "drone.lab3.home.arpa",
		NominatedDetail: "the system re-derived this Comb after the previous update confirmed",
		BackendEnforced: true,
		EnforcedDetail:  "the update system refuses a second request while one is held",
		Steps: []colonyupdate.Step{{
			Name:       "build identity",
			Outcome:    colonyupdate.OutcomeConfirmedComplete,
			Detail:     "running build matches the build on disk",
			Evidence:   "running 73b57d5 / on disk 73b57d5",
			ObservedAt: time.Now(),
		}},
	}
	fake := &fakeColonyUpdate{state: completed}

	// A stale page: the browser submits a Comb that has nothing to do with
	// what the server last said.
	rec := postColonyUpdate(t, newColonyUpdateTestServer(t, fake), "sting.lab3.home.arpa")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, `data-comb="brood.lab3.home.arpa" data-target="true"`) {
		t.Error("the page does not mark the Comb the SERVER named as the target")
	}
	if strings.Contains(body, `data-comb="sting.lab3.home.arpa" data-target="true"`) {
		t.Error("the page marked the Comb the BROWSER asked for as the target; that is the forged-view failure")
	}
	// The whole page carries exactly one data-target="true", and it is
	// brood's.
	if got := strings.Count(body, `data-target="true"`); got != 1 {
		t.Errorf("page marks %d Combs as the update target, want exactly 1", got)
	}
	// The stale page's Comb is enabled on neither the server's nor its own
	// terms; the server says drone is on offer, so drone is the one
	// enabled control and the success above did not enable anything.
	if got := colonyUpdateEnabledControls(body); got != 1 {
		t.Errorf("page has %d enabled controls after a completed update, want 1 (the Comb the server is now offering)", got)
	}
	// And the requested Comb is explicitly framed as the operator's input.
	if !strings.Contains(body, "it is not a target and nothing on this page will treat it as one") {
		t.Error("the operator's own request is not framed as an input rather than a target")
	}
}

// TestColonyUpdateStalePageCannotEnableAControlTheServerDidNot is the
// other half of the same property, at the GET level. A page fetched while
// one Comb was on offer, then reloaded after the system changed its mind,
// must show the new answer. There is no client-side state to preserve, so
// the test is simply that a changed backend state changes the markup.
func TestColonyUpdateStalePageCannotEnableAControlTheServerDidNot(t *testing.T) {
	fake := &fakeColonyUpdate{state: idleNominatedState("brood.lab3.home.arpa")}
	s := newColonyUpdateTestServer(t, fake)

	first := renderColonyUpdate(t, s)
	if got := colonyUpdateEnabledControls(first); got != 1 {
		t.Fatalf("first render has %d enabled controls, want 1", got)
	}
	if strings.Contains(first, `data-comb-update="brood.lab3.home.arpa" disabled`) {
		t.Error("the Comb the system IS offering was disabled on the first render")
	}
	if !strings.Contains(first, `data-comb-update="sting.lab3.home.arpa" disabled`) {
		t.Error("a Comb the system is NOT offering was not disabled on the first render")
	}

	// The system re-derives its answer - the leader moved, the health gate
	// changed, anything at all. Nothing on the page is carried over.
	fake.mu.Lock()
	fake.state = idleNominatedState("sting.lab3.home.arpa")
	fake.mu.Unlock()

	second := renderColonyUpdate(t, s)
	if got := colonyUpdateEnabledControls(second); got != 1 {
		t.Fatalf("second render has %d enabled controls, want 1", got)
	}
	if strings.Contains(second, `data-comb-update="sting.lab3.home.arpa" disabled`) {
		t.Error("the newly offered Comb is still disabled after the re-read")
	}
	for _, control := range colonyUpdateControls(second) {
		if updateControlNodeID(control) == "brood.lab3.home.arpa" && !strings.Contains(control, " disabled") {
			t.Error("the previously offered Comb is still enabled after a re-read; the page is carrying stale state")
		}
	}
	if !strings.Contains(second, "the update system is offering sting.lab3.home.arpa right now") {
		t.Error("the re-render does not name the Comb now on offer")
	}
	if fake.readCount() < 2 {
		t.Errorf("the page read the update system %d times across two renders, want at least 2: each render must re-read", fake.readCount())
	}
}

// TestColonyUpdateForgedNodeIDIsRefusedBeforeTheBackendIsAsked is the
// membership gate. A POST naming a Comb that was never a member must not
// reach the update system at all: the frontend's own credentials must not
// be pointed at a caller-supplied name, which is the same reasoning
// knownColonyMember already documents for handleHostPage.
func TestColonyUpdateForgedNodeIDIsRefusedBeforeTheBackendIsAsked(t *testing.T) {
	fake := &fakeColonyUpdate{state: idleNominatedState("brood.lab3.home.arpa")}
	rec := postColonyUpdate(t, newColonyUpdateTestServer(t, fake), "attacker.example.com")

	if rec.Code != http.StatusNotFound {
		t.Errorf("POST with a non-member node_id status = %d, want 404", rec.Code)
	}
	if got := fake.recorded(); len(got) != 0 {
		t.Errorf("the update system was asked to update a non-member: %+v", got)
	}
}

// TestColonyUpdatePostDoesNotGateOnTheRenderedNomination is the most
// important design test in this file, and the one that would be easiest to
// break by accident.
//
// The operator clicks Update on a Comb this page currently shows as
// disabled. The POST must STILL reach the backend. It must not consult
// the page's own .CanUpdate booleans, because doing so would make the
// greyed-out control the enforcement mechanism - precisely the mistake
// ADR-0145 rejects. The server is the thing that refuses, and it is only
// honest about that if it is actually given the chance.
func TestColonyUpdatePostDoesNotGateOnTheRenderedNomination(t *testing.T) {
	fake := &fakeColonyUpdate{state: idleNominatedState("brood.lab3.home.arpa")}
	s := newColonyUpdateTestServer(t, fake)

	// On the rendered page, sting is disabled: the system is offering
	// brood. The operator (or a curl) submits sting anyway.
	body := renderColonyUpdate(t, s)
	if !strings.Contains(body, `data-comb-update="sting.lab3.home.arpa" disabled`) {
		t.Fatal("sting is not disabled on the page, so this test is not exercising a forced click")
	}

	rec := postColonyUpdate(t, s, "sting.lab3.home.arpa")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200; the frontend must not refuse on the page's behalf", rec.Code)
	}

	recorded := fake.recorded()
	if len(recorded) != 1 {
		t.Fatalf("the update system received %d requests, want exactly 1: the frontend must forward the intent and let the server decide", len(recorded))
	}
	if recorded[0].NodeID != "sting.lab3.home.arpa" {
		t.Errorf("the update system was asked about %q, want the Comb the client actually submitted", recorded[0].NodeID)
	}
}

// ---------------------------------------------------------------------------
// The inert page, and what it must not claim
// ---------------------------------------------------------------------------

// TestColonyUpdateInertPageSaysItIsInert is the state of this branch, and
// it is the single most important thing for a later reader of a live UI to
// be able to see. With nothing wired, the page must state that no update
// system is attached, must enable nothing, and must not describe itself as
// enforcing anything.
func TestColonyUpdateInertPageSaysItIsInert(t *testing.T) {
	// newTestServer without SetColonyUpdateController: exactly how the
	// binary is built today.
	body := renderColonyUpdate(t, newTestServer(t, &fakeClient{statusResp: updateTestStatus()}))

	for _, want := range []string{
		"No Colony update system is attached",
		"nothing here is enforced and nothing here can start an update",
		"Unobserved",
		`data-backend-enforced="false"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the inert page is missing %q", want)
		}
	}
	// The headline for an UNOBSERVED system is the "not attached" one.
	// The "does not report enforcement" headline belongs to a system that
	// answered but disclaimed enforcement, which the inert implementation
	// never is - so its absence here is correct, and TestColonyUpdate
	// UnenforcedBackendSaysSo covers the other branch.
	if strings.Contains(body, "assume nothing is stopping a concurrent request") {
		t.Error("the inert page used the 'system answered but disclaims enforcement' headline; nothing answered at all")
	}
	if got := colonyUpdateEnabledControls(body); got != 0 {
		t.Errorf("the inert page has %d enabled update controls, want 0: it cannot start an update, so it must offer none", got)
	}
	if strings.Contains(body, colonyUpdateOutcomeLabel(colonyupdate.OutcomeConfirmedComplete)) {
		t.Error("the inert page claims a confirmed complete update")
	}
	// Every Comb is still listed, even on an inert page. "There is no
	// update system" is not a reason to hide the Colony.
	if got := strings.Count(body, `data-comb="`); got != len(updateTestCombs()) {
		t.Errorf("the inert page listed %d Combs, want %d", got, len(updateTestCombs()))
	}
}

// TestColonyUpdateInertBackendReportsNoEnforcement checks the inert
// implementation itself rather than only the page built on it. If a future
// change made Inert claim enforcement, the page would start telling
// operators that a single-flight guard exists when none does - the most
// dangerous single line on this page.
func TestColonyUpdateInertBackendReportsNoEnforcement(t *testing.T) {
	state, err := colonyupdate.Inert{}.ColonyUpdateState(context.Background())
	if err != nil {
		t.Fatalf("Inert.ColonyUpdateState() error: %v", err)
	}
	if state.Observed {
		t.Error("Inert reports an observed state; there is nothing to observe")
	}
	if state.BackendEnforced {
		t.Error("Inert claims to enforce colony-wide single-flight; nothing enforces it")
	}
	if state.NominatedNodeID != "" {
		t.Errorf("Inert nominates %q; it has no basis on which to choose a Comb", state.NominatedNodeID)
	}
	if state.Phase != colonyupdate.OutcomeUnobserved {
		t.Errorf("Inert reports phase %q, want %q", state.Phase, colonyupdate.OutcomeUnobserved)
	}
	if state.UnavailableReason == "" {
		t.Error("Inert gives no reason for being unavailable")
	}

	if _, err := (colonyupdate.Inert{}).RequestColonyUpdate(context.Background(), colonyupdate.Request{NodeID: "brood.lab3.home.arpa"}); !errors.Is(err, colonyupdate.ErrNoImplementation) {
		t.Errorf("Inert.RequestColonyUpdate() error = %v, want ErrNoImplementation", err)
	}
}

// TestColonyUpdateUnenforcedBackendSaysSo covers the third headline
// branch: a system that answered, is offering a Comb, and explicitly does
// NOT claim to enforce colony-wide single-flight. The page must say that
// plainly. Hard-coding "the server enforces this" regardless of what the
// system reported would be the single most dangerous line on this page,
// because it is the one claim an operator would rely on.
func TestColonyUpdateUnenforcedBackendSaysSo(t *testing.T) {
	state := idleNominatedState("brood.lab3.home.arpa")
	state.BackendEnforced = false
	state.EnforcedDetail = "this build has no colony-wide lease; nothing is holding the update"

	body := renderColonyUpdate(t, newColonyUpdateTestServer(t, &fakeColonyUpdate{state: state}))

	if !strings.Contains(body, "assume nothing is stopping a concurrent request") {
		t.Errorf("the page does not say enforcement is absent: %s", colonyUpdateExcerpt(body, "enforced-headline"))
	}
	if strings.Contains(body, "is enforced by the server, independently of this page") {
		t.Error("the page claims the server enforces single-flight when the system said it does not")
	}
	if !strings.Contains(body, "this build has no colony-wide lease") {
		t.Error("the page drops the system's own explanation of what enforces it")
	}
	if !strings.Contains(body, `data-backend-enforced="false"`) {
		t.Error("the page does not carry the enforcement flag the headline was chosen from")
	}
}

// TestColonyUpdateReadFailureRendersAsUnobservedNotAsIdle is the
// distinction that a nil error would quietly destroy. A read that FAILED
// must not render as "no update is running", which is a confident
// statement the page has no evidence for.
func TestColonyUpdateReadFailureRendersAsUnobservedNotAsIdle(t *testing.T) {
	fake := &fakeColonyUpdate{stateErr: errors.New("dial tcp 10.90.0.94:17700: connect: connection refused")}
	body := renderColonyUpdate(t, newColonyUpdateTestServer(t, fake))

	if !strings.Contains(body, "Unobserved") {
		t.Error("a failed read does not render as unobserved")
	}
	if !strings.Contains(body, "the Colony update system could not be read") {
		t.Error("a failed read does not say the system could not be read")
	}
	if !strings.Contains(body, "not evidence that the Colony is fine") {
		t.Error("the page does not say that a failed read is not evidence of health")
	}
	if got := colonyUpdateEnabledControls(body); got != 0 {
		t.Errorf("a failed read produced %d enabled controls, want 0", got)
	}
	if strings.Contains(body, "no update is in progress") {
		t.Error("a failed read was rendered as an idle colony")
	}
}

// TestColonyUpdateContradictoryStateEnablesNothing covers a state the
// update system could genuinely produce: in progress, but naming no
// target. The page must report the contradiction and enable nothing,
// rather than resolving it either way - enabling everything because
// there is nothing to be in flight on, and disabling everything because
// something is, are both guesses.
func TestColonyUpdateContradictoryStateEnablesNothing(t *testing.T) {
	state := inFlightState("sting.lab3.home.arpa")
	state.TargetNodeID = ""
	state.SingleFlightHolder = ""
	body := renderColonyUpdate(t, newColonyUpdateTestServer(t, &fakeColonyUpdate{state: state}))

	if !strings.Contains(body, "reading is inconsistent") {
		t.Errorf("the page does not report the contradiction: %s", colonyUpdateExcerpt(body, "inconsistent"))
	}
	if got := colonyUpdateEnabledControls(body); got != 0 {
		t.Errorf("a contradictory reading produced %d enabled controls, want 0", got)
	}
	if got := strings.Count(body, `data-target="true"`); got != 0 {
		t.Errorf("a contradictory reading marks %d Combs as the target, want 0", got)
	}
}

// TestColonyUpdateStaleNominationForADepartedCombEnablesNothing checks
// the one place this page second-guesses the update system. A nomination
// naming a Comb that is no longer a current member is dropped, because a
// control pointing at a departed Comb is worse than no control. The
// page says it is doing so rather than silently showing nothing.
func TestColonyUpdateStaleNominationForADepartedCombEnablesNothing(t *testing.T) {
	body := renderColonyUpdate(t, newColonyUpdateTestServer(t, &fakeColonyUpdate{state: idleNominatedState("ghost.lab3.home.arpa")}))

	if got := colonyUpdateEnabledControls(body); got != 0 {
		t.Errorf("a nomination for a non-member produced %d enabled controls, want 0", got)
	}
	if !strings.Contains(body, "is not a current member of this Colony") {
		t.Error("the page does not say why the nomination was dropped")
	}
}

// TestColonyUpdateMembershipFailureRendersNoCombs is the honesty case for
// an empty list. A page that claims to list every Comb while listing none
// is the one thing it must never do, so the error is shown and the empty
// row says in words that this is not a complete list.
func TestColonyUpdateMembershipFailureRendersNoCombs(t *testing.T) {
	s := newTestServer(t, &fakeClient{statusErr: errors.New("raftd is not reachable over its internal socket")})
	s.SetColonyUpdateController(&fakeColonyUpdate{state: idleNominatedState("brood.lab3.home.arpa")})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/colony-update", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "could not read current Colony membership") {
		t.Error("page does not report that membership could not be read")
	}
	if got := strings.Count(body, `data-comb="`); got != 0 {
		t.Errorf("page listed %d Combs when membership was unreadable, want 0", got)
	}
	if !strings.Contains(body, "must not be read as though it were") {
		t.Error("the empty state does not warn that this is not a complete list")
	}
}

// ---------------------------------------------------------------------------
// Roles
// ---------------------------------------------------------------------------

// TestColonyUpdatePostRequiresAdmin. Starting a Colony update restarts
// managerd and raftd, which is at least as strong as POST
// /machine/services/{name}/restart - so it is Admin, matching that
// route. The page itself stays readable by every role, because every role
// may read the truth about what the Colony is doing.
func TestColonyUpdatePostRequiresAdmin(t *testing.T) {
	viewer := map[string]manager.Role{"gjb": manager.RoleViewer}
	operator := map[string]manager.Role{"gjb": manager.RoleOperator}
	admin := map[string]manager.Role{"gjb": manager.RoleAdmin}

	for _, tc := range []struct {
		name     string
		roleMap  map[string]manager.Role
		wantCode int
	}{
		{name: "viewer", roleMap: viewer, wantCode: http.StatusForbidden},
		{name: "operator", roleMap: operator, wantCode: http.StatusForbidden},
		{name: "admin", roleMap: admin, wantCode: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewServer(&fakeClient{statusResp: updateTestStatus()}, fakeAuthenticator{user: "gjb", pass: "pw"},
				tc.roleMap, nil, "", "", nil, false)
			if err != nil {
				t.Fatalf("NewServer() error: %v", err)
			}
			fake := &fakeColonyUpdate{state: idleNominatedState("brood.lab3.home.arpa")}
			s.SetColonyUpdateController(fake)

			// Log in so a session exists and the role, not the missing
			// session, is what is being tested.
			cookies := loginAs(t, s, "gjb", "pw")

			rec := postColonyUpdateWith(t, s, "brood.lab3.home.arpa", cookies)

			if rec.Code != tc.wantCode {
				t.Errorf("POST as %s status = %d, want %d; body=%s", tc.name, rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode != http.StatusOK && len(fake.recorded()) != 0 {
				t.Errorf("a %s reached the update system %d times, want 0", tc.name, len(fake.recorded()))
			}
			if tc.wantCode == http.StatusOK && len(fake.recorded()) != 1 {
				t.Errorf("an Admin's request reached the update system %d times, want exactly 1", len(fake.recorded()))
			}
			// And the username is the one the backend records, so an
			// operation's "requested by" is real rather than "unknown".
			if tc.wantCode == http.StatusOK && fake.recorded()[0].RequestedBy != "gjb" {
				t.Errorf("the backend recorded RequestedBy = %q, want %q", fake.recorded()[0].RequestedBy, "gjb")
			}
		})
	}
}

// TestColonyUpdateReadOnlyRoleSeesThePage checks the other half of that
// decision: every role may READ the update page, including a Viewer who
// can change nothing. A role that cannot act still needs to see whether an
// update is in progress.
func TestColonyUpdateReadOnlyRoleSeesThePage(t *testing.T) {
	s, err := NewServer(&fakeClient{statusResp: updateTestStatus()}, fakeAuthenticator{user: "gjb", pass: "pw"},
		map[string]manager.Role{"gjb": manager.RoleViewer}, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	s.SetColonyUpdateController(&fakeColonyUpdate{state: inFlightState("sting.lab3.home.arpa")})

	cookies := loginAs(t, s, "gjb", "pw")

	req := httptest.NewRequest(http.MethodGet, "/colony-update", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET as viewer status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "sting.lab3.home.arpa") {
		t.Error("a Viewer cannot see which Comb is being updated")
	}
	if !strings.Contains(body, "Your role does not permit starting a Colony update") {
		t.Error("a Viewer is not told why every control is disabled")
	}
	if got := colonyUpdateEnabledControls(body); got != 0 {
		t.Errorf("a Viewer has %d enabled controls, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Navigation
// ---------------------------------------------------------------------------

// TestColonyUpdateIsLinkedFromTheSidebar. A page nobody can reach is not
// delivered, and this is the one line of markup added to the shared
// layout, so it is worth a test that the link exists and marks itself
// current.
func TestColonyUpdateIsLinkedFromTheSidebar(t *testing.T) {
	body := renderColonyUpdate(t, newColonyUpdateTestServer(t, &fakeColonyUpdate{state: idleNominatedState("brood.lab3.home.arpa")}))

	if !strings.Contains(body, `href="/colony-update" aria-current="page"`) {
		t.Error("the sidebar does not link the update page, or does not mark it current on that page")
	}
}

// TestColonyUpdatePageAddsNoClientSideScript is the project's standing
// constraint stated as a test. This page must stay server-rendered: the
// only client-side behaviour allowed is the vendored htmx, exercised
// through attributes. An inline <script> here would be a regression that
// no other test in this package would catch.
func TestColonyUpdatePageAddsNoClientSideScript(t *testing.T) {
	body := renderColonyUpdate(t, newColonyUpdateTestServer(t, &fakeColonyUpdate{state: inFlightState("sting.lab3.home.arpa")}))

	// The shared layout legitimately carries its own theme/sidebar
	// script. The page's own content sits between <main> and </main>, so
	// that is the region this test constrains.
	start := strings.Index(body, "<main")
	end := strings.Index(body, "</main>")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("could not locate the page's <main> region")
	}
	main := body[start:end]
	if strings.Contains(main, "<script") {
		t.Errorf("the update page adds a <script> inside its content; this project stays server-rendered. Found: %s", colonyUpdateExcerpt(main, "<script"))
	}
	if !strings.Contains(main, "hx-get=\"/colony-update/panel\"") {
		t.Error("the live panel does not use the vendored htmx poll idiom")
	}
	// And no inline event handlers, which are the other way a script gets
	// in without a <script> tag.
	for _, handler := range []string{"onclick=", "onsubmit=", "onload="} {
		if strings.Contains(main, handler) {
			t.Errorf("the update page uses the inline handler %q; this project stays server-rendered", handler)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// updateControlNodeID extracts the Comb from one rendered control's
// opening tag, so a test can tell which row a control belongs to.
func updateControlNodeID(control string) string {
	attribute := colonyUpdateControlAttribute + `"`
	found := strings.Index(control, attribute)
	if found < 0 {
		return ""
	}
	rest := control[found+len(attribute):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// stepBadgeTag returns the rendered step badge element for one outcome.
//
// It keys on data-step-outcome, which only the per-step badge carries -
// the colony phase badge uses data-outcome, and the step's <li> carries
// data-outcome too, so keying on the wrong one would match a wrapper
// rather than the badge whose colour and wording are under test.
func stepBadgeTag(body string, outcome colonyupdate.Outcome) string {
	needle := `data-step-outcome="` + string(outcome) + `">`
	found := strings.Index(body, needle)
	if found < 0 {
		return ""
	}
	start := strings.LastIndexByte(body[:found], '<')
	end := strings.Index(body[found:], "</span>")
	if start < 0 || end < 0 {
		return ""
	}
	return body[start : found+end+len("</span>")]
}

// stepCountFor counts how many times an outcome appears as a step badge.
// One step, one badge, so anything above 1 in a single-step render means
// two different outcomes are sharing a rendering.
func stepCountFor(body string, outcome colonyupdate.Outcome) int {
	return strings.Count(body, `data-step-outcome="`+string(outcome)+`"`)
}

// colonyUpdateExcerpt returns a short window of the rendered page around
// needle, so a failing assertion prints something an operator could read
// rather than 60KB of HTML. It is a test-only convenience and it never
// fails the test itself.
func colonyUpdateExcerpt(body, needle string) string {
	found := strings.Index(body, needle)
	if found < 0 {
		if len(body) > 400 {
			return "...(no " + needle + " in the page)..." + body[:400]
		}
		return fmt.Sprintf("...(no %s in the page)...", needle)
	}
	start := found - 200
	if start < 0 {
		start = 0
	}
	end := found + 300
	if end > len(body) {
		end = len(body)
	}
	return "..." + body[start:end] + "..."
}

// TestClusterOverviewTopologyListWrapperSurvives is a guard on a wrapper
// that has already been destroyed by accident once in this project.
//
// cluster_overview.html packs the entire Comb card - name, reachability,
// health, digest, leader, cause headline - onto ONE line inside the
// cockpit-topology-list <div>. An edit anywhere in that file can silently
// drop the wrapper's opening or closing tag, and the damage is subtle: the
// cards still render, they simply stop being laid out as a grid, so the
// page looks "a bit off" rather than broken and no existing test fails.
//
// This page deliberately does not touch cluster_overview.html at all, and
// this test is what keeps that decision honest rather than a claim: it
// asserts the wrapper is present, that it wraps the Comb cards, and that
// the number of topology cards equals the number of known Combs. If a
// later change to this feature ever does need to edit that file, this
// test is the thing that catches the wrapper going missing.
func TestClusterOverviewTopologyListWrapperSurvives(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId:    "brood.lab3.home.arpa",
		RaftReachable:    true,
		RaftState:        "Follower",
		RaftNodeId:       "brood.lab3.home.arpa",
		RaftLeaderId:     "brood.lab3.home.arpa",
		RaftAppliedIndex: 181,
		KnownNodeIds:     updateTestCombs(),
	}}
	rec := httptest.NewRecorder()
	newTestServer(t, client).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, `<div class="cockpit-topology-list">`) {
		t.Error("the cockpit-topology-list wrapper is missing from the command center; its Comb cards lost their grid layout")
	}
	if got := strings.Count(body, `class="cockpit-topology-node`); got != len(updateTestCombs()) {
		t.Errorf("rendered %d topology cards, want %d - one per known Comb", got, len(updateTestCombs()))
	}
	// And the panel head that carries the colony digest badge is still
	// inside the same section, so a refactor that moved one out of the
	// other would be caught here too.
	if !strings.Contains(body, `<section class="panel cockpit-topology">`) {
		t.Error("the topology panel section is missing")
	}
}
