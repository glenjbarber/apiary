// Package manager: the mutually-authorized Colony-join flow (ADR-0083),
// modeled on a device-pairing handshake. A joining Comb calls
// RequestJoinColony on one existing Colony member it names by address;
// that member raft-Applies a PendingJoinRequest so every current
// member's UI shows the same request regardless of which one first
// received the call. An Admin visually compares the returned code
// against the one shown on the joining Comb's own screen before calling
// ApproveJoinRequest, which is what actually calls AddVoter against
// this managerd's own local raftd.
package manager

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/guardrail"
	"github.com/glenjbarber/apiary/internal/hostcert"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

// approveJoinRequestConfirmPhrase is ApproveJoinRequest's own exact-match
// confirmation phrase (ADR-0113) - the same convention as raftd's own
// -reset (raftdResetConfirmPhrase, raftdservice.go), apiaryinstall's
// -apply-network, and ConvertStandaloneToJoiner's own confirm_phrase
// (convertjoiner.go). Approving a join request is the moment this Comb
// starts trusting a brand new Comb's identity going forward - the Admin
// must type this exactly, every time, whether or not the pending
// request carries a tls_cert_fingerprint, since the UI always shows
// whatever identity information is available (a real fingerprint, or an
// explicit "no TLS certificate presented") right next to this field.
const approveJoinRequestConfirmPhrase = "yes-trust-new-comb"

// defaultJoinRequestTTL bounds how long a PendingJoinRequest stays
// actionable - checked lazily at read/apply time (see
// internal/raft/fsm.go's pendingJoinRequestExpired), not enforced by any
// background sweep.
const defaultJoinRequestTTL = 15 * time.Minute

// defaultUnauthenticatedForwardTimeout bounds every dial-and-relay call
// to a caller-supplied target_address (ADR-0092), independent of
// whatever deadline (if any) the caller's own incoming request context
// carries. A 2026-09-12 audit (docs/audits/2026-09-12-security-audit.md)
// noted these three RPCs are deliberately unauthenticated, so
// target_address can name any host:port an attacker chooses; without a
// bound here, pointing it at an address that never completes a TCP
// handshake (a black-holed IP, a firewalled port) would hang the
// handling goroutine for as long as the underlying OS's own connect
// timeout, one goroutine per call - a mild resource-exhaustion angle on
// top of the reachability-oracle behavior the audit also flagged as an
// accepted residual risk. Same value as defaultApplyTimeout, not
// because the two are related, just because it's this codebase's own
// existing "a reasonable few seconds for one RPC hop" default.
const defaultUnauthenticatedForwardTimeout = 10 * time.Second

// checkTargetAddressAllowed enforces the target_address allowlist
// (ADR-0097): when s.knownPeerAddresses is configured (non-nil, via
// -known-peer-addresses), target_address on RequestJoinColony/
// GetJoinRequestStatus/CancelJoinRequest must exactly match one of its
// entries or the call is refused before ever dialing. A 2026-09-12
// independent audit (docs/audits/2026-09-12-security-audit.md)
// confirmed ADR-0096's dialUnauthenticated closed the credential-leak
// half of this finding but not the underlying primitive: these three
// RPCs are deliberately unauthenticated (a joining Comb has no Colony
// API key yet), so target_address itself is caller-supplied and could
// still name any host an attacker chooses, usable as a limited network-
// reachability oracle. This allowlist is opt-in, not a default-secure
// change: an operator's own Colony is a small, fixed, already-known set
// of addresses (this project's own actual deployments top out at four
// hosts), so pre-listing them is practical, but forcing it on every
// deployment would break a fresh, never-configured join with no
// migration path. Nil (unconfigured, the default) preserves ADR-0092's
// original accept-any-target_address behavior exactly.
func (s *Server) checkTargetAddressAllowed(target string) error {
	if s.knownPeerAddresses == nil {
		return nil
	}
	if !s.knownPeerAddresses[target] {
		return fmt.Errorf("target_address %q is not in this node's -known-peer-addresses allowlist", target)
	}
	return nil
}

