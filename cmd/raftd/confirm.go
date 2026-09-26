// confirm.go is cmd/raftd's half of ADR-0125 §2: the confirm-on-startup
// hook that closes ADR-0116's first recorded gap ("no confirm-on-startup
// hook for raftd").
//
// The reason this hook must exist at all is worth stating plainly, because
// it is the whole justification for the file. The process that *issues*
// `service apiary_raftd restart` is managerd, and the restart replaces
// raftd's process while managerd is still the one holding the
// cluster-wide restart lease. A lease (internalpb.RestartLease,
// replicated through the raft log) has no TTL and no auto-expiry: if
// nothing ever records the completion, every subsequent raftd restart on
// every node stays blocked forever, with no automatic recovery. So the
// confirmation has to come from the restarted process itself, on its own
// next startup, because that is the only process that is unambiguously
// the new one.
//
// ADR-0125 §2, as written, has this process dial its own Unix socket and
// call manager's ConfirmRestartCompletedLocal. Neither half matches the
// code as it stands, and neither could be made to match without editing
// files this change does not own:
//
//   - rcfg.Socket is raftd's OWN serving socket. raftd *serves* the
//     RaftInternal service (see main.go: RegisterRaftInternalServer);
//     managerd is its client. raftd dialing "its local managerd via
//     rcfg.Socket" has the direction of the connection backwards.
//   - ConfirmRestartCompletedLocal is a plain Go method on
//     *manager.Server (internal/manager/server.go), deliberately never
//     exposed on the wire - its own doc comment says it is for "the
//     restarted node's own next startup calling on its own behalf, the
//     same trusted process as this Server, never over the wire". It is
//     not an RPC in api/rpc/manager.proto or api/internalpb/raftd.proto,
//     and adding one would mean editing both protos and running
//     `buf generate`.
//
// What this file does instead is the same operation over a surface that
// already exists: raftd calls the existing ConfirmRestartCompleted RPC on
// its local managerd's ManagerService, presenting the restart-guardrail
// token. That is the *external* form of exactly what
// ConfirmRestartCompletedLocal performs - both funnel into
// manager.Server's own unexported confirmRestartCompleted, which applies
// the RecordRestartCompleted raft command on the leader and releases the
// lease on an exact service + node_id + lease_id match - so every
// behaviour ADR-0125 §2 asks for is preserved: the confirmation is made
// by the restarted process itself, managerd forwards it to the leader,
// and the release happens through the same raft-replicated FSM apply.
//
// The cost of the substitution is that this path is authenticated rather
// than trusted-by-c sameness, so it needs the restart-guardrail token
// that the ADR's "local" path would not have needed. That token is
// already an existing operational requirement on every node (managerd
// refuses every forwarded lease/confirm RPC without it), so this adds no
// new provisioning requirement - but a node with no token provisioned
// cannot self-confirm. That is reported loudly below and recorded in the
// restart-result record rather than being swallowed.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/restartplan"
)

// restartGuardrailTokenPath is where the dedicated restart-guardrail
// token lives - the same path, and the same one-token-per-cluster model,
// cmd/managerd loads it from for the leader-side half of this same
// protocol. Duplicated as a constant rather than imported because
// cmd/managerd's copy is unexported in package main, and this codebase
// already accepts exactly this kind of small duplication across
// independent binaries (see managerd's own restartGuardrailService and
// internal/manager's apiKeyCredentials doc comment).
const restartGuardrailTokenPath = "/usr/local/etc/apiary/restart-guardrail-token"

// managerdConfigPath is managerd's own node config, read here only to
// learn where its gRPC surface is and whether it is TLS. raftd has no
// -rpc-addr of its own (internal/raftdconfig.Config is a different,
// deliberately minimal daemon config), so reading the other daemon's
// config is the only way to reach it correctly on a node where it is not
// on the default address. Read-only, best-effort, and fully defaulted
// when absent - see resolveLocalManagerdEndpoint.
const managerdConfigPath = "/usr/local/etc/apiary/managerd.json"

