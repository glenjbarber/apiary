package jailnet

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/glenjbarber/apiary/internal/jail"
)

// Aliases so the fakes below read as the jail package's own types
// rather than as a re-declaration of them: the point of a fake is to
// model the real dependency, not to invent a parallel one.
type (
	jailNetState = jail.JailNetState
	jailAddress  = jail.Address
)

// errJailGone is the real sentinel, aliased so the fake models the
// dependency rather than a parallel invention of it: a test exercising
// the "jail stopped mid-observation" path must be exercising the same
// errors.Is check the production caller uses.
var errJailGone = jail.ErrJailNotFound

// fakeRunner is a scripted host-side command runner, and the single
// source of truth for what a test believes this node's interfaces look
// like.
//
// It is not just a log: `ifconfig <iface> up`, `addm` and `deletem`
// actually change the modelled interface state, the way the real
// commands do. That is what makes the second-pass assertion in the
// drift matrix meaningful - a fake that ignored writes would let a
// reconciler which "repaired" nothing at all pass the same test.
type fakeRunner struct {
	mu      sync.Mutex
	ifaces  map[string]*ifaceState
	failing map[string]string // exact command -> stderr text to fail with
	allErr  string            // if set, every command fails with this text
	calls   []string
}

// ifaceState is one modelled host interface.
type ifaceState struct {
	name   string
	up     bool
	member string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{ifaces: map[string]*ifaceState{}, failing: map[string]string{}}
}

// with records an interface as present with the given ifconfig output,
// which is parsed for its UP flag and bridge membership exactly the way
// the code under test parses it - so a test seeds a node using the same
// reader it will later judge that node with.
func (f *fakeRunner) with(name, out string) *fakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := &ifaceState{name: name}
	st.up = ifconfigIsUp(out)
	st.member = ifconfigMemberOf(out)
	f.ifaces[name] = st
	return f
}

// withState records an interface directly, for tests that would
// otherwise have to spell out a whole ifconfig block.
func (f *fakeRunner) withState(name string, up bool, member string) *fakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ifaces[name] = &ifaceState{name: name, up: up, member: member}
	return f
}

// without removes an interface from the node, which is how a test says
// "the reboot happened": the record still names it, the kernel does not.
func (f *fakeRunner) without(name string) *fakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ifaces, name)
	return f
}

// fail makes any command containing key fail with msg, modelling
// anything from a permission error to a hung node.
func (f *fakeRunner) fail(key, msg string) *fakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing[key] = msg
	return f
}

// failAll makes every command fail, which is how a test says "this node
// cannot answer anything right now" and must therefore expect unknown
// rather than a confident verdict about anything.
func (f *fakeRunner) failAll(msg string) *fakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allErr = msg
	return f
}

// render produces the ifconfig(8) output a real node would print for
// this modelled interface.
func (s *ifaceState) render() string {
	flags := "flags=8843<BROADCAST,RUNNING,SIMPLEX,MULTICAST>"
	if s.up {
		flags = "flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST>"
	}
	out := s.name + ": " + flags + " metric 0 mtu 1500\n" +
		"\tether 02:1a:2b:3c:4d:5e\n" +
		"\tmedia: Ethernet autoselect (1000baseT <full-duplex>)\n" +
		"\tstatus: active\n"
	if s.member != "" {
		out += "\tmember: " + s.member + " flags=3<LEARNING,DISCOVER>\n" +
			"\t\tifmaxaddr 0 port 8 priority 128 path cost 20000 proto rstp\n"
	}
	return out
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	command := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, command)

	if f.allErr != "" {
		return "", fmt.Errorf("%s: %s", command, f.allErr)
	}
	if msg, bad := f.failing[command]; bad {
		return "", fmt.Errorf("%s: %s", command, msg)
	}
	if name != "ifconfig" {
		return "", fmt.Errorf("fakeRunner: unexpected command %q", command)
	}
	if len(args) == 0 {
		return "", fmt.Errorf("fakeRunner: unexpected command %q", command)
	}

	iface := args[0]
	if len(args) == 1 {
		// The observation call.
		st, ok := f.ifaces[iface]
		if !ok {
			return "", fmt.Errorf("ifconfig: interface %s does not exist", iface)
		}
		return st.render(), nil
	}

	// A write. Modelled with real effect so a repair actually repairs.
	st, ok := f.ifaces[iface]
	if !ok {
		return "", fmt.Errorf("ifconfig: interface %s does not exist", iface)
	}
	switch {
	case len(args) == 2 && args[1] == "up":
		st.up = true
	case len(args) == 3 && args[1] == "addm":
		member, ok := f.ifaces[args[2]]
		if !ok {
			return "", fmt.Errorf("ifconfig: interface %s does not exist", args[2])
		}
		member.member = iface
	case len(args) == 3 && args[1] == "deletem":
		member, ok := f.ifaces[args[2]]
		if !ok {
			return "", fmt.Errorf("ifconfig: interface %s does not exist", args[2])
		}
		member.member = ""
	}
	return "", nil
}

