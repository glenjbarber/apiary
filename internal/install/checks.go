package install

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Host config paths this package inspects/mutates directly (not via
// Runner, since these are plain file reads/writes) - the same defaults
// internal/hostconfig uses for the same two files. Package-level vars
// (not consts) purely so tests can point them at a temp file instead of
// a real host's own /etc/rc.conf and /etc/pf.conf.
var (
	pfConfPath      = "/etc/pf.conf"
	etcServicesPath = "/etc/services"
)

// apiaryPFAnchor is the exact anchor stanza internal/pf's own doc comment
// (internal/pf/exec.go) says it requires already present in pf.conf.
const apiaryPFAnchor = `anchor "apiary/*"`

// apiaryServiceEntry is one line this project wants present in
// /etc/services (services(5)) - purely a documentation/registration
// convenience (netstat -p, sockstat, and getservbyname(3) callers can
// resolve a symbolic name instead of a bare port number); Apiary's own
// daemons never call getservbyname to find their own default port -
// see internal/frontend/fixedport.go's identical, independently
// maintained port list for the web UI's own fixed-port fields.
type apiaryServiceEntry struct {
	Name    string
	Port    string
	Proto   string
	Comment string
}

var apiaryServiceEntries = []apiaryServiceEntry{
	{Name: "apiary-raftd", Port: "17600", Proto: "tcp", Comment: "Apiary raftd Raft consensus"},
	{Name: "apiary-managerd", Port: "17700", Proto: "tcp", Comment: "Apiary managerd RPC"},
	{Name: "apiary-frontend", Port: "8080", Proto: "tcp", Comment: "Apiary frontend web UI"},
	{Name: "apiary-restshimd", Port: "8081", Proto: "tcp", Comment: "Apiary restshimd REST API"},
}

func (e apiaryServiceEntry) line() string {
	// A plain "%-16s" field width guarantees nothing once Name is at
	// least as wide as the field (e.g. "apiary-restshimd" is 17 chars) -
	// with no separating space, the name and port/proto would run
	// together into one unparseable token. Pad to at least one space
	// explicitly instead of trusting the format verb's own width alone.
	pad := 16 - len(e.Name)
	if pad < 1 {
		pad = 1
	}
	return fmt.Sprintf("%s%s%s/%s\t\t\t#%s", e.Name, strings.Repeat(" ", pad), e.Port, e.Proto, e.Comment)
}

// servicesLineMatch reports whether an /etc/services line's own name
// and port/proto exactly match e - the trailing comment/whitespace is
// never compared, since a host administrator may have reformatted it.
func servicesLineMatch(line string, e apiaryServiceEntry) bool {
	fields := strings.Fields(strings.SplitN(line, "#", 2)[0])
	if len(fields) < 2 {
		return false
	}
	return fields[0] == e.Name && fields[1] == e.Port+"/"+e.Proto
}

// servicesLinePortConflict reports the name already bound to
// port/proto on line, or "" if line doesn't claim that port/proto at
// all - used to fail closed rather than append a second, conflicting
// name for a port this project wants to claim.
func servicesLinePortConflict(line, port, proto string) string {
	trimmed := strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
	if trimmed == "" {
		return ""
	}
	fields := strings.Fields(trimmed)
	if len(fields) < 2 || fields[1] != port+"/"+proto {
		return ""
	}
	return fields[0]
}

var registry = []Check{
	zfsPoolCheck,
	zfsBaseDatasetCheck,
	vmmLoadedCheck,
	nmdmLoadedCheck,
	bhyveFirmwarePkgCheck,
	bhyveBinariesCheck,
	dnsmasqPkgCheck,
	dnsmasqRcEnableCheck,
	// pf-anchor before pf-enabled: pf-anchor's Apply creates /etc/pf.conf
	// when it doesn't exist yet (a stock FreeBSD install ships without
	// one) - pf-enabled's Apply runs `service pf onestart`, which needs a
	// parseable pf.conf to already be there.
	pfAnchorCheck,
	pfEnabledCheck,
	etcServicesCheck,
	gatewayEnableCheck,
	vlanUplinkCheck,
	bhyveBridgeCheck,
	uplinkBridgingCheck,
	hastdEnableCheck,
	pamServiceCheck,
}

func always(Options) bool { return true }

// firstNonEmpty returns stderr when non-blank, else err's own message -
// matching every other internal/* package's runCmd error-surfacing
// convention.
func firstNonEmpty(stderr string, err error) string {
	if s := strings.TrimSpace(stderr); s != "" {
		return s
	}
	return err.Error()
}

// sysrcValue reads an /etc/rc.conf variable via `sysrc -n`. ok is false
// when the variable is unset or sysrc itself failed (e.g. not installed) -
// callers treat both the same way: "not confirmed set."
func sysrcValue(ctx context.Context, r Runner, name string) (value string, ok bool) {
	out, _, err := r.Run(ctx, "sysrc", "-n", name)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// ensureRcListContains appends value to a space-separated rc.conf list
// variable via `sysrc <var>+=<value>` only when it isn't already present -
// sysrc's own += does not de-duplicate, so repeated -apply runs would
// otherwise grow kld_list/cloned_interfaces without bound.
func ensureRcListContains(ctx context.Context, r Runner, varName, value string) error {
	if current, ok := sysrcValue(ctx, r, varName); ok {
		for _, w := range strings.Fields(current) {
			if w == value {
				return nil
			}
		}
	}
	if _, stderr, err := r.Run(ctx, "sysrc", varName+"+="+value); err != nil {
		return fmt.Errorf("sysrc %s+=%s: %s", varName, value, firstNonEmpty(stderr, err))
	}
	return nil
}

func setRcVar(ctx context.Context, r Runner, assignment string) error {
	if _, stderr, err := r.Run(ctx, "sysrc", assignment); err != nil {
		return fmt.Errorf("sysrc %s: %s", assignment, firstNonEmpty(stderr, err))
	}
	return nil
}

// ---- ZFS ----

var zfsPoolCheck = Check{
	ID:          "zfs-pool",
	Description: "a ZFS pool exists for Apiary's datasets",
	Risk:        RiskManualOnly,
	Applicable:  always,
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		pool := opt.ZFSPool
		if pool == "" {
			pool = "zroot"
		}
		if _, stderr, err := r.Run(ctx, "zpool", "list", "-H", pool); err != nil {
			return Result{ID: "zfs-pool", Status: StatusMissing,
				Detail:  firstNonEmpty(stderr, err),
				FixHint: fmt.Sprintf("zpool create %s <vdev...> - disk layout is host-specific, Apiary will not choose this for you", pool)}
		}
		return Result{ID: "zfs-pool", Status: StatusOK, Detail: fmt.Sprintf("pool %q present", pool)}
	},
}

// zfsBase resolves opt.ZFSBase against opt.ZFSPool, matching
// cmd/managerd's own "-zfs-base zroot/apiary" default exactly - a fresh
// pool has no child datasets at all, so this is a real, separate thing
// to check from the pool's own existence.
func zfsBase(opt Options) string {
	if opt.ZFSBase != "" {
		return opt.ZFSBase
	}
	pool := opt.ZFSPool
	if pool == "" {
		pool = "zroot"
	}
	return pool + "/apiary"
}

// zfsBaseDatasetCheck is the direct regression check for a real bug
// found live: a fresh pool passing zfs-pool has no child datasets at
// all, so managerd's own "-zfs-base zroot/apiary" (its default) doesn't
// exist yet either - the first VM ever created failed with "zfs create
// zroot/apiary/<id>: cannot create '...': parent does not exist", a
// confusing error one layer removed from the actual missing piece.
var zfsBaseDatasetCheck = Check{
	ID:          "zfs-base-dataset",
	Description: "the -zfs-base dataset managerd provisions VM/jail datasets under already exists",
	Risk:        RiskSafe,
	Applicable:  always,
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		base := zfsBase(opt)
		if _, stderr, err := r.Run(ctx, "zfs", "list", "-H", base); err != nil {
			return Result{ID: "zfs-base-dataset", Status: StatusMissing,
				Detail:  firstNonEmpty(stderr, err),
				FixHint: fmt.Sprintf("zfs create -p %s", base)}
		}
		return Result{ID: "zfs-base-dataset", Status: StatusOK, Detail: fmt.Sprintf("dataset %q present", base)}
	},
	Apply: func(ctx context.Context, r Runner, opt Options) error {
		base := zfsBase(opt)
		if _, _, err := r.Run(ctx, "zfs", "list", "-H", base); err == nil {
			return nil
		}
		if _, stderr, err := r.Run(ctx, "zfs", "create", "-p", base); err != nil {
			return fmt.Errorf("zfs create -p %s: %s", base, firstNonEmpty(stderr, err))
		}
		return nil
	},
}

