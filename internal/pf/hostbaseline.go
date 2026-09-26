package pf

import (
	"context"
	"fmt"
	"strings"
)

// The host's own ruleset: the half of a firewall's behaviour that
// internal/pf does not own and has never looked at.
//
// Everything this package does happens inside `anchor "apiary/*"`. Not
// one call anywhere in the tree loads the main ruleset - every write is
// `pfctl -a <anchor> -f -` or `pfctl -a <anchor> -F rules`, and the only
// host file it touches is the one-line `anchor "apiary/*"` stanza
// internal/install/checks.go verifies in /etc/pf.conf. That confinement
// is deliberate and load-bearing (ADR-0140), but it has a consequence
// that used to go entirely unexamined: everything about the host's
// *actual* filtering posture is somebody else's business, so Apiary had
// no idea whether the rules it wrote were being evaluated by anything
// capable of enforcing them.
//
// That gap is not hypothetical. Measured read-only on a live four-node
// cluster, /etc/pf.conf on a Combs was:
//
//	anchor "apiary/*"
//
// One line, the whole file. `pfctl -sr` printed exactly one rule -
// `anchor "apiary/*" all` - the `apiary` anchor existed and contained no
// rules, and there was no `set skip on lo`, no `set block-policy`, and no
// block rule anywhere. pf passes any packet that matches no rule, so on
// that host pf was *enabled* and filtering *nothing*: raftd (17600),
// managerd peer RPC (17700), the web UI (8080) and restshimd (8081) were
// all reachable from the LAN with no packet filtering, confirmed by
// probe rather than inferred. The compounding hazard was that the one
// place that could have noticed - `pfctl -s info`, whose stated intent in
// internal/hoststats was "confirming the firewall is actually enabled" -
// cheerfully reports Status: Enabled, and its counters look normal, on
// exactly that host. **pf being enabled is a statement about pfctl, not
// about filtering.** That is the defect this file exists to make
// visible, and it is why nothing below is ever allowed to answer
// "unknown" with anything reassuring.

// Observation is one read-only fact about a ruleset, and it has three
// values rather than two.
//
// The third value is the whole design, for the same reason
// DriftState has four: a boolean would have to render "pfctl did not
// tell us" as true or false, and both renderings lie. `present` on an
// unreadable ruleset asserts a firewall nobody has looked at;
// `absent` asserts a host posture nobody observed. So every fact read
// from the kernel here carries its own silence.
type Observation string

const (
	// Present: the fact was read and is there.
	Present Observation = "present"

	// Absent: the fact was read and is not there. This is a real
	// observation of a host that is less filtered than it looks, not a
	// failure to read.
	Absent Observation = "absent"

	// Unknown: the fact could not be read - pfctl missing, not
	// permitted, the anchor or the ruleset unreadable, or the output
	// shaped in a way this package does not recognise. Never a pass.
	Unknown Observation = "unknown"
)

// Bool renders an observation as a display flag: true only for Present.
// Absent and Unknown are both false, and that is the point - a caller
// that wants to tell them apart must read the Observation itself.
func (o Observation) Bool() bool { return o == Present }

// BaselineState is this node's honest verdict about the host's own
// packet filtering - the main ruleset, with Apiary's own anchors
// excluded from the claim.
//
// The states are ordered by how much they let a reader assume. The
// naming reuses the two words this codebase already has for exactly
// these two situations - internal/pf's `unknown` and
// internal/jailnet's `unfiltered`/`filtered` - rather than a parallel
// vocabulary; where the subject differs (jailnet's `unfiltered` is "this
// jail has no rules configured", this one is "this host's own ruleset
// cannot drop anything") the doc comments say so.
type BaselineState string

