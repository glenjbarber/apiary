package vlan

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/glenjbarber/apiary/internal/netif"
)

// ErrBridgeSVI marks every failure this package raises because of
// FreeBSD's Bridge SVI rule, so a caller (or an operator reading a log
// line) can classify the cause with errors.Is instead of matching on
// message text - the same discipline internal/assumecheck's own
// stable reason-code vocabulary applies to assumption results
// (ReasonBridgeMembershipUnknown and friends).
//
// The rule: a vlan(4) interface whose vlandev is a bridge(4) is that
// bridge's own L3 switch interface. It is not an Ethernet interface
// enslaved to the bridge; it *is* the bridge's third layer. FreeBSD
// therefore refuses to enslave it to any bridge, with a diagnostic that
// names the concept outright - confirmed live on brood:
//
//	ifconfig: BRDGADD vlan2: Invalid argument (Bridge SVI cannot be added to a bridge)
var ErrBridgeSVI = errors.New("vlan: Bridge SVI: a VLAN whose parent is a bridge is that bridge's own L3 interface and cannot be a member of any bridge")

// MembershipState is the honest, four-way answer to "is iface on bridge,
// and what did this call do about it". Every state that is not a positive
// observation says so: nothing here collapses "I could not tell" into
// "it is not a member", the distinction internal/netif.BridgeMembers
// already draws (an unreadable bridge is an error, never an empty member
// list) and internal/assumecheck's ReasonBridgeMembershipUnknown applies
// to assumption results (an unread bridge is silence, not evidence of
// absence, ADR-0056).
type MembershipState int

const (
	// MembershipUnknown makes no membership claim at all: either the
	// bridge's own member list could not be read, or the interface's
	// nature could not be established, or an issued addm did not
	// succeed. Paired with a non-nil error in every case. A caller must
	// treat it as a failure of this pass, never as permission to skip
	// the addm.
	MembershipUnknown MembershipState = iota
	// MembershipPresent: positively observed as already a member. No
	// command was issued to change anything (re-running addm on an
	// existing member is itself an error on FreeBSD).
	MembershipPresent
	// MembershipAdded: positively observed as not a member, then added,
	// and the addm succeeded.
	MembershipAdded
	// MembershipSVI: iface is a Bridge SVI, so no addm was issued and
	// none is valid. SVIParent names the bridge that owns this
	// interface's L3. Whether that is a success or a failure depends on
	// which bridge was asked about, which is why this is a distinct
	// state rather than a bare bool - see EnsureMember.
	MembershipSVI
)

func (s MembershipState) String() string {
	switch s {
	case MembershipUnknown:
		return "unknown"
	case MembershipPresent:
		return "present"
	case MembershipAdded:
		return "added"
	case MembershipSVI:
		return "bridge_svi"
	default:
		return fmt.Sprintf("MembershipState(%d)", int(s))
	}
}

// Membership is what EnsureMember observed, and what it did about it.
// State is the answer; SVIParent is set only for MembershipSVI, where it
// names the bridge that actually owns the interface's L3.
type Membership struct {
	Bridge    string
	Iface     string
	State     MembershipState
	SVIParent string
}

// BridgeObserver reports the members of a bridge and whether an
// interface is a bridge at all. It is the exact subset of
// internal/netif.BridgeMembers this package needs, declared as an
// interface for the same reason internal/cluster and internal/jailnet
// declare their own per-dependency interfaces: so this package's
// decisions can be tested with no FreeBSD anywhere in sight. The
// production implementation is internal/netif's own, not a second one.
type BridgeObserver interface {
	BridgeMembers(ctx context.Context, bridge string) (members []string, isBridge bool, err error)
}

// netifBridgeObserver adapts internal/netif.BridgeMembers to
// BridgeObserver. This is the default when Manager.Bridges is nil.
type netifBridgeObserver struct{}

func (netifBridgeObserver) BridgeMembers(ctx context.Context, bridge string) ([]string, bool, error) {
	return netif.BridgeMembers(ctx, bridge)
}

func (m *Manager) bridges() BridgeObserver {
	if m.Bridges == nil {
		return netifBridgeObserver{}
	}
	return m.Bridges
}

// sviState is the internal four-way answer to "is this interface a Bridge
// SVI, and whose". Distinct from MembershipState because the SVI question
// is asked about one interface, while Membership is about one
// interface/bridge pair.
type sviState int

const (
	// sviUnknown: could not be established. Never treated as "no".
	sviUnknown sviState = iota
	// sviNotAVLAN: the interface is not a vlan(4) interface at all, so
	// it cannot be an SVI. This is the epair(4)/tap(4) case, and it is a
	// positive observation from the interface's own ifconfig output
	// rather than an absence of evidence.
	sviNotAVLAN
	// sviNo: it is a vlan(4) interface whose observed parent is an
	// ordinary interface, so adding it to a bridge is meaningful.
	sviNo
	// sviOnBridge: it is a vlan(4) interface whose observed parent is a
	// bridge, so it is that bridge's SVI.
	sviOnBridge
)

