// Package restshimdconfig persists cmd/restshimd's own local
// settings (ADR-0100) as a plain JSON file on disk, replacing what
// were previously its own CLI flags. Save (ADR-0102) is called only
// from managerd's own RPC handlers (UpdateRestshimdConfig) - restshimd
// itself has no RPC/web UI of its own to expose live editing through
// directly, unlike managerd's own internal/nodeconfig, so managerd
// writes this file on restshimd's behalf instead (the two are always
// co-located on the same host). A change here takes effect the next
// time restshimd restarts.
package restshimdconfig

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
const DefaultPath = "/usr/local/etc/apiary/restshimd.json"

// Config is the full set of settings restshimd reads at startup.
// Every field mirrors a flag restshimd used to take on the command
// line - see each field's own doc comment for the flag it replaces.
type Config struct {
	// ManagerAddr mirrors -manager-addr: TCP address of managerd's
	// external RPC API.
	ManagerAddr string `json:"manager_addr,omitempty"`

	// HTTPAddr mirrors -http-addr: address to serve the REST API on.
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

	// TLSCert/TLSKey mirror -tls-cert/-tls-key: serve the REST API
	// over HTTPS; leave both empty to serve plaintext HTTP.
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`
}

// defaults matches every flag's own former default value exactly, so
// a missing or partial config file behaves identically to how an
// unset flag used to.
func defaults() Config {
	return Config{
		ManagerAddr: "127.0.0.1:17700",
		HTTPAddr:    "127.0.0.1:8081",
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
// to silently fall back to.
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
		return Config{}, fmt.Errorf("restshimdconfig: parsing %s: %w", m.path(), err)
	}
	return cfg, nil
}

// Save writes cfg, replacing whatever was there before in full (not a
// merge) - the caller (managerd's UpdateRestshimdConfig handler) is
// expected to Load first if it wants to change only one field, the
// same convention nodeconfig.Manager.Save already establishes. 0600 to
// match every sibling config file's own convention, even though this
// one holds no secret today.
func (m *Manager) Save(cfg Config) error {
	if err := validate(cfg); err != nil {
		return err
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// Written atomically via a temp file + explicit chmod, not a plain
	// os.WriteFile (2026-09-15 audit fix) - see frontendconfig.Save's
	// own doc comment for why a plain os.WriteFile's mode argument
	// alone is insufficient for an already-existing file.
	return atomicWriteFile(m.path(), body)
}

// atomicWriteFile writes body to path via a temp file in the same
// directory, explicitly chmod 0600, then renames it into place -
// mirroring internal/assumptions.Manager's own atomic-write-then-
// rename convention.
func atomicWriteFile(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".restshimdconfig-*.tmp")
	if err != nil {
		return fmt.Errorf("restshimdconfig: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once successfully renamed
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("restshimdconfig: writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("restshimdconfig: syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("restshimdconfig: closing temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("restshimdconfig: setting permissions: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("restshimdconfig: finalizing write: %w", err)
	}
	return nil
}

// validate rejects a value that would be unsafe to interpolate or
// otherwise malformed, without judging semantic correctness - the same
// posture and reasoning as nodeconfig.validate's own doc comment.
func validate(cfg Config) error {
	if cfg.ManagerAddr != "" {
		if _, _, err := net.SplitHostPort(cfg.ManagerAddr); err != nil {
			return fmt.Errorf("restshimdconfig: invalid manager_addr %q: %w", cfg.ManagerAddr, err)
		}
	}
	if cfg.HTTPAddr != "" {
		if _, _, err := net.SplitHostPort(cfg.HTTPAddr); err != nil {
			return fmt.Errorf("restshimdconfig: invalid http_addr %q: %w", cfg.HTTPAddr, err)
		}
	}
	for _, f := range []struct{ name, value string }{
		{"manager_tls_ca", cfg.ManagerTLSCA},
		{"tls_cert", cfg.TLSCert},
		{"tls_key", cfg.TLSKey},
	} {
		if err := validateNoNewline(f.name, f.value); err != nil {
			return err
		}
	}
	return nil
}

// validateNoNewline rejects a newline/carriage-return - defense in
// depth, mirroring nodeconfig.validatePathField's own reasoning.
func validateNoNewline(name, value string) error {
	if value == "" {
		return nil
	}
	for _, r := range value {
		if r == '\n' || r == '\r' {
			return fmt.Errorf("restshimdconfig: invalid %s: must not contain newlines", name)
		}
	}
	return nil
}
