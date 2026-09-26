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
	// of plaintext. The default (false) deliberately tracks managerd's own
	// default, which is plaintext too - TLS there is opt-in via its
	// tls_cert/tls_key, and this file has no way to see that, so the two
	// defaults are kept equal rather than one guessing at the other.
	// Turning TLS on in managerd.json is therefore an operator action that
	// must be mirrored here; Config.Validate rejects the half-finished
	// versions of that, and internal/managerlink verifies the result
	// against the live endpoint at startup rather than at first request.
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
// to silently fall back to. A well-formed but self-contradictory one is
// also an error (Config.Validate), on the same reasoning: there is no
// other value to fall back to, and starting with a config that cannot
// work only moves the failure to the first request, where it is
// unattributable.
func (m *Manager) Load() (Config, error) {
	cfg := defaults()
	data, err := os.ReadFile(m.path())
	if err != nil {
		if os.IsNotExist(err) {
			// Validated too, so Load's contract holds on every path: what
			// it returns is either a config that can work or an error.
			// defaults() is valid by construction today; this keeps it
			// that way if it is ever edited.
			if err := cfg.Validate(); err != nil {
				return Config{}, err
			}
			return cfg, nil
		}
		return Config{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("restshimdconfig: parsing %s: %w", m.path(), err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
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
	if err := cfg.Validate(); err != nil {
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

// Validate rejects a config that cannot work, or that contradicts itself.
//
// It is deliberately confined to what the file itself can prove, with no
// I/O: whether managerd actually speaks the scheme this file asks for is
// a fact about another process on the network, and no config file can know
// it. That half of the check lives in internal/managerlink, which asks the
// live endpoint at startup and refuses to serve when the two disagree.
// Keeping the two apart matters - this one says "these two lines of one
// file contradict each other", that one says "this file and managerd
// disagree" - and together they mean a wrong scheme is caught before the
// first request instead of during it.
func (c Config) Validate() error {
	if c.ManagerAddr == "" {
		return fmt.Errorf("restshimdconfig: manager_addr must be set - restshimd has no other way to reach managerd")
	}
	if err := validateHostPort("manager_addr", c.ManagerAddr); err != nil {
		return err
	}
	if c.HTTPAddr != "" {
		if err := validateHostPort("http_addr", c.HTTPAddr); err != nil {
			return err
		}
	}
	for _, f := range []struct{ name, value string }{
		{"manager_tls_ca", c.ManagerTLSCA},
		{"manager_tls_server_name", c.ManagerTLSServerName},
		{"tls_cert", c.TLSCert},
		{"tls_key", c.TLSKey},
	} {
		if err := validateNoNewline(f.name, f.value); err != nil {
			return err
		}
	}
	if err := c.validateTLSConsistency(); err != nil {
		return err
	}
	return c.validateTLSPair()
}

// validateTLSConsistency rejects trust settings that cannot be in effect.
// manager_tls_ca and manager_tls_server_name are consulted only when
// manager_tls is true - tlsdial.ManagerDialOption ignores the CA entirely
// on a plaintext dial, by design - so setting either while manager_tls is
// false means two settings in one file disagree about the same thing, and
// one of them is wrong. That is precisely the shape of the live failure
// this file now has to be able to catch (managerd was switched to TLS and
// this file was left behind), so it is caught here at the point where the
// two settings can still be named, rather than in a 502 afterwards.
func (c Config) validateTLSConsistency() error {
	if c.ManagerTLS {
		return nil
	}
	for _, f := range []struct{ name, value string }{
		{"manager_tls_ca", c.ManagerTLSCA},
		{"manager_tls_server_name", c.ManagerTLSServerName},
	} {
		if f.value != "" {
			return fmt.Errorf("restshimdconfig: %s is set to %q but manager_tls is false: "+
				"%s only takes effect on a TLS dial, so either manager_tls should be true or "+
				"%s should be empty - exactly one of the two is wrong", f.name, f.value, f.name, f.name)
		}
	}
	return nil
}

// validateTLSPair rejects serving TLS with only one of tls_cert/tls_key.
// cmd/restshimd checks this again at startup, where it turns into a
// refusal to serve plaintext by accident; doing it here as well means a
// config like that cannot be written in the first place, by hand or
// through managerd's UpdateRestshimdConfig.
func (c Config) validateTLSPair() error {
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return fmt.Errorf("restshimdconfig: tls_cert and tls_key must be set together "+
			"(tls_cert=%q, tls_key=%q) - serving HTTPS needs both, and serving plaintext because "+
			"only one was set would be a confusing way to fail", c.TLSCert, c.TLSKey)
	}
	return nil
}

// validateHostPort rejects an address that could not be dialed or served
// on, naming the field so a typo in a hand-edited file is obvious.
func validateHostPort(field, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("restshimdconfig: invalid %s %q: %w", field, addr, err)
	}
	if port == "" {
		return fmt.Errorf("restshimdconfig: invalid %s %q: no port", field, addr)
	}
	if host == "" && field == "manager_addr" {
		return fmt.Errorf("restshimdconfig: invalid %s %q: no host - dial an explicit address, "+
			"not a wildcard", field, addr)
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