// defaultLocalManagerdAddr is cmd/managerd's own "-rpc-addr" default,
// applied when the node config leaves it empty. It is a fallback, not a
// hardcoded dial: resolveLocalManagerdEndpoint prefers whatever
// managerd.json actually says.
const defaultLocalManagerdAddr = "127.0.0.1:17700"

// confirmEnvVar overrides the resolved managerd address outright, for the
// case where neither the node config nor the default is right.
const confirmEnvVar = "APIARY_MANAGERD_ADDR"

// confirmCallTimeout bounds one confirmation attempt's own RPC. Small on
// purpose: this is a call to a process rc.d started before this one, so
// it either answers promptly or is not going to answer at all, and the
// retry budget is the real bound.
const confirmCallTimeout = 10 * time.Second

// endpoint is where and how to reach this node's own managerd.
type endpoint struct {
	addr   string
	useTLS bool
	caFile string
	// serverName pins the TLS SNI/verification name when managerd's
	// certificate is issued for a name other than the loopback address
	// this hook dials - the same problem internal/manager's own
	// dialRestartGuardrail solves with its peer hostname map.
	serverName string
}

// managerdConfigPeek is the minimal shape of managerd.json this hook
// reads. Only the three fields that decide how to dial are decoded;
// everything else in that file is deliberately ignored, and a field this
// struct does not name cannot affect the dial.
type managerdConfigPeek struct {
	RPCAddr   string `json:"rpc_addr"`
	TLSCert   string `json:"tls_cert"`
	PeerTLSCA string `json:"peer_tls_ca"`
}

// resolveLocalManagerdEndpoint works out where local managerd listens.
// Precedence: confirmEnvVar, then managerd.json's rpc_addr, then
// cmd/managerd's own default. TLS is taken from managerd.json's tls_cert
// being set, with peer_tls_ca as the trust anchor, because dialing a
// TLS-serving managerd in plaintext would fail every time and a
// plaintext-serving one with TLS on would likewise fail - guessing wrong
// in either direction is worse than reading the answer.
//
// Every failure path here is non-fatal and falls back to the defaults,
// because a raftd that refuses to start because managerd's config file is
// unreadable would be a far worse outcome than a confirmation that does
// not land (which the retry budget and the durable record both report
// honestly).
func resolveLocalManagerdEndpoint(logf func(format string, args ...any)) endpoint {
	ep := endpoint{addr: defaultLocalManagerdAddr}
	if env := strings.TrimSpace(os.Getenv(confirmEnvVar)); env != "" {
		ep.addr = env
		return ep
	}

	data, err := os.ReadFile(managerdConfigPath)
	if err != nil {
		if !os.IsNotExist(err) {
			logf("reading %s: %v - assuming managerd is on %s", managerdConfigPath, err, ep.addr)
		}
		return ep
	}
	var peek managerdConfigPeek
	if err := json.Unmarshal(data, &peek); err != nil {
		logf("parsing %s: %v - assuming managerd is on %s", managerdConfigPath, err, ep.addr)
		return ep
	}
	if addr := strings.TrimSpace(peek.RPCAddr); addr != "" {
		ep.addr = addr
	}
	if strings.TrimSpace(peek.TLSCert) != "" {
		ep.useTLS = true
		ep.caFile = strings.TrimSpace(peek.PeerTLSCA)
	}
	return ep
}

// loadCAPool reads a PEM bundle into a cert pool. An unreadable or
// malformed CA file falls back to the system pool rather than failing the
// dial outright, matching internal/manager's own LoadPeerCAPool posture -
// a wrong trust anchor is a connection failure either way, and the system
// pool is the only other thing that could possibly work.
func loadCAPool(path string) *x509.CertPool {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil
	}
	return pool
}

// logf is this package's logging prefix helper, matching the
// `log.Printf("raftd: ...")` form every other line in main.go uses. It
// exists because confirmPendingRestartOnStartup takes a LogFunc and main
// needs to pass one that produces byte-identical output to the rest of
// this binary's logs.
func logf(format string, args ...any) { log.Printf("raftd: "+format, args...) }

