package vlan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The Bridge SVI rule (ADR-0138) and the three membership states, all
// against the fake host. Nothing here shells out: ifconfig, bridge(4)
// and vlan(4) are all modelled, including the kernel's own refusal to
// enslave a Bridge SVI, so a regression that re-introduces the addm
// fails with the kernel's own wording instead of passing quietly.
//
// What macOS cannot establish, and is not claimed here: that this
// FreeBSD actually refuses the addm (internal/vlan's
// integration_test.go asserts that, gated to the testbed), and anything
// about bhyve, jails, VNET, PF, ZFS, HAST or real-network timing.

// --- The plain-NIC path: the addm is still the right thing to do ---

func TestEnsureVLANAndMember_PlainNICUplinkStillAdds(t *testing.T) {
	h := newFakeHost().nic("em0").bridge("apnet-abcd1234")
	m := newTestManager(h, "em0")
	ctx := context.Background()

	name, created, err := m.EnsureVLAN(ctx, 2)
	if err != nil {
		t.Fatalf("EnsureVLAN(2) error: %v", err)
	}
	if name != "vlan2" || !created {
		t.Fatalf("EnsureVLAN(2) = (%q, %v), want (\"vlan2\", true)", name, created)
	}
	if h.isSVI("vlan2") {
		t.Fatal("vlan2 is an SVI on a plain-NIC uplink; the model or the tagging is wrong")
	}

	membership, err := m.EnsureMember(ctx, "apnet-abcd1234", "vlan2")
	if err != nil {
		t.Fatalf("EnsureMember() error: %v", err)
	}
	if membership.State != MembershipAdded {
		t.Errorf("state = %v, want %v", membership.State, MembershipAdded)
	}
	if !h.memberOf("apnet-abcd1234", "vlan2") {
		t.Error("vlan2 was not added to the bridge")
	}
}

// Idempotency across repeat reconcile passes: exactly one create and one
// addm across two full passes, and the second pass reads the membership
// instead of re-issuing it (re-running addm on an existing member is
// itself an error on FreeBSD).
func TestEnsureVLANAndMember_AreIdempotentAcrossPasses(t *testing.T) {
	h := newFakeHost().nic("em0").bridge("apnet-abcd1234")
	m := newTestManager(h, "em0")
	ctx := context.Background()

	for pass := 1; pass <= 3; pass++ {
		name, created, err := m.EnsureVLAN(ctx, 2)
		if err != nil {
			t.Fatalf("pass %d: EnsureVLAN(2) error: %v", pass, err)
		}
		if pass == 1 && !created {
			t.Error("pass 1: created = false, want true")
		}
		if pass > 1 && created {
			t.Errorf("pass %d: created = true, want false - it would re-create an existing interface", pass)
		}
		membership, err := m.EnsureMember(ctx, "apnet-abcd1234", name)
		if err != nil {
			t.Fatalf("pass %d: EnsureMember() error: %v", pass, err)
		}
		want := MembershipAdded
		if pass > 1 {
			want = MembershipPresent
		}
		if membership.State != want {
			t.Errorf("pass %d: state = %v, want %v", pass, membership.State, want)
		}
	}

	if got := h.issuedAny("vlan2 create"); got != 1 {
		t.Errorf("issued %d `ifconfig vlan2 create` calls across 3 passes, want 1", got)
	}
	if got := h.issuedAny("apnet-abcd1234 addm vlan2"); got != 1 {
		t.Errorf("issued %d `addm` calls across 3 passes, want 1", got)
	}
}

// --- The bridge uplink: an SVI, refused up front, never added ---

// The live brood failure's first half: with a bridge configured as the
// uplink, the VLAN would become a Bridge SVI, so EnsureVLAN refuses
// before creating anything at all.
func TestEnsureVLAN_BridgeUplinkIsRefusedBeforeAnythingIsCreated(t *testing.T) {
	h := newFakeHost().bridge("bridge0").nic("em0")
	m := newTestManager(h, "bridge0")

	name, created, err := m.EnsureVLAN(context.Background(), 2)
	if !errors.Is(err, ErrBridgeSVI) {
		t.Fatalf("EnsureVLAN(2) error = %v, want one matching ErrBridgeSVI", err)
	}
	if name != "" || created {
		t.Errorf("EnsureVLAN(2) = (%q, %v), want (\"\", false) on refusal", name, created)
	}
	// The whole point of refusing here rather than after creation: no
	// interface to clean up, and no bridge created either.
	if h.issued("vlan2") {
		t.Errorf("a command touching vlan2 was issued: %v", h.calls)
	}
	if _, present := h.ifaces["vlan2"]; present {
		t.Error("vlan2 exists after a refusal")
	}
	// The operator gets the kernel's own diagnostic, quoted, plus what to
	// do about it - the bare EINVAL the reconciler used to surface said
	// nothing about either.
	for _, want := range []string{
		"Bridge SVI cannot be added to a bridge",
		"bridge0",
		"-vlan-uplink",
		"uplink_bridged",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message is missing %q: %s", want, err)
		}
	}
}

