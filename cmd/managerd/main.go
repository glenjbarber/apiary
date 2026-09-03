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
	"os/exec"
	"os/signal"
	"strings"
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
	"github.com/glenjbarber/apiary/internal/resetutil"
	"github.com/glenjbarber/apiary/internal/ufsmount"
	"github.com/glenjbarber/apiary/internal/vlan"
	"github.com/glenjbarber/apiary/internal/zfs"
)

// Confirmation phrases for the one-shot reset modes (ADR-0038). Bare
// boolean flags would be too easy to leave sitting in rc.conf's
// apiary_managerd_args by accident - every service here runs under
// daemon(8)'s -r auto-restart supervisor, so an accidentally-persistent
// reset flag would wipe resources on every single respawn.
const (
	resetManagedConfirmPhrase = "yes-wipe-managed-resources"
	factoryResetConfirmPhrase = "yes-nuke-everything"
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
	dhcpDNSServer := flag.String("dhcp-dns-server", "", "DNS server address handed to DHCP clients on this node's Apiary-managed networks (dnsmasq's own port=0 disables its resolver, so without this every VM gets a dead-end DNS server - see internal/dhcpd.NetworkScope.DNSServer); leave empty only if no VM on a managed network needs working DNS resolution")
	hastEnabled := flag.Bool("hast-enabled", false, "enable HAST-backed VM disk replication support on this node (requires a real, patched hastd - see ADR-0026); needed on both a replicated VM's owning node and its replica node, regardless of bhyve support")
	jailEnabled := flag.Bool("jail-enabled", false, "enable jail orchestration on this node (requires internal/jail's own prerequisites - see CLAUDE.md)")
	jailPrefix := flag.String("jail-prefix", "apiary-", "name prefix for jails this node creates")
	jailMountBase := flag.String("jail-mount-base", "/apiary-jails", "parent directory a replicated jail's HAST-backed root filesystem is mounted under (non-replicated jails use their ZFS dataset's own mountpoint instead)")
	jailDiskSizeMB := flag.Uint64("jail-disk-size-mb", 2048, "size of a replicated jail's HAST-backed root filesystem in MB (ignored for non-replicated jails, which use their ZFS dataset's own quota)")
	peerAPIKey := flag.String("peer-api-key", "", "API key this node's reconciler attaches when forwarding a raft write to another node's managerd (see ADR-0029); required once the cluster has any API key created (ADR-0023), since peer calls go through the same authenticated ManagerService API as everything else")
	peerManagerdPort := flag.String("peer-managerd-port", "", "port assumed for a peer node's managerd external API when forwarding (ADR-0029); defaults to this node's own -rpc-addr port, since every node in a real deployment is expected to use the same port")
	peerTLS := flag.Bool("peer-tls", false, "dial peer managerds over TLS instead of plaintext when forwarding (ADR-0029/ADR-0035); requires every peer's managerd to also be TLS-enabled")
	peerTLSHostnameMap := flag.String("peer-tls-hostname-map", "", "comma-separated ip=hostname pairs used to verify a peer's TLS certificate, since a raft leader_hint is always a bare address and a real cert is never issued for a bare IP (e.g. \"10.50.0.11=freebsd-apiary.apiary.work,10.50.0.12=freebsd-apiary2.apiary.work\"); only consulted when -peer-tls is set")
	tlsCert := flag.String("tls-cert", "", "PEM certificate file for managerd's external gRPC API; leave unset (with -tls-key) to serve plaintext, as before")
	tlsKey := flag.String("tls-key", "", "PEM private key file matching -tls-cert")
	resetManaged := flag.String("reset-managed", "", fmt.Sprintf("Tier 2 reset (ADR-0038): destroy every real VM/jail/dataset/ISO this node's own -zfs-base/-jail-prefix/-bhyve-prefix/-iso-dir manage, then exit, rather than starting the server. Never touches anything outside that scope - safe to run without double-checking. Must be exactly %q or nothing happens", resetManagedConfirmPhrase))
	factoryReset := flag.String("factory-reset", "", fmt.Sprintf("Tier 3 reset (ADR-0038): runs the same destruction as -reset-managed, then also destroys anything named in -factory-reset-extra-jails/-factory-reset-extra-datasets regardless of scope, then exits. Must be exactly %q or nothing happens", factoryResetConfirmPhrase))
	factoryResetExtraJails := flag.String("factory-reset-extra-jails", "", "comma-separated jail names to destroy for real during -factory-reset, outside the normal -jail-prefix scope (e.g. a jail you want gone that Apiary itself didn't create) - nothing here is ever auto-discovered, only what's named")
	factoryResetExtraDatasets := flag.String("factory-reset-extra-datasets", "", "comma-separated ZFS dataset/pool names to destroy recursively during -factory-reset, outside the normal -zfs-base scope - nothing here is ever auto-discovered, only what's named")
	flag.Parse()

	if *resetManaged != "" || *factoryReset != "" {
		return runReset(*resetManaged, *factoryReset, *factoryResetExtraJails, *factoryResetExtraDatasets, *zfsBase, *jailPrefix, *bhyvePrefix, *isoDir)
	}

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

	// Parsed once here (rather than inside PeerReporter) so a malformed
	// entry is a plain, silently-skipped no-op instead of a runtime
	// panic - an operator typo shouldn't crash the whole daemon.
	peerHostnames := map[string]string{}
	for _, pair := range strings.Split(*peerTLSHostnameMap, ",") {
		if ip, host, ok := strings.Cut(pair, "="); ok && ip != "" && host != "" {
			peerHostnames[ip] = host
		}
	}

	// Shared between the reconciler's own write-forwarding (ADR-0029)
	// and the server's read-forwarding (ADR-0035) - both are forwarding
	// to the same leader managerd over the same authenticated API, so
	// there's no reason for two separate peer clients/credentials.
	peers := manager.NewPeerReporter(*peerAPIKey, *peerTLS, peerHostnames)

	reconciler := &cluster.Reconciler{
		Raft:             raftClient,
		ZFS:              zfs.New(*zfsBase),
		LocalNodeID:      raftNodeID,
		BootROM:          *bhyveBootROM,
		DiskSizeMB:       *diskSizeMB,
		Bridge:           *bhyveBridge,
		ISOs:             isos,
		Peers:            peers,
		PeerManagerdPort: resolvedPeerPort,
		DNSServer:        *dhcpDNSServer,
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

	srv := manager.NewServer(raftClient, id, isos, vncArg, serialLogArg, vlanArg, peers, resolvedPeerPort)
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

// isoManagerAdapter satisfies resetutil.ISOManager against a real
// *isostore.Manager, whose List() returns []isostore.Info rather than
// []resetutil.ISOInfo - resetutil deliberately doesn't import isostore
// just for this one struct shape (see its own doc comment).
type isoManagerAdapter struct{ m *isostore.Manager }

func (a isoManagerAdapter) List() ([]resetutil.ISOInfo, error) {
	infos, err := a.m.List()
	if err != nil {
		return nil, err
	}
	out := make([]resetutil.ISOInfo, len(infos))
	for i, info := range infos {
		out[i] = resetutil.ISOInfo{Name: info.Name}
	}
	return out, nil
}

func (a isoManagerAdapter) Delete(name string) error { return a.m.Delete(name) }

// runReset implements managerd's one-shot -reset-managed/-factory-reset
// modes (ADR-0038, Tiers 2 and 3) - run instead of the normal server,
// with no raftd/raft dependency at all, since the whole point is
// cleaning up real resources whether or not raft still tracks them.
// resetManaged and factoryReset are each checked against their own
// confirmation phrase independently; either (or both) may be set, but a
// mismatched non-empty value is always a hard error with nothing done,
// never a silent no-op.
func runReset(resetManaged, factoryReset, extraJails, extraDatasets, zfsBase, jailPrefix, bhyvePrefix, isoDir string) error {
	doManaged := false
	if resetManaged != "" {
		if resetManaged != resetManagedConfirmPhrase {
			return fmt.Errorf("-reset-managed value %q does not match the required confirmation phrase %q - nothing was done", resetManaged, resetManagedConfirmPhrase)
		}
		doManaged = true
	}
	doFactory := false
	if factoryReset != "" {
		if factoryReset != factoryResetConfirmPhrase {
			return fmt.Errorf("-factory-reset value %q does not match the required confirmation phrase %q - nothing was done", factoryReset, factoryResetConfirmPhrase)
		}
		doFactory = true
		doManaged = true // Tier 3 always includes Tier 2's own destruction first.
	}
	if !doManaged {
		return nil
	}

	ctx := context.Background()
	res := resetutil.ManagedResources(ctx, jail.New(jailPrefix), bhyve.New(bhyvePrefix), zfs.New(zfsBase), isoManagerAdapter{isostore.New(isoDir)})
	log.Printf("managerd: reset-managed: removed %d jail(s) %v, destroyed %d VM(s) %v, destroyed %d dataset(s) %v, deleted %d ISO(s) %v",
		len(res.JailsRemoved), res.JailsRemoved, len(res.VMsDestroyed), res.VMsDestroyed, len(res.DatasetsDestroyed), res.DatasetsDestroyed, len(res.ISOsDeleted), res.ISOsDeleted)
	for _, err := range res.Errors {
		log.Printf("managerd: reset-managed: %v", err)
	}

	if doFactory {
		for _, name := range splitCommaList(extraJails) {
			log.Printf("managerd: factory-reset: removing extra jail %q", name)
			if out, err := exec.CommandContext(ctx, "jail", "-r", name).CombinedOutput(); err != nil {
				log.Printf("managerd: factory-reset: removing jail %q: %v: %s", name, err, out)
			}
		}
		for _, name := range splitCommaList(extraDatasets) {
			log.Printf("managerd: factory-reset: destroying extra dataset/pool %q", name)
			if out, err := exec.CommandContext(ctx, "zfs", "destroy", "-r", name).CombinedOutput(); err != nil {
				log.Printf("managerd: factory-reset: destroying dataset %q: %v: %s", name, err, out)
			}
		}
	}

	if len(res.Errors) > 0 {
		return fmt.Errorf("reset completed with %d error(s), see log above", len(res.Errors))
	}
	log.Printf("managerd: reset complete")
	return nil
}

// splitCommaList splits a comma-separated flag value into non-empty
// trimmed entries, returning nil for an empty input.
func splitCommaList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
