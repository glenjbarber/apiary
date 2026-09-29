package frontend

import (
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/assumptionregister"
	"github.com/glenjbarber/apiary/web"
)

func TestServer_AssumptionRegisterPageRendersLocalClaim(t *testing.T) {
	client := &fakeClient{assumptionClaimsResp: &rpcpb.ListAssumptionClaimsResponse{
		Claims: []*rpcpb.AssumptionClaim{{
			Id: "route-uplink", Statement: "NAT owns the default route",
			Owner: "operations", Scope: "hive:apiarium",
			Evidence: "route -n get default", ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
		}},
	}}
	s := newTestServer(t, client)
	req := httptest.NewRequest(http.MethodGet, "/assumption-register", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"Assumption Register", "route-uplink", "operations", "hive:apiarium", "route -n get default"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("page missing %q: %s", want, rec.Body.String())
		}
	}
}

// escaped is what html/template produced of a string. The state
// sentences and the reasons are written for a person to read, so they
// contain apostrophes and quotes on purpose, and the template escapes
// both. Comparing rendered text against unescaped expectations would
// make every such assertion fail for a reason that has nothing to do
// with what it is checking.
func escaped(s string) string { return html.EscapeString(s) }

// renderRegisterPage drives the real embedded template through the real
// handler, exactly as a browser would, so these assertions are about
// the HTML an operator actually reads rather than about the view struct
// a unit test could have got right on its own.
func renderRegisterPage(t *testing.T, claims ...*rpcpb.AssumptionClaim) string {
	t.Helper()
	client := &fakeClient{assumptionClaimsResp: &rpcpb.ListAssumptionClaimsResponse{Claims: claims}}
	s := newTestServer(t, client)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assumption-register", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func renderSimulatePage(t *testing.T, claims ...*rpcpb.AssumptionClaim) string {
	t.Helper()
	s := newTestServer(t, &fakeClient{simulateResp: &rpcpb.SimulateNodeFailureResponse{RelevantClaims: claims}})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/simulate?node_id=apiarium", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("simulate status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// stateCell returns the one claim-state cell in a rendered page, so a
// test can say what the verdict is without the surrounding page -
// including the evidence column, which legitimately says "supported"
// for a claim whose derived state is something else - satisfying the
// assertion by accident.
func stateCell(t *testing.T, body string) string {
	t.Helper()
	const open = `<td class="claim-state" data-state="`
	start := strings.Index(body, open)
	if start < 0 {
		t.Fatalf("no claim-state cell in the rendered page:\n%s", body)
	}
	rest := body[start:]
	if strings.Count(body, open) != 1 {
		t.Fatalf("want exactly one claim to render so the state cell is unambiguous, got several")
	}
	end := strings.Index(rest, "</td>")
	if end < 0 {
		t.Fatalf("unterminated claim-state cell:\n%s", rest)
	}
	return rest[:end]
}

// wireClaimFor renders a stored claim the way managerd does on a read:
// the recorded fields go on the wire as they are stored, and the state
// is derived against the given clock and never written back. Building
// the wire value this way rather than hand-writing State keeps these
// tests honest end to end - a regression in the evaluator breaks them
// here too, and not only on the page.
//
// It deliberately does not call into internal/manager: the frontend
// package must not depend on the manager, and these two lines of
// derivation are the whole contract between them.
func wireClaimFor(claim assumptionregister.Claim, now time.Time) *rpcpb.AssumptionClaim {
	evaluation := claim.Evaluate(now)
	wire := &rpcpb.AssumptionClaim{
		Id: claim.ID, Statement: claim.Statement, Owner: claim.Owner,
		Scope: claim.Scope, Evidence: claim.Evidence,
		ConsequenceIfFalse: claim.ConsequenceIfFalse,
		VerificationMethod: claim.VerificationMethod,
		EvidenceStatus:     string(claim.EvidenceStatus),
		ExpiresAtUnix:      claim.ExpiresAt.Unix(),
		State:              string(evaluation.State),
		StateDetail:        evaluation.Reason,
	}
	if !claim.LastVerified.IsZero() {
		wire.LastVerifiedUnix = claim.LastVerified.Unix()
	}
	return wire
}

// withEvidence records a check outcome on a claim, the way an operator
// or a future checker would.
func withEvidence(c assumptionregister.Claim, status string, lastVerified time.Time) assumptionregister.Claim {
	c.EvidenceStatus = assumptionregister.EvidenceStatus(status)
	c.LastVerified = lastVerified
	return c
}

// TestClaimStateIsNeverRenderedAsSupported is the test the whole feature
// exists to pass.
//
// Every case below but the last is a claim Apiary has no grounds to call
// true, and none of them may reach the operator's screen with the word
// "Supported" in the state cell, whatever the recorded evidence status
// says. The dangerous case is the expired one: a claim whose evidence
// WAS confirmed, inside its own lifetime, and which has since aged out.
// Every naive implementation renders that as supported, because the
// raw field says so; the whole reason the state is derived at read time
// is that it does not.
func TestClaimStateIsNeverRenderedAsSupported(t *testing.T) {
	now := time.Now()
	live, past := now.Add(time.Hour), now.Add(-time.Hour)

	base := func() assumptionregister.Claim {
		return assumptionregister.Claim{
			ID: "a", Statement: "s", Owner: "ops", Scope: "hive:apiarium",
			Evidence: "a note", ExpiresAt: live,
		}
	}
	expired := func(c assumptionregister.Claim) assumptionregister.Claim {
		c.ExpiresAt = past
		return c
	}

	cases := []struct {
		name string
		// override replaces the derived state on the wire, for the two
		// cases where the point is that the page normalizes what it
		// did not recognize rather than trusting it.
		overrideState  string
		overrideDetail string
		claim          assumptionregister.Claim
		wantState      string
	}{
		{name: "never checked at all", claim: base(), wantState: claimUnknown},
		{name: "recorded unobserved, unexpired", claim: withEvidence(base(), "unobserved", time.Time{}), wantState: claimUnknown},
		{name: "recorded supported but never verified", claim: withEvidence(base(), "supported", time.Time{}), wantState: claimUnknown},
		{name: "verified in the future", claim: withEvidence(base(), "supported", now.Add(2*time.Hour)), wantState: claimUnknown},
		{name: "contradicted", claim: withEvidence(base(), "contradicted", now.Add(-time.Hour)), wantState: claimContradicted},
		{
			name:      "expired, even though it was confirmed in time",
			claim:     expired(withEvidence(base(), "supported", now.Add(-24*time.Hour))),
			wantState: claimStale,
		},
		{
			name:      "expired and not applicable",
			claim:     expired(withEvidence(base(), "not_applicable", now.Add(-24*time.Hour))),
			wantState: claimNotApplicable,
		},
		{name: "genuinely supported", claim: withEvidence(base(), "supported", now.Add(-time.Hour)), wantState: claimSupported},
		{
			name:           "a state token this build does not recognize",
			claim:          base(),
			overrideState:  "probably fine",
			overrideDetail: "a peer said so",
			wantState:      claimUnknown,
		},
		{
			name:          "no state at all, from a node that never evaluated it",
			claim:         base(),
			overrideState: "",
			wantState:     claimUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire := wireClaimFor(tc.claim, now)
			if tc.overrideState != "" || tc.overrideDetail != "" {
				wire.State, wire.StateDetail = tc.overrideState, tc.overrideDetail
			}
			cell := stateCell(t, renderRegisterPage(t, wire))

			wantAttr := `<td class="claim-state" data-state="` + tc.wantState + `">`
			if !strings.Contains(cell, wantAttr) {
				t.Fatalf("state cell is not %s:\n%s", wantAttr, cell)
			}
			// The badge class and the short word are the colour, and
			// neither is allowed to be the only thing there.
			if want := `class="badge ` + claimStateBadge[tc.wantState] + `"`; !strings.Contains(cell, want) {
				t.Errorf("state cell has no %q badge:\n%s", want, cell)
			}
			if want := ">" + claimStateShort[tc.wantState] + "<"; !strings.Contains(cell, want) {
				t.Errorf("state cell does not name the state in words (%s):\n%s", want, cell)
			}
			// And the full sentence, which is what makes the badge
			// redundant rather than load-bearing.
			if want := escaped(claimStateWords[tc.wantState]); !strings.Contains(cell, want) {
				t.Errorf("state cell is missing its sentence (%q):\n%s", want, cell)
			}
			// The reason, in words, whenever the node supplied one.
			if detail := escaped(wire.GetStateDetail()); detail != "" && !strings.Contains(cell, detail) {
				t.Errorf("state cell does not carry the node's state_detail (%q):\n%s", detail, cell)
			}
			if tc.wantState != claimSupported {
				if strings.Contains(cell, claimStateShort[claimSupported]) {
					t.Errorf("a %s claim's state cell reads as supported:\n%s", tc.wantState, cell)
				}
				if strings.Contains(cell, escaped(claimStateWords[claimSupported])) {
					t.Errorf("a %s claim's state cell carries the supported sentence:\n%s", tc.wantState, cell)
				}
				if strings.Contains(cell, `class="badge `+claimStateBadge[claimSupported]+`"`) {
					t.Errorf("a %s claim's state cell wears the supported badge:\n%s", tc.wantState, cell)
				}
			}
		})
	}
}

