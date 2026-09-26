package pf

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// DefaultKnownGoodDir is where the node-local last-known-good records
// live. /var/db is the FreeBSD convention for exactly this kind of
// mutable local state (dnsmasq's lease file, pf's own state, Sendmail's
// queue), and it is deliberately outside /etc, which this project does
// not manage and which is a host prerequisite it refuses to own.
const DefaultKnownGoodDir = "/var/db/apiary/pf"

// pfctlBin is the one external tool this package drives. Named once so
// the read-back, the load, and the flush cannot drift apart on the
// spelling.
const pfctlBin = "pfctl"

// Manager applies and clears per-VM pf(8) anchor rulesets via pfctl(8),
// and keeps the evidence needed to tell whether a load actually took.
//
// It is not a zero-sized struct any more (it was, until ADR-0137): it
// holds a mutex for the drift observations, the last-known-good
// directory, and the exec seam tests substitute. Use a pointer; the
// zero value is still valid and means "the real pfctl, the default
// last-known-good directory, read-back on".
type Manager struct {
	// KnownGoodDir overrides DefaultKnownGoodDir. Empty means the
	// default. Tests set it to a temp dir; production leaves it alone.
	KnownGoodDir string

	// DisableReadback turns off the post-load read-back verification.
	// It exists for one case: a host where `pfctl -a <anchor> -sr` is
	// known to be unavailable or prohibitively slow, where every
	// reconcile tick would otherwise pay for a second pfctl exec per
	// anchor. It is a performance valve only - drift is
	// undetectable while it is set, so the last-known-good record is
	// still written, and CheckDrift still works if called explicitly.
	DisableReadback bool

	mu        sync.Mutex
	lastDrift map[string]Drift

	// exec is the seam the tests use to stand in for pfctl. nil means
	// the real thing (runCmdStdin). Never set from outside the
	// package.
	exec func(ctx context.Context, stdin, name string, args ...string) (string, error)
}

func (m *Manager) knownGood() knownGoodStore {
	dir := m.KnownGoodDir
	if dir == "" {
		dir = DefaultKnownGoodDir
	}
	return knownGoodStore{dir: dir}
}

func (m *Manager) exec_(ctx context.Context, stdin, name string, args ...string) (string, error) {
	if m.exec != nil {
		return m.exec(ctx, stdin, name, args...)
	}
	return runCmdStdin(ctx, stdin, name, args...)
}

// Apply replaces anchor's entire ruleset with rules (rendered via
// RenderRules), the same full-replace-not-diff convention
// internal/hast's WriteConfig already uses for hast.conf. No rules
// means an empty anchor, i.e. everything allowed - matching this
// project's de facto behavior before firewall support existed.
//
// Three things happen on the way through, in this order, and the order
// is the design (ADR-0137):
//
//  1. Render, and refuse to render anything with an undeclared or
//     malformed scope. Nothing reaches pfctl until the whole ruleset is
//     known-good as text.
//  2. Load it.
//  3. Only if the load succeeded, record it as this node's
//     last-known-good ruleset for that anchor, atomically.
//
// A failure to record after a successful load is returned as an error
// even though pf is already enforcing the ruleset: the load happened,
// and saying so plainly is better than reporting success while the
// baseline that every later drift check depends on is missing.
func (m *Manager) Apply(ctx context.Context, anchor string, rules []Rule) error {
	body, err := RenderRules(rules)
	if err != nil {
		return err
	}
	if _, err := m.exec_(ctx, body, pfctlBin, "-a", anchor, "-f", "-"); err != nil {
		return err
	}
	if err := m.knownGood().record(anchor, body); err != nil {
		return fmt.Errorf("pf: ruleset for anchor %s was loaded, but its last-known-good record could not be written: %w", anchor, err)
	}
	m.verify(ctx, anchor, body)
	return nil
}

