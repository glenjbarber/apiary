package vlan

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// fakeHost models the slice of ifconfig(8)/bridge(4)/vlan(4) that this
// package drives, so the whole decision path - including the kernel's own
// Bridge SVI refusal - can be exercised on a non-FreeBSD host with no
// ifconfig, no network and no root.
//
// One model, two views: it implements both Runner (rendering a real
// ifconfig-shaped block per interface) and BridgeObserver (reporting
// membership from the same map), so the two can never disagree with
// each other the way two independent fakes would.
//
// The addm refusal for an SVI is modelled with the kernel's own wording
// on purpose: a test that accidentally regresses into issuing the addm
// fails loudly with "Bridge SVI cannot be added to a bridge" rather than
// quietly passing against a permissive fake.
type fakeHost struct {
	mu     sync.Mutex
	ifaces map[string]*fakeIface
	calls  []string

	// renderParent controls whether an interface's parent is printed as
	// ifconfig(8)'s "Parent name:" line. false models an ifconfig that
	// does not print it, which is what exercises the configured-uplink
	// fallback in observeSVI.
	renderParent bool

	// observeErr fails BridgeMembers for a named interface - the "we
	// could not read the bridge" case that must never look like "not a
	// member".
	observeErr map[string]error
	// readErr fails `ifconfig <name>` for a named interface, as a real
	// ifconfig does for a name that does not exist.
	readErr map[string]string
	// addmErr fails every addm with this stderr text.
	addmErr string
	// failTag fails `ifconfig <vlan> vlan ... vlandev ...` with this
	// stderr text, modelling a kernel-side refusal to tag onto a parent
	// that is not a valid vlandev.
	failTag string
}

type fakeIface struct {
	name    string
	bridge  bool
	vlan    int
	parent  string
	members []string
	inet    string
	up      bool
}

func newFakeHost() *fakeHost {
	return &fakeHost{ifaces: map[string]*fakeIface{}, observeErr: map[string]error{}, readErr: map[string]string{}, renderParent: true}
}

// bridge adds a bridge(4) interface to the model.
func (h *fakeHost) bridge(name string) *fakeHost {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ifaces[name] = &fakeIface{name: name, bridge: true, up: true}
	return h
}

// nic adds an ordinary non-bridge interface (a physical uplink, or an
// epair/tap end) to the model.
func (h *fakeHost) nic(name string) *fakeHost {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ifaces[name] = &fakeIface{name: name, up: true}
	return h
}

// vlan adds a vlan(4) interface already tagged onto parent, which is what
// EnsureVLAN produces and what a Bridge SVI is: a VLAN whose parent is a
// bridge.
func (h *fakeHost) vlan(name string, id int, parent string) *fakeHost {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ifaces[name] = &fakeIface{name: name, vlan: id, parent: parent, up: true}
	return h
}

func (h *fakeHost) command(args ...string) string {
	return strings.TrimSpace(strings.Join(args, " "))
}

func (h *fakeHost) issued(fragment string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, call := range h.calls {
		if strings.Contains(call, fragment) {
			return true
		}
	}
	return false
}

func (h *fakeHost) issuedAny(fragments ...string) int {
	n := 0
	for _, f := range fragments {
		if h.issued(f) {
			n++
		}
	}
	return n
}

func (h *fakeHost) memberOf(bridge, iface string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if b, ok := h.ifaces[bridge]; ok {
		return containsInterface(b.members, iface)
	}
	return false
}

// isSVI reports the kernel's own classification, used by tests to assert
// what the model now holds independently of any state this package
// believes.
func (h *fakeHost) isSVI(iface string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.ifaces[iface]
	if !ok || v.vlan == 0 {
		return false
	}
	parent, ok := h.ifaces[v.parent]
	return ok && parent.bridge
}

