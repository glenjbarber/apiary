// ConvertStandaloneToJoiner (ADR-0105): a guarded, single-purpose action
// that converts an already-bootstrapped, standalone single-node Comb into
// a joiner for an existing Colony. See raftdservice.go for the underlying
// stop/reset/start controller this reuses, and joincolony.go for the
// ADR-0083 mutual-authorization flow this hands off to once the local
// conversion succeeds.
package manager

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/guardrail"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

// convertToJoinerConfirmPhrase is ConvertStandaloneToJoiner's own
// exact-match confirmation phrase - the same convention as raftd's own
// -reset (raftdResetConfirmPhrase, raftdservice.go) and apiaryinstall's
// -apply-network. Anything other than an exact match is rejected with no
// action taken at all - not even a check of the other fields.
const convertToJoinerConfirmPhrase = "yes-convert-to-joiner"

// ConvertStandaloneToJoiner implements rpcpb.ManagerServiceServer. Every
// stage below fails closed and returns immediately on error - a partial
// failure is always reported as an error, never accompanied by a
// join_request_id/join_request_code (see the response message's own doc
// comment). Admin-tier only (auth.go).
func (s *Server) ConvertStandaloneToJoiner(ctx context.Context, req *rpcpb.ConvertStandaloneToJoinerRequest) (*rpcpb.ConvertStandaloneToJoinerResponse, error) {
	if req.GetConfirmPhrase() != convertToJoinerConfirmPhrase {
		return &rpcpb.ConvertStandaloneToJoinerResponse{Error: fmt.Sprintf("confirm_phrase %q does not match the required confirmation phrase %q - nothing was done", req.GetConfirmPhrase(), convertToJoinerConfirmPhrase)}, nil
	}
	if s.raftdConversionConfig == nil || s.raftdConversion == nil {
		return &rpcpb.ConvertStandaloneToJoinerResponse{Error: "this node has no raftd conversion support configured"}, nil
	}

	target := req.GetTargetManagerdAddress()
	if target == "" {
		return &rpcpb.ConvertStandaloneToJoinerResponse{Error: "target_managerd_address must be set"}, nil
	}
	newRaftBind := req.GetRaftBind()
	if err := validateJoinerRaftBind(newRaftBind); err != nil {
		return &rpcpb.ConvertStandaloneToJoinerResponse{Error: err.Error()}, nil
	}

	// Load first, before touching anything - fail closed on an
	// unreadable/malformed local config (a fail-closed condition in its
	// own right, not just a prerequisite for the fields below).
	cfg, err := s.raftdConversionConfig.Load()
	if err != nil {
		return &rpcpb.ConvertStandaloneToJoinerResponse{Error: fmt.Sprintf("reading this node's raftd configuration: %v", err)}, nil
	}
	if cfg.DataDir == "" {
		cfg = mergeRaftdDefaults(cfg)
	}

	// Reachability preflight against the target Colony member, using the
	// exact same evaluateJoinReachability the target's own
	// ApproveJoinRequest will run - so a failure here can never diverge
	// from what would happen on the other side anyway. Checked before any
	// irreversible local action (stop/reset).
	if report := s.evaluateJoinReachability(ctx, target); report.Verdict != guardrail.Allow {
		detail := "unknown reason"
		if len(report.Findings) > 0 {
			detail = report.Findings[0].Detail
		}
		return &rpcpb.ConvertStandaloneToJoinerResponse{Error: fmt.Sprintf("target_managerd_address %q is not reachable: %s", target, detail)}, nil
	}

	// Stop raftd - reversible on its own (a plain restart brings back the
	// exact same standalone state) - before checking for existing state,
	// since HasExistingState opens the same BoltDB file raftd itself holds
	// open while running.
	if err := s.raftdConversion.Stop(ctx); err != nil {
		return &rpcpb.ConvertStandaloneToJoinerResponse{Error: fmt.Sprintf("stopping apiary_raftd: %v", err)}, nil
	}

	hadState, err := raftnode.HasExistingState(raftnode.Config{DataDir: cfg.DataDir})
	if err != nil {
		// Best-effort restart so a broken check doesn't also leave the
		// Comb down - the RPC still reports the real error.
		_ = s.raftdConversion.Start(ctx)
		return &rpcpb.ConvertStandaloneToJoinerResponse{Error: fmt.Sprintf("checking existing raft state at %s: %v", cfg.DataDir, err)}, nil
	}
	if !hadState {
		_ = s.raftdConversion.Start(ctx)
		return &rpcpb.ConvertStandaloneToJoinerResponse{Error: fmt.Sprintf("no existing raft state found at %s - nothing to convert; configure join/await_join directly instead of this action", cfg.DataDir)}, nil
	}

	backupPath, err := s.raftdConversion.Reset(ctx)
	if err != nil {
		// raftd is stopped and its prior state's fate is uncertain - do
		// not attempt to restart automatically here, and do not guess at
		// a backup path. Surface exactly what happened.
		return &rpcpb.ConvertStandaloneToJoinerResponse{Error: fmt.Sprintf("resetting local raft state: %v (apiary_raftd is currently stopped)", err)}, nil
	}

	newCfg := cfg
	newCfg.RaftBind = newRaftBind
	newCfg.AwaitJoin = true
	newCfg.Join = ""
	if err := s.raftdConversionConfig.Save(newCfg); err != nil {
		return &rpcpb.ConvertStandaloneToJoinerResponse{Error: fmt.Sprintf("saving updated raftd configuration: %v (apiary_raftd is currently stopped; existing state was already moved to %s)", err, backupPath), BackupDataDir: backupPath}, nil
	}

	if err := s.raftdConversion.Start(ctx); err != nil {
		return &rpcpb.ConvertStandaloneToJoinerResponse{Error: fmt.Sprintf("starting apiary_raftd with new configuration: %v", err), BackupDataDir: backupPath}, nil
	}

	if err := waitForRaftdListening(ctx, s.raftdConversion, newRaftBind); err != nil {
		return &rpcpb.ConvertStandaloneToJoinerResponse{Error: err.Error(), BackupDataDir: backupPath}, nil
	}

	nodeID := cfg.NodeID
	if nodeID == "" {
		// Mirrors internal/raft's own "empty means os.Hostname()"
		// convention (see raftdconfig.Config.NodeID's doc comment) -
		// duplicated here rather than shared because resolving it for
		// real requires constructing a raft Node, which this RPC has no
		// reason to do.
		if h, herr := os.Hostname(); herr == nil {
			nodeID = h
		}
	}

	joinResp, err := s.RequestJoinColony(ctx, &rpcpb.RequestJoinColonyRequest{
		NodeId:          nodeID,
		RaftBindAddress: newRaftBind,
		TargetAddress:   target,
	})
	if err != nil {
		return &rpcpb.ConvertStandaloneToJoinerResponse{Error: fmt.Sprintf("submitting join request: %v", err), BackupDataDir: backupPath}, nil
	}
	if joinResp.GetError() != "" {
		return &rpcpb.ConvertStandaloneToJoinerResponse{Error: joinResp.GetError(), BackupDataDir: backupPath}, nil
	}

	return &rpcpb.ConvertStandaloneToJoinerResponse{
		BackupDataDir:   backupPath,
		JoinRequestId:   joinResp.GetRequestId(),
		JoinRequestCode: joinResp.GetCode(),
	}, nil
}