// TestClaimStateReachedTheBrowserCarriesItsReason is the half of the
// rule the state table cannot express: every state must arrive with a
// sentence saying why, so a reader is never left to infer a verdict's
// cause from a token and a colour.
func TestClaimStateReachedTheBrowserCarriesItsReason(t *testing.T) {
	for _, state := range claimStateOrder {
		t.Run(state, func(t *testing.T) {
			reason := "because the reporting Comb ran the check and got " + state
			cell := stateCell(t, renderRegisterPage(t, &rpcpb.AssumptionClaim{
				Id: "a", Statement: "s", Evidence: "a note",
				State: state, StateDetail: reason,
				ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
			}))
			if !strings.Contains(cell, escaped(reason)) {
				t.Fatalf("state_detail did not reach the rendered page:\n%s", cell)
			}
		})
	}
}

// TestClaimStateVocabularyIsClosed guards against a state being added to
// the register's evaluation without this page learning to say it. A
// state with no words here would render as a bare token or as an empty
// cell, which is the failure the whole rendering is built to prevent.
func TestClaimStateVocabularyIsClosed(t *testing.T) {
	for _, state := range claimStateOrder {
		if claimStateWords[state] == "" {
			t.Errorf("state %q has no sentence", state)
		}
		if claimStateShort[state] == "" {
			t.Errorf("state %q has no short word for the badge", state)
		}
		if claimStateBadge[state] == "" {
			t.Errorf("state %q has no badge class", state)
		}
		// The words must not be the token. A sentence that is just
		// "supported" with a capital letter is a bare enum wearing a
		// sentence's clothes.
		if strings.EqualFold(claimStateWords[state], strings.ReplaceAll(state, "_", " ")) {
			t.Errorf("state %q's sentence is its own token: %q", state, claimStateWords[state])
		}
	}
	// Every badge class the state vocabulary uses must be a class
	// layout.html actually styles, or the colour silently falls back to
	// the neutral badge and a stale claim looks exactly like a healthy
	// one in the one place the colour is doing anything.
	layout, err := web.FS.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range claimStateOrder {
		if !strings.Contains(string(layout), ".badge."+claimStateBadge[state]) {
			t.Errorf("no layout.html rule for .badge.%s (state %q)", claimStateBadge[state], state)
		}
	}
}

