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
	//
	// It may legitimately name a bridge(4) interface - brood's own
	// managerd config sets it to the management bridge - and
	// ensureUplinkBridgedNetwork/assumecheck both handle that. What this
	// Manager will not do is tag an Apiary-owned VLAN onto it, because
	// that produces a Bridge SVI (ADR-0138); EnsureVLAN refuses that up
	// front with an actionable error.
	Uplink string

	// Runner executes the ifconfig(8) calls this Manager makes. nil
	// means the real shell, which is the only correct setting in
	// production; tests inject a fake. Same nil-able-injection seam
	// internal/jailnet's Reconciler.Runner already documents.
	Runner Runner

	// Bridges observes bridge membership, which is how the Bridge SVI
	// question is answered (ADR-0138). nil means internal/netif's real
	// ifconfig(8) reader - reused, not reimplemented, so there is one
	// parser of a bridge's own member list in this codebase. Tests
	// inject a fake.
	Bridges BridgeObserver
}

// ifaceExists reports whether an interface named name currently exists,
// via `ifconfig <name>` - FreeBSD's ifconfig reports "does not exist" on
// stderr (wrapped into the returned error by runCmd) for an absent
// interface, which is the only failure mode treated as "doesn't exist"
// rather than a real error.
func (m *Manager) ifaceExists(ctx context.Context, name string) (bool, error) {
	_, err := m.run(ctx, "ifconfig", name)
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
	out, err := m.run(ctx, "ifconfig", name)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return false, false, nil
		}
		return false, false, err
	}
	return true, isUp(out), nil
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
//
// A bridge(4) uplink is refused here, before anything is created
// (ADR-0138). FreeBSD accepts `ifconfig vlanN vlan N vlandev bridgeM`
// and produces a Bridge SVI, but the SVI is the bridge's own L3
// interface rather than an Ethernet member, so the reconciler's very
// next step - adding it to the network's own per-network bridge - is
// refused by the kernel ("BRDGADD vlan2: Invalid argument (Bridge SVI
// cannot be added to a bridge)", confirmed live on brood). Refusing at
// the point of creation is the difference between one clear error and
// an interface that gets created and then has to be cleaned up again on
// every reconcile tick, and it leaves the caller with nothing
// half-created to leak.
//
// The refusal is not a guess: it rests on an observation of m.Uplink
// through internal/netif.BridgeMembers, and when that observation cannot
// be made at all the answer is also a refusal - an unread uplink is
// silence, not permission (the same rule
// internal/assumecheck's ReasonBridgeMembershipUnknown applies).
func (m *Manager) EnsureVLAN(ctx context.Context, vlanID uint32) (name string, created bool, err error) {
	if vlanID == 0 {
		return m.Uplink, false, nil
	}
	if m.Uplink == "" {
		return "", false, fmt.Errorf("vlan: EnsureVLAN(%d): no uplink interface configured", vlanID)
	}

	switch state, err := m.observeUplinkSVI(ctx); {
	case err != nil:
		// Deliberately fatal rather than assumed-benign: a tagged VLAN is
		// the one thing in this package that can produce a kernel-level
		// misconfiguration rather than just a failed command, so it is
		// not created on an unanswered question. Retried on the next
		// tick, which re-observes from scratch.
		return "", false, fmt.Errorf("vlan: EnsureVLAN(%d): cannot determine whether the configured uplink %q is a bridge, so refusing to tag a VLAN onto it: %w", vlanID, m.Uplink, err)
	case state == sviOnBridge:
		return "", false, fmt.Errorf("%w: the configured uplink %q is a bridge, so tagging VLAN %d onto it would create a Bridge SVI.%s", ErrBridgeSVI, m.Uplink, vlanID, sviGuidance)
	}

	name = vlanIfaceName(vlanID)
	exists, err := m.ifaceExists(ctx, name)
	if err != nil {
		return "", false, fmt.Errorf("vlan: checking %s: %w", name, err)
	}
	if exists {
		return name, false, nil
	}

	if _, err := m.run(ctx, "ifconfig", name, "create"); err != nil {
		return "", false, fmt.Errorf("vlan: creating %s: %w", name, err)
	}
	if _, err := m.run(ctx, "ifconfig", name, "vlan", strconv.FormatUint(uint64(vlanID), 10), "vlandev", m.Uplink); err != nil {
		m.run(ctx, "ifconfig", name, "destroy")
		return "", false, fmt.Errorf("vlan: tagging %s onto %s: %w", name, m.Uplink, err)
	}
	if _, err := m.run(ctx, "ifconfig", name, "up"); err != nil {
		return "", false, fmt.Errorf("vlan: bringing up %s: %w", name, err)
	}
	return name, true, nil
}

