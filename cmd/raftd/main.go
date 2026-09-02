// Command raftd runs the Apiary raft consensus agent as a standalone
// process, exposing the internal RaftInternal protocol over a Unix domain
// socket for managerd to consume.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

const socketPerm = 0o660

// resetConfirmPhrase is the exact value -reset must be given to actually
// wipe raft state (Tier 1, docs/adr/0038-tiered-reset-cli.md). A bare
// boolean flag would be too easy to leave sitting in rc.conf's
// apiary_raftd_args by accident - every service here runs under
// daemon(8)'s -r auto-restart supervisor, so an accidentally-persistent
// reset flag would wipe the cluster on every single respawn. Anything
// other than an exact match is rejected with no action taken.
const resetConfirmPhrase = "yes-wipe-raft-state"

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
	joinSocket := flag.String("join", "", "internal socket path of an existing cluster member to join through (leave empty to bootstrap a new single-node cluster)")
	internalToken := flag.String("internal-token", "", "shared secret required from every RaftInternal caller (managerd, or a peer raftd during -join); leave empty to rely on the socket's own file permissions alone, as before (see ADR-0023)")
	reset := flag.String("reset", "", fmt.Sprintf("Tier 1 reset (ADR-0038): wipe this node's own raft state and exit, rather than starting the server - real VMs/jails/disks are untouched, just orphaned from tracking until re-registered. Must be exactly %q or nothing happens; the next normal (no -reset) start bootstraps fresh automatically against the now-empty -data-dir", resetConfirmPhrase))
	flag.Parse()

	if *reset != "" {
		return resetDataDir(*reset, *dataDir)
	}

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
	// New resolves defaults (e.g. NodeID from hostname) that cfg above may
	// not reflect; read the resolved value back for logging/joining.
	resolvedNodeID := node.Status().NodeID

	switch {
	case hadState:
		log.Printf("raftd: resuming existing raft state")
	case *joinSocket != "":
		if err := joinCluster(*joinSocket, resolvedNodeID, *bindAddr, *internalToken); err != nil {
			return fmt.Errorf("joining cluster via %s: %w", *joinSocket, err)
		}
		log.Printf("raftd: joined existing cluster via %s", *joinSocket)
	default:
		if err := node.Bootstrap(); err != nil {
			return fmt.Errorf("bootstrapping single-node cluster: %w", err)
		}
		log.Printf("raftd: bootstrapped new single-node cluster")
	}

	lis, err := listenUnix(*socketPath)
	if err != nil {
		return fmt.Errorf("listening on socket: %w", err)
	}

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(raftnode.TokenUnaryInterceptor(*internalToken)),
		grpc.StreamInterceptor(raftnode.TokenStreamInterceptor(*internalToken)),
	)
	internalpb.RegisterRaftInternalServer(grpcServer, raftnode.NewServer(node))

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- grpcServer.Serve(lis)
	}()

	log.Printf("raftd: listening on %s (node-id=%s, raft-bind=%s, data-dir=%s)",
		*socketPath, resolvedNodeID, *bindAddr, *dataDir)

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

	// GracefulStop blocks until every open connection/RPC finishes on its
	// own - with no bound, a single stuck or unexpectedly long-lived
	// client (a stray connection, a slow peer) can hang shutdown
	// indefinitely, which is exactly what a real reboot hit: raftd
	// didn't respond to SIGTERM within any reasonable window, needing a
	// SIGKILL to actually stop. Fall back to a forceful Stop() rather
	// than wait forever - rc.d's own shutdown sequence already gives
	// every service a bounded window before escalating, but that
	// escalation shouldn't be the only thing standing between a normal
	// shutdown and a multi-minute hang.
	const gracefulStopTimeout = 10 * time.Second
	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(gracefulStopTimeout):
		log.Printf("raftd: GracefulStop did not complete within %s, forcing Stop", gracefulStopTimeout)
		grpcServer.Stop()
	}
	if err := node.Shutdown(); err != nil {
		log.Printf("raftd: error shutting down raft: %v", err)
	}
	_ = os.Remove(*socketPath)

	return nil
}

// resetDataDir implements Tier 1 (docs/adr/0038-tiered-reset-cli.md): a
// one-shot mode run instead of the normal server, moving dataDir aside
// to a timestamped backup path rather than deleting it outright - cheap
// insurance matching the manual practice this feature replaces, not a
// full undo system. phrase must exactly equal resetConfirmPhrase or
// nothing happens at all (not even a partial rename attempt).
func resetDataDir(phrase, dataDir string) error {
	if phrase != resetConfirmPhrase {
		return fmt.Errorf("-reset value %q does not match the required confirmation phrase %q - nothing was done", phrase, resetConfirmPhrase)
	}

	backup := fmt.Sprintf("%s.reset-backup-%d", dataDir, time.Now().Unix())
	if _, err := os.Stat(dataDir); err == nil {
		if err := os.Rename(dataDir, backup); err != nil {
			return fmt.Errorf("moving %s aside to %s: %w", dataDir, backup, err)
		}
		log.Printf("raftd: moved existing raft state from %s to %s", dataDir, backup)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking %s: %w", dataDir, err)
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("recreating empty %s: %w", dataDir, err)
	}

	log.Printf("raftd: reset complete - %s is now empty; the next normal start will bootstrap a fresh single-node cluster", dataDir)
	return nil
}

// joinCluster asks the existing cluster member listening on joinSocket to
// add this node (nodeID at raftBindAddr) as a voter. The target must
// already be reachable, and must be (or forward to) the current leader.
// token is presented as the target's own -internal-token, if it has one
// configured - a real multi-node deployment is expected to use the same
// token on every raftd, the same assumption -peer-api-key already makes
// for managerd's own cross-node calls (ADR-0029).
func joinCluster(joinSocket, nodeID, raftBindAddr, token string) error {
	conn, err := grpc.NewClient(
		"unix://"+joinSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(raftnode.TokenCredentials(token)),
	)
	if err != nil {
		return fmt.Errorf("dialing %s: %w", joinSocket, err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := internalpb.NewRaftInternalClient(conn)
	resp, err := client.AddVoter(ctx, &internalpb.AddVoterRequest{
		Id:      nodeID,
		Address: raftBindAddr,
	})
	if err != nil {
		return err
	}
	if resp.GetError() != "" {
		if resp.GetLeaderHint() != "" {
			return fmt.Errorf("%s (leader hint: %s)", resp.GetError(), resp.GetLeaderHint())
		}
		return errors.New(resp.GetError())
	}
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
