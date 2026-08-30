// Package hoststats gathers a point-in-time snapshot of the local
// node's resource usage and hardware health: CPU load, memory, ZFS
// pool capacity/health, per-disk SMART status, and network interface
// traffic counters. Like internal/isostore, this is physical, per-node
// data - never replicated through raft.
//
// Each subsystem is gathered independently via FreeBSD's own tools
// (sysctl, zpool, smart(8), netstat) - no third-party monitoring
// agent. Shelling-out and parsing are kept separate throughout (the
// parse* functions take command output as a plain string) so the
// parsing logic can be unit-tested with fixture data on any OS, the
// same reasoning internal/isostore's pure file-I/O design follows,
// without needing a real FreeBSD host for that part.
package hoststats

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// CPUInfo reports processor count and system load averages.
type CPUInfo struct {
	Cores     int
	LoadAvg1  float64
	LoadAvg5  float64
	LoadAvg15 float64
}

// MemInfo reports physical memory in bytes.
type MemInfo struct {
	TotalBytes uint64
	FreeBytes  uint64
}

// PoolInfo is one ZFS pool's capacity and health, from `zpool list`.
type PoolInfo struct {
	Name        string
	SizeBytes   uint64
	AllocBytes  uint64
	FreeBytes   uint64
	CapacityPct uint32
	Health      string
}

// DiskInfo is one disk's SMART-derived identity and health, from
// FreeBSD's own smart(8) (part of the base pkgbase set - see
// TASKS.md). Healthy is only meaningful when Error is empty; a query
// failure (unsupported device, permission, no SMART support) doesn't
// imply anything about the disk's actual condition.
type DiskInfo struct {
	Name    string
	Model   string
	Serial  string
	Healthy bool
	Error   string
}

// NetIface is one network interface's cumulative traffic counters
// since boot - not a computed rate. A rate would need two samples over
// a known interval; that's a deliberate v1 simplification (see
// ADR-0018), not an oversight.
type NetIface struct {
	Name    string
	RxBytes uint64
	TxBytes uint64

	// Up is this interface's real current state (ifconfig's own UP
	// flag) - added alongside ADR-0022's network management work, whose
	// Networks page colors a *virtual* bridge's up/down status; this
	// does the same for every physical interface (e.g. the uplink NIC
	// itself), which that page never listed at all.
	Up bool
}

// PFInfo is a summary of pf(8)'s current status and counters, from
// `pfctl -s info` - confirming the firewall is actually enabled and
// doing something, for the host stats page (ADR-0022's network
// management work is what first gave this project a reason to run pf
// at all).
type PFInfo struct {
	Enabled bool

	// CurrentStates is pf's live state table size ("current entries").
	CurrentStates uint64

	// Matches is the cumulative count of packets that matched any rule
	// since pf was last enabled - a simple "is traffic actually hitting
	// pf" signal, not broken down by rule/anchor.
	Matches uint64
}

// Snapshot is a point-in-time view of the local host. Errors records
// any subsystem that failed to report - the rest of the snapshot is
// still populated on a best-effort basis (e.g. a host with no ZFS
// pools, or a smart(8) failure on one disk, shouldn't blank out CPU/
// memory/network too).
type Snapshot struct {
	CPU    CPUInfo
	Mem    MemInfo
	Pools  []PoolInfo
	Disks  []DiskInfo
	Net    []NetIface
	PF     PFInfo
	Errors []string
}

func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}

// Gather collects a Snapshot of the local host.
func Gather(ctx context.Context) *Snapshot {
	s := &Snapshot{}

	if cpu, err := gatherCPU(ctx); err != nil {
		s.Errors = append(s.Errors, "cpu: "+err.Error())
	} else {
		s.CPU = *cpu
	}

	if mem, err := gatherMem(ctx); err != nil {
		s.Errors = append(s.Errors, "memory: "+err.Error())
	} else {
		s.Mem = *mem
	}

	if pools, err := gatherPools(ctx); err != nil {
		s.Errors = append(s.Errors, "zfs: "+err.Error())
	} else {
		s.Pools = pools
	}

	if disks, err := gatherDisks(ctx); err != nil {
		s.Errors = append(s.Errors, "disks: "+err.Error())
	} else {
		s.Disks = disks
	}

	if net, err := gatherNet(ctx); err != nil {
		s.Errors = append(s.Errors, "network: "+err.Error())
	} else {
		s.Net = net
	}

	if pf, err := gatherPF(ctx); err != nil {
		s.Errors = append(s.Errors, "pf: "+err.Error())
	} else {
		s.PF = *pf
	}

	return s
}

