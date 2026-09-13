// Package restshimdconfig persists cmd/restshimd's own local
// settings (ADR-0100) as a plain JSON file on disk, replacing what
// were previously its own CLI flags. Hand-edited only - restshimd has
// no web UI of its own to expose live editing through, unlike
// managerd's internal/nodeconfig. A change here takes effect the next
// time restshimd restarts.
package restshimdconfig

import (
	"encoding/json"
	"fmt"
	"os"
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
