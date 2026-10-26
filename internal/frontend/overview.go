package frontend

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

var _ = fmt.Sprintf
var _ = sort.Strings
var _ = strings.TrimSpace
var _ = time.Now

// The Blend overview (the "Apiary: UI Redesign" handoff's section 7).
//
// WHAT THIS FILE IS. The view model behind the new landing page: a
// status line, a "Needs attention" list, and the Combs section in its
// three views (Table, Cards, Matrix). It is a separate file from
// cluster_overview.go because that file is the OLD Command Center's
// evidence machinery, and this one is a new, much smaller surface built
// on top of it.
//
// THE ONE RULE THAT SHAPES EVERY FIELD HERE.
//
// "Never guess, and never show a missing value as zero or ok."
//
// Every value on this page is either OBSERVED or it is explicitly
// unknown. There is no third option. A Combs count that could not be
// read is "unknown", not "0". A CPU percentage that could not be
// computed is "unknown", not "0%". A ZFS pool that was not read is
// "unknown", not "free".
//
// That is why almost every field below is a struct with a Value, a
// Known flag, and a Detail - rather than a plain string. The template
// cannot render a number as if it were real, because the template does
// not get a number unless the number was observed.

// The seven states of the status vocabulary (handoff section 9). Every
// status surface in the new UI uses exactly these, and each must look
// different without relying on colour alone.
const (
	statusOK            = "ok"
	statusWarn          = "warn"
	statusCritical      = "critical"
	statusStale         = "stale"
	statusUnknown       = "unknown"
	statusNotApplicable = "not-applicable"
	statusContradictory = "contradictory"
)

// statusLabel names each state in words. The label is the text
// equivalent every status dot carries, so a dot is never the only cue -
// this project's own rule, and the handoff's section 12.
var statusLabel = map[string]string{
	statusOK:            "OK",
	statusWarn:          "Warning",
	statusCritical:      "Critical",
	statusStale:         "Stale",
	statusUnknown:       "Unknown",
	statusNotApplicable: "Not applicable",
	statusContradictory: "Contradictory",
}

// statusOrder is the fixed order the seven states appear in the
// matrix legend and in any grouped count, worst first.
var statusOrder = []string{
	statusCritical, statusContradictory, statusWarn, statusStale,
	statusUnknown, statusNotApplicable, statusOK,
}

// The resource thresholds (handoff section 7.3). They live here, in one
// place, as named constants - so a threshold is changed in one place and
// every surface that uses it changes together.
const (
	// cpuWarnPct is the CPU load percentage at which a Comb is
	// reported as over its resource threshold.
	cpuWarnPct = 85.0
	// memWarnPct is the memory usage percentage at the same threshold.
	memWarnPct = 85.0
	// zfsWarnPct is the ZFS pool capacity percentage at the same
	// threshold.
	zfsWarnPct = 85.0
)

// overviewMetric is one item in the status line.
//
// Value is the rendered value, or "" when nothing was observed.
// Known says which. Detail is the reason, carried into the title
// attribute so a reader can ask WHY a value is unknown rather than
// being left with a bare "unknown".
type overviewMetric struct {
	Label  string
	Value  string
	Known  bool
	Detail string
	// Warn marks a value that is over its threshold - the one
	// exception to "known means fine".
	Warn bool
	// Muted is used for a value that is known but not a headline
	// (the warnings counter's own styling).
	Muted bool
}

// overviewStatusLine is the whole status line.
type overviewStatusLine struct {
	Metrics []overviewMetric
	// Stale marks the line as showing values that could not be
	// refreshed. The handoff is explicit that a failed refresh must
	// leave the last content on screen and mark the values stale
	// rather than blanking them.
	Stale bool
	// StaleDetail explains why the line is stale.
	StaleDetail string
}

// overviewHealthChip is the overall health chip in the top bar.
//
// The handoff's rules (section 7.1) are specific and slightly
// counterintuitive, so they are implemented exactly as written:
//   - "Healthy" (green) when nothing is critical, contradictory, or
//     unknown. A WARNING does not turn the chip amber - warnings are
//     counted in the status line instead.
//   - "Attention" (red) when anything is critical or contradictory.
//   - "Unknown" (hollow grey) when the Colony itself has no valid
//     observation.
type overviewHealthChip struct {
	State  string
	Label  string
	Detail string
}

