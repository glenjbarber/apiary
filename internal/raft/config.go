// Package raft wraps HashiCorp's raft library into a single-node-bootstrap
// (for now) consensus node, backed by raft-boltdb for durable storage, and
// exposes it over the internal gRPC-over-UDS protocol defined in
// api/internal/raftd.proto.
package raft

import (
	"errors"
	"os"
)

// Config configures a Node.
type Config struct {
	// NodeID uniquely identifies this node within the raft cluster. If
	// empty, it defaults to the machine hostname.
	NodeID string

	// DataDir is the directory used for the raft log/stable store (BoltDB)
	// and file-based snapshots. It is created if it does not exist.
	DataDir string

	// BindAddr is the TCP address the raft transport listens on for
	// inter-node communication - a real network address in a genuine
	// multi-node cluster, not necessarily loopback.
	BindAddr string

	// TLSCert, TLSKey, and TLSCA configure mutual TLS on the raft
	// transport itself (ADR-0078) - this node's own certificate/key and
	// the CA used to verify every peer. All three must be set together,
	// or all left empty for today's plain-TCP behavior (see
	// withDefaults). Opt-in, off by default, matching every other TLS
	// capability in this codebase.
	TLSCert string
	TLSKey  string
	TLSCA   string
}

// DefaultBindAddr is used when Config.BindAddr is empty.
const DefaultBindAddr = "127.0.0.1:17600"

// withDefaults returns a copy of cfg with defaults applied.
func (cfg Config) withDefaults() (Config, error) {
	if cfg.NodeID == "" {
		host, err := os.Hostname()
		if err != nil {
			return cfg, err
		}
		cfg.NodeID = host
	}
	if cfg.BindAddr == "" {
		cfg.BindAddr = DefaultBindAddr
	}
	if cfg.DataDir == "" {
		return cfg, errors.New("raft: Config.DataDir must be set")
	}
	if !allSetOrAllEmpty(cfg.TLSCert, cfg.TLSKey, cfg.TLSCA) {
		return cfg, errors.New("raft: TLSCert, TLSKey, and TLSCA must all be set together, or all left empty")
	}
	return cfg, nil
}

func allSetOrAllEmpty(values ...string) bool {
	empty := 0
	for _, v := range values {
		if v == "" {
			empty++
		}
	}
	return empty == 0 || empty == len(values)
}