func (s sviState) String() string {
	switch s {
	case sviUnknown:
		return "unknown"
	case sviNotAVLAN:
		return "not_a_vlan"
	case sviNo:
		return "not_an_svi"
	case sviOnBridge:
		return "bridge_svi"
	default:
		return fmt.Sprintf("sviState(%d)", int(s))
	}
}

// sviGuidance is the operator-facing half of the Bridge SVI refusal,
// shared verbatim by both places this package can detect one (the
// early refusal in EnsureVLAN and the no-addm path in EnsureMember) so
// the two never drift into telling an operator two different stories
// about the same node. It is appended to ErrBridgeSVI, which is what
// makes the whole thing a single classification (errors.Is) carrying one
// message.
const sviGuidance = " FreeBSD says so in as many words when the addm is attempted: \"BRDGADD vlan2: Invalid argument (Bridge SVI cannot be added to a bridge)\" (confirmed live on brood). Apiary will not build a per-network bridge on top of a Bridge SVI: point -vlan-uplink at a physical NIC (the interface that is a member of that bridge) so tagged networks can be provisioned, or mark the network uplink_bridged and opt in per node (ADR-0101) to use the host's own bridge as-is. Auto-attaching VM taps to the management bridge is deliberately not an option - that is what ADR-0101's per-node opt-in exists to prevent."

// observeUplinkSVI reports whether m.Uplink - the interface a VLAN would
// be tagged onto - is itself a bridge, which is what makes the VLAN an
// SVI rather than an ordinary bridge member. This is the check that is
// derived from configuration plus one observation, rather than from a
// parse of the VLAN's own output, so it works even on an ifconfig(8)
// that does not name a VLAN's parent.
func (m *Manager) observeUplinkSVI(ctx context.Context) (sviState, error) {
	_, isBridge, err := m.bridges().BridgeMembers(ctx, m.Uplink)
	if err != nil {
		return sviUnknown, fmt.Errorf("reading the configured uplink %q: %w", m.Uplink, err)
	}
	if isBridge {
		return sviOnBridge, nil
	}
	return sviNo, nil
}

// observeSVI establishes whether iface is a Bridge SVI, and if so which
// bridge owns it. Two independent sources, in order of directness:
//
//  1. ifconfig(8)'s own parent line for the interface, when this ifconfig
//     prints one ("Parent name: <iface>", which FreeBSD's if_vlan(4)
//     prints for a vlan(4) interface's parent). This is a direct
//     observation of the interface's real parent, so it is used whenever
//     it is present - including to correct a configured uplink that does
//     not match an interface someone else created.
//  2. otherwise, the configured vlandev (m.Uplink) plus one observation
//     of whether that is a bridge. This is exact for an interface
//     EnsureVLAN itself just created and tagged, which is the case in
//     the live brood failure; for a same-named interface that already
//     existed and was tagged by hand, it is a stated assumption, which
//     is why source 1 is preferred and why this function reports
//     sviUnknown rather than guessing when it cannot observe the uplink.
//
// Every failure returns a non-nil error alongside sviUnknown, so no
// caller can mistake silence for "it is not an SVI".
func (m *Manager) observeSVI(ctx context.Context, iface string) (sviState, string, error) {
	out, err := m.run(ctx, "ifconfig", iface)
	if err != nil {
		return sviUnknown, "", fmt.Errorf("reading %q's own interface state: %w", iface, err)
	}
	// The vlan(4) line is what proves this is a vlan(4) interface at
	// all, and therefore the only kind that can be an SVI. An epair(4) or
	// tap(4) host side has no such line, and is settled here - positively
	// - rather than by failing to look like something else.
	if !isVLANInterface(out) {
		return sviNotAVLAN, "", nil
	}
	if parent := vlanParentName(out); parent != "" {
		_, isBridge, err := m.bridges().BridgeMembers(ctx, parent)
		if err != nil {
			return sviUnknown, parent, fmt.Errorf("reading %q's parent %q to tell an SVI from an ordinary VLAN: %w", iface, parent, err)
		}
		if isBridge {
			return sviOnBridge, parent, nil
		}
		return sviNo, parent, nil
	}
	if m.Uplink == "" {
		// No configured vlandev and no parent line: there is nothing left
		// to observe, and assuming "not an SVI" here would be exactly the
		// silent guess this whole change exists to remove.
		return sviUnknown, "", fmt.Errorf("ifconfig(8) did not report a parent for %q and this Manager has no configured uplink to fall back on", iface)
	}
	switch state, err := m.observeUplinkSVI(ctx); {
	case err != nil:
		return sviUnknown, "", err
	case state == sviOnBridge:
		return sviOnBridge, m.Uplink, nil
	default:
		return sviNo, m.Uplink, nil
	}
}

