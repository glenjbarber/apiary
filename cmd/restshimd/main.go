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
	"google.golang.org/grpc/credentials/insecure"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/restshim"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("restshimd: %v", err)
	}
}

func run() error {
	managerAddr := flag.String("manager-addr", "127.0.0.1:17700", "TCP address of managerd's external RPC API")
	httpAddr := flag.String("http-addr", "127.0.0.1:8081", "address to serve the REST API on")
	flag.Parse()

	conn, err := grpc.NewClient(*managerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dialing managerd at %s: %w", *managerAddr, err)
	}
	defer conn.Close()

	srv := restshim.NewServer(rpcpb.NewManagerServiceClient(conn))

	log.Printf("restshimd: listening on %s (manager-addr=%s)", *httpAddr, *managerAddr)
	return http.ListenAndServe(*httpAddr, srv)
}
