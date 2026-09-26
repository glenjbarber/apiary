package netif

import (
	"errors"
	"reflect"
	"testing"
)

// Fixtures below are verbatim ifconfig(8) output captured on brood
// (10.90.0.94, FreeBSD 16.0-CURRENT) - the host that reported the
// ADR-0055 "NAT uplink owns default route" false. The parser is pure
// specifically so these can be exercised here instead of only on a
// FreeBSD testbed.

// bridge0 with em0 attached, exactly as brood printed it.
const broodBridge0Ifconfig = `bridge0: flags=1008843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST,LOWER_UP> metric 0 mtu 1500
	options=10<VLAN_HWTAGGING>
	ether 3c:97:0e:e9:15:7c
	inet 10.90.0.94 netmask 0xffffff00 broadcast 10.90.0.255
	id 00:00:00:00:00:00 priority 32768 hellotime 2 fwddelay 15
	maxage 20 holdcnt 6 proto rstp maxaddr 2000 timeout 1200
	root id 00:00:00:00:00:00 priority 32768 ifcost 0 port 0
	bridge flags=0<>
	member: em0 flags=143<LEARNING,DISCOVER,AUTOEDGE,AUTOPTP>
	        port 1 priority 128 path cost 20000 vlan protocol 802.1q
	groups: bridge
	nd6 options=809<PERFORMNUD,IFDISABLED,STABLEADDR>
`

// The same member NIC, seen on its own: no `groups:` line at all, and no
// IPv4 address because the installer left the member addressless.
const broodEm0Ifconfig = `em0: flags=1008943<UP,BROADCAST,RUNNING,PROMISC,SIMPLEX,MULTICAST,LOWER_UP> metric 0 mtu 1500
	options=4e520bb<RXCSUM,TXCSUM,VLAN_MTU,VLAN_HWTAGGING,JUMBO_MTU,VLAN_HWCSUM,WOL_MAGIC,VLAN_HWFILTER,VLAN_HWTSO,RXCSUM_IPV6,TXCSUM_IPV6,HWSTATS,MEXTPG>
	ether 3c:97:0e:e9:15:7c
	inet6 fe80::3e97:eff:fee9:157c%em0 prefixlen 64 scopeid 0x1
	media: Ethernet autoselect (1000baseT <full-duplex>)
	status: active
	nd6 options=823<PERFORMNUD,ACCEPT_RTADV,AUTO_LINKLOCAL,STABLEADDR>
`

const broodIfconfigA = broodEm0Ifconfig + `
lo0: flags=1008049<UP,LOOPBACK,RUNNING,MULTICAST,LOWER_UP> metric 0 mtu 16384
	options=680003<RXCSUM,TXCSUM,LINKSTATE,RXCSUM_IPV6,TXCSUM_IPV6>
	inet6 ::1 prefixlen 128
	inet6 fe80::1%lo0 prefixlen 64 scopeid 0x2
	inet 127.0.0.1 netmask 0xff000000
	nd6 options=21<PERFORMNUD,AUTO_LINKLOCAL,IFDISABLED>
	groups: lo
` + broodBridge0Ifconfig

func TestParseBridgeIfconfigOutput_BroodBridge(t *testing.T) {
	members, isBridge, err := parseBridgeIfconfigOutput("bridge0", broodBridge0Ifconfig, nil)
	if err != nil {
		t.Fatalf("parseBridgeIfconfigOutput() error = %v, want nil", err)
	}
	if !isBridge {
		t.Error("isBridge = false, want true (bridge0 prints `groups: bridge`)")
	}
	if want := []string{"em0"}; !reflect.DeepEqual(members, want) {
		t.Errorf("members = %v, want %v", members, want)
	}
}

