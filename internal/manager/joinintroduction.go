// ADR-0147 Part 2 as managerd sees it: the two new Admin-side steps
// between a request being recorded and it being approvable, the
// trust-store writers, and the fingerprints the joiner advertises.
//
// THE JOINER SIDE IS apiaryctl, NOT A PAGE. The ADR's own sketch had a
// four-step "Requesting Comb's own Machine page" driving
// GetLocalJoinIdentity. That Comb is a headless FreeBSD server with no
// browser, so the page does not exist and this change does not build
// it. What replaces it is `apiaryctl join-introduce`, which PRINTS the
// first code and the fingerprints and NEVER submits anything to the
// target on the operator's behalf.
//
// That non-submission is the property the whole design protects, and
// it is why the CLI is not a convenience wrapper around the same calls
// a page would make. A page that auto-submitted the first code to the
// target would satisfy stage one by itself, which is exactly the
// attestation stage one exists to demand from a human: the target's
// operator has to have read the value off the joiner's own screen and
// typed it into the target's own page. The CLI therefore has no code
// path that calls VerifyJoinIntroduction, ApproveJoinRequest, or any
// other state-changing RPC on the target, and that is stated at every
// call site rather than left to be discovered.
//
// The third party to this file is the FIRST CODE, and the inversion is
// what makes the flow work. The joiner generates it, locally, before it
// dials anything; the target checks the value its operator pasted
// against what the request carried. What that proves is not "two
// people read the same six digits" - it is "the operator at the target
// read this value off the requesting Comb's own screen".

package manager

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostcert"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

// joinFirstCodeDigits and joinSecondPinDigits are the two rendering
// widths, and they are NOT the same value on purpose.
//
// The first code is a correlation value: its strength comes from a
// human transcribing it correctly, and a longer string makes the
// transcription worse. Six digits is unchanged from ADR-0083 for
// exactly that reason.
//
// The second PIN is the one whose guessing an attacker would want. It
// is copy-pasted rather than read aloud, so it gets more entropy for
// free, and it is the value that actually authorizes a membership
// change.
const (
	joinFirstCodeDigits  = 6
	joinSecondPinDigits  = 8
	joinCodeModulus      = 1000000  // 10^6, for the first code
	joinSecondPinModulus = 10000000 // 10^8, for the second PIN
)

// joinSecondPinTTL is the second PIN's own deadline. Shorter than
// defaultJoinRequestTTL (15 minutes) deliberately: the PIN is the
// value that actually authorizes something, and it only has to survive
// one copy-and-paste between two screens.
//
// Checked lazily, at read and approve time, against the wall clock -
// never inside the FSM, which must reach the identical decision on
// every replica and on every replay years later.
const joinSecondPinTTL = 5 * time.Minute

// generateJoinFirstCode is the joiner's 6-digit first code, from
// crypto/rand over 10^6, rendered zero-padded to exactly six digits.
//
// Zero-padding is not cosmetic: an unpadded 6-digit draw is a
// five-digit string a third of the time, and a value the operator
// transcribes has to have a fixed width or the transcription stops
// being exact.
func generateJoinFirstCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(joinCodeModulus))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", joinFirstCodeDigits, n.Int64()), nil
}

// generateJoinSecondPin is the TARGET's 8-digit second PIN. It is
// never generated on the joiner and never derived from the first code;
// it is an independent draw, because its whole job is to be a value
// the joiner cannot already know.
func generateJoinSecondPin() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(joinSecondPinModulus))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", joinSecondPinDigits, n.Int64()), nil
}

// advertisedCertificate is one certificate this Comb actually HOLDS,
// read from the certificate FILE rather than from the config string
// that names it. A path naming a missing or unparsable file therefore
// contributes nothing at all, rather than contributing a fingerprint
// of nothing.
type advertisedCertificate struct {
	// name is the config field the path came from, for error messages
	// an operator can act on ("raft_tls_cert is set to X, which does
	// not parse").
	name string

	// fingerprint is hostcert.Fingerprint(cert.Raw) - the ONE place
	// that format is defined. internal/manager's own older
	// tlsCertFingerprint helper is now a delegation to this rather
	// than a second implementation, precisely so the value an operator
	// reads off the joiner and the value the target compares cannot
	// ever be spelled two ways.
	fingerprint string

	// pem is the leaf certificate itself, which is what a pin has to
	// carry: a digest cannot rebuild a PEM bundle, and a re-imaged
	// Comb still has to dial the Colony it belongs to.
	pem string

	notAfter time.Time
}