const (
	// BaselineFiltering: the host's own ruleset can drop traffic. It
	// carries a default block policy, or at least one block rule.
	// This is the ONLY state that may be presented as "the host is
	// filtering" - and even then it says the host *can* filter, never
	// that any particular packet was.
	BaselineFiltering BaselineState = "filtering"

	// BaselineUnfiltered: pf is enabled, and the host's own ruleset
	// cannot drop a packet at all - no default block policy, no block
	// rule. Every packet that Apiary's `apiary/*` anchors do not match
	// is passed, which on a measured live host meant the whole cluster
	// was reachable from the LAN.
	//
	// The precise claim, because it is easy to overstate: this is about
	// the host's *own* rules. A block rule Apiary loaded into
	// `apiary/<name>` is still enforced - it is a rule pf evaluates
	// like any other - so "unfiltered" here means "nothing outside
	// Apiary's anchors is filtered", and the corollary is the alarming
	// one: an empty Apiary anchor filters nothing at all.
	BaselineUnfiltered BaselineState = "unfiltered"

	// BaselineDisabled: pf itself is not enabled. Distinct from
	// unfiltered on purpose: the remedy is different, and neither of
	// them is ever a "the firewall is working" answer.
	BaselineDisabled BaselineState = "disabled"

	// BaselineUnknown: this node could not establish the host's
	// posture. pfctl missing, not permitted, output unparseable, or the
	// main ruleset unreadable. Silence, not health.
	BaselineUnknown BaselineState = "unknown"
)

// Filters reports whether this node can honestly say the host's
// filtering posture is the good one. False for unfiltered, false for
// disabled, and - deliberately - false for unknown: "we did not look" is
// not "it is fine", the same rule Drift.InSync() follows.
func (s BaselineState) Filters() bool { return s == BaselineFiltering }

// Baseline is one read-only assessment of the host's main ruleset: the
// individual facts, the verdict they add up to, and the raw ruleset they
// were read from (kept because a one-line /etc/pf.conf is the most
// damning evidence there is, and an operator should be able to see it).
//
// It is a value, not an error, for Drift's reason: there is no caller
// that can forget to look at a returned struct.
type Baseline struct {
	State BaselineState

	// PFEnabled: `pfctl -s info`'s Status line. Unknown when pfctl could
	// not be run or printed no recognisable Status - a case hoststats'
	// own parsePFInfo silently renders as `Enabled: false`, conflating
	// "pf is off" with "we could not ask".
	PFEnabled Observation

	// DefaultPolicy: a `set block-policy ...` directive in the main
	// ruleset. Without one, pf passes anything that matches no rule -
	// which is the entire measured hazard.
	DefaultPolicy Observation

	// BlockRule: at least one `block ...` rule in the main ruleset.
	// Distinct from DefaultPolicy on purpose. A host with `block
	// return in log` and no block policy still drops the traffic that
	// matches that rule and passes everything else, and calling that
	// "filtered" or "unfiltered" without saying which rule is the whole
	// ambiguity this struct exists to resolve.
	BlockRule Observation

	// SkipLoopback: `set skip on lo` (or `lo0` - same interface, both
	// spellings appear in the wild). Its absence means loopback
	// traffic walks the whole ruleset.
	SkipLoopback Observation

	// ApiaryAnchorReached: the main ruleset contains an anchor rule
	// covering the `apiary/*` namespace, which is the only thing that
	// makes the rules this package loads evaluate at all. Absent means
	// every rule Apiary has ever loaded is sitting in an anchor nothing
	// reaches - a green drift check on a completely inert ruleset.
	ApiaryAnchorReached Observation

	// MainRuleset is the main ruleset exactly as pfctl printed it, and
	// is empty when it could not be read.
	MainRuleset string

	// Detail is why, in one line, for logs and a future UI row.
	Detail string
}

// HostReads is the raw output of the two read-only commands this
// assessment is a pure function of. Both errors are carried rather than
// discarded, because "pfctl said nothing" and "pfctl was never asked"
// are the two halves of the same unknown and they deserve different
// words in Detail.
//
// Neither command mutates anything: `pfctl -s info` and `pfctl -sr`
// (no -a, so the main ruleset, not an anchor) are reads. This file
// contains no code path that can write to pf, and the ADR records that
// as a property of the design rather than an accident.
type HostReads struct {
	Info           string
	InfoErr        error
	MainRuleset    string
	MainRulesetErr error
}

