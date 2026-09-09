package vlan

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Manager creates and tears down the vlan(4)/bridge(4) interfaces that
// realize a NetworkDefinition (api/internalpb/state.proto) on this
// node. Every operation is idempotent - internal/cluster's Reconciler
// calls these every tick, the same way it re-checks ZFS dataset/bhyve
// VM existence rather than tracking "did I already do this" itself.
type Manager struct {
	// Uplink is the physical interface a tagged VLAN attaches to (e.g.
	// "re0", "em0" - confirmed to differ per node in this project's own
	// fleet). Required for any network with a non-zero VLAN ID; unused
	// for untagged (vlan_id == 0) networks, which attach directly to
	// Uplink instead of a vlan(4) sub-interface.
	Uplink string
}

// ifaceExists reports whether an interface named name currently exists,
// via `ifconfig <name>` - FreeBSD's ifconfig reports "does not exist" on
// stderr (wrapped into the returned error by runCmd) for an absent
// interface, which is the only failure mode treated as "doesn't exist"
// rather than a real error.
func ifaceExists(ctx context.Context, name string) (bool, error) {
	_, err := runCmd(ctx, "ifconfig", name)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "does not exist") {
		return false, nil
	}
	return false, err
}

// InterfaceStatus reports whether an interface named name exists on
// this node and, if so, whether it's currently up - for the web UI's
// Networks page, which shows each network's bridge status per node
// (physical, real-time state; not part of NetworkDefinition's own
// ephemeral fields). exists is false (with up meaningless) if the
// interface doesn't exist here yet - e.g. no VM on this network has
// been reconciled on this node.
func (m *Manager) InterfaceStatus(ctx context.Context, name string) (exists, up bool, err error) {
	out, err := runCmd(ctx, "ifconfig", name)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return false, false, nil
		}
		return false, false, err
	}
	return true, isUp(out), nil
}

// UplinkInterface returns the configured uplink interface name (the
// Uplink field itself, exposed as a method so *Manager can satisfy an
// interface a struct field can't - see internal/manager's VLANStatus).
func (m *Manager) UplinkInterface() string {
	return m.Uplink
}

// Down administratively brings the uplink interface down
// (`ifconfig <uplink> down`) - an explicit, operator-triggered action,
// never called by the reconciler's own tick loop (see ADR-0085: EnsureVLAN
// already short-circuits for the untagged/raw-uplink case without any
// ifconfig calls, so a downed uplink is never silently re-upped by normal
// reconciliation).
func (m *Manager) Down(ctx context.Context) error {
	if m.Uplink == "" {
		return fmt.Errorf("vlan: no uplink interface configured")
	}
	_, err := runCmd(ctx, "ifconfig", m.Uplink, "down")
	return err
}

// Up administratively brings the uplink interface back up
// (`ifconfig <uplink> up`), reversing Down.
func (m *Manager) Up(ctx context.Context) error {
	if m.Uplink == "" {
		return fmt.Errorf("vlan: no uplink interface configured")
	}
	_, err := runCmd(ctx, "ifconfig", m.Uplink, "up")
	return err
}

// isUp parses whether the interface is up from the first line of
// `ifconfig <name>`'s output, e.g.
// "bridge0: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> ..." -
// checking for an exact "UP" flag inside the <...> list, not just a
// substring match (which could also match "UP" appearing as part of a
// longer, unrelated flag name).
func isUp(ifconfigOutput string) bool {
	line, _, _ := strings.Cut(ifconfigOutput, "\n")
	start := strings.Index(line, "<")
	end := strings.Index(line, ">")
	if start == -1 || end == -1 || end < start {
		return false
	}
	for _, flag := range strings.Split(line[start+1:end], ",") {
		if flag == "UP" {
			return true
		}
	}
	return false
}

// vlanIfaceName returns the stable, deterministic interface name for a
// given VLAN tag - FreeBSD's vlan(4) cloner recognizes "vlanN" as a
// creatable name directly (like "bridgeN"/"tapN"), so there's no need
// for kernel auto-numbering (which wouldn't be stable/discoverable
// across reconciler ticks or a managerd restart anyway).
func vlanIfaceName(vlanID uint32) string {
	return fmt.Sprintf("vlan%d", vlanID)
}

// EnsureVLAN ensures a vlan(4) interface exists for vlanID, tagged onto
// m.Uplink, and returns its name. vlanID == 0 means "untagged" - there
// is no vlan interface to create, and the caller should attach directly
// to m.Uplink instead (returned as-is).
func (m *Manager) EnsureVLAN(ctx context.Context, vlanID uint32) (name string, created bool, err error) {
	if vlanID == 0 {
		return m.Uplink, false, nil
	}
	if m.Uplink == "" {
		return "", false, fmt.Errorf("vlan: EnsureVLAN(%d): no uplink interface configured", vlanID)
	}

	name = vlanIfaceName(vlanID)
	exists, err := ifaceExists(ctx, name)
	if err != nil {
		return "", false, fmt.Errorf("vlan: checking %s: %w", name, err)
	}
	if exists {
		return name, false, nil
	}

	if _, err := runCmd(ctx, "ifconfig", name, "create"); err != nil {
		return "", false, fmt.Errorf("vlan: creating %s: %w", name, err)
	}
	if _, err := runCmd(ctx, "ifconfig", name, "vlan", strconv.FormatUint(uint64(vlanID), 10), "vlandev", m.Uplink); err != nil {
		runCmd(ctx, "ifconfig", name, "destroy")
		return "", false, fmt.Errorf("vlan: tagging %s onto %s: %w", name, m.Uplink, err)
	}
	if _, err := runCmd(ctx, "ifconfig", name, "up"); err != nil {
		return "", false, fmt.Errorf("vlan: bringing up %s: %w", name, err)
	}
	return name, true, nil
}

