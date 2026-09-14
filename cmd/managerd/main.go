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
	"github.com/glenjbarber/apiary/internal/assumecheck"
	"github.com/glenjbarber/apiary/internal/assumptionregister"
	"github.com/glenjbarber/apiary/internal/assumptions"
	"github.com/glenjbarber/apiary/internal/bhyve"
	"github.com/glenjbarber/apiary/internal/cloudflare"
	"github.com/glenjbarber/apiary/internal/cluster"
	"github.com/glenjbarber/apiary/internal/deadman"
	"github.com/glenjbarber/apiary/internal/dhcpd"
	"github.com/glenjbarber/apiary/internal/hast"
	"github.com/glenjbarber/apiary/internal/hostconfig"
	"github.com/glenjbarber/apiary/internal/isostore"
	"github.com/glenjbarber/apiary/internal/jail"
	"github.com/glenjbarber/apiary/internal/jailarchive"
	"github.com/glenjbarber/apiary/internal/manager"
	"github.com/glenjbarber/apiary/internal/netroute"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/origincert"
	"github.com/glenjbarber/apiary/internal/pam"
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
	// Only these five one-shot destructive/export modes remain CLI
	// flags (ADR-0100) - see this file's own resetManagedConfirmPhrase/
	// factoryResetConfirmPhrase doc comment for why: a value persisted
	// in a config file would act on every daemon(8) -r respawn, not
	// just once.
	resetManaged := flag.String("reset-managed", "", fmt.Sprintf("Tier 2 reset (ADR-0038): destroy every real VM/jail/dataset/ISO this node's own zfs_base/jail_prefix/bhyve_prefix/iso_dir config manage, then exit, rather than starting the server. Never touches anything outside that scope - safe to run without double-checking. Must be exactly %q or nothing happens", resetManagedConfirmPhrase))
	factoryReset := flag.String("factory-reset", "", fmt.Sprintf("Tier 3 reset (ADR-0038): runs the same destruction as -reset-managed, then also destroys anything named in -factory-reset-extra-jails/-factory-reset-extra-datasets regardless of scope, then exits. Must be exactly %q or nothing happens", factoryResetConfirmPhrase))
	factoryResetExtraJails := flag.String("factory-reset-extra-jails", "", "comma-separated jail names to destroy for real during -factory-reset, outside the normal jail_prefix scope (e.g. a jail you want gone that Apiary itself didn't create) - nothing here is ever auto-discovered, only what's named")
	factoryResetExtraDatasets := flag.String("factory-reset-extra-datasets", "", "comma-separated ZFS dataset/pool names to destroy recursively during -factory-reset, outside the normal zfs_base scope - nothing here is ever auto-discovered, only what's named")
	exportHostConfig := flag.String("export-host-config", "", "read-only: write a redacted snapshot of this Comb's /etc/rc.conf, /etc/pf.conf, and /etc/master.passwd into this directory, then exit, rather than starting the server - see internal/hostconfig's own doc comment for exactly what's redacted and why there is no matching restore/apply flag")
	flag.Parse()

	if *exportHostConfig != "" {
		return runExportHostConfig(hostconfig.Files{}, *exportHostConfig)
	}

	// -reset-managed/-factory-reset are, like raftd's own -reset/
	// -restore, "break glass" recovery tools - they must keep working
	// even if managerd.json itself is malformed, since a broken config
	// is exactly the situation an operator most needs them for. A
	// Load() error here falls back to applyManagerdDefaults's zero-cfg
	// baseline with a warning, rather than aborting; normal server
	// startup below is stricter.
	oneShotReset := *resetManaged != "" || *factoryReset != ""
	nodeConfigMgr := &nodeconfig.Manager{}
	cfg, err := nodeConfigMgr.Load()
	if err != nil {
		if !oneShotReset {
			return fmt.Errorf("reading %s: %w", nodeconfig.DefaultPath, err)
		}
		log.Printf("managerd: reading %s: %v - falling back to defaults for this one-shot command", nodeconfig.DefaultPath, err)
		cfg = nodeconfig.Config{}
	}
	applyManagerdDefaults(&cfg)

	if *resetManaged != "" || *factoryReset != "" {
		return runReset(*resetManaged, *factoryReset, *factoryResetExtraJails, *factoryResetExtraDatasets, cfg.ZFSBase, cfg.JailPrefix, cfg.BhyvePrefix, cfg.ISODir)
	}

	id := cfg.NodeID
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
	raftClient, err := manager.Dial(cfg.RaftdSocket, cfg.RaftdToken)
	if err != nil {
		return fmt.Errorf("connecting to raftd at %s: %w", cfg.RaftdSocket, err)
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

	lis, err := net.Listen("tcp", cfg.RPCAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.RPCAddr, err)
	}

	isos := isostore.New(cfg.ISODir)

	// Defaults to this node's own rpc_addr port when unset - every
	// node in a real deployment is expected to run managerd's external
	// API on the same port (see ADR-0029), differing only by host.
	resolvedPeerPort := cfg.PeerManagerdPort
	if resolvedPeerPort == "" {
		if _, port, err := net.SplitHostPort(cfg.RPCAddr); err == nil {
			resolvedPeerPort = port
		}
	}

	// Parsed once here (rather than inside PeerReporter) so a malformed
	// entry is a plain, silently-skipped no-op instead of a runtime
	// panic - an operator typo shouldn't crash the whole daemon.
	peerHostnames := map[string]string{}
	for _, pair := range strings.Split(cfg.PeerTLSHostnameMap, ",") {
		if ip, host, ok := strings.Cut(pair, "="); ok && ip != "" && host != "" {
			peerHostnames[ip] = host
		}
	}

	peerTLS := cfg.PeerTLS != nil && *cfg.PeerTLS

	// Shared between the reconciler's own write-forwarding (ADR-0029)
	// and the server's read-forwarding (ADR-0035) - both are forwarding
	// to the same leader managerd over the same authenticated API, so
	// there's no reason for two separate peer clients/credentials.
	peers := manager.NewPeerReporter(cfg.PeerAPIKey, peerTLS, peerHostnames)
	if cfg.PeerTLSCA != "" {
		pool, err := manager.LoadPeerCAPool(cfg.PeerTLSCA)
		if err != nil {
			return fmt.Errorf("managerd: %w", err)
		}
		peers.CAPool = pool
	}

	zfsMgr := zfs.New(cfg.ZFSBase)
	reconciler := &cluster.Reconciler{
		Raft:             raftClient,
		ZFS:              zfsMgr,
		LocalNodeID:      raftNodeID,
		BootROM:          cfg.BhyveBootROM,
		DiskSizeMB:       cfg.DiskSizeMB,
		Bridge:           cfg.BhyveBridge,
		ISOs:             isos,
		JailArchives:     jailarchive.New(),
		Peers:            peers,
		PeerManagerdPort: resolvedPeerPort,
		DNSServer:        cfg.DNSServer,
		NetworkStatePath: cluster.DefaultNetworkStatePath,
		Interval:         cfg.ReconcileInterval,
	}
	// HAST is independent of bhyve support: a node holding only a HAST
	// secondary replica (see ADR-0026) never runs the VM at all, so this
	// is set regardless of bhyve_bootrom, unlike VLAN/DHCP/PF below.
	if cfg.HASTEnabled != nil && *cfg.HASTEnabled {
		reconciler.HAST = hast.New()
	}
	// Keep lifecycle inspection/teardown available even when provisioning
	// is disabled, otherwise an explicit DeleteJail tombstone is stranded.
	// Nothing is created while JailProvisioningDisabled is true. Kept as
	// its own concrete-typed variable (not read back out of
	// reconciler.Jail, whose interface type is intentionally narrower)
	// so it can also be wired into the jail console (below) - jexec
	// console access to an already-running jail is independent of
	// jail_enabled, the same "inspection/teardown always available"
	// posture ADR-0064 already established for lifecycle operations.
	jailEnabled := cfg.JailEnabled != nil && *cfg.JailEnabled
	jailMgr := jail.New(cfg.JailPrefix)
	reconciler.Jail = jailMgr
	reconciler.JailProvisioningDisabled = !jailEnabled
	reconciler.Mount = ufsmount.New()
	reconciler.JailBase = cfg.JailMountBase
	reconciler.JailDiskSizeMB = cfg.JailDiskSizeMB
	// Cloudflare Tunnel exposure (ADR-0063) is independent of bhyve/
	// HAST/jail support - it only ever proxies to a VM's own managed-
	// network address, using a Tunnel the operator pre-provisioned by
	// hand (cloudflared tunnel create). Empty cloudflare_token_file
	// disables it entirely, the same opt-in pattern as hast_enabled/
	// jail_enabled above; reconciler.Cloudflare stays nil in that case,
	// so reconcileCloudflareTunnel still cleans up any leftover process
	// from before the feature was disabled (see the Reconciler field's
	// own doc comment).
	if cfg.CloudflareTokenFile != "" {
		if cfg.CloudflareZoneID == "" || cfg.CloudflareTunnelID == "" || cfg.CloudflareTunnelCredentialsFile == "" {
			return fmt.Errorf("cloudflare_token_file requires cloudflare_zone_id, cloudflare_tunnel_id, and cloudflare_tunnel_credentials_file to also be set")
		}
		tokenBytes, err := os.ReadFile(cfg.CloudflareTokenFile)
		if err != nil {
			return fmt.Errorf("reading cloudflare_token_file: %w", err)
		}
		reconciler.Cloudflare = &cloudflare.Manager{}
		reconciler.CloudflareToken = strings.TrimSpace(string(tokenBytes))
		reconciler.CloudflareZoneID = cfg.CloudflareZoneID
		reconciler.CloudflareTunnelID = cfg.CloudflareTunnelID
		reconciler.CloudflareTunnelCredentialsFile = cfg.CloudflareTunnelCredentialsFile
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
	if cfg.BhyveBootROM != "" {
		bhyveMgr = bhyve.New(cfg.BhyvePrefix)
		reconciler.Bhyve = bhyveMgr
	}

	// VLAN/DHCP/PF used to be nested inside the bhyve-enabled branch
	// above, on the reasoning that "a dataset-only node has no VM NICs
	// to attach anywhere." That doesn't hold: jails use ip4=inherit, so
	// they never needed VLAN/DHCP either, and this coupling had a real,
	// previously undiscovered side effect - it silently left
	// reconciler.Uplink unset (so assumecheck.Checker's uplink-mismatch
	// health check went inert) on any node with bhyve disabled,
	// regardless of jails. VLAN specifically still needs a real uplink
	// interface to tag onto, so this is keyed only on uplink now - the
	// same opt-in pattern as Bhyve/ISOs above, just independent of it.
	// See ADR-0085.
	var vlanMgr *vlan.Manager
	if cfg.Uplink != "" {
		vlanMgr = &vlan.Manager{Uplink: cfg.Uplink}
		reconciler.VLAN = vlanMgr
		reconciler.DHCP = &dhcpd.Manager{}
		reconciler.PF = &pf.Manager{}
		reconciler.Uplink = cfg.Uplink
		if cfg.NATUplink != "" {
			reconciler.Uplink = cfg.NATUplink
		}
	}

	// uplink_bridged NetworkDefinition support (ADR-0101) is gated by an
	// explicit confirmation phrase, not a plain boolean - see
	// nodeconfig.Config.AllowUplinkBridging's own doc comment for why. A
	// node that never opts in pays zero cost here: reconciler.DeadMan
	// stays nil, so ensureVM never shells out to at(8)/atq(1)/atrm(1).
	const uplinkBridgingConfirmPhrase = "yes-share-uplink-bridge"
	reconciler.UplinkBridgingEnabled = cfg.AllowUplinkBridging == uplinkBridgingConfirmPhrase
	if reconciler.UplinkBridgingEnabled {
		reconciler.DeadMan = &deadman.Manager{StateDir: "/var/db/apiary/deadman"}
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

	// Automated Assumption Checks (ADR-0055) - validated up front rather
	// than left to misbehave silently at runtime on a nonsensical
	// combination.
	if cfg.AssumptionCheckInterval <= 0 {
		return fmt.Errorf("assumption_check_interval must be positive")
	}
	if cfg.AssumptionHeartbeatInterval < cfg.AssumptionCheckInterval {
		return fmt.Errorf("assumption_heartbeat_interval (%s) must be >= assumption_check_interval (%s)", cfg.AssumptionHeartbeatInterval, cfg.AssumptionCheckInterval)
	}
	assumptionStaleAfter := cfg.AssumptionStaleAfter
	if assumptionStaleAfter <= 0 {
		assumptionStaleAfter = 3 * cfg.AssumptionCheckInterval
	}
	if assumptionStaleAfter < 2*cfg.AssumptionCheckInterval {
		return fmt.Errorf("assumption_stale_after (%s) must be at least 2x assumption_check_interval (%s)", assumptionStaleAfter, cfg.AssumptionCheckInterval)
	}
	if cfg.AssumptionRunDeadline >= cfg.AssumptionCheckInterval {
		return fmt.Errorf("assumption_run_deadline (%s) must be less than assumption_check_interval (%s)", cfg.AssumptionRunDeadline, cfg.AssumptionCheckInterval)
	}
	if cfg.AssumptionHistoryLimit <= 0 {
		return fmt.Errorf("assumption_history_limit must be positive")
	}
	if cfg.AssumptionHistoryMaxAge <= 0 {
		return fmt.Errorf("assumption_history_max_age must be positive")
	}

	assumptionsMgr := &assumptions.Manager{}
	registerMgr := &assumptionregister.Manager{}
	// Constructed after reconciler above so reconciler.Uplink already
	// reflects both the nat-uplink-falls-back-to-uplink resolution and
	// the loaded config - never re-derived independently here, which
	// would risk a second, driftable copy of that same logic (see
	// ADR-0055).
	assumptionChecker := &assumecheck.Checker{
		NodeID:                raftNodeID,
		Raft:                  raftClient,
		Peers:                 peers,
		PeerManagerdAddr:      peerManagerdAddrFunc(resolvedPeerPort),
		Route:                 netrouteChecker{},
		Uplink:                reconciler.Uplink,
		PeerTLSConfigured:     peerTLS,
		PeerAuthKeyConfigured: cfg.PeerAPIKey != "",
		Store:                 assumptionsMgr,
		HeartbeatInterval:     cfg.AssumptionHeartbeatInterval,
		RunDeadline:           cfg.AssumptionRunDeadline,
		HistoryLimit:          cfg.AssumptionHistoryLimit,
		HistoryMaxAge:         cfg.AssumptionHistoryMaxAge,
	}

	// pam_service requires TLS (ADR-0087): a login password now
	// necessarily travels over this RPC channel from frontend, over the
	// real network (rpc_addr is not loopback-only, per ADR-0022's own
	// bridge-migration history) - unlike before, when PAM ran in-process
	// inside frontend and a password never crossed the wire at all.
	// Refused outright, the same "cheap to prevent, so don't just
	// document it" posture as the tls_cert/tls_key pairing check below.
	// UpdateNodeConfig enforces this too (see internal/manager/server.go)
	// so a bad combination set through the web UI is rejected immediately,
	// not just on the next restart.
	if cfg.PAMService != "" && (cfg.TLSCert == "" || cfg.TLSKey == "") {
		return fmt.Errorf("pam_service requires tls_cert/tls_key to also be set - a login password must not travel to this RPC over a plaintext channel")
	}

	srv := manager.NewServer(raftClient, id, isos, vncArg, serialLogArg, vlanArg, peers, resolvedPeerPort, zfsMgr, nodeConfigMgr, assumptionsMgr, assumptionStaleAfter, reconciler)
	srv.SetAssumptionRegister(registerMgr)
	srv.SetOriginCAIssuer(cloudflare.OriginCAIssuer{})
	// ADR-0088: reconciler already satisfies natPauser (NATUplink/
	// PauseOutboundNAT) structurally - wired unconditionally, since
	// both methods are themselves no-ops when PF/NetworkStatePath
	// aren't configured, the same posture as reconciler's other
	// nil-tolerant optional-dependency methods.
	srv.SetNATPauser(reconciler)
	srv.SetKnownPeerAddresses(splitCommaList(cfg.KnownPeerAddresses))
	if cfg.PAMService != "" {
		srv.SetPAMAuthenticator(pam.PAMAuthenticator{ServiceName: cfg.PAMService})
	}
	originCARenewer := &origincert.Renewer{
		Config: func() (string, string, error) {
			cfg, err := nodeConfigMgr.Load()
			if err != nil {
				return "", "", err
			}
			return cfg.OriginCADirectory, cfg.OriginCATokenFile, nil
		},
		Issuer: cloudflare.OriginCAIssuer{},
		RestartService: func(service string) {
			if _, err := srv.RestartNodeService(context.Background(), &rpcpb.RestartNodeServiceRequest{Name: service}); err != nil {
				log.Printf("managerd: origin-ca renewal: restarting %s: %v", service, err)
			}
		},
	}
	// Every RPC (including UploadISO's stream) is gated by srv's own
	// API-key check - see ADR-0023. Auth stays fully open until the
	// first key is created (CreateAPIKey itself included), so this is
	// non-breaking for any deployment that hasn't created a key yet.
	serverOpts := []grpc.ServerOption{
		grpc.UnaryInterceptor(srv.AuthUnaryInterceptor),
		grpc.StreamInterceptor(srv.AuthStreamInterceptor),
	}
	// TLS is opt-in (both tls_cert and tls_key must be set) - without
	// it, an API key (ADR-0023) travels to managerd in plaintext over
	// the network, which matters the moment rpc_addr is bound to
	// anything beyond loopback (see ADR-0029's own consequences). Left
	// unset, this preserves the plaintext behavior every deployment so
	// far has used.
	if cfg.TLSCert != "" || cfg.TLSKey != "" {
		if cfg.TLSCert == "" || cfg.TLSKey == "" {
			return fmt.Errorf("both tls_cert and tls_key must be set together")
		}
		creds, err := credentials.NewServerTLSFromFile(cfg.TLSCert, cfg.TLSKey)
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

	log.Printf("managerd: listening on %s (node-id=%s, raftd-socket=%s, vlan-uplink=%s, hast-enabled=%v, jail-enabled=%v, peer-managerd-port=%s, tls=%v, cloudflare-enabled=%v, pam-service=%s)", cfg.RPCAddr, id, cfg.RaftdSocket, cfg.Uplink, cfg.HASTEnabled != nil && *cfg.HASTEnabled, jailEnabled, resolvedPeerPort, cfg.TLSCert != "", cfg.CloudflareTokenFile != "", cfg.PAMService)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go runReconcileLoop(ctx, reconciler, cfg.ReconcileInterval)
	go runAssumptionCheckLoop(ctx, assumptionChecker, cfg.AssumptionCheckInterval)
	go runOriginCARenewalLoop(ctx, originCARenewer, cfg.OriginCARenewalCheckInterval)

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

// runAssumptionCheckLoop mirrors runReconcileLoop's own shape exactly -
// an immediate first run, then one per tick of interval, until ctx is
// done. Errors are logged, not fatal, matching reconcileOnce's own
// posture.
func runAssumptionCheckLoop(ctx context.Context, checker *assumecheck.Checker, interval time.Duration) {
	assumptionCheckOnce(ctx, checker)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			assumptionCheckOnce(ctx, checker)
		}
	}
}

func assumptionCheckOnce(ctx context.Context, checker *assumecheck.Checker) {
	if err := checker.RunOnce(ctx); err != nil {
		log.Printf("managerd: assumption check: %v", err)
	}
}

// runOriginCARenewalLoop mirrors runReconcileLoop's own shape exactly -
// an immediate first run, then one per tick of interval, until ctx is
// done. Errors are logged, not fatal: a renewal failure (an unreadable
// token file, a Cloudflare API error) is retried on the next tick, not a
// reason to bring managerd down.
func runOriginCARenewalLoop(ctx context.Context, renewer *origincert.Renewer, interval time.Duration) {
	originCARenewalOnce(ctx, renewer)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			originCARenewalOnce(ctx, renewer)
		}
	}
}