// EnsureBridge ensures a bridge(4) interface named name exists (and is
// up), creating it with that exact name if not - FreeBSD's ifconfig
// supports naming a cloned interface directly at creation time via
// `name`, the same way any interface can be renamed.
func (m *Manager) EnsureBridge(ctx context.Context, name string) (created bool, err error) {
	exists, err := m.ifaceExists(ctx, name)
	if err != nil {
		return false, fmt.Errorf("vlan: checking bridge %s: %w", name, err)
	}
	if !exists {
		if _, err := m.run(ctx, "ifconfig", "bridge", "create", "name", name); err != nil {
			return false, fmt.Errorf("vlan: creating bridge %s: %w", name, err)
		}
		created = true
	}
	if _, err := m.run(ctx, "ifconfig", name, "up"); err != nil {
		return false, fmt.Errorf("vlan: bringing up bridge %s: %w", name, err)
	}
	return created, nil
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
	out, err := m.run(ctx, "ifconfig", bridge)
	if err != nil {
		return fmt.Errorf("vlan: checking bridge %s address: %w", bridge, err)
	}
	if strings.Contains(out, "inet "+ip+" ") {
		return nil
	}
	if _, err := m.run(ctx, "ifconfig", bridge, "inet", cidr); err != nil {
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

// EnsureEpair creates a new epair(4) pair and adds its host-side ("a")
// end to bridge, for VNET jail networking (ADR-0117) - the epair
// equivalent of bhyve's own per-VM tap(4)/createTap, reusing this same
// Manager's existing bridge-membership logic (EnsureMember) rather than
// duplicating it. EnsureMember is now SVI-aware (ADR-0138); the epair
// path is deliberately unaffected by that, because an epair(4) end is
// never a Bridge SVI - see the explicit check below and the SVI
// blast-radius test.
//
// Unlike a vlan(4)/bridge(4) interface, an epair(4) pair can't be
// created with a caller-chosen name - `ifconfig epair create` always
// auto-numbers both ends (e.g. "epair0a"/"epair0b"), so this always
// creates a fresh pair; the caller (internal/cluster's reconciler) is
// responsible for recording the returned names so a later tick or
// teardown can find them again, mirroring how internal/bhyve's own
// tapfile records its tap device name for the same reason.
//
// hostSide is the "a" end (kept on the host, added to bridge); jailSide
// is the "b" end (handed to jail(8) via vnet.interface, moved into the
// jail's own vnet at jail creation time and never touched by this
// Manager again).
func (m *Manager) EnsureEpair(ctx context.Context, bridge string) (hostSide, jailSide string, err error) {
	out, err := m.run(ctx, "ifconfig", "epair", "create")
	if err != nil {
		return "", "", fmt.Errorf("vlan: creating epair: %w", err)
	}
	// `ifconfig epair create` prints only the "a" end's name, e.g.
	// "epair0a" - the "b" end is the same base with the trailing "a"
	// swapped for "b" (FreeBSD's own epair(4) naming convention).
	hostSide = strings.TrimSpace(out)
	if !strings.HasSuffix(hostSide, "a") {
		m.run(ctx, "ifconfig", hostSide, "destroy")
		return "", "", fmt.Errorf("vlan: unexpected epair create output %q", out)
	}
	jailSide = strings.TrimSuffix(hostSide, "a") + "b"

	if _, err := m.run(ctx, "ifconfig", hostSide, "up"); err != nil {
		m.run(ctx, "ifconfig", hostSide, "destroy")
		return "", "", fmt.Errorf("vlan: bringing up %s: %w", hostSide, err)
	}
	// An epair(4) end is an ordinary Ethernet interface: it can be, and
	// here must be, an ordinary bridge member, so the Bridge SVI case
	// this call can now report (ADR-0138) is unreachable for it by
	// construction. It is still checked rather than assumed, because a
	// caller that gets MembershipSVI here has been handed something this
	// function has no idea how to move into a jail, and quietly dropping
	// the addm would leave the jail with no network at all.
	membership, err := m.EnsureMember(ctx, bridge, hostSide)
	if err != nil {
		m.run(ctx, "ifconfig", hostSide, "destroy")
		return "", "", err
	}
	if membership.State == MembershipSVI {
		m.run(ctx, "ifconfig", hostSide, "destroy")
		return "", "", fmt.Errorf("vlan: %s was reported as a Bridge SVI on %s, which cannot happen for an epair(4) end and will not be treated as joined to bridge %s", hostSide, membership.SVIParent, bridge)
	}
	return hostSide, jailSide, nil
}

// DestroyEpair tears down an epair(4) pair by its host-side ("a") name
// - destroying either end destroys both, the same as bhyve's tap
// devices. Best-effort and idempotent, like DestroyBridge/DestroyVLAN:
// "already gone" (e.g. because the jail that owned the "b" end was
// already removed, taking the whole pair with it) is not an error.
func (m *Manager) DestroyEpair(ctx context.Context, hostSide string) error {
	if hostSide == "" {
		return nil
	}
	_, err := m.run(ctx, "ifconfig", hostSide, "destroy")
	if err != nil && !strings.Contains(err.Error(), "does not exist") {
		return fmt.Errorf("vlan: destroying epair %s: %w", hostSide, err)
	}
	return nil
}

// DestroyBridge tears bridge down. Best-effort and idempotent, like
// internal/bhyve's destroyTap: "already gone" is not an error, since
// this runs during teardown where a previous partial attempt may have
// already gotten this far.
func (m *Manager) DestroyBridge(ctx context.Context, name string) error {
	_, err := m.run(ctx, "ifconfig", name, "destroy")
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
	_, err := m.run(ctx, "ifconfig", name, "destroy")
	if err != nil && !strings.Contains(err.Error(), "does not exist") {
		return fmt.Errorf("vlan: destroying %s: %w", name, err)
	}
	return nil
}