// TestNeverVerifiedRendersAsWordsNotTheEpoch guards the one place a
// missing fact can be made to look like a present one. The wire says
// last_verified_unix is 0 for a claim nobody has ever checked, and 0 as
// a Unix timestamp is 1 January 1970 - which reads as a confirmation,
// dated. Silence dressed as evidence is worse than silence.
func TestNeverVerifiedRendersAsWordsNotTheEpoch(t *testing.T) {
	body := renderRegisterPage(t, &rpcpb.AssumptionClaim{
		Id: "offsite-backup", Statement: "an off-platform backup exists",
		Evidence: "asked the operator", ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
	})
	if !strings.Contains(body, "never verified") {
		t.Errorf("page does not say the evidence was never verified:\n%s", body)
	}
	for _, date := range []string{"1970", "1969"} {
		if strings.Contains(body, date) {
			t.Errorf("page renders %q for a claim that was never verified", date)
		}
	}
}

// TestConsequenceIfFalseIsShownNextToTheClaim covers Priority 2's own
// question - what becomes unsafe or unknown if this is false - which is
// useless if it is only visible at the moment somebody contradicts the
// claim. It is the operator's reason for accepting the risk, so it has
// to be on the page while they are deciding.
func TestConsequenceIfFalseIsShownNextToTheClaim(t *testing.T) {
	const consequence = "peers cannot reach each other, so every migration plan that assumes hairpin is wrong"

	body := renderRegisterPage(t, &rpcpb.AssumptionClaim{
		Id: "nat-uplink", Statement: "NAT owns the default route",
		Evidence: "route -n get default", ConsequenceIfFalse: consequence,
		ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
	})
	if !strings.Contains(body, consequence) {
		t.Errorf("the stated consequence of this claim being false is not on the register page:\n%s", body)
	}

	// And on the simulator, which is where somebody reads claims while
	// planning something.
	simBody := renderSimulatePage(t, wireClaimFor(assumptionregister.Claim{
		ID: "nat-uplink", Statement: "NAT owns the default route",
		Evidence: "route -n get default", ConsequenceIfFalse: consequence,
		EvidenceStatus: assumptionregister.EvidenceSupported,
		LastVerified:   time.Now().Add(-time.Hour),
		ExpiresAt:      time.Now().Add(-time.Hour),
	}, time.Now()))
	if !strings.Contains(simBody, consequence) {
		t.Errorf("the simulator does not show the consequence of the claim being false:\n%s", simBody)
	}
	if !strings.Contains(simBody, escaped(claimStateWords[claimStale])) {
		t.Errorf("the simulator does not render the stale claim's state in words:\n%s", simBody)
	}
}

