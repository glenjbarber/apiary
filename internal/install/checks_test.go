package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner is a scripted Runner: each call consumes the next matching
// canned response keyed by the joined command line, or falls back to a
// default "not found" failure - mirroring internal/assumecheck's own
// fake-interface test style.
type fakeRunner struct {
	responses map[string]fakeResponse
	calls     []string
}

type fakeResponse struct {
	stdout string
	stderr string
	err    error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{responses: map[string]fakeResponse{}}
}

func (f *fakeRunner) on(cmdline string, resp fakeResponse) {
	f.responses[cmdline] = resp
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, key)
	if resp, ok := f.responses[key]; ok {
		return resp.stdout, resp.stderr, resp.err
	}
	return "", "not found", errors.New("exit status 1")
}

func (f *fakeRunner) callCount(cmdline string) int {
	n := 0
	for _, c := range f.calls {
		if c == cmdline {
			n++
		}
	}
	return n
}

var errNotFound = errors.New("exit status 1")

// TestZFSBaseDatasetCheck is the direct regression test for a real bug
// found live: a fresh pool passing zfs-pool has no child datasets yet,
// so managerd's own -zfs-base ("<pool>/apiary" by default) doesn't
// exist either - the first VM ever created failed with "zfs create
// zroot/apiary/<id>: cannot create '...': parent does not exist".
func TestZFSBaseDatasetCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("defaults to <pool>/apiary", func(t *testing.T) {
		if got := zfsBase(Options{ZFSPool: "tank"}); got != "tank/apiary" {
			t.Fatalf("zfsBase = %q, want tank/apiary", got)
		}
		if got := zfsBase(Options{}); got != "zroot/apiary" {
			t.Fatalf("zfsBase = %q, want zroot/apiary (zroot default)", got)
		}
	})

	t.Run("explicit -zfs-base overrides the default", func(t *testing.T) {
		if got := zfsBase(Options{ZFSPool: "tank", ZFSBase: "tank/custom"}); got != "tank/custom" {
			t.Fatalf("zfsBase = %q, want tank/custom", got)
		}
	})

	t.Run("missing dataset", func(t *testing.T) {
		r := newFakeRunner()
		res := zfsBaseDatasetCheck.Probe(ctx, r, Options{ZFSPool: "zroot"})
		if res.Status != StatusMissing {
			t.Fatalf("status = %v, want missing", res.Status)
		}
		if res.FixHint == "" {
			t.Fatal("expected a fix hint when missing")
		}
	})

	t.Run("apply creates it only when missing", func(t *testing.T) {
		r := newFakeRunner()
		r.on("zfs create -p zroot/apiary", fakeResponse{})
		if err := zfsBaseDatasetCheck.Apply(ctx, r, Options{ZFSPool: "zroot"}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if got := r.callCount("zfs create -p zroot/apiary"); got != 1 {
			t.Fatalf("zfs create called %d times, want 1", got)
		}

		r2 := newFakeRunner()
		r2.on("zfs list -H zroot/apiary", fakeResponse{stdout: "zroot/apiary"})
		if err := zfsBaseDatasetCheck.Apply(ctx, r2, Options{ZFSPool: "zroot"}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if got := r2.callCount("zfs create -p zroot/apiary"); got != 0 {
			t.Fatalf("zfs create called %d times, want 0 (already present)", got)
		}
	})
}

func TestVMMLoadedCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("loaded", func(t *testing.T) {
		r := newFakeRunner()
		r.on("kldstat -n vmm", fakeResponse{stdout: "vmm.ko loaded"})
		res := vmmLoadedCheck.Probe(ctx, r, Options{})
		if res.Status != StatusOK {
			t.Fatalf("status = %v, want ok", res.Status)
		}
	})

	t.Run("missing", func(t *testing.T) {
		r := newFakeRunner()
		res := vmmLoadedCheck.Probe(ctx, r, Options{})
		if res.Status != StatusMissing {
			t.Fatalf("status = %v, want missing", res.Status)
		}
		if res.FixHint == "" {
			t.Fatal("expected a fix hint when missing")
		}
	})

	t.Run("apply loads and persists exactly once", func(t *testing.T) {
		r := newFakeRunner()
		r.on("kldload vmm", fakeResponse{})
		r.on("sysrc -n kld_list", fakeResponse{err: errNotFound})
		r.on("sysrc kld_list+=vmm", fakeResponse{})
		if err := vmmLoadedCheck.Apply(ctx, r, Options{}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if got := r.callCount("sysrc kld_list+=vmm"); got != 1 {
			t.Fatalf("sysrc kld_list+=vmm called %d times, want 1", got)
		}
	})

	t.Run("apply is idempotent when kld_list already contains vmm", func(t *testing.T) {
		r := newFakeRunner()
		r.on("kldstat -n vmm", fakeResponse{stdout: "vmm.ko loaded"})
		r.on("sysrc -n kld_list", fakeResponse{stdout: "if_bridge vmm nmdm"})
		if err := vmmLoadedCheck.Apply(ctx, r, Options{}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if got := r.callCount("sysrc kld_list+=vmm"); got != 0 {
			t.Fatalf("sysrc kld_list+=vmm called %d times, want 0 (already present)", got)
		}
		if got := r.callCount("kldload vmm"); got != 0 {
			t.Fatalf("kldload vmm called %d times, want 0 (already loaded)", got)
		}
	})
}

// TestKldLoadedNameVsModuleRegistry is the direct regression test for a
// real bug found live, twice: vmm.ko does not register a kernel module
// literally named "vmm" (kldstat -m vmm falsely reported "not loaded" on
// a host where plain kldstat clearly showed vmm.ko loaded), while
// kldstat -n vmm (bare, no ".ko" suffix needed) correctly finds it -
// confirmed against the real command on that same host. nmdm.ko happens
// to coincide with its own module name, which is why nmdm-loaded never
// surfaced this. kldLoaded must match by loaded-file name (-n <module>)
// first, falling back to -m only when that fails.
func TestKldLoadedNameVsModuleRegistry(t *testing.T) {
	ctx := context.Background()

	t.Run("file-name match succeeds even when module-name lookup would not", func(t *testing.T) {
		r := newFakeRunner()
		r.on("kldstat -n vmm", fakeResponse{stdout: "Id Refs Address Size Name\n6 1 0x0 0x0 vmm.ko"})
		// Deliberately no "kldstat -m vmm" response registered - if
		// kldLoaded fell back to -m first, or at all when -n already
		// succeeded, this would return not-found and the test would fail.
		if !kldLoaded(ctx, r, "vmm") {
			t.Fatal("kldLoaded(vmm) = false, want true via -n vmm")
		}
	})

	t.Run("falls back to module-name lookup when file-name lookup fails", func(t *testing.T) {
		r := newFakeRunner()
		r.on("kldstat -m nmdm", fakeResponse{stdout: "nmdm loaded"})
		if !kldLoaded(ctx, r, "nmdm") {
			t.Fatal("kldLoaded(nmdm) = false, want true via -m fallback")
		}
	})

	t.Run("false when neither lookup finds it", func(t *testing.T) {
		if kldLoaded(ctx, newFakeRunner(), "vmm") {
			t.Fatal("kldLoaded(vmm) = true, want false when both lookups fail")
		}
	})
}

func TestBhyveFirmwarePkgCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("installed", func(t *testing.T) {
		r := newFakeRunner()
		r.on("pkg info bhyve-firmware", fakeResponse{stdout: "bhyve-firmware-1.0"})
		res := bhyveFirmwarePkgCheck.Probe(ctx, r, Options{})
		if res.Status != StatusOK {
			t.Fatalf("status = %v, want ok", res.Status)
		}
	})

	t.Run("apply installs only when missing", func(t *testing.T) {
		r := newFakeRunner()
		r.on("pkg install -y bhyve-firmware", fakeResponse{})
		if err := bhyveFirmwarePkgCheck.Apply(ctx, r, Options{}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if got := r.callCount("pkg install -y bhyve-firmware"); got != 1 {
			t.Fatalf("pkg install called %d times, want 1", got)
		}

		r2 := newFakeRunner()
		r2.on("pkg info bhyve-firmware", fakeResponse{stdout: "already here"})
		if err := bhyveFirmwarePkgCheck.Apply(ctx, r2, Options{}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if got := r2.callCount("pkg install -y bhyve-firmware"); got != 0 {
			t.Fatalf("pkg install called %d times, want 0 (already installed)", got)
		}
	})

	t.Run("respects custom package name", func(t *testing.T) {
		r := newFakeRunner()
		r.on("pkg info edk2-bhyve", fakeResponse{stdout: "present"})
		res := bhyveFirmwarePkgCheck.Probe(ctx, r, Options{BhyveFirmwarePkg: "edk2-bhyve"})
		if res.Status != StatusOK {
			t.Fatalf("status = %v, want ok", res.Status)
		}
	})
}

func TestPFEnabledCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("misconfigured when not running", func(t *testing.T) {
		r := newFakeRunner()
		r.on("sysrc -n pf_enable", fakeResponse{stdout: "YES"})
		res := pfEnabledCheck.Probe(ctx, r, Options{})
		if res.Status != StatusMisconfigured {
			t.Fatalf("status = %v, want misconfigured (pf not running)", res.Status)
		}
	})

	t.Run("ok when enabled and running", func(t *testing.T) {
		r := newFakeRunner()
		r.on("sysrc -n pf_enable", fakeResponse{stdout: "YES"})
		r.on("service pf onestatus", fakeResponse{})
		res := pfEnabledCheck.Probe(ctx, r, Options{})
		if res.Status != StatusOK {
			t.Fatalf("status = %v, want ok", res.Status)
		}
	})

	t.Run("apply skips onestart when already running", func(t *testing.T) {
		r := newFakeRunner()
		r.on("service pf onestatus", fakeResponse{})
		r.on("sysrc pf_enable=YES", fakeResponse{})
		if err := pfEnabledCheck.Apply(ctx, r, Options{}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if got := r.callCount("service pf onestart"); got != 0 {
			t.Fatalf("service pf onestart called %d times, want 0", got)
		}
	})
}

func TestGatewayEnableCheckApplicability(t *testing.T) {
	if gatewayEnableCheck.Applicable(Options{EnableNAT: false}) {
		t.Fatal("gateway-enable should not apply when EnableNAT is false")
	}
	if !gatewayEnableCheck.Applicable(Options{EnableNAT: true}) {
		t.Fatal("gateway-enable should apply when EnableNAT is true")
	}
}

// TestGatewayEnableCheckLiveForwarding is the regression test for a real
// gap: Apply only ever persisted gateway_enable=YES to rc.conf, which
// takes effect on the next reboot only - a host that hasn't rebooted
// since would report StatusOK while ADR-0048's own NAT rules silently
// never route anything. Probe and Apply must both also check/set the
// live net.inet.ip.forwarding sysctl, not just the persisted value.
func TestGatewayEnableCheckLiveForwarding(t *testing.T) {
	ctx := context.Background()

	t.Run("misconfigured when persisted but not live", func(t *testing.T) {
		r := newFakeRunner()
		r.on("sysrc -n gateway_enable", fakeResponse{stdout: "YES"})
		r.on("sysctl -n net.inet.ip.forwarding", fakeResponse{stdout: "0"})
		res := gatewayEnableCheck.Probe(ctx, r, Options{EnableNAT: true})
		if res.Status != StatusMisconfigured {
			t.Fatalf("status = %v, want misconfigured when rc.conf is set but the running kernel isn't forwarding yet", res.Status)
		}
	})

	t.Run("ok only when both persisted and live", func(t *testing.T) {
		r := newFakeRunner()
		r.on("sysrc -n gateway_enable", fakeResponse{stdout: "YES"})
		r.on("sysctl -n net.inet.ip.forwarding", fakeResponse{stdout: "1"})
		res := gatewayEnableCheck.Probe(ctx, r, Options{EnableNAT: true})
		if res.Status != StatusOK {
			t.Fatalf("status = %v, want ok", res.Status)
		}
	})

	t.Run("apply sets the live sysctl, not just rc.conf", func(t *testing.T) {
		r := newFakeRunner()
		r.on("sysrc gateway_enable=YES", fakeResponse{})
		r.on("sysctl -n net.inet.ip.forwarding", fakeResponse{stdout: "0"})
		r.on("sysctl net.inet.ip.forwarding=1", fakeResponse{})
		if err := gatewayEnableCheck.Apply(ctx, r, Options{EnableNAT: true}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if got := r.callCount("sysctl net.inet.ip.forwarding=1"); got != 1 {
			t.Fatalf("sysctl net.inet.ip.forwarding=1 called %d times, want 1", got)
		}
	})

	t.Run("apply skips the sysctl call when already live", func(t *testing.T) {
		r := newFakeRunner()
		r.on("sysrc gateway_enable=YES", fakeResponse{})
		r.on("sysctl -n net.inet.ip.forwarding", fakeResponse{stdout: "1"})
		if err := gatewayEnableCheck.Apply(ctx, r, Options{EnableNAT: true}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if got := r.callCount("sysctl net.inet.ip.forwarding=1"); got != 0 {
			t.Fatalf("sysctl net.inet.ip.forwarding=1 called %d times, want 0 (already live)", got)
		}
	})
}

func TestPFAnchorCheck(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "pf.conf")
	old := pfConfPath
	pfConfPath = path
	defer func() { pfConfPath = old }()

	if err := os.WriteFile(path, []byte("set skip on lo0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := pfAnchorCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusMissing {
		t.Fatalf("status = %v, want missing", res.Status)
	}

	if err := pfAnchorCheck.Apply(ctx, newFakeRunner(), Options{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), apiaryPFAnchor) {
		t.Fatalf("expected anchor appended, got: %s", body)
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("expected a .bak file before mutating pf.conf: %v", err)
	}

	// Applying again must not duplicate the anchor line.
	before := string(body)
	if err := pfAnchorCheck.Apply(ctx, newFakeRunner(), Options{}); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Fatalf("second apply changed the file; want idempotent no-op\nbefore=%q\nafter=%q", before, after)
	}

	res = pfAnchorCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusOK {
		t.Fatalf("status = %v, want ok after apply", res.Status)
	}
}

// TestPFAnchorCheckMissingFile covers a stock FreeBSD install, which ships
// with no /etc/pf.conf at all - confirmed live on a fresh VM host, where
// the original implementation reported StatusUnknown and Apply failed
// outright trying to read a file that doesn't exist yet.
func TestPFAnchorCheckMissingFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "pf.conf") // deliberately never created
	old := pfConfPath
	pfConfPath = path
	defer func() { pfConfPath = old }()

	res := pfAnchorCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusMissing {
		t.Fatalf("status = %v, want missing (not unknown) when pf.conf does not exist", res.Status)
	}

	if err := pfAnchorCheck.Apply(ctx, newFakeRunner(), Options{}); err != nil {
		t.Fatalf("apply on a nonexistent pf.conf: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected pf.conf to be created: %v", err)
	}
	if !strings.Contains(string(body), apiaryPFAnchor) {
		t.Fatalf("expected anchor in newly-created pf.conf, got: %s", body)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Fatal("did not expect a .bak file when pf.conf never existed")
	}

	res = pfAnchorCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusOK {
		t.Fatalf("status = %v, want ok after apply", res.Status)
	}
}

func TestBhyveBridgeCheck(t *testing.T) {
	ctx := context.Background()
	opt := Options{BhyveBridge: "bridge0", VLANUplink: "em0"}

	if !bhyveBridgeCheck.Applicable(opt) {
		t.Fatal("bhyve-bridge should apply when both flags are set")
	}
	if bhyveBridgeCheck.Applicable(Options{BhyveBridge: "bridge0"}) {
		t.Fatal("bhyve-bridge should not apply without -vlan-uplink too")
	}

	r := newFakeRunner()
	res := bhyveBridgeCheck.Probe(ctx, r, opt)
	if res.Status != StatusMissing {
		t.Fatalf("status = %v, want missing when bridge does not exist", res.Status)
	}

	r2 := newFakeRunner()
	r2.on("ifconfig bridge0", fakeResponse{stdout: "bridge0: flags=...\n\tmember: em1 flags=...\n"})
	res2 := bhyveBridgeCheck.Probe(ctx, r2, opt)
	if res2.Status != StatusMisconfigured {
		t.Fatalf("status = %v, want misconfigured when uplink not attached", res2.Status)
	}

	r3 := newFakeRunner()
	r3.on("ifconfig bridge0", fakeResponse{stdout: "bridge0: flags=...\n\tmember: em0 flags=...\n"})
	r3.on("ifconfig em0", fakeResponse{stdout: "em0: flags=...\n\tether 02:00:00:00:00:01\n"})
	r3.on("sysrc -n cloned_interfaces", fakeResponse{stdout: "bridge0"})
	r3.on("sysrc -n ifconfig_em0", fakeResponse{stdout: "up"})
	r3.on("sysrc -n ifconfig_bridge0", fakeResponse{stdout: "addm em0 up SYNCDHCP"})
	r3.on("sysrc -n create_args_bridge0", fakeResponse{stdout: "ether 02:00:00:00:00:01"})
	res3 := bhyveBridgeCheck.Probe(ctx, r3, opt)
	if res3.Status != StatusOK {
		t.Fatalf("status = %v, want ok when uplink attached", res3.Status)
	}

	// DHCP on the physical bridge member is the old, reboot-fragile
	// layout. The stock devd rule is not the problem: dhclient quietstart
	// still honors dhcpif(), so the installer must migrate the rc.conf
	// DHCP token to the bridge rather than disable devd system-wide.
	r4 := newFakeRunner()
	r4.on("ifconfig bridge0", fakeResponse{stdout: "bridge0: flags=...\n\tmember: em0 flags=...\n"})
	r4.on("sysrc -n cloned_interfaces", fakeResponse{stdout: "bridge0"})
	r4.on("sysrc -n ifconfig_em0", fakeResponse{stdout: "DHCP"})
	r4.on("sysrc -n ifconfig_bridge0", fakeResponse{stdout: "addm em0 up"})
	res4 := bhyveBridgeCheck.Probe(ctx, r4, opt)
	if res4.Status != StatusMisconfigured {
		t.Fatalf("status = %v, want misconfigured when DHCP remains on em0", res4.Status)
	}
	if !strings.Contains(res4.Detail, "instead of bridge0") {
		t.Fatalf("detail = %q, want bridge DHCP migration explanation", res4.Detail)
	}

	r5 := newFakeRunner()
	r5.on("sysrc -n ifconfig_em0", fakeResponse{stdout: "DHCP"})
	r5.on("sysrc -n ifconfig_bridge0", fakeResponse{stdout: "addm em0 up"})
	r5.on("ifconfig em0", fakeResponse{stdout: "em0: flags=...\n\tether 02:00:00:00:00:01\n"})
	r5.on("ifconfig bridge0", fakeResponse{stdout: "bridge0: flags=...\n\tmember: em0 flags=...\n"})
	r5.on("ifconfig bridge0 up", fakeResponse{})
	r5.on("ifconfig em0 up", fakeResponse{})
	r5.on("sysrc -n cloned_interfaces", fakeResponse{stdout: "bridge0"})
	r5.on("sysrc ifconfig_em0=up", fakeResponse{})
	r5.on("sysrc create_args_bridge0=ether 02:00:00:00:00:01", fakeResponse{})
	r5.on("sysrc ifconfig_bridge0=addm em0 up SYNCDHCP", fakeResponse{})
	if err := bhyveBridgeCheck.Apply(ctx, r5, opt); err != nil {
		t.Fatalf("apply DHCP bridge migration: %v", err)
	}
	for _, call := range []string{
		"sysrc ifconfig_em0=up",
		"sysrc create_args_bridge0=ether 02:00:00:00:00:01",
		"sysrc ifconfig_bridge0=addm em0 up SYNCDHCP",
	} {
		if got := r5.callCount(call); got != 1 {
			t.Errorf("%s calls = %d, want 1", call, got)
		}
	}
}

func TestUplinkBridgingCheck(t *testing.T) {
	ctx := context.Background()

	if uplinkBridgingCheck.Applicable(Options{}) {
		t.Fatal("uplink-bridging should not apply when -allow-uplink-bridging is unset")
	}
	opt := Options{AllowUplinkBridging: "yes-share-uplink-bridge", BhyveBridge: "bridge0", VLANUplink: "em0"}
	if !uplinkBridgingCheck.Applicable(opt) {
		t.Fatal("uplink-bridging should apply once -allow-uplink-bridging is set")
	}

	res := uplinkBridgingCheck.Probe(ctx, newFakeRunner(), Options{AllowUplinkBridging: "yes-share-uplink-bridge"})
	if res.Status != StatusMisconfigured {
		t.Fatalf("status = %v, want misconfigured when -bhyve-bridge/-vlan-uplink are unset", res.Status)
	}

	res2 := uplinkBridgingCheck.Probe(ctx, newFakeRunner(), opt)
	if res2.Status != StatusManual {
		t.Fatalf("status = %v, want manual (report-only, never auto-fixed) once bridge/uplink are configured", res2.Status)
	}
	if uplinkBridgingCheck.Apply != nil {
		t.Error("uplink-bridging must have no Apply - it never modifies anything, only warns")
	}
}

func TestPAMServiceCheckNeverAutoFixes(t *testing.T) {
	if pamServiceCheck.Apply != nil {
		t.Fatal("pam-service must never have an Apply - PAM/account config is permanently manual-only")
	}
	if pamServiceCheck.Risk != RiskManualOnly {
		t.Fatalf("pam-service risk = %v, want manual-only", pamServiceCheck.Risk)
	}
}

// TestDnsmasqRcEnableCheck is the direct regression test for a real
// bug found live: dnsmasq_enable=YES starts dnsmasq via rc.d at boot
// before managerd's own reconciler has had its first tick to
// (re)create the network interfaces dnsmasq is configured to serve -
// dnsmasq logged "unknown interface" twice before self-healing once
// the reconciler's own restart caught up.
func TestDnsmasqRcEnableCheck(t *testing.T) {
	ctx := context.Background()

	r := newFakeRunner()
	r.on("sysrc -n dnsmasq_enable", fakeResponse{stdout: "YES"})
	res := dnsmasqRcEnableCheck.Probe(ctx, r, Options{})
	if res.Status != StatusMisconfigured {
		t.Fatalf("status = %v, want misconfigured when dnsmasq_enable=YES", res.Status)
	}

	r2 := newFakeRunner()
	r2.on("sysrc -n dnsmasq_enable", fakeResponse{stdout: "NO"})
	res2 := dnsmasqRcEnableCheck.Probe(ctx, r2, Options{})
	if res2.Status != StatusOK {
		t.Fatalf("status = %v, want ok when dnsmasq_enable=NO", res2.Status)
	}

	r3 := newFakeRunner()
	res3 := dnsmasqRcEnableCheck.Probe(ctx, r3, Options{})
	if res3.Status != StatusOK {
		t.Fatalf("status = %v, want ok when dnsmasq_enable is unset entirely", res3.Status)
	}

	r4 := newFakeRunner()
	r4.on("sysrc -n dnsmasq_enable", fakeResponse{stdout: "YES"})
	r4.on("sysrc dnsmasq_enable=NO", fakeResponse{stdout: "dnsmasq_enable: YES -> NO"})
	if err := dnsmasqRcEnableCheck.Apply(ctx, r4, Options{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func TestEtcServicesCheck(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "services")
	old := etcServicesPath
	etcServicesPath = path
	defer func() { etcServicesPath = old }()

	if err := os.WriteFile(path, []byte("ssh\t\t\t22/tcp\nhttp\t\t\t80/tcp\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := etcServicesCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusMissing {
		t.Fatalf("status = %v, want missing", res.Status)
	}

	if err := etcServicesCheck.Apply(ctx, newFakeRunner(), Options{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range apiaryServiceEntries {
		if !strings.Contains(string(body), e.Name) || !strings.Contains(string(body), e.Port+"/"+e.Proto) {
			t.Errorf("expected %s (%s/%s) appended, got: %s", e.Name, e.Port, e.Proto, body)
		}
	}
	if !strings.Contains(string(body), "ssh") || !strings.Contains(string(body), "http") {
		t.Errorf("existing entries must survive untouched, got: %s", body)
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("expected a .bak file before mutating /etc/services: %v", err)
	}

	// Applying again must not duplicate any entry.
	before := string(body)
	if err := etcServicesCheck.Apply(ctx, newFakeRunner(), Options{}); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Fatalf("second apply changed the file; want idempotent no-op\nbefore=%q\nafter=%q", before, after)
	}

	res = etcServicesCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusOK {
		t.Fatalf("status = %v, want ok after apply", res.Status)
	}
}

// TestEtcServicesCheckMissingFile mirrors TestPFAnchorCheckMissingFile -
// a from-scratch host with no /etc/services at all (unlikely on real
// FreeBSD, but the check must not crash trying to read it).
func TestEtcServicesCheckMissingFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "services") // deliberately never created
	old := etcServicesPath
	etcServicesPath = path
	defer func() { etcServicesPath = old }()

	res := etcServicesCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusMissing {
		t.Fatalf("status = %v, want missing (not unknown) when /etc/services does not exist", res.Status)
	}

	if err := etcServicesCheck.Apply(ctx, newFakeRunner(), Options{}); err != nil {
		t.Fatalf("apply on a nonexistent /etc/services: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected /etc/services to be created: %v", err)
	}
	for _, e := range apiaryServiceEntries {
		if !strings.Contains(string(body), e.Name) {
			t.Errorf("expected %s in newly-created /etc/services, got: %s", e.Name, body)
		}
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Fatal("did not expect a .bak file when /etc/services never existed")
	}
}

// TestEtcServicesCheckPortConflictFailsClosed is this check's own
// regression test for the one real risk in mutating a shared system
// file: another service (real or operator-added) already claiming one
// of Apiary's own ports under a different name must never be silently
// overwritten or duplicated - the check reports Misconfigured and
// Apply refuses to touch the file at all.
func TestEtcServicesCheckPortConflictFailsClosed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "services")
	old := etcServicesPath
	etcServicesPath = path
	defer func() { etcServicesPath = old }()

	original := "some-other-service\t17700/tcp\t\t#not Apiary\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	res := etcServicesCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusMisconfigured {
		t.Fatalf("status = %v, want misconfigured when another service already claims 17700/tcp", res.Status)
	}
	if !strings.Contains(res.Detail, "some-other-service") {
		t.Errorf("Detail should name the conflicting service, got: %s", res.Detail)
	}

	if err := etcServicesCheck.Apply(ctx, newFakeRunner(), Options{}); err == nil {
		t.Fatal("Apply() error = nil, want a refusal when a port conflict exists")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != original {
		t.Fatalf("file must be left untouched on a conflict, got: %s", body)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Fatal("did not expect a .bak file when Apply refused due to a conflict")
	}
}

// TestManagerdNodeIDCheckRejectsSamplePlaceholder is the regression
// test for the failure mode etc/apiary/README.md already warns about
// in prose: an operator who copied managerd.json.sample verbatim got
// "<this-node-id>" committed into raft state on first start, and no
// later edit could take it back out. Until this check existed the
// warning was documentation only - nothing between "cp the sample" and
// "raftd boots" looked at the value at all.
func TestManagerdNodeIDCheckRejectsSamplePlaceholder(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "managerd.json")
	old := managerdConfigPath
	managerdConfigPath = path
	defer func() { managerdConfigPath = old }()

	// managerd.json.sample's opening block, verbatim: the placeholder
	// under test plus three neighbouring fields that also ship
	// angle-bracketed, so this also proves the check stays on node_id.
	sample := `{
  "node_id": "<this-node-id>",
  "rpc_addr": "<this-node-hostname>:17700",
  "uplink": "<uplink-ifname>",
  "zfs_base": "<your-pool-name>/apiary"
}
`
	if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}

	res := managerdNodeIDCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusMisconfigured {
		t.Fatalf("status = %v, want misconfigured for the sample's literal placeholder", res.Status)
	}
	// The message has to name the field, the file, and the value the
	// operator actually has on their disk, or it is not actionable.
	for _, want := range []string{"node_id", path, "<this-node-id>"} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("Detail should name %q so the operator can see what to change, got: %s", want, res.Detail)
		}
	}
	if !strings.Contains(res.FixHint, path) {
		t.Errorf("FixHint should name the file to edit, got: %s", res.FixHint)
	}
	if managerdNodeIDCheck.Apply != nil {
		t.Error("Apiary must never pick a raft node id on an operator's behalf: Apply must stay nil")
	}
	if managerdNodeIDCheck.Risk != RiskManualOnly {
		t.Errorf("Risk = %v, want manual-only (it is a report-only fact about a file)", managerdNodeIDCheck.Risk)
	}
}

