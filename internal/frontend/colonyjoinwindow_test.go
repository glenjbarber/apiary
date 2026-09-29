package frontend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	manager "github.com/glenjbarber/apiary/internal/manager"
)

// errTestDialFailed stands in for a transport-level failure of the
// unauthenticated read, which is the case the view has to tell apart
// from the manager's own closed-window refusal.
var errTestDialFailed = errors.New("dial failed: connection refused")

// managerClosedRefusal is the manager's real closed-window refusal,
// obtained by asking the manager package rather than by copy-pasting the
// string. A test that paraphrased it would silently stop testing the
// closed path: IsColonyWindowClosedRefusal compares the whole string, so
// an invented sentence reads as a FAILED READ, which is a different and
// much less interesting branch.
var managerClosedRefusal = manager.ColonyWindowClosedRefusal()

// windowAt builds a live window response whose deadline is `in` from
// now, so tests do not have to reason about absolute epochs.
func windowAt(t *testing.T, in time.Duration) *rpcpb.ColonyJoinWindow {
	t.Helper()
	now := time.Now()
	return &rpcpb.ColonyJoinWindow{
		Enabled:       true,
		OpenedBy:      "comb-a",
		OpenedByKey:   "key-1",
		OpenedAtUnix:  now.Add(-time.Minute).Unix(),
		ExpiresAtUnix: now.Add(in).Unix(),
	}
}

func getMachine(t *testing.T, s *Server) string {
	t.Helper()
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/machine", nil))
	return rr.Body.String()
}

func postForm(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.ServeHTTP(rr, req)
	return rr
}

// TestJoinWindow_ClosedIsTheDefaultAndRendersAsClosed is the ordinary
// case, and the one that matters most: Part 4 INVERTS the default, so a
// closed window is what an operator sees almost always. The failure this
// guards is the page rendering a closed window as an error, or as an
// open one, because "no window came back" was read as a failure to read.
func TestJoinWindow_ClosedIsTheDefaultAndRendersAsClosed(t *testing.T) {
	// The real manager's exact refusal string, not a paraphrase: the
	// view tells "closed" from "failed to read" by asking the manager
	// package whether the string is its own, so a test using an
	// invented sentence would be testing the failed-read path.
	c := &fakeClient{getWindowResp: &rpcpb.GetColonyJoinWindowResponse{Error: managerClosedRefusal}}
	s := newTestServer(t, c)
	body := getMachine(t, s)

	if !strings.Contains(body, "Join window") {
		t.Fatalf("machine page has no join-window control; the panel is missing entirely")
	}
	if !strings.Contains(body, "Closed") {
		t.Errorf("closed window did not render as closed")
	}
	if strings.Contains(body, "Close now") {
		t.Errorf("page offered \"Close now\" for a window that is not open")
	}
	// The refusal must reach the operator: it names the next step, and a
	// control that silently reads "closed" is how an operator ends up not
	// knowing a window was already open elsewhere.
	if !strings.Contains(body, "not currently accepting") {
		t.Errorf("closed-window refusal not surfaced; body lacked the manager's own wording")
	}
}

// TestJoinWindow_OpenShowsDeadlineAndCloseButton covers the live case,
// and specifically that the deadline renders as an ABSOLUTE LOCAL TIME.
// "Closes in 14 minutes" is not comparable between the two screens an
// operator is looking at; an absolute time is.
func TestJoinWindow_OpenShowsDeadlineAndCloseButton(t *testing.T) {
	w := windowAt(t, 15*time.Minute)
	c := &fakeClient{getWindowResp: &rpcpb.GetColonyJoinWindowResponse{
		Window: w, ColonyNodeId: "colony-1", LeaderNodeId: "leader-1",
	}}
	s := newTestServer(t, c)
	body := getMachine(t, s)

	if !strings.Contains(body, "Close now") {
		t.Errorf("open window did not offer \"Close now\"")
	}
	rendered := deadlineLocal(w.GetExpiresAtUnix())
	if !strings.Contains(body, rendered) {
		t.Errorf("page did not render the absolute deadline %q; operator cannot compare it against the other screen", rendered)
	}
	if !strings.Contains(body, "comb-a") {
		t.Errorf("page did not name who opened the window")
	}
}