// overviewAttentionItem is one row of the "Needs attention" list.
//
// The handoff wants one sentence with the key noun in bold, a faint
// detail, and one action link. The template does the bolding from
// Subject and Detail; this carries the parts.
type overviewAttentionItem struct {
	// State is the seven-state vocabulary value driving the dot.
	State string
	// Subject is the key noun - the thing that needs attention,
	// rendered bold.
	Subject string
	// Sentence is the rest of the line.
	Sentence string
	// Detail is the faint parenthetical (for example "last sync 4m 12s
	// ago").
	Detail string
	// Action is the label of the action link.
	Action string
	// Href is where the action link goes. It is always a page or form
	// that already exists - the handoff forbids new action endpoints
	// in this phase.
	Href string
}

// overviewView is one Comb's row in the Combs section, in all three
// views. It is ONE struct rendered three ways, so the three views can
// never disagree about a Comb's state.
type overviewView struct {
	NodeID string
	// Href is the Comb's own page.
	Href string
	// State is the seven-state value for this Comb overall.
	State string
	// StateDetail explains the state.
	StateDetail string
	// IsLeader marks the observed Colony leader.
	IsLeader bool
	// Role is "Leader" or "Follower".
	Role string
	// CPU/Memory/ZFS are the three meters. Each carries the same
	// struct so an unreadable one reads as unknown rather than 0%.
	CPU    overviewMeter
	Memory overviewMeter
	ZFS    overviewMeter

	// Observed says whether this Comb's OWN readings were actually
	// observed.
	//
	// It is false when this frontend's own host data was returned
	// for a Comb that is NOT this frontend's own Comb - which is
	// what happens when peer forwarding is not configured, because
	// the request is then answered by this Comb's own colocated
	// managerd. That answer is this Comb's data wearing another
	// Comb's name, and putting it on another Comb's row would be
	// false evidence. So the overview refuses it and says unknown.
	//
	// The Colony update page deliberately keeps the OLD behaviour for
	// its own rows: it names which Comb is nominated, and its rows
	// answer a different question. This flag scopes the refusal to
	// the overview, where the meters are the whole point.
	Observed bool
	// Cells is the Comb's cells, as chips.
	Cells []overviewCell
	// CellCount is how many cells this Comb owns.
	CellCount int
	// Uptime is the Comb's uptime, if it could be observed.
	//
	// IT IS EMPTY IN PHASE 1, AND THE COLUMN SAYS SO. No RPC in this
	// codebase carries a host's uptime - the HostStats response has
	// load averages and memory and pools, but nothing that says how
	// long the machine has been up. The handoff's rule is that a
	// value the server cannot compute renders as "unknown" and never
	// as a guess, so the column is present (the design calls for it)
	// and reads "unknown" rather than showing an invented number.
	//
	// The field exists so that adding a real uptime reading later is
	// a change to ONE place rather than a change to the template.
	Uptime string
	// UptimeKnown says whether Uptime holds a real reading.
	UptimeKnown bool
	// UptimeDetail explains why the uptime is not known.
	UptimeDetail string

	// OS is the Comb's FreeBSD release, if it could be observed.
	//
	// Like the uptime, it is empty in Phase 1 for the same reason:
	// the base system can be read for the Comb this frontend is
	// COLOCATED with (HostPackages), but there is no peer-forwarded
	// read for any other Comb, and showing this Comb's release on
	// every row would be a lie about the others.
	OS       string
	OSKnown  bool
	OSDetail string
	// Checks are the matrix columns for this Comb. Only checks with a
	// real evidence source are present - see overviewCheck.
	Checks []overviewCheck
	// DigestLabel/DigestDetail/DigestClass carry this Comb's own FSM
	// state digest verdict (ADR-0143). The verdict is a per-Comb
	// fact - "this Comb's state digest agrees with the others" - so
	// it belongs on the Comb's row, where the reader can compare it
	// with its neighbours'.
	DigestLabel  string
	DigestDetail string
	DigestClass  string

	// CauseHeadline/CauseDetail are the Comb's own cause line: the
	// one-line summary of WHY this Comb is in the state it is in, and
	// the verdict it comes from.
	//
	// It is the same cause the old Command Center's per-Comb card
	// carried (ADR-0144's cause line), and it is on the Comb's own
	// evidence page in full - this is the summary, not the evidence.
	// The rule it obeys is unchanged and still enforced by
	// clusterNodeEvidence: a healthy Comb's cause badge is relabelled
	// "evidence current" so the word "healthy" is not printed twice
	// for the same Comb on the same line.
	CauseHeadline string
	CauseBadge    string
	CauseLabel    string
	// CauseState is the cause's own state, in the cause vocabulary
	// (never_observed, observed_healthy, observed_failed,
	// not_applicable) - NOT the seven-state overview vocabulary.
	CauseState string

	// Suffrage is the Comb's raft membership: "Voter", "Nonvoter",
	// "Staging", or "" when it was not observed.
	//
	// It is MEMBERSHIP, not a health verdict, and it is carried here
	// because ADR-0144's Comb tree was where an operator could see
	// it. The new rail replaced that tree, so the fact has to
	// survive somewhere - and the Combs table is where a reader
	// already looks at a Comb's row.
	Suffrage string
}

