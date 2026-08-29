// Command raftd runs the Apiary raft consensus agent as a standalone
// process, exposing the internal RaftInternal protocol over a Unix domain
// socket for managerd to consume.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"google.golang.org/grpc"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

const socketPerm = 0o660

func main() {
	if err := run(); err != nil {
		log.Fatalf("raftd: %v", err)
	}
}

func run() error {
	dataDir := flag.String("data-dir", "/var/db/apiary/raftd", "directory for raft log/stable store and snapshots")
	socketPath := flag.String("socket", "/var/run/apiary/raftd.sock", "Unix domain socket path for the internal RaftInternal protocol")
	nodeID := flag.String("node-id", "", "unique ID for this raft node (defaults to hostname)")
	bindAddr := flag.String("raft-bind", raftnode.DefaultBindAddr, "loopback TCP address for the raft transport")
	flag.Parse()

	cfg := raftnode.Config{
		NodeID:   *nodeID,
		DataDir:  *dataDir,
		BindAddr: *bindAddr,
	}

	hadState, err := raftnode.HasExistingState(cfg)
	if err != nil {
		return fmt.Errorf("checking existing raft state: %w", err)
	}

	node, err := raftnode.New(cfg)
	if err != nil {
		return fmt.Errorf("creating raft node: %w", err)
	}

	if !hadState {
		if err := node.Bootstrap(); err != nil {
			return fmt.Errorf("bootstrapping single-node cluster: %w", err)
		}
		log.Printf("raftd: bootstrapped new single-node cluster")
	} else {
		log.Printf("raftd: resuming existing raft state")
	}

	lis, err := listenUnix(*socketPath)
	if err != nil {
		return fmt.Errorf("listening on socket: %w", err)
	}

	grpcServer := grpc.NewServer()
	internalpb.RegisterRaftInternalServer(grpcServer, raftnode.NewServer(node))

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- grpcServer.Serve(lis)
	}()

	log.Printf("raftd: listening on %s (node-id=%s, raft-bind=%s, data-dir=%s)",
		*socketPath, cfg.NodeID, *bindAddr, *dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
		log.Printf("raftd: shutting down")
	case err := <-serveErrCh:
		if err != nil {
			return fmt.Errorf("grpc server: %w", err)
		}
	}

	grpcServer.GracefulStop()
	if err := node.Shutdown(); err != nil {
		log.Printf("raftd: error shutting down raft: %v", err)
	}
	_ = os.Remove(*socketPath)

	return nil
}

// listenUnix removes any stale socket file left over from an unclean
// shutdown, ensures the parent directory exists, and listens on path.
func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating socket dir: %w", err)
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing stale socket: %w", err)
	}

	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}

	if err := os.Chmod(path, socketPerm); err != nil {
		lis.Close()
		return nil, fmt.Errorf("setting socket permissions: %w", err)
	}

	return lis, nil
}
