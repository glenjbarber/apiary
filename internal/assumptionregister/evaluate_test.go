package assumptionregister

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// evalNow is the fixed clock every evaluation test reads against. A
// fixed clock is the point: the whole design is that a verdict ages on
// its own, so a test that let the real clock drift would be testing
// time.Now rather than Evaluate.
var evalNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// baseClaim is a well-formed, unobserved claim: the shape an operator
// gets for free from the form, with nothing yet checked. Every case
// below is this claim with exactly one fact changed, so a failure names
// the fact rather than a whole scenario.
func baseClaim() Claim {
	return Claim{
		ID:        "nat-uplink",
		Statement: "The configured NAT uplink owns the default route",
		Owner:     "ops",
		Scope:     "hive apiarium",
		Evidence:  "2026-09-29 route get output",
		ExpiresAt: evalNow.Add(24 * time.Hour),
	}
}

// withStatus is baseClaim recorded with a status, a verification time,
// and nothing else changed.
func withStatus(s EvidenceStatus, lastVerified time.Time) Claim {
	c := baseClaim()
	c.EvidenceStatus = s
	c.LastVerified = lastVerified
	return c
}

// TestEvaluateEveryState pins each of the five states to the exact
// recorded facts that produce it. The states are not interchangeable:
// supported and contradicted say something about the world, unknown
// says there is no evidence either way, stale says the clock passed the
// claim's own expiry, and not_applicable says the claim was never about
// this environment. Per this project's reading of quiet results, unknown
// is not healthy and not failed, and conflating it with any of the other
// four is the failure this whole type exists to prevent.
func TestEvaluateEveryState(t *testing.T) {
	verified := evalNow.Add(-2 * time.Hour)

	expiredAndConfirmed := withStatus(EvidenceSupported, verified)
	expiredAndConfirmed.ExpiresAt = evalNow.Add(-time.Minute)

	notApplicable := withStatus(EvidenceNotApplicable, verified)
	notApplicable.ExpiresAt = evalNow.Add(time.Hour)

	cases := []struct {
		name  string
		claim Claim
		want  State
	}{
		{"unobserved and unexpired is unknown", baseClaim(), StateUnknown},
		{"supported and verified within the claim's lifetime is supported", withStatus(EvidenceSupported, verified), StateSupported},
		{"contradicted is contradicted", withStatus(EvidenceContradicted, verified), StateContradicted},
		{"expired is stale, whatever the recorded status", expiredAndConfirmed, StateStale},
		{"recorded not_applicable is not_applicable", notApplicable, StateNotApplicable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.claim.Evaluate(evalNow)
			if got.State != tc.want {
				t.Fatalf("Evaluate().State = %q, want %q (reason %q)", got.State, tc.want, got.Reason)
			}
			if got.Reason == "" {
				t.Fatal("Evaluate().Reason is empty; a verdict with no words is a verdict conveyed by a bare enum")
			}
			if c := tc.claim.State(evalNow); c != tc.want {
				t.Fatalf("State() = %q, want %q", c, tc.want)
			}
		})
	}
}