// ClassifyHost turns the two raw reads into a verdict. It is pure, which
// is the only reason the whole model is testable on a machine with no
// pf, no root and no network - the same property drift.go has, and the
// reason this is a function and not a method.
func ClassifyHost(reads HostReads) Baseline {
	b := Baseline{
		State:               BaselineUnknown,
		PFEnabled:           parsePFStatus(reads.Info, reads.InfoErr),
		DefaultPolicy:       Absent,
		BlockRule:           Absent,
		SkipLoopback:        Absent,
		ApiaryAnchorReached: Absent,
		MainRuleset:         reads.MainRuleset,
	}
	if reads.MainRulesetErr != nil {
		// Every fact below is read out of the main ruleset, so a failed
		// read makes all four of them unknown. Guessing "absent" here
		// would manufacture a scary-looking host out of a permission
		// error, which is its own kind of lie.
		b.DefaultPolicy, b.BlockRule, b.SkipLoopback, b.ApiaryAnchorReached = Unknown, Unknown, Unknown, Unknown
	} else {
		b.DefaultPolicy = observation(hasDefaultBlockPolicy(reads.MainRuleset))
		b.BlockRule = observation(hasBlockRule(reads.MainRuleset))
		b.SkipLoopback = observation(hasSkipLoopback(reads.MainRuleset))
		b.ApiaryAnchorReached = observation(hasApiaryAnchorRule(reads.MainRuleset))
	}

	b.State, b.Detail = b.verdict()
	return b
}

// verdict is the one place a BaselineState is decided, so the rule is
// stated once and every path goes through it. The failure direction is
// chosen, not inherited: any unknown among the inputs yields
// BaselineUnknown rather than the reassuring answer, because the whole
// defect being fixed is a host that reports green while nothing filters.
func (b Baseline) verdict() (BaselineState, string) {
	switch b.PFEnabled {
	case Unknown:
		return BaselineUnknown, "pf's own status could not be read, so nothing can be said about what this host is filtering"
	case Absent:
		return BaselineDisabled, "pf is not enabled on this host, so no ruleset - Apiary's anchors included - is being evaluated"
	}
	if b.DefaultPolicy == Unknown || b.BlockRule == Unknown {
		return BaselineUnknown, "the host's main ruleset could not be read, so whether anything can be dropped is not established"
	}
	if b.DefaultPolicy == Present || b.BlockRule == Present {
		return BaselineFiltering, "the host's own ruleset carries a default block policy or a block rule, so it can drop traffic"
	}
	detail := "pf is enabled but the host's main ruleset has no default block policy and no block rule: every packet its rules do not match is passed, so the only thing on this host that can drop a packet is what Apiary has loaded into apiary/*"
	if b.ApiaryAnchorReached == Absent {
		detail += " - and the main ruleset does not reference that namespace at all, so nothing Apiary has loaded is being evaluated either"
	}
	return BaselineUnfiltered, detail
}

// String renders the verdict for a log line, a CLI, or a UI row. The
// subject is named in every branch, because "unfiltered" with no
// subject is how this whole file's problem started.
func (b Baseline) String() string {
	switch b.State {
	case BaselineFiltering:
		return fmt.Sprintf("host ruleset: filtering - %s", b.Detail)
	case BaselineUnfiltered:
		return fmt.Sprintf("host ruleset: UNFILTERED - %s", b.Detail)
	case BaselineDisabled:
		return fmt.Sprintf("host ruleset: pf DISABLED - %s", b.Detail)
	default:
		return fmt.Sprintf("host ruleset: UNKNOWN - %s", b.Detail)
	}
}

