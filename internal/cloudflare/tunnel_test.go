package cloudflare

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRenderConfig_IncludesCatchAllAndHTTPScheme(t *testing.T) {
	got := RenderConfig("tunnel-1", "/etc/cloudflared/creds.json", []Ingress{
		{Hostname: "web.example.com", Address: "10.60.0.5:8080"},
	})
	if !strings.Contains(got, "tunnel: tunnel-1") || !strings.Contains(got, "credentials-file: /etc/cloudflared/creds.json") {
		t.Fatalf("config missing tunnel/credentials-file lines: %s", got)
	}
	if !strings.Contains(got, "hostname: web.example.com") || !strings.Contains(got, "service: http://10.60.0.5:8080") {
		t.Errorf("config missing the expected ingress rule, or used a scheme other than http:// (ADR-0063 finding 2): %s", got)
	}
	if strings.Contains(got, "tcp://") || strings.Contains(got, "https://") {
		t.Errorf("config must never use tcp:// or https:// origin scheme in v1 (ADR-0063 finding 2): %s", got)
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "service: http_status:404") {
		t.Errorf("config missing the required cloudflared catch-all rule: %s", got)
	}
}

func TestRenderConfig_DeterministicRegardlessOfInputOrder(t *testing.T) {
	a := RenderConfig("t1", "creds.json", []Ingress{
		{Hostname: "b.example.com", Address: "10.0.0.2:80"},
		{Hostname: "a.example.com", Address: "10.0.0.1:80"},
	})
	b := RenderConfig("t1", "creds.json", []Ingress{
		{Hostname: "a.example.com", Address: "10.0.0.1:80"},
		{Hostname: "b.example.com", Address: "10.0.0.2:80"},
	})
	if a != b {
		t.Fatalf("RenderConfig is not deterministic across input order:\na=%s\nb=%s", a, b)
	}
}

// fakeExec simulates daemon(8)/kill without needing real binaries -
// "daemon" writes a pidfile naming the current test process's own PID
// (always alive via signal 0), "kill" just removes it, matching what a
// real kill+pidfile-cleanup sequence would leave behind.
func fakeExec(t *testing.T, pidfilePath *string) func(ctx context.Context, name string, args ...string) (string, error) {
	return func(ctx context.Context, name string, args ...string) (string, error) {
		switch name {
		case "daemon":
			for i, a := range args {
				if a == "-p" && i+1 < len(args) {
					*pidfilePath = args[i+1]
				}
			}
			if *pidfilePath != "" {
				if err := os.WriteFile(*pidfilePath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
					t.Fatalf("fakeExec: writing pidfile: %v", err)
				}
			}
			return "", nil
		case "kill":
			return "", nil
		default:
			t.Fatalf("fakeExec: unexpected command %q %v", name, args)
			return "", nil
		}
	}
}

func TestEnsureRunning_LaunchesOnFirstCall(t *testing.T) {
	dir := t.TempDir()
	var pidfilePath string
	m := &Manager{RunDir: dir, execCommand: fakeExec(t, &pidfilePath)}

	if err := m.EnsureRunning(context.Background(), "tunnel-1", "creds.json", []Ingress{{Hostname: "web.example.com", Address: "10.0.0.1:80"}}); err != nil {
		t.Fatalf("EnsureRunning() error: %v", err)
	}
	if pidfilePath == "" {
		t.Fatal("expected daemon(8) to be invoked on the first call")
	}
	alive, err := m.processAlive()
	if err != nil || !alive {
		t.Fatalf("processAlive() = %v, %v, want true after a fresh launch", alive, err)
	}
}

func TestEnsureRunning_NoRestartWhenConfigUnchangedAndAlive(t *testing.T) {
	dir := t.TempDir()
	launches := 0
	m := &Manager{RunDir: dir, execCommand: func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "daemon" {
			launches++
			for i, a := range args {
				if a == "-p" && i+1 < len(args) {
					os.WriteFile(args[i+1], []byte(strconv.Itoa(os.Getpid())), 0o644)
				}
			}
		}
		return "", nil
	}}

	ingresses := []Ingress{{Hostname: "web.example.com", Address: "10.0.0.1:80"}}
	if err := m.EnsureRunning(context.Background(), "tunnel-1", "creds.json", ingresses); err != nil {
		t.Fatalf("EnsureRunning() first call error: %v", err)
	}
	if err := m.EnsureRunning(context.Background(), "tunnel-1", "creds.json", ingresses); err != nil {
		t.Fatalf("EnsureRunning() second call error: %v", err)
	}
	if launches != 1 {
		t.Fatalf("launches = %d, want exactly 1 - an unchanged config with a live process must not restart", launches)
	}
}