// isVLANInterface recognizes the vlan(4) line ifconfig(8) prints for a
// VLAN interface, e.g. "\tvlan: 2 vlanpcp: 0 vlanhwtag: 1". Anchored at
// the start of a trimmed line so it cannot match an unrelated line that
// happens to contain the word.
func isVLANInterface(ifconfigOutput string) bool {
	for _, raw := range strings.Split(ifconfigOutput, "\n") {
		if strings.HasPrefix(strings.TrimSpace(raw), "vlan: ") {
			return true
		}
	}
	return false
}

// vlanParentName extracts the parent interface name from ifconfig(8)'s
// own "Parent name: <iface>" line, or "" when this output carries no such
// line. Deliberately a single exact spelling rather than a set of
// guesses: an unrecognized spelling must fall through to the
// configured-uplink observation above, not be invented into a wrong
// answer here. Pure, so the shape can be exercised from fixtures.
func vlanParentName(ifconfigOutput string) string {
	for _, raw := range strings.Split(ifconfigOutput, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "Parent name:") {
			continue
		}
		if name := strings.TrimSpace(strings.TrimPrefix(line, "Parent name:")); name != "" {
			return name
		}
	}
	return ""
}

// EnsureMember ensures iface is a member of bridge, adding it if not
// already, and reports what it actually observed rather than a bare
// success (ADR-0138).
//
// The three states a caller has to be able to tell apart are kept
// distinct rather than collapsed into a bool:
//
//   - already a member (MembershipPresent): observed in the bridge's own
//     member list, nothing issued.
//   - added (MembershipAdded): observed absent, addm issued, addm
//     succeeded.
//   - a Bridge SVI (MembershipSVI): no addm issued and none valid, since
//     the interface is a bridge's own L3 (see ErrBridgeSVI). This is a
//     success only when the bridge asked about *is* that bridge - the
//     caller's own uplink path, which this package never chooses for
//     anyone. It is a refusal, with an error, for every other bridge,
//     because there is no topology in which the addm could have worked.
//   - and could not be observed (MembershipUnknown): a non-nil error,
//     and specifically never a licence to skip the addm.
//
// The refusal is deliberate. Silently skipping the addm and continuing
// would hand the reconciler a per-network bridge with no path to its own
// VLAN - the gateway address on it, VM taps on it, and nothing
// connecting it to anything - which is a network that looks provisioned
// and is dead. Automatically putting the VM taps on the SVI's own bridge
// instead would be the same topology ADR-0101 made an explicit per-node
// opt-in, so it is not selected here either.
func (m *Manager) EnsureMember(ctx context.Context, bridge, iface string) (Membership, error) {
	membership := Membership{Bridge: bridge, Iface: iface}
	members, _, err := m.bridges().BridgeMembers(ctx, bridge)
	if err != nil {
		// Deliberately an error, not an empty member list: this package
		// inherits internal/netif's own rule (and
		// internal/assumecheck's ReasonBridgeMembershipUnknown) that a
		// bridge whose membership could not be read is silence, not
		// evidence of absence.
		membership.State = MembershipUnknown
		return membership, fmt.Errorf("reading bridge %s's members to check %s: %w", bridge, iface, err)
	}
	if containsInterface(members, iface) {
		membership.State = MembershipPresent
		return membership, nil
	}

	state, parent, err := m.observeSVI(ctx, iface)
	switch {
	case err != nil:
		membership.State = MembershipUnknown
		return membership, fmt.Errorf("determining whether %s can be added to bridge %s: %w", iface, bridge, err)
	case state == sviUnknown:
		membership.State = MembershipUnknown
		return membership, fmt.Errorf("determining whether %s can be added to bridge %s: could not establish whether it is a Bridge SVI", iface, bridge)
	case state == sviOnBridge:
		membership.State, membership.SVIParent = MembershipSVI, parent
		if parent == bridge {
			// The bridge asked about is the SVI's own parent: the
			// interface already is that bridge's L3, so there is nothing
			// to add and nothing to add it to. Reported as a distinct
			// state rather than MembershipPresent, because "it is a
			// member" is a different fact about the host than "it is the
			// bridge's own switch interface".
			return membership, nil
		}
		return membership, fmt.Errorf("%w: %s is a Bridge SVI on %s, so it cannot be added to %s.%s", ErrBridgeSVI, iface, parent, bridge, sviGuidance)
	}

	if _, err := m.run(ctx, "ifconfig", bridge, "addm", iface); err != nil {
		// Not MembershipAdded: the addm is the one claim here that did
		// not come true, so the honest state is that no membership claim
		// is being made and the next pass re-reads the bridge.
		membership.State = MembershipUnknown
		return membership, fmt.Errorf("vlan: adding %s to bridge %s: %w", iface, bridge, err)
	}
	membership.State = MembershipAdded
	return membership, nil
}

func containsInterface(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