// validateJoinerRaftBind rejects a raft_bind value a remote Colony peer
// could never actually dial - the same class of mistake docs/bootstrap.md
// already warns operators about by hand for the manual join path.
func validateJoinerRaftBind(addr string) error {
	if addr == "" {
		return fmt.Errorf("raft_bind must be set")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid raft_bind %q: %w", addr, err)
	}
	switch strings.ToLower(host) {
	case "", "0.0.0.0", "127.0.0.1", "localhost", "::", "::1":
		return fmt.Errorf("raft_bind %q is not a real, remotely-reachable address - a Colony peer must be able to dial it directly", addr)
	}
	return nil
}

// mergeRaftdDefaults fills in raftdconfig.Defaults() for any field Load
// left at its zero value because the config file did not set it - Load()
// already does this internally, so in practice cfg.DataDir is never empty
// coming out of a successful Load; this exists purely as defense in depth
// against a future raftdConversionConfigStore implementation (e.g. a test
// fake) that does not replicate that behavior.
func mergeRaftdDefaults(cfg raftdconfig.Config) raftdconfig.Config {
	defaults := raftdconfig.Defaults()
	if cfg.DataDir == "" {
		cfg.DataDir = defaults.DataDir
	}
	if cfg.Socket == "" {
		cfg.Socket = defaults.Socket
	}
	return cfg
}
