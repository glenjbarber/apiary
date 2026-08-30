// Command frontend serves Apiary's HTMX web UI. It is a client of
// managerd's external gRPC API (api/rpc.ManagerService), the same way
// internal/restshim is - it never talks to raftd directly.
package main

import (
	"context"
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

// apiKeyCredentials attaches an API key to every outgoing managerd call
// as gRPC metadata, matching the "authorization: Bearer <key>"
// convention internal/manager's auth interceptor expects (ADR-0023).
// RequireTransportSecurity is false to match this project's existing
// insecure local-network transport (see grpc.WithTransportCredentials
// below) - the key travels in plaintext the same way everything else
// on this connection already does.
type apiKeyCredentials string

func (k apiKeyCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(k)}, nil
}

func (apiKeyCredentials) RequireTransportSecurity() bool { return false }

func main() {
	if err := run(); err != nil {
		log.Fatalf("frontend: %v", err)
	}
}

func run() error {
	managerAddr := flag.String("manager-addr", "127.0.0.1:17700", "TCP address of managerd's external RPC API")
	httpAddr := flag.String("http-addr", "127.0.0.1:8080", "address to serve the web UI on")
	flag.Parse()

	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	// APIARY_MANAGER_API_KEY authenticates every call to managerd once
	// it has API-key auth enabled (ADR-0023) - unset by default, since
	// auth is opt-in and off until the first key is ever created via
	// the /apikeys page. Once that happens, this must be set (and
	// frontend restarted) or every call starts failing Unauthenticated.
	if apiKey := os.Getenv("APIARY_MANAGER_API_KEY"); apiKey != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(apiKeyCredentials(apiKey)))
	}
	conn, err := grpc.NewClient(*managerAddr, dialOpts...)
	if err != nil {
		return fmt.Errorf("dialing managerd at %s: %w", *managerAddr, err)
	}
	defer conn.Close()

	// Credentials come from the environment, not flags, so they don't
	// show up in `ps` output. Both or neither must be set - a single one
	// set is almost certainly a typo, not an intentional "half enabled"
	// state, so it's treated as a startup error rather than silently
	// disabling login.
	user := os.Getenv("APIARY_UI_USER")
	pass := os.Getenv("APIARY_UI_PASSWORD")
	if (user == "") != (pass == "") {
		return fmt.Errorf("both APIARY_UI_USER and APIARY_UI_PASSWORD must be set together (or neither, to disable login)")
	}

	srv, err := frontend.NewServer(rpcpb.NewManagerServiceClient(conn), user, pass)
	if err != nil {
		return fmt.Errorf("creating frontend server: %w", err)
	}
	if user != "" {
		log.Printf("frontend: login enabled (user=%s)", user)
	} else {
		log.Printf("frontend: no login configured (set APIARY_UI_USER/APIARY_UI_PASSWORD to require one)")
	}

	log.Printf("frontend: listening on %s (manager-addr=%s)", *httpAddr, *managerAddr)
	return http.ListenAndServe(*httpAddr, srv)
}