// setIface installs a fresh interface, as ifconfig epair create does.
func (f *fakeRunner) setIface(name string, up bool, member string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ifaces[name] = &ifaceState{name: name, up: up, member: member}
}

// dropIface removes an interface, as ifconfig destroy does.
func (f *fakeRunner) dropIface(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ifaces, name)
}

func (f *fakeRunner) memberOf(iface string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.ifaces[iface]; ok {
		return st.member
	}
	return ""
}

// epairHostSides counts the modelled host-side epair interfaces, which
// is the question "did this reconciler leak interfaces" actually asks.
// The bridge driver's own allocation counter cannot answer it, because a
// baseline seeded directly onto the node never went through it.
func (f *fakeRunner) epairHostSides() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for name := range f.ifaces {
		if strings.HasPrefix(name, "epair") && strings.HasSuffix(name, "a") {
			n++
		}
	}
	return n
}

func (f *fakeRunner) has(iface string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.ifaces[iface]
	return ok
}

func (f *fakeRunner) ran(command string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == command {
			n++
		}
	}
	return n
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeBridge is an in-memory Bridger: the set of bridges that exist, an
// epair counter, and injected failures. Its EnsureEpair/EnsureMember
// side effects are applied to the fakeRunner, so the modelled node stays
// internally consistent no matter which of the two drivers a repair went
// through.
type fakeBridge struct {
	mu       sync.Mutex
	runner   *fakeRunner
	bridges  map[string]bool
	epairs   []epairPair
	nextPair int
	creates  int
	destroys int
	failEp   string
	failMem  string
	failBr   string
}

type epairPair struct{ host, jail string }

func newFakeBridge(runner *fakeRunner) *fakeBridge {
	return &fakeBridge{runner: runner, bridges: map[string]bool{}}
}

func (b *fakeBridge) withBridge(name string) *fakeBridge {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bridges[name] = true
	return b
}

func (b *fakeBridge) pairCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.epairs)
}

func (b *fakeBridge) has(bridge string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bridges[bridge]
}

func (b *fakeBridge) EnsureBridge(ctx context.Context, name string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failBr != "" {
		return false, fmt.Errorf("fakeBridge: EnsureBridge(%s): %s", name, b.failBr)
	}
	if b.bridges[name] {
		return false, nil
	}
	b.bridges[name] = true
	return true, nil
}

func (b *fakeBridge) EnsureMember(ctx context.Context, bridge, iface string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failMem != "" {
		return fmt.Errorf("fakeBridge: EnsureMember(%s, %s): %s", bridge, iface, b.failMem)
	}
	b.mu.Unlock()
	// A bridge membership is exclusive, so joining a new bridge implies
	// leaving the old one. Modelling that is what lets a test assert the
	// post-repair node state without the fake having to parse the
	// reconciler's own commands.
	b.runner.setIface(iface, b.runner.ifaceUp(iface), bridge)
	b.mu.Lock()
	return nil
}

func (b *fakeBridge) EnsureEpair(ctx context.Context, bridge string) (string, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failEp != "" {
		return "", "", fmt.Errorf("fakeBridge: EnsureEpair(%s): %s", bridge, b.failEp)
	}
	n := b.nextPair
	b.nextPair++
	pair := epairPair{host: fmt.Sprintf("epair%da", n), jail: fmt.Sprintf("epair%db", n)}
	b.epairs = append(b.epairs, pair)
	b.creates++
	b.mu.Unlock()

	// `ifconfig epair create` brings both ends up; only the host side is
	// joined to a bridge, exactly as vlan.EnsureEpair does it.
	b.runner.setIface(pair.host, true, bridge)
	b.runner.setIface(pair.jail, true, "")
	b.mu.Lock()
	return pair.host, pair.jail, nil
}

