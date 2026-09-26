package restartplan

import (
	"strings"
	"testing"
)

// TestOutcomeClassification is the project's central convention, stated
// as code: unknown is never success and never failure. Every constant is
// checked against IsSuccess/IsFailure/IsUnknown, so adding an outcome
// later without deciding which of the three it belongs to fails here
// rather than in production.
func TestOutcomeClassification(t *testing.T) {
	cases := []struct {
		outcome   Outcome
		success   bool
		failure   bool
		unknown   bool
		humanName string
	}{
		{OutcomeConfirmed, true, false, false, "confirmed"},
		{OutcomeFailed, false, true, false, "failed"},
		{OutcomeBlocked, false, false, false, "blocked"},
		{OutcomeUnobserved, false, false, true, "unobserved"},
		{OutcomeUnverified, false, false, true, "unverified"},
	}
	for _, tc := range cases {
		t.Run(string(tc.outcome), func(t *testing.T) {
			if IsSuccess(tc.outcome) != tc.success {
				t.Errorf("IsSuccess = %v, want %v", IsSuccess(tc.outcome), tc.success)
			}
			if IsFailure(tc.outcome) != tc.failure {
				t.Errorf("IsFailure = %v, want %v", IsFailure(tc.outcome), tc.failure)
			}
			if IsUnknown(tc.outcome) != tc.unknown {
				t.Errorf("IsUnknown = %v, want %v", IsUnknown(tc.outcome), tc.unknown)
			}
			// The three predicates must partition the space: exactly one
			// of them, or none, but never two.
			n := 0
			for _, b := range []bool{IsSuccess(tc.outcome), IsFailure(tc.outcome), IsUnknown(tc.outcome)} {
				if b {
					n++
				}
			}
			if n > 1 {
				t.Errorf("outcome %q is classified as more than one of success/failure/unknown", tc.outcome)
			}
		})
	}

	t.Run("the vocabulary matches internal/cluster's own", func(t *testing.T) {
		// The project's third states are named for what happened to the
		// observation: internal/cluster has unverified_replica for "no
		// live observation was even attempted" and replica_unobserved
		// for "attempted, got nothing usable". This package's two
		// unknown constants exist for exactly those two cases, and their
		// doc comments say so; this test is what stops the pairing from
		// quietly inverting.
		if !strings.Contains(strings.ToLower(string(OutcomeUnobserved)), "unobserved") {
			t.Errorf("OutcomeUnobserved = %q, want the 'unobserved' half of the pair", OutcomeUnobserved)
		}
		if !strings.Contains(strings.ToLower(string(OutcomeUnverified)), "unverified") {
			t.Errorf("OutcomeUnverified = %q, want the 'unverified' half of the pair", OutcomeUnverified)
		}
	})
}

// TestResultRender pins the operator-facing wording, because "renders as
// unknown" is a requirement and not an aspiration: a rendering that reads
// like a success is how an unknown becomes a false claim in a log.
func TestResultRender(t *testing.T) {
	cases := []struct {
		name       string
		result     Result
		mustHave   []string
		mustNotHav []string
	}{
		{
			name: "confirmed reads as a confirmation",
			result: Result{Service: DefaultService, NodeID: "comb-a", LeaseID: 7,
				Outcome: OutcomeConfirmed, Detail: "reported itself back healthy", Evidence: []string{"lease released"}},
			mustHave:   []string{"apiary_raftd on comb-a", "confirmed healthy", "lease 7"},
			mustNotHav: []string{"UNKNOWN", "FAILED", "not attempted"},
		},
		{
			name: "failed reads as a failure",
			result: Result{Service: DefaultService, NodeID: "comb-a", LeaseID: 7,
				Outcome: OutcomeFailed, Detail: "service returned exit status 1", Evidence: []string{"exit 1"}},
			mustHave:   []string{"FAILED", "exit status 1"},
			mustNotHav: []string{"confirmed healthy"},
		},
		{
			name: "blocked reads as not attempted, not as a failure",
			result: Result{Service: DefaultService, NodeID: "comb-a", LeaseID: 0,
				Outcome: OutcomeBlocked, Detail: "quorum would be lost", Evidence: []string{"raftd-quorum-safety"}},
			mustHave:   []string{"not attempted", "blocked by guardrail", "quorum would be lost"},
			mustNotHav: []string{"FAILED", "confirmed healthy", "UNKNOWN"},
		},
		{
			name: "unobserved reads as unknown and says so in those words",
			result: Result{Service: DefaultService, NodeID: "comb-a", LeaseID: 9,
				Outcome: OutcomeUnobserved, Detail: "no attempt confirmed it",
				Evidence: []string{"attempt 1/5: connection refused"}},
			mustHave: []string{"UNKNOWN", "unobserved", "not a confirmed failure and not a confirmed success"},
		},
		{
			name: "unverified reads as unknown too",
			result: Result{Service: DefaultService, NodeID: "comb-a", LeaseID: 0,
				Outcome: OutcomeUnverified, Detail: "the probe could not run"},
			mustHave: []string{"UNKNOWN", "unverified", "not a confirmed failure and not a confirmed success"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.result.Render()
			for _, want := range tc.mustHave {
				if !strings.Contains(got, want) {
					t.Errorf("Render() = %q, want it to contain %q", got, want)
				}
			}
			for _, unwanted := range tc.mustNotHav {
				if strings.Contains(got, unwanted) {
					t.Errorf("Render() = %q, must not contain %q - a rendering may never be mistaken for another outcome", got, unwanted)
				}
			}
		})
	}

	t.Run("a node-less result still names its service", func(t *testing.T) {
		got := Result{Service: DefaultService, Outcome: OutcomeBlocked, Detail: "no target"}.Render()
		if !strings.HasPrefix(got, DefaultService+":") {
			t.Errorf("Render() = %q, want it to start with the service name", got)
		}
	})
}