// TestManagerdNodeIDCheckAcceptsRealNodeID is the other half: the
// check must stay quiet on every config an operator legitimately
// produces, or it is a check people learn to skip.
func TestManagerdNodeIDCheckAcceptsRealNodeID(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "managerd.json")
	old := managerdConfigPath
	managerdConfigPath = path
	defer func() { managerdConfigPath = old }()

	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{
			// What docs/bootstrap.md Step 8 tells the operator to
			// write once the sample's placeholders are substituted.
			name:    "substituted id alongside substituted neighbours",
			body:    `{"node_id": "node1", "uplink": "igb0", "zfs_base": "zroot/apiary"}`,
			wantSub: "node1",
		},
		{
			// Only the whole-value-wrapped pattern is a placeholder.
			// Brackets anywhere else are a legitimate id.
			name:    "angle brackets inside a real id",
			body:    `{"node_id": "node<1>"}`,
			wantSub: "node<1>",
		},
		{
			// README's documented alternative to setting the key: leave
			// it out entirely and managerd falls back to os.Hostname().
			name: "key absent",
			body: `{"rpc_addr": "node1.example.lab:17700"}`,
		},
		{
			name: "key present but empty",
			body: `{"node_id": ""}`,
		},
		{
			// A leading bracket that is not a placeholder pair must not
			// trip an anchored pattern.
			name:    "id that only starts with a bracket",
			body:    `{"node_id": "<node1"}`,
			wantSub: "<node1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			res := managerdNodeIDCheck.Probe(ctx, newFakeRunner(), Options{})
			if res.Status != StatusOK {
				t.Fatalf("status = %v, want ok (detail: %s)", res.Status, res.Detail)
			}
			if tc.wantSub != "" && !strings.Contains(res.Detail, tc.wantSub) {
				t.Errorf("Detail should report the accepted node_id %q, got: %s", tc.wantSub, res.Detail)
			}
		})
	}
}

