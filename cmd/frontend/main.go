// Command frontend serves Apiary's HTMX web UI. It is a client of
// managerd's external gRPC API (api/rpc.ManagerService), the same way
// internal/restshim is - it never talks to raftd directly.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/frontend"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("frontend: %v", err)
	}
}

func run() error {
	managerAddr := flag.String("manager-addr", "127.0.0.1:17700", "TCP address of managerd's external RPC API")
	httpAddr := flag.String("http-addr", "127.0.0.1:8080", "address to serve the web UI on")
	flag.Parse()

	conn, err := grpc.NewClient(*managerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dialing managerd at %s: %w", *managerAddr, err)
	}
	defer conn.Close()

	srv, err := frontend.NewServer(rpcpb.NewManagerServiceClient(conn))
	if err != nil {
		return fmt.Errorf("creating frontend server: %w", err)
	}

	log.Printf("frontend: listening on %s (manager-addr=%s)", *httpAddr, *managerAddr)
	return http.ListenAndServe(*httpAddr, srv)
}