// gatherPF shells out to `pfctl -s info` - an error here (pfctl
// missing, or pf not permitted to query, e.g. no root) is a real
// failure, distinct from pf simply being disabled (which still
// succeeds, just reports PFInfo.Enabled == false).
func gatherPF(ctx context.Context) (*PFInfo, error) {
	out, err := runCmd(ctx, "pfctl", "-s", "info")
	if err != nil {
		return nil, err
	}
	return parsePFInfo(out), nil
}

// parsePFInfo extracts the fields hoststats cares about from `pfctl -s
// info`'s output - a "Status: Enabled/Disabled ..." header line, a
// "State Table" section with a "current entries <n>" line, and a
// "Counters" section with a "match <n>" line. Every other line/counter
// is ignored; this isn't meant to be a full pf(8) stats dump.
func parsePFInfo(out string) *PFInfo {
	info := &PFInfo{}
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		fields := strings.Fields(trimmed)
		switch {
		case strings.HasPrefix(trimmed, "Status:"):
			info.Enabled = len(fields) >= 2 && fields[1] == "Enabled"
		case strings.HasPrefix(trimmed, "current entries") && len(fields) >= 3:
			info.CurrentStates, _ = strconv.ParseUint(fields[2], 10, 64)
		case strings.HasPrefix(trimmed, "match") && len(fields) >= 2:
			info.Matches, _ = strconv.ParseUint(fields[1], 10, 64)
		}
	}
	return info
}

func gatherCPU(ctx context.Context) (*CPUInfo, error) {
	coresOut, err := runCmd(ctx, "sysctl", "-n", "hw.ncpu")
	if err != nil {
		return nil, err
	}
	cores, err := strconv.Atoi(strings.TrimSpace(coresOut))
	if err != nil {
		return nil, fmt.Errorf("parsing hw.ncpu: %w", err)
	}

	loadOut, err := runCmd(ctx, "sysctl", "-n", "vm.loadavg")
	if err != nil {
		return nil, err
	}
	l1, l5, l15, err := parseLoadAvg(loadOut)
	if err != nil {
		return nil, err
	}
	return &CPUInfo{Cores: cores, LoadAvg1: l1, LoadAvg5: l5, LoadAvg15: l15}, nil
}

// parseLoadAvg parses sysctl vm.loadavg's "{ 1.23 4.56 7.89 }" format.
func parseLoadAvg(s string) (l1, l5, l15 float64, err error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	fields := strings.Fields(s)
	if len(fields) != 3 {
		return 0, 0, 0, fmt.Errorf("unexpected vm.loadavg format: %q", s)
	}
	vals := make([]float64, 3)
	for i, f := range fields {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("parsing load average %q: %w", f, err)
		}
		vals[i] = v
	}
	return vals[0], vals[1], vals[2], nil
}

func gatherMem(ctx context.Context) (*MemInfo, error) {
	totalOut, err := runCmd(ctx, "sysctl", "-n", "hw.physmem")
	if err != nil {
		return nil, err
	}
	freeCountOut, err := runCmd(ctx, "sysctl", "-n", "vm.stats.vm.v_free_count")
	if err != nil {
		return nil, err
	}
	pageSizeOut, err := runCmd(ctx, "sysctl", "-n", "vm.stats.vm.v_page_size")
	if err != nil {
		return nil, err
	}
	return parseMemInfo(totalOut, freeCountOut, pageSizeOut)
}

func parseMemInfo(totalOut, freeCountOut, pageSizeOut string) (*MemInfo, error) {
	total, err := strconv.ParseUint(strings.TrimSpace(totalOut), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parsing hw.physmem: %w", err)
	}
	freeCount, err := strconv.ParseUint(strings.TrimSpace(freeCountOut), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parsing v_free_count: %w", err)
	}
	pageSize, err := strconv.ParseUint(strings.TrimSpace(pageSizeOut), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parsing v_page_size: %w", err)
	}
	return &MemInfo{TotalBytes: total, FreeBytes: freeCount * pageSize}, nil
}

func gatherPools(ctx context.Context) ([]PoolInfo, error) {
	out, err := runCmd(ctx, "zpool", "list", "-Hp", "-o", "name,size,alloc,free,capacity,health")
	if err != nil {
		return nil, err
	}
	return parseZpoolList(out), nil
}