// TestJoinWindow_ExpiredIsNotLive pins the property that keeps the page
// honest about the gate: internal/manager's requireLiveColonyJoinWindow
// refuses an expired window at APPROVAL time, so a page still showing an
// expired window as open would advertise a join guaranteed to be refused.
func TestJoinWindow_ExpiredIsNotLive(t *testing.T) {
	w := windowAt(t, 15*time.Minute)
	w.ExpiresAtUnix = time.Now().Add(-time.Minute).Unix()
	c := &fakeClient{getWindowResp: &rpcpb.GetColonyJoinWindowResponse{Window: w}}
	s := newTestServer(t, c)
	body := getMachine(t, s)

	if strings.Contains(body, "Close now") {
		t.Errorf("an expired window rendered as open")
	}
	if !strings.Contains(body, "expired") {
		t.Errorf("expired window did not say so; operator would see a window the approval gate refuses")
	}
}

// TestJoinWindow_TransportErrorIsNotReportedAsClosed is the distinction
// the view type exists for. GetColonyJoinWindow's Error field also
// carries "no peer forwarding is configured", a dial failure, and a
// disallowed-target refusal - all DIFFERENT problems with DIFFERENT
// fixes from "the window is closed". Collapsing them sends an operator to
// open a window that already exists.
func TestJoinWindow_TransportErrorIsNotReportedAsClosed(t *testing.T) {
	c := &fakeClient{getWindowErr: errTestDialFailed}
	s := newTestServer(t, c)
	body := getMachine(t, s)

	if strings.Contains(body, "Open join window") {
		t.Errorf("a failed read offered \"Open join window\"; the control must not invite an act based on a read that failed")
	}
	if !strings.Contains(body, "dial failed") {
		t.Errorf("the transport failure was not surfaced; operator cannot tell a failed read from a closed window")
	}
	if !strings.Contains(body, "could not be read") {
		t.Errorf("failed read did not say it is not the same as closed; an operator would read the empty state as \"shut\"")
	}
}

// TestJoinWindow_ForwardingRefusalIsNotReadAsClosed covers the OTHER
// source of the same conflation, and it is the one a string comparison
// in the UI would get wrong: the manager returns its closed-window
// refusal AND its forwarding failures ("no peer forwarding is
// configured", "reaching <addr>: ...", disallowed target) in the SAME
// Error field. Telling them apart by asking the manager whether the
// string is its own refusal is what keeps "fix your network" from
// being rendered as "open the window".
func TestJoinWindow_ForwardingRefusalIsNotReadAsClosed(t *testing.T) {
	for _, refusal := range []string{
		"no peer forwarding is configured on this node; cannot reach the named Colony member",
		"reaching 10.62.0.2:17700: dial tcp: connection refused",
		"target address 10.62.0.2:17700 is not in this node's known_peer_addresses allowlist",
	} {
		c := &fakeClient{getWindowResp: &rpcpb.GetColonyJoinWindowResponse{Error: refusal}}
		s := newTestServer(t, c)
		body := getMachine(t, s)

		if strings.Contains(body, "Open join window") {
			t.Errorf("refusal %q was rendered as a closed window offering to open one", refusal)
		}
		if !strings.Contains(body, "could not be read") {
			t.Errorf("refusal %q did not render as a failed read", refusal)
		}
	}
}

// TestColonyAdvert_ReadFailureDoesNotTellOperatorToOpenTheWindow is the
// same rule on the requester's side, where the cost of getting it wrong
// is higher: an operator told "the Colony is closed, open the window"
// when the truth is "this Comb cannot reach that member" will ask an
// Admin to open a window that is already open, and the join still will
// not work.
func TestColonyAdvert_ReadFailureDoesNotTellOperatorToOpenTheWindow(t *testing.T) {
	c := &fakeClient{getWindowResp: &rpcpb.GetColonyJoinWindowResponse{
		Error: "reaching 10.62.0.2:17700: dial tcp: connection refused",
	}}
	s := newTestServer(t, c)

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/machine/colony-advert?colony_target_address=10.62.0.2", nil))
	body := rr.Body.String()

	if strings.Contains(body, "not an authorization failure") {
		t.Errorf("a dial failure was rendered as a closed Colony; operator would open a window that may already be open")
	}
	if !strings.Contains(body, "not the same as a closed Colony") {
		t.Errorf("a dial failure did not say it is not the same as closed")
	}
}