// TestManagerdNodeIDCheckIgnoresOtherPlaceholderFields pins the scope
// of the check. managerd.json.sample ships three other
// angle-bracketed placeholders (rpc_addr, uplink, zfs_base), and
// refusing those would be a different, much broader validation the
// project has not asked for - one that would bury the single finding
// that cannot be undone.
func TestManagerdNodeIDCheckIgnoresOtherPlaceholderFields(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "managerd.json")
	old := managerdConfigPath
	managerdConfigPath = path
	defer func() { managerdConfigPath = old }()

	body := `{
  "node_id": "node1",
  "rpc_addr": "<this-node-hostname>:17700",
  "uplink": "<uplink-ifname>",
  "zfs_base": "<your-pool-name>/apiary"
}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := managerdNodeIDCheck.Probe(ctx, newFakeRunner(), Options{}); res.Status != StatusOK {
		t.Fatalf("status = %v, want ok: only node_id is in this check's scope (detail: %s)", res.Status, res.Detail)
	}
}

// TestManagerdNodeIDCheckMissingFile covers the documented bootstrap
// order: docs/bootstrap.md runs this installer at Step 3, but
// managerd.json is only created at Step 8. Reporting a host as not
// ready because it has not been configured yet would make the check
// unusable on a fresh host for no gain - an absent file holds no
// placeholder.
func TestManagerdNodeIDCheckMissingFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "managerd.json") // deliberately never created
	old := managerdConfigPath
	managerdConfigPath = path
	defer func() { managerdConfigPath = old }()

	res := managerdNodeIDCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusOK {
		t.Fatalf("status = %v, want ok when managerd.json does not exist yet (detail: %s)", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, path) {
		t.Errorf("Detail should name the path it looked at, got: %s", res.Detail)
	}
}

// TestManagerdNodeIDCheckUnreadableOrMalformed covers the case where
// the probe cannot observe the field at all. It reports Unknown rather
// than inventing a verdict: this check has no opinion about JSON
// well-formedness (managerd refuses the same file for the same
// reason) and must never report a node_id as clean when it could not
// read one.
func TestManagerdNodeIDCheckUnreadableOrMalformed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "managerd.json")
	old := managerdConfigPath
	managerdConfigPath = path
	defer func() { managerdConfigPath = old }()

	if err := os.WriteFile(path, []byte(`{"node_id": `), 0o600); err != nil {
		t.Fatal(err)
	}
	res := managerdNodeIDCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusUnknown {
		t.Fatalf("status = %v, want unknown for unparseable JSON (detail: %s)", res.Status, res.Detail)
	}
	if res.FixHint == "" {
		t.Error("expected a fix hint when the file could not be parsed")
	}
}

// TestManagerdNodeIDCheckRegistered pins the registration itself: a
// check that is defined but never added to the registry reports
// nothing, which is the exact failure this change exists to close.
func TestManagerdNodeIDCheckRegistered(t *testing.T) {
	idx := -1
	for i, c := range All() {
		if c.ID == "managerd-node-id" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("managerd-node-id is not in the registry: it would never run")
	}
	if idx != 0 {
		t.Errorf("managerd-node-id is at index %d, want 0 - the one unrecoverable finding should be reported before host provisioning is proposed", idx)
	}
	if All()[0].ID != "managerd-node-id" {
		t.Errorf("All()[0] = %q, want managerd-node-id", All()[0].ID)
	}
}

func TestManualOnlyChecksHaveNoApply(t *testing.T) {
	for _, c := range All() {
		if c.Risk == RiskManualOnly && c.Apply != nil {
			t.Errorf("check %q is RiskManualOnly but has a non-nil Apply", c.ID)
		}
		if c.Risk != RiskManualOnly && c.Apply == nil {
			t.Errorf("check %q is risk %v but has a nil Apply", c.ID, c.Risk)
		}
	}
}