// The live brood failure's second half: given an SVI that already exists
// (created by hand, or by an earlier version of this code), EnsureMember
// must not attempt the addm - the fake host refuses exactly as the kernel
// does, so a regression is visible.
func TestEnsureMember_BridgeSVINoAddmIsIssued(t *testing.T) {
	h := newFakeHost().bridge("bridge0").bridge("apnet-abcd1234").vlan("vlan2", 2, "bridge0")
	if !h.isSVI("vlan2") {
		t.Fatal("model error: vlan2 parented to a bridge must be an SVI")
	}
	m := newTestManager(h, "bridge0")

	membership, err := m.EnsureMember(context.Background(), "apnet-abcd1234", "vlan2")
	if !errors.Is(err, ErrBridgeSVI) {
		t.Fatalf("EnsureMember() error = %v, want one matching ErrBridgeSVI", err)
	}
	if membership.State != MembershipSVI {
		t.Errorf("state = %v, want %v", membership.State, MembershipSVI)
	}
	if membership.SVIParent != "bridge0" {
		t.Errorf("SVIParent = %q, want %q", membership.SVIParent, "bridge0")
	}
	if h.issued("addm") {
		t.Errorf("an addm was issued for a Bridge SVI, which FreeBSD refuses: %v", h.calls)
	}
	if h.memberOf("apnet-abcd1234", "vlan2") {
		t.Error("vlan2 ended up a member of apnet-abcd1234")
	}
}

// The same SVI, asked about the bridge that actually owns it: no addm is
// issued or valid, and this is a distinct state rather than a failure -
// the interface already IS that bridge's L3. The only caller that can
// reach this is one that deliberately pointed the network at that
// bridge; nothing in this package chooses it.
func TestEnsureMember_SVIOfTheBridgeItselfIsASeparateState(t *testing.T) {
	h := newFakeHost().bridge("bridge0").vlan("vlan2", 2, "bridge0")
	m := newTestManager(h, "bridge0")

	membership, err := m.EnsureMember(context.Background(), "bridge0", "vlan2")
	if err != nil {
		t.Fatalf("EnsureMember(bridge0, vlan2) error: %v, want nil: the SVI is this bridge's own L3", err)
	}
	if membership.State != MembershipSVI {
		t.Errorf("state = %v, want %v", membership.State, MembershipSVI)
	}
	if membership.SVIParent != "bridge0" {
		t.Errorf("SVIParent = %q, want %q", membership.SVIParent, "bridge0")
	}
	if h.issued("addm") {
		t.Errorf("an addm was issued: %v", h.calls)
	}
}

// --- Silence is not absence: every unreadable observation is Unknown ---

// The bridge's own member list could not be read. The critical property
// is what is NOT done: no addm, because "could not read" must never be
// acted on as "not a member" and retried blindly.
func TestEnsureMember_UnreadableBridgeIsUnknownNotAbsence(t *testing.T) {
	h := newFakeHost().bridge("apnet-abcd1234").nic("tap0")
	h.observeErr["apnet-abcd1234"] = errors.New("ifconfig: permission denied")
	m := newTestManager(h, "em0")

	membership, err := m.EnsureMember(context.Background(), "apnet-abcd1234", "tap0")
	if err == nil {
		t.Fatal("EnsureMember() error = nil, want the observation failure reported")
	}
	if membership.State != MembershipUnknown {
		t.Errorf("state = %v, want %v", membership.State, MembershipUnknown)
	}
	if h.issued("addm") {
		t.Errorf("an addm was issued after membership could not be read: %v", h.calls)
	}
	if !strings.Contains(err.Error(), "reading bridge apnet-abcd1234's members") {
		t.Errorf("error does not say the membership could not be read: %v", err)
	}
}