// ---- kernel modules ----

// kldLoaded checks whether module (e.g. "vmm") is loaded. `-n <name>`
// (matching the loaded KLD's own file, resolved the same way `kldload`
// resolves a bare name to "<name>.ko") is tried first, since `-m <name>`
// matches the kernel's internal *module* name registry instead - a
// different namespace a KLD isn't guaranteed to register itself into
// under its own file's base name. Confirmed live, twice: `kldstat -m vmm`
// falsely reports "not loaded" on a host where plain `kldstat` clearly
// shows vmm.ko loaded, and `kldstat -n vmm` (bare, no ".ko" suffix - an
// appended ".ko" was tried first and is unnecessary) correctly finds it.
// nmdm.ko happens to coincide with its file's own module name, which is
// why nmdm-loaded never surfaced this - `-m` is kept only as a fallback
// for that direction.
func kldLoaded(ctx context.Context, r Runner, module string) bool {
	if _, _, err := r.Run(ctx, "kldstat", "-n", module); err == nil {
		return true
	}
	_, _, err := r.Run(ctx, "kldstat", "-m", module)
	return err == nil
}

var vmmLoadedCheck = Check{
	ID:          "vmm-loaded",
	Description: "vmm.ko (bhyve's kernel module) is loaded",
	Risk:        RiskSafe,
	Applicable:  always,
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		if kldLoaded(ctx, r, "vmm") {
			return Result{ID: "vmm-loaded", Status: StatusOK, Detail: "vmm.ko loaded"}
		}
		return Result{ID: "vmm-loaded", Status: StatusMissing, Detail: "vmm.ko not loaded",
			FixHint: `kldload vmm; sysrc kld_list+=vmm`}
	},
	Apply: func(ctx context.Context, r Runner, opt Options) error {
		if !kldLoaded(ctx, r, "vmm") {
			if _, stderr, err := r.Run(ctx, "kldload", "vmm"); err != nil {
				return fmt.Errorf("kldload vmm: %s", firstNonEmpty(stderr, err))
			}
		}
		return ensureRcListContains(ctx, r, "kld_list", "vmm")
	},
}

