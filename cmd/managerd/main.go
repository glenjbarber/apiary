// Command managerd runs the Apiary management daemon: it connects to
// raftd over the internal Unix domain socket protocol and exposes
// managerd's own external RPC API (api/rpc) over TCP.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/manager"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("managerd: %v", err)
	}
}

func run() error {
	raftdSocket := flag.String("raftd-socket", "/var/run/apiary/raftd.sock", "path to raftd's internal Unix domain socket")
	rpcAddr := flag.String("rpc-addr", "127.0.0.1:17700", "TCP address for managerd's external RPC API")
	nodeID := flag.String("node-id", "", "identity reported by managerd in Status responses (defaults to hostname)")
	flag.Parse()

	id := *nodeID
	if id == "" {
		host, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("determining node-id: %w", err)
		}
		id = host
	}

	// There is no process-supervision/retry infrastructure yet, so a
	// managerd that can't reach raftd at all isn't in a useful state:
	// fail fast rather than retrying with backoff.
	raftClient, err := manager.Dial(*raftdSocket)
	if err != nil {
		return fmt.Errorf("connecting to raftd at %s: %w", *raftdSocket, err)
	}
	defer raftClient.Close()

	lis, err := net.Listen("tcp", *rpcAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", *rpcAddr, err)
	}

	grpcServer := grpc.NewServer()
	rpcpb.RegisterManagerServiceServer(grpcServer, manager.NewServer(raftClient, id))

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- grpcServer.Serve(lis)
	}()

	log.Printf("managerd: listening on %s (node-id=%s, raftd-socket=%s)", *rpcAddr, id, *raftdSocket)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
		log.Printf("managerd: shutting down")
	case err := <-serveErrCh:
		if err != nil {
			return fmt.Errorf("grpc server: %w", err)
		}
	}

	grpcServer.GracefulStop()
	return nil
}