// An addm that fails claims nothing: the interface is not a member, and
// this call established no membership either. The next pass re-reads the
// bridge rather than trusting either outcome.
func TestEnsureMember_FailedAddmClaimsNothing(t *testing.T) {
	h := newFakeHost().bridge("apnet-abcd1234").nic("tap0")
	h.addmErr = "Device busy"
	m := newTestManager(h, "em0")

	membership, err := m.EnsureMember(context.Background(), "apnet-abcd1234", "tap0")
	if err == nil {
		t.Fatal("EnsureMember() error = nil, want the failed addm reported")
	}
	if membership.State != MembershipUnknown {
		t.Errorf("state = %v, want %v", membership.State, MembershipUnknown)
	}
	if !h.issued("addm tap0") {
		t.Errorf("commands issued: %v, want the addm to have been attempted", h.calls)
	}
	if h.memberOf("apnet-abcd1234", "tap0") {
		t.Error("tap0 is recorded as a member after a failed addm")
	}
}

// The interface's own state could not be read, so whether it is an SVI
// could not be established. Also unknown, and also no addm.
func TestEnsureMember_UndeterminableInterfaceIsUnknown(t *testing.T) {
	h := newFakeHost().bridge("bridge0").bridge("apnet-abcd1234").vlan("vlan2", 2, "bridge0")
	h.readErr["vlan2"] = "ifconfig: too much memory"
	m := newTestManager(h, "bridge0")

	membership, err := m.EnsureMember(context.Background(), "apnet-abcd1234", "vlan2")
	if err == nil {
		t.Fatal("EnsureMember() error = nil, want the unreadable interface reported")
	}
	if membership.State != MembershipUnknown {
		t.Errorf("state = %v, want %v", membership.State, MembershipUnknown)
	}
	if h.issued("addm") {
		t.Errorf("an addm was issued: %v", h.calls)
	}
}

// The uplink itself could not be read, so EnsureVLAN cannot rule out an
// SVI. A tagged VLAN is the one thing here that could produce a
// kernel-level misconfiguration rather than a failed command, so an
// unanswered question is a refusal, not a guess.
func TestEnsureVLAN_UnreadableUplinkIsARefusalNotAGuess(t *testing.T) {
	h := newFakeHost().nic("em0")
	h.observeErr["em0"] = errors.New("ifconfig: permission denied")
	m := newTestManager(h, "em0")

	if _, _, err := m.EnsureVLAN(context.Background(), 2); err == nil {
		t.Fatal("EnsureVLAN(2) error = nil, want a refusal when the uplink cannot be observed")
	} else if !strings.Contains(err.Error(), "cannot determine whether the configured uplink") {
		t.Errorf("error does not say why: %v", err)
	}
	if h.issued("vlan2") {
		t.Errorf("a VLAN command was issued despite the unanswered question: %v", h.calls)
	}
}

// An ifconfig(8) that does not name a VLAN's parent, with nothing else to
// fall back on, is unknown - never "not an SVI".
func TestEnsureMember_NoParentLineAndNoUplinkIsUnknown(t *testing.T) {
	h := newFakeHost()
	h.renderParent = false
	h.bridge("apnet-abcd1234").vlan("vlan2", 2, "")
	m := &Manager{Runner: h, Bridges: h} // deliberately no Uplink

	membership, err := m.EnsureMember(context.Background(), "apnet-abcd1234", "vlan2")
	if err == nil {
		t.Fatal("EnsureMember() error = nil, want unknown without a parent or a configured uplink")
	}
	if membership.State != MembershipUnknown {
		t.Errorf("state = %v, want %v", membership.State, MembershipUnknown)
	}
	if h.issued("addm") {
		t.Errorf("an addm was issued: %v", h.calls)
	}
}

// When ifconfig does not print a parent line, the configured uplink plus
// one observation of it is the fallback - and it must still get a plain
// NIC right, which is every node in this project's own fleet.
func TestObserveSVI_FallsBackToTheConfiguredUplinkWhenNoParentLine(t *testing.T) {
	// Each case needs its own host and its own configured uplink: the
	// fallback answers from the configuration, so the two cases differ
	// only in which interface this Manager was told to tag onto.
	bridgeUplink := newFakeHost()
	bridgeUplink.renderParent = false
	bridgeUplink.bridge("bridge0").vlan("vlan2", 2, "bridge0")
	state, parent, err := newTestManager(bridgeUplink, "bridge0").observeSVI(context.Background(), "vlan2")
	if err != nil {
		t.Fatalf("observeSVI() error: %v", err)
	}
	if state != sviOnBridge || parent != "bridge0" {
		t.Errorf("bridge uplink: observeSVI() = (%v, %q), want (%v, %q)", state, parent, sviOnBridge, "bridge0")
	}

	nicUplink := newFakeHost()
	nicUplink.renderParent = false
	nicUplink.nic("em0").vlan("vlan7", 7, "em0")
	state, parent, err = newTestManager(nicUplink, "em0").observeSVI(context.Background(), "vlan7")
	if err != nil {
		t.Fatalf("observeSVI() error: %v", err)
	}
	if state != sviNo || parent != "em0" {
		t.Errorf("NIC uplink: observeSVI() = (%v, %q), want (%v, %q)", state, parent, sviNo, "em0")
	}
}