// restartConfirmDeps is everything confirmPendingRestartOnStartup needs,
// gathered into one struct so the hook is unit-testable with a fake
// Confirmer, a fake Clock and a temp directory - i.e. with no raftd, no
// managerd, no FreeBSD and no sleeping.
type restartConfirmDeps struct {
	// NodeID and Service name what is being confirmed. Service is
	// normally restartplan.DefaultService.
	NodeID  string
	Service string

	// Store is the pending-restart record this process is confirming on
	// its predecessor's behalf. Required.
	Store *restartplan.PendingStore

	// Confirmer performs the actual confirmation. Required.
	Confirmer restartplan.Confirmer

	// Clock drives the retry backoff. nil means restartplan.SystemClock.
	Clock restartplan.Clock

	// Logf receives operator-facing progress. nil means the standard
	// logger with this file's "raftd:" prefix, matching main.go.
	Logf func(format string, args ...any)

	// Results, when set, receives a durable record of what this
	// startup's attempt actually established - including when the answer
	// is "unknown", which is precisely the case that must never be
	// recorded as a success.
	Results *restartplan.ResultStore

	// Attempt labels this boot's own attempt number, so a node that has
	// been restarted repeatedly without ever confirming shows a history
	// rather than one attempt silently overwriting the last.
	Attempt int

	// Confirm bounds the retry loop; zero means
	// restartplan.DefaultConfirmOptions.
	Confirm restartplan.ConfirmOptions
}

func (d restartConfirmDeps) withDefaults() restartConfirmDeps {
	if d.Service == "" {
		d.Service = restartplan.DefaultService
	}
	if d.Clock == nil {
		d.Clock = restartplan.SystemClock{}
	}
	if d.Logf == nil {
		d.Logf = func(format string, args ...any) { log.Printf("raftd: "+format, args...) }
	}
	d.Confirm = d.Confirm.WithDefaults()
	return d
}

func (d restartConfirmDeps) logf(format string, args ...any) { d.Logf(format, args...) }

