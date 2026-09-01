// Command restshimd serves internal/restshim's REST/JSON translation of
// managerd's external gRPC API. Like cmd/frontend, it's just another
// client of ManagerService, dialed the same way - it never talks to
// raftd directly. Unlike cmd/frontend, it attaches no API key of its
// own: each caller's own "Authorization" header is forwarded straight
// through to managerd as gRPC metadata (see internal/restshim's
// authContext), since restshim has no single application-level
// identity the way the UI does - it's meant to sit in front of
// external tooling (curl, Terraform, CI) where each caller presents
// its own key.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/restshim"
	"github.com/glenjbarber/apiary/internal/tlsdial"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("restshimd: %v", err)
	}
}

func run() error {
	managerAddr := flag.String("manager-addr", "127.0.0.1:17700", "TCP address of managerd's external RPC API")
	httpAddr := flag.String("http-addr", "127.0.0.1:8081", "address to serve the REST API on")
	managerTLS := flag.Bool("manager-tls", false, "dial managerd over TLS instead of plaintext (must match managerd's own -tls-cert/-tls-key)")
	managerTLSCA := flag.String("manager-tls-ca", "", "PEM CA file to trust for managerd's certificate (for a self-signed cert); leave empty to trust the system certificate pool")
	managerTLSServerName := flag.String("manager-tls-server-name", "", "hostname to verify managerd's certificate against, if different from -manager-addr's host (e.g. managerd stays loopback-only but its cert names a real public hostname); leave empty to verify against -manager-addr itself")
	tlsCert := flag.String("tls-cert", "", "PEM certificate file to serve the REST API over HTTPS; leave unset (with -tls-key) to serve plaintext HTTP, as before")
	tlsKey := flag.String("tls-key", "", "PEM private key file matching -tls-cert")
	flag.Parse()

	dialCreds, err := tlsdial.ManagerDialOption(*managerTLS, *managerTLSCA, *managerTLSServerName)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(*managerAddr, dialCreds)
	if err != nil {
		return fmt.Errorf("dialing managerd at %s: %w", *managerAddr, err)
	}
	defer conn.Close()

	srv := restshim.NewServer(rpcpb.NewManagerServiceClient(conn))

	log.Printf("restshimd: listening on %s (manager-addr=%s, manager-tls=%v, tls=%v)", *httpAddr, *managerAddr, *managerTLS, *tlsCert != "")
	if *tlsCert != "" || *tlsKey != "" {
		if *tlsCert == "" || *tlsKey == "" {
			return fmt.Errorf("both -tls-cert and -tls-key must be set together")
		}
		return http.ListenAndServeTLS(*httpAddr, *tlsCert, *tlsKey, srv)
	}
	return http.ListenAndServe(*httpAddr, srv)
}