// TestSimulatorReportsExpiredClaimsRatherThanHidingThem is the
// simulator's half of the behaviour change. The manager stopped
// filtering expired claims out, so this page now legitimately receives
// them, and the page's job is to make their arrival an improvement
// rather than a new way to mislead: a claim that vanished from a
// simulation reads as "nothing here depends on a register claim", which
// is a claim of its own and a much worse one.
func TestSimulatorReportsExpiredClaimsRatherThanHidingThem(t *testing.T) {
	now := time.Now()
	base := func() assumptionregister.Claim {
		return assumptionregister.Claim{
			ID: "gateway-availability", Statement: "the external gateway is reachable",
			Owner: "ops", Scope: "colony", Evidence: "curl to the origin",
			EvidenceStatus: assumptionregister.EvidenceSupported,
			LastVerified:   now.Add(-time.Hour),
		}
	}

	expiredClaim := base()
	expiredClaim.ExpiresAt = now.Add(-24 * time.Hour)
	expired := renderSimulatePage(t, wireClaimFor(expiredClaim, now))
	if !strings.Contains(expired, "gateway-availability") {
		t.Errorf("the simulator dropped an expired claim instead of reporting it stale:\n%s", expired)
	}
	if !strings.Contains(expired, escaped(claimStateWords[claimStale])) {
		t.Errorf("the simulator does not say the expired claim is stale:\n%s", expired)
	}
	// The page must not describe the list as unexpired any more, which
	// stopped being true the moment the filter went away.
	if strings.Contains(expired, "Unexpired claims") {
		t.Errorf("the simulator still calls these claims unexpired:\n%s", expired)
	}

	// The supported one still reads as supported, so the page is not
	// simply colouring everything the same out of caution.
	liveClaim := base()
	liveClaim.ExpiresAt = now.Add(time.Hour)
	live := renderSimulatePage(t, wireClaimFor(liveClaim, now))
	if !strings.Contains(live, escaped(claimStateWords[claimSupported])) {
		t.Errorf("a genuinely confirmed claim does not read as supported:\n%s", live)
	}
}

