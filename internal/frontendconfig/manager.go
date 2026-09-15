// Package frontendconfig persists cmd/frontend's own local settings
// (ADR-0100) as a plain JSON file on disk, replacing what were
// previously its own CLI flags and, for ManagerAPIKey, the
// APIARY_MANAGER_API_KEY environment variable (formerly sourced via
// etc/rc.d/apiary_frontend's own envfile mechanism, now retired).
// Save (ADR-0102) is called only from managerd's own RPC handlers
// (UpdateFrontendConfig) - frontend itself has no RPC layer of its own
// to expose live editing through directly, unlike managerd's own
// internal/nodeconfig, so managerd writes this file on frontend's
// behalf instead (the two are always co-located on the same host).
// This is unrelated to internal/loginconfig, which persists the login
// role map (a live, RPC-editable, Users-page concern) - untouched by
// this package. A change here takes effect the next time frontend
// restarts.
package frontendconfig

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// DefaultPath is where this file lives by default on a pkg-installed
// FreeBSD system, alongside every other daemon's own config file
// under /usr/local/etc/apiary (ADR-0100).
const DefaultPath = "/usr/local/etc/apiary/frontend.json"

// Config is the full set of settings frontend reads at startup. Every
// field mirrors a flag frontend used to take on the command line
// (ManagerAPIKey instead mirrors the APIARY_MANAGER_API_KEY
// environment variable) - see each field's own doc comment.
type Config struct {
	// ManagerAddr mirrors -manager-addr: TCP address of managerd's
	// external RPC API.
	ManagerAddr string `json:"manager_addr,omitempty"`

	// HTTPAddr mirrors -http-addr: address to serve the web UI on.
	HTTPAddr string `json:"http_addr,omitempty"`

	// ManagerTLS mirrors -manager-tls: dial managerd over TLS instead
	// of plaintext.
	ManagerTLS bool `json:"manager_tls,omitempty"`

	// ManagerTLSCA mirrors -manager-tls-ca: PEM CA file to trust for
	// managerd's certificate; empty trusts the system pool.
	ManagerTLSCA string `json:"manager_tls_ca,omitempty"`

	// ManagerTLSServerName mirrors -manager-tls-server-name: hostname
	// to verify managerd's certificate against, if different from
	// ManagerAddr's own host.
	ManagerTLSServerName string `json:"manager_tls_server_name,omitempty"`

	// TLSCert/TLSKey mirror -tls-cert/-tls-key: serve the web UI over
	// HTTPS; leave both empty to serve plaintext HTTP.
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`

	// PeerTLS mirrors -peer-tls: dial other cluster nodes' managerd
	// over TLS when fetching their host stats for the cluster
	// overview page.
	PeerTLS bool `json:"peer_tls,omitempty"`

	// PeerTLSCA mirrors -peer-tls-ca (ADR-0093): a PEM file trusted
	// instead of the system certificate pool when dialing a peer over
	// TLS. Only consulted when PeerTLS is set.
	PeerTLSCA string `json:"peer_tls_ca,omitempty"`

	// PeerHostnameSuffix mirrors -peer-hostname-suffix: appended to a
	// node ID to form its managerd hostname for the cluster overview
	// page.
	PeerHostnameSuffix string `json:"peer_hostname_suffix,omitempty"`

	// PeerManagerPort mirrors -peer-manager-port: port assumed for a
	// peer node's managerd external API when fetching its host stats.
	PeerManagerPort string `json:"peer_manager_port,omitempty"`

	// ManagerAPIKey mirrors the former APIARY_MANAGER_API_KEY
	// environment variable: the API key frontend attaches to every
	// call it makes to managerd on the logged-in user's behalf, once
	// managerd has API-key auth enabled (ADR-0023). A real credential
	// - this file must be root-owned, mode 0600 (Load only warns, see
	// warnIfWorldReadable; it does not refuse to start).
	ManagerAPIKey string `json:"manager_api_key,omitempty"`
}

// defaults matches every flag's own former default value exactly, so
// a missing or partial config file behaves identically to how an
// unset flag used to.
func defaults() Config {
	return Config{
		ManagerAddr:     "127.0.0.1:17700",
		HTTPAddr:        "127.0.0.1:8080",
		PeerManagerPort: "17700",
	}
}

// Manager loads this file. The zero value is ready to use (Path empty
// means DefaultPath).
type Manager struct {
	Path string
}

func (m *Manager) path() string {
	if m.Path == "" {
		return DefaultPath
	}
	return m.Path
}

// Load reads the persisted config, starting from defaults() so a
// missing file - or one that only sets some fields - behaves exactly
// like every flag's own former default for whatever it doesn't set.
// A missing file is not an error (a fresh install with no config
// written yet); a malformed one is, since there is no flag value left
// to silently fall back to. Warns (does not fail) if the file is
// readable by group/other despite holding ManagerAPIKey - see
// warnIfWorldReadable.
func (m *Manager) Load() (Config, error) {
	cfg := defaults()
	data, err := os.ReadFile(m.path())
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return Config{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("frontendconfig: parsing %s: %w", m.path(), err)
	}
	if cfg.ManagerAPIKey != "" {
		warnIfWorldReadable(m.path())
	}
	return cfg, nil
}

// Save writes cfg, replacing whatever was there before in full (not a
// merge) - the caller (managerd's UpdateFrontendConfig handler) is
// expected to Load first if it wants to change only one field, the
// same convention nodeconfig.Manager.Save already establishes. 0600
// since this file can hold ManagerAPIKey.
func (m *Manager) Save(cfg Config) error {
	if err := validate(cfg); err != nil {
		return err
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// Written atomically via a temp file + explicit chmod, not a plain
	// os.WriteFile (2026-09-15 audit fix): os.WriteFile's mode argument
	// is only applied when it CREATES a file - an existing file (e.g.
	// one hand-created at 0644 before this package ever touched it)
	// kept whatever mode it already had on every previous Save, leaving
	// ManagerAPIKey world/group-readable despite this file's own
	// "must be root-owned, mode 0600" doc comment above.
	return atomicWriteFile(m.path(), body)
}

// atomicWriteFile writes body to path via a temp file in the same
// directory, explicitly chmod 0600, then renames it into place -
// mirroring internal/assumptions.Manager's own atomic-write-then-
// rename convention.
func atomicWriteFile(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".frontendconfig-*.tmp")
	if err != nil {
		return fmt.Errorf("frontendconfig: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once successfully renamed
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("frontendconfig: writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("frontendconfig: syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("frontendconfig: closing temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("frontendconfig: setting permissions: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("frontendconfig: finalizing write: %w", err)
	}
	return nil
}

// validate rejects a value that would be unsafe to interpolate or
// otherwise malformed, without judging semantic correctness (e.g.
// whether ManagerAddr is actually reachable) - the same posture and
// reasoning as nodeconfig.validate's own doc comment.
func validate(cfg Config) error {
	if cfg.ManagerAddr != "" {
		if _, _, err := net.SplitHostPort(cfg.ManagerAddr); err != nil {
			return fmt.Errorf("frontendconfig: invalid manager_addr %q: %w", cfg.ManagerAddr, err)
		}
	}
	if cfg.HTTPAddr != "" {
		if _, _, err := net.SplitHostPort(cfg.HTTPAddr); err != nil {
			return fmt.Errorf("frontendconfig: invalid http_addr %q: %w", cfg.HTTPAddr, err)
		}
	}
	for _, f := range []struct{ name, value string }{
		{"manager_tls_ca", cfg.ManagerTLSCA},
		{"tls_cert", cfg.TLSCert},
		{"tls_key", cfg.TLSKey},
		{"peer_tls_ca", cfg.PeerTLSCA},
	} {
		if err := validateNoNewline(f.name, f.value); err != nil {
			return err
		}
	}
	for _, f := range []struct{ name, value string }{
		{"peer_hostname_suffix", cfg.PeerHostnameSuffix},
		{"peer_manager_port", cfg.PeerManagerPort},
		{"manager_api_key", cfg.ManagerAPIKey},
	} {
		if err := validateNoNewline(f.name, f.value); err != nil {
			return err
		}
	}
	return nil
}

// validateNoNewline rejects a newline/carriage-return - defense in
// depth against future code that might interpolate this value into
// generated text, mirroring nodeconfig.validatePathField's own
// reasoning and doc comment.
func validateNoNewline(name, value string) error {
	if value == "" {
		return nil
	}
	for _, r := range value {
		if r == '\n' || r == '\r' {
			return fmt.Errorf("frontendconfig: invalid %s: must not contain newlines", name)
		}
	}
	return nil
}

// warnIfWorldReadable logs (does not fail) when path is readable by
// group or other - this file is hand-edited, not written by this
// process the way nodeconfig.Manager.Save enforces 0600 for
// managerd's own file, so there is nothing that automatically fixes
// permissions here. A hard startup failure over a permissions nit on
// a live system would itself be a self-inflicted outage risk, so this
// only warns.
func warnIfWorldReadable(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "apiary: %s is readable by group/other but contains manager_api_key - recommend chmod 600\n", path)
	}
}
