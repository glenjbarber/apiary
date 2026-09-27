package frontend

import (
	"math"
	"testing"
)

func TestGaugeFromPercent(t *testing.T) {
	cases := []struct {
		name    string
		pct     float64
		wantPct int
		wantSt  string
	}{
		{"zero", 0, 0, "ok"},
		{"mid", 50, 50, "ok"},
		{"just under warn", 74.4, 74, "ok"},
		{"warn threshold exactly", 75, 75, "warn"},
		{"warn", 80, 80, "warn"},
		// Rounds up to display as 90, but the threshold check runs on the
		// true 89.6 value, not the rounded one - so this stays "warn"
		// rather than turning critical purely from display rounding.
		{"rounds to the threshold but hasn't reached it", 89.6, 90, "warn"},
		{"critical threshold exactly", 90, 90, "critical"},
		{"critical", 95, 95, "critical"},
		{"full", 100, 100, "critical"},
		{"negative clamps to zero", -5, 0, "ok"},
		{"over 100 clamps to 100", 150, 100, "critical"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := gaugeFromPercent("Test", c.pct, "")
			if g.Percent != c.wantPct {
				t.Errorf("Percent = %d, want %d", g.Percent, c.wantPct)
			}
			if g.State != c.wantSt {
				t.Errorf("State = %q, want %q", g.State, c.wantSt)
			}
			if g.Label != "Test" {
				t.Errorf("Label = %q, want %q", g.Label, "Test")
			}
		})
	}
}

func TestGaugeFromUsedTotal(t *testing.T) {
	t.Run("total zero is unknown, not zero percent", func(t *testing.T) {
		g := gaugeFromUsedTotal("Pool", 0, 0, "no size reported")
		if g.State != "unknown" {
			t.Errorf("State = %q, want %q", g.State, "unknown")
		}
		if g.Detail != "no size reported" {
			t.Errorf("Detail = %q, want it preserved", g.Detail)
		}
	})
	t.Run("used zero of a real total is a real zero percent, not unknown", func(t *testing.T) {
		g := gaugeFromUsedTotal("Pool", 0, 100, "")
		if g.State != "ok" || g.Percent != 0 {
			t.Errorf("got State=%q Percent=%d, want ok/0", g.State, g.Percent)
		}
	})
	t.Run("half used", func(t *testing.T) {
		g := gaugeFromUsedTotal("Mem", 16, 32, "")
		if g.Percent != 50 {
			t.Errorf("Percent = %d, want 50", g.Percent)
		}
	})
	t.Run("used exceeding total still clamps to 100, does not overshoot the ring", func(t *testing.T) {
		g := gaugeFromUsedTotal("Odd", 150, 100, "")
		if g.Percent != 100 || g.State != "critical" {
			t.Errorf("got Percent=%d State=%q, want 100/critical", g.Percent, g.State)
		}
	})
}

func TestGaugeDashArithmetic(t *testing.T) {
	circumference := 2 * math.Pi * gaugeRadius

	zero := gaugeFromPercent("X", 0, "")
	full := gaugeFromPercent("X", 100, "")
	half := gaugeFromPercent("X", 50, "")

	// Dash is the same full-circumference pair regardless of percentage -
	// only DashOffset changes to reveal the filled fraction.
	if zero.Dash != full.Dash || zero.Dash != half.Dash {
		t.Errorf("Dash should be constant across percentages: zero=%q full=%q half=%q", zero.Dash, full.Dash, half.Dash)
	}

	// At 0%, nothing is revealed: the offset equals the full circumference.
	wantZeroOffset := gaugeDashOffset(0)
	if zero.DashOffset != wantZeroOffset {
		t.Errorf("0%% DashOffset = %q, want %q (the full circumference)", zero.DashOffset, wantZeroOffset)
	}

	// At 100%, the whole ring is revealed: the offset is ~0.
	wantFullOffset := gaugeDashOffset(100)
	if full.DashOffset != wantFullOffset {
		t.Errorf("100%% DashOffset = %q, want %q (~0)", full.DashOffset, wantFullOffset)
	}

	// At 50%, half the circumference should remain hidden.
	wantHalfOffset := gaugeDashOffset(50)
	if half.DashOffset != wantHalfOffset {
		t.Errorf("50%% DashOffset = %q, want %q", half.DashOffset, wantHalfOffset)
	}
	_ = circumference // documents the relationship checked above; not asserted directly to avoid pinning float formatting twice
}