var nmdmLoadedCheck = Check{
	ID:          "nmdm-loaded",
	Description: "nmdm.ko (bhyve serial console) is loaded - not loaded by default on FreeBSD, unlike tap(4) (ADR-0032)",
	Risk:        RiskSafe,
	Applicable:  always,
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		if kldLoaded(ctx, r, "nmdm") {
			return Result{ID: "nmdm-loaded", Status: StatusOK, Detail: "nmdm.ko loaded"}
		}
		return Result{ID: "nmdm-loaded", Status: StatusMissing, Detail: "nmdm.ko not loaded",
			FixHint: `kldload nmdm; sysrc kld_list+=nmdm`}
	},
	Apply: func(ctx context.Context, r Runner, opt Options) error {
		if !kldLoaded(ctx, r, "nmdm") {
			if _, stderr, err := r.Run(ctx, "kldload", "nmdm"); err != nil {
				return fmt.Errorf("kldload nmdm: %s", firstNonEmpty(stderr, err))
			}
		}
		return ensureRcListContains(ctx, r, "kld_list", "nmdm")
	},
}

// ---- packages ----

func pkgInstalled(ctx context.Context, r Runner, pkg string) bool {
	_, _, err := r.Run(ctx, "pkg", "info", pkg)
	return err == nil
}

var bhyveFirmwarePkgCheck = Check{
	ID:          "bhyve-firmware-pkg",
	Description: "the bhyve UEFI firmware package is installed",
	Risk:        RiskSafe,
	Applicable:  always,
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		pkg := opt.BhyveFirmwarePkg
		if pkg == "" {
			pkg = "bhyve-firmware"
		}
		if pkgInstalled(ctx, r, pkg) {
			return Result{ID: "bhyve-firmware-pkg", Status: StatusOK, Detail: pkg + " installed"}
		}
		return Result{ID: "bhyve-firmware-pkg", Status: StatusMissing, Detail: pkg + " not installed",
			FixHint: "pkg install -y " + pkg}
	},
	Apply: func(ctx context.Context, r Runner, opt Options) error {
		pkg := opt.BhyveFirmwarePkg
		if pkg == "" {
			pkg = "bhyve-firmware"
		}
		if pkgInstalled(ctx, r, pkg) {
			return nil
		}
		if _, stderr, err := r.Run(ctx, "pkg", "install", "-y", pkg); err != nil {
			return fmt.Errorf("pkg install -y %s: %s", pkg, firstNonEmpty(stderr, err))
		}
		return nil
	},
}

var dnsmasqPkgCheck = Check{
	ID:          "dnsmasq-pkg",
	Description: "dnsmasq is installed (Apiary's DHCP backend, ADR-0022)",
	Risk:        RiskSafe,
	Applicable:  always,
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		if pkgInstalled(ctx, r, "dnsmasq") {
			return Result{ID: "dnsmasq-pkg", Status: StatusOK, Detail: "dnsmasq installed"}
		}
		return Result{ID: "dnsmasq-pkg", Status: StatusMissing, Detail: "dnsmasq not installed",
			FixHint: "pkg install -y dnsmasq"}
	},
	Apply: func(ctx context.Context, r Runner, opt Options) error {
		if pkgInstalled(ctx, r, "dnsmasq") {
			return nil
		}
		if _, stderr, err := r.Run(ctx, "pkg", "install", "-y", "dnsmasq"); err != nil {
			return fmt.Errorf("pkg install -y dnsmasq: %s", firstNonEmpty(stderr, err))
		}
		return nil
	},
}