func originCARenewalOnce(ctx context.Context, renewer *origincert.Renewer) {
	if err := renewer.RunOnce(ctx); err != nil {
		log.Printf("managerd: origin-ca renewal: %v", err)
	}
}

// peerManagerdAddrFunc mirrors internal/manager.Server's own unexported
// peerManagerdAddr method (and internal/cluster's resolvePeerManagerdAddr)
// exactly - duplicated across this package boundary for the same reason
// defaultPeerManagerdPort already is in both of those.
func peerManagerdAddrFunc(port string) func(string) string {
	return func(raftAddress string) string {
		host, _, err := net.SplitHostPort(raftAddress)
		if err != nil {
			host = raftAddress
		}
		return net.JoinHostPort(host, port)
	}
}

// netrouteChecker adapts the free function internal/netroute.DefaultRouteInterface
// to the single-method interface internal/assumecheck.Checker.Route expects.
type netrouteChecker struct{}

func (netrouteChecker) DefaultRouteInterface(ctx context.Context) (string, bool, error) {
	return netroute.DefaultRouteInterface(ctx)
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

// runExportHostConfig implements managerd's one-shot -export-host-config
// mode - read-only, no raftd/raft dependency, no confirmation phrase
// needed (unlike -reset-managed/-factory-reset below, nothing here is
// destructive). See internal/hostconfig's own package doc comment for
// the full design, including why this is deliberately export-only.
// files takes explicit source paths so this is testable against
// fixture files rather than a real host's own /etc/rc.conf et al. -
// main() itself always passes hostconfig.Files{}, letting the package
// fall back to the real default locations.
func runExportHostConfig(files hostconfig.Files, outDir string) error {
	if err := hostconfig.Export(files, outDir); err != nil {
		return err
	}
	log.Printf("managerd: export-host-config: wrote a redacted rc.conf/pf.conf/master.passwd snapshot to %s", outDir)
	return nil
}

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
			if jail.IsProtected(name) {
				log.Printf("managerd: factory-reset: leaving protected jail %q untouched", name)
				continue
			}
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

// applyManagerdDefaults fills any zero-value field in cfg with the
// same default every corresponding flag used to have (ADR-0100), so a
// missing or partial managerd.json behaves identically to how an
// unset flag used to. Fields left at "" (e.g. BhyveBootROM, Uplink,
// TLSCert, PAMService, ...) are unchanged - their own former flag
// default was already empty, meaning "disabled"/"use system default."
func applyManagerdDefaults(cfg *nodeconfig.Config) {
	if cfg.RPCAddr == "" {
		cfg.RPCAddr = "127.0.0.1:17700"
	}
	if cfg.RaftdSocket == "" {
		cfg.RaftdSocket = "/var/run/apiary/raftd.sock"
	}
	if cfg.ZFSBase == "" {
		cfg.ZFSBase = "zroot/apiary"
	}
	if cfg.ReconcileInterval == 0 {
		cfg.ReconcileInterval = 30 * time.Second
	}
	if cfg.BhyvePrefix == "" {
		cfg.BhyvePrefix = "apiary-"
	}
	if cfg.ISODir == "" {
		cfg.ISODir = "/var/db/apiary/isos"
	}
	if cfg.JailPrefix == "" {
		cfg.JailPrefix = "apiary-"
	}
	if cfg.JailMountBase == "" {
		cfg.JailMountBase = "/apiary-jails"
	}
	if cfg.JailDiskSizeMB == 0 {
		cfg.JailDiskSizeMB = 2048
	}
	if cfg.AssumptionCheckInterval == 0 {
		cfg.AssumptionCheckInterval = 60 * time.Second
	}
	if cfg.AssumptionHeartbeatInterval == 0 {
		cfg.AssumptionHeartbeatInterval = time.Hour
	}
	if cfg.AssumptionRunDeadline == 0 {
		cfg.AssumptionRunDeadline = 20 * time.Second
	}
	if cfg.AssumptionHistoryLimit == 0 {
		cfg.AssumptionHistoryLimit = 200
	}
	if cfg.AssumptionHistoryMaxAge == 0 {
		cfg.AssumptionHistoryMaxAge = 30 * 24 * time.Hour
	}
	if cfg.OriginCARenewalCheckInterval == 0 {
		cfg.OriginCARenewalCheckInterval = time.Hour
	}
}