// TestEvaluatePrecedence walks the ladder in the order Evaluate applies
// it, stacking each rung's facts on the ones above it, so a reordering
// of the branches would fail here rather than pass by luck.
func TestEvaluatePrecedence(t *testing.T) {
	// Rung 1: not_applicable beats everything, including its own
	// expiry. A claim that does not apply has nothing left to expire.
	notApplicableExpired := withStatus(EvidenceNotApplicable, evalNow.Add(-2*time.Hour))
	notApplicableExpired.ExpiresAt = evalNow.Add(-time.Hour)
	if got := notApplicableExpired.State(evalNow); got != StateNotApplicable {
		t.Fatalf("expired not_applicable = %q, want not_applicable", got)
	}

	// Rung 2: stale beats a recorded supported. A confirmation made
	// during the claim's lifetime says nothing about it now.
	staleButConfirmed := withStatus(EvidenceSupported, evalNow.Add(-2*time.Hour))
	staleButConfirmed.ExpiresAt = evalNow.Add(-time.Hour)
	if got := staleButConfirmed.State(evalNow); got != StateStale {
		t.Fatalf("expired supported = %q, want stale", got)
	}

	// stale beats contradicted too, for the same reason: the claim is
	// no longer current, so what its last check found has stopped being
	// the answer to the question being asked.
	staleAndContradicted := staleButConfirmed
	staleAndContradicted.EvidenceStatus = EvidenceContradicted
	if got := staleAndContradicted.State(evalNow); got != StateStale {
		t.Fatalf("expired contradicted = %q, want stale", got)
	}

	// Rung 3 and 4 are each other's inputs: contradicted outranks
	// supported in the sense that a claim cannot be both, and the later
	// recorded observation is the one on disk. That is expressed by the
	// single status field, so the ladder is exercised by checking the
	// two are distinct and each lands where it belongs.
	if EvidenceContradicted == EvidenceSupported {
		t.Fatal("contradicted and supported are the same token; the ladder has a hole")
	}
	if got := withStatus(EvidenceContradicted, evalNow.Add(-time.Hour)).State(evalNow); got != StateContradicted {
		t.Fatalf("contradicted = %q, want contradicted", got)
	}

	// Rung 5: unknown is the floor, and supported outranks it.
	if got := withStatus(EvidenceSupported, evalNow.Add(-time.Hour)).State(evalNow); got != StateSupported {
		t.Fatalf("confirmed = %q, want supported", got)
	}
	if got := baseClaim().State(evalNow); got != StateUnknown {
		t.Fatalf("bare claim = %q, want unknown", got)
	}
}

// TestEvaluateFailsClosed is the load-bearing test. Every one of these
// claims is something this build has no grounds to call true, and every
// one of them must read as unknown rather than supported. A single
// supported here is a claim reporting as proven with nothing behind it,
// which is the exact failure Priority 2 exists to prevent.
func TestEvaluateFailsClosed(t *testing.T) {
	supportedButNoTime := withStatus(EvidenceSupported, time.Time{})

	supportedButFuture := withStatus(EvidenceSupported, evalNow.Add(time.Hour))

	unrecognized := withStatus(EvidenceStatus("probably fine"), evalNow.Add(-time.Hour))

	supportedNoEvidence := withStatus(EvidenceSupported, evalNow.Add(-time.Hour))
	supportedNoEvidence.Evidence = "   "

	explicitUnobserved := withStatus(EvidenceUnobserved, evalNow.Add(-time.Hour))

	cases := []struct {
		name  string
		claim Claim
	}{
		{"supported with no last_verified at all", supportedButNoTime},
		{"last_verified in the future is not evidence of anything yet", supportedButFuture},
		{"a status token this build does not recognize", unrecognized},
		{"supported status but no recorded evidence to support it", supportedNoEvidence},
		{"the explicit unobserved token", explicitUnobserved},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.claim.Evaluate(evalNow)
			if got.State == StateSupported {
				t.Fatalf("Evaluate() = supported (%q); nothing in this case is evidence of anything", got.Reason)
			}
			if got.State != StateUnknown {
				t.Fatalf("Evaluate() = %q, want unknown", got.State)
			}
			if got.Reason == "" {
				t.Fatal("Reason is empty")
			}
		})
	}
}

// TestEvaluateExpiredNotApplicableStaysNotApplicable isolates the one
// rung of the ladder whose justification is easiest to get wrong. A
// not_applicable claim that has also expired is still not_applicable,
// not stale: reporting it as stale would be talking about a claim about
// this environment as no longer current, when it was never current here
// to begin with.
func TestEvaluateExpiredNotApplicableStaysNotApplicable(t *testing.T) {
	c := withStatus(EvidenceNotApplicable, evalNow.Add(-2*time.Hour))
	for _, expires := range []time.Time{
		evalNow.Add(365 * 24 * time.Hour),
		evalNow.Add(time.Hour),
		evalNow,
		evalNow.Add(-time.Hour),
		evalNow.Add(-365 * 24 * time.Hour),
	} {
		c.ExpiresAt = expires
		got := c.Evaluate(evalNow)
		if got.State != StateNotApplicable {
			t.Fatalf("expires_at %s: Evaluate() = %q, want not_applicable",
				expires.Format(time.RFC3339), got.State)
		}
	}
}

