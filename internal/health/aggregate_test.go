package health

import "testing"

// The aggregation rule is the ONE implementation of "how bad is the
// worst node", shared by the Command Center's health tree and ADR-0160's
// monitoring report. These tests pin the rule, because two
// implementations of it would eventually drift - and a monitoring system
// that contradicts the UI it is supposed to mirror is worse than one
// that has no opinion.

func TestAggregateStatus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []Status
		want     Status
	}{
		{"empty is unknown, never healthy", nil, StatusUnknown},
		{"all healthy", []Status{StatusHealthy, StatusHealthy}, StatusHealthy},
		{"one degraded", []Status{StatusHealthy, StatusDegraded}, StatusDegraded},
		{"contradictory is degraded", []Status{StatusHealthy, StatusContradictory}, StatusDegraded},
		{"one stale", []Status{StatusHealthy, StatusStale}, StatusStale},
		{"one unknown", []Status{StatusHealthy, StatusUnknown}, StatusUnknown},
		// Degraded outranks stale, and stale outranks unknown: the
		// worst thing about the Colony is what the report says.
		{"degraded outranks stale", []Status{StatusStale, StatusDegraded}, StatusDegraded},
		{"stale outranks unknown", []Status{StatusUnknown, StatusStale}, StatusStale},
		{"degraded outranks everything", []Status{StatusUnknown, StatusStale, StatusDegraded}, StatusDegraded},
		// An unrecognized status - one a newer managerd computed and
		// this build has never heard of - must not be treated as
		// healthy. It falls to the default arm, which is unknown.
		{"an unrecognized status is not healthy", []Status{StatusHealthy, Status("something-new")}, StatusUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AggregateStatus(tc.statuses); got != tc.want {
				t.Errorf("AggregateStatus(%v) = %q, want %q", tc.statuses, got, tc.want)
			}
		})
	}
}
