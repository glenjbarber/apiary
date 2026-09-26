package jail

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Real `ifconfig epair0b` output from inside a VNET jail, the shape
// ADR-0117's addressing depends on being parsed.
const sampleEpairIfconfig = `epair0b: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0 mtu 1500
	options=3<RXCSUM,TXCSUM,VLAN_MTU>
	ether 02:1a:2b:3c:4d:5e
	inet 10.0.1.5 netmask 0xffffff00 broadcast 10.0.1.255
	media: Ethernet autoselect (1000baseT <full-duplex>)
	status: active
	nd6 options=29<PERFORMNUD,IFDISABLED,AUTO_LINKLOCAL>
`

func TestParseIfconfigAddresses(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []Address
	}{
		{
			name: "classic hex netmask",
			out:  sampleEpairIfconfig,
			want: []Address{{IP: "10.0.1.5", PrefixLen: 24}},
		},
		{
			name: "inline prefix length",
			out:  "epair0b: flags=8843<UP>\n\tinet 192.168.4.9/26\n",
			want: []Address{{IP: "192.168.4.9", PrefixLen: 26}},
		},
		{
			name: "dotted-quad netmask",
			out:  "epair0b: flags=8843<UP>\n\tinet 172.16.0.7 netmask 255.255.255.128\n",
			want: []Address{{IP: "172.16.0.7", PrefixLen: 25}},
		},
		{
			name: "dotted-hex netmask",
			out:  "epair0b: flags=8843<UP>\n\tinet 172.16.0.7 netmask 0xff.ff.ff.80\n",
			want: []Address{{IP: "172.16.0.7", PrefixLen: 25}},
		},
		{
			name: "no prefix information at all is not guessed at",
			// Refusing here matters: a false "no addresses" reading
			// turns a parser gap into drift, and drift into a
			// redundant - and ifconfig-failing - re-add, every tick.
			out:  "epair0b: flags=8843<UP>\n\tinet 10.0.1.5\n",
			want: nil,
		},
		{
			name: "unindented inet line is not an address assignment",
			out:  "inet 10.0.1.5 netmask 0xffffff00\n",
			want: nil,
		},
		{
			name: "inet6 is out of scope for this IPv4-only design",
			out:  "epair0b: flags=8843<UP>\n\tinet6 fe80::1%epair0b prefixlen 64 scopeid 0x1\n",
			want: nil,
		},
		{
			name: "multiple addresses keep ifconfig's order",
			out: "epair0b: flags=8843<UP>\n" +
				"\tinet 10.0.1.5 netmask 0xffffff00 broadcast 10.0.1.255\n" +
				"\tinet 10.0.1.6 netmask 0xffffff00 broadcast 10.0.1.255\n",
			want: []Address{
				{IP: "10.0.1.5", PrefixLen: 24},
				{IP: "10.0.1.6", PrefixLen: 24},
			},
		},
		{
			name: "empty output means no addresses",
			out:  "",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIfconfigAddresses(tt.out)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseIfconfigAddresses() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNetmaskPrefixLen(t *testing.T) {
	tests := []struct {
		in   string
		want int
		ok   bool
	}{
		{in: "0xffffff00", want: 24, ok: true},
		{in: "ffffff00", want: 24, ok: true},
		{in: "0xff.ff.ff.00", want: 24, ok: true},
		{in: "255.255.255.0", want: 25 - 1, ok: true},
		{in: "0xffffffc0", want: 26, ok: true},
		{in: "0xff000000", want: 8, ok: true},
		{in: "0x00000000", want: 0, ok: true},
		{in: "0xffffffff", want: 32, ok: true},
		// A non-contiguous mask is not a prefix length at all; Size()
		// alone would report its total set bits and silently invent
		// /24 out of 0xff00ff00.
		{in: "0xff00ff00", ok: false},
		{in: "", ok: false},
		{in: "not-a-mask", ok: false},
		{in: "0x", ok: false},
		{in: "255.255.255", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := netmaskPrefixLen(tt.in)
			if ok != tt.ok || (ok && got != tt.want) {
				t.Errorf("netmaskPrefixLen(%q) = %d, %v; want %d, %v", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

const sampleNetstatRoutes = `Routing tables

Destination        Gateway            Netif Addrs
0.0.0.0/0          10.0.1.1           epair0b  U
10.0.1.0/24        link#1             epair0b  U
10.0.1.5           link#1             epair0b  UH
`

func TestParseNetstatDefaultRoute(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{name: "default via gateway", out: sampleNetstatRoutes, want: "10.0.1.1"},
		{
			name: "legacy bare default destination",
			out:  "Destination      Gateway       Netif Addrs\ndefault          192.168.1.1    epair0b  U\n",
			want: "192.168.1.1",
		},
		{
			name: "no default row means no default route",
			out:  "Destination        Gateway            Netif Addrs\n10.0.1.0/24        link#1             epair0b  U\n",
			want: "",
		},
		{
			name: "an unparseable default row is not guessed at",
			out:  "Destination        Gateway            Netif Addrs\n0.0.0.0/0          link#1             epair0b  U\n",
			want: "",
		},
		{name: "empty routing table", out: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseNetstatDefaultRoute(tt.out); got != tt.want {
				t.Errorf("parseNetstatDefaultRoute() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestObserveNet_Present confirms the whole-success path: a jail with
// the expected address and default route is reported as exactly that.
func TestObserveNet_Present(t *testing.T) {
	f := newFakeRunner().
		on("jexec apiary-web-1 ifconfig epair0b", sampleEpairIfconfig).
		on("jexec apiary-web-1 netstat -rn -f inet", sampleNetstatRoutes)
	m := New("apiary-")
	m.Runner = f

	state, err := m.ObserveNet(context.Background(), "apiary-web-1", "epair0b")
	if err != nil {
		t.Fatalf("ObserveNet() error = %v", err)
	}
	if !state.InterfacePresent {
		t.Errorf("InterfacePresent = false, want true")
	}
	if want := []Address{{IP: "10.0.1.5", PrefixLen: 24}}; !reflect.DeepEqual(state.Addresses, want) {
		t.Errorf("Addresses = %v, want %v", state.Addresses, want)
	}
	if state.DefaultRoute != "10.0.1.1" {
		t.Errorf("DefaultRoute = %q, want %q", state.DefaultRoute, "10.0.1.1")
	}
}

// TestObserveNet_InterfaceAbsentIsNotUnknown confirms the one case
// where InterfacePresent is legitimately false: ifconfig gave a
// definite "no such interface" answer, and nothing else failed.
func TestObserveNet_InterfaceAbsentIsNotUnknown(t *testing.T) {
	f := newFakeRunner().
		onErr("jexec apiary-web-1 ifconfig epair0b", "ifconfig: interface epair0b does not exist")
	m := New("apiary-")
	m.Runner = f

	state, err := m.ObserveNet(context.Background(), "apiary-web-1", "epair0b")
	if err != nil {
		t.Fatalf("ObserveNet() error = %v, want nil for a definite absent interface", err)
	}
	if state.InterfacePresent {
		t.Errorf("InterfacePresent = true, want false")
	}
	if state.DefaultRoute != "" || len(state.Addresses) != 0 {
		t.Errorf("state = %+v, want a bare absent-interface state with no other fields populated", state)
	}
}

// TestObserveNet_UnknownIsNotAbsent is the honesty test: an ifconfig
// failure that is NOT a "does not exist" answer must surface as an
// error (which every caller reports as unknown), never as a state
// claiming the interface is gone. Folding the two together would let a
// permission error or a missing binary silently convince Apiary that a
// jail's interface vanished.
func TestObserveNet_UnknownIsNotAbsent(t *testing.T) {
	for _, msg := range []string{
		"jexec: exec failed: operation not permitted",
		"jexec: jail not running",
		"sh: ifconfig: not found",
	} {
		t.Run(msg, func(t *testing.T) {
			f := newFakeRunner().onErr("jexec apiary-web-1 ifconfig epair0b", msg)
			m := New("apiary-")
			m.Runner = f

			state, err := m.ObserveNet(context.Background(), "apiary-web-1", "epair0b")
			if err == nil {
				t.Fatalf("ObserveNet() = %+v, want an error (unknown), not a definite state", state)
			}
			if state.InterfacePresent || len(state.Addresses) != 0 || state.DefaultRoute != "" {
				t.Errorf("state = %+v, want the zero value alongside the error: nothing was observed", state)
			}
			// The routing table must not even be consulted once the
			// first observation already failed - a half-observation
			// would look like a half-missing configuration.
			if n := f.count(); n != 1 {
				t.Errorf("ran %d commands, want exactly 1 (stop at the first failed observation)", n)
			}
		})
	}
}

// TestObserveNet_RouteFailureIsUnknownNotHalfATrue confirms that a
// successful interface read followed by a failed route read yields no
// state at all, rather than a state that looks correct about the
// interface and silently empty about routing.
func TestObserveNet_RouteFailureIsUnknownNotHalfATrue(t *testing.T) {
	f := newFakeRunner().
		on("jexec apiary-web-1 ifconfig epair0b", sampleEpairIfconfig).
		onErr("jexec apiary-web-1 netstat -rn -f inet", "jexec: jail not found")
	m := New("apiary-")
	m.Runner = f

	state, err := m.ObserveNet(context.Background(), "apiary-web-1", "epair0b")
	if err == nil {
		t.Fatalf("ObserveNet() = %+v, want an error when the routing table cannot be read", state)
	}
	if state.InterfacePresent {
		t.Errorf("state = %+v, want the zero value: a partial observation is not a state", state)
	}
}

// TestEnsureAddressing_Idempotent is the central property: reconciling
// an already-correct jail must issue no commands at all, not re-run
// ifconfig and hope it is idempotent. ifconfig in fact refuses a
// duplicate address with "File exists" and route refuses a second
// default with "File exists", so a re-issuing reconciler would break
// on the second tick of every jail.
func TestEnsureAddressing_Idempotent(t *testing.T) {
	f := newFakeRunner().
		on("jexec apiary-web-1 ifconfig epair0b", sampleEpairIfconfig).
		on("jexec apiary-web-1 netstat -rn -f inet", sampleNetstatRoutes)
	m := New("apiary-")
	m.Runner = f
	addr := Address{IP: "10.0.1.5", PrefixLen: 24}

	for i := 0; i < 3; i++ {
		if err := m.EnsureAddressing(context.Background(), "apiary-web-1", "epair0b", addr, "10.0.1.1"); err != nil {
			t.Fatalf("pass %d: EnsureAddressing() error = %v", i+1, err)
		}
	}
	// 2 reads per pass, 3 passes, and nothing else.
	if got := f.count(); got != 6 {
		t.Errorf("ran %d commands over 3 passes, want 6 (2 observation reads per pass, zero repairs)", got)
	}
}

// TestEnsureAddressing_RepairsEachKindOfDrift walks every drift the
// reconciler is meant to repair, in one pass each.
func TestEnsureAddressing_RepairsEachKindOfDrift(t *testing.T) {
	tests := []struct {
		name       string
		ifconfig   string
		netstat    string
		addr       Address
		gw         string
		wantWrites []string
	}{
		{
			name:     "missing address is added",
			ifconfig: "epair0b: flags=8843<UP>\n\tether 02:1a:2b:3c:4d:5e\n",
			netstat:  "Routing tables\n",
			addr:     Address{IP: "10.0.1.5", PrefixLen: 24},
			gw:       "",
			wantWrites: []string{
				"jexec apiary-web-1 ifconfig epair0b inet 10.0.1.5/24 up",
			},
		},
		{
			name:     "a foreign address is removed, not left alongside",
			ifconfig: "epair0b: flags=8843<UP>\n\tinet 10.0.1.5 netmask 0xffffff00\n\tinet 10.0.1.99 netmask 0xffffff00\n",
			netstat:  "Routing tables\n",
			addr:     Address{IP: "10.0.1.5", PrefixLen: 24},
			gw:       "",
			wantWrites: []string{
				"jexec apiary-web-1 ifconfig epair0b inet 10.0.1.99 delete",
			},
		},
		{
			name:     "a wrong prefix length is replaced, not left as-is",
			ifconfig: "epair0b: flags=8843<UP>\n\tinet 10.0.1.5 netmask 0xffff0000\n",
			netstat:  "Routing tables\n",
			addr:     Address{IP: "10.0.1.5", PrefixLen: 24},
			gw:       "",
			wantWrites: []string{
				"jexec apiary-web-1 ifconfig epair0b inet 10.0.1.5 delete",
				"jexec apiary-web-1 ifconfig epair0b inet 10.0.1.5/24 up",
			},
		},
		{
			name:     "a missing default route is added",
			ifconfig: sampleEpairIfconfig,
			netstat:  "Routing tables\n",
			addr:     Address{IP: "10.0.1.5", PrefixLen: 24},
			gw:       "10.0.1.1",
			wantWrites: []string{
				"jexec apiary-web-1 route add default 10.0.1.1",
			},
		},
		{
			name: "a wrong gateway is replaced, not left as-is",
			// `route add` fails outright when a default route
			// already exists, so the old one must go first.
			ifconfig: sampleEpairIfconfig,
			netstat:  "Routing tables\n0.0.0.0/0          10.0.1.254         epair0b  U\n",
			addr:     Address{IP: "10.0.1.5", PrefixLen: 24},
			gw:       "10.0.1.1",
			wantWrites: []string{
				"jexec apiary-web-1 route delete default",
				"jexec apiary-web-1 route add default 10.0.1.1",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRunner().
				on("jexec apiary-web-1 ifconfig epair0b", tt.ifconfig).
				on("jexec apiary-web-1 netstat -rn -f inet", tt.netstat)
			for _, w := range tt.wantWrites {
				f.on(w, "")
			}
			m := New("apiary-")
			m.Runner = f

			if err := m.EnsureAddressing(context.Background(), "apiary-web-1", "epair0b", tt.addr, tt.gw); err != nil {
				t.Fatalf("EnsureAddressing() error = %v", err)
			}
			for _, w := range tt.wantWrites {
				if n := f.callsTo(w); n != 1 {
					t.Errorf("command %q ran %d times, want exactly 1", w, n)
				}
			}
			// Reads plus exactly the scripted writes: no more.
			if got, want := f.count(), 2+len(tt.wantWrites); got != want {
				t.Errorf("ran %d commands, want %d (2 reads + %d writes)", got, want, len(tt.wantWrites))
			}
		})
	}
}

// TestEnsureAddressing_EmptyGatewayLeavesRoutingAlone confirms the
// ADR-0117 "no address at all" carve-out: a jail with no allocated IP
// (e.g. an uplink_bridged network, which skips allocation exactly as it
// does for VMs) must not have a default route invented, added, or
// deleted on its behalf.
func TestEnsureAddressing_EmptyGatewayLeavesRoutingAlone(t *testing.T) {
	f := newFakeRunner().
		on("jexec apiary-web-1 ifconfig epair0b", sampleEpairIfconfig).
		on("jexec apiary-web-1 netstat -rn -f inet", sampleNetstatRoutes)
	m := New("apiary-")
	m.Runner = f

	if err := m.EnsureAddressing(context.Background(), "apiary-web-1", "epair0b", Address{IP: "10.0.1.5", PrefixLen: 24}, ""); err != nil {
		t.Fatalf("EnsureAddressing() error = %v", err)
	}
	for _, forbidden := range []string{
		"jexec apiary-web-1 route add default 10.0.1.1",
		"jexec apiary-web-1 route delete default",
	} {
		if n := f.callsTo(forbidden); n != 0 {
			t.Errorf("command %q ran %d times, want 0: an unallocated jail's routing is its own business", forbidden, n)
		}
	}
}

// TestEnsureAddressing_MissingInterfaceNeedsRestart confirms the one
// case that cannot be repaired in place is reported as an error rather
// than silently succeeding: jail(8) has no way to add a vnet
// interface to an already-running jail, so "nothing to do" here would
// mean the jail stays broken forever while every tick reports success.
func TestEnsureAddressing_MissingInterfaceNeedsRestart(t *testing.T) {
	f := newFakeRunner().
		onErr("jexec apiary-web-1 ifconfig epair0b", "ifconfig: interface epair0b does not exist")
	m := New("apiary-")
	m.Runner = f

	err := m.EnsureAddressing(context.Background(), "apiary-web-1", "epair0b", Address{IP: "10.0.1.5", PrefixLen: 24}, "10.0.1.1")
	if err == nil {
		t.Fatalf("EnsureAddressing() error = nil, want a restart-required error")
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Errorf("error = %v, want it to name the restart that is actually required", err)
	}
	if n := f.count(); n != 1 {
		t.Errorf("ran %d commands, want 1: an absent interface must not be answered with a route add", n)
	}
}

// TestObserveNet_JailGoneIsNotInterfaceGone is the distinction the VNET
// reconciler most needs: a jexec that cannot find the jail and an
// ifconfig that cannot find the interface are both "not found", but
// they demand opposite responses - stop reconciling this jail, versus
// restart it because its vnet interface went missing. Reporting the
// first as the second would make every reconciler tick ask for a
// pointless restart of a jail that simply isn't up.
func TestObserveNet_JailGoneIsNotInterfaceGone(t *testing.T) {
	for _, msg := range []string{
		"jls: apiary-web-1: not found",
		"jexec: jail not found",
	} {
		t.Run(msg, func(t *testing.T) {
			f := newFakeRunner().onErr("jexec apiary-web-1 ifconfig epair0b", msg)
			m := New("apiary-")
			m.Runner = f

			state, err := m.ObserveNet(context.Background(), "apiary-web-1", "epair0b")
			if !errors.Is(err, ErrJailNotFound) {
				t.Fatalf("ObserveNet() error = %v, want it to wrap ErrJailNotFound", err)
			}
			if state.InterfacePresent {
				t.Errorf("state = %+v, want the zero value: nothing was observed", state)
			}
			// The evidence behind the verdict must survive, not be
			// replaced by a bare sentinel - a caller escalating this
			// to an operator needs the tool's own words.
			if !strings.Contains(err.Error(), msg) {
				t.Errorf("error = %v, want it to retain the tool's own wording as evidence", err)
			}
		})
	}
}

// TestObserveNet_MissingBinaryInsideJailIsUnknown confirms a shell
// report about the *inner* command still reads as unknown rather than
// as an absent interface: nothing was learned about the interface, so
// nothing may be concluded about it.
func TestObserveNet_MissingBinaryInsideJailIsUnknown(t *testing.T) {
	f := newFakeRunner().onErr("jexec apiary-web-1 ifconfig epair0b", "sh: ifconfig: not found")
	m := New("apiary-")
	m.Runner = f

	state, err := m.ObserveNet(context.Background(), "apiary-web-1", "epair0b")
	if err == nil {
		t.Fatalf("ObserveNet() = %+v, want an error", state)
	}
	if errors.Is(err, ErrJailNotFound) {
		t.Errorf("error = %v, want it NOT to claim the jail is gone: the jail answered, ifconfig did not", err)
	}
	if state.InterfacePresent {
		t.Errorf("state = %+v, want the zero value", state)
	}
}

// TestEnsureAddressing_RejectsUnusableAddressing confirms the
// reconciler refuses to write a nonsense address or gateway rather than
// passing it to ifconfig, and refuses before it observes anything -
// there is no point reading the jail's state in order to go on to do
// nothing with it.
func TestEnsureAddressing_RejectsUnusableAddressing(t *testing.T) {
	tests := []struct {
		name string
		addr Address
		gw   string
	}{
		{name: "not an address", addr: Address{IP: "not-an-ip", PrefixLen: 24}},
		{name: "IPv6 is out of scope", addr: Address{IP: "2001:db8::1", PrefixLen: 64}},
		{name: "prefix length out of range", addr: Address{IP: "10.0.1.5", PrefixLen: 33}},
		{name: "unusable gateway", addr: Address{IP: "10.0.1.5", PrefixLen: 24}, gw: "nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRunner()
			m := New("apiary-")
			m.Runner = f

			if err := m.EnsureAddressing(context.Background(), "apiary-web-1", "epair0b", tt.addr, tt.gw); err == nil {
				t.Fatalf("EnsureAddressing() error = nil, want a rejection of %v/%q", tt.addr, tt.gw)
			}
			if n := f.count(); n != 0 {
				t.Errorf("ran %d commands, want 0: bad desired state is rejected before any observation or write", n)
			}
		})
	}
}

// TestEnsureAddressing_AcceptsUnnormalizedInput confirms idempotency
// does not depend on the caller having spelled the address exactly the
// way ifconfig renders it: an equivalently-written address must still
// be recognized as already-correct rather than deleted and re-added on
// every single tick.
func TestEnsureAddressing_AcceptsUnnormalizedInput(t *testing.T) {
	f := newFakeRunner().
		on("jexec apiary-web-1 ifconfig epair0b", sampleEpairIfconfig).
		on("jexec apiary-web-1 netstat -rn -f inet", sampleNetstatRoutes)
	m := New("apiary-")
	m.Runner = f

	if err := m.EnsureAddressing(context.Background(), "apiary-web-1", "epair0b", Address{IP: "10.0.1.5", PrefixLen: 24}, "10.0.1.1"); err != nil {
		t.Fatalf("EnsureAddressing() error = %v", err)
	}
	if got := f.count(); got != 2 {
		t.Errorf("ran %d commands, want 2 (observation only)", got)
	}
}
