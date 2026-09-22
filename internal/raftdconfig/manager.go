// Package raftdconfig persists cmd/raftd's own local settings
// (ADR-0100) as a plain JSON file on disk, replacing what were
// previously its own CLI flags. Hand-edited only, for every field -
// unlike managerd's own internal/nodeconfig, there is no RPC path that
// writes this file at all. managerd's RPC does read it (GetRaftdConfig,
// ADR-0102, for read-only display on the Machine page), but there is
// no UpdateRaftdConfig: a 2026-09-15 audit found that editing
// internal_token or raft TLS material through a plain per-host save
// form is unsafe for a consensus-critical daemon - internal_token must
// match managerd's own separately-configured raftd_token for
// RaftInternal auth to keep working, and raft TLS material is
// cluster-coupled (changing it on one voter and restarting can isolate
// that voter and lose quorum). Both need a real coordinated rotation
// workflow (updating every affected file, and for TLS checking peer
// compatibility, before any restart) - out of scope here. Save exists
// on this Manager purely as general-purpose infrastructure a future
// rotation workflow could use; nothing in this codebase calls it today.
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
	"net"
	"os"
	"path/filepath"

	"github.com/glenjbarber/apiary/internal/commonconfig"
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
//
// Before reading this file, Load also consults common.json (ADR-0111)
// in the same directory for fields shared across every daemon on this
// Comb. A field this file itself sets always wins; common.json only
// fills in node_id when this file leaves it unset.
func (m *Manager) Load() (Config, error) {
	common, err := loadCommonConfig(m.path())
	if err != nil {
		return Config{}, err
	}
	cfg := Defaults()
	data, err := os.ReadFile(m.path())
	if err != nil {
		if os.IsNotExist(err) {
			return applyCommonConfig(cfg, common), nil
		}
		return Config{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("raftdconfig: parsing %s: %w", m.path(), err)
	}
	if cfg.InternalToken != "" {
		warnIfWorldReadable(m.path())
	}
	return applyCommonConfig(cfg, common), nil
}

// loadCommonConfig loads common.json from the same directory as
// servicePath (ADR-0111). A missing common.json is not an error - it
// yields a zero-value commonconfig.Config, so applyCommonConfig below
// has nothing to fill in.
func loadCommonConfig(servicePath string) (commonconfig.Config, error) {
	cm := commonconfig.Manager{Path: filepath.Join(filepath.Dir(servicePath), filepath.Base(commonconfig.DefaultPath))}
	return cm.Load()
}

// applyCommonConfig fills NodeID from common when this file doesn't
// set it itself - the only field raftdconfig.Config shares with
// commonconfig.Config today (ADR-0111).
func applyCommonConfig(cfg Config, common commonconfig.Config) Config {
	if cfg.NodeID == "" {
		cfg.NodeID = common.NodeID
	}
	return cfg
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

// Save writes cfg, replacing whatever was there before in full (not a
// merge) - the caller (managerd's UpdateRaftdConfig handler) is
// expected to Load first and carry over every RPC-excluded field
// (see the package doc comment) before calling Save, the same
// convention nodeconfig.Manager.Save already establishes. Save itself
// persists whatever Config it's given, full stop - it does not know or
// enforce which fields the RPC layer chooses to expose. 0600 since
// this file can hold InternalToken.
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
	// alone is insufficient for an already-existing file. Especially
	// important here: this file can hold InternalToken.
	return atomicWriteFile(m.path(), body)
}

// atomicWriteFile writes body to path via a temp file in the same
// directory, explicitly chmod 0600, then renames it into place -
// mirroring internal/assumptions.Manager's own atomic-write-then-
// rename convention.
func atomicWriteFile(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".raftdconfig-*.tmp")
	if err != nil {
		return fmt.Errorf("raftdconfig: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once successfully renamed
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("raftdconfig: writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("raftdconfig: syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("raftdconfig: closing temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("raftdconfig: setting permissions: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("raftdconfig: finalizing write: %w", err)
	}
	return nil
}

// validate rejects a value that would be unsafe to interpolate or
// otherwise malformed, without judging semantic correctness (e.g.
// whether DataDir actually exists) - the same posture and reasoning as
// nodeconfig.validate's own doc comment. Join/AwaitJoin get no
// additional validation here beyond Load's own JSON unmarshaling,
// since they're excluded from UpdateRaftdConfig entirely (see the
// package doc comment) - this only needs to protect hand-edited files.
func validate(cfg Config) error {
	if cfg.DataDir != "" && !filepath.IsAbs(cfg.DataDir) {
		return fmt.Errorf("raftdconfig: invalid data_dir %q: must be an absolute path", cfg.DataDir)
	}
	if cfg.Socket != "" && !filepath.IsAbs(cfg.Socket) {
		return fmt.Errorf("raftdconfig: invalid socket %q: must be an absolute path", cfg.Socket)
	}
	if cfg.RaftBind != "" {
		if _, _, err := net.SplitHostPort(cfg.RaftBind); err != nil {
			return fmt.Errorf("raftdconfig: invalid raft_bind %q: %w", cfg.RaftBind, err)
		}
	}
	for _, f := range []struct{ name, value string }{
		{"raft_tls_cert", cfg.RaftTLSCert},
		{"raft_tls_key", cfg.RaftTLSKey},
		{"raft_tls_ca", cfg.RaftTLSCA},
		{"internal_token", cfg.InternalToken},
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
			return fmt.Errorf("raftdconfig: invalid %s: must not contain newlines", name)
		}
	}
	return nil
}