// localAdvertisedCertificates reads every certificate this managerd
// actually holds: its own managerd serving certificate (tls_cert) and
// its raftd transport certificate (raft_tls_cert) when Raft transport
// TLS is configured.
//
// Both are read from disk. The config strings are only used to FIND
// the files, which is the whole distinction the ADR draws: a
// fingerprint of "the path /etc/apiary/tls.crt" is a fingerprint of
// nothing at all, and a request carrying one would be comparing the
// operator's screen against a path.
//
// tls_cert is MANDATORY and its absence is an error rather than an
// empty list. A joiner with no serving certificate is a Comb that
// cannot be pinned, cannot be trusted by anything it forwards to, and
// cannot be approved under Part 2 at all - so recording a request from
// it would only produce a row certain to be refused later. Failing here
// says so at the moment the operator can still go and fix it.
//
// raft_tls_cert is optional and its absence is not an error. A Comb
// that has not turned on Raft transport TLS genuinely holds one
// certificate and advertises one.
func (s *Server) localAdvertisedCertificates() ([]advertisedCertificate, error) {
	if s.nodeConfig == nil {
		return nil, fmt.Errorf("this node has no node-config file configured, so it holds no certificate to advertise; the joining Comb's managerd needs its own node-config with a tls_cert (see `apiaryctl install --apply`)")
	}
	cfg, err := s.nodeConfig.Load()
	if err != nil {
		return nil, fmt.Errorf("reading this node's own configuration: %w", err)
	}
	if strings.TrimSpace(cfg.TLSCert) == "" {
		return nil, fmt.Errorf("this node's own configuration names no tls_cert, so it has no certificate fingerprint to advertise. A join request carrying no fingerprints cannot be approved - run `apiaryctl install --apply` on the joining Comb first")
	}
	serving, err := readCertificateFile("tls_cert", cfg.TLSCert)
	if err != nil {
		return nil, err
	}
	certs := []advertisedCertificate{*serving}

	// raft_tls_cert comes from raftd.json, which managerd reads
	// through its own raftdConfigStore. A nil store (a managerd wired
	// without one, which some tests are) is treated as "not
	// configured" rather than as a fault: it means this managerd
	// genuinely cannot see whether Raft transport TLS is on, and the
	// honest advertisement of one certificate is better than a
	// refusal for a file that may well not exist.
	var raftTLSPath string
	if s.raftdConfig != nil {
		if rcfg, rerr := s.raftdConfig.Load(); rerr == nil {
			raftTLSPath = strings.TrimSpace(rcfg.RaftTLSCert)
		}
	}
	if raftTLSPath == "" {
		return certs, nil
	}
	raftCert, err := readCertificateFile("raft_tls_cert", raftTLSPath)
	if err != nil {
		// A CONFIGURED but unreadable raft_tls_cert is an error, not a
		// skip. Advertising one fingerprint when the operator believes
		// two are in play is a request that will be compared against
		// the wrong list and refused for a reason nobody can see.
		return nil, err
	}
	return append(certs, *raftCert), nil
}

// readCertificateFile loads one certificate by path and derives its
// fingerprint and PEM. Named after the config field so the refusal
// says which setting is wrong rather than only which file.
func readCertificateFile(name, path string) (*advertisedCertificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s is set to %q, which cannot be read: %w - a join request has to advertise a real certificate fingerprint, not a path", name, path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s is set to %q, which does not contain a PEM certificate", name, path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s is set to %q, whose certificate does not parse: %w", name, path, err)
	}
	return &advertisedCertificate{
		name:        name,
		fingerprint: hostcert.Fingerprint(cert.Raw),
		pem:         string(pem.EncodeToMemory(block)),
		notAfter:    cert.NotAfter,
	}, nil
}

