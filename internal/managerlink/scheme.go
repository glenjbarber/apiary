// Package managerlink answers one question that grpc.NewClient cannot:
// does the managerd endpoint this process is configured to reach actually
// speak the transport scheme this process is configured to speak?
//
// grpc.NewClient is lazy by design - it resolves a target and returns a
// ClientConn without ever opening a socket - so a scheme disagreement is
// not a startup error, it is a per-request surprise. restshimd's live
// failure was exactly that: manager_tls=false in restshimd.json against
// a TLS-enabled managerd, which surfaced as "error reading server
// preface: EOF" and a bare 502 to every REST caller, with nothing
// anywhere in either log saying the two files disagreed.
//
// One rule governs everything here: what the probe learns is only ever
// *compared* against what was configured, never used to pick credentials
// on its own. Dialing whichever way the peer happens to answer would turn
// a configuration error into a silent downgrade, and would make a
// security decision nobody asked for. A misclassification can therefore
// only ever cost a refused start with a precise message.
package managerlink

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/glenjbarber/apiary/internal/tlsdial"
)

// Scheme is what an endpoint speaks on the wire, as far as anyone can
// tell by talking to it.
type Scheme string

const (
	// SchemePlaintext is an endpoint that answered something that is not a
	// TLS record - in this codebase, an HTTP/2 (gRPC) server.
	SchemePlaintext Scheme = "plaintext"

	// SchemeTLS is an endpoint that answered with a TLS record. Either it
	// completed a handshake with our plaintext preface (bizarre) or, as
	// every real TLS listener does, it sent an alert saying the record it
	// was given was not a handshake (see Detect).
	SchemeTLS Scheme = "tls"

	// SchemeUnknown is "no evidence", not a third answer: the probe
	// connected but got nothing it could classify, or could not connect at
	// all. Never rounded toward either real scheme - a TLS posture that
	// could not be established must not be reported as one that was.
	SchemeUnknown Scheme = "unknown"
)

func (s Scheme) String() string { return string(s) }

// DialFunc is the injectable dialer Detect uses, so the whole probe can
// be tested in-process (bufconn, net.Pipe) with no live network.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// DefaultDial is the real-network dialer used when Config.Dial is nil.
func DefaultDial(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

// clientPreface is the HTTP/2 connection preface every HTTP/2 client
// sends first (RFC 9113 §3.4), verbatim. It is a fixed 24-byte constant
// with no options in it, so sending it discloses nothing about this
// process, carries no credential, and no API key or forwarded
// Authorization header ever travels over a probe connection.
const clientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// Detect reports what addr actually speaks.
//
// It tries the HTTP/2 connection preface first, because in the common
// plaintext deployment that one connection answers the question outright
// and nothing TLS-shaped is ever sent to a plaintext port. A reply that
// identifies a scheme ends it there.
//
// The preface alone cannot answer for a TLS listener, though, and the
// reason is worth recording: a Go gRPC server with TLS enabled answers a
// plaintext preface with *nothing at all* - a clean EOF, no alert record
// (measured, not assumed; see Detect's tests). Silence is not evidence, so
// when the preface gets no usable reply Detect asks the same question in
// the other direction - a real TLS ClientHello, with verification
// disabled - and answers from that instead. A peer that completes a
// handshake speaks TLS, even a doomed one: an untrusted certificate still
// proves the peer is a TLS server, which is the only fact being asked for
// here. Trust is a separate question, asked separately, by verifyTrust.
//
// Neither probe ever carries a request, a credential or an API key; both
// are closed after one exchange.
func Detect(ctx context.Context, addr string, dial DialFunc, timeout time.Duration) (Scheme, error) {
	scheme, err := detectPreface(ctx, addr, dial, timeout)
	if scheme != SchemeUnknown {
		return scheme, err
	}
	// The preface got nothing usable. Ask again, in the other direction,
	// with the preface's own failure kept as context for the case where
	// this one is silent too.
	if tlsScheme, tlsErr := detectClientHello(ctx, addr, dial, timeout); tlsScheme != SchemeUnknown {
		return tlsScheme, tlsErr
	}
	return SchemeUnknown, err
}

// detectPreface writes the HTTP/2 client preface and classifies the first
// byte that comes back. The classification is deliberately one-sided: a
// first byte of 0x14-0x17 is a TLS record type (change_cipher_spec,
// alert, handshake, application_data) and nothing else can be, so
// SchemeTLS is a *positive* identification. Any other first byte means
// "not TLS" - sound in that direction too, because a TLS listener cannot
// answer a plaintext preface with a non-TLS record.
//
// An EOF, a timeout, or an unwritable socket is SchemeUnknown: no
// evidence, and deliberately not rounded toward either real scheme.
func detectPreface(ctx context.Context, addr string, dial DialFunc, timeout time.Duration) (Scheme, error) {
	if dial == nil {
		dial = DefaultDial
	}
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return SchemeUnknown, fmt.Errorf("connecting to %s: %w", addr, err)
	}
	defer conn.Close()

	if err := setDeadline(ctx, conn, timeout); err != nil {
		return SchemeUnknown, fmt.Errorf("setting a deadline on the probe connection to %s: %w", addr, err)
	}
	if _, err := conn.Write([]byte(clientPreface)); err != nil {
		return SchemeUnknown, fmt.Errorf("writing an HTTP/2 preface to %s: %w", addr, err)
	}
	var first [1]byte
	if _, err := io.ReadFull(conn, first[:]); err != nil {
		return SchemeUnknown, fmt.Errorf("reading %s's reply to an HTTP/2 preface: %w", addr, err)
	}
	if isTLSRecordType(first[0]) {
		return SchemeTLS, nil
	}
	return SchemePlaintext, nil
}

