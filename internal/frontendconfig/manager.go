// Package frontendconfig persists cmd/frontend's own local settings
// (ADR-0100) as a plain JSON file on disk, replacing what were
// previously its own CLI flags and, for ManagerAPIKey, the
// APIARY_MANAGER_API_KEY environment variable (formerly sourced via
// etc/rc.d/apiary_frontend's own envfile mechanism, now retired).
// Hand-edited only - frontend has no RPC layer of its own to expose
// live editing through, unlike managerd's internal/nodeconfig. This
// is unrelated to internal/loginconfig, which persists the login
// role map (a live, RPC-editable, Users-page concern) - untouched by
// this package. A change here takes effect the next time frontend
// restarts.
package frontendconfig

import (
	"encoding/json"
	"fmt"
	"os"
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