// localAdvertisedFingerprints is localAdvertisedCertificates reduced to
// the list that goes on the wire, in a stable order (serving
// certificate first, then raft transport) so two renders of the same
// Comb produce identical text for an operator to compare.
func (s *Server) localAdvertisedFingerprints() ([]string, error) {
	certs, err := s.localAdvertisedCertificates()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(certs))
	for _, c := range certs {
		out = append(out, c.fingerprint)
	}
	return out, nil
}

// localJoinCertificateForPin returns the leaf to pin for a request
// about nodeID: this Comb's own serving certificate. The raft
// transport certificate is advertised for the operator to compare but
// is not the pin - the pin is what a peer dials this Comb with, and
// the serving certificate is the one every peer dials it with.
func (s *Server) localJoinCertificateForPin() (*advertisedCertificate, error) {
	certs, err := s.localAdvertisedCertificates()
	if err != nil {
		return nil, err
	}
	return &certs[0], nil
}

// secondPinExpired reports whether a request's second PIN has lapsed at
// this node's own clock. A predicate, not a sweep, for the same reason
// ColonyJoinWindowLive is one: nothing has to run for the deadline to
// be enforced, and every member decides the same way from the same
// replicated field.
func secondPinExpired(req *internalpb.PendingJoinRequest) bool {
	if req.GetSecondPin() == "" {
		return false
	}
	if req.GetSecondPinExpiresAtUnix() <= 0 {
		// No deadline recorded. Treated as expired rather than as
		// "unlimited": an authorization value with an unknown lifetime
		// is an authorization value that outlives the request it was
		// issued for, and the request's own 15-minute TTL is already
		// the outer bound.
		return true
	}
	return time.Now().Unix() >= req.GetSecondPinExpiresAtUnix()
}

// joinRequestNotActionable names, in one place, every reason a request
// is refused before an operator's pasted value is even compared. Kept
// as a helper so VerifyJoinIntroduction, ReissueJoinSecondPin and
// ApproveJoinRequest cannot each invent their own slightly different
// wording for "not yet", and so an operator learns the same next step
// whichever form they used.
func joinRequestNotActionable(req *internalpb.PendingJoinRequest, opName string) string {
	switch {
	case req.GetStatus() == internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_FAILED:
		return fmt.Sprintf("%s: request_id %q is FAILED. It used all %d attempts at a stage and is terminal - purge it and have the joining Comb request again", opName, req.GetRequestId(), raftnode.MaxJoinStageAttempts)
	case req.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING:
		return fmt.Sprintf("%s: request_id %q is %s, not pending", opName, req.GetRequestId(), req.GetStatus())
	case joinRequestExpired(req):
		return fmt.Sprintf("%s: request_id %q has expired", opName, req.GetRequestId())
	case req.GetStage() != internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED:
		return fmt.Sprintf("%s: request_id %q is at stage %s. The first code and fingerprints have to be verified on this Colony's page before the second PIN exists", opName, req.GetRequestId(), req.GetStage())
	case secondPinExpired(req):
		return fmt.Sprintf("%s: request_id %q's second PIN expired at %s. Reissue it on this Colony's page - the expired value is not usable, and the requesting Comb will be told to poll again",
			opName, req.GetRequestId(), time.Unix(req.GetSecondPinExpiresAtUnix(), 0).UTC().Format(time.RFC3339))
	}
	return ""
}