// verify is the post-load read-back. It records its verdict in the
// Manager so LastDrift can report it later; it does not fail the load,
// because a textually-different `pfctl -sr` rendering of a ruleset that
// did load is not evidence the operator should be told the firewall is
// down, and because the reconciler cannot act on a transient read-back
// failure any better than it could act on the original load - it
// re-applies next tick. Drift is surfaced, not swallowed: CheckDrift
// and LastDrift both expose it, and nothing here logs-and-drops it.
func (m *Manager) verify(ctx context.Context, anchor, body string) {
	if m.DisableReadback {
		return
	}
	m.remember(m.observe(ctx, anchor, body, KnownGoodLoaded))
}

// Flush removes every rule from anchor - used when a VM is torn down,
// so its anchor doesn't linger with stale rules referencing a VM that
// no longer exists. pf(8) anchors are created lazily on first use
// (Apply), so an anchor that was never populated - or whose rules a
// previous, partially-completed teardown already flushed - genuinely
// has nothing to flush; pfctl reports that case as "No such anchor",
// which is treated as success here rather than an error, the same
// idempotent-teardown posture this project already applies to
// PurgeVM/DeleteVM (a resource already gone is not a failure).
//
// It also forgets the anchor's last-known-good record: a flushed
// anchor's last-known-good ruleset *is* the empty one, and the honest
// encoding of "the baseline is the empty ruleset" here is "there is no
// baseline", which is the same state a never-loaded anchor is in.
func (m *Manager) Flush(ctx context.Context, anchor string) error {
	_, err := m.exec_(ctx, "", pfctlBin, "-a", anchor, "-F", "rules")
	if err != nil && !strings.Contains(err.Error(), "No such anchor") {
		return err
	}
	if ferr := m.knownGood().forget(anchor); ferr != nil {
		return fmt.Errorf("pf: anchor %s was flushed, but its last-known-good record could not be removed: %w", anchor, ferr)
	}
	m.forgetDrift(anchor)
	return nil
}

// ApplyNAT installs one outbound-NAT rule in anchor so subnet's own
// traffic reaches the real internet through uplink (the node's own
// physical interface, already connected to a normal internet-routed
// LAN) - the same "isolated network gets outbound access via NAT
// through the host's own uplink" a home router provides, done here so
// an Apiary-managed network never needs an external router of its own
// (see ADR-0048; this replaces the ExternalGateway/shared-VLAN
// approach from ADR-0047 for the common case of a node with its own
// working internet connection). Uses the same modern `match ... nat-to`
// form already proven to work in this project's own hand-configured
// firewall reference config, not the older two-pass `nat` ruleset -
// both are valid pf syntax, but this one loads through the same
// `pfctl -a anchor -f -` single-pass path Apply already uses. Full-
// replace, not diff, matching Apply's own idempotent-reapply-every-tick
// convention - safe to call unconditionally every reconcile tick.
//
// The NAT line names its own scope already (`on <uplink> from <subnet>
// to any`) and is a match/NAT rule rather than a filter rule, so it is
// outside the ADR-0137 scoping vocabulary by construction; it is
// recorded and read back exactly like a filter ruleset, because it
// occupies the same anchor and a partial load is just as invisible
// there.
func (m *Manager) ApplyNAT(ctx context.Context, anchor, subnet, uplink string) error {
	// Defense in depth: uplink is node-config data validated at
	// UpdateNodeConfig (internal/nodeconfig.validate) or a managerd
	// startup flag - but this function has no way to know that
	// validation ran, so it rejects a newline itself rather than
	// interpolating it unchecked into a ruleset pfctl then loads.
	if strings.ContainsAny(uplink, "\n\r") || strings.ContainsAny(subnet, "\n\r") {
		return fmt.Errorf("pf: uplink/subnet must not contain newlines")
	}
	body := fmt.Sprintf("match out on %s from %s to any nat-to (%s)\n", uplink, subnet, uplink)
	if _, err := m.exec_(ctx, body, pfctlBin, "-a", anchor, "-f", "-"); err != nil {
		return err
	}
	if err := m.knownGood().record(anchor, body); err != nil {
		return fmt.Errorf("pf: NAT rule for anchor %s was loaded, but its last-known-good record could not be written: %w", anchor, err)
	}
	m.verify(ctx, anchor, body)
	return nil
}
