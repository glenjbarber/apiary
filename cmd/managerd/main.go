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
	"google.golang.org/grpc/credentials"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/bhyve"
	"github.com/glenjbarber/apiary/internal/cluster"
	"github.com/glenjbarber/apiary/internal/dhcpd"
	"github.com/glenjbarber/apiary/internal/hast"
	"github.com/glenjbarber/apiary/internal/isostore"
	"github.com/glenjbarber/apiary/internal/jail"
	"github.com/glenjbarber/apiary/internal/manager"
	"github.com/glenjbarber/apiary/internal/pf"
	"github.com/glenjbarber/apiary/internal/ufsmount"
	"github.com/glenjbarber/apiary/internal/vlan"
	"github.com/glenjbarber/apiary/internal/zfs"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("managerd: %v", err)
	}
}

func run() error {
	raftdSocket := flag.String("raftd-socket", "/var/run/apiary/raftd.sock", "path to raftd's internal Unix domain socket")
	raftdToken := flag.String("raftd-token", "", "shared secret to present to raftd's internal socket (must match raftd's own -internal-token); leave empty if raftd has none configured")
	rpcAddr := flag.String("rpc-addr", "127.0.0.1:17700", "TCP address for managerd's external RPC API")
	nodeID := flag.String("node-id", "", "identity reported by managerd in Status responses (defaults to hostname)")
	zfsBase := flag.String("zfs-base", "zroot/apiary", "base ZFS dataset under which this node's VM storage is provisioned")
	reconcileInterval := flag.Duration("reconcile-interval", 30*time.Second, "how often to reconcile local VM storage against raftd's VM list")
	bhyvePrefix := flag.String("bhyve-prefix", "apiary-", "name prefix for bhyve VMs this node creates")
	bhyveBootROM := flag.String("bhyve-bootrom", "", "UEFI boot ROM path for bhyve VMs; leave empty to disable bhyve provisioning on this node (e.g. nodes without hardware-assisted virtualization)")
	bhyveBridge := flag.String("bhyve-bridge", "", "existing bridge(4) interface to attach bhyve VMs' tap devices to; leave empty to disable VM networking on this node")
	diskSizeMB := flag.Uint64("disk-size-mb", 0, "size of each VM's boot disk image in MB (0 uses the reconciler's own default)")
	isoDir := flag.String("iso-dir", "/var/db/apiary/isos", "directory where uploaded installer images are stored on this node")
	vlanUplink := flag.String("vlan-uplink", "", "physical interface VLAN-tagged networks attach to (e.g. \"re0\", \"em0\" - differs per node); leave empty to disable network management (VLANs/DHCP/firewall) on this node")
	hastEnabled := flag.Bool("hast-enabled", false, "enable HAST-backed VM disk replication support on this node (requires a real, patched hastd - see ADR-0026); needed on both a replicated VM's owning node and its replica node, regardless of bhyve support")
	jailEnabled := flag.Bool("jail-enabled", false, "enable jail orchestration on this node (requires internal/jail's own prerequisites - see CLAUDE.md)")
	jailPrefix := flag.String("jail-prefix", "apiary-", "name prefix for jails this node creates")
	jailMountBase := flag.String("jail-mount-base", "/apiary-jails", "parent directory a replicated jail's HAST-backed root filesystem is mounted under (non-replicated jails use their ZFS dataset's own mountpoint instead)")
	jailDiskSizeMB := flag.Uint64("jail-disk-size-mb", 2048, "size of a replicated jail's HAST-backed root filesystem in MB (ignored for non-replicated jails, which use their ZFS dataset's own quota)")
	peerAPIKey := flag.String("peer-api-key", "", "API key this node's reconciler attaches when forwarding a raft write to another node's managerd (see ADR-0029); required once the cluster has any API key created (ADR-0023), since peer calls go through the same authenticated ManagerService API as everything else")
	peerManagerdPort := flag.String("peer-managerd-port", "", "port assumed for a peer node's managerd external API when forwarding (ADR-0029); defaults to this node's own -rpc-addr port, since every node in a real deployment is expected to use the same port")
	tlsCert := flag.String("tls-cert", "", "PEM certificate file for managerd's external gRPC API; leave unset (with -tls-key) to serve plaintext, as before")
	tlsKey := flag.String("tls-key", "", "PEM private key file matching -tls-cert")
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
	raftClient, err := manager.Dial(*raftdSocket, *raftdToken)
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

	isos := isostore.New(*isoDir)

	// Defaults to this node's own -rpc-addr port when unset - every
	// node in a real deployment is expected to run managerd's external
	// API on the same port (see ADR-0029), differing only by host.
	resolvedPeerPort := *peerManagerdPort
	if resolvedPeerPort == "" {
		if _, port, err := net.SplitHostPort(*rpcAddr); err == nil {
			resolvedPeerPort = port
		}
	}

	reconciler := &cluster.Reconciler{
		Raft:             raftClient,
		ZFS:              zfs.New(*zfsBase),
		LocalNodeID:      raftNodeID,
		BootROM:          *bhyveBootROM,
		DiskSizeMB:       *diskSizeMB,
		Bridge:           *bhyveBridge,
		ISOs:             isos,
		Peers:            manager.NewPeerReporter(*peerAPIKey),
		PeerManagerdPort: resolvedPeerPort,
	}
	// HAST is independent of bhyve support: a node holding only a HAST
	// secondary replica (see ADR-0026) never runs the VM at all, so this
	// is set regardless of -bhyve-bootrom, unlike VLAN/DHCP/PF below.
	if *hastEnabled {
		reconciler.HAST = hast.New()
	}
	// Jail orchestration is independent of both bhyve and HAST: a node
	// can run plain, non-replicated jails with neither. Mount is only
	// ever consulted for a replicated jail (see ensureJail), so it's
	// always set here regardless of -hast-enabled - a jail naming
	// ReplicaNodeID on a node without -hast-enabled still gets a clear
	// error from ensureJail itself, the same as a replicated VM does.
	if *jailEnabled {
		reconciler.Jail = jail.New(*jailPrefix)
		reconciler.Mount = ufsmount.New()
		reconciler.JailBase = *jailMountBase
		reconciler.JailDiskSizeMB = *jailDiskSizeMB
	}
	// reconciler.Bhyve/bhyveMgr are left nil when no boot ROM is
	// configured, so nodes without hardware-assisted virtualization (the
	// common case today - see ADR-0015) keep doing safe dataset-only
	// reconciliation instead of failing every tick trying to call
	// bhyve(8). Passing a literal nil into NewServer below (rather than a
	// nil *bhyve.Manager boxed into a non-nil vncLookup interface value)
	// matters here: a boxed nil pointer would panic the first time
	// GetVMConsole called a method on it.
	var bhyveMgr *bhyve.Manager
	var vlanMgr *vlan.Manager
	if *bhyveBootROM != "" {
		bhyveMgr = bhyve.New(*bhyvePrefix)
		reconciler.Bhyve = bhyveMgr

		// VLAN/DHCP/PF only make sense alongside real bhyve provisioning
		// (a dataset-only node has no VM NICs to attach anywhere), and
		// VLAN specifically needs a real uplink interface to tag onto -
		// leave all three nil (network management disabled on this
		// node) if either prerequisite is missing, the same opt-in
		// pattern as Bhyve/ISOs above.
		if *vlanUplink != "" {
			vlanMgr = &vlan.Manager{Uplink: *vlanUplink}
			reconciler.VLAN = vlanMgr
			reconciler.DHCP = &dhcpd.Manager{}
			reconciler.PF = &pf.Manager{}
		}
	}

	// Passing literal nils into NewServer below (rather than nil
	// *bhyve.Manager/*vlan.Manager values boxed into non-nil vncLookup/
	// vlanStatus interface values) matters here: a boxed nil pointer
	// would panic the first time GetVMConsole/ListNetworks called a
	// method on it.
	var vncArg manager.VNCLookup
	var serialLogArg manager.SerialLogLookup
	var vlanArg manager.VLANStatus
	if bhyveMgr != nil {
		vncArg = bhyveMgr
		serialLogArg = bhyveMgr
	}
	if vlanMgr != nil {
		vlanArg = vlanMgr
	}

	srv := manager.NewServer(raftClient, id, isos, vncArg, serialLogArg, vlanArg)
	// Every RPC (including UploadISO's stream) is gated by srv's own
	// API-key check - see ADR-0023. Auth stays fully open until the
	// first key is created (CreateAPIKey itself included), so this is
	// non-breaking for any deployment that hasn't created a key yet.
	serverOpts := []grpc.ServerOption{
		grpc.UnaryInterceptor(srv.AuthUnaryInterceptor),
		grpc.StreamInterceptor(srv.AuthStreamInterceptor),
	}
	// TLS is opt-in (both -tls-cert and -tls-key must be set) - without
	// it, an API key (ADR-0023) travels to managerd in plaintext over
	// the network, which matters the moment -rpc-addr is bound to
	// anything beyond loopback (see ADR-0029's own consequences). Left
	// unset, this preserves the plaintext behavior every deployment so
	// far has used.
	if *tlsCert != "" || *tlsKey != "" {
		if *tlsCert == "" || *tlsKey == "" {
			return fmt.Errorf("both -tls-cert and -tls-key must be set together")
		}
		creds, err := credentials.NewServerTLSFromFile(*tlsCert, *tlsKey)
		if err != nil {
			return fmt.Errorf("loading TLS cert/key: %w", err)
		}
		serverOpts = append(serverOpts, grpc.Creds(creds))
	}
	grpcServer := grpc.NewServer(serverOpts...)
	rpcpb.RegisterManagerServiceServer(grpcServer, srv)

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- grpcServer.Serve(lis)
	}()

	log.Printf("managerd: listening on %s (node-id=%s, raftd-socket=%s, vlan-uplink=%s, hast-enabled=%v, jail-enabled=%v, peer-managerd-port=%s, tls=%v)", *rpcAddr, id, *raftdSocket, *vlanUplink, *hastEnabled, *jailEnabled, resolvedPeerPort, *tlsCert != "")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