// dnsmasqRcEnableCheck (ADR-0094) flags a real, confirmed-live boot
// race: internal/dhcpd.Manager.WriteAndReload already calls `service
// dnsmasq restart` itself on every network reconcile, which is the
// only start dnsmasq ever needs - a persisted dnsmasq_enable=YES also
// starts it via rc.d at boot, before managerd's own reconciler has had
// its first tick to (re)create the network interfaces dnsmasq is
// configured to serve. Found live: dnsmasq logged "unknown interface"
// twice at boot before self-healing once the reconciler caught up and
// issued its own restart - harmless there only because that restart
// happened to follow soon after, not because the race is safe in
// general.
var dnsmasqRcEnableCheck = Check{
	ID:          "dnsmasq-rc-enable",
	Description: "dnsmasq_enable is not YES in rc.conf - Apiary's own reconciler starts/restarts dnsmasq itself on every network change (ADR-0022), so a boot-time rc.d start only races it before the first reconcile tick creates the interface",
	Risk:        RiskSafe,
	Applicable:  always,
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		persisted, ok := sysrcValue(ctx, r, "dnsmasq_enable")
		if ok && strings.EqualFold(persisted, "YES") {
			return Result{ID: "dnsmasq-rc-enable", Status: StatusMisconfigured,
				Detail:  "dnsmasq_enable=YES races Apiary's own dnsmasq restart at boot",
				FixHint: "sysrc dnsmasq_enable=NO - Apiary starts dnsmasq itself once it has real networks to serve"}
		}
		return Result{ID: "dnsmasq-rc-enable", Status: StatusOK, Detail: "dnsmasq_enable is not YES"}
	},
	Apply: func(ctx context.Context, r Runner, opt Options) error {
		return setRcVar(ctx, r, "dnsmasq_enable=NO")
	},
}

var bhyveBinariesCheck = Check{
	ID:          "bhyve-binaries",
	Description: "bhyve(8)/bhyvectl(8) are present (base system sanity check)",
	Risk:        RiskManualOnly,
	Applicable:  always,
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		for _, bin := range []string{"bhyve", "bhyvectl"} {
			if _, _, err := r.Run(ctx, "which", bin); err != nil {
				return Result{ID: "bhyve-binaries", Status: StatusMissing,
					Detail:  bin + " not found on $PATH",
					FixHint: bin + " ships in the FreeBSD base system - check this host's installation is not stripped down"}
			}
		}
		return Result{ID: "bhyve-binaries", Status: StatusOK, Detail: "bhyve and bhyvectl present"}
	},
}

// ---- pf ----

func pfRunning(ctx context.Context, r Runner) bool {
	_, _, err := r.Run(ctx, "service", "pf", "onestatus")
	return err == nil
}

var pfEnabledCheck = Check{
	ID:          "pf-enabled",
	Description: "pf is enabled and running",
	Risk:        RiskSafe,
	Applicable:  always,
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		enabled, _ := sysrcValue(ctx, r, "pf_enable")
		running := pfRunning(ctx, r)
		if strings.EqualFold(enabled, "YES") && running {
			return Result{ID: "pf-enabled", Status: StatusOK, Detail: "pf_enable=YES, pf running"}
		}
		return Result{ID: "pf-enabled", Status: StatusMisconfigured,
			Detail:  fmt.Sprintf("pf_enable=%q running=%v", enabled, running),
			FixHint: `sysrc pf_enable=YES; service pf onestart`}
	},
	Apply: func(ctx context.Context, r Runner, opt Options) error {
		if err := setRcVar(ctx, r, "pf_enable=YES"); err != nil {
			return err
		}
		if !pfRunning(ctx, r) {
			if _, stderr, err := r.Run(ctx, "service", "pf", "onestart"); err != nil {
				return fmt.Errorf("service pf onestart: %s", firstNonEmpty(stderr, err))
			}
		}
		return nil
	},
}

var pfAnchorCheck = Check{
	ID:          "pf-anchor",
	Description: `pf.conf reserves an Apiary anchor point (anchor "apiary/*"), required by internal/pf`,
	Risk:        RiskSafe,
	Applicable:  always,
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		body, err := os.ReadFile(pfConfPath)
		if os.IsNotExist(err) {
			// A stock FreeBSD install ships with no /etc/pf.conf at all
			// until an operator (or this tool) creates one - a real,
			// reportable "missing" fact, not a measurement failure.
			return Result{ID: "pf-anchor", Status: StatusMissing, Detail: pfConfPath + " does not exist yet",
				FixHint: fmt.Sprintf("create %s containing %s", pfConfPath, apiaryPFAnchor)}
		}
		if err != nil {
			return Result{ID: "pf-anchor", Status: StatusUnknown, Detail: err.Error(),
				FixHint: fmt.Sprintf("add %s to %s", apiaryPFAnchor, pfConfPath)}
		}
		if strings.Contains(string(body), apiaryPFAnchor) {
			return Result{ID: "pf-anchor", Status: StatusOK, Detail: "anchor present"}
		}
		return Result{ID: "pf-anchor", Status: StatusMissing, Detail: "anchor not found in " + pfConfPath,
			FixHint: fmt.Sprintf("add %s to %s", apiaryPFAnchor, pfConfPath)}
	},
	Apply: func(ctx context.Context, r Runner, opt Options) error {
		body, err := os.ReadFile(pfConfPath)
		if os.IsNotExist(err) {
			// Nothing to back up or preserve - a stock FreeBSD install
			// has no pf.conf at all until something creates one.
			if err := os.WriteFile(pfConfPath, []byte(apiaryPFAnchor+"\n"), 0o644); err != nil {
				return fmt.Errorf("creating %s: %w", pfConfPath, err)
			}
			if pfRunning(ctx, r) {
				if _, stderr, err := r.Run(ctx, "service", "pf", "reload"); err != nil {
					return fmt.Errorf("service pf reload: %s", firstNonEmpty(stderr, err))
				}
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", pfConfPath, err)
		}
		if strings.Contains(string(body), apiaryPFAnchor) {
			return nil
		}
		// Back up before mutating a live host firewall config, mirroring
		// internal/hostconfig's own "never overwrite without a copy"
		// caution (though that package only ever reads - this is the
		// first place in the codebase that mutates one of the three
		// files it names, so the same caution applies with more force).
		if err := os.WriteFile(pfConfPath+".bak", body, 0o644); err != nil {
			return fmt.Errorf("backing up %s: %w", pfConfPath, err)
		}
		updated := append(append([]byte{}, body...), []byte("\n"+apiaryPFAnchor+"\n")...)
		if err := os.WriteFile(pfConfPath, updated, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", pfConfPath, err)
		}
		if pfRunning(ctx, r) {
			if _, stderr, err := r.Run(ctx, "service", "pf", "reload"); err != nil {
				return fmt.Errorf("service pf reload: %s", firstNonEmpty(stderr, err))
			}
		}
		return nil
	},
}