// loadJoinRequestForOperator reads one request from this member's own
// replicated copy, for the Admin-side steps above. Local, not
// leader-restricted: the record is replicated so every member shows
// the same request, and the leader check happens in the handlers where
// the mutation is.
func (s *Server) loadJoinRequestForOperator(ctx context.Context, requestID string) (*internalpb.PendingJoinRequest, error) {
	getResp, err := s.raft.GetPendingJoinRequestLocal(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if getResp.GetError() != "" {
		return nil, fmt.Errorf("%s", getResp.GetError())
	}
	if getResp.GetRequest() == nil {
		return nil, fmt.Errorf("request_id %q does not exist", requestID)
	}
	return getResp.GetRequest(), nil
}

// VerifyJoinIntroduction implements rpcpb.ManagerServiceServer -
// Admin-tier (internal/manager/auth.go), and ADR-0147 Part 2's stage
// one.
//
// The receiving member may be any member, not only the leader: the
// target's page is whichever member the joiner dialed. The check
// itself is a raft command, so the leader that applies it makes the
// decision from the same replicated record the operator is looking at.
//
// The response deliberately does NOT carry the second PIN. The PIN
// goes to the requesting Comb, on that Comb's own poll, and nowhere
// else - see GetJoinRequestStatus.
func (s *Server) VerifyJoinIntroduction(ctx context.Context, req *rpcpb.VerifyJoinIntroductionRequest) (*rpcpb.VerifyJoinIntroductionResponse, error) {
	if req.GetRequestId() == "" {
		return &rpcpb.VerifyJoinIntroductionResponse{Error: "request_id is required"}, nil
	}
	if req.GetIntroductionCode() == "" {
		return &rpcpb.VerifyJoinIntroductionResponse{Error: "the first code is required. It is the 6-digit value the REQUESTING Comb generated and shows on its own screen (`apiaryctl join-introduce --field first-code`); it is not a value this Colony generates"}, nil
	}
	// The fingerprints are pasted by a human off the requesting Comb's
	// screen. An empty list is refused here rather than allowed to
	// reach the FSM, because "the operator pasted nothing" and "the
	// operator pasted something that did not match" are different
	// problems and only one of them is a failed attempt worth counting.
	if len(req.GetFingerprints()) == 0 {
		return &rpcpb.VerifyJoinIntroductionResponse{Error: "the joining Comb's advertised fingerprints are required. Read them off the requesting Comb's own screen (`apiaryctl join-introduce --field fingerprints`), one per line - comparing the request against nothing is exactly what this step refuses to do"}, nil
	}
	// Leadership first, and before any local work, for the same reason
	// ApproveJoinRequest does it: the command has to be applied by the
	// leader, and the fingerprints being pinned have to be attributed
	// to the member whose operator accepted them.
	if status, statusErr := s.raft.Status(ctx); statusErr == nil && !status.GetIsLeader() {
		if hint := currentLeaderRaftAddress(status); hint != "" && s.peers != nil {
			if fwd, ferr := s.peers.VerifyJoinIntroduction(ctx, s.peerManagerdAddr(hint), req); ferr == nil {
				return fwd, nil
			}
		}
	}

	// The PIN is generated HERE, on the member that received the
	// click, and handed to the FSM as an input. It is discarded if the
	// comparison fails, so generating it speculatively leaks nothing,
	// and it is not written to any local file or log on this path -
	// the only place it ever lands is replicated state, from where the
	// requesting Comb may fetch it.
	secondPin, err := generateJoinSecondPin()
	if err != nil {
		return &rpcpb.VerifyJoinIntroductionResponse{Error: fmt.Sprintf("generating the second PIN: %v", err)}, nil
	}
	now := time.Now()
	cmd := &internalpb.Command{
		Op: &internalpb.Command_VerifyJoinIntroduction{
			VerifyJoinIntroduction: &internalpb.VerifyJoinIntroduction{
				RequestId:              req.GetRequestId(),
				IntroductionCode:       req.GetIntroductionCode(),
				Fingerprints:           req.GetFingerprints(),
				SecondPin:              secondPin,
				SecondPinExpiresAtUnix: now.Add(joinSecondPinTTL).Unix(),
				AtUnix:                 now.Unix(),
			},
		},
	}
	// The pin is ATTEMPTED here, and its evaluation result is folded
	// into the command rather than applied separately. See
	// VerifyJoinIntroduction's own comment in state.proto: "the
	// operator compared the fingerprints" and "the certificate is
	// pinned" have to be one log entry, or there is a window in which a
	// request is CODE_VERIFIED with no trust anchor behind it.
	if peer, perr := s.buildPinForRequest(req.GetRequestId(), now); perr != nil {
		return &rpcpb.VerifyJoinIntroductionResponse{Error: perr.Error()}, nil
	} else if peer != nil {
		cmd.GetVerifyJoinIntroduction().Peer = peer
	}

	result, appErr, leaderHint := s.applyJoinRequestCommand(ctx, cmd, 0)
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.VerifyJoinIntroduction(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.VerifyJoinIntroductionResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.VerifyJoinIntroductionResponse{Request: fromInternalPendingJoinRequest(result)}, nil
}

// buildPinForRequest assembles the TrustedPeer this member's operator
// is pinning by accepting the request's advertised fingerprints.
//
// comb_name is the operator-facing label, taken from this member's own
// configuration of the JOINER's name where one exists and otherwise
// the node_id. It is purely descriptive - no caller may refuse, route
// or authenticate on it, because a name is not an identity and the
// same name can legitimately be reused by a re-imaged Comb.
//
// pinned_by is THIS member's node_id, not anything the caller sent:
// the attribution has to be a fact about which Comb's operator accepted
// the certificate, or the log's answer to "who trusted this" is a
// claim.
//
// A nil peer with a nil error means "there is nothing to pin", which
// the FSM treats as a legal verify that leaves the trust store
// untouched. That is the pre-ADR-0147 shape, and it is why approval
// additionally requires that the fingerprints were actually
// evaluated.
func (s *Server) buildPinForRequest(requestID string, now time.Time) (*internalpb.TrustedPeer, error) {
	pending, err := s.loadJoinRequestForOperator(context.Background(), requestID)
	if err != nil {
		return nil, err
	}
	// Fail closed BEFORE evaluating anything: a request that advertised
	// no fingerprints has nothing to pin and nothing to compare, and
	// advancing it to CODE_VERIFIED would produce a handshake the
	// target's operator believes it completed.
	if len(pending.GetAdvertisedFingerprints()) == 0 {
		return nil, fmt.Errorf("request_id %q advertised no certificate fingerprints, so there is nothing to pin and nothing to compare. A request with no fingerprints cannot be approved; have the joining Comb re-request with `apiaryctl join-introduce`", requestID)
	}
	leaf, err := s.localJoinCertificateForPin()
	if err != nil {
		return nil, err
	}
	// The pin is only meaningful if it is the certificate the request
	// carried, and the operator's paste is what asserts that. If this
	// member's own serving certificate is not among the advertised
	// fingerprints, the request is about a different machine than the
	// one this member would be pinning - and the operator's comparison
	// has not happened yet, so this is not a refusal of the
	// comparison, it is a refusal to pin the wrong thing.
	pinned := false
	for _, fp := range pending.GetAdvertisedFingerprints() {
		if constantTimeFingerprintEqual(leaf.fingerprint, fp) {
			pinned = true
			break
		}
	}
	if !pinned {
		return nil, fmt.Errorf("request_id %q advertised fingerprints that do not include this Comb's own serving certificate (%s). There is nothing here to pin: this request is about a different machine than the one whose certificate would be trusted", requestID, leaf.fingerprint)
	}
	return &internalpb.TrustedPeer{
		NodeId:       pending.GetNodeId(),
		CombName:     pending.GetNodeId(),
		Fingerprint:  leaf.fingerprint,
		CertPem:      leaf.pem,
		NotAfterUnix: leaf.notAfter.Unix(),
		PinnedAtUnix: now.Unix(),
		PinnedBy:     s.localNodeID(),
	}, nil
}

// ReissueJoinSecondPin implements rpcpb.ManagerServiceServer -
// Admin-tier, ADR-0147 Part 2's explicit re-arm of the second PIN.
//
// It exists for exactly one situation: AddVoter failed after the PIN
// was already spent. The spent PIN is deliberately NOT restored,
// because restoring it would be a free retry of a value the operator
// has already typed once, on a request that has already been acted on.
// Re-arming is a second visible act, it invalidates the previous PIN,
// and it is capped at 2 in replicated state.
func (s *Server) ReissueJoinSecondPin(ctx context.Context, req *rpcpb.ReissueJoinSecondPinRequest) (*rpcpb.ReissueJoinSecondPinResponse, error) {
	if req.GetRequestId() == "" {
		return &rpcpb.ReissueJoinSecondPinResponse{Error: "request_id is required"}, nil
	}
	if status, statusErr := s.raft.Status(ctx); statusErr == nil && !status.GetIsLeader() {
		if hint := currentLeaderRaftAddress(status); hint != "" && s.peers != nil {
			if fwd, ferr := s.peers.ReissueJoinSecondPin(ctx, s.peerManagerdAddr(hint), req); ferr == nil {
				return fwd, nil
			}
		}
	}
	secondPin, err := generateJoinSecondPin()
	if err != nil {
		return &rpcpb.ReissueJoinSecondPinResponse{Error: fmt.Sprintf("generating a new second PIN: %v", err)}, nil
	}
	now := time.Now()
	cmd := &internalpb.Command{
		Op: &internalpb.Command_ReissueJoinSecondPin{
			ReissueJoinSecondPin: &internalpb.ReissueJoinSecondPin{
				RequestId:              req.GetRequestId(),
				SecondPin:              secondPin,
				SecondPinExpiresAtUnix: now.Add(joinSecondPinTTL).Unix(),
				AtUnix:                 now.Unix(),
			},
		},
	}
	result, appErr, leaderHint := s.applyJoinRequestCommand(ctx, cmd, 0)
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.ReissueJoinSecondPin(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.ReissueJoinSecondPinResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	remaining := uint32(0)
	if used := result.GetSecondPinReissues(); used < raftnode.MaxJoinSecondPinReissues {
		remaining = raftnode.MaxJoinSecondPinReissues - used
	}
	return &rpcpb.ReissueJoinSecondPinResponse{
		Request:           fromInternalPendingJoinRequest(result),
		ReissuesRemaining: remaining,
	}, nil
}

// PinPeerCertificate implements rpcpb.ManagerServiceServer - Admin-tier.
//
// This is the writer ADR-0147 Part 4's replicated trust store was
// missing: before this change, nothing in the tree constructed a
// Command_PinTrustedPeer, so the store was always empty and the
// derived peer-ca.pem it feeds had nothing in it. The introduction path
// above is its intended writer and is not a page's own action; this
// RPC exists because an operator can also pin a certificate out of
// band, and because an Admin CAN add a pin.
//
// That last clause is why the store is documented as a mechanism for
// honest operation rather than as a security boundary. A pin is not a
// substitute for the root-owned authorization entry, and NOTHING
// authenticates a caller by presenting one.
//
// It cannot create or modify an authorization entry. That is ADR-0147
// Part 3's load-bearing negative: the only writer there is the
// root-only apiaryctl binary acting on a local file, and any future
// RPC that touches that file is a security regression even if it is
// Admin-tier.
func (s *Server) PinPeerCertificate(ctx context.Context, req *rpcpb.PinPeerCertificateRequest) (*rpcpb.PinPeerCertificateResponse, error) {
	if req.GetPeer().GetNodeId() == "" {
		return &rpcpb.PinPeerCertificateResponse{Error: "node_id is required"}, nil
	}
	if status, statusErr := s.raft.Status(ctx); statusErr == nil && !status.GetIsLeader() {
		if hint := currentLeaderRaftAddress(status); hint != "" && s.peers != nil {
			if fwd, ferr := s.peers.PinPeerCertificate(ctx, s.peerManagerdAddr(hint), req); ferr == nil {
				return fwd, nil
			}
		}
	}
	// The caller's own values are not trusted for the two fields that
	// have to be facts: the pin's fingerprint is re-derived from the
	// certificate it carries, and the expiry is re-read from the
	// certificate. The FSM refuses a pin whose stated values disagree
	// with its own PEM, so this is belt to that braces - but doing it
	// here means the refusal names the real cause.
	peer := proto.Clone(req.GetPeer()).(*internalpb.TrustedPeer)
	info, err := hostcert.Evaluate([]byte(peer.GetCertPem()), hostcert.EvaluateOptions{
		Now:                time.Unix(peer.GetPinnedAtUnix(), 0).UTC(),
		RequireDNSSAN:      true,
		RequireLoopbackSAN: true,
	})
	if err != nil {
		return &rpcpb.PinPeerCertificateResponse{Error: fmt.Sprintf("refusing to pin: %v", err)}, nil
	}
	peer.Fingerprint = info.Fingerprint
	peer.NotAfterUnix = info.NotAfter.Unix()
	if peer.GetPinnedBy() == "" {
		peer.PinnedBy = s.localNodeID()
	}
	cmd := &internalpb.Command{
		Op: &internalpb.Command_PinTrustedPeer{PinTrustedPeer: &internalpb.PinTrustedPeer{Peer: peer}},
	}
	result, appErr, leaderHint := s.applyTrustedPeerCommand(ctx, cmd, 0)
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.PinPeerCertificate(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.PinPeerCertificateResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.PinPeerCertificateResponse{Peer: toRPCTustedPeer(result)}, nil
}

// UnpinPeerCertificate implements rpcpb.ManagerServiceServer - Admin-tier.
//
// Drops a pin held for a request that reached a terminal state without
// becoming a voter. That cleanup is ALSO done inside the same log entry
// that settles the request (the FSM's own dropUnpromotedPin on
// reject, cancel, purge and exhaustion), precisely so an interrupted
// approval cannot leave a dead certificate standing forever. This RPC
// is the manual path for the case the automatic one cannot cover: an
// operator who has concluded, by some reasoning of their own, that a
// pin should not be here.
//
// reason is recorded in the log entry as part of the removal, so an
// incident review can answer "who dropped this and why" rather than
// only "when".
func (s *Server) UnpinPeerCertificate(ctx context.Context, req *rpcpb.UnpinPeerCertificateRequest) (*rpcpb.UnpinPeerCertificateResponse, error) {
	if req.GetNodeId() == "" {
		return &rpcpb.UnpinPeerCertificateResponse{Error: "node_id is required"}, nil
	}
	if status, statusErr := s.raft.Status(ctx); statusErr == nil && !status.GetIsLeader() {
		if hint := currentLeaderRaftAddress(status); hint != "" && s.peers != nil {
			if fwd, ferr := s.peers.UnpinPeerCertificate(ctx, s.peerManagerdAddr(hint), req); ferr == nil {
				return fwd, nil
			}
		}
	}
	if strings.TrimSpace(req.GetReason()) == "" {
		return &rpcpb.UnpinPeerCertificateResponse{Error: "a reason is required. Dropping a certificate is one an operator went to the trouble of comparing by hand; a removal with no recorded reason is the thing an incident review cannot use"}, nil
	}
	cmd := &internalpb.Command{
		Op: &internalpb.Command_UnpinTrustedPeer{UnpinTrustedPeer: &internalpb.UnpinTrustedPeer{
			NodeId: req.GetNodeId(),
			Reason: strings.TrimSpace(req.GetReason()),
		}},
	}
	result, appErr, leaderHint := s.applyTrustedPeerCommand(ctx, cmd, 0)
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.UnpinPeerCertificate(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.UnpinPeerCertificateResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.UnpinPeerCertificateResponse{Peer: toRPCTustedPeer(result)}, nil
}

// applyTrustedPeerCommand is applyJoinRequestCommand's twin for the
// trust store: same submit, same leader_hint convention, different
// result message. Deliberately a second small function rather than a
// generalized one, because a join request and a pinned certificate
// are different records and a helper that un-marshalled "whichever
// result type the command implies" would be a helper whose failure
// mode is a silent wrong type.
func (s *Server) applyTrustedPeerCommand(ctx context.Context, cmd *internalpb.Command, timeoutMs uint32) (peer *internalpb.TrustedPeer, appErr, leaderHint string) {
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
	peer = &internalpb.TrustedPeer{}
	if err := proto.Unmarshal(resp.GetResult(), peer); err != nil {
		return nil, err.Error(), ""
	}
	return peer, "", ""
}

// constantTimeFingerprintEqual is raftnode's hashed constant-time
// value comparison, used here for the one comparison the manager layer
// makes rather than delegating: "is this member's own certificate among
// the fingerprints this request advertised". It is spelled through
// raft's exported helper rather than reimplemented so there is one
// comparison in the tree, not two that could drift.
func constantTimeFingerprintEqual(a, b string) bool {
	return raftnode.ConstantTimeValueEqual(a, b)
}