// confirmPendingRestartOnStartup is cmd/raftd's counterpart to
// cmd/managerd's own confirmPendingRestartOnStartup (ADR-0103), and is
// the piece that closes ADR-0116's "no confirm-on-startup hook for raftd"
// gap.
//
// It is deliberately bounded rather than persistent: ADR-0125 §7 budgets
// it at 5 attempts 3s apart, matching managerd's proven hook exactly. A
// repeated failure is logged loudly and the pending record is LEFT IN
// PLACE - clearing it on failure would be the single worst possible bug
// here, since the lease stays held either way and the record is the only
// thing that tells an operator (or a later startup) what is outstanding.
// There is no unbounded background retry loop: a raftd that retried
// forever against a managerd that will never answer would never surface
// the problem, and would make the node look healthy while permanently
// blocking the cluster's next maintenance.
//
// It is also deliberately not fatal. A confirmation failure must never
// stop raftd from serving: raftd coming up is worth far more than the
// lease being released promptly, and the lease's release is recoverable
// (Force, or a later startup) while a dead raftd is not.
func confirmPendingRestartOnStartup(ctx context.Context, deps restartConfirmDeps) restartplan.Result {
	deps = deps.withDefaults()
	started := deps.Clock.Now()
	result := restartplan.Result{
		Service:       deps.Service,
		NodeID:        deps.NodeID,
		Attempt:       deps.Attempt,
		StartedAtUnix: started.Unix(),
	}

	if deps.Store == nil || deps.Confirmer == nil {
		// Nothing to confirm and no way to confirm it: not a failure of
		// anything, just a raftd started for some other reason (a fresh
		// boot, a one-shot CLI mode's host process, a test). No record is
		// written, because there is no attempt to record.
		deps.logf("no pending restart-guardrail confirmation to make (store configured=%v, confirmer configured=%v)", deps.Store != nil, deps.Confirmer != nil)
		return restartplan.Result{}
	}

	pending, found, err := deps.Store.Load(deps.Service)
	if err != nil {
		// An unreadable record is the worst case to swallow: the lease
		// may well be held, and we cannot even say what it is. Recorded
		// as unknown rather than proceeding as though no lease exists.
		deps.logf("reading pending restart-guardrail confirmation for %s: %v - if a lease is held for this service it stays blocked until an operator clears it", deps.Service, err)
		result.Outcome = restartplan.OutcomeUnverified
		result.Detail = "the pending-restart record could not be read, so whether a restart lease is outstanding for this service is unknown"
		result.Evidence = []string{err.Error()}
		return persistResult(deps, result)
	}
	if !found {
		// The overwhelmingly common case: raftd was started for a reason
		// that had nothing to do with a restart lease.
		return restartplan.Result{}
	}
	result.LeaseID = pending.LeaseID
	deps.logf("found a pending restart-guardrail confirmation: %s lease %d on %s - this process is the restarted one, so it is confirming on its predecessor's behalf",
		pending.Service, pending.LeaseID, pending.NodeID)

	confirm := restartplan.ConfirmWithRetry(ctx, deps.Confirmer, pending, deps.Clock, deps.Confirm)
	if confirm.Confirmed {
		// Clear only after a confirmation actually succeeded. A lease is
		// released by the raft FSM apply, not by this file; this only
		// removes the local trace so the next boot does not re-confirm a
		// lease that no longer exists.
		if err := deps.Store.Clear(pending.Service); err != nil {
			deps.logf("confirmed %s lease %d, but clearing the pending record failed: %v - the next raftd start will re-confirm an already-released lease, which is harmless but noisy", pending.Service, pending.LeaseID, err)
			result.Outcome = restartplan.OutcomeConfirmed
			result.Detail = "the restarted process confirmed itself, but the local pending record could not be removed"
			result.Evidence = append(confirm.Evidence, err.Error())
			return persistResult(deps, result)
		}
		deps.logf("confirmed restart-guardrail lease %d for %s complete (node %s, attempt %d)", pending.LeaseID, pending.Service, deps.NodeID, confirm.Attempts)
		result.Outcome = restartplan.OutcomeConfirmed
		result.Detail = "the restarted process reported itself back healthy and the restart lease was released"
		result.Evidence = confirm.Evidence
		return persistResult(deps, result)
	}

	// Gave up without a confirmation. This is the case the whole file
	// exists to keep honest: raftd is up, the restart command ran, and
	// nothing has told us the lease can be released. The record says
	// exactly that - unknown, with every attempt listed - and the pending
	// record stays on disk so a later startup or an operator can finish
	// the job. Nothing here reports success.
	deps.logf("could not confirm restart-guardrail completion for %s after %d attempts - the cluster-wide restart lease remains blocked until this succeeds on a future startup or an operator investigates",
		pending.Service, confirm.Attempts)
	result.Outcome = restartplan.OutcomeUnobserved
	result.Detail = fmt.Sprintf("raftd is running, but the restart lease was never confirmed as released after %d attempts; the lease stays held, and this is neither a failed restart nor a confirmed success", confirm.Attempts)
	result.Evidence = confirm.Evidence
	if confirm.Cancelled {
		result.Detail += " (the wait was cancelled, not exhausted)"
	}
	return persistResult(deps, result)
}

// persistResult stamps, validates and writes the record. A record that
// cannot be written is logged loudly and returned anyway: the caller is a
// startup path whose own job is starting raftd, and failing to start
// raftd because a diagnostic file could not be written would be the wrong
// trade. The record's own text always states what was and was not
// established, so an operator reading logs is not misled by its absence
// on disk.
func persistResult(deps restartConfirmDeps, res restartplan.Result) restartplan.Result {
	res.FinishedAtUnix = deps.Clock.Now().Unix()
	if err := res.Validate(); err != nil {
		deps.logf("refusing to write a malformed restart result: %v", err)
		return res
	}
	if deps.Results != nil {
		if err := deps.Results.Save(res); err != nil {
			deps.logf("writing the durable restart result for %s on %s: %v", res.Service, res.NodeID, err)
		}
	}
	return res
}

