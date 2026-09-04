// Package pathtrace computes a read-only, evidence-labelled explanation of a
// VM Cell's intended network path. It performs no I/O; managerd gathers raft
// intent and owner-Hive observations before calling Compute.
package pathtrace

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

type Status string

const (
	StatusClear         Status = "clear"
	StatusBlocked       Status = "blocked"
	StatusUnknown       Status = "unknown"
	StatusNotApplicable Status = "not_applicable"
)

type Rule struct {
	Direction string
	Action    string
	Protocol  string
	PortRange string
}

type Cell struct {
	ID             string
	Name           string
	NodeID         string
	DesiredState   string
	Phase          string
	PhaseError     string
	NetworkID      string
	IPAddress      string
	MACAddress     string
	FirewallRules  []Rule
	FirewallPaused bool
}

type Network struct {
	ID              string
	Name            string
	VLANID          uint32
	Subnet          string
	ExternalGateway string
}

type Evidence struct {
	BridgeStatus string
	BridgeDetail string
	PFObserved   bool
	PFEnabled    bool
	PFDetail     string
	NATStatus    Status
	NATDetail    string
}

type Request struct {
	Cell        Cell
	Network     *Network
	Destination string
	Protocol    string
	Port        uint32
	Evidence    Evidence
}

type Step struct {
	Stage       string
	Status      Status
	Summary     string
	Evidence    string
	Explanation string
}

type Trace struct {
	Status      Status
	Summary     string
	Steps       []Step
	NonAtomic   bool
	ActiveProbe bool
}