// TestValidateRejectsUnusableEvidenceStatus covers the three incoherent
// records Validate refuses rather than storing. Each would otherwise be
// a silent downgrade discovered later at read time, where the operator
// has long since moved on and never learns their typo was the cause.
func TestValidateRejectsUnusableEvidenceStatus(t *testing.T) {
	t.Run("an unrecognized status token", func(t *testing.T) {
		c := withStatus(EvidenceStatus("suported"), evalNow.Add(-time.Hour)) // the typo this exists for
		err := c.Validate()
		if err == nil {
			t.Fatal("Validate() = nil, want error")
		}
		if !strings.Contains(err.Error(), "suported") {
			t.Errorf("error %q does not quote the offending token", err)
		}
		// The message must also name what is acceptable, or the
		// operator is left guessing at the vocabulary.
		for _, token := range []string{"unobserved", "supported", "contradicted", "not_applicable"} {
			if !strings.Contains(err.Error(), token) {
				t.Errorf("error %q does not list the accepted token %q", err.Error(), token)
			}
		}
	})

	t.Run("supported with no last_verified", func(t *testing.T) {
		c := withStatus(EvidenceSupported, time.Time{})
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() = nil, want error")
		}
	})

	t.Run("last_verified exactly at expires_at", func(t *testing.T) {
		c := withStatus(EvidenceSupported, baseClaim().ExpiresAt)
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() = nil, want error")
		}
	})

	t.Run("last_verified after expires_at", func(t *testing.T) {
		c := withStatus(EvidenceSupported, baseClaim().ExpiresAt.Add(time.Minute))
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() = nil, want error")
		}
	})

	t.Run("a coherent record is accepted", func(t *testing.T) {
		c := withStatus(EvidenceSupported, baseClaim().ExpiresAt.Add(-time.Second))
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})
}

// TestNewFieldsRoundTripThroughTheStore proves the new fields survive
// the disk format the manager actually uses, and that a claim read back
// evaluates to the same verdict. A field that is not persisted cannot
// be evaluated next boot, which would make the whole state model a
// display-only fiction.
func TestNewFieldsRoundTripThroughTheStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "register.json")
	m := &Manager{Path: path}

	verified := evalNow.Add(-90 * time.Minute)
	claim := baseClaim()
	claim.ConsequenceIfFalse = "the default route leaves the Cell, so NAT hairpin is silently absent"
	claim.VerificationMethod = "re-run route get and compare against the configured uplink"
	claim.EvidenceStatus = EvidenceSupported
	claim.LastVerified = verified

	if err := m.Save(claim, evalNow); err != nil {
		t.Fatal(err)
	}

	// Read back through a fresh Manager so the result is the file's
	// and not one handle's in-process state.
	stored, err := (&Manager{Path: path}).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("claims = %d, want 1", len(stored))
	}
	got := stored[0]
	if got.ConsequenceIfFalse != claim.ConsequenceIfFalse {
		t.Errorf("consequence_if_false = %q, want %q", got.ConsequenceIfFalse, claim.ConsequenceIfFalse)
	}
	if got.VerificationMethod != claim.VerificationMethod {
		t.Errorf("verification_method = %q, want %q", got.VerificationMethod, claim.VerificationMethod)
	}
	if got.EvidenceStatus != EvidenceSupported {
		t.Errorf("evidence_status = %q, want supported", got.EvidenceStatus)
	}
	if !got.LastVerified.Equal(verified) {
		t.Errorf("last_verified = %s, want %s", got.LastVerified, verified)
	}
	if g, w := got.State(evalNow), StateSupported; g != w {
		t.Errorf("State() after a round trip = %q, want %q", g, w)
	}

	// The very same bytes, read one second past this claim's expiry,
	// must read stale with nothing rewritten in between. That is the
	// property the derived-and-never-persisted design exists to give.
	afterExpiry := got.State(got.ExpiresAt.Add(time.Second))
	if afterExpiry != StateStale {
		t.Fatalf("State() one second past expiry = %q, want stale", afterExpiry)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "2026-09-29") && !strings.Contains(string(body), "2026-09") {
		t.Fatalf("on-disk register has no verification timestamp in it:\n%s", body)
	}
}