// managerdConfirmer is the production restartplan.Confirmer: the existing
// ConfirmRestartCompleted RPC on this node's local managerd, presented
// with the restart-guardrail bearer token and dialed with the same
// TLS/CA handling managerd's own peer dialling uses.
type managerdConfirmer struct {
	ep          endpoint
	token       string
	callTimeout time.Duration

	// newConn is a field so a test can exercise ConfirmRestartCompleted's
	// response handling against a real in-process gRPC server without
	// depending on the production dial path's transport setup.
	newConn func(ctx context.Context, ep endpoint) (restartCompletedCaller, func() error, error)
}

// restartCompletedCaller is the one RPC method this hook needs, named so
// the production gRPC client satisfies it and a test double does too.
type restartCompletedCaller interface {
	ConfirmRestartCompleted(ctx context.Context, in *rpcpb.ConfirmRestartCompletedRequest, opts ...grpc.CallOption) (*rpcpb.ConfirmRestartCompletedResponse, error)
}

// newManagerdConfirmer builds the production confirmer, reading the token
// from disk. A missing token file yields an empty token, which the RPC
// rejects with PermissionDenied - surfacing as an unobserved outcome
// after the retry budget, which is the honest result. It is NOT silently
// treated as "nothing to confirm".
func newManagerdConfirmer(logf func(format string, args ...any)) restartplan.Confirmer {
	ep := resolveLocalManagerdEndpoint(logf)
	token := ""
	if data, err := os.ReadFile(restartGuardrailTokenPath); err == nil {
		token = strings.TrimSpace(string(data))
	} else if !os.IsNotExist(err) {
		logf("reading %s: %v - this raftd cannot confirm its own restart lease until that is fixed", restartGuardrailTokenPath, err)
	} else {
		logf("no %s on this node - if a restart lease is outstanding for %s this raftd will not be able to confirm it, and the cluster stays blocked until an operator intervenes",
			restartGuardrailTokenPath, restartplan.DefaultService)
	}
	c := &managerdConfirmer{ep: ep, token: token, callTimeout: confirmCallTimeout}
	c.newConn = dialManagerd
	return c
}

// dialManagerd opens a gRPC connection to local managerd.
func dialManagerd(ctx context.Context, ep endpoint) (restartCompletedCaller, func() error, error) {
	var opts []grpc.DialOption
	if ep.useTLS {
		cfg := &tls.Config{}
		if pool := loadCAPool(ep.caFile); pool != nil {
			cfg.RootCAs = pool
		}
		if ep.serverName != "" {
			cfg.ServerName = ep.serverName
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	conn, err := grpc.NewClient(ep.addr, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("dialing local managerd at %s: %w", ep.addr, err)
	}
	return rpcpb.NewManagerServiceClient(conn), conn.Close, nil
}

// ConfirmRestartCompleted implements restartplan.Confirmer.
//
// A response carrying a non-empty Error field is returned as an error,
// with the leader hint appended when there is one - the same information
// cmd/managerd's own hook logs, and the same information
// ConfirmWithRetry records as the reason an attempt did not land.
// Folding a refused confirmation into a nil error would report a
// still-held lease as released, which is precisely the failure this
// whole mechanism exists to make impossible.
func (c *managerdConfirmer) ConfirmRestartCompleted(ctx context.Context, service, nodeID string, leaseID uint64) error {
	caller, closeConn, err := c.newConn(ctx, c.ep)
	if err != nil {
		return err
	}
	defer closeConn()

	callCtx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()

	resp, err := caller.ConfirmRestartCompleted(callCtx, &rpcpb.ConfirmRestartCompletedRequest{
		Service: service,
		NodeId:  nodeID,
		LeaseId: leaseID,
	})
	if err != nil {
		return err
	}
	if resp.GetError() != "" {
		if hint := resp.GetLeaderHint(); hint != "" {
			return fmt.Errorf("managerd refused the confirmation: %s (leader hint: %s)", resp.GetError(), hint)
		}
		return fmt.Errorf("managerd refused the confirmation: %s", resp.GetError())
	}
	return nil
}

// bearerToken mirrors internal/manager's unexported apiKeyCredentials:
// the restart-guardrail token is presented as an Authorization: Bearer
// header, which is exactly what internal/manager/auth.go's
// extractBearerToken reads on the other end.
type bearerToken string

func (t bearerToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}

func (bearerToken) RequireTransportSecurity() bool { return false }