// The bridged topology must be readable out of multi-block `ifconfig -a`
// output too, which is the shape a future switch to -a would take. A
// `groups: lo` on lo0 must not be mistaken for a bridge, and em0's block
// must not contribute members to bridge0.
func TestParseBridgeIfconfigOutput_BroodIfconfigA(t *testing.T) {
	members, isBridge, err := parseBridgeIfconfigOutput("bridge0", broodIfconfigA, nil)
	if err != nil {
		t.Fatalf("parseBridgeIfconfigOutput() error = %v, want nil", err)
	}
	if !isBridge {
		t.Error("isBridge = false, want true")
	}
	if want := []string{"em0"}; !reflect.DeepEqual(members, want) {
		t.Errorf("members = %v, want %v", members, want)
	}

	// The ordinary-NIC case: a real, reportable "not a bridge", which is
	// what keeps a genuine mismatch reporting false rather than unknown.
	members, isBridge, err = parseBridgeIfconfigOutput("em0", broodEm0Ifconfig, nil)
	if err != nil {
		t.Fatalf("parseBridgeIfconfigOutput() error = %v, want nil", err)
	}
	if isBridge {
		t.Error("isBridge = true for em0, want false")
	}
	if len(members) != 0 {
		t.Errorf("members = %v, want none for an ordinary NIC", members)
	}
}

// A bridge with zero members is legal and must still be reported as a
// bridge, so the caller can distinguish "bridge, uplink not attached"
// (a mismatch) from "not a bridge at all".
func TestParseBridgeIfconfigOutput_BridgeWithNoMembers(t *testing.T) {
	const empty = `bridge1: flags=1008843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST,LOWER_UP> metric 0 mtu 1500
	ether 02:00:00:00:00:01
	groups: bridge
`
	members, isBridge, err := parseBridgeIfconfigOutput("bridge1", empty, nil)
	if err != nil {
		t.Fatalf("parseBridgeIfconfigOutput() error = %v, want nil", err)
	}
	if !isBridge {
		t.Error("isBridge = false, want true for a memberless bridge")
	}
	if len(members) != 0 {
		t.Errorf("members = %v, want none", members)
	}
}

func TestParseBridgeIfconfigOutput_MultipleMembersSorted(t *testing.T) {
	const two = `bridge2: flags=1008843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST,LOWER_UP> metric 0 mtu 1500
	member: vtnet0 flags=143<LEARNING,DISCOVER,AUTOEDGE,AUTOPTP>
	        port 1 priority 128 path cost 20000
	member: em0 flags=143<LEARNING,DISCOVER,AUTOEDGE,AUTOPTP>
	        port 2 priority 128 path cost 20000
	groups: bridge
`
	members, isBridge, err := parseBridgeIfconfigOutput("bridge2", two, nil)
	if err != nil || !isBridge {
		t.Fatalf("got isBridge=%v err=%v, want true/nil", isBridge, err)
	}
	// Sorted so the answer is deterministic regardless of ifconfig's
	// port ordering.
	if want := []string{"em0", "vtnet0"}; !reflect.DeepEqual(members, want) {
		t.Errorf("members = %v, want %v", members, want)
	}
}

// A non-zero exit is a measurement failure, never evidence of absence: the
// caller must be able to report unknown rather than a false mismatch.
func TestParseBridgeIfconfigOutput_ExecFailureIsNotAbsence(t *testing.T) {
	members, isBridge, err := parseBridgeIfconfigOutput("bridge0", "", errors.New("exit status 1"))
	if err == nil {
		t.Fatal("err = nil, want the exec failure surfaced to the caller")
	}
	if isBridge {
		t.Error("isBridge = true on exec failure, want false")
	}
	if members != nil {
		t.Errorf("members = %v, want nil on exec failure", members)
	}
}

// Exiting cleanly but printing no block for the interface asked about is
// equally a failure to measure, and must not be read as "not a bridge".
func TestParseBridgeIfconfigOutput_NoBlockForRequestedInterface(t *testing.T) {
	_, isBridge, err := parseBridgeIfconfigOutput("bridge9", broodIfconfigA, nil)
	if err == nil {
		t.Fatal("err = nil, want unrecognized-output surfaced to the caller")
	}
	if isBridge {
		t.Error("isBridge = true for an interface with no block, want false")
	}
}

func TestBridgeMembers_EmptyNameIsAnError(t *testing.T) {
	if _, _, err := BridgeMembers(t.Context(), ""); err == nil {
		t.Fatal("BridgeMembers(\"\") error = nil, want an error for an empty interface name")
	}
}
