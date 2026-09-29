package frontend

import (
	"fmt"
	"math"
)

// gaugeView is the rendered data for one radial Gauge (docs/web-ui-redesign.md
// Section 1). It is a pure presentation value: every number the template
// needs (the SVG stroke-dasharray/stroke-dashoffset pair, the rounded
// display percentage, which of the seven vocabulary states to color it
// with) is computed here, so _gauge.html does no arithmetic and cannot
// disagree with a test that exercises this file alone.
//
// State names one of the states documented in the redesign spec's
// Section 3.1 - "ok"/"warn"/"critical" for a real reading, "unknown"
// when there is nothing to show a percentage for at all. A Gauge never
// computes "not-applicable"/"stale"/"contradictory" itself: those are
// health-model verdicts a real evidence source assigns, not something
// derivable from a bare percentage.
type gaugeView struct {
	Label      string
	Percent    int
	Dash       string
	DashOffset string
	State      string
	Detail     string
}

// gaugeRadius is the SVG circle radius in user units, matching
// _gauge.html's viewBox. Both must be kept in the same units for the
// stroke-dasharray/stroke-dashoffset math below to draw the arc
// correctly - if the template's viewBox or radius ever changes, this
// constant has to change with it.
const gaugeRadius = 16.0

// gaugeWarnThreshold/gaugeCriticalThreshold are capacity/load
// thresholds, not a health verdict - a page with a real evidence-backed
// health computation for the same resource should use that instead of
// a gauge's percentage alone. These exist only for resources (CPU load,
// memory, disk capacity) that have no health verdict of their own to
// borrow, matching how host.html already reports these same numbers
// today without a verdict attached.
const (
	gaugeWarnThreshold     = 75.0
	gaugeCriticalThreshold = 90.0
)

// gaugeFromUsedTotal builds a Gauge from a used/total pair (e.g. memory
// used vs. total, or a ZFS pool's allocated vs. size bytes).
// total==0 is "no data available," never "0%" - a gauge that read 0%
// for a pool this code could not size would misreport an empty
// resource as a healthy, mostly-free one. It renders as the "unknown"
// state instead, matching Section 3.1's rule that unknown and a real
// zero must never look the same.
func gaugeFromUsedTotal(label string, used, total uint64, detail string) gaugeView {
	if total == 0 {
		return gaugeView{Label: label, State: "unknown", Detail: detail, Dash: gaugeDasharray(), DashOffset: gaugeDashOffset(0)}
	}
	pct := float64(used) / float64(total) * 100
	return gaugeFromPercent(label, pct, detail)
}

// gaugeFromLoadAverage builds a Gauge for CPU load expressed against a
// known core count - the one reading in this file with no natural
// percentage of its own (a load average of 1.5 means something
// different on 2 cores than on 16). cores<=0 is "unknown," not a
// divide-by-zero guess: a HostStats read that came back with no core
// count is missing evidence, not evidence of an idle machine.
func gaugeFromLoadAverage(label string, loadAvg1 float64, cores int32, detail string) gaugeView {
	if cores <= 0 {
		return gaugeView{Label: label, State: "unknown", Detail: detail, Dash: gaugeDasharray(), DashOffset: gaugeDashOffset(0)}
	}
	pct := loadAvg1 / float64(cores) * 100
	return gaugeFromPercent(label, pct, detail)
}

// gaugeFromPercent builds a Gauge directly from a percentage - for a
// reading that has no natural used/total pair, such as CPU load
// expressed as a fraction of cores.
func gaugeFromPercent(label string, pct float64, detail string) gaugeView {
	clamped := pct
	if clamped < 0 {
		clamped = 0
	}
	if clamped > 100 {
		clamped = 100
	}
	return gaugeView{
		Label:      label,
		Percent:    int(clamped + 0.5),
		Dash:       gaugeDasharray(),
		DashOffset: gaugeDashOffset(clamped),
		State:      gaugeState(clamped),
		Detail:     detail,
	}
}

// gaugeState maps an already-clamped [0,100] percentage to one of the
// three reading states a bare percentage can honestly produce.
func gaugeState(clampedPct float64) string {
	switch {
	case clampedPct >= gaugeCriticalThreshold:
		return "critical"
	case clampedPct >= gaugeWarnThreshold:
		return "warn"
	default:
		return "ok"
	}
}

// gaugeCircumference is the ring's full length in the same SVG user
// units as gaugeRadius.
func gaugeCircumference() float64 {
	return 2 * math.Pi * gaugeRadius
}

// gaugeDasharray is constant across every gauge (it does not depend on
// the percentage) - both numbers equal the full circumference, which is
// the standard SVG trick for drawing a dashed ring whose "dash" is the
// entire circle and whose "gap" is used, via stroke-dashoffset below,
// to reveal only the filled fraction of it.
func gaugeDasharray() string {
	c := gaugeCircumference()
	return fmt.Sprintf("%.3f %.3f", c, c)
}

// gaugeDashOffset computes how much of the ring to hide so that exactly
// pct percent of it appears filled, starting from the top (the
// template rotates the circle -90deg so 0% starts at 12 o'clock rather
// than SVG's default 3 o'clock).
func gaugeDashOffset(pct float64) string {
	c := gaugeCircumference()
	return fmt.Sprintf("%.3f", c*(1-pct/100))
}