// parseZpoolList parses `zpool list -Hp`'s tab-separated, no-header
// output. -p is what makes size/alloc/free exact byte counts instead
// of human-readable strings like "2.72T".
func parseZpoolList(out string) []PoolInfo {
	var pools []PoolInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 6 {
			continue
		}
		size, _ := strconv.ParseUint(fields[1], 10, 64)
		alloc, _ := strconv.ParseUint(fields[2], 10, 64)
		free, _ := strconv.ParseUint(fields[3], 10, 64)
		capPct, _ := strconv.ParseUint(strings.TrimSuffix(fields[4], "%"), 10, 32)
		pools = append(pools, PoolInfo{
			Name: fields[0], SizeBytes: size, AllocBytes: alloc, FreeBytes: free,
			CapacityPct: uint32(capPct), Health: fields[5],
		})
	}
	return pools
}

// gatherDisks enumerates local disks (kern.disks) and queries each via
// smart(8). A per-disk query failure is recorded on that disk's
// DiskInfo.Error rather than failing the whole gather - some devices
// (e.g. USB, NVMe) may not support this specific tool.
func gatherDisks(ctx context.Context) ([]DiskInfo, error) {
	out, err := runCmd(ctx, "sysctl", "-n", "kern.disks")
	if err != nil {
		return nil, err
	}
	var disks []DiskInfo
	for _, name := range strings.Fields(strings.TrimSpace(out)) {
		disks = append(disks, gatherOneDisk(ctx, name))
	}
	return disks, nil
}

func gatherOneDisk(ctx context.Context, name string) DiskInfo {
	info := DiskInfo{Name: name}

	infoOut, err := runCmd(ctx, "smart", "-i", "/dev/"+name)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	model, serial := parseSmartInfo(infoOut)
	info.Model, info.Serial = model, serial

	statusOut, err := runCmd(ctx, "smart", "-d", "/dev/"+name)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	healthy, found := parseSmartStatus(statusOut)
	if !found {
		info.Error = "no SMART Status line in smart(8) output"
		return info
	}
	info.Healthy = healthy
	return info
}

// parseSmartInfo parses `smart -i`'s tab-separated "Field\tValue"
// lines for the device model and serial number.
func parseSmartInfo(out string) (model, serial string) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "Device":
			model = fields[1]
		case "Serial":
			serial = fields[1]
		}
	}
	return model, serial
}

// parseSmartStatus parses `smart -d`'s attribute table for the overall
// "SMART Status" line - 0 means healthy, nonzero means the drive is
// reporting a failure. found is false if that line wasn't present at
// all (e.g. an unsupported device), which callers must not confuse
// with an actual healthy=false.
func parseSmartStatus(out string) (healthy, found bool) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) >= 2 && fields[0] == "SMART Status" {
			val, _ := strconv.Atoi(strings.TrimSpace(fields[1]))
			return val == 0, true
		}
	}
	return false, false
}

func gatherNet(ctx context.Context) ([]NetIface, error) {
	out, err := runCmd(ctx, "netstat", "-ibn")
	if err != nil {
		return nil, err
	}
	ifaces := parseNetstat(out)
	// Up state comes from a separate `ifconfig <name>` per interface -
	// netstat's own byte-counter rows carry no flags. Best-effort per
	// interface, like every other per-item gather in this file: a
	// failure to determine one interface's up/down state (e.g. it
	// vanished between the two commands) just leaves Up false rather
	// than failing network stats entirely.
	for i := range ifaces {
		if out, err := runCmd(ctx, "ifconfig", ifaces[i].Name); err == nil {
			ifaces[i].Up = parseIfconfigUp(out)
		}
	}
	return ifaces, nil
}

// parseIfconfigUp reports whether the interface is up, from the first
// line of `ifconfig <name>`'s output, e.g.
// "re0: flags=8863<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> ..." -
// checking for an exact "UP" flag inside the <...> list.
func parseIfconfigUp(out string) bool {
	line, _, _ := strings.Cut(out, "\n")
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

// parseNetstat parses `netstat -ibn`'s per-interface link-layer row
// (identified by a "<Link#N>" Network column) for cumulative Ibytes/
// Obytes. Each interface has multiple rows (link layer, then one per
// address family); only the first (link) row per interface name is
// kept, since it's the only one carrying byte counters.
func parseNetstat(out string) []NetIface {
	var ifaces []NetIface
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 12 || !strings.HasPrefix(fields[2], "<Link") {
			continue
		}
		name := fields[0]
		if seen[name] {
			continue
		}
		seen[name] = true
		rx, _ := strconv.ParseUint(fields[7], 10, 64)
		tx, _ := strconv.ParseUint(fields[10], 10, 64)
		ifaces = append(ifaces, NetIface{Name: name, RxBytes: rx, TxBytes: tx})
	}
	return ifaces
}
