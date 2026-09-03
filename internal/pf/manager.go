package pf

import (
	"context"
	"fmt"
	"strings"
)

// Manager applies and clears per-VM pf(8) anchor rulesets via pfctl(8).
type Manager struct{}

// Apply replaces anchor's entire ruleset with rules (rendered via
// RenderRules), the same full-replace-not-diff convention
// internal/hast's WriteConfig already uses for hast.conf. No rules
// means an empty anchor, i.e. everything allowed - matching this
// project's de facto behavior before firewall support existed.
func (m *Manager) Apply(ctx context.Context, anchor string, rules []Rule) error {
	body, err := RenderRules(rules)
	if err != nil {
		return err
	}
	_, err = runCmdStdin(ctx, body, "pfctl", "-a", anchor, "-f", "-")
	return err
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
func (m *Manager) Flush(ctx context.Context, anchor string) error {
	_, err := runCmd(ctx, "pfctl", "-a", anchor, "-F", "rules")
	if err != nil && strings.Contains(err.Error(), "No such anchor") {
		return nil
	}
	return err
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
func (m *Manager) ApplyNAT(ctx context.Context, anchor, subnet, uplink string) error {
	body := fmt.Sprintf("match out on %s from %s to any nat-to (%s)\n", uplink, subnet, uplink)
	_, err := runCmdStdin(ctx, body, "pfctl", "-a", anchor, "-f", "-")
	return err
}