// TestAnUnsetEvidenceStatusReadsAsUnobserved checks the zero value is
// the same silence as the explicit token, so a claim saved before the
// field existed evaluates exactly like one saved with it spelled out.
func TestAnUnsetEvidenceStatusReadsAsUnobserved(t *testing.T) {
	unset := baseClaim()
	unset.LastVerified = evalNow.Add(-time.Hour)
	explicit := withStatus(EvidenceUnobserved, evalNow.Add(-time.Hour))

	if unset.EvidenceStatus != "" {
		t.Fatalf("the zero value is %q, want the empty string", unset.EvidenceStatus)
	}
	if a, b := unset.Evaluate(evalNow).State, explicit.Evaluate(evalNow).State; a != b || a != StateUnknown {
		t.Fatalf("unset = %q and explicit unobserved = %q, want both unknown", a, b)
	}
}

// TestDerivedStateIsNeverPersisted reads the store's own bytes rather
// than the decoded struct, because the struct is where a derived field
// would be easiest to smuggle back in. The recorded half must of course
// be present; the derived half must not appear at all.
func TestDerivedStateIsNeverPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "register.json")
	m := &Manager{Path: path}

	c := withStatus(EvidenceSupported, evalNow.Add(-time.Hour))
	c.ConsequenceIfFalse = "external reachability is lost"
	if err := m.Save(c, evalNow); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, forbidden := range []string{`"state"`, `"state_detail"`, `"State"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("on-disk register contains %q; the derived verdict must never be written back:\n%s", forbidden, body)
		}
	}
	// What is there is the observation, not the conclusion: the token
	// "supported" appears as the recorded evidence status, and the only
	// way to recover a verdict is to re-run Evaluate against a clock.
	if !strings.Contains(text, `"evidence_status": "supported"`) {
		t.Fatalf("on-disk register does not carry the recorded evidence status:\n%s", body)
	}
	if !strings.Contains(text, `"last_verified"`) {
		t.Fatalf("on-disk register does not carry the verification time:\n%s", body)
	}
}

// TestAutomatedObservationNeverMutatesAnOperatorClaim is named in this
// package's doc comment as the test that holds the operator/automation
// boundary, so it is checked here rather than in a file someone has to
// go looking for.
//
// The boundary is one-directional by construction: the recorded fields
// are the only things on disk, and Evaluate is a pure function of them
// plus the clock. An automated check can record what it saw in
// EvidenceStatus and when in LastVerified - and that is the entire
// mechanism by which automation is allowed to touch an operator's claim
// at all. It cannot write a verdict, because there is nowhere to put
// one, and every read re-derives from what was recorded.
func TestAutomatedObservationNeverMutatesAnOperatorClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "register.json")
	m := &Manager{Path: path}

	operator := baseClaim()
	operator.ConsequenceIfFalse = "loses external reachability for every cell in this hive"
	if err := m.Save(operator, evalNow); err != nil {
		t.Fatal(err)
	}
	before, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("claims = %d, want 1", len(before))
	}
	// The operator wrote this claim and checked nothing, so it is
	// unknown - not a pass.
	if got := before[0].State(evalNow); got != StateUnknown {
		t.Fatalf("operator's unverified claim = %q, want unknown", got)
	}

	// An automated checker finds the evidence contradicted. It may
	// record that, because recording an observation is the only channel
	// automation has here.
	observed := before[0]
	observed.EvidenceStatus = EvidenceContradicted
	observed.LastVerified = evalNow.Add(-time.Minute)
	if err := m.Save(observed, evalNow.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	afterList, err := (&Manager{Path: path}).List()
	if err != nil {
		t.Fatal(err)
	}
	after := afterList[0]
	if got := after.State(evalNow); got != StateContradicted {
		t.Fatalf("after an automated observation, state = %q, want contradicted", got)
	}

	// What automation did not get to touch: the operator's statement,
	// owner, scope, evidence, and stated consequence. It reported what
	// it saw; it did not rewrite what the operator said.
	if after.Statement != operator.Statement ||
		after.Owner != operator.Owner ||
		after.Scope != operator.Scope ||
		after.Evidence != operator.Evidence ||
		after.ConsequenceIfFalse != operator.ConsequenceIfFalse {
		t.Fatalf("an automated observation altered the operator's claim:\n before %#v\n after  %#v", before[0], after)
	}
	// Nor did it stamp a verdict into the record: the claim's creation
	// is still the operator's save, and the bytes hold only
	// observations.
	if !after.CreatedAt.Equal(before[0].CreatedAt) {
		t.Fatalf("created_at moved from %s to %s", before[0].CreatedAt, after.CreatedAt)
	}
}
