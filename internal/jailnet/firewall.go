package jailnet

import (
	"context"
	"fmt"

	"github.com/glenjbarber/apiary/internal/pf"
)

// AnchorPrefix is the pf(8) anchor namespace every jail's rules live
// under, mirroring internal/pf's own per-VM anchor convention (see
// cluster's vmAnchor) exactly: one anchor per resource, named from the
// resource's own id, so a jail's ruleset can be replaced or flushed
// without touching any other jail's or any VM's.
//
// The "jailnet" prefix rather than a bare "jail" is deliberate: pf
// anchors are a flat global namespace on the host, and a name that
// could collide with a VM's or an operator's own anchor is a name that
// will eventually collide.
const AnchorPrefix = "apiary/jail/"

// Anchor returns the pf(8) anchor name for one jail. Exported because
// the reconciler that tears a jail down, the UI that shows whether a
// jail is filtered, and an operator at a pfctl prompt all need to
// compute the same name, and three copies of that string is three
// chances to disagree.
func Anchor(jailID string) string {
	return AnchorPrefix + jailID
}

// Firewall is the jail half of ADR-0117's firewalling stage: it renders
// and loads one jail's pf rules into that jail's own anchor, and
// flushes it again on teardown.
//
// It reuses internal/pf's existing Rule type and RenderRules verbatim
// rather than inventing a second ruleset language. That is the whole
// argument for it being a small package: a VNET jail with rules should
// be filtered by exactly the same syntax an operator already knows from
// a VM, compiled by exactly the same renderer, into exactly the same
// `pfctl -a <anchor> -f -` mechanism - so there are no new pf concepts
// in this design at all, only the existing ones pointed at a jail.
//
// What is *not* here, and why, is worth stating plainly: there is no
// JailDefinition.firewall_rules field to read the rules from. That field
// is a change to api/internalpb/state.proto and api/rpc/manager.proto,
// which are owned elsewhere; the ADR already names it as the intended
// extension. Until it exists, every jail's rule set is empty, so this
// package's output is "no rules configured" - which is the same
// unfiltered posture an unfiltered VM has today, and is *not* the same
// as "Apiary checked and found no firewall needed". FirewallStatus
// below reports those two situations differently on purpose.
type Firewall struct {
	// PF loads a rendered ruleset into an anchor. nil means the real
	// pfctl, which is the only correct setting in production.
	PF PFLoader
}

// PFLoader is the subset of *pf.Manager this package needs. *pf.Manager
// satisfies it unchanged.
type PFLoader interface {
	Apply(ctx context.Context, anchor string, rules []pf.Rule) error
	Flush(ctx context.Context, anchor string) error
}

// FirewallStatus is what Apiary can honestly say about one jail's
// filtering. The distinction between the last two values is the whole
// point, and it is the same distinction internal/cluster/simulate.go
// draws between "checked and fine" and "could not check".
type FirewallStatus string

const (
	// FirewallStatusUnfiltered means no rules are configured for this
	// jail, so its traffic is not filtered. This is a definite answer
	// about the configuration, not a claim that the jail is exposed:
	// a jail with no rules still only reaches its own network's
	// bridge, exactly like an unfiltered VM.
	FirewallStatusUnfiltered FirewallStatus = "unfiltered"

	// FirewallStatusFiltered means rules are configured and the most
	// recent load of them succeeded.
	FirewallStatusFiltered FirewallStatus = "filtered"

	// FirewallStatusUnknown means the anchor's current contents could
	// not be read, or the last load's outcome could not be confirmed.
	// Never collapsed into either of the two above: "Apiary did not
	// look" is not "there is no firewall", and an operator reading a
	// green "unfiltered" on a node whose pfctl is broken would be
	// misled about a security-relevant fact.
	FirewallStatusUnknown FirewallStatus = "unknown"
)