// TestRegisterFormCarriesTheNewFields covers the operator's half of the
// change. These three fields are how a claim acquires evidence at all
// now: without them an operator can only ever write unobserved claims,
// and the register becomes a list of things nobody has checked.
func TestRegisterFormCarriesTheNewFields(t *testing.T) {
	body := renderRegisterPage(t, &rpcpb.AssumptionClaim{
		Id: "a", Statement: "s", Evidence: "e", ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
	})
	for _, want := range []string{
		`name="consequence_if_false"`,
		`name="evidence_status"`,
		`name="last_verified"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the register form has no %s input:\n%s", want, body)
		}
	}

	// The select must offer the recorded vocabulary, in words, and must
	// not offer "stale" - stale is what Apiary concludes, not something
	// an operator writes down.
	_, selectBody, ok := strings.Cut(body, `name="evidence_status"`)
	if !ok {
		t.Fatal("no evidence_status select")
	}
	end := strings.Index(selectBody, "</select>")
	if end < 0 {
		t.Fatal("unterminated evidence_status select")
	}
	selectBody = selectBody[:end]
	for _, token := range []string{"unobserved", "supported", "contradicted", "not_applicable"} {
		if !strings.Contains(selectBody, `value="`+token+`"`) {
			t.Errorf("the evidence_status select does not offer %q:\n%s", token, selectBody)
		}
	}
	for _, token := range []string{claimStale, claimUnknown} {
		if strings.Contains(selectBody, `value="`+token+`"`) {
			t.Errorf("the evidence_status select offers %q, which is a conclusion rather than a recorded observation", token)
		}
	}
	// And every option is spelled out, so somebody choosing one learns
	// what it asserts rather than guessing from a token. The rendered
	// text is HTML-escaped, so the expectation is escaped too.
	for _, opt := range claimEvidenceStatusOptions {
		if opt.Label == opt.Value {
			t.Errorf("evidence status %q is offered as a bare token", opt.Value)
		}
		if !strings.Contains(selectBody, html.EscapeString(opt.Label)) {
			t.Errorf("the select does not explain %q as %q:\n%s", opt.Value, opt.Label, selectBody)
		}
	}
}

// TestRegisterEvidenceStatusChoicesMatchTheStoredVocabulary keeps the
// form and the register from drifting apart. A token the form cannot
// enter is a claim an operator cannot give evidence to, and a token the
// form offers that the register refuses is a form that lies.
func TestRegisterEvidenceStatusChoicesMatchTheStoredVocabulary(t *testing.T) {
	offered := map[string]bool{}
	for _, opt := range claimEvidenceStatusOptions {
		if offered[opt.Value] {
			t.Errorf("the form offers %q twice", opt.Value)
		}
		offered[opt.Value] = true
	}
	for _, want := range []string{"unobserved", "supported", "contradicted", "not_applicable"} {
		if !offered[want] {
			t.Errorf("the form does not offer the recorded status %q", want)
		}
	}
}