// overviewMeter is one labelled meter: a percentage and a thin bar.
type overviewMeter struct {
	Label string
	// Percent is the reading, 0-100. Only meaningful when Known.
	Percent int
	// Known says whether the reading was observed at all.
	Known bool
	// Detail explains the reading, or why there is none.
	Detail string
	// Over is the threshold this value is compared against. It is
	// carried so the template does no arithmetic and no threshold
	// constants live in markup.
	Over bool
	// Warn marks the value as over its threshold.
	Warn bool
}

// overviewCell is one cell chip on a Comb's card.
type overviewCell struct {
	ID   string
	Kind string
	// Stopped marks a cell that is not running. A stopped cell is
	// struck through in faint text.
	Stopped bool
	// Href is the cell's page.
	Href string
}

// overviewCheck is one matrix cell: a single check's state for a single
// Comb.
type overviewCheck struct {
	// Name is the column header.
	Name string
	// State is the seven-state value.
	State string
	// Detail is the hover title's detail text.
	Detail string
}

// overviewCombView is the Combs section: the set of Combs plus which
// view is selected.
type overviewCombView struct {
	Combs []overviewView
	// View is the selected view: "table", "cards", or "matrix".
	View string
	// DigestColony is the colony-wide FSM state agreement verdict
	// (ADR-0143). It is carried here so the new overview keeps
	// showing it - the old Command Center showed it, and dropping a
	// verdict from the page that replaced it would be a regression,
	// not a simplification.
	DigestColony stateDigestColony
	// DigestLabel is the colony-level badge's wording.
	DigestLabel string
}

// The three Combs views. The names are the cookie's values too, so
// there is exactly one spelling of each anywhere.
const (
	combViewTable  = "table"
	combViewCards  = "cards"
	combViewMatrix = "matrix"
)

// combViews is the ordered list of the three views, used by the
// segmented control.
var combViews = []string{combViewTable, combViewCards, combViewMatrix}

// combViewLabel names each view for the segmented control.
var combViewLabel = map[string]string{
	combViewTable:  "Table",
	combViewCards:  "Cards",
	combViewMatrix: "Matrix",
}

// normalizeCombView maps a requested view name onto a real one.
//
// An invalid or missing value falls back to Table. It NEVER falls back
// to a view that is not in the list, and it never produces an empty
// string, because an empty view name would render a page with no Comb
// section at all and look like there are no Combs.
func normalizeCombView(name string) string {
	for _, v := range combViews {
		if v == name {
			return v
		}
	}
	return combViewTable
}

// statusLegendEntry is one entry of the matrix's legend: a state and
// the word that names it.
//
// The legend shows ALL SEVEN states, always - including the ones no
// Comb currently has. A legend that only listed the states present
// would change shape as the Colony changed, and a reader could not
// tell a missing state from a state that has never occurred.
type statusLegendEntry struct {
	State string
	Label string
}