// detectClientHello asks the question in the TLS direction: it completes
// a real TLS handshake if the peer speaks TLS, with certificate
// verification explicitly disabled.
//
// Verification is off here on purpose, and this function's result must
// never be used as a reason to trust anything: it answers one question -
// is the peer a TLS server - and a handshake that succeeds against an
// untrusted certificate is the *proof* of a TLS peer. What is worth
// trusting is decided by verifyTrust, using the configured CA and server
// name, and nothing here feeds into that.
func detectClientHello(ctx context.Context, addr string, dial DialFunc, timeout time.Duration) (Scheme, error) {
	cfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // detection only; see above
	if host, _, err := net.SplitHostPort(addr); err == nil {
		// SNI, so a TLS listener that serves several names answers the
		// one it was asked about. Harmless with verification off, and Go
		// omits SNI for an IP literal regardless.
		cfg.ServerName = host
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dialer := &tls.Dialer{Config: cfg, NetDialer: &net.Dialer{}}
	if dial != nil {
		// Keep the injectable dialer in the loop (net.Pipe in tests) by
		// handshaking over it rather than via tls.Dialer's own.
		raw, err := dial(tctx, "tcp", addr)
		if err != nil {
			return SchemeUnknown, fmt.Errorf("connecting to %s for a TLS handshake: %w", addr, err)
		}
		defer raw.Close()
		if err := setDeadline(ctx, raw, timeout); err != nil {
			return SchemeUnknown, err
		}
		return classifyHandshake(tls.Client(raw, cfg).HandshakeContext(tctx))
	}
	conn, err := dialer.DialContext(tctx, "tcp", addr)
	if err != nil {
		return classifyHandshake(err)
	}
	defer conn.Close()
	return SchemeTLS, nil
}

// classifyHandshake reads the one fact a handshake carries about the
// peer's identity: a peer that answered a ClientHello with a non-TLS
// record is definitively not a TLS server.
func classifyHandshake(err error) (Scheme, error) {
	if err == nil {
		return SchemeTLS, nil
	}
	var recordHeaderErr tls.RecordHeaderError
	if errors.As(err, &recordHeaderErr) {
		// The peer sent something that is not a TLS record - an HTTP/2
		// GOAWAY, most likely, from a plaintext server rejecting the
		// bytes we sent it. Definitive, not an inference.
		return SchemePlaintext, nil
	}
	return SchemeUnknown, err
}

// setDeadline bounds one probe exchange, honoring the earlier of the
// caller's own deadline and timeout.
func setDeadline(ctx context.Context, conn net.Conn, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	return conn.SetDeadline(deadline)
}

// isTLSRecordType reports whether b is one of the four TLS record types
// (RFC 8446 §5.1). Those are the only values a TLS endpoint can legally
// start its reply with.
func isTLSRecordType(b byte) bool {
	switch b {
	case 0x14, 0x15, 0x16, 0x17:
		return true
	default:
		return false
	}
}

// verifyTrust performs a real TLS handshake against addr using exactly
// the trust material the gRPC dial will use (tlsdial.ManagerTLSConfig, so
// there is one set of rules rather than two), and returns the handshake's
// own error verbatim - an x509 verification failure names the exact
// problem, and flattening it into "TLS failed" would throw that away.
//
// An empty ServerName is filled in from addr's host here, exactly as
// grpc's own TLS credentials do at dial time (credentials.NewTLS's
// ClientHandshake), so a check that passes here cannot fail there for
// want of a hostname.
func verifyTrust(ctx context.Context, addr, caFile, serverName string) error {
	cfg, err := tlsdial.ManagerTLSConfig(true, caFile, serverName)
	if err != nil {
		return err
	}
	if cfg.ServerName == "" {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("splitting %s into host and port to verify managerd's certificate: %w", addr, err)
		}
		cfg.ServerName = host
	}
	d := &tls.Dialer{Config: cfg}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.(*tls.Conn).HandshakeContext(ctx)
}