func TestEnsureRunning_RestartsOnConfigChange(t *testing.T) {
	dir := t.TempDir()
	launches := 0
	m := &Manager{RunDir: dir, execCommand: func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "daemon" {
			launches++
			for i, a := range args {
				if a == "-p" && i+1 < len(args) {
					os.WriteFile(args[i+1], []byte(strconv.Itoa(os.Getpid())), 0o644)
				}
			}
		}
		return "", nil
	}}

	if err := m.EnsureRunning(context.Background(), "tunnel-1", "creds.json", []Ingress{{Hostname: "web.example.com", Address: "10.0.0.1:80"}}); err != nil {
		t.Fatalf("EnsureRunning() first call error: %v", err)
	}
	if err := m.EnsureRunning(context.Background(), "tunnel-1", "creds.json", []Ingress{{Hostname: "web2.example.com", Address: "10.0.0.2:80"}}); err != nil {
		t.Fatalf("EnsureRunning() second call error: %v", err)
	}
	if launches != 2 {
		t.Fatalf("launches = %d, want exactly 2 - a changed ingress set must trigger a restart", launches)
	}
}

func TestEnsureRunning_RestartsOnDeadProcessEvenWithUnchangedConfig(t *testing.T) {
	// Regression test for ADR-0063 finding 6: a process that died on its
	// own (not from any config change) must still be relaunched.
	dir := t.TempDir()
	launches := 0
	m := &Manager{RunDir: dir, execCommand: func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "daemon" {
			launches++
			for i, a := range args {
				if a == "-p" && i+1 < len(args) {
					os.WriteFile(args[i+1], []byte(strconv.Itoa(os.Getpid())), 0o644)
				}
			}
		}
		return "", nil
	}}

	ingresses := []Ingress{{Hostname: "web.example.com", Address: "10.0.0.1:80"}}
	if err := m.EnsureRunning(context.Background(), "tunnel-1", "creds.json", ingresses); err != nil {
		t.Fatalf("EnsureRunning() first call error: %v", err)
	}
	// Simulate the process dying independently: overwrite the pidfile
	// with a PID that is certainly not alive.
	if err := os.WriteFile(m.pidfile(), []byte("999999999"), 0o644); err != nil {
		t.Fatalf("writing dead pidfile: %v", err)
	}
	if err := m.EnsureRunning(context.Background(), "tunnel-1", "creds.json", ingresses); err != nil {
		t.Fatalf("EnsureRunning() second call error: %v", err)
	}
	if launches != 2 {
		t.Fatalf("launches = %d, want exactly 2 - a dead process must be relaunched even with an unchanged config", launches)
	}
}

func TestEnsureRunning_EmptyIngressesStopsInsteadOfLaunching(t *testing.T) {
	dir := t.TempDir()
	var pidfilePath string
	m := &Manager{RunDir: dir, execCommand: fakeExec(t, &pidfilePath)}

	if err := m.EnsureRunning(context.Background(), "tunnel-1", "creds.json", []Ingress{{Hostname: "web.example.com", Address: "10.0.0.1:80"}}); err != nil {
		t.Fatalf("EnsureRunning() first call error: %v", err)
	}
	if err := m.EnsureRunning(context.Background(), "tunnel-1", "creds.json", nil); err != nil {
		t.Fatalf("EnsureRunning() with empty ingresses error: %v", err)
	}
	alive, err := m.processAlive()
	if err != nil || alive {
		t.Fatalf("processAlive() = %v, %v, want false after the ingress set became empty", alive, err)
	}
	if _, err := os.Stat(m.configPath()); !os.IsNotExist(err) {
		t.Errorf("expected the config file to be removed when there is nothing to expose, stat err = %v", err)
	}
}

func TestStopIfRunning_NoOpWhenNothingRunning(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{RunDir: dir}
	if err := m.StopIfRunning(context.Background()); err != nil {
		t.Fatalf("StopIfRunning() error: %v, want nil when nothing is running", err)
	}
}

func TestStopIfRunning_IdempotentWhenCalledTwice(t *testing.T) {
	dir := t.TempDir()
	var pidfilePath string
	m := &Manager{RunDir: dir, execCommand: fakeExec(t, &pidfilePath)}
	if err := m.EnsureRunning(context.Background(), "tunnel-1", "creds.json", []Ingress{{Hostname: "web.example.com", Address: "10.0.0.1:80"}}); err != nil {
		t.Fatalf("EnsureRunning() error: %v", err)
	}
	if err := m.StopIfRunning(context.Background()); err != nil {
		t.Fatalf("StopIfRunning() first call error: %v", err)
	}
	if err := m.StopIfRunning(context.Background()); err != nil {
		t.Fatalf("StopIfRunning() second call error: %v", err)
	}
}

func TestManager_DefaultRunDirUsedWhenEmpty(t *testing.T) {
	m := &Manager{}
	if got := m.configPath(); filepath.Dir(got) != DefaultRunDir {
		t.Errorf("configPath() = %q, want under DefaultRunDir %q", got, DefaultRunDir)
	}
}