// etcServicesCheck registers Apiary's own fixed listener ports
// (apiaryServiceEntries) in /etc/services (services(5)) - purely a
// registration convenience for netstat/sockstat/getservbyname(3)
// callers, never consulted by Apiary's own daemons themselves. Fails
// closed rather than silently duplicating a port: if an existing,
// differently-named entry already claims one of these ports, the
// check reports Misconfigured and Apply refuses to touch the file at
// all, since resolving that conflict is a judgment call for whoever
// already owns the existing entry, not something to overwrite.
var etcServicesCheck = Check{
	ID:          "etc-services",
	Description: "Apiary's own daemons (raftd/managerd/frontend/restshimd) are registered in /etc/services",
	Risk:        RiskSafe,
	Applicable:  always,
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		body, err := os.ReadFile(etcServicesPath)
		if os.IsNotExist(err) {
			return Result{ID: "etc-services", Status: StatusMissing, Detail: etcServicesPath + " does not exist yet",
				FixHint: fmt.Sprintf("create %s registering Apiary's own service ports", etcServicesPath)}
		}
		if err != nil {
			return Result{ID: "etc-services", Status: StatusUnknown, Detail: err.Error(),
				FixHint: "add Apiary's own service entries to " + etcServicesPath}
		}
		lines := strings.Split(string(body), "\n")
		var missing []string
		for _, e := range apiaryServiceEntries {
			found := false
			for _, line := range lines {
				if servicesLineMatch(line, e) {
					found = true
					break
				}
				if owner := servicesLinePortConflict(line, e.Port, e.Proto); owner != "" && owner != e.Name {
					return Result{ID: "etc-services", Status: StatusMisconfigured,
						Detail:  fmt.Sprintf("%s/%s is already registered to %q, not %q", e.Port, e.Proto, owner, e.Name),
						FixHint: fmt.Sprintf("resolve the conflicting %s/%s entry in %s by hand - Apiary will not overwrite an existing service registration", e.Port, e.Proto, etcServicesPath)}
				}
			}
			if !found {
				missing = append(missing, e.Name)
			}
		}
		if len(missing) == 0 {
			return Result{ID: "etc-services", Status: StatusOK, Detail: "all Apiary service entries present"}
		}
		return Result{ID: "etc-services", Status: StatusMissing,
			Detail:  "missing entries: " + strings.Join(missing, ", "),
			FixHint: fmt.Sprintf("append the missing entries to %s", etcServicesPath)}
	},
	Apply: func(ctx context.Context, r Runner, opt Options) error {
		body, err := os.ReadFile(etcServicesPath)
		if os.IsNotExist(err) {
			var buf strings.Builder
			buf.WriteString("# Apiary service registrations - see docs/bootstrap.md\n")
			for _, e := range apiaryServiceEntries {
				buf.WriteString(e.line() + "\n")
			}
			if err := os.WriteFile(etcServicesPath, []byte(buf.String()), 0o644); err != nil {
				return fmt.Errorf("creating %s: %w", etcServicesPath, err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", etcServicesPath, err)
		}
		lines := strings.Split(string(body), "\n")
		var toAppend []string
		for _, e := range apiaryServiceEntries {
			found := false
			for _, line := range lines {
				if servicesLineMatch(line, e) {
					found = true
					break
				}
				if owner := servicesLinePortConflict(line, e.Port, e.Proto); owner != "" && owner != e.Name {
					return fmt.Errorf("%s/%s is already registered to %q, not %q - resolve this by hand before re-running", e.Port, e.Proto, owner, e.Name)
				}
			}
			if !found {
				toAppend = append(toAppend, e.line())
			}
		}
		if len(toAppend) == 0 {
			return nil
		}
		// Back up before mutating a shared system file, mirroring
		// pf-anchor's own caution above.
		if err := os.WriteFile(etcServicesPath+".bak", body, 0o644); err != nil {
			return fmt.Errorf("backing up %s: %w", etcServicesPath, err)
		}
		updated := append([]byte{}, body...)
		if len(updated) > 0 && updated[len(updated)-1] != '\n' {
			updated = append(updated, '\n')
		}
		updated = append(updated, []byte(strings.Join(toAppend, "\n")+"\n")...)
		if err := os.WriteFile(etcServicesPath, updated, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", etcServicesPath, err)
		}
		return nil
	},
}

// ---- NAT / gateway ----

// ipForwardingLive reports the currently-running kernel's own
// net.inet.ip.forwarding sysctl - separate from gateway_enable's
// persisted /etc/rc.conf value, since sysrc alone only takes effect on
// the *next* boot. Checking only the persisted value would report
// StatusOK on a host that hasn't rebooted since -apply ran, even though
// ADR-0048 is explicit that a NAT'd packet is silently never routed
// anywhere without live forwarding - the exact trap this check exists to
// catch.
func ipForwardingLive(ctx context.Context, r Runner) bool {
	out, _, err := r.Run(ctx, "sysctl", "-n", "net.inet.ip.forwarding")
	return err == nil && strings.TrimSpace(out) == "1"
}

var gatewayEnableCheck = Check{
	ID:          "gateway-enable",
	Description: "gateway_enable=YES (IP forwarding) for self-hosted outbound NAT (ADR-0048)",
	Risk:        RiskSafe,
	Applicable:  func(opt Options) bool { return opt.EnableNAT },
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		persisted, _ := sysrcValue(ctx, r, "gateway_enable")
		live := ipForwardingLive(ctx, r)
		if strings.EqualFold(persisted, "YES") && live {
			return Result{ID: "gateway-enable", Status: StatusOK, Detail: "gateway_enable=YES, net.inet.ip.forwarding=1 (live)"}
		}
		return Result{ID: "gateway-enable", Status: StatusMisconfigured,
			Detail:  fmt.Sprintf("gateway_enable=%q live-forwarding=%v", persisted, live),
			FixHint: "sysrc gateway_enable=YES; sysctl net.inet.ip.forwarding=1"}
	},
	Apply: func(ctx context.Context, r Runner, opt Options) error {
		if err := setRcVar(ctx, r, "gateway_enable=YES"); err != nil {
			return err
		}
		if !ipForwardingLive(ctx, r) {
			if _, stderr, err := r.Run(ctx, "sysctl", "net.inet.ip.forwarding=1"); err != nil {
				return fmt.Errorf("sysctl net.inet.ip.forwarding=1: %s", firstNonEmpty(stderr, err))
			}
		}
		return nil
	},
}