func (b *fakeBridge) DestroyEpair(ctx context.Context, hostSide string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.destroys++
	jailSide := ""
	for i, p := range b.epairs {
		if p.host == hostSide {
			jailSide = p.jail
			b.epairs = append(b.epairs[:i], b.epairs[i+1:]...)
			break
		}
	}
	b.mu.Unlock()
	b.runner.dropIface(hostSide)
	if jailSide != "" {
		b.runner.dropIface(jailSide)
	}
	b.mu.Lock()
	return nil
}

// ifaceUp reports the modelled UP state of an interface, defaulting to
// true for one the fake has not seen (which is the state ifconfig
// epair create leaves both ends in).
func (f *fakeRunner) ifaceUp(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.ifaces[name]; ok {
		return st.up
	}
	return true
}

// fakeJail is an in-memory JailDriver: which jails are running, and what
// each one's own interface currently looks like from the inside.
type fakeJail struct {
	mu       sync.Mutex
	running  map[string]bool
	net      map[string]jailNet
	failList string
	failObs  string
	failFix  string
	fixes    []string
}

type jailNet struct {
	iface   string
	present bool
	addrs   []string // "ip/len"
	route   string
}

func newFakeJail() *fakeJail {
	return &fakeJail{running: map[string]bool{}, net: map[string]jailNet{}}
}

// up marks a jail running with a jail-side interface carrying addrs
// (each "ip/len") and the given default route.
func (j *fakeJail) up(id, iface string, addrs []string, route string) *fakeJail {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running[id] = true
	j.net[id] = jailNet{iface: iface, present: true, addrs: addrs, route: route}
	return j
}

// upWithoutIface marks a jail running whose recorded interface is not
// present inside it: the FindingRestartRequired case.
func (j *fakeJail) upWithoutIface(id, iface string) *fakeJail {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running[id] = true
	j.net[id] = jailNet{iface: iface, present: false}
	return j
}

// stopped removes a jail, the state a jail is in before it is created.
func (j *fakeJail) stopped(id string) *fakeJail {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.running, id)
	delete(j.net, id)
	return j
}

func (j *fakeJail) JailExists(ctx context.Context, name string) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failList != "" {
		return false, fmt.Errorf("fakeJail: JailExists(%s): %s", name, j.failList)
	}
	return j.running[name], nil
}

func (j *fakeJail) ObserveNet(ctx context.Context, id, iface string) (jailNetState, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failObs != "" {
		return jailNetState{}, fmt.Errorf("fakeJail: ObserveNet(%s): %s", id, j.failObs)
	}
	state, ok := j.net[id]
	if !ok {
		// Modelled with the real sentinel so a test exercising this
		// path exercises the same errors.Is check the production caller
		// uses.
		return jailNetState{}, fmt.Errorf("fakeJail: %s: %w", id, errJailGone)
	}
	if !state.present {
		return jailNetState{InterfacePresent: false}, nil
	}
	var addrs []jailAddress
	for _, a := range state.addrs {
		ip, prefix, _ := strings.Cut(a, "/")
		n := 0
		fmt.Sscanf(prefix, "%d", &n)
		addrs = append(addrs, jailAddress{IP: ip, PrefixLen: n})
	}
	return jailNetState{InterfacePresent: true, Addresses: addrs, DefaultRoute: state.route}, nil
}

func (j *fakeJail) EnsureAddressing(ctx context.Context, id, iface string, addr jailAddress, gw string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failFix != "" {
		return fmt.Errorf("fakeJail: EnsureAddressing(%s): %s", id, j.failFix)
	}
	j.fixes = append(j.fixes, fmt.Sprintf("%s %s %s/%d gw=%s", id, iface, addr.IP, addr.PrefixLen, gw))
	state := j.net[id]
	want := fmt.Sprintf("%s/%d", addr.IP, addr.PrefixLen)
	kept := make([]string, 0, 1)
	for _, a := range state.addrs {
		if a == want {
			kept = append(kept, a)
		}
	}
	if len(kept) == 0 {
		kept = append(kept, want)
	}
	// An empty gateway means "leave the jail's own routing alone", which
	// is what an unallocated jail gets.
	if gw != "" {
		state.route = gw
	}
	state.addrs = kept
	j.net[id] = state
	return nil
}

func (j *fakeJail) fixCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.fixes)
}

func (j *fakeJail) addresses(id string) []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.net[id].addrs
}

func (j *fakeJail) route(id string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.net[id].route
}
