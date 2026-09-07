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

func TestVMMLoadedCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("loaded", func(t *testing.T) {
		r := newFakeRunner()
		r.on("kldstat -m vmm", fakeResponse{stdout: "vmm loaded"})
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
		r.on("kldstat -m vmm", fakeResponse{stdout: "vmm loaded"})
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

func TestRCConfPermsCheck(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "rc.conf")
	old := rcConfPath
	rcConfPath = path
	defer func() { rcConfPath = old }()

	if err := os.WriteFile(path, []byte("hostname=test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := rcConfPermsCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusMisconfigured {
		t.Fatalf("status = %v, want misconfigured for mode 0644", res.Status)
	}

	if err := rcConfPermsCheck.Apply(ctx, newFakeRunner(), Options{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	res = rcConfPermsCheck.Probe(ctx, newFakeRunner(), Options{})
	if res.Status != StatusOK {
		t.Fatalf("status = %v, want ok after chmod 600", res.Status)
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
		t.Fatalf("status = %v, want misconfigured when uplink not enslaved", res2.Status)
	}

	r3 := newFakeRunner()
	r3.on("ifconfig bridge0", fakeResponse{stdout: "bridge0: flags=...\n\tmember: em0 flags=...\n"})
	res3 := bhyveBridgeCheck.Probe(ctx, r3, opt)
	if res3.Status != StatusOK {
		t.Fatalf("status = %v, want ok when uplink enslaved", res3.Status)
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