// EnsureBridge ensures a bridge(4) interface named name exists (and is
// up), creating it with that exact name if not - FreeBSD's ifconfig
// supports naming a cloned interface directly at creation time via
// `name`, the same way any interface can be renamed.
func (m *Manager) EnsureBridge(ctx context.Context, name string) (created bool, err error) {
	exists, err := ifaceExists(ctx, name)
	if err != nil {
		return false, fmt.Errorf("vlan: checking bridge %s: %w", name, err)
	}
	if !exists {
		if _, err := runCmd(ctx, "ifconfig", "bridge", "create", "name", name); err != nil {
			return false, fmt.Errorf("vlan: creating bridge %s: %w", name, err)
		}
		created = true
	}
	if _, err := runCmd(ctx, "ifconfig", name, "up"); err != nil {
		return false, fmt.Errorf("vlan: bringing up bridge %s: %w", name, err)
	}
	return created, nil
}

// EnsureMember ensures iface is a member of bridge, adding it if not
// already (re-running `addm` on an existing member errors, so this
// checks first via the bridge's own reported member list).
func (m *Manager) EnsureMember(ctx context.Context, bridge, iface string) error {
	out, err := runCmd(ctx, "ifconfig", bridge)
	if err != nil {
		return fmt.Errorf("vlan: checking bridge %s members: %w", bridge, err)
	}
	if strings.Contains(out, "member: "+iface+" ") {
		return nil
	}
	if _, err := runCmd(ctx, "ifconfig", bridge, "addm", iface); err != nil {
		return fmt.Errorf("vlan: adding %s to bridge %s: %w", iface, bridge, err)
	}
	return nil
}

// EnsureBridgeAddress assigns subnet's gateway address (its first host
// address, ".1" - the FSM never allocates this to a VM, see
// internal/raft's allocateIP) to bridge itself, so VMs on this network
// can route through this node.
func (m *Manager) EnsureBridgeAddress(ctx context.Context, bridge, subnet string) error {
	cidr, ip, err := gatewayCIDR(subnet)
	if err != nil {
		return err
	}
	out, err := runCmd(ctx, "ifconfig", bridge)
	if err != nil {
		return fmt.Errorf("vlan: checking bridge %s address: %w", bridge, err)
	}
	if strings.Contains(out, "inet "+ip+" ") {
		return nil
	}
	if _, err := runCmd(ctx, "ifconfig", bridge, "inet", cidr); err != nil {
		return fmt.Errorf("vlan: assigning %s to bridge %s: %w", cidr, bridge, err)
	}
	return nil
}

// gatewayCIDR returns subnet's first host address (".1") as both a
// "ip/prefixlen" string (for ifconfig) and a bare ip string (for
// idempotency checks against ifconfig's own output).
func gatewayCIDR(subnet string) (cidr, ip string, err error) {
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", "", fmt.Errorf("vlan: invalid subnet %q: %w", subnet, err)
	}
	base := ipnet.IP.To4()
	if base == nil {
		return "", "", fmt.Errorf("vlan: subnet %q is not IPv4", subnet)
	}
	gw := net.IPv4(base[0], base[1], base[2], base[3]|1)
	ones, _ := ipnet.Mask.Size()
	return fmt.Sprintf("%s/%d", gw.String(), ones), gw.String(), nil
}

// DestroyBridge tears bridge down. Best-effort and idempotent, like
// internal/bhyve's destroyTap: "already gone" is not an error, since
// this runs during teardown where a previous partial attempt may have
// already gotten this far.
func (m *Manager) DestroyBridge(ctx context.Context, name string) error {
	_, err := runCmd(ctx, "ifconfig", name, "destroy")
	if err != nil && !strings.Contains(err.Error(), "does not exist") {
		return fmt.Errorf("vlan: destroying bridge %s: %w", name, err)
	}
	return nil
}

// DestroyVLAN tears down the vlan(4) interface for vlanID. vlan_id zero
// represents the host uplink rather than an Apiary-created interface, so it
// is deliberately never destroyed. Like DestroyBridge, this is idempotent.
func (m *Manager) DestroyVLAN(ctx context.Context, vlanID uint32) error {
	if vlanID == 0 {
		return nil
	}
	name := vlanIfaceName(vlanID)
	_, err := runCmd(ctx, "ifconfig", name, "destroy")
	if err != nil && !strings.Contains(err.Error(), "does not exist") {
		return fmt.Errorf("vlan: destroying %s: %w", name, err)
	}
	return nil
}