// Apply loads rules into jailID's anchor, replacing whatever was there.
// An empty rule set is a no-op rather than a flush, because "this jail
// has no rules" and "this jail's rules were removed" are different
// events and only one of them is an instruction.
//
// It returns the status it can honestly claim: FirewallStatusFiltered
// only when pfctl itself reported success, and FirewallStatusUnknown -
// never FirewallStatusUnfiltered - when the load failed. Reporting a
// failed load as "unfiltered" would be technically true and
// operationally disastrous, since the operator would read it as
// "nothing is protecting this jail" when the truth is "Apiary thinks
// something is protecting this jail and is wrong".
func (f *Firewall) Apply(ctx context.Context, jailID string, rules []pf.Rule) (FirewallStatus, error) {
	if jailID == "" {
		return FirewallStatusUnknown, fmt.Errorf("no jail id, so there is no pf anchor to load rules into")
	}
	if len(rules) == 0 {
		return FirewallStatusUnfiltered, nil
	}
	if f.PF == nil {
		return FirewallStatusUnknown, fmt.Errorf("no pf support is configured on this node, so jail %q's rules could not be loaded", jailID)
	}
	// RenderRules is pure and rejects a malformed rule outright, so a
	// bad rule is a loud local error before anything is loaded - not a
	// half-applied ruleset left behind on the host.
	if _, err := pf.RenderRules(rules); err != nil {
		return FirewallStatusUnknown, fmt.Errorf("jail %q's firewall rules do not render: %w", jailID, err)
	}
	if err := f.PF.Apply(ctx, Anchor(jailID), rules); err != nil {
		return FirewallStatusUnknown, fmt.Errorf("loading rules into jail %q's pf anchor %s: %w", jailID, Anchor(jailID), err)
	}
	return FirewallStatusFiltered, nil
}

// Flush empties jailID's anchor, and is what teardown calls. It is
// idempotent: an anchor that is already empty, or never existed, is
// not an error, because teardown runs again after a partial attempt
// and must not fail on the part that already succeeded.
func (f *Firewall) Flush(ctx context.Context, jailID string) error {
	if jailID == "" || f.PF == nil {
		return nil
	}
	if err := f.PF.Flush(ctx, Anchor(jailID)); err != nil {
		return fmt.Errorf("flushing jail %q's pf anchor %s: %w", jailID, Anchor(jailID), err)
	}
	return nil
}

// JailRules converts a jail's configured rules into pf rules. It takes
// a slice of the neutral shape rather than the proto type directly, so
// this package stays independent of the wire schema the same way
// internal/pf's own Rule does.
//
// Until JailDefinition gains a firewall_rules field, callers pass an
// empty slice and every jail is FirewallStatusUnfiltered. This function
// exists anyway so the wiring, when the field lands, is a call and not
// a change.
func JailRules(configured []JailRule) []pf.Rule {
	out := make([]pf.Rule, 0, len(configured))
	for _, r := range configured {
		out = append(out, pf.Rule{
			Direction: r.Direction,
			Action:    r.Action,
			Protocol:  r.Protocol,
			PortRange: r.PortRange,
			// ADR-0137's explicit opt-in for an any-to-any rule. A
			// JailRule has no interface or address field to narrow
			// with (JailRule is field-for-field api/internalpb's
			// FirewallRule, and ADR-0129 owns adding the scope fields
			// to that proto message), so the honest thing today is to
			// say out loud that the rule is about everything rather
			// than let an undeclared scope mean it by accident.
			Any: true,
		})
	}
	return out
}

// JailRule is one configured rule for one jail, in the neutral shape
// this package takes. It is field-for-field identical to
// api/internalpb's FirewallRule and to pf.Rule - deliberately, so that
// the eventual JailDefinition.firewall_rules field is a one-line
// conversion at the call site rather than a translation layer.
type JailRule struct {
	Direction string // "in" or "out"
	Action    string // "pass" or "block"
	Protocol  string // "tcp", "udp", "icmp", or "" (any)
	PortRange string // "22", "8000-9000", or "" (any)
}

// DescribeFirewall renders a status and its anchor name for logs and
// for a UI row, so both read from the same place rather than each
// re-deriving "which anchor belongs to this jail".
func DescribeFirewall(jailID string, status FirewallStatus) string {
	if jailID == "" {
		return string(FirewallStatusUnknown)
	}
	return fmt.Sprintf("%s (pf anchor %s)", status, Anchor(jailID))
}