// AssessHost reads the running host's own ruleset and returns the
// verdict. It never returns an error, for the reason CheckDrift doesn't:
// the caller's question ("is this VM's firewall actually in effect?") is
// not improved by an error string it might log and ignore.
//
// The read is two pfctl invocations, both read-only. It is *not* run on
// every Apply: the main ruleset is a host-level fact that changes on a
// human's timescale, not once per reconcile tick per VM, so a caller
// asks for it deliberately (internal/hoststats, once per stats gather;
// the install preflight, once per run) and the verdict is a value the
// caller keeps.
func (m *Manager) AssessHost(ctx context.Context) Baseline {
	infoOut, infoErr := m.exec_(ctx, "", pfctlBin, "-s", "info")
	rulesOut, rulesErr := m.exec_(ctx, "", pfctlBin, "-sr")
	return ClassifyHost(HostReads{Info: infoOut, InfoErr: infoErr, MainRuleset: rulesOut, MainRulesetErr: rulesErr})
}

// Decisive reports whether this anchor's rules can actually change some
// packet's fate, given what the host's own ruleset would otherwise do.
//
// It is the answer to a question DriftInSync cannot answer by itself.
// DriftInSync is a statement about *text*: the ruleset pf is enforcing
// in this anchor equals the text this node loaded. That is a real and
// necessary guarantee, and it is satisfied identically on a host where
// the main ruleset never references `apiary/*` (so nothing evaluates the
// anchor) and on a host whose anchor holds only `pass` rules under a
// permissive default policy (so the traffic would have been permitted
// anyway). Both are "in sync". Neither is a firewall.
//
// The four conditions, each of which is an observation rather than an
// assumption:
//
//   - the anchor is in sync, and it is not empty (an empty anchor drops
//     nothing, which is the state a flushed or never-populated anchor is
//     in);
//   - pf is actually enabled - if it is not, no anchor rule is
//     evaluated, and "in sync" is a statement about text on a disk;
//   - the main ruleset reaches the `apiary/*` namespace - otherwise pf
//     never descends into the anchor at all;
//   - the rules can change an outcome: either they contain a block rule
//     of their own, or the host's default policy already drops
//     unmatched traffic and a `pass` rule is a hole in it. An anchor of
//     `pass` rules under a permissive default policy is the
//     "loaded, matching, and irrelevant" case, and it is the one that
//     has been invisible until now.
//
// Every false return is a "we did not establish that", never "nothing is
// wrong" - the same discipline as BaselineState.Filters.
func (d Drift) Decisive(b Baseline) bool {
	if !d.InSync() || d.Observed == "" {
		return false
	}
	if b.PFEnabled != Present {
		return false
	}
	if b.ApiaryAnchorReached != Present {
		return false
	}
	return hasBlockRule(d.Observed) || b.DefaultPolicy == Present
}

// Qualify renders the honest one-line qualification of a drift verdict
// given the host's baseline: what d.InSync() does *not* establish.
//
// It returns the empty string when the two together are a real
// guarantee, and a sentence the reader cannot misread as reassurance
// when they are not. Keeping this a pure function of two values is what
// lets it be tested for every combination of host posture and anchor
// state without a pf anywhere.
func (d Drift) Qualify(b Baseline) string {
	if d.Decisive(b) {
		return ""
	}
	if !d.InSync() {
		return "" // the drift verdict already says something worse than a caveat
	}
	switch {
	case d.Observed == "":
		return "this anchor is empty, so it drops nothing; an in-sync empty anchor is not a firewall"
	case b.PFEnabled != Present:
		return "pf is not confirmed enabled on this host, so nothing in this anchor is being evaluated"
	case b.ApiaryAnchorReached != Present:
		return "the host's main ruleset does not reference the apiary/* namespace, so these rules are loaded and never evaluated"
	case !hasBlockRule(d.Observed) && b.DefaultPolicy != Present:
		return "these rules only pass, and the host's own ruleset would have permitted the traffic anyway, so they change nothing"
	default:
		return ""
	}
}