// Run is the fake-command entry point: it interprets the calls this
// package makes, and fails the ones a real kernel would fail.
func (h *fakeHost) Run(_ context.Context, name string, args ...string) (string, error) {
	if name != "ifconfig" {
		return "", fmt.Errorf("fakeHost: unexpected command %q", name)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cmd := h.command(args...)
	h.calls = append(h.calls, cmd)

	if len(args) == 0 {
		return "", fmt.Errorf("ifconfig: no arguments")
	}
	target := args[0]

	if target == "epair" && len(args) == 2 && args[1] == "create" {
		name := h.nextEpair()
		h.ifaces[name] = &fakeIface{name: name, up: true}
		return name, nil
	}
	if target == "bridge" && len(args) == 4 && args[1] == "create" && args[2] == "name" {
		h.ifaces[args[3]] = &fakeIface{name: args[3], bridge: true, up: true}
		return "", nil
	}
	if len(args) == 1 {
		if text, bad := h.readErr[target]; bad {
			return "", fmt.Errorf("ifconfig %s: %s", target, text)
		}
		iface, ok := h.ifaces[target]
		if !ok {
			return "", fmt.Errorf("ifconfig: interface %s does not exist", target)
		}
		return h.render(iface), nil
	}

	iface, ok := h.ifaces[target]
	if !ok {
		if args[1] != "create" {
			return "", fmt.Errorf("ifconfig: interface %s does not exist", target)
		}
		iface = &fakeIface{name: target}
		h.ifaces[target] = iface
	}
	switch args[1] {
	case "create":
		h.ifaces[target] = &fakeIface{name: target}
		return "", nil
	case "destroy":
		delete(h.ifaces, target)
		return "", nil
	case "up":
		iface.up = true
		return "", nil
	case "inet":
		iface.inet = args[2]
		return "", nil
	case "vlan":
		// `ifconfig vlanN vlan <id> vlandev <parent>`: a VLAN parented
		// to a bridge is that bridge's SVI, which the model records by
		// construction - it is only ever *used* as a bridge member that
		// the rule shows up.
		if h.failTag != "" {
			return "", fmt.Errorf("ifconfig %s vlan: %s", target, h.failTag)
		}
		if len(args) != 5 || args[3] != "vlandev" {
			return "", fmt.Errorf("ifconfig %s: unsupported vlan arguments", target)
		}
		var id int
		if _, err := fmt.Sscanf(args[2], "%d", &id); err != nil {
			return "", fmt.Errorf("ifconfig %s: bad vlan id %q", target, args[2])
		}
		if _, ok := h.ifaces[args[4]]; !ok {
			return "", fmt.Errorf("ifconfig %s: vlandev %s: Device not configured", target, args[4])
		}
		iface.vlan, iface.parent = id, args[4]
		return "", nil
	case "addm":
		member := args[2]
		if h.addmErr != "" {
			return "", fmt.Errorf("ifconfig %s addm %s: %s", target, member, h.addmErr)
		}
		m, ok := h.ifaces[member]
		if !ok {
			return "", fmt.Errorf("ifconfig %s addm %s: File not found", target, member)
		}
		if parent, ok := h.ifaces[m.parent]; ok && m.vlan != 0 && parent.bridge {
			// The kernel's own diagnostic, verbatim from brood.
			return "", fmt.Errorf("ifconfig: BRDGADD %s: Invalid argument (Bridge SVI cannot be added to a bridge)", member)
		}
		if containsInterface(iface.members, member) {
			return "", fmt.Errorf("ifconfig %s addm %s: File exists", target, member)
		}
		iface.members = append(iface.members, member)
		return "", nil
	case "deletem":
		member := args[2]
		kept := iface.members[:0]
		for _, m := range iface.members {
			if m != member {
				kept = append(kept, m)
			}
		}
		iface.members = kept
		return "", nil
	}
	return "", fmt.Errorf("ifconfig %s: unsupported arguments %v", target, args)
}

func (h *fakeHost) nextEpair() string {
	for n := 0; ; n++ {
		name := fmt.Sprintf("epair%da", n)
		if _, taken := h.ifaces[name]; !taken {
			return name
		}
	}
}

// BridgeObserver, from the same model the commands mutate.
func (h *fakeHost) BridgeMembers(_ context.Context, bridge string) ([]string, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err, bad := h.observeErr[bridge]; bad {
		return nil, false, err
	}
	iface, ok := h.ifaces[bridge]
	if !ok {
		return nil, false, fmt.Errorf("ifconfig: interface %s does not exist", bridge)
	}
	members := append([]string(nil), iface.members...)
	sort.Strings(members)
	return members, iface.bridge, nil
}

// render produces an ifconfig(8)-shaped block, in the same column-0
// header / indented continuation shape internal/netif's parser expects
// (and the same shape isUp here expects).
func (h *fakeHost) render(iface *fakeIface) string {
	flags := "flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST>"
	if !iface.up {
		flags = "flags=8802<BROADCAST,SIMPLEX,MULTICAST>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s metric 0 mtu 1500\n", iface.name, flags)
	if iface.inet != "" {
		fmt.Fprintf(&b, "\tinet %s\n", iface.inet)
	}
	if iface.vlan != 0 {
		if h.renderParent && iface.parent != "" {
			fmt.Fprintf(&b, "\tParent name: %s\n", iface.parent)
		}
		fmt.Fprintf(&b, "\tvlan: %d vlanpcp: 0 vlanhwtag: 1\n", iface.vlan)
	}
	for i, member := range iface.members {
		fmt.Fprintf(&b, "\tmember: %s flags=143<LEARNING,DISCOVER>\n", member)
		if i == 0 {
			b.WriteString("\t        port 1 priority 128 path cost 20000\n")
		}
	}
	if iface.bridge {
		b.WriteString("\tgroups: bridge\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// newTestManager wires a Manager to the fake host on both seams.
func newTestManager(h *fakeHost, uplink string) *Manager {
	return &Manager{Uplink: uplink, Runner: h, Bridges: h}
}