// Where ifconfig DOES name the parent, the observation of the interface
// wins over the configured uplink - which is what keeps a same-named
// interface somebody else created (tagged onto a NIC) usable on a node
// whose uplink is a bridge, instead of being misclassified as an SVI.
func TestObserveSVI_ObservedParentWinsOverTheConfiguredUplink(t *testing.T) {
	h := newFakeHost().bridge("bridge0").nic("re0").bridge("apnet-abcd1234").vlan("vlan2", 2, "re0")
	m := newTestManager(h, "bridge0") // configured uplink is the bridge

	state, parent, err := m.observeSVI(context.Background(), "vlan2")
	if err != nil {
		t.Fatalf("observeSVI() error: %v", err)
	}
	if state != sviNo || parent != "re0" {
		t.Errorf("observeSVI() = (%v, %q), want (%v, %q) - the observed parent is re0", state, parent, sviNo, "re0")
	}
	// ... and it is therefore addable, which is the behavior that would
	// have been lost by trusting the configuration alone.
	membership, err := m.EnsureMember(context.Background(), "apnet-abcd1234", "vlan2")
	if err != nil {
		t.Fatalf("EnsureMember() error: %v", err)
	}
	if membership.State != MembershipAdded {
		t.Errorf("state = %v, want %v", membership.State, MembershipAdded)
	}
}

// --- Blast radius: an epair(4) end is never an SVI ---

// internal/jailnet calls EnsureMember for an epair host side on the very
// same Manager. An epair is an ordinary Ethernet interface, so it must be
// added even on a node whose configured uplink is a bridge - the VLAN
// rule must not reach it.
func TestEnsureMember_EpairIsAddedEvenWithABridgeUplink(t *testing.T) {
	h := newFakeHost().bridge("bridge0").bridge("apnet-jail01").nic("epair0a")
	m := newTestManager(h, "bridge0")

	membership, err := m.EnsureMember(context.Background(), "apnet-jail01", "epair0a")
	if err != nil {
		t.Fatalf("EnsureMember(epair0a) error: %v", err)
	}
	if membership.State != MembershipAdded {
		t.Errorf("state = %v, want %v", membership.State, MembershipAdded)
	}
	if !h.memberOf("apnet-jail01", "epair0a") {
		t.Error("epair0a was not added to the jail's bridge")
	}
}

// The full epair path (ADR-0117), on a bridge-uplink node: the pair is
// created, brought up, and joined. This is the regression guard for the
// jail networking path as a whole, not just for the membership call.
func TestEnsureEpair_OnABridgeUplinkNode(t *testing.T) {
	h := newFakeHost().bridge("bridge0").bridge("apnet-jail01")
	m := newTestManager(h, "bridge0")

	hostSide, jailSide, err := m.EnsureEpair(context.Background(), "apnet-jail01")
	if err != nil {
		t.Fatalf("EnsureEpair() error: %v", err)
	}
	if !strings.HasSuffix(hostSide, "a") || !strings.HasSuffix(jailSide, "b") {
		t.Fatalf("EnsureEpair() = (%q, %q), want the usual a/b ends", hostSide, jailSide)
	}
	if !h.memberOf("apnet-jail01", hostSide) {
		t.Error("the epair host side was not added to the jail's bridge")
	}
}

