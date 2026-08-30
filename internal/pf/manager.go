package pf

import "context"

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
// no longer exists.
func (m *Manager) Flush(ctx context.Context, anchor string) error {
	_, err := runCmd(ctx, "pfctl", "-a", anchor, "-F", "rules")
	return err
}