func Compute(req Request) (Trace, error) {
	destination := strings.TrimSpace(req.Destination)
	if destination == "" {
		return Trace{}, fmt.Errorf("destination is required")
	}
	protocol := strings.ToLower(strings.TrimSpace(req.Protocol))
	if protocol != "" && protocol != "tcp" && protocol != "udp" && protocol != "icmp" {
		return Trace{}, fmt.Errorf("protocol %q is invalid: use tcp, udp, icmp, or leave it empty", req.Protocol)
	}
	if req.Port > 65535 {
		return Trace{}, fmt.Errorf("port %d is invalid", req.Port)
	}
	if req.Port != 0 && protocol != "tcp" && protocol != "udp" {
		return Trace{}, fmt.Errorf("a destination port requires tcp or udp")
	}

	parsedIP := net.ParseIP(destination)
	if parsedIP != nil && parsedIP.To4() == nil {
		return Trace{}, fmt.Errorf("IPv6 destinations are not supported in Cell Path Trace v1")
	}

	trace := Trace{NonAtomic: true, ActiveProbe: false}
	add := func(stage string, status Status, summary, evidence, explanation string) {
		trace.Steps = append(trace.Steps, Step{Stage: stage, Status: status, Summary: summary, Evidence: evidence, Explanation: explanation})
	}

	switch {
	case req.Cell.DesiredState == "stopped":
		add("Cell", StatusBlocked, "Cell is configured to be stopped", "raft VM desired state", "A stopped Cell cannot originate live network traffic.")
	case req.Cell.DesiredState == "deleting" || req.Cell.Phase == "deleting":
		add("Cell", StatusBlocked, "Cell is being deleted", "raft VM desired state and phase", "A deleting Cell is not a stable source for a path trace.")
	case req.Cell.Phase == "error":
		detail := req.Cell.PhaseError
		if detail == "" {
			detail = "the reconciler reported an unspecified error"
		}
		add("Cell", StatusBlocked, "Cell reconciliation is in error", "raft VM phase", detail)
	case req.Cell.DesiredState != "running":
		add("Cell", StatusUnknown, "Cell desired state is unspecified", "raft VM desired state", "Apiary cannot confirm that the Cell is intended to be running.")
	case req.Cell.Phase == "ready":
		add("Cell", StatusClear, "Cell is intended to run and reported ready", "raft VM desired state and phase", "The owner Hive last reported the running Cell ready.")
	default:
		add("Cell", StatusUnknown, "Cell is not yet reported ready", "raft VM phase", fmt.Sprintf("The current phase is %q.", emptyAs(req.Cell.Phase, "pending")))
	}

	if req.Cell.NodeID == "" {
		add("Virtual interface", StatusBlocked, "Cell has no owner Hive", "raft VM definition", "Apiary cannot identify the Hive whose network state must be inspected.")
	}
	add("Tap attachment", StatusUnknown, "Current tap attachment is not observed", "Cell Path Trace v1 evidence gap", "A ready phase records successful reconciliation, but v1 has no current owner-Hive tap observation or freshness timestamp.")
	if req.Cell.NetworkID == "" {
		add("Virtual interface", StatusUnknown, "Cell uses the flat-bridge path", "raft VM definition", "The flat bridge is node-local configuration and is not represented in the Cell's replicated network intent.")
		add("Destination response", StatusNotApplicable, "No active probe was sent", "Cell Path Trace v1 boundary", "The destination was not contacted.")
		return finalize(trace), nil
	}
	if req.Cell.IPAddress == "" || req.Cell.MACAddress == "" {
		add("Virtual interface", StatusBlocked, "Managed-network address assignment is incomplete", "raft VM definition", "A managed-network Cell must have both an assigned IPv4 address and MAC address.")
	} else {
		add("Virtual interface", StatusClear, "Cell has an assigned IPv4 and MAC address", "raft VM definition", fmt.Sprintf("%s via %s", req.Cell.IPAddress, req.Cell.MACAddress))
	}

	if req.Network == nil {
		add("Managed network", StatusBlocked, "Referenced managed network was not found", "raft network list", fmt.Sprintf("The Cell references network %q.", req.Cell.NetworkID))
		add("Destination response", StatusNotApplicable, "No active probe was sent", "Cell Path Trace v1 boundary", "The destination was not contacted.")
		return finalize(trace), nil
	}
	_, subnet, err := net.ParseCIDR(req.Network.Subnet)
	if err != nil || subnet.IP.To4() == nil {
		add("Managed network", StatusBlocked, "Managed network has an invalid IPv4 subnet", "raft network definition", fmt.Sprintf("Configured subnet: %q.", req.Network.Subnet))
		add("Destination response", StatusNotApplicable, "No active probe was sent", "Cell Path Trace v1 boundary", "The destination was not contacted.")
		return finalize(trace), nil
	}
	cellIP := net.ParseIP(req.Cell.IPAddress)
	if cellIP == nil || !subnet.Contains(cellIP) {
		add("Managed network", StatusBlocked, "Cell address is outside the managed subnet", "raft VM and network definitions", fmt.Sprintf("Cell address %q is not within %s.", req.Cell.IPAddress, req.Network.Subnet))
	} else {
		vlan := "untagged"
		if req.Network.VLANID != 0 {
			vlan = fmt.Sprintf("VLAN %d", req.Network.VLANID)
		}
		add("Managed network", StatusClear, "Cell address matches the managed subnet", "raft VM and network definitions", fmt.Sprintf("Network %s (%s, %s).", displayName(req.Network.Name, req.Network.ID), req.Network.Subnet, vlan))
	}

	add("DHCP lease and options", StatusUnknown, "Guest DHCP state is not observed", "Cell Path Trace v1 evidence gap", "Raft records the assigned address, but v1 cannot confirm the guest accepted its lease, route, or DNS options.")
	if parsedIP == nil {
		add("DNS", StatusUnknown, "Guest name resolution is not observed", "Cell Path Trace v1 boundary", "Apiary does not yet expose the DHCP DNS option or a guest resolver result through RPC.")
	} else {
		add("DNS", StatusNotApplicable, "Destination is already an IPv4 address", "trace request", "No name resolution is required for this destination.")
	}

	switch req.Evidence.BridgeStatus {
	case "up":
		add("Owner-Hive bridge", StatusClear, "Managed-network bridge is up", "owner Hive local bridge status", req.Evidence.BridgeDetail)
	case "down":
		add("Owner-Hive bridge", StatusBlocked, "Managed-network bridge is down", "owner Hive local bridge status", req.Evidence.BridgeDetail)
	default:
		add("Owner-Hive bridge", StatusUnknown, "Managed-network bridge state is unknown", "owner Hive local bridge status", req.Evidence.BridgeDetail)
	}

	addFirewallStep(&trace, req.Cell, req.Evidence, protocol, req.Port)

	if parsedIP == nil {
		add("Route", StatusUnknown, "Route cannot be classified before DNS resolution", "destination and network subnet", "The destination hostname has no observed address in v1.")
	} else if subnet.Contains(parsedIP) {
		add("Route", StatusClear, "Destination is on the managed subnet", "destination and raft network definition", "The path is on-link and does not require an external gateway or NAT.")
	} else if req.Network.ExternalGateway != "" {
		add("Route", StatusClear, "Managed network declares an external gateway", "raft network definition", fmt.Sprintf("Configured gateway: %s. Gateway reachability is not actively probed.", req.Network.ExternalGateway))
	} else {
		addNATStep(&trace, req.Evidence)
	}

	add("Destination response", StatusNotApplicable, "No active probe was sent", "Cell Path Trace v1 boundary", "A clear trace means no blocker was found in the observed scope, not that the endpoint answered.")
	return finalize(trace), nil
}

