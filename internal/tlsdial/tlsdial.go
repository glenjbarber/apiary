// Package tlsdial builds the grpc.DialOption a managerd client (cmd/
// frontend, cmd/restshimd) needs to reach managerd's own external API -
// shared because both commands need the exact same TLS-or-plaintext
// logic, and there's no other natural place for two separate `main`
// packages to share it without introducing a dependency between them.
package tlsdial

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// ManagerDialOption returns the transport credentials a client should
// dial managerd with. useTLS=false (the default for every deployment so
// far) preserves today's plaintext behavior. useTLS=true with an empty
// caFile trusts the host's own system certificate pool - the expected
// case for a real, CA-signed certificate (see cmd/managerd's own
// -tls-cert/-tls-key). caFile, if set, is trusted *instead of* the
// system pool - the expected case for a self-signed certificate, the
// same tradeoff any internal-only TLS deployment faces.
//
// serverName, if set, overrides the hostname used for certificate
// verification independently of the address actually dialed - needed
// when managerd stays loopback-only (its -rpc-addr is 127.0.0.1, never
// exposed on the network) but its certificate names a real public
// hostname a CA like Let's Encrypt could actually issue for (Let's
// Encrypt never issues for "127.0.0.1" or "localhost"). Leave empty to
// verify against the dialed address itself, the default grpc-go
// behavior.
func ManagerDialOption(useTLS bool, caFile, serverName string) (grpc.DialOption, error) {
	if !useTLS {
		return grpc.WithTransportCredentials(insecure.NewCredentials()), nil
	}
	cfg := &tls.Config{ServerName: serverName}
	if caFile == "" {
		return grpc.WithTransportCredentials(credentials.NewTLS(cfg)), nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("tlsdial: reading CA file %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tlsdial: no valid certificates found in %s", caFile)
	}
	cfg.RootCAs = pool
	return grpc.WithTransportCredentials(credentials.NewTLS(cfg)), nil
}
