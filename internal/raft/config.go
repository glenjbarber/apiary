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

	// BindAddr is the loopback TCP address the raft transport listens on
	// for inter-node communication. Even though v1 only supports
	// single-node bootstrap, a real TCP transport (rather than an in-memory
	// one) is used so a later multi-node slice needs no transport rework.
	BindAddr string
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
	return cfg, nil
}