// An epair that somehow came back reported as an SVI is a failure, not a
// quiet success: EnsureEpair destroys the pair rather than handing back a
// host side that was never joined to anything. The state is one no kernel
// produces - an epair end is not a VLAN - which is why sviEpairHost has
// to fake the interface's own output to reach it at all.
func TestEnsureEpair_RefusesAnSVIReportRatherThanClaimingSuccess(t *testing.T) {
	host := &sviEpairHost{fakeHost: newFakeHost(), sviParent: "apnet-jail01"}
	host.bridge("bridge0").bridge("apnet-jail01")
	m := &Manager{Uplink: "bridge0", Runner: host, Bridges: host}

	if _, _, err := m.EnsureEpair(context.Background(), "apnet-jail01"); err == nil {
		t.Fatal("EnsureEpair() error = nil, want a refusal when the host side is reported as a Bridge SVI")
	} else if !strings.Contains(err.Error(), "Bridge SVI") {
		t.Errorf("error does not name the problem: %v", err)
	}
	if host.memberOf("apnet-jail01", "epair0a") {
		t.Error("a pair was left joined to the bridge after the refusal")
	}
	if _, present := host.ifaces["epair0a"]; present {
		t.Error("the epair was left on the host after the refusal")
	}
}

// --- Pure parse helpers, exercised from fixtures ---

func TestVLANParentName(t *testing.T) {
	const withParent = `vlan2: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0 mtu 1500
	options=9c<RXCSUM,TXCSUM,VLAN_MTU,VLAN_HWTAGGING,VLAN_HWCSUM,VLAN_HWFILTER>
	ether 00:00:00:00:00:00
	Parent name: bridge0
	inet 10.90.2.1 netmask 0xffffff00 broadcast 10.90.2.255
	vlan: 2 vlanpcp: 0 vlanhwtag: 1
	media: Ethernet autoselect (1000baseT <full-duplex>)
	status: active
`
	const withoutParent = `vlan2: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0 mtu 1500
	ether 00:00:00:00:00:00
	vlan: 2 vlanpcp: 0 vlanhwtag: 1
`
	const notAVLAN = `em0: flags=1008943<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0 mtu 1500
	inet 10.90.0.94 netmask 0xffffff00 broadcast 10.90.0.255
	media: Ethernet autoselect (1000baseT <full-duplex>)
`
	cases := []struct {
		name         string
		out          string
		wantParent   string
		wantIsVLANIf bool
	}{
		{"parent line present", withParent, "bridge0", true},
		{"no parent line", withoutParent, "", true},
		{"not a vlan interface", notAVLAN, "", false},
		{"empty", "", "", false},
	}
	for _, c := range cases {
		if got := vlanParentName(c.out); got != c.wantParent {
			t.Errorf("%s: vlanParentName() = %q, want %q", c.name, got, c.wantParent)
		}
		if got := isVLANInterface(c.out); got != c.wantIsVLANIf {
			t.Errorf("%s: isVLANInterface() = %v, want %v", c.name, got, c.wantIsVLANIf)
		}
	}
}

// A bridge's own member line must not be mistaken for a VLAN line by a
// substring match - the check is anchored at the start of a trimmed line.
func TestIsVLANInterface_IgnoresAMemberNamedLikeAVLAN(t *testing.T) {
	const out = `bridge0: flags=1008843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0 mtu 1500
	member: vlan9 flags=143<LEARNING,DISCOVER>
	        port 1 priority 128 path cost 20000 vlan protocol 802.1q
	groups: bridge
`
	if isVLANInterface(out) {
		t.Error("isVLANInterface() = true for a bridge block; the vlan protocol text is not a VLAN interface")
	}
}

func TestMembershipStateString(t *testing.T) {
	for state, want := range map[MembershipState]string{
		MembershipUnknown:  "unknown",
		MembershipPresent:  "present",
		MembershipAdded:    "added",
		MembershipSVI:      "bridge_svi",
		MembershipState(9): "MembershipState(9)",
	} {
		if got := state.String(); got != want {
			t.Errorf("MembershipState(%d).String() = %q, want %q", int(state), got, want)
		}
	}
}

// sviEpairHost reports a freshly created epair host side as a VLAN whose
// parent is the very bridge it is being joined to. No kernel produces
// this - an epair(4) end is not a vlan(4) interface - and it exists only
// to reach EnsureEpair's own guard, which would otherwise be
// unreachable code with no way to prove it.
type sviEpairHost struct {
	*fakeHost
	sviParent string
}

func (h *sviEpairHost) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := h.fakeHost.Run(ctx, name, args...)
	if err != nil || name != "ifconfig" || len(args) != 1 {
		return out, err
	}
	if strings.HasPrefix(args[0], "epair") && strings.HasSuffix(args[0], "a") {
		return fmt.Sprintf("%s: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0 mtu 1500\n\tParent name: %s\n\tvlan: 1 vlanpcp: 0 vlanhwtag: 1\n", args[0], h.sviParent), nil
	}
	return out, nil
}
