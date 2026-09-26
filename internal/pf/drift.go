package pf

import (
	"context"
	"fmt"
	"strings"
)

// Read-back: prove the load took effect, rather than assuming it did.
//
// `pfctl -a <anchor> -f -` returning success is a statement about
// pfctl's *parse and load*, and it is not, on its own, a statement
// about what the kernel is now enforcing. A load can be refused
// halfway, a ruleset can be edited out of band by hand, a host can be
// rebooted with a ruleset that never survived, and a future pfctl can
// change what it accepts. In every one of those cases `Apply` returns
// nil, the reconciler moves on, and the system has no idea that what it
// believes about the firewall and what the firewall is doing have
// diverged. Reading the running ruleset back and comparing it against
// the last-known-good record (see knowngood.go) is what turns that
// silence into evidence.
//
// The comparison is a pure function of two strings, so the whole model
// is testable with no pfctl, no network and no root - which is the
// only reason it is worth having on a codebase whose maintainers are on
// macOS.

// DriftState is what this node can honestly say about one anchor.
//
// The four values are the point. A two-valued "in sync / drifted" model
// would have to render "we could not read pf" as one of those two, and
// both choices lie: reporting a failed read as in-sync reports a
// firewall nobody has checked, and reporting it as drift reports a
// change nobody observed. So the silence gets its own state, and
// "nothing to compare against" gets another.
type DriftState string

const (
	// DriftInSync: the running ruleset was read back and canonicalised,
	// and it equals the last ruleset this node successfully loaded.
	// This is the ONLY state that may be presented as "enforced".
	DriftInSync DriftState = "match"

	// DriftDetected: the running ruleset was read back successfully and
	// differs from the last-known-good record. This is a positive
	// observation of a disagreement - someone hand-edited the anchor,
	// or a load only partly took, or the record is from before a
	// reboot that dropped it.
	DriftDetected DriftState = "drift"

	// DriftNotLoaded: this node has no last-known-good record for this
	// anchor, so there is no baseline to compare against. It says
	// nothing about whether pf is currently enforcing anything - only
	// that Apiary on this node never successfully loaded this anchor,
	// or has flushed it since.
	DriftNotLoaded DriftState = "not_loaded"

	// DriftUnknown: something prevented the comparison - pfctl missing
	// or failing, the record on disk being corrupt, the store being
	// unreadable. No evidence either way. Never rendered as in sync,
	// and never rendered as a specific kind of drift.
	DriftUnknown DriftState = "unknown"
)

// Drift is one read-back observation: what state the anchor is in, the
// two rulesets that were compared, and the record state the comparison
// was made against. It is a value, not an error - there is no caller
// that can forget to look at a returned struct, and there is no code
// path in which the observation is produced and then thrown away
// without someone having been handed the chance to act on it.
type Drift struct {
	Anchor    string
	State     DriftState
	Expected  string         // canonicalised last-known-good ruleset
	Observed  string         // canonicalised ruleset read back from pf
	KnownGood KnownGoodState // the record state this was judged against
	Detail    string         // why, in one line, for logs and a future UI
}

// InSync reports whether the ruleset is positively confirmed to match
// what this node last loaded. DriftUnknown and DriftNotLoaded are both
// false, deliberately: "we did not check" is not "it is fine".
func (d Drift) InSync() bool { return d.State == DriftInSync }

// String renders the state for a log line, a CLI, or a future UI row.
func (d Drift) String() string {
	switch d.State {
	case DriftInSync:
		return fmt.Sprintf("%s: in sync with the last ruleset this node loaded", d.Anchor)
	case DriftDetected:
		return fmt.Sprintf("%s: DRIFT - the running ruleset differs from the ruleset this node last loaded; pf is enforcing something Apiary did not write (%s)", d.Anchor, d.Detail)
	case DriftNotLoaded:
		return fmt.Sprintf("%s: not loaded - %s", d.Anchor, d.Detail)
	default:
		return fmt.Sprintf("%s: UNKNOWN - %s", d.Anchor, d.Detail)
	}
}

// Readback returns the ruleset pf is currently enforcing in anchor, as
// `pfctl -a <anchor> -sr` prints it.
//
// Two honest caveats, both of which are why the caller compares
// canonically rather than literally, and both of which are unverifiable
// from macOS and therefore recorded as open questions rather than
// asserted here: pfctl's -sr output format is not a stable API (it may
// resolve a port to a service name, or print `port = N` where we
// wrote `port N`, or print evaluation flags we did not write), and
// `pfctl -a <anchor> -sr` on an anchor that does not exist is reported
// by pf as an error rather than as empty output. The canonicaliser
// below is deliberately conservative - whitespace, comments, and blank
// lines only - so the failure direction of a format difference is a
// *reported* drift rather than a silently missed one. A testbed run
// against a real pf is what should widen it, and the widening is a
// change to one pure function.
func (m *Manager) Readback(ctx context.Context, anchor string) (string, error) {
	return m.exec_(ctx, "", pfctlBin, "-a", anchor, "-sr")
}

