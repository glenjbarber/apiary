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
	"time"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/bhyve"
	"github.com/glenjbarber/apiary/internal/cluster"
	"github.com/glenjbarber/apiary/internal/manager"
	"github.com/glenjbarber/apiary/internal/zfs"
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
	zfsBase := flag.String("zfs-base", "zroot/apiary", "base ZFS dataset under which this node's VM storage is provisioned")
	reconcileInterval := flag.Duration("reconcile-interval", 30*time.Second, "how often to reconcile local VM storage against raftd's VM list")
	bhyvePrefix := flag.String("bhyve-prefix", "apiary-", "name prefix for bhyve VMs this node creates")
	bhyveBootROM := flag.String("bhyve-bootrom", "", "UEFI boot ROM path for bhyve VMs; leave empty to disable bhyve provisioning on this node (e.g. nodes without hardware-assisted virtualization)")
	diskSizeMB := flag.Uint64("disk-size-mb", 0, "size of each VM's boot disk image in MB (0 uses the reconciler's own default)")
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

	// The reconciler must key off raftd's own node ID (what VMDefinition
	// .node_id values actually reference), not managerd's separate
	// -node-id flag above - the two happen to default to the same
	// hostname, but are logically distinct identities.
	statusCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	raftStatus, err := raftClient.Status(statusCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("querying raftd status: %w", err)
	}
	raftNodeID := raftStatus.GetNodeId()

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

	reconciler := &cluster.Reconciler{
		Raft:        raftClient,
		ZFS:         zfs.New(*zfsBase),
		LocalNodeID: raftNodeID,
		BootROM:     *bhyveBootROM,
		DiskSizeMB:  *diskSizeMB,
	}
	// Bhyve is left nil when no boot ROM is configured, so nodes without
	// hardware-assisted virtualization (the common case today - see
	// ADR-0015) keep doing safe dataset-only reconciliation instead of
	// failing every tick trying to call bhyve(8).
	if *bhyveBootROM != "" {
		reconciler.Bhyve = bhyve.New(*bhyvePrefix)
	}
	go runReconcileLoop(ctx, reconciler, *reconcileInterval)

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

// runReconcileLoop calls reconciler.RunOnce immediately and then on every
// tick of interval, until ctx is done. Errors are logged, not fatal: a
// non-leader node failing to list VMs is an expected, routine condition,
// not a reason to bring managerd down.
func runReconcileLoop(ctx context.Context, reconciler *cluster.Reconciler, interval time.Duration) {
	reconcileOnce(ctx, reconciler)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileOnce(ctx, reconciler)
		}
	}
}

func reconcileOnce(ctx context.Context, reconciler *cluster.Reconciler) {
	if err := reconciler.RunOnce(ctx); err != nil {
		log.Printf("managerd: reconcile: %v", err)
	}
}
