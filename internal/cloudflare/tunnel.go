package cloudflare

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// DefaultRunDir is where this node's cloudflared config/pidfile/log
// live by default, alongside internal/isostore's/internal/nodeconfig's
// own /var/db/apiary convention. Fixed and independent of the
// feature's own flags, so it's stable across enable/disable - see
// Manager.StopIfRunning.
const DefaultRunDir = "/var/db/apiary/cloudflared"

// Ingress is one exposed Cell: a public hostname proxying to the
// Cell's own local HTTP address. See ADR-0063 for why v1 is
// HTTP-origin-only - address is proxied to as plain http://, never
// https:// or raw tcp://.
type Ingress struct {
	Hostname string
	Address  string // "ip:port"
}

// RenderConfig builds cloudflared's own YAML config for tunnelID,
// authenticating via credentialsFile (the tunnel's own credentials
// JSON from `cloudflared tunnel create`), with one ingress rule per
// entry plus cloudflared's own required catch-all final rule.
// Ingresses are sorted by hostname first, so the same desired set
// always renders identically regardless of caller iteration order -
// load-bearing for EnsureRunning's own content-diff restart check.
func RenderConfig(tunnelID, credentialsFile string, ingresses []Ingress) string {
	sorted := make([]Ingress, len(ingresses))
	copy(sorted, ingresses)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Hostname < sorted[j].Hostname })

	var b strings.Builder
	fmt.Fprintf(&b, "tunnel: %s\n", tunnelID)
	fmt.Fprintf(&b, "credentials-file: %s\n", credentialsFile)
	b.WriteString("ingress:\n")
	for _, ing := range sorted {
		fmt.Fprintf(&b, "  - hostname: %s\n    service: http://%s\n", ing.Hostname, ing.Address)
	}
	// cloudflared requires a final catch-all rule with no hostname -
	// without one, cloudflared refuses to start at all.
	b.WriteString("  - service: http_status:404\n")
	return b.String()
}

// Manager manages this node's own single, shared cloudflared process -
// a Hive-wide singleton (unlike internal/bhyve's per-VM processes),
// launched via daemon(8) exactly like internal/bhyve's own serial
// console reader (ADR-0032). RunDir defaults to DefaultRunDir.
type Manager struct {
	RunDir string

	// execCommand runs `name args...`, mirroring runCmd's own contract -
	// overridable in tests to avoid needing real daemon(8)/cloudflared
	// binaries. Defaults to the real runCmd.
	execCommand func(ctx context.Context, name string, args ...string) (string, error)
}

func (m *Manager) runDir() string {
	if m.RunDir == "" {
		return DefaultRunDir
	}
	return m.RunDir
}

func (m *Manager) configPath() string { return filepath.Join(m.runDir(), "config.yml") }
func (m *Manager) pidfile() string    { return filepath.Join(m.runDir(), "cloudflared.pid") }
func (m *Manager) logfile() string    { return filepath.Join(m.runDir(), "cloudflared.log") }

func (m *Manager) exec() func(ctx context.Context, name string, args ...string) (string, error) {
	if m.execCommand != nil {
		return m.execCommand
	}
	return runCmd
}

// processAlive mirrors internal/bhyve.Manager.processAlive exactly: a
// missing pidfile is "not alive" (not an error), a stale/unparseable
// pidfile is also "not alive," and liveness is a real signal-0 probe,
// not just "the pidfile exists."
func (m *Manager) processAlive() (bool, error) {
	data, err := os.ReadFile(m.pidfile())
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return false, nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false, nil
	}
	return true, nil
}

func (m *Manager) stop(ctx context.Context) {
	if data, err := os.ReadFile(m.pidfile()); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			m.exec()(ctx, "kill", strconv.Itoa(pid))
		}
	}
	os.Remove(m.pidfile())
}

// StopIfRunning stops this node's cloudflared process if one is
// running and removes its config, regardless of whether the feature is
// currently enabled - called both when the desired ingress set becomes
// empty and when the whole feature is disabled (Reconciler.Cloudflare
// == nil), since RunDir's fixed path means this works even without any
// of the feature's own flags configured (ADR-0063 finding 8).
func (m *Manager) StopIfRunning(ctx context.Context) error {
	alive, err := m.processAlive()
	if err != nil {
		return err
	}
	if alive {
		m.stop(ctx)
	}
	os.Remove(m.configPath())
	return nil
}

// EnsureRunning converges this node's cloudflared process toward the
// desired ingress list. An empty list means nothing to expose, handled
// identically to the feature being disabled (StopIfRunning) - there is
// no value in an idle tunnel process serving only the catch-all rule.
// Otherwise: restarts on a config-content change (comparing the
// rendered YAML against what's on disk) OR on a failed liveness check,
// checked independently every call - a process that died on its own
// (edge disconnect, OOM-kill) must be relaunched even when the desired
// config hasn't changed at all (ADR-0063 finding 6, mirroring the
// ADR-0043/ADR-0027 lesson that a config-change-only check misses a
// process dying independently of any change).
func (m *Manager) EnsureRunning(ctx context.Context, tunnelID, credentialsFile string, ingresses []Ingress) error {
	if len(ingresses) == 0 {
		return m.StopIfRunning(ctx)
	}
	if err := os.MkdirAll(m.runDir(), 0o700); err != nil {
		return fmt.Errorf("creating cloudflared run dir: %w", err)
	}

	newConfig := RenderConfig(tunnelID, credentialsFile, ingresses)
	oldConfig, _ := os.ReadFile(m.configPath()) // missing file just means "always different" - fine
	alive, err := m.processAlive()
	if err != nil {
		return err
	}
	if string(oldConfig) == newConfig && alive {
		return nil
	}

	if err := os.WriteFile(m.configPath(), []byte(newConfig), 0o600); err != nil {
		return fmt.Errorf("writing cloudflared config: %w", err)
	}
	if alive {
		m.stop(ctx)
	}
	if _, err := m.exec()(ctx, "daemon",
		"-f",
		"-p", m.pidfile(),
		"-o", m.logfile(),
		"cloudflared", "tunnel", "run", "--config", m.configPath(),
	); err != nil {
		return fmt.Errorf("starting cloudflared: %w", err)
	}
	return nil
}