// CheckDrift reads the running ruleset back, compares it against this
// node's last-known-good record for anchor, and returns the verdict.
//
// It never returns an error, and never returns DriftInSync as a way of
// saying "I could not tell". Every failure - no record, an unreadable
// record, a missing pfctl - becomes a state the caller can display,
// because the caller's problem ("is this VM's firewall actually in
// effect?") is not improved by an error string it might log and
// ignore.
func (m *Manager) CheckDrift(ctx context.Context, anchor string) Drift {
	rec, state, err := m.knownGood().load(anchor)
	if err != nil {
		return Drift{Anchor: anchor, State: DriftUnknown, KnownGood: state, Detail: fmt.Sprintf("reading the last-known-good record: %v", err)}
	}
	switch state {
	case KnownGoodAbsent:
		return Drift{Anchor: anchor, State: DriftNotLoaded, KnownGood: state, Detail: "no last-known-good record for this anchor on this node, so there is nothing to compare the running ruleset against"}
	case KnownGoodCorrupt:
		return Drift{Anchor: anchor, State: DriftUnknown, KnownGood: state, Detail: rec.Detail}
	}
	return m.observe(ctx, anchor, rec.Body, state)
}

// observe is the shared comparison: read back, canonicalise both
// sides, classify. It is the post-load path of Apply as well as the
// on-demand path of CheckDrift - after a load, the ruleset that *should*
// be running is the one just written, which is the same string the
// record was about to be.
func (m *Manager) observe(ctx context.Context, anchor, expected string, kg KnownGoodState) Drift {
	observed, err := m.Readback(ctx, anchor)
	if err != nil {
		return Drift{
			Anchor:    anchor,
			State:     DriftUnknown,
			Expected:  canonicalRules(expected),
			KnownGood: kg,
			Detail:    fmt.Sprintf("reading the running ruleset back from pf: %v", err),
		}
	}
	got := canonicalRules(observed)
	want := canonicalRules(expected)
	if got == want {
		return Drift{Anchor: anchor, State: DriftInSync, Expected: want, Observed: got, KnownGood: kg, Detail: "the running ruleset matches the ruleset this node last loaded"}
	}
	return Drift{
		Anchor:    anchor,
		State:     DriftDetected,
		Expected:  want,
		Observed:  got,
		KnownGood: kg,
		Detail:    firstDifference(want, got),
	}
}

// LastDrift returns the most recent read-back observation this Manager
// made for anchor, if it has made one. Every Apply, ApplyNAT and
// CheckDrift on this node records its verdict here, so drift survives
// the call that found it instead of existing only for as long as the
// caller cared to log it.
func (m *Manager) LastDrift(anchor string) (Drift, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.lastDrift[anchor]
	return d, ok
}

// remember records an observation for later retrieval.
func (m *Manager) remember(d Drift) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastDrift == nil {
		m.lastDrift = make(map[string]Drift)
	}
	m.lastDrift[d.Anchor] = d
}

// forgetDrift drops a stored observation, on teardown of the thing the
// anchor belonged to.
func (m *Manager) forgetDrift(anchor string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.lastDrift, anchor)
}

// canonicalRules reduces a ruleset to a comparable form: per-line
// whitespace runs collapsed to single spaces, blank lines and pf
// comments dropped.
//
// It deliberately does NOT sort, and does not try to understand pf's
// output beyond whitespace. Two reasons, in order:
//
//   - Sorting would hide a real change. Rule order is pf's evaluation
//     order (ADR-0075, no `quick`, last match wins), so a ruleset whose
//     lines were reordered by hand *has* changed meaning, and a
//     set-comparison would call it in sync.
//   - Aggressive normalisation is how "in sync" quietly becomes "we
//     normalised both sides until they matched". Anything beyond
//     whitespace needs a real pf to test against, and macOS cannot
//     supply one.
func canonicalRules(body string) string {
	lines := make([]string, 0, strings.Count(body, "\n")+1)
	for _, line := range strings.Split(body, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// firstDifference describes a mismatch in a form an operator can act
// on: which side has a line the other does not, and which line.
func firstDifference(expected, observed string) string {
	exp := ruleLines(expected)
	obs := ruleLines(observed)
	for i := 0; i < len(exp) || i < len(obs); i++ {
		switch {
		case i >= len(obs):
			return fmt.Sprintf("line %d (%q) is loaded by Apiary but not running in pf", i+1, exp[i])
		case i >= len(exp):
			return fmt.Sprintf("line %d (%q) is running in pf but was not loaded by Apiary", i+1, obs[i])
		case exp[i] != obs[i]:
			return fmt.Sprintf("line %d differs: Apiary loaded %q, pf is enforcing %q", i+1, exp[i], obs[i])
		}
	}
	return "the rulesets differ only in comment or whitespace"
}

func ruleLines(body string) []string {
	if body == "" {
		return nil
	}
	return strings.Split(body, "\n")
}
