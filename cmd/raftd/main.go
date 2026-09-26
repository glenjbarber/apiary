// Command raftd runs the Apiary raft consensus agent as a standalone
// process, exposing the internal RaftInternal protocol over a Unix domain
// socket for managerd to consume.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"github.com/glenjbarber/apiary/internal/buildinfo"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
	"github.com/glenjbarber/apiary/internal/restartplan"
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

// restoreConfirmPhrase is -restore's own confirmation phrase, matching
// resetConfirmPhrase's exact-match-or-nothing-happens posture
// (docs/adr/0051-raftd-config-save-restore.md) - restoring is
// destructive to whatever (if anything) is currently in -data-dir, the
// same class of accidental-persistent-flag risk resetConfirmPhrase's
// own doc comment already explains.
const restoreConfirmPhrase = "yes-restore-raft-state"

// configArchiveFormatVersion is the internalpb.ConfigArchive envelope
// version this binary writes (-export) and requires (-restore) -
// independent of FSMSnapshotState's own field additions, which the
// archive's payload carries unchanged. Bump this if ConfigArchive's
// own shape ever changes incompatibly.
const configArchiveFormatVersion = 1

func main() {
	if err := run(); err != nil {
		log.Fatalf("raftd: %v", err)
	}
}

func run() error {
	// Only these five one-shot recovery/destructive modes remain CLI
	// flags (see this file's own resetConfirmPhrase/
	// restoreConfirmPhrase doc comments, and raftdconfig's package doc
	// comment, for why: a value persisted in a config file would act
	// on every daemon(8) -r respawn, not just once).
	reset := flag.String("reset", "", fmt.Sprintf("Tier 1 reset (ADR-0038): wipe this node's own raft state and exit, rather than starting the server - real VMs/jails/disks are untouched, just orphaned from tracking until re-registered. Must be exactly %q or nothing happens; the next normal (no -reset) start bootstraps fresh automatically against the now-empty data directory", resetConfirmPhrase))
	exportPath := flag.String("export", "", "write this node's current, live ephemeral state (VMs/networks/jails/API keys) to the given path as a portable archive, then exit, rather than starting the server - requires raftd already running and reachable at its own configured socket (see docs/adr/0051-raftd-config-save-restore.md)")
	restorePhrase := flag.String("restore", "", fmt.Sprintf("restore ephemeral state from -restore-file into this node's own, currently-empty data directory, then exit, rather than starting the server - run -reset first if it isn't already empty. Must be exactly %q or nothing happens; the next normal start picks up the restored state automatically", restoreConfirmPhrase))
	restoreFile := flag.String("restore-file", "", "path to a -export archive to load for -restore")
	restoreDryRun := flag.String("restore-dry-run", "", "validate the archive at the given path (format version, checksum) and print a summary of what it contains, then exit - makes no changes at all, needs no confirmation phrase, and does not touch the data directory")
	statusJSON := flag.Bool("status", false, "read this node's own persisted raft state (current term, log bounds, membership) straight from its data directory and exit - starts no server, takes no vote, contacts no peer, and writes nothing. Requires raftd to be stopped, since it holds an exclusive lock on the log; add -status-json for machine-readable output")
	statusAsJSON := flag.Bool("status-json", false, "same read-only offline read as -status, rendered as a single JSON object for scripting. Implies -status")
	buildinfo.RegisterVersionFlag(flag.CommandLine)
	flag.Parse()

	// Checked before ANY other work, so `-version` answers on a
	// machine with no /var/db/apiary, no config, and no
	// privileges. That is the whole point: you want to ask a
	// deployed binary what it is when it is the only thing left
	// to ask.
	if buildinfo.VersionRequested() {
		fmt.Print(buildinfo.Report("raftd"))
		return nil
	}

	// -reset/-export/-restore/-restore-dry-run are raftd's "break
	// glass" recovery tools - they must keep working even if
	// raftd.json itself is malformed, since a broken config is
	// exactly the situation an operator most needs them for. Load()
	// error here falls back to raftdconfig.Defaults() with a warning,
	// rather than aborting; normal server startup below is stricter.
	oneShot := *reset != "" || *exportPath != "" || *restorePhrase != "" || *restoreDryRun != "" || *statusJSON || *statusAsJSON
	cfgMgr := &raftdconfig.Manager{}
	rcfg, err := cfgMgr.Load()
	if err != nil {
		if !oneShot {
			return fmt.Errorf("reading %s: %w", raftdconfig.DefaultPath, err)
		}
		log.Printf("raftd: reading %s: %v - falling back to defaults for this one-shot command", raftdconfig.DefaultPath, err)
		rcfg = raftdconfig.Defaults()
	}

	if *reset != "" {
		return resetDataDir(*reset, rcfg.DataDir)
	}

	if *statusJSON || *statusAsJSON {
		return printOfflineStatus(raftnode.Config{
			NodeID:   rcfg.NodeID,
			DataDir:  rcfg.DataDir,
			BindAddr: rcfg.RaftBind,
		}, *statusAsJSON)
	}

	if err := validateAwaitJoinFlags(rcfg.AwaitJoin, rcfg.Join); err != nil {
		return err
	}

	cfg := raftnode.Config{
		NodeID:   rcfg.NodeID,
		DataDir:  rcfg.DataDir,
		BindAddr: rcfg.RaftBind,
		TLSCert:  rcfg.RaftTLSCert,
		TLSKey:   rcfg.RaftTLSKey,
		TLSCA:    rcfg.RaftTLSCA,
	}

	if *exportPath != "" {
		return exportLiveState(*exportPath, rcfg.Socket, rcfg.InternalToken)
	}
	if *restoreDryRun != "" {
		return dryRunRestore(*restoreDryRun)
	}
	if *restorePhrase != "" {
		return restoreDataDir(*restorePhrase, *restoreFile, cfg)
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

	if err := startupJoinOrBootstrap(node, hadState, rcfg.Join, resolvedNodeID, rcfg.RaftBind, rcfg.InternalToken, rcfg.AwaitJoin); err != nil {
		return err
	}

	lis, err := listenUnix(rcfg.Socket)
	if err != nil {
		return fmt.Errorf("listening on socket: %w", err)
	}

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(raftnode.TokenUnaryInterceptor(rcfg.InternalToken)),
		grpc.StreamInterceptor(raftnode.TokenStreamInterceptor(rcfg.InternalToken)),
	)
	internalpb.RegisterRaftInternalServer(grpcServer, raftnode.NewServer(node))

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- grpcServer.Serve(lis)
	}()

	log.Printf("raftd: %s listening on %s (node-id=%s, raft-bind=%s, data-dir=%s, raft-tls=%v)",
		buildinfo.String(),
		rcfg.Socket, resolvedNodeID, rcfg.RaftBind, rcfg.DataDir, rcfg.RaftTLSCert != "")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ADR-0125 §2: this process may BE the restarted one. If managerd
	// reserved a restart-lease FSM lease for apiary_raftd on this node
	// and wrote the pending-restart record before issuing
	// `service apiary_raftd restart`, that record is on disk right now
	// and only this new process can honestly say "I am back". The lease
	// has no TTL, so a node that never confirms stays blocked
	// cluster-wide forever.
	//
	// Launched as a goroutine, after the server is already serving, for
	// two reasons. The confirmation is a loopback call to a process rc.d
	// started before this one - there is no reason for raftd's own
	// readiness to wait on it, and blocking startup on it would mean a
	// missing or hung managerd delayed raftd coming up at all. And the
	// hook is bounded (5 attempts, 3s apart) and non-fatal by
	// construction; see confirm.go. It is also not redundant with
	// anything: cmd/managerd has its own equivalent hook for
	// apiary_managerd, and the two services have independent leases.
	confirmCtx, cancelConfirm := context.WithCancel(ctx)
	defer cancelConfirm()
	go confirmPendingRestartOnStartup(confirmCtx, restartConfirmDeps{
		NodeID:    resolvedNodeID,
		Service:   restartplan.DefaultService,
		Store:     restartplan.NewPendingStore(restartplan.DefaultStateDir),
		Confirmer: newManagerdConfirmer(logf),
		Clock:     restartplan.SystemClock{},
		Logf:      logf,
		Results:   restartplan.NewResultStore(restartplan.DefaultResultDir),
	})

	select {
	case <-ctx.Done():
		// Stop the confirmation hook as part of shutting down rather than
		// leaving it to run its own budget out against a managerd that
		// may itself be going away. It is bounded either way; this just
		// makes the shutdown deterministic.
		cancelConfirm()
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
	_ = os.Remove(rcfg.Socket)

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

// exportLiveState implements the export half of
// docs/adr/0051-raftd-config-save-restore.md: dial a running raftd at
// socketPath, read its current, live FSM state via ExportState (not a
// periodic on-disk raft snapshot - see FSM.SnapshotState's doc comment
// for why that would be unreliable), and write it to path as an
// internalpb.ConfigArchive. Non-destructive - unlike every other
// one-shot mode in this file, needs no confirmation phrase.
func exportLiveState(path, socketPath, token string) error {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(raftnode.TokenCredentials(token)),
	)
	if err != nil {
		return fmt.Errorf("dialing %s: %w", socketPath, err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := internalpb.NewRaftInternalClient(conn)
	resp, err := client.ExportState(ctx, &internalpb.ExportStateRequest{})
	if err != nil {
		return err
	}
	if resp.GetError() != "" {
		if resp.GetLeaderHint() != "" {
			return fmt.Errorf("%s (leader hint: %s)", resp.GetError(), resp.GetLeaderHint())
		}
		return errors.New(resp.GetError())
	}

	checksum := sha256.Sum256(resp.GetFsmSnapshotState())
	archive := &internalpb.ConfigArchive{
		FormatVersion:    configArchiveFormatVersion,
		ExportedUnix:     time.Now().Unix(),
		NodeId:           resp.GetNodeId(),
		AppliedIndex:     resp.GetAppliedIndex(),
		FsmSnapshotState: resp.GetFsmSnapshotState(),
		Checksum:         checksum[:],
	}
	data, err := proto.Marshal(archive)
	if err != nil {
		return fmt.Errorf("marshaling archive: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	log.Printf("raftd: exported live state (node-id=%s, applied-index=%d) to %s",
		resp.GetNodeId(), resp.GetAppliedIndex(), path)
	return nil
}

// loadConfigArchive reads and validates an archive written by -export:
// format_version, then a SHA-256 checksum of its embedded
// FSMSnapshotState payload - independent of raft's own on-disk CRC64,
// since a portable archive may sit outside raftd's data directory for
// a long time before being read back. Shared by restoreDataDir and
// -restore-dry-run so both apply the exact same validation.
func loadConfigArchive(file string) (*internalpb.ConfigArchive, *internalpb.FSMSnapshotState, error) {
	if file == "" {
		return nil, nil, errors.New("archive path must be set")
	}

	data, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", file, err)
	}

	var archive internalpb.ConfigArchive
	if err := proto.Unmarshal(data, &archive); err != nil {
		return nil, nil, fmt.Errorf("parsing %s as a config archive: %w", file, err)
	}
	if archive.GetFormatVersion() != configArchiveFormatVersion {
		return nil, nil, fmt.Errorf("%s has format_version %d, this raftd only supports %d", file, archive.GetFormatVersion(), configArchiveFormatVersion)
	}
	checksum := sha256.Sum256(archive.GetFsmSnapshotState())
	if !bytes.Equal(checksum[:], archive.GetChecksum()) {
		return nil, nil, fmt.Errorf("%s failed checksum verification - the file may be corrupt or truncated", file)
	}

	var state internalpb.FSMSnapshotState
	if err := proto.Unmarshal(archive.GetFsmSnapshotState(), &state); err != nil {
		return nil, nil, fmt.Errorf("parsing %s's embedded FSM state: %w", file, err)
	}

	return &archive, &state, nil
}

// dryRunRestore validates an archive exactly like restoreDataDir would,
// then prints a summary of what it contains and returns - it never
// touches -data-dir or calls raftnode.SeedSnapshot. Needs no
// confirmation phrase, since it makes no changes at all.
func dryRunRestore(file string) error {
	archive, state, err := loadConfigArchive(file)
	if err != nil {
		return err
	}

	log.Printf("raftd: %s is a valid archive (format_version=%d, exported-node-id=%s, exported-unix=%d, applied-index=%d)",
		file, archive.GetFormatVersion(), archive.GetNodeId(), archive.GetExportedUnix(), archive.GetAppliedIndex())
	log.Printf("raftd: would restore %d VMs, %d networks, %d jails, %d API keys - nothing was changed (dry run)",
		len(state.GetVms()), len(state.GetNetworks()), len(state.GetJails()), len(state.GetApiKeys()))
	return nil
}

// restoreDataDir implements the restore half of
// docs/adr/0051-raftd-config-save-restore.md: verify phrase and
// archive integrity, then seed cfg.DataDir with the archive's payload
// via raftnode.SeedSnapshot so the next normal start picks it up
// automatically - see that function's own doc comment for why this
// needs no new runtime code path. phrase must exactly equal
// restoreConfirmPhrase or nothing happens at all, matching
// resetDataDir's own posture.
func restoreDataDir(phrase, file string, cfg raftnode.Config) error {
	if phrase != restoreConfirmPhrase {
		return fmt.Errorf("-restore value %q does not match the required confirmation phrase %q - nothing was done", phrase, restoreConfirmPhrase)
	}

	archive, state, err := loadConfigArchive(file)
	if err != nil {
		return err
	}

	if err := raftnode.SeedSnapshot(cfg, state); err != nil {
		return fmt.Errorf("seeding %s with %s: %w", cfg.DataDir, file, err)
	}

	log.Printf("raftd: restored %s (exported node-id=%s, applied-index=%d, %d VMs, %d networks, %d jails, %d API keys) into %s - the next normal start will pick it up automatically",
		file, archive.GetNodeId(), archive.GetAppliedIndex(), len(state.GetVms()), len(state.GetNetworks()), len(state.GetJails()), len(state.GetApiKeys()), cfg.DataDir)
	return nil
}

// validateAwaitJoinFlags rejects the one combination that makes no sense:
// -await-join asks raftd to sit passively waiting to be joined, while
// -join actively dials a peer to join through - a caller can't mean both.
func validateAwaitJoinFlags(awaitJoin bool, joinSocket string) error {
	if awaitJoin && joinSocket != "" {
		return errors.New("-await-join and -join are mutually exclusive")
	}
	return nil
}

// startupJoinOrBootstrap decides, exactly once at startup, what this node
// should do about cluster membership: resume (hadState), join through an
// existing member's socket (-join), sit passively waiting to be added by
// some other Colony's leader (-await-join, ADR-0083), or self-bootstrap a
// new single-node cluster (the default, unchanged since before ADR-0083).
// Exactly one of these branches ever runs, and -await-join never
// self-forms anything - it only starts node's transport (already done by
// raftnode.New before this is called) and returns, mirroring
// internal/raft/multinode_test.go's newUnbootstrappedNode.
func startupJoinOrBootstrap(node *raftnode.Node, hadState bool, joinSocket, resolvedNodeID, bindAddr, internalToken string, awaitJoin bool) error {
	switch {
	case hadState:
		log.Printf("raftd: resuming existing raft state")
	case joinSocket != "":
		if err := joinCluster(joinSocket, resolvedNodeID, bindAddr, internalToken); err != nil {
			return fmt.Errorf("joining cluster via %s: %w", joinSocket, err)
		}
		log.Printf("raftd: joined existing cluster via %s", joinSocket)
	case awaitJoin:
		log.Printf("raftd: -await-join set - not bootstrapping; waiting to be added as a voter by an existing Colony's leader")
	default:
		if err := node.Bootstrap(); err != nil {
			return fmt.Errorf("bootstrapping single-node cluster: %w", err)
		}
		log.Printf("raftd: bootstrapped new single-node cluster")
	}
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