// TestJoinWindow_OpenRefusesNonPositiveDuration covers the one input the
// form takes. A typo in a duration must not be silently coerced to the
// configured default: the operator asked for something specific and would
// get something else with no indication.
func TestJoinWindow_OpenRefusesNonPositiveDuration(t *testing.T) {
	for _, bad := range []string{"0", "-60", "abc", "15m"} {
		c := &fakeClient{}
		s := newTestServer(t, c)

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/machine/join-window/open",
			strings.NewReader(url.Values{"duration_seconds": {bad}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.ServeHTTP(rr, req)

		if c.lastOpenWindowReq != nil {
			t.Errorf("duration %q: reached OpenColonyJoinWindow; a bad duration must be refused before the RPC", bad)
		}
		if !strings.Contains(rr.Body.String(), "greater than zero") {
			t.Errorf("duration %q: refusal did not explain the requirement", bad)
		}
	}
}

// TestJoinWindow_OpenBlankDurationUsesConfiguredDefault is the other
// half: blank is NOT an error, and it must send no duration at all
// rather than a zero or a guessed default. Sending a number the operator
// did not type would be inventing intent.
func TestJoinWindow_OpenBlankDurationUsesConfiguredDefault(t *testing.T) {
	c := &fakeClient{openWindowResp: &rpcpb.OpenColonyJoinWindowResponse{
		Window: windowAt(t, 15*time.Minute), AppliedDurationSeconds: 900,
	}}
	s := newTestServer(t, c)

	rr := postForm(t, s, "/machine/join-window/open")

	if c.lastOpenWindowReq == nil {
		t.Fatalf("blank duration never reached OpenColonyJoinWindow")
	}
	if got := c.lastOpenWindowReq.GetDurationSeconds(); got != 0 {
		t.Errorf("DurationSeconds = %d, want 0 so the manager applies its configured default", got)
	}
	// A redirect, not a re-render: a refresh must not re-post "open",
	// which would be a second window-opening act nobody intended.
	if rr.Code != http.StatusFound {
		t.Errorf("status = %d, want 302 so a browser refresh cannot re-open the window", rr.Code)
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "join_window_notice=") {
		t.Errorf("Location = %q, want it to carry the outcome notice", loc)
	}
}

// TestJoinWindow_OpenEchoesAppliedDuration pins the reason the handler
// reads applied_duration_seconds rather than subtracting timestamps: an
// operator whose request exceeded the ceiling is told which number won.
func TestJoinWindow_OpenEchoesAppliedDuration(t *testing.T) {
	c := &fakeClient{openWindowResp: &rpcpb.OpenColonyJoinWindowResponse{
		Window: windowAt(t, 15*time.Minute), AppliedDurationSeconds: 900,
	}}
	s := newTestServer(t, c)

	rr := postForm(t, s, "/machine/join-window/open")

	loc, err := url.QueryUnescape(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location %q: %v", rr.Header().Get("Location"), err)
	}
	if !strings.Contains(loc, "15 minutes") {
		t.Errorf("notice %q does not name the duration actually applied; the operator cannot tell which number won", loc)
	}
}

// TestJoinWindow_CloseCallsRPCAndRedirects covers the close path's one
// real property: it takes no input, and a browser refresh cannot repeat
// a close.
func TestJoinWindow_CloseCallsRPCAndRedirects(t *testing.T) {
	c := &fakeClient{closeWindowResp: &rpcpb.CloseColonyJoinWindowResponse{
		Window: &rpcpb.ColonyJoinWindow{Enabled: false},
	}}
	s := newTestServer(t, c)

	rr := postForm(t, s, "/machine/join-window/close")

	if c.lastCloseWindowReq == nil {
		t.Fatalf("close never reached the RPC")
	}
	if rr.Code != http.StatusFound {
		t.Errorf("status = %d, want 302", rr.Code)
	}
	if !strings.Contains(rr.Header().Get("Location"), "join_window_notice=") {
		t.Errorf("Location = %q, want the outcome notice", rr.Header().Get("Location"))
	}
}

// TestColonyAdvert_ClosedSaysOpenTheWindowNotCheckYourPIN is the
// requester-side wording rule, and it is a correctness rule rather than a
// style one. A closed window and a wrong second PIN have completely
// different fixes, so an operator told the wrong one goes looking for a
// PIN that was never the problem.
func TestColonyAdvert_ClosedSaysOpenTheWindowNotCheckYourPIN(t *testing.T) {
	c := &fakeClient{getWindowResp: &rpcpb.GetColonyJoinWindowResponse{
		Error: managerClosedRefusal,
	}}
	s := newTestServer(t, c)

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/machine/colony-advert?colony_target_address=10.62.0.2", nil))
	body := rr.Body.String()

	if !strings.Contains(body, "not an authorization failure") {
		t.Errorf("closed Colony did not distinguish itself from an authorization failure")
	}
	if !strings.Contains(body, "open the join window") {
		t.Errorf("the closed case did not name the fix (open the window)")
	}
}

// TestColonyAdvert_NoTargetRendersAPromptNotAFailure covers the fresh
// load, where the operator has not filled the form in yet. That state
// must read as a prompt, not as a failed dial.
func TestColonyAdvert_NoTargetRendersAPromptNotAFailure(t *testing.T) {
	c := &fakeClient{}
	s := newTestServer(t, c)

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/machine/colony-advert", nil))
	body := rr.Body.String()

	if c.lastGetWindowReq != nil {
		t.Errorf("an empty target still dialed the RPC; it should render a prompt instead")
	}
	if !strings.Contains(body, "Name the Colony member") {
		t.Errorf("empty target did not render a prompt; operator sees a blank page")
	}
}

// TestColonyAdvert_SaysFingerprintsUnpublishedWhenThereAreNone is the
// honesty test for the half of Part 4 that is not built. The RPC's
// publication fields (colony_name, managerd_fingerprints) "belong to the
// trust-store half of Part 4 and are filled in there" - which is not
// built. An empty list must therefore read as "not implemented", never as
// "there is nothing to check", because those send an operator very
// different places.
func TestColonyAdvert_SaysFingerprintsUnpublishedWhenThereAreNone(t *testing.T) {
	c := &fakeClient{getWindowResp: &rpcpb.GetColonyJoinWindowResponse{
		Window: windowAt(t, 15*time.Minute), ColonyNodeId: "colony-1",
	}}
	s := newTestServer(t, c)

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/machine/colony-advert?colony_target_address=10.62.0.2", nil))
	body := rr.Body.String()

	if !strings.Contains(body, "not yet advertising") && !strings.Contains(body, "not published") {
		t.Errorf("an empty fingerprint list did not say it is unimplemented; an operator would read it as \"nothing to check\"")
	}
	if strings.Contains(body, "SHA256:") {
		t.Errorf("page rendered a fingerprint that the RPC never returned")
	}
}

// TestColonyAdvert_RendersFingerprintsWhenTheColonyPublishesThem is the
// other half of that rule, so the test above cannot be satisfied by a
// page that simply never renders fingerprints at all.
func TestColonyAdvert_RendersFingerprintsWhenTheColonyPublishesThem(t *testing.T) {
	c := &fakeClient{getWindowResp: &rpcpb.GetColonyJoinWindowResponse{
		Window:       windowAt(t, 15*time.Minute),
		ColonyNodeId: "colony-1",
		ColonyName:   "colony-1.apiary.work",
		ManagerdFingerprints: map[string]string{
			"colony-1": "SHA256:aa:bb:cc",
			"colony-2": "SHA256:dd:ee:ff",
		},
	}}
	s := newTestServer(t, c)

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/machine/colony-advert?colony_target_address=10.62.0.2", nil))
	body := rr.Body.String()

	for _, want := range []string{"SHA256:aa:bb:cc", "SHA256:dd:ee:ff", "colony-1.apiary.work"} {
		if !strings.Contains(body, want) {
			t.Errorf("advert page did not render %q", want)
		}
	}
}

// TestColonyAdvert_ForwardsTargetAddressOnTheOperatorsBehalf is the
// ADR-0092 property: the target address is passed through, because naming
// a remote member is the entire point of this read. The port is filled in
// for the operator and never taken from what they typed - ADR-0139.
func TestColonyAdvert_ForwardsTargetAddressOnTheOperatorsBehalf(t *testing.T) {
	c := &fakeClient{getWindowResp: &rpcpb.GetColonyJoinWindowResponse{
		Window: windowAt(t, 15*time.Minute), ColonyNodeId: "colony-1",
	}}
	s := newTestServer(t, c)

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/machine/colony-advert?colony_target_address=10.62.0.2", nil))

	if c.lastGetWindowReq == nil {
		t.Fatalf("the read never reached the RPC")
	}
	if got := c.lastGetWindowReq.GetTargetAddress(); got != "10.62.0.2:17700" {
		t.Errorf("TargetAddress = %q, want the operator's host with the fixed port applied", got)
	}
}

// TestDeadlineLocal_RendersAbsoluteLocalTimeAndDashesZero guards the
// helper the whole countdown depends on. A zero unix value must not
// render as 1970, which is a date an operator would read as a real
// deadline.
func TestDeadlineLocal_RendersAbsoluteLocalTimeAndDashesZero(t *testing.T) {
	if got := deadlineLocal(0); got != "-" {
		t.Errorf("deadlineLocal(0) = %q, want a dash; 1970 would read as a real deadline", got)
	}
	if got := deadlineLocal(-5); got != "-" {
		t.Errorf("deadlineLocal(-5) = %q, want a dash", got)
	}
	live := deadlineLocal(time.Now().Add(time.Hour).Unix())
	if !strings.Contains(live, ":") {
		t.Errorf("deadlineLocal(live) = %q, want an absolute clock time", live)
	}
}

// TestRemainingText_PicksTheLargestExactUnit keeps the countdown honest:
// "14 minutes" when 44 seconds remain would overstate the time left.
func TestRemainingText_PicksTheLargestExactUnit(t *testing.T) {
	now := time.Now()
	cases := []struct {
		live bool
		left time.Duration
		want string
	}{
		{false, 0, "no time left"},
		{true, 44 * time.Second, "44 seconds"},
		{true, 15 * time.Minute, "15 minutes"},
		{true, 90 * time.Second, "1 minutes 30 seconds"},
		{true, 2 * time.Hour, "2 hours"},
		{true, 150 * time.Minute, "2 hours 30 minutes"},
	}
	for _, c := range cases {
		// Truncate FIRST, then add, rather than adding then truncating:
		// time.Now() carries a sub-second fraction, so now.Add(d).Unix()
		// rounds d DOWN and every case would come up a second short -
		// which is how this test failed its own expectations first time.
		v := colonyJoinWindowView{Live: c.live, Now: now, ExpiresAtUnix: now.Unix() + int64(c.left.Seconds())}
		if got := remainingText(v); got != c.want {
			t.Errorf("remainingText(live=%v, left=%v) = %q, want %q", c.live, c.left, got, c.want)
		}
	}
}

// TestRemainingText_FloorsAtZeroAndNeverGoesNegative is the edge that
// matters at the boundary: a window that expired a second ago must read
// as no time left, not as a negative countdown.
func TestRemainingText_FloorsAtZeroAndNeverGoesNegative(t *testing.T) {
	now := time.Now()
	v := colonyJoinWindowView{Live: true, Now: now, ExpiresAtUnix: now.Add(-time.Hour).Unix()}
	if got := v.remaining(); got != 0 {
		t.Errorf("remaining() on an expired window = %v, want 0", got)
	}
}
