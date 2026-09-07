package raft

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/hashicorp/raft"
)

// tlsStreamLayer implements raft.StreamLayer over a mutually authenticated
// TLS connection between cluster members. Unlike the plain TCP transport
// raft.NewTCPTransport builds, both the accepting and dialing side present
// a certificate and verify the other's against a shared CA pool -
// appropriate here since raft members form a closed, symmetric peer set
// fixed by the cluster configuration, not a public-facing API the way
// managerd's own external gRPC endpoint is (see internal/tlsdial, which
// only ever verifies a server certificate, never presents a client one).
type tlsStreamLayer struct {
	net.Listener
	tlsConfig *tls.Config
}

// newTLSConfig loads a mutual-TLS config from certFile/keyFile (this
// node's own identity) and caFile (used to verify every peer, since raft
// members dial and accept from each other symmetrically - the same pool
// serves as both RootCAs, for the side dialing out, and ClientCAs, for
// the side accepting a connection).
func newTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("raft: loading TLS certificate/key: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("raft: reading TLS CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("raft: no valid certificates found in CA file %s", caFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// newTLSStreamLayer starts listening on bindAddr with tlsConfig, ready to
// use as a raft.NetworkTransport's StreamLayer.
func newTLSStreamLayer(bindAddr string, tlsConfig *tls.Config) (*tlsStreamLayer, error) {
	listener, err := tls.Listen("tcp", bindAddr, tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("raft: creating TLS listener: %w", err)
	}
	return &tlsStreamLayer{Listener: listener, tlsConfig: tlsConfig}, nil
}

// Dial implements raft.StreamLayer. It presents this node's own
// certificate and verifies the peer's against the same CA pool used to
// accept incoming connections - see newTLSConfig.
func (t *tlsStreamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	return tls.DialWithDialer(dialer, "tcp", string(address), t.tlsConfig)
}

// newTransport builds cfg's raft.Transport: a plain raft.NewTCPTransport
// when TLSCert/TLSKey/TLSCA are all empty (today's only behavior, and
// still the default), or a mutually authenticated TLS transport when all
// three are set - cfg.withDefaults already rejects any partial set.
func newTransport(cfg Config) (raft.Transport, error) {
	if cfg.TLSCert == "" && cfg.TLSKey == "" && cfg.TLSCA == "" {
		addr, err := net.ResolveTCPAddr("tcp", cfg.BindAddr)
		if err != nil {
			return nil, fmt.Errorf("raft: resolving bind addr: %w", err)
		}
		transport, err := raft.NewTCPTransport(cfg.BindAddr, addr, 3, 10*time.Second, os.Stderr)
		if err != nil {
			return nil, fmt.Errorf("raft: creating transport: %w", err)
		}
		return transport, nil
	}
	tlsConfig, err := newTLSConfig(cfg.TLSCert, cfg.TLSKey, cfg.TLSCA)
	if err != nil {
		return nil, err
	}
	streamLayer, err := newTLSStreamLayer(cfg.BindAddr, tlsConfig)
	if err != nil {
		return nil, err
	}
	return raft.NewNetworkTransport(streamLayer, 3, 10*time.Second, os.Stderr), nil
}
