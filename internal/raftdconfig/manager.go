// Package raftdconfig persists cmd/raftd's own local settings
// (ADR-0100) as a plain JSON file on disk, replacing what were
// previously its own CLI flags. Hand-edited only - raftd has no web
// UI of its own to expose live editing through, unlike managerd's
// internal/nodeconfig. A change here takes effect the next time
// raftd restarts.
//
// Deliberately excluded: raftd's five one-shot recovery/destructive
// flags (-reset, -export, -restore, -restore-file, -restore-dry-run)
// stay CLI-only, never fields on this struct. Every daemon here runs
// under daemon(8)'s -r auto-restart supervisor, so an
// accidentally-persistent destructive value in a config file would
// act on every single respawn - a CLI arg typed once has no such
// risk. See cmd/raftd/main.go's own resetConfirmPhrase/
// restoreConfirmPhrase doc comments for the full reasoning.
package raftdconfig

import (
	"encoding/json"
	"fmt"
	"os"

	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

// DefaultPath is where this file lives by default on a pkg-installed
// FreeBSD system, alongside every other daemon's own config file
// under /usr/local/etc/apiary (ADR-0100).
const DefaultPath = "/usr/local/etc/apiary/raftd.json"

// Config is the full set of settings raftd reads at startup. Every
// field mirrors a flag raftd used to take on the command line - see
// each field's own doc comment for the flag it replaces.
type Config struct {
	// DataDir mirrors -data-dir: directory for raft log/stable store
	// and snapshots.
	DataDir string `json:"data_dir,omitempty"`

	// Socket mirrors -socket: Unix domain socket path for the
	// internal RaftInternal protocol.
	Socket string `json:"socket,omitempty"`

	// NodeID mirrors -node-id: unique ID for this raft node. Empty
	// means "use os.Hostname()", resolved by internal/raft itself.
	NodeID string `json:"node_id,omitempty"`

	// RaftBind mirrors -raft-bind: TCP address for the raft
	// transport.
	RaftBind string `json:"raft_bind,omitempty"`

	// Join mirrors -join: internal socket path of an existing cluster
	// member to join through. Safe to leave set across restarts once
	// this node has already joined or bootstrapped - raftd only ever
	// consults this on a truly fresh, empty DataDir (see
	// startupJoinOrBootstrap's own doc comment).
	Join string `json:"join,omitempty"`

	// AwaitJoin mirrors -await-join (ADR-0083): on a fresh, empty
	// DataDir, wait to be added as a voter rather than
	// self-bootstrapping. Same "only consulted on a fresh DataDir"
	// safety as Join above. Mutually exclusive with Join.
	AwaitJoin bool `json:"await_join,omitempty"`

	// InternalToken mirrors -internal-token: shared secret required
	// from every RaftInternal caller. A real credential - this file
	// must be root-owned, mode 0600 (Load only warns, see
	// warnIfWorldReadable; it does not refuse to start).
	InternalToken string `json:"internal_token,omitempty"`

	// RaftTLSCert/RaftTLSKey/RaftTLSCA mirror -raft-tls-cert/
	// -raft-tls-key/-raft-tls-ca (ADR-0078): this node's own
	// certificate/key and the CA bundle used to verify every peer on
	// the raft transport. Leave all three empty for plain-TCP
	// behavior.
	RaftTLSCert string `json:"raft_tls_cert,omitempty"`
	RaftTLSKey  string `json:"raft_tls_key,omitempty"`
	RaftTLSCA   string `json:"raft_tls_ca,omitempty"`
}

// Defaults matches every flag's own former default value exactly, so
// a missing or partial config file behaves identically to how an
// unset flag used to. Exported so cmd/raftd's own one-shot recovery
// flags (-reset/-export/-restore/-restore-dry-run) can fall back to
// it directly when the config file itself fails to load - see this
// package's own doc comment for why those flags must keep working
// even when the config is broken.
func Defaults() Config {
	return Config{
		DataDir:  "/var/db/apiary/raftd",
		Socket:   "/var/run/apiary/raftd.sock",
		RaftBind: raftnode.DefaultBindAddr,
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

// Load reads the persisted config, starting from Defaults() so a
// missing file - or one that only sets some fields - behaves exactly
// like every flag's own former default for whatever it doesn't set.
// A missing file is not an error (a fresh install with no config
// written yet); a malformed one is, since there is no flag value left
// to silently fall back to. Warns (does not fail) if the file is
// readable by group/other despite holding InternalToken - see
// warnIfWorldReadable.
func (m *Manager) Load() (Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(m.path())
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return Config{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("raftdconfig: parsing %s: %w", m.path(), err)
	}
	if cfg.InternalToken != "" {
		warnIfWorldReadable(m.path())
	}
	return cfg, nil
}

// warnIfWorldReadable logs (does not fail) when path is readable by
// group or other - this file is hand-edited, not written by this
// process, so nothing automatically fixes permissions here. A hard
// startup failure over a permissions nit on a live system would
// itself be a self-inflicted outage risk, so this only warns.
func warnIfWorldReadable(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "apiary: %s is readable by group/other but contains internal_token - recommend chmod 600\n", path)
	}
}