// ---- networking: uplink / bridge ----

var vlanUplinkCheck = Check{
	ID:          "vlan-uplink",
	Description: "the operator-named uplink NIC (-vlan-uplink) exists",
	Risk:        RiskManualOnly,
	Applicable:  func(opt Options) bool { return opt.VLANUplink != "" },
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		if _, stderr, err := r.Run(ctx, "ifconfig", opt.VLANUplink); err != nil {
			return Result{ID: "vlan-uplink", Status: StatusMissing,
				Detail:  firstNonEmpty(stderr, err),
				FixHint: fmt.Sprintf("no interface named %q on this host - Apiary cannot fabricate a NIC, pass the real uplink name via -vlan-uplink", opt.VLANUplink)}
		}
		return Result{ID: "vlan-uplink", Status: StatusOK, Detail: opt.VLANUplink + " exists"}
	},
}

var bhyveBridgeCheck = Check{
	ID:          "bhyve-bridge",
	Description: "the bhyve bridge (-bhyve-bridge) exists and has the uplink NIC (-vlan-uplink) attached to it",
	Risk:        RiskNetwork,
	Applicable:  func(opt Options) bool { return opt.BhyveBridge != "" && opt.VLANUplink != "" },
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		out, stderr, err := r.Run(ctx, "ifconfig", opt.BhyveBridge)
		if err != nil {
			return Result{ID: "bhyve-bridge", Status: StatusMissing,
				Detail:  firstNonEmpty(stderr, err),
				FixHint: fmt.Sprintf("- Preferred: apiaryinstall -apply-network yes-modify-network -vlan-uplink %s -bhyve-bridge %s\n- Risk: this can interrupt an SSH session using %s. Use a console when possible.\n- Manual equivalent: ifconfig %s create; ifconfig %s addm %s up", opt.VLANUplink, opt.BhyveBridge, opt.VLANUplink, opt.BhyveBridge, opt.BhyveBridge, opt.VLANUplink)}
		}
		if !strings.Contains(out, "member: "+opt.VLANUplink) {
			return Result{ID: "bhyve-bridge", Status: StatusMisconfigured,
				Detail:  opt.BhyveBridge + " exists but does not have " + opt.VLANUplink + " attached",
				FixHint: fmt.Sprintf("ifconfig %s addm %s", opt.BhyveBridge, opt.VLANUplink)}
		}

		cloned, _ := sysrcValue(ctx, r, "cloned_interfaces")
		if !wordPresent(cloned, opt.BhyveBridge) {
			return bridgeRCResult(opt, fmt.Sprintf("%s is live but is not listed in cloned_interfaces", opt.BhyveBridge))
		}

		uplinkConfig, _ := sysrcValue(ctx, r, "ifconfig_"+opt.VLANUplink)
		bridgeConfig, _ := sysrcValue(ctx, r, "ifconfig_"+opt.BhyveBridge)
		if !strings.Contains(bridgeConfig, "addm "+opt.VLANUplink) {
			return bridgeRCResult(opt, fmt.Sprintf("ifconfig_%s does not persistently attach %s", opt.BhyveBridge, opt.VLANUplink))
		}
		if hasDHCPToken(uplinkConfig) {
			return bridgeRCResult(opt, fmt.Sprintf("DHCP is configured on bridge member %s instead of %s", opt.VLANUplink, opt.BhyveBridge))
		}
		if hasDHCPToken(bridgeConfig) {
			if !wordPresentFold(bridgeConfig, "SYNCDHCP") {
				return bridgeRCResult(opt, fmt.Sprintf("ifconfig_%s uses asynchronous DHCP; SYNCDHCP is required for deterministic boot networking", opt.BhyveBridge))
			}
			uplinkOut, stderr, err := r.Run(ctx, "ifconfig", opt.VLANUplink)
			if err != nil {
				return bridgeRCResult(opt, firstNonEmpty(stderr, err))
			}
			mac := interfaceMAC(uplinkOut)
			createArgs, _ := sysrcValue(ctx, r, "create_args_"+opt.BhyveBridge)
			if mac == "" || !strings.EqualFold(strings.TrimSpace(createArgs), "ether "+mac) {
				return bridgeRCResult(opt, fmt.Sprintf("create_args_%s must pin the bridge MAC to %s's MAC %s", opt.BhyveBridge, opt.VLANUplink, mac))
			}
		}
		return Result{ID: "bhyve-bridge", Status: StatusOK, Detail: fmt.Sprintf("%s has %s attached", opt.BhyveBridge, opt.VLANUplink)}
	},
	Apply: func(ctx context.Context, r Runner, opt Options) error {
		uplinkConfig, _ := sysrcValue(ctx, r, "ifconfig_"+opt.VLANUplink)
		bridgeConfig, _ := sysrcValue(ctx, r, "ifconfig_"+opt.BhyveBridge)
		moveDHCPToBridge := hasDHCPToken(uplinkConfig) || hasDHCPToken(bridgeConfig)
		mac := ""
		if moveDHCPToBridge {
			uplinkOut, stderr, err := r.Run(ctx, "ifconfig", opt.VLANUplink)
			if err != nil {
				return fmt.Errorf("ifconfig %s: %s", opt.VLANUplink, firstNonEmpty(stderr, err))
			}
			mac = interfaceMAC(uplinkOut)
			if mac == "" {
				return fmt.Errorf("ifconfig %s: no ether address found", opt.VLANUplink)
			}
		}
		if _, _, err := r.Run(ctx, "ifconfig", opt.BhyveBridge); err != nil {
			if _, stderr, err := r.Run(ctx, "ifconfig", opt.BhyveBridge, "create"); err != nil {
				return fmt.Errorf("ifconfig %s create: %s", opt.BhyveBridge, firstNonEmpty(stderr, err))
			}
		}
		if out, _, _ := r.Run(ctx, "ifconfig", opt.BhyveBridge); !strings.Contains(out, "member: "+opt.VLANUplink) {
			if _, stderr, err := r.Run(ctx, "ifconfig", opt.BhyveBridge, "addm", opt.VLANUplink); err != nil {
				return fmt.Errorf("ifconfig %s addm %s: %s", opt.BhyveBridge, opt.VLANUplink, firstNonEmpty(stderr, err))
			}
		}
		if _, stderr, err := r.Run(ctx, "ifconfig", opt.BhyveBridge, "up"); err != nil {
			return fmt.Errorf("ifconfig %s up: %s", opt.BhyveBridge, firstNonEmpty(stderr, err))
		}
		if _, stderr, err := r.Run(ctx, "ifconfig", opt.VLANUplink, "up"); err != nil {
			return fmt.Errorf("ifconfig %s up: %s", opt.VLANUplink, firstNonEmpty(stderr, err))
		}
		if err := ensureRcListContains(ctx, r, "cloned_interfaces", opt.BhyveBridge); err != nil {
			return err
		}
		if moveDHCPToBridge {
			// The physical member stays addressless. Pinning the bridge to
			// the member's MAC preserves the DHCP identity while SYNCDHCP
			// makes the management lease available deterministically during
			// boot. The live lease is deliberately not moved here because
			// doing so would sever the SSH session running apiaryinstall.
			if err := setRcVar(ctx, r, fmt.Sprintf("ifconfig_%s=up", opt.VLANUplink)); err != nil {
				return err
			}
			if err := setRcVar(ctx, r, fmt.Sprintf("create_args_%s=ether %s", opt.BhyveBridge, mac)); err != nil {
				return err
			}
			return setRcVar(ctx, r, fmt.Sprintf("ifconfig_%s=addm %s up SYNCDHCP", opt.BhyveBridge, opt.VLANUplink))
		}
		return setRcVar(ctx, r, fmt.Sprintf("ifconfig_%s=addm %s up", opt.BhyveBridge, opt.VLANUplink))
	},
}