// parsePFStatus reads the `Status:` line of `pfctl -s info`.
//
// It deliberately returns Unknown for a pfctl that produced no Status
// line, and for a status word this package does not recognise. A
// two-valued parse of that line is exactly the bug this file exists to
// fix: internal/hoststats' parsePFInfo answers "Disabled" for output it
// did not understand, so a pfctl this code has never seen renders as a
// host with its firewall off rather than as a host it could not ask.
func parsePFStatus(info string, err error) Observation {
	if err != nil {
		return Unknown
	}
	for _, line := range strings.Split(info, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 || fields[0] != "Status:" {
			continue
		}
		switch {
		case strings.EqualFold(fields[1], "Enabled"):
			return Present
		case strings.EqualFold(fields[1], "Disabled"):
			return Absent
		default:
			return Unknown
		}
	}
	return Unknown
}

// observation lifts a parsed yes/no into the three-valued form, which
// is the whole point: a successful read of "no" is Absent (a real fact
// about a host that is filtering nothing) and is allowed to be rendered
// as a negative finding, while the same boolean coming from a failed
// read would have been indistinguishable from it.
func observation(yes bool) Observation {
	if yes {
		return Present
	}
	return Absent
}

// statements splits a pf ruleset into the statements pf is evaluating,
// dropping blank lines and pf comments.
//
// Comments are dropped rather than parsed because a comment is not a
// rule: `# block everything` in a hand-maintained pf.conf, echoed by a
// pfctl that echoes comments, must not read as a block rule and
// manufacture a "filtering" verdict out of a sentence.
func statements(body string) []string {
	lines := make([]string, 0, strings.Count(body, "\n")+1)
	for _, line := range strings.Split(body, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// hasBlockRule reports whether body contains a rule that can drop a
// packet.
//
// The prefix test is on the whole first word, so `set block-policy
// drop` (a *default policy*, reported separately) is not counted as a
// block rule, and `block` is not matched inside `blockout` or a
// `pass ... "block"` string. Whether pfctl renders `set` directives in
// `-sr` output is a pfctl-format question this package cannot answer
// from macOS; the failure direction if it does not is a block policy
// that reads as absent, which is a *pessimistic* miss rather than a
// false "filtering", and the ADR records it as an open item for the
// testbed.
func hasBlockRule(body string) bool {
	for _, line := range statements(body) {
		if line == "block" || strings.HasPrefix(line, "block ") {
			return true
		}
	}
	return false
}

// hasDefaultBlockPolicy reports whether body carries a `set
// block-policy` directive, which is what makes pf drop packets that
// reach the end of the ruleset unmatched.
func hasDefaultBlockPolicy(body string) bool {
	for _, line := range statements(body) {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "set" && fields[1] == "block-policy" {
			return true
		}
	}
	return false
}

// hasSkipLoopback reports whether body exempts loopback from the
// ruleset. `lo` and `lo0` are the same interface under the two names
// FreeBSD has used, and both spellings are in the wild in hand-written
// pf.conf files, so both count. The directive is `set skip on <if>` -
// the `on` is part of the pf grammar, not a comment.
func hasSkipLoopback(body string) bool {
	for _, line := range statements(body) {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[0] == "set" && fields[1] == "skip" && fields[2] == "on" && (fields[3] == "lo" || fields[3] == "lo0") {
			return true
		}
	}
	return false
}

// hasApiaryAnchorRule reports whether the main ruleset contains an
// anchor rule that reaches the `apiary/*` namespace.
//
// A bare `anchor "apiary"` is deliberately NOT counted. pf anchors are
// a flat namespace and a rule matches an anchor by its full name, so
// `anchor "apiary"` evaluates rules loaded into the anchor literally
// named `apiary` and nothing in `apiary/<name>`. Only the wildcard
// `anchor "apiary/*"` - the exact stanza internal/install/checks.go
// verifies - is what makes the per-VM and per-jail anchors this package
// loads reachable, and the difference between the two is a firewall
// that does nothing versus a firewall that works.
func hasApiaryAnchorRule(body string) bool {
	for _, line := range statements(body) {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "anchor" {
			continue
		}
		if strings.Trim(fields[1], `"`) == "apiary/*" {
			return true
		}
	}
	return false
}
