// Command frontend serves Apiary's HTMX web UI. It is a client of
// managerd's external gRPC API (api/rpc.ManagerService), the same way
// internal/restshim is - it never talks to raftd directly.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

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

	// Credentials come from the environment, not flags, so they don't
	// show up in `ps` output. Both or neither must be set - a single one
	// set is almost certainly a typo, not an intentional "half enabled"
	// state, so it's treated as a startup error rather than silently
	// disabling auth.
	user := os.Getenv("APIARY_UI_USER")
	pass := os.Getenv("APIARY_UI_PASSWORD")
	if (user == "") != (pass == "") {
		return fmt.Errorf("both APIARY_UI_USER and APIARY_UI_PASSWORD must be set together (or neither, to disable auth)")
	}
	var handler http.Handler = srv
	if user != "" {
		handler = frontend.BasicAuth(user, pass, handler)
		log.Printf("frontend: HTTP Basic Auth enabled (user=%s)", user)
	} else {
		log.Printf("frontend: no auth configured (set APIARY_UI_USER/APIARY_UI_PASSWORD to enable HTTP Basic Auth)")
	}

	log.Printf("frontend: listening on %s (manager-addr=%s)", *httpAddr, *managerAddr)
	return http.ListenAndServe(*httpAddr, handler)
}