func bridgeRCResult(opt Options, detail string) Result {
	return Result{
		ID:     "bhyve-bridge",
		Status: StatusMisconfigured,
		Detail: detail,
		FixHint: fmt.Sprintf("- Apply the persistent bridge layout: apiaryinstall -apply-network yes-modify-network -vlan-uplink %s -bhyve-bridge %s\n"+
			"- If %s currently uses DHCP, the installer moves that DHCP configuration to %s, pins the bridge MAC, and leaves the physical member addressless.\n"+
			"- Risk: the live lease is not moved, but attaching %s can still interrupt the current SSH session. Use a console when possible.",
			opt.VLANUplink, opt.BhyveBridge, opt.VLANUplink, opt.BhyveBridge, opt.VLANUplink),
	}
}

func wordPresent(value, wanted string) bool {
	for _, word := range strings.Fields(value) {
		if word == wanted {
			return true
		}
	}
	return false
}

func wordPresentFold(value, wanted string) bool {
	for _, word := range strings.Fields(value) {
		if strings.EqualFold(word, wanted) {
			return true
		}
	}
	return false
}

func hasDHCPToken(value string) bool {
	for _, word := range strings.Fields(value) {
		switch strings.ToUpper(word) {
		case "DHCP", "SYNCDHCP", "NOSYNCDHCP":
			return true
		}
	}
	return false
}

