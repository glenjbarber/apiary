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
	rcConfPath = "/etc/rc.conf"
	pfConfPath = "/etc/pf.conf"
)

// apiaryPFAnchor is the exact anchor stanza internal/pf's own doc comment
// (internal/pf/exec.go) says it requires already present in pf.conf.
const apiaryPFAnchor = `anchor "apiary/*"`

var registry = []Check{
	zfsPoolCheck,
	zfsBaseDatasetCheck,
	vmmLoadedCheck,
	nmdmLoadedCheck,
	bhyveFirmwarePkgCheck,
	bhyveBinariesCheck,
	dnsmasqPkgCheck,
	// pf-anchor before pf-enabled: pf-anchor's Apply creates /etc/pf.conf
	// when it doesn't exist yet (a stock FreeBSD install ships without
	// one) - pf-enabled's Apply runs `service pf onestart`, which needs a
	// parseable pf.conf to already be there.
	pfAnchorCheck,
	pfEnabledCheck,
	gatewayEnableCheck,
	rcConfPermsCheck,
	vlanUplinkCheck,
	bhyveBridgeCheck,
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

// ---- rc.conf permissions ----

var rcConfPermsCheck = Check{
	ID:          "rcconf-perms",
	Description: "/etc/rc.conf is not group/world readable (ADR-0067: it can carry a live -peer-api-key secret)",
	Risk:        RiskSafe,
	Applicable:  always,
	Probe: func(ctx context.Context, r Runner, opt Options) Result {
		info, err := os.Stat(rcConfPath)
		if err != nil {
			return Result{ID: "rcconf-perms", Status: StatusUnknown, Detail: err.Error()}
		}
		if info.Mode().Perm()&0o077 != 0 {
			return Result{ID: "rcconf-perms", Status: StatusMisconfigured,
				Detail:  fmt.Sprintf("mode %04o is group/world accessible", info.Mode().Perm()),
				FixHint: "chmod 600 " + rcConfPath}
		}
		return Result{ID: "rcconf-perms", Status: StatusOK, Detail: fmt.Sprintf("mode %04o", info.Mode().Perm())}
	},
	Apply: func(ctx context.Context, r Runner, opt Options) error {
		if err := os.Chmod(rcConfPath, 0o600); err != nil {
			return fmt.Errorf("chmod 600 %s: %w", rcConfPath, err)
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
				FixHint: fmt.Sprintf("ifconfig %s create; ifconfig %s addm %s up - RISK: this can drop network connectivity if run over the uplink NIC's own SSH session (ADR-0022's own near-miss); run apiaryinstall -apply-network yes-modify-network instead of by hand where possible", opt.BhyveBridge, opt.BhyveBridge, opt.VLANUplink)}
		}
		if !strings.Contains(out, "member: "+opt.VLANUplink) {
			return Result{ID: "bhyve-bridge", Status: StatusMisconfigured,
				Detail:  opt.BhyveBridge + " exists but does not have " + opt.VLANUplink + " attached",
				FixHint: fmt.Sprintf("ifconfig %s addm %s", opt.BhyveBridge, opt.VLANUplink)}
		}
		return Result{ID: "bhyve-bridge", Status: StatusOK, Detail: fmt.Sprintf("%s has %s attached", opt.BhyveBridge, opt.VLANUplink)}
	},
	Apply: func(ctx context.Context, r Runner, opt Options) error {
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
		// Persist across reboots, mirroring the rc.conf lines ADR-0022
		// itself documents for this exact setup (the simple addm form -
		// the MAC-pinning create_args_bridgeN variant ADR-0022 also shows
		// is not handled here; disclosed in the ADR for this feature).
		if err := ensureRcListContains(ctx, r, "cloned_interfaces", opt.BhyveBridge); err != nil {
			return err
		}
		return setRcVar(ctx, r, fmt.Sprintf("ifconfig_%s=addm %s up", opt.BhyveBridge, opt.VLANUplink))
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