func addFirewallStep(trace *Trace, cell Cell, evidence Evidence, protocol string, port uint32) {
	add := func(status Status, summary, explanation string) {
		trace.Steps = append(trace.Steps, Step{Stage: "Firewall policy", Status: status, Summary: summary, Evidence: "raft VM firewall intent", Explanation: explanation})
	}
	if cell.FirewallPaused {
		add(StatusClear, "Cell firewall enforcement is paused", "Apiary applies an empty allow-all anchor while paused.")
		return
	}
	var outbound []Rule
	for _, rule := range cell.FirewallRules {
		if rule.Direction == "out" {
			outbound = append(outbound, rule)
		}
	}
	if len(outbound) == 0 {
		add(StatusClear, "No outbound Cell firewall rule restricts this path", "Apiary's empty or inbound-only Cell anchor preserves allow-all outbound behavior.")
		return
	}
	if !evidence.PFObserved {
		add(StatusUnknown, "Owner Hive packet-filter state is unknown", emptyAs(evidence.PFDetail, "Apiary cannot confirm that the declared Cell rules are currently enforced."))
		return
	}
	if !evidence.PFEnabled {
		add(StatusUnknown, "Declared Cell firewall rules are not enforced", emptyAs(evidence.PFDetail, "The owner Hive reports PF disabled."))
		return
	}
	if protocol == "" {
		add(StatusUnknown, "Protocol is required to evaluate outbound rules", "The Cell has outbound rules, but this trace did not specify tcp, udp, or icmp.")
		return
	}
	if (protocol == "tcp" || protocol == "udp") && port == 0 {
		baseAction := "pass"
		portCanChangeAction := false
		for _, rule := range outbound {
			if rule.Protocol != "" && rule.Protocol != protocol {
				continue
			}
			if rule.PortRange == "" {
				baseAction = rule.Action
				portCanChangeAction = false
			} else if rule.Action != baseAction {
				portCanChangeAction = true
			}
		}
		if portCanChangeAction {
			add(StatusUnknown, "Destination port is required to evaluate outbound rules", "At least one matching protocol rule can change the result for only part of the port range.")
			return
		}
	}
	action := "pass"
	matched := false
	for _, rule := range outbound {
		if rule.Protocol != "" && rule.Protocol != protocol {
			continue
		}
		if rule.PortRange != "" && !portMatches(rule.PortRange, port) {
			continue
		}
		action = rule.Action
		matched = true
	}
	if matched && action == "block" {
		add(StatusBlocked, "Declared outbound firewall policy blocks this path", "The last matching Apiary Cell rule is block.")
		return
	}
	if matched {
		add(StatusClear, "Declared outbound firewall policy passes this path", "The last matching Apiary Cell rule is pass.")
		return
	}
	add(StatusClear, "No outbound rule matches this path", "Apiary's established default for unmatched Cell traffic is allow.")
}

func addNATStep(trace *Trace, evidence Evidence) {
	add := func(status Status, summary, explanation string) {
		trace.Steps = append(trace.Steps, Step{Stage: "Route and NAT", Status: status, Summary: summary, Evidence: "owner Hive PF status and NAT-uplink assumption", Explanation: explanation})
	}
	if !evidence.PFObserved {
		add(StatusUnknown, "Owner Hive packet-filter state is unknown", emptyAs(evidence.PFDetail, "Apiary could not read HostStats from the owner Hive."))
		return
	}
	if !evidence.PFEnabled {
		add(StatusBlocked, "Owner Hive packet filter is disabled", emptyAs(evidence.PFDetail, "Self-hosted outbound NAT requires PF."))
		return
	}
	switch evidence.NATStatus {
	case StatusClear:
		add(StatusClear, "NAT uplink owns the observed default route", evidence.NATDetail)
	case StatusBlocked, StatusNotApplicable:
		add(StatusBlocked, "No usable self-hosted NAT route is proven", evidence.NATDetail)
	default:
		add(StatusUnknown, "Self-hosted NAT route is unverified", evidence.NATDetail)
	}
}

func finalize(trace Trace) Trace {
	trace.Status = StatusClear
	trace.Summary = "No blocker was found within Cell Path Trace v1's observed scope."
	for _, step := range trace.Steps {
		if step.Status == StatusBlocked {
			trace.Status = StatusBlocked
			trace.Summary = fmt.Sprintf("First blocker: %s: %s", step.Stage, step.Summary)
			return trace
		}
	}
	for _, step := range trace.Steps {
		if step.Status == StatusUnknown {
			trace.Status = StatusUnknown
			trace.Summary = fmt.Sprintf("First evidence gap: %s: %s", step.Stage, step.Summary)
			return trace
		}
	}
	return trace
}

func portMatches(portRange string, port uint32) bool {
	if port == 0 {
		return false
	}
	if lo, hi, ok := strings.Cut(portRange, "-"); ok {
		loN, err1 := strconv.ParseUint(lo, 10, 16)
		hiN, err2 := strconv.ParseUint(hi, 10, 16)
		return err1 == nil && err2 == nil && uint64(port) >= loN && uint64(port) <= hiN
	}
	n, err := strconv.ParseUint(portRange, 10, 16)
	return err == nil && uint64(port) == n
}

func emptyAs(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func displayName(name, id string) string {
	if name == "" {
		return id
	}
	return fmt.Sprintf("%s (%s)", name, id)
}