// TestResultValidate covers the honesty invariants: a verdict with no
// reason is not a record, and the two positive verdicts additionally
// require the evidence that earned them.
func TestResultValidate(t *testing.T) {
	valid := func(mutate func(*Result)) Result {
		r := Result{Service: DefaultService, NodeID: "comb-a", LeaseID: 1,
			Outcome: OutcomeConfirmed, Detail: "confirmed by the restarted process",
			Evidence: []string{"lease released by RecordRestartCompleted"}}
		mutate(&r)
		return r
	}

	t.Run("a well-formed record validates", func(t *testing.T) {
		if err := valid(func(*Result) {}).Validate(); err != nil {
			t.Errorf("Validate = %v, want nil", err)
		}
	})

	t.Run("a record with no service is refused", func(t *testing.T) {
		if err := valid(func(r *Result) { r.Service = "" }).Validate(); err == nil {
			t.Errorf("Validate accepted a record with no service")
		}
	})

	t.Run("a record with no node is refused", func(t *testing.T) {
		if err := valid(func(r *Result) { r.NodeID = "" }).Validate(); err == nil {
			t.Errorf("Validate accepted a record with no node_id")
		}
	})

	t.Run("a verdict with no detail is refused", func(t *testing.T) {
		if err := valid(func(r *Result) { r.Detail = "   " }).Validate(); err == nil {
			t.Errorf("Validate accepted a verdict with no stated reason")
		}
	})

	t.Run("a confirmation with no evidence is refused", func(t *testing.T) {
		if err := valid(func(r *Result) { r.Evidence = nil }).Validate(); err == nil {
			t.Errorf("Validate accepted an unbacked confirmation - this is the exact bug the type exists to make unwritable")
		}
	})

	t.Run("a failure with no evidence is refused", func(t *testing.T) {
		r := valid(func(r *Result) { r.Outcome = OutcomeFailed })
		r.Evidence = nil
		if err := r.Validate(); err == nil {
			t.Errorf("Validate accepted an unbacked failure")
		}
	})

	t.Run("an unknown needs a reason but not positive evidence", func(t *testing.T) {
		r := valid(func(r *Result) { r.Outcome = OutcomeUnobserved })
		r.Evidence = []string{"attempt 1/5: connection refused"}
		if err := r.Validate(); err != nil {
			t.Errorf("Validate = %v, want nil - unknown carries what was tried, not a proof", err)
		}
	})

	t.Run("a block needs only a reason", func(t *testing.T) {
		r := Result{Service: DefaultService, NodeID: "comb-a", Outcome: OutcomeBlocked,
			Detail: "the quorum-safety guardrail did not allow this restart"}
		if err := r.Validate(); err != nil {
			t.Errorf("Validate = %v, want nil", err)
		}
	})

	t.Run("an unrecognized outcome is refused", func(t *testing.T) {
		if err := valid(func(r *Result) { r.Outcome = Outcome("probably fine") }).Validate(); err == nil {
			t.Errorf("Validate accepted a made-up outcome - a new state must be named deliberately, not by typo")
		}
	})

	t.Run("the empty outcome is refused rather than defaulted", func(t *testing.T) {
		if err := valid(func(r *Result) { r.Outcome = "" }).Validate(); err == nil {
			t.Errorf("Validate accepted an empty outcome; the zero value must not read as a verdict")
		}
	})
}

// TestResultStoreRefusesUnwritableVerdicts checks that the store's
// validation is a real gate rather than a comment: a record that would
// lie does not reach the disk.
func TestResultStoreRefusesUnwritableVerdicts(t *testing.T) {
	dir := t.TempDir()
	s := NewResultStore(dir)

	unbacked := Result{Service: DefaultService, NodeID: "comb-a", Outcome: OutcomeConfirmed,
		Detail: "confirmed", LeaseID: 1}
	if err := s.Save(unbacked); err == nil {
		t.Errorf("Save wrote an unbacked confirmation to disk")
	}
	if _, found, err := s.Load("comb-a", DefaultService); err != nil || found {
		t.Errorf("Load after a refused Save = (found=%v, err=%v), want (false, nil) - nothing reached the disk", found, err)
	}
}