// dialReachable is the production reachabilityCheck (Server field) -
// a plain TCP connect, not a raft protocol handshake, so it can't catch
// every way a peer might be misconfigured, but it does catch the exact
// failure this project has already been burned by more than once (see
// SHARED.md): approving a join before the joiner's raftd was actually
// up and listening on raft_bind_address. AddVoter commits immediately
// under whatever quorum exists at that instant and can only be reversed
// with agreement from every current voter - including one that was
// never reachable in the first place - so the only recovery from
// approving an unreachable node has been a full raft state wipe. See
// ApproveJoinRequest's own use of this (ADR-0097).
func dialReachable(ctx context.Context, addr string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// preApprovalReachabilityTimeout bounds how long ApproveJoinRequest
// waits for dialReachable before giving up and refusing to approve.
const preApprovalReachabilityTimeout = 5 * time.Second

// currentLeaderRaftAddress finds status's own leader_id in its servers
// list and returns that server's raft address, or "" if the leader is
// unknown or not currently a listed server. Used by
// ApproveJoinRequest/PreflightApproveJoinRequest/ReserveRestartLease/
// ConfirmRestartCompleted to forward to the leader BEFORE doing any
// local work that only the leader's own vantage point should perform
// (ADR-0103) - a cheap, local, already-available read (Status), not a
// new RPC.
func currentLeaderRaftAddress(status *internalpb.StatusResponse) string {
	leaderID := status.GetLeaderId()
	if leaderID == "" {
		return ""
	}
	for _, srv := range status.GetServers() {
		if srv.GetId() == leaderID {
			return srv.GetAddress()
		}
	}
	return ""
}

// evaluateJoinReachability dials addr (via s.reachabilityCheck, swappable
// in tests) and returns guardrail.EvaluateJoinReachability's own verdict
// - the single source of truth for both ApproveJoinRequest's real gate
// and PreflightApproveJoinRequest's preview (ADR-0103), so the two can
// never silently drift apart. A nil s.reachabilityCheck (never set in
// production, only possible if a caller deliberately disables it)
// reports Allow, matching ApproveJoinRequest's own prior "if
// s.reachabilityCheck != nil" skip.
// existingVoter looks up nodeID in this managerd's own local raft
// membership view (ADR-0106) and returns its ServerInfo if it is
// already a member (Voter or Nonvoter - either already occupies the
// identity, so both count for collision purposes), or nil if it is not
// currently known. A raft Status error is returned unmodified so
// callers can decide how to treat "raft unreachable" themselves,
// rather than this helper silently treating it as "no collision."
func (s *Server) existingVoter(ctx context.Context, nodeID string) (*internalpb.ServerInfo, error) {
	status, err := s.raft.Status(ctx)
	if err != nil {
		return nil, err
	}
	for _, srv := range status.GetServers() {
		if srv.GetId() == nodeID {
			return srv, nil
		}
	}
	return nil, nil
}

func (s *Server) evaluateJoinReachability(ctx context.Context, addr string) guardrail.Report {
	if s.reachabilityCheck == nil {
		return guardrail.Report{Intent: "approve-join-request", Verdict: guardrail.Allow}
	}
	checkCtx, cancel := context.WithTimeout(ctx, preApprovalReachabilityTimeout)
	err := s.reachabilityCheck(checkCtx, addr)
	cancel()
	fact := guardrail.JoinReachabilityFact{Address: addr, Reachable: err == nil}
	if err != nil {
		fact.DialError = err.Error()
	}
	return guardrail.EvaluateJoinReachability(fact)
}

// localJoinerLogState reads THIS managerd's own local raftd for the one
// number the join-log guardrail needs, and returns it as a fact ready for
// guardrail.EvaluateJoinLogState.
//
// It reads its own raftd, not the joiner's, and that is the whole design
// constraint. When RequestJoinColony is submitted from the joining Comb's
// own Machine page - the normal path - the managerd receiving this call IS
// the joining Comb's managerd, so its local raftd is the joiner's raftd
// and raftd's own last_log_index is authoritative there. A Comb that has
// never been in a cluster has no log entries and reports 0; one that was
// previously a member, or that bootstrapped on its own, reports the index
// it reached.
//
// The approver cannot do this check itself later, and that is not an
// oversight. raftd holds bbolt's exclusive lock on raft.db for as long as
// it is running, which on an -await-join node is exactly when a join
// request is being made, so any second open of that file would block
// instead of answering. The evidence has to be gathered here, on the node
// that can gather it, and carried on the wire.
//
// A read that fails, or a managerd with no local raft at all, returns an
// UNOBSERVED fact rather than a false zero. ApproveJoinRequest refuses
// that, deliberately: "this build cannot tell" is not grounds for
// performing the one irreversible action the check exists to prevent.
func (s *Server) localJoinerLogState(ctx context.Context) guardrail.JoinerLogStateFact {
	fact := guardrail.JoinerLogStateFact{}
	if s.raft == nil {
		fact.ReadError = "this node has no local raft client"
		return fact
	}
	status, err := s.raft.Status(ctx)
	if err != nil {
		fact.ReadError = err.Error()
		return fact
	}
	fact.Observed = true
	fact.LastLogIndex = status.GetLastLogIndex()
	return fact
}

// localTLSCertFingerprint reads this managerd's own configured tls_cert
// (ADR-0087/ADR-0093) and returns its SHA-256 fingerprint in the same
// "SHA256:AA:BB:..." shape openssl/ssh tooling commonly uses, so an
// Admin can cross-check it against the joining Comb's own certificate
// by any familiar means (ADR-0113). Returns "" with no error whenever
// there is nothing to report - no nodeConfig configured, no tls_cert
// set, or the file is unreadable/unparsable - since a joining Comb with
// TLS not yet configured is a legitimate, existing deployment state
// (TLS is opt-in throughout this codebase), not a fatal condition that
// should block RequestJoinColony from forwarding the join itself.
func (s *Server) localTLSCertFingerprint() string {
	if s.nodeConfig == nil {
		return ""
	}
	cfg, err := s.nodeConfig.Load()
	if err != nil || cfg.TLSCert == "" {
		return ""
	}
	data, err := os.ReadFile(cfg.TLSCert)
	if err != nil {
		return ""
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return ""
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	return tlsCertFingerprint(cert.Raw)
}

// tlsCertFingerprint is a DELEGATION to hostcert.Fingerprint and no
// longer a second definition of the format.
//
// It used to compute "SHA256:" + colon-separated uppercase hex here,
// which meant the tree had two places that could spell a fingerprint
// and one of them was only exercised by a test. ADR-0147 Part 2 makes
// that a wire-compatibility bug rather than a tidiness matter: the
// operator compares a value the JOINER printed against a value the
// TARGET compares, and those two have to be the same string. The
// helper is kept as a one-line forwarding function because several
// call sites and an existing test name it, and deleting a name costs
// more than the indirection does.
func tlsCertFingerprint(der []byte) string {
	return hostcert.Fingerprint(der)
}

// generateJoinRequestID returns a random, non-secret identifier for a
// new PendingJoinRequest - mirrors generateAPIKeyID's own shape
// (auth.go), a short hex id purely for correlating a joining Comb's own
// poll with the record an Admin approves/rejects.
func generateJoinRequestID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "jreq-" + hex.EncodeToString(buf), nil
}

// expiredJoinRequestMessage returns a non-empty error when requestID exists
// and has passed its TTL. Expiry is checked here, against the wall clock,
// before a resolve command is submitted - never inside the FSM, which must
// stay deterministic across replicas and log replays (see
// applyResolvePendingJoinRequest). A missing request or a failed read returns
// "" so the FSM's own does-not-exist and already-resolved errors still apply.
func (s *Server) expiredJoinRequestMessage(ctx context.Context, requestID, opName string) string {
	resp, err := s.raft.GetPendingJoinRequestLocal(ctx, requestID)
	if err != nil || resp.GetError() != "" || resp.GetRequest() == nil {
		return ""
	}
	if joinRequestExpired(resp.GetRequest()) {
		return fmt.Sprintf("%s: request_id %q has expired", opName, requestID)
	}
	return ""
}

func joinRequestExpired(req *internalpb.PendingJoinRequest) bool {
	return time.Now().Unix() >= req.GetExpiresAtUnix()
}

// applyJoinRequestCommand mirrors applyNetworkCommand/applyJailCommand
// exactly (server.go/jail.go's own identical per-type Apply wrappers),
// unmarshaling raftd's ApplyResponse.Result as a PendingJoinRequest.
func (s *Server) applyJoinRequestCommand(ctx context.Context, cmd *internalpb.Command, timeoutMs uint32) (req *internalpb.PendingJoinRequest, appErr, leaderHint string) {
	timeout := defaultApplyTimeout
	if timeoutMs > 0 {
		timeout = time.Duration(timeoutMs) * time.Millisecond
	}

	payload, err := proto.Marshal(cmd)
	if err != nil {
		return nil, err.Error(), ""
	}

	resp, err := s.raft.Apply(ctx, payload, timeout)
	if err != nil {
		return nil, err.Error(), ""
	}
	if resp.GetError() != "" {
		return nil, resp.GetError(), resp.GetLeaderHint()
	}

	req = &internalpb.PendingJoinRequest{}
	if err := proto.Unmarshal(resp.GetResult(), req); err != nil {
		return nil, err.Error(), ""
	}
	return req, "", ""
}

// RequestJoinColony implements rpcpb.ManagerServiceServer - see this
// file's own doc comment and ADR-0083. Deliberately exempted from
// checkAuth entirely (internal/manager/auth.go) since the caller has no
// Colony API key yet by definition.
//
// target_address (ADR-0092) is checked first: when set, this call is
// the JOINING Comb's own managerd asking the operator-named existing
// Colony member to record the request on ITS OWN raft instead - this
// managerd never raft-Applies anything itself in that case, it's purely
// a dial-and-relay. Empty target_address preserves the original
// ADR-0083 behavior below (record directly on whichever managerd
// receives the call).
func (s *Server) RequestJoinColony(ctx context.Context, req *rpcpb.RequestJoinColonyRequest) (*rpcpb.RequestJoinColonyResponse, error) {
	if req.GetNodeId() == "" || req.GetRaftBindAddress() == "" {
		return &rpcpb.RequestJoinColonyResponse{Error: "node_id and raft_bind_address must both be set"}, nil
	}
	// This RPC is unauthenticated, and every accepted call becomes a record
	// replicated to every voter. Reject malformed or oversized fields here so
	// they never reach the Raft log, whether this call records the request or
	// only relays it to another Colony member.
	if err := raftnode.ValidateJoinRequestFields(req.GetNodeId(), req.GetRaftBindAddress(), req.GetTlsCertFingerprint()); err != nil {
		return &rpcpb.RequestJoinColonyResponse{Error: err.Error()}, nil
	}
	// ADR-0147 Part 4: the Colony's normal state is CLOSED. A request
	// arriving at a closed Colony is refused with a message naming the
	// fix, which inverts the pre-ADR-0147 default where this call was
	// accepted at any moment by any member.
	//
	// This is checked before the pending-count cap below because it is
	// the check the operator is most likely to be able to act on, and
	// because on a closed Colony the cap is not the problem: a cap
	// refusal says "an Admin must approve, reject, or purge some",
	// which is a different instruction from the one that applies here.
	//
	// Only the local-recording path is gated. A request carrying
	// target_address is this managerd relaying on a joining Comb's
	// behalf, and the member that actually records it gates the same
	// call on its own authoritative view - gating here as well would
	// mean a follower could refuse on a stale window while the leader
	// would have accepted, and the joiner would be told the Colony is
	// closed when it is open.
	if req.GetTargetAddress() == "" {
		if _, live := s.liveColonyJoinWindow(ctx); !live {
			return &rpcpb.RequestJoinColonyResponse{Error: fmt.Sprintf(
				"refusing to record this join request: %s", colonyWindowClosedRefusal)}, nil
		}
	}
	if req.GetTargetAddress() == "" && s.raft != nil {
		if pending, err := s.raft.ListPendingJoinRequestsLocal(ctx); err == nil && len(pending.GetRequests()) >= raftnode.MaxActionableJoinRequests {
			return &rpcpb.RequestJoinColonyResponse{Error: fmt.Sprintf("too many join requests are pending (%d); an Admin must approve, reject, or purge some before more can be accepted", len(pending.GetRequests()))}, nil
		}
	}
	// ADR-0106: a node_id that already names an existing raft voter is
	// never accepted as a new join - it's either an accidental collision
	// with a different physical Comb, or the same Comb trying to report
	// a corrected address, and this call has no way to tell which.
	// UpdateVoterAddress is the only supported path for the latter.
	// target_address forwarding (below) skips this check here since the
	// named remote member performs the identical check on its own,
	// authoritative view of Colony membership.
	if req.GetTargetAddress() == "" {
		if existing, err := s.existingVoter(ctx, req.GetNodeId()); err == nil && existing != nil {
			return &rpcpb.RequestJoinColonyResponse{Error: fmt.Sprintf(
				"node_id %q is already a Colony voter at address %q; use UpdateVoterAddress to correct an existing member's address instead of requesting a new join",
				req.GetNodeId(), existing.GetAddress())}, nil
		}
	}
	// The joiner's own raft log state, gathered once here and used for both
	// the forwarded request and the locally-recorded record, so the two
	// paths cannot disagree about what the joiner claimed.
	//
	// Three cases, in this order:
	//
	//  1. The caller set it explicitly. Trusted as-is - this is what lets a
	//     test drive a real approval against a real single-node raft.
	//  2. target_address is set, meaning this managerd is being called by
	//     the JOINING Comb's own frontend and so IS the joiner. Its local
	//     raftd's last_log_index is then the authoritative number. This is
	//     the normal path - the joining Comb's own Machine page always
	//     names a target member.
	//  3. Neither. The legacy "record directly on whichever managerd
	//     receives the call" shape, where this managerd may be an existing
	//     Colony MEMBER rather than the joiner. Its own log index would be
	//     evidence about the wrong machine entirely, so nothing is derived
	//     and the record stays unobserved, which ApproveJoinRequest
	//     refuses. A fabricated zero would be worse than no evidence.
	logState := s.localJoinerLogState(ctx)
	if req.GetJoinerLogStateObserved() || req.GetTargetAddress() == "" {
		logState.Observed, logState.LastLogIndex = req.GetJoinerLogStateObserved(), req.GetJoinerLastLogIndex()
	}
	// ADR-0147 Part 2: the FIRST CODE is the joiner's own, generated
	// locally before it dialed anything, and it is required. Generating
	// one here instead would restore the pre-ADR-0147 shape exactly -
	// both operators reading a value out of the same server's state -
	// and the whole two-way flow exists to stop that being what the
	// check means.
	if req.GetIntroductionCode() == "" {
		return &rpcpb.RequestJoinColonyResponse{Error: "introduction_code is required. It is the 6-digit value the JOINING Comb generated for itself before dialing - run `apiaryctl join-introduce`, which prints it. This Colony does not generate it: a code both sides could read out of the same server is not the check this flow is for"}, nil
	}
	// The joiner's real fingerprints, resolved ONCE, here, BEFORE any
	// forwarding and before the local-record path is chosen.
	//
	// This is the fix the ADR calls out as the one that matters most and
	// that was previously wrong: the local-record path used to read the
	// caller's field directly and only the forward path filled anything
	// in, so a request recorded here carried whatever the caller sent -
	// usually nothing. Under Part 2 the operator compares a value off
	// the joiner's own screen against what the request carried, and a
	// request that carried nothing is compared against nothing.
	//
	// An empty list is refused rather than recorded. A request with no
	// fingerprints can never be approved, so recording one only produces
	// a row certain to be refused later; saying so here is at the moment
	// the operator can still go and fix the joining Comb.
	advertisedFingerprints := req.GetAdvertisedFingerprints()
	if len(advertisedFingerprints) == 0 {
		own, err := s.localAdvertisedFingerprints()
		if err != nil {
			return &rpcpb.RequestJoinColonyResponse{Error: fmt.Sprintf("refusing to record this join request: %v", err)}, nil
		}
		advertisedFingerprints = own
	}
	if target := req.GetTargetAddress(); target != "" {
		if s.peers == nil {
			return &rpcpb.RequestJoinColonyResponse{Error: "no peer forwarding is configured on this node; cannot reach the named Colony member"}, nil
		}
		if err := s.checkTargetAddressAllowed(target); err != nil {
			return &rpcpb.RequestJoinColonyResponse{Error: err.Error()}, nil
		}
		// Unauthenticated dial (ADR-0096): target is caller-supplied and
		// this RPC is itself deliberately never authenticated, so it must
		// never be dialed with this node's own shared -peer-api-key
		// attached - see PeerForwarder's own doc comment. Bounded by
		// defaultUnauthenticatedForwardTimeout regardless of the caller's
		// own context, so an unreachable target can't hang this goroutine
		// indefinitely (2026-09-12 audit finding).
		fctx, cancel := context.WithTimeout(ctx, defaultUnauthenticatedForwardTimeout)
		defer cancel()
		// tls_cert_fingerprint (ADR-0113): fill it in from this managerd's
		// own local tls_cert when the caller didn't already supply one -
		// the normal case, since the joining Comb's own frontend submits
		// this call with only node_id/raft_bind_address/target_address set.
		// A caller (or a test) that already set it explicitly is trusted as-is.
		fingerprint := req.GetTlsCertFingerprint()
		if fingerprint == "" {
			fingerprint = s.localTLSCertFingerprint()
		}
		resp, err := s.peers.RequestJoinColonyUnauthenticated(fctx, target, &rpcpb.RequestJoinColonyRequest{
			NodeId: req.GetNodeId(), RaftBindAddress: req.GetRaftBindAddress(), TimeoutMs: req.GetTimeoutMs(),
			TlsCertFingerprint:     fingerprint,
			JoinerLogStateObserved: logState.Observed,
			JoinerLastLogIndex:     logState.LastLogIndex,
			// ADR-0147 Part 2: the first code and the REAL fingerprints
			// travel with the forward. The fingerprints are the ones
			// resolved above from this Comb's own certificate files, not
			// the caller's field, and the leader compares the operator's
			// paste against them.
			IntroductionCode:       req.GetIntroductionCode(),
			AdvertisedFingerprints: advertisedFingerprints,
		})
		if err != nil {
			return &rpcpb.RequestJoinColonyResponse{Error: fmt.Sprintf("reaching %s: %v", target, err)}, nil
		}
		return resp, nil
	}
	requestID, err := generateJoinRequestID()
	if err != nil {
		return &rpcpb.RequestJoinColonyResponse{Error: fmt.Sprintf("generating request id: %v", err)}, nil
	}
	// Refused here only on a KNOWN-BAD reading, not on an absent one, so the
	// operator gets the reason at the moment they can still act on it. An
	// unobserved reading is deliberately still recorded: the legacy direct
	// path below can never produce evidence, and refusing to record there
	// would change that path's behaviour for no safety gain, since
	// ApproveJoinRequest refuses anything that is not Allow regardless.
	// What that path now gets instead is a request the Admin can see and a
	// preflight that explains why it cannot be approved.
	if report := guardrail.EvaluateJoinLogState(logState); report.Verdict == guardrail.Block {
		return &rpcpb.RequestJoinColonyResponse{Error: fmt.Sprintf(
			"refusing to record this join request: %s - wipe this Comb's raft state and request again",
			report.Findings[0].Detail)}, nil
	}
	// The FIRST CODE is the joiner's own value, carried through
	// unchanged. The target does not generate it and does not compare
	// it against its own; it compares the value its operator pasted
	// against the value on this line.
	code := req.GetIntroductionCode()
	_ = err

	// Read the window a second time for the epoch to record, rather
	// than reusing the liveness read above: the two answers are then
	// independent observations, and a window that lapsed between them
	// yields a request whose epoch the approval-time gate will reject,
	// instead of a request stamped with an epoch that never existed.
	epoch := int64(0)
	if w, live := s.liveColonyJoinWindow(ctx); live {
		epoch = w.GetOpenedAtUnix()
	}

	now := time.Now()
	cmd := &internalpb.Command{
		Op: &internalpb.Command_CreatePendingJoinRequest{
			CreatePendingJoinRequest: &internalpb.CreatePendingJoinRequest{
				Request: &internalpb.PendingJoinRequest{
					RequestId:       requestID,
					NodeId:          req.GetNodeId(),
					RaftBindAddress: req.GetRaftBindAddress(),
					Code:            code,
					// ADR-0147 Part 2: a request is INTRODUCED the
					// moment it is recorded, and it carries the
					// fingerprints the operator is about to compare
					// against. Both are set on THIS path as well as on
					// the forward above - the asymmetry the ADR names
					// as the defect is exactly this line missing.
					Stage:                  internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED,
					AdvertisedFingerprints: advertisedFingerprints,
					RequestedAtUnix:        now.Unix(),
					ExpiresAtUnix:          now.Add(defaultJoinRequestTTL).Unix(),
					Status:                 internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING,
					TlsCertFingerprint:     req.GetTlsCertFingerprint(),
					JoinerLogStateObserved: logState.Observed,
					JoinerLastLogIndex:     logState.LastLogIndex,
					// ADR-0147 Part 4: the window's epoch, recorded at
					// creation so a later reopen cannot finish this
					// request. The live-window check above guarantees
					// there IS a live window here, so this is a
					// non-zero opened_at_unix in practice.
					WindowOpenedAtUnix: epoch,
				},
			},
		},
	}
	result, appErr, leaderHint := s.applyJoinRequestCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.RequestJoinColony(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.RequestJoinColonyResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.RequestJoinColonyResponse{RequestId: result.GetRequestId(), Code: result.GetCode()}, nil
}

// GetJoinRequestStatus implements rpcpb.ManagerServiceServer - a local,
// non-leader-restricted read (ADR-0083: a joining Comb polls whichever
// specific existing member it originally contacted), also exempted
// from checkAuth entirely for the same reason RequestJoinColony is.
//
// target_address (ADR-0092) mirrors RequestJoinColony's own field: when
// set, this poll is forwarded to that address instead of reading this
// node's own local raft - needed because RequestJoinColony's own
// target_address means the request itself lives on a REMOTE Colony
// member, not here.
func (s *Server) GetJoinRequestStatus(ctx context.Context, req *rpcpb.GetJoinRequestStatusRequest) (*rpcpb.GetJoinRequestStatusResponse, error) {
	if target := req.GetTargetAddress(); target != "" {
		if s.peers == nil {
			return &rpcpb.GetJoinRequestStatusResponse{Error: "no peer forwarding is configured on this node; cannot reach the named Colony member"}, nil
		}
		if err := s.checkTargetAddressAllowed(target); err != nil {
			return &rpcpb.GetJoinRequestStatusResponse{Error: err.Error()}, nil
		}
		// Unauthenticated dial (ADR-0096), bounded (2026-09-12 audit
		// finding) - see RequestJoinColony's own identical comment above.
		fctx, cancel := context.WithTimeout(ctx, defaultUnauthenticatedForwardTimeout)
		defer cancel()
		resp, err := s.peers.GetJoinRequestStatusUnauthenticated(fctx, target, req.GetRequestId())
		if err != nil {
			return &rpcpb.GetJoinRequestStatusResponse{Error: fmt.Sprintf("reaching %s: %v", target, err)}, nil
		}
		return resp, nil
	}
	resp, err := s.raft.GetPendingJoinRequestLocal(ctx, req.GetRequestId())
	if err != nil {
		return &rpcpb.GetJoinRequestStatusResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		return &rpcpb.GetJoinRequestStatusResponse{Error: resp.GetError()}, nil
	}
	pending := resp.GetRequest()
	// ADR-0147 Part 2: the SECOND PIN is released here and only here,
	// and it is released on TWO values rather than the request_id alone.
	//
	// The ADR specified request_id on its own, which is defensible -
	// 64 bits of crypto/rand is unguessable - but this poll is
	// unauthenticated and reachable by anyone who can reach the port,
	// and reaching the port is not the hurdle Part 2 exists to raise.
	// Requiring the first code as well closes the guessing path for the
	// cost of one field, and it is nearly free: the first code is the
	// REQUESTER's own, single-use, already replicated, already capped at
	// 5 attempts, and already CLEARED the moment it was accepted. The
	// value that authorizes the release is therefore the same value
	// whose absence from the record is what the request itself
	// demonstrates.
	//
	// Compared in constant time, through the same helper the FSM uses,
	// so there is one comparison in the tree and not a second one that
	// could be less careful.
	out := &rpcpb.GetJoinRequestStatusResponse{Request: fromInternalPendingJoinRequest(pending)}
	if pending.GetStage() != internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED || pending.GetSecondPin() == "" {
		return out, nil
	}
	if secondPinExpired(pending) {
		// Reported, not released. The requesting Comb's operator needs to
		// know the PIN they are watching for is never going to arrive,
		// rather than watching a poll that silently returns nothing
		// until the request's own 15 minutes run out.
		out.Error = fmt.Sprintf("the second PIN for this request expired at %s and has not been reissued. The target Colony's Admin has to reissue it on their own page (`apiaryctl` on the joiner keeps polling; nothing has failed)",
			time.Unix(pending.GetSecondPinExpiresAtUnix(), 0).UTC().Format(time.RFC3339))
		return out, nil
	}
	if !raftnode.ConstantTimeValueEqual(hashJoinValue(req.GetIntroductionCode()), pending.GetAcceptedIntroductionCodeSha256()) || pending.GetAcceptedIntroductionCodeSha256() == "" {
		return out, nil
	}
	out.SecondPin = pending.GetSecondPin()
	out.SecondPinExpiresAtUnix = pending.GetSecondPinExpiresAtUnix()
	return out, nil
}

// hashJoinValue is the SHA-256 of one human-entered value, hex-encoded.
//
// A digest rather than the value, so the post-acceptance poll can be
// checked against something without the accepted first code sitting in
// replicated state - which is the whole point of clearing it there.
// Compared through raftnode.ConstantTimeValueEqual, so the comparison
// discipline is the same one the FSM applies to all three values.
func hashJoinValue(v string) string {
	return raftnode.HashJoinValue(v)
}

// ListJoinRequests implements rpcpb.ManagerServiceServer - Admin-only
// (internal/manager/auth.go), a local, non-leader-restricted read of
// every currently-Pending, not-yet-expired request across the whole
// Colony (raft-replicated, so this is the same list regardless of
// which member's UI it's viewed from).
func (s *Server) ListJoinRequests(ctx context.Context, _ *rpcpb.ListJoinRequestsRequest) (*rpcpb.ListJoinRequestsResponse, error) {
	resp, err := s.raft.ListPendingJoinRequestsLocal(ctx)
	if err != nil {
		return &rpcpb.ListJoinRequestsResponse{Error: err.Error()}, nil
	}
	requests := make([]*rpcpb.PendingJoinRequest, 0, len(resp.GetRequests()))
	for _, r := range resp.GetRequests() {
		requests = append(requests, fromInternalPendingJoinRequest(r))
	}
	return &rpcpb.ListJoinRequestsResponse{Requests: requests}, nil
}

// ApproveJoinRequest implements rpcpb.ManagerServiceServer - Admin-only.
// The handler that actually runs on the leader calls AddVoter against
// this managerd's own local raftd (RaftClient.AddVoter) before marking
// the request Approved - if AddVoter itself fails, the request is left
// Pending (not marked Approved) so the Admin can retry, rather than
// silently recording success for a membership change that didn't
// actually happen.
//
// Before AddVoter, a pre-approval reachability check (ADR-0097) dials
// pending.raft_bind_address - approving a join before the joiner is
// actually reachable has stranded this project's own cluster more than
// once (see SHARED.md): AddVoter commits under whatever quorum exists
// the instant it's called, and reversing it needs agreement from every
// current voter, including the one that was never reachable. The only
// recovery available in this codebase is a full raft state wipe. This
// check can't guarantee the joiner is fully healthy (it's a plain TCP
// dial, not a raft handshake), but it catches exactly the failure mode
// that has actually happened here: clicking Approve before the joining
// Comb's raftd was actually up.
func (s *Server) ApproveJoinRequest(ctx context.Context, req *rpcpb.ApproveJoinRequestRequest) (*rpcpb.ApproveJoinRequestResponse, error) {
	// confirm_phrase (ADR-0113) is checked first, before anything else in
	// this function - including the leadership check/forward below - so a
	// wrong or missing phrase never causes so much as a forwarded RPC to
	// a peer, exactly the same "no action at all" posture
	// ConvertStandaloneToJoiner's own confirm_phrase check already
	// established (convertjoiner.go).
	if req.GetConfirmPhrase() != approveJoinRequestConfirmPhrase {
		return &rpcpb.ApproveJoinRequestResponse{Error: fmt.Sprintf(
			"confirm_phrase %q does not match the required confirmation phrase %q - nothing was done",
			req.GetConfirmPhrase(), approveJoinRequestConfirmPhrase)}, nil
	}

	// ADR-0147 Part 4: the live-window check comes FIRST, before the
	// PINs, before the authorization file, before the reachability
	// dial. It is the cheapest check available and the one an operator
	// most needs explained, and it is evaluated here on the leader
	// rather than read when a form was rendered - a window that expires
	// while an approval form sits in a browser must not be approvable on
	// the strength of when the form was drawn.
	//
	// It runs after the confirm_phrase check above, which is
	// deliberate and matches ADR-0113's own posture: a wrong or missing
	// phrase must cause no action at all, not even a forwarded RPC, so
	// the phrase stays first and the window is the first thing checked
	// once the call is genuinely an approval attempt.
	if err := s.requireLiveColonyJoinWindow(ctx, req.GetRequestId()); err != nil {
		return &rpcpb.ApproveJoinRequestResponse{Error: err.Error()}, nil
	}

	// ADR-0147 Part 3: this Comb stamps its OWN identity onto the call
	// before it can be forwarded, and only if nothing has stamped it
	// already. That "only if empty" is the whole mechanism: a forwarded
	// approval must still name the Comb the operator actually presented
	// it to, so a second forward must not overwrite the first Comb's
	// stamp with this one's.
	//
	// The stamp is this managerd's own configuration, never anything
	// the caller sent. Part 3's two-person rule counts DISTINCT Combs,
	// and a caller-supplied value would make that count a claim about
	// the caller rather than a fact about the Colony.
	if req.GetApprovingNodeId() == "" {
		req.ApprovingNodeId = s.localNodeID()
	}

	// Check leadership BEFORE running the local reachability dial below -
	// only the leader's own network vantage point actually matters, since
	// only the leader calls AddVoter. A follower with its own, different
	// network path to the joiner's address could otherwise reject (or
	// wrongly approve) based on the wrong node's view (ADR-0103). The
	// existing post-AddVoter-failure forward further down stays as a
	// fallback for the race window where leadership changes between this
	// check and the AddVoter call itself.
	if status, statusErr := s.raft.Status(ctx); statusErr == nil && !status.GetIsLeader() {
		if hint := currentLeaderRaftAddress(status); hint != "" && s.peers != nil {
			if fwd, ferr := s.peers.ApproveJoinRequest(ctx, s.peerManagerdAddr(hint), req); ferr == nil {
				return fwd, nil
			}
		}
	}

	getResp, err := s.raft.GetPendingJoinRequestLocal(ctx, req.GetRequestId())
	if err != nil {
		return &rpcpb.ApproveJoinRequestResponse{Error: err.Error()}, nil
	}
	if getResp.GetError() != "" {
		return &rpcpb.ApproveJoinRequestResponse{Error: getResp.GetError()}, nil
	}
	pending := getResp.GetRequest()
	if joinRequestExpired(pending) {
		return &rpcpb.ApproveJoinRequestResponse{Error: fmt.Sprintf("ApprovePendingJoinRequest: request_id %q has expired", req.GetRequestId())}, nil
	}

	// ADR-0147 Part 2: the second PIN gate is a PRECONDITION check
	// here, and it is deliberately NOT the point at which the PIN is
	// spent - see the ConsumeJoinSecondPin submission immediately
	// before AddVoter for why.
	//
	// This check refuses the call before anything downstream runs, and
	// it costs no attempt, because "this request cannot be approved
	// yet" and "this request is not the one you think it is" are
	// different answers. A request that has not been introduced, or
	// whose PIN has lapsed, is not a failed guess; it is a request that
	// has not reached the step the operator is at. Counting that as an
	// attempt would let an operator burn five tries by clicking Approve
	// in the wrong order.
	//
	// Every one of these is evaluated on the LEADER at the moment of
	// the approval, never when a form was drawn, so a PIN that expired
	// or was reissued while a browser sat open is refused rather than
	// spent.
	if msg := joinRequestNotActionable(pending, "ApprovePendingJoinRequest"); msg != "" {
		return &rpcpb.ApproveJoinRequestResponse{Error: msg}, nil
	}
	if req.GetSecondPin() == "" {
		return &rpcpb.ApproveJoinRequestResponse{Error: "second_pin is required. It is the 8-digit value this Colony generated and showed on the REQUESTING Comb's own screen; paste it here alongside the confirmation phrase. The confirmation phrase alone is a constant anyone can read out of this repository, and it is not the whole of the gate"}, nil
	}
	// ADR-0147 Part 3, in the ADR's own order: the live join window
	// (checked above, before this) is the cheapest gate and the one an
	// operator most needs explained; the authorization entry is next;
	// the two-person rule after that; and Part 2's reachability,
	// not-already-a-voter and join-log checks run last, as they always
	// did.
	//
	// Every one of these is checked on the LEADER, here, at the moment
	// of the approval - never when a form was rendered, and never on
	// the member that happened to receive the click. An authorization
	// that expires while an approval sits in a browser must not be
	// spendable on the strength of when the form was drawn, and a
	// leader change between the click and here must not change which
	// file is read.
	authorization, err := s.authorizePendingJoin(ctx, pending, req)
	if err != nil {
		return &rpcpb.ApproveJoinRequestResponse{Error: err.Error()}, nil
	}
	if authorization.awaitingSecond {
		return &rpcpb.ApproveJoinRequestResponse{
			Request:                     fromInternalPendingJoinRequest(authorization.record),
			AwaitingSecondAuthorization: true,
			SecondAuthorizationRequired: authorization.awaitingMessage,
		}, nil
	}

	if report := s.evaluateJoinReachability(ctx, pending.GetRaftBindAddress()); report.Verdict != guardrail.Allow {
		return &rpcpb.ApproveJoinRequestResponse{Error: fmt.Sprintf(
			"refusing to approve: %s - approving an unreachable node strands the cluster and can only be recovered by wiping raft state, so this is refused rather than attempted",
			report.Findings[0].Detail)}, nil
	}

	// Re-checked here, not just at RequestJoinColony time (ADR-0106): the
	// colony's membership can change between a request being created and
	// an Admin approving it (including another request for the same
	// node_id being approved in that window), and AddVoter is the actual
	// point of no return this whole guardrail exists to protect.
	if existing, err := s.existingVoter(ctx, pending.GetNodeId()); err == nil && existing != nil {
		return &rpcpb.ApproveJoinRequestResponse{Error: fmt.Sprintf(
			"refusing to approve: node_id %q is already a Colony voter at address %q; use UpdateVoterAddress instead",
			pending.GetNodeId(), existing.GetAddress())}, nil
	}

	// The join-log guardrail (guardrail.EvaluateJoinLogState), applied to
	// the EVIDENCE RECORDED ON THE PENDING REQUEST rather than to anything
	// read here. This runs on the approving member, whose own raft knows
	// nothing about the joiner's log, and it is deliberately the last check
	// before AddVoter.
	//
	// Two things it is worth being precise about.
	//
	// The evidence is a snapshot taken when the request was created, not a
	// lock held across the approval, so there is a real race here: a joiner
	// could in principle acquire a log between the two. That race is
	// accepted rather than closed, because closing it needs the joiner to
	// hold some token across the whole window, which is a protocol this
	// codebase does not have. What bounds it in practice is that the only
	// way the joiner gains a non-empty log is by being made a voter, which
	// is the very call this check precedes - so on the path that matters,
	// the log cannot change underneath the check. Wiping the joiner's
	// state in the meantime only makes the join safer, and the operator
	// would have to deliberately re-image a Comb mid-approval to exploit
	// the gap.
	//
	// Unobserved is refused, not allowed. A pending request created by an
	// older managerd, or one recorded through the legacy direct path where
	// this member is not the joiner, carries no evidence at all, and
	// approving it would be performing the irreversible action on the
	// strength of an absence.
	if report := guardrail.EvaluateJoinLogState(guardrail.JoinerLogStateFact{
		NodeID:       pending.GetNodeId(),
		Observed:     pending.GetJoinerLogStateObserved(),
		LastLogIndex: pending.GetJoinerLastLogIndex(),
	}); report.Verdict != guardrail.Allow {
		return &rpcpb.ApproveJoinRequestResponse{Error: fmt.Sprintf(
			"refusing to approve: %s", report.Findings[0].Detail)}, nil
	}

	// The PIN is SPENT here, by its own raft command, and this is the
	// last thing that happens before AddVoter. Two placement decisions
	// are in it and both are deliberate.
	//
	// It is before AddVoter because ADR-0147 Part 2 says so: a failed
	// AddVoter must not leave a live PIN behind on a request that has
	// already been acted on once. Re-arming is ReissueJoinSecondPin,
	// which is visible in the log and capped at 2.
	//
	// It is AFTER every non-mutating gate above - reachability, the
	// already-a-voter check, the join-log guardrail, and the Part 3
	// authorization read. Those can all fail on facts an operator fixes
	// by fixing a Comb and clicking again, and none of them is a wrong
	// guess. Spending the PIN before them would turn the most common
	// recoverable failure in this flow (a joiner whose raftd is not up
	// yet, which has stranded this project's own cluster more than once)
	// into one that also burns a reissue from a cap of two. The
	// ADR's rule is "cleared before AddVoter"; this is before AddVoter.
	if _, appErr, _ := s.consumeJoinSecondPin(ctx, req.GetRequestId(), req.GetSecondPin(), 0); appErr != "" {
		return &rpcpb.ApproveJoinRequestResponse{Error: appErr}, nil
	}

	timeout := defaultApplyTimeout
	if req.GetTimeoutMs() > 0 {
		timeout = time.Duration(req.GetTimeoutMs()) * time.Millisecond
	}
	addResp, err := s.raft.AddVoter(ctx, pending.GetNodeId(), pending.GetRaftBindAddress(), 0, timeout)
	if err != nil {
		return &rpcpb.ApproveJoinRequestResponse{Error: fmt.Sprintf("adding %q as a raft voter: %v", pending.GetNodeId(), err)}, nil
	}
	if addResp.GetError() != "" {
		if addResp.GetLeaderHint() != "" && s.peers != nil {
			if fwd, ferr := s.peers.ApproveJoinRequest(ctx, s.peerManagerdAddr(addResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.ApproveJoinRequestResponse{Error: addResp.GetError(), LeaderHint: addResp.GetLeaderHint()}, nil
	}

	// authorization.id is set by the only path that reaches AddVoter:
	// authorizePendingJoin refuses every other case, and names the exact
	// entry that authorized this approval so the raft log - not a local
	// file - is the record of its use.
	//
	// It is written on the SAME command that approves rather than by a
	// second one afterwards, because "this join was approved" and "this
	// entry was spent" have to be a single atomic fact. A log entry that
	// approved without recording the spend would leave single use
	// resting on a file only the leader reads, which is precisely the
	// gap ADR-0147 Part 3 exists to close.
	cmd := &internalpb.Command{
		Op: &internalpb.Command_ApprovePendingJoinRequest{
			ApprovePendingJoinRequest: &internalpb.ApprovePendingJoinRequest{
				RequestId:       req.GetRequestId(),
				AuthorizationId: authorization.id,
				ConsumedAtUnix:  authorization.consumedAt,
			},
		},
	}
	result, appErr, leaderHint := s.applyJoinRequestCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.ApproveJoinRequest(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.ApproveJoinRequestResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.ApproveJoinRequestResponse{Request: fromInternalPendingJoinRequest(result)}, nil
}

// consumeJoinSecondPin spends a request's second PIN, through the
// replicated command that both compares it and clears it.
//
// The comparison lives in the FSM for the same reason the first code's
// does: the PIN is already in replicated state, so every replica can
// reach the identical answer, and a manager-side check followed by a
// "yes it matched" command would put the deciding value behind a byte
// the FSM cannot check.
//
// A failure here is NOT free to retry: the attempt counter advanced in
// the same entry, and the fifth failure is terminal FAILED. That is the
// point of the cap - an approval form that can be submitted five times
// is a six-digit-or-eight-digit oracle on this Colony's own pending
// list.
func (s *Server) consumeJoinSecondPin(ctx context.Context, requestID, pin string, timeoutMs uint32) (*internalpb.PendingJoinRequest, string, string) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_ConsumeJoinSecondPin{
			ConsumeJoinSecondPin: &internalpb.ConsumeJoinSecondPin{
				RequestId: requestID,
				SecondPin: pin,
				AtUnix:    time.Now().Unix(),
			},
		},
	}
	return s.applyJoinRequestCommand(ctx, cmd, timeoutMs)
}

// UpdateVoterAddress implements rpcpb.ManagerServiceServer (ADR-0106) -
// Admin-only. The first-class replacement for resubmitting
// RequestJoinColony/ApproveJoinRequest against an already-voting
// node_id (the ad hoc technique used during the 2026-09-17 brood/drone
// incident, see SHARED.md) now that those two reject that case
// outright. Requires node_id to already be a known voter - the mirror
// image of ApproveJoinRequest's own new "must not already exist" check
// - so this can never be used to sneak in a brand-new member without
// the mutual-authorization join flow.
func (s *Server) UpdateVoterAddress(ctx context.Context, req *rpcpb.UpdateVoterAddressRequest) (*rpcpb.UpdateVoterAddressResponse, error) {
	if req.GetNodeId() == "" || req.GetNewRaftBindAddress() == "" {
		return &rpcpb.UpdateVoterAddressResponse{Error: "node_id and new_raft_bind_address must both be set"}, nil
	}
	// Deliberately no validateJoinerRaftBind-style format check here -
	// that helper exists for ConvertStandaloneToJoiner (ADR-0105), where
	// a Comb validates its OWN bind address and a loopback value is
	// always a self-inflicted misconfiguration. This RPC plays
	// ApproveJoinRequest's role instead (approving ANOTHER Comb's
	// claimed address), which has never format-checked addresses beyond
	// the reachability guardrail below - a real deployment's addresses
	// are never loopback in practice, and this project's own integration
	// test harness legitimately uses loopback throughout. Adding a
	// stricter check here than ApproveJoinRequest already applies would
	// make the two RPCs inconsistent for no safety benefit.

	// Same leadership-first ordering as ApproveJoinRequest, and for the
	// same reason: only the leader's own network vantage point matters,
	// since only the leader can actually call AddVoter.
	if status, statusErr := s.raft.Status(ctx); statusErr == nil && !status.GetIsLeader() {
		if hint := currentLeaderRaftAddress(status); hint != "" && s.peers != nil {
			if fwd, ferr := s.peers.UpdateVoterAddress(ctx, s.peerManagerdAddr(hint), req); ferr == nil {
				return fwd, nil
			}
		}
	}

	existing, err := s.existingVoter(ctx, req.GetNodeId())
	if err != nil {
		return &rpcpb.UpdateVoterAddressResponse{Error: fmt.Sprintf("reading current Colony membership: %v", err)}, nil
	}
	if existing == nil {
		return &rpcpb.UpdateVoterAddressResponse{Error: fmt.Sprintf(
			"node_id %q is not a current Colony member; use RequestJoinColony/ApproveJoinRequest to add a new member instead", req.GetNodeId())}, nil
	}

	// Same reachability guardrail as ApproveJoinRequest, applied to the
	// NEW address - an unreachable new address strands the cluster
	// exactly as an unreachable new joiner does (ADR-0097), and this is
	// the only other call site that ever changes a voter's address.
	if report := s.evaluateJoinReachability(ctx, req.GetNewRaftBindAddress()); report.Verdict != guardrail.Allow {
		return &rpcpb.UpdateVoterAddressResponse{Error: fmt.Sprintf(
			"refusing to update: %s - approving an unreachable address strands the cluster and can only be recovered by wiping raft state, so this is refused rather than attempted",
			report.Findings[0].Detail)}, nil
	}

	timeout := defaultApplyTimeout
	if req.GetTimeoutMs() > 0 {
		timeout = time.Duration(req.GetTimeoutMs()) * time.Millisecond
	}
	addResp, err := s.raft.AddVoter(ctx, req.GetNodeId(), req.GetNewRaftBindAddress(), 0, timeout)
	if err != nil {
		return &rpcpb.UpdateVoterAddressResponse{Error: fmt.Sprintf("updating %q's address: %v", req.GetNodeId(), err)}, nil
	}
	if addResp.GetError() != "" {
		if addResp.GetLeaderHint() != "" && s.peers != nil {
			if fwd, ferr := s.peers.UpdateVoterAddress(ctx, s.peerManagerdAddr(addResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.UpdateVoterAddressResponse{Error: addResp.GetError(), LeaderHint: addResp.GetLeaderHint()}, nil
	}
	return &rpcpb.UpdateVoterAddressResponse{}, nil
}

// PreflightApproveJoinRequest previews ApproveJoinRequest's own
// reachability gate without ever calling AddVoter (ADR-0103) - Admin-tier,
// not Viewer, since it makes managerd dial a caller-selected pending
// request's raft_bind_address, which a Viewer could otherwise use as a
// network reachability oracle. Applies the identical leadership-first
// reordering as ApproveJoinRequest, so the preview always reflects the
// same network vantage point the real approval would use, and calls the
// same evaluateJoinReachability helper so the two can never drift apart.
func (s *Server) PreflightApproveJoinRequest(ctx context.Context, req *rpcpb.PreflightApproveJoinRequestRequest) (*rpcpb.PreflightApproveJoinRequestResponse, error) {
	if status, statusErr := s.raft.Status(ctx); statusErr == nil && !status.GetIsLeader() {
		if hint := currentLeaderRaftAddress(status); hint != "" && s.peers != nil {
			if fwd, ferr := s.peers.PreflightApproveJoinRequest(ctx, s.peerManagerdAddr(hint), req); ferr == nil {
				return fwd, nil
			}
		}
	}

	getResp, err := s.raft.GetPendingJoinRequestLocal(ctx, req.GetRequestId())
	if err != nil {
		return &rpcpb.PreflightApproveJoinRequestResponse{Error: err.Error()}, nil
	}
	if getResp.GetError() != "" {
		return &rpcpb.PreflightApproveJoinRequestResponse{Error: getResp.GetError()}, nil
	}
	pending := getResp.GetRequest()

	// Both of ApproveJoinRequest's own pre-AddVoter checks, evaluated here
	// through the same two functions it uses, so the preview cannot drift
	// from the enforcement - which is the entire reason this RPC exists
	// (ADR-0103). Combined rather than reported separately, because the
	// question the UI asks is one question. Evaluated once, since
	// evaluateJoinReachability dials the joiner and this is a preview.
	report := guardrail.Combine(
		s.evaluateJoinReachability(ctx, pending.GetRaftBindAddress()),
		guardrail.EvaluateJoinLogState(guardrail.JoinerLogStateFact{
			NodeID:       pending.GetNodeId(),
			Observed:     pending.GetJoinerLogStateObserved(),
			LastLogIndex: pending.GetJoinerLastLogIndex(),
		}),
	)
	return &rpcpb.PreflightApproveJoinRequestResponse{
		Verdict:  string(report.Verdict),
		Findings: toRPCGuardrailFindings(report.Findings),
	}, nil
}

// RejectJoinRequest implements rpcpb.ManagerServiceServer - Admin-only,
// the symmetric decline action. Never touches raft membership at all -
// just marks the record Rejected.
func (s *Server) RejectJoinRequest(ctx context.Context, req *rpcpb.RejectJoinRequestRequest) (*rpcpb.RejectJoinRequestResponse, error) {
	if msg := s.expiredJoinRequestMessage(ctx, req.GetRequestId(), "RejectPendingJoinRequest"); msg != "" {
		return &rpcpb.RejectJoinRequestResponse{Error: msg}, nil
	}
	cmd := &internalpb.Command{
		Op: &internalpb.Command_RejectPendingJoinRequest{
			RejectPendingJoinRequest: &internalpb.RejectPendingJoinRequest{RequestId: req.GetRequestId()},
		},
	}
	result, appErr, leaderHint := s.applyJoinRequestCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.RejectJoinRequest(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.RejectJoinRequestResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.RejectJoinRequestResponse{Request: fromInternalPendingJoinRequest(result)}, nil
}

// CancelJoinRequest implements rpcpb.ManagerServiceServer - the
// requesting Comb's own self-service withdrawal of its still-pending
// request. Deliberately exempted from checkAuth entirely
// (internal/manager/auth.go), the same reason RequestJoinColony/
// GetJoinRequestStatus already are: the caller is this request's own
// creator, which by definition has no Colony API key yet. Knowledge of
// request_id is the only credential this needs, matching
// GetJoinRequestStatus's own already-established scoping.
//
// target_address (ADR-0092) mirrors RequestJoinColony/
// GetJoinRequestStatus's own field: when set, this cancel is forwarded
// there instead of acting on this node's own local raft, since the
// request itself may live on a remote Colony member.
func (s *Server) CancelJoinRequest(ctx context.Context, req *rpcpb.CancelJoinRequestRequest) (*rpcpb.CancelJoinRequestResponse, error) {
	if target := req.GetTargetAddress(); target != "" {
		if s.peers == nil {
			return &rpcpb.CancelJoinRequestResponse{Error: "no peer forwarding is configured on this node; cannot reach the named Colony member"}, nil
		}
		if err := s.checkTargetAddressAllowed(target); err != nil {
			return &rpcpb.CancelJoinRequestResponse{Error: err.Error()}, nil
		}
		// Unauthenticated dial (ADR-0096), bounded (2026-09-12 audit
		// finding) - see RequestJoinColony's own identical comment above.
		fctx, cancel := context.WithTimeout(ctx, defaultUnauthenticatedForwardTimeout)
		defer cancel()
		resp, err := s.peers.CancelJoinRequestUnauthenticated(fctx, target, &rpcpb.CancelJoinRequestRequest{RequestId: req.GetRequestId(), TimeoutMs: req.GetTimeoutMs()})
		if err != nil {
			return &rpcpb.CancelJoinRequestResponse{Error: fmt.Sprintf("reaching %s: %v", target, err)}, nil
		}
		return resp, nil
	}
	if msg := s.expiredJoinRequestMessage(ctx, req.GetRequestId(), "CancelPendingJoinRequest"); msg != "" {
		return &rpcpb.CancelJoinRequestResponse{Error: msg}, nil
	}
	cmd := &internalpb.Command{
		Op: &internalpb.Command_CancelPendingJoinRequest{
			CancelPendingJoinRequest: &internalpb.CancelPendingJoinRequest{RequestId: req.GetRequestId()},
		},
	}
	result, appErr, leaderHint := s.applyJoinRequestCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.CancelJoinRequest(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.CancelJoinRequestResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.CancelJoinRequestResponse{Request: fromInternalPendingJoinRequest(result)}, nil
}

// PurgeJoinRequest implements rpcpb.ManagerServiceServer - Admin-only,
// same tier as Approve/Reject/List. Removes the record outright
// regardless of its current status, for cleaning up a stale or
// erroneous entry that Approve/Reject/Cancel would otherwise keep
// forever. Idempotent, mirroring PurgeVM/PurgeJail's own posture - not
// an error if request_id is already gone.
func (s *Server) PurgeJoinRequest(ctx context.Context, req *rpcpb.PurgeJoinRequestRequest) (*rpcpb.PurgeJoinRequestResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_PurgeJoinRequest{
			PurgeJoinRequest: &internalpb.PurgeJoinRequest{RequestId: req.GetRequestId()},
		},
	}
	_, appErr, leaderHint := s.applyJoinRequestCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.PurgeJoinRequest(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.PurgeJoinRequestResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.PurgeJoinRequestResponse{}, nil
}