func interfaceMAC(ifconfigOutput string) string {
	for _, line := range strings.Split(ifconfigOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "ether" {
			return fields[1]
		}
	}
	return ""
}

// uplinkBridgingCheck (ADR-0101) is applicable only when the operator has
// actually opted into uplink_bridged NetworkDefinition support on this
// node (-allow-uplink-bridging). It never applies anything itself -
// StatusManual either way - it exists purely to make the real risk
// explicit at install time, and to catch the meaningless case of opting
// in without a bridge/uplink for it to reuse.
var uplinkBridgingCheck = Check{
	ID:          "uplink-bridging",
	Description: "uplink_bridged networks (-allow-uplink-bridging) are configured on top of a working -bhyve-bridge/-vlan-uplink pair",
	Risk:        RiskManualOnly,
	Applicable:  func(opt Options) bool { return opt.AllowUplinkBridging != "" },
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		if opt.BhyveBridge == "" || opt.VLANUplink == "" {
			return Result{ID: "uplink-bridging", Status: StatusMisconfigured,
				Detail:  "-allow-uplink-bridging is set but -bhyve-bridge and/or -vlan-uplink is not - uplink_bridged mode has no bridge to reuse",
				FixHint: "set both -bhyve-bridge and -vlan-uplink, or unset -allow-uplink-bridging"}
		}
		return Result{ID: "uplink-bridging", Status: StatusManual,
			Detail: fmt.Sprintf("this node will attach uplink_bridged VMs' taps directly to %s, the same bridge carrying this host's own management traffic - a misbehaving VM there has direct L2 access to that broadcast domain (see ADR-0101)", opt.BhyveBridge)}
	},
}

// ---- HAST (report-only: the fix requires a source patch, ADR-0022) ----

var hastdEnableCheck = Check{
	ID:          "hastd-enable",
	Description: "hastd_enable=YES, for HAST-replicated VM disks/jails (ADR-0026) - opt in via -enable-hast",
	Risk:        RiskManualOnly,
	Applicable:  func(opt Options) bool { return opt.EnableHAST },
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		v, _ := sysrcValue(ctx, r, "hastd_enable")
		if strings.EqualFold(v, "YES") {
			return Result{ID: "hastd-enable", Status: StatusOK, Detail: "hastd_enable=YES"}
		}
		return Result{ID: "hastd-enable", Status: StatusManual, Detail: fmt.Sprintf("hastd_enable=%q", v),
			FixHint: "sysrc hastd_enable=YES - and apply the hast_proto_recv_hdr source patch for FreeBSD bug 298085 before relying on hastd (see docs/adr/0022-network-management.md); Apiary does not carry or apply this patch itself"}
	},
}

// ---- PAM (report-only: never auto-create auth config) ----

var pamServiceCheck = Check{
	ID:          "pam-service",
	Description: "the /etc/pam.d/<service> file for -pam-service exists (ADR-0030)",
	Risk:        RiskManualOnly,
	Applicable:  func(opt Options) bool { return opt.PAMService != "" },
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		path := "/etc/pam.d/" + opt.PAMService
		if _, err := os.Stat(path); err != nil {
			return Result{ID: "pam-service", Status: StatusManual, Detail: err.Error(),
				FixHint: "create " + path + " (e.g. pam_unix.so for auth/account) - Apiary never generates PAM or account configuration itself"}
		}
		return Result{ID: "pam-service", Status: StatusOK, Detail: path + " present"}
	},
}
