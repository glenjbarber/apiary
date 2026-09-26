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
	"github.com/glenjbarber/apiary/internal/buildinfo"
	"log"
	"net/http"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/restshim"
	"github.com/glenjbarber/apiary/internal/restshimdconfig"
	"github.com/glenjbarber/apiary/internal/tlsdial"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("restshimd: %v", err)
	}
}

func run() error {
	// These two never had flags of their own, so they never called
	// flag.Parse. Registering -version is what introduces the first
	// one; the parse has to happen for it to take effect at all.
	buildinfo.RegisterVersionFlag(flag.CommandLine)
	flag.Parse()

	if buildinfo.VersionRequested() {
		fmt.Print(buildinfo.Report("restshimd"))
		return nil
	}

	cfg, err := (&restshimdconfig.Manager{}).Load()
	if err != nil {
		return fmt.Errorf("reading %s: %w", restshimdconfig.DefaultPath, err)
	}

	dialCreds, err := tlsdial.ManagerDialOption(cfg.ManagerTLS, cfg.ManagerTLSCA, cfg.ManagerTLSServerName)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(cfg.ManagerAddr, dialCreds)
	if err != nil {
		return fmt.Errorf("dialing managerd at %s: %w", cfg.ManagerAddr, err)
	}
	defer conn.Close()

	srv := restshim.NewServer(rpcpb.NewManagerServiceClient(conn))

	log.Printf("restshimd: %s listening on %s (manager-addr=%s, manager-tls=%v, tls=%v)", buildinfo.String(), cfg.HTTPAddr, cfg.ManagerAddr, cfg.ManagerTLS, cfg.TLSCert != "")
	if cfg.TLSCert != "" || cfg.TLSKey != "" {
		if err := requireTLSPair(cfg.TLSCert, cfg.TLSKey); err != nil {
			return err
		}
		return http.ListenAndServeTLS(cfg.HTTPAddr, cfg.TLSCert, cfg.TLSKey, srv)
	}
	return http.ListenAndServe(cfg.HTTPAddr, srv)
}

// requireTLSPair rejects a config setting only one of tls_cert/tls_key -
// serving TLS needs both, and silently falling back to plaintext
// because only one was set would be a confusing way to fail.
func requireTLSPair(cert, key string) error {
	if cert == "" || key == "" {
		return fmt.Errorf("both tls_cert and tls_key must be set together")
	}
	return nil
}
