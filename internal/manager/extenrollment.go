package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/extnodes"
	"github.com/glenjbarber/apiary/internal/joinauth"
	"google.golang.org/protobuf/proto"
)

// ADR-0191's enrollment half, on the managerd side.
//
// WHAT THIS DOES, in one sentence: it admits a discovered external host
// to this Colony's REPORTING DOMAIN, and nothing else.
//
// WHAT THIS DOES NOT DO, in one sentence: it adds no voter.
//
// The distinction is not a matter of phrasing. A raft membership change
// is a quorum event: it must be applied on the leader and committed, and
// it makes the new member a peer in Apiary's consensus. A Proxmox node
// is a Linux host running QEMU/KVM. An Apiary Comb is a FreeBSD host
// running bhyve, with its own managerd, its own ZFS datasets, and its
// own cells. Admitting the first as the second would claim a management
// relationship that does not exist, and the reconciler would then try to
// act on it.
//
// So the ENROLLMENT borrows the join flow's AUTHORIZATION and none of
// its MEMBERSHIP:
//
//   - the Colony join window (ADR-0147 Part 4) - a live, time-bounded
//     statement that this Colony is accepting new trust relationships
//     right now
//   - the two-person rule (ADR-0147 Part 3) - two distinct API keys,
//     presented to two distinct Colony members, from a single-use
//     root-owned authorization entry
//   - the same confirmation phrase
//
// and its terminal effect is a record in the local external-node store.
//
// THE WINDOW IS REQUIRED BECAUSE OF WHAT ENROLLMENT GRANTS, not
// because of membership. An enrolled external node is a host other Combs
// will report on, so it is a trust relationship that must be opened
// deliberately and must expire.
//
// The authorizing acts are REPLICATED (internal/raft/externalenrollment.go)
// because the two-person rule has to count the same way on every replica
// and across a leadership change. The enrollment RECORD is local, because
// a provider credential is a per-host secret and because an enrolled
// external host is not a Colony fact in the way a member is.

// enrollExternalNodeConfirmPhrase is the phrase that must be typed to
// enroll an external node.
//
// It is deliberately a different string from the Colony join's
// "yes-trust-new-comb". The two actions are different: one admits a
// MEMBER that will hold a vote and speak Apiary's raft protocol, and
// one admits an EXTERNAL HOST that holds neither. An operator who
// approves one has not thereby approved the other, and a UI that used
// one phrase for both would make that difference invisible.
const enrollExternalNodeConfirmPhrase = "yes-enroll-external-node"

// removeExternalEnrollmentConfirmPhrase is the phrase that must be typed
// to remove an enrollment.
//
// It is distinct from the enrollment phrase for the same reason: removing
// an admitted host is a different act from admitting one, and a phrase
// shared between them would make an operator's acknowledgement of one
// count as an acknowledgement of the other.
const removeExternalEnrollmentConfirmPhrase = "yes-remove-external-node"

// externalEnrollmentTTL is how long an enrollment attempt stays
// completable.
//
// An attempt that can sit pending forever is one that can be completed
// long after the operator who started it has forgotten about it, which
// is the same reasoning ADR-0147 Part 2 applies to a join request.
const externalEnrollmentTTL = 15 * time.Minute

// enrollExternalNode implements rpcpb.ManagerServiceServer: the
// ENROLLMENT half of ADR-0191.
//
// Admin-authorized. It is gated by the same machinery that gates a
// Colony join - a live join window, two distinct API keys on two
// distinct members, a single-use root-owned authorization entry, and the
// confirmation phrase - and it adds no voter.
//
// It is TWO-PHASE, and the phases are the point:
//
//   - The FIRST call records one authorizing act and deliberately admits
//     nobody. The response says a second authorization is required.
//   - The SECOND call, with a DIFFERENT API key, presented to a
//     DIFFERENT Colony member, completes the enrollment.
//
// One act must never be able to enroll a host on its own, and a single
// operator must never be able to admit a host on their own either.
func (s *Server) EnrollExternalNode(ctx context.Context, req *rpcpb.EnrollExternalNodeRequest) (*rpcpb.EnrollExternalNodeResponse, error) {
	resp := &rpcpb.EnrollExternalNodeResponse{}

	if req.GetConfirmPhrase() != enrollExternalNodeConfirmPhrase {
		// The phrase is FIRST, before every other check, so that a
		// wrong or missing phrase causes no action at all - not even
		// a raft command. This is ADR-0113's own posture and the
		// join flow's own convention.
		resp.Error = fmt.Sprintf("refusing to enroll: confirm_phrase must be exactly %q, and nothing was enrolled", enrollExternalNodeConfirmPhrase)
		return resp, nil
	}

	if s.raft == nil {
		resp.Error = "refusing to enroll: this managerd has no raft client, and an enrollment is a Colony-wide decision that cannot be recorded without one"
		return resp, nil
	}
	if s.extNodes == nil {
		resp.Error = "refusing to enroll: external-node enrollment is not enabled on this managerd"
		return resp, nil
	}

	provider := strings.TrimSpace(req.GetProvider())
	name := strings.TrimSpace(req.GetName())
	if provider == "" || name == "" {
		resp.Error = "refusing to enroll: the provider and the host's name are both required"
		return resp, nil
	}
	sourceID := strings.TrimSpace(req.GetSourceId())

	// The host must have been OBSERVED. An enrollment of a host that
	// discovery never saw is an operator enrolling a name from
	// memory, which is how a typo becomes a permanent record.
	observed, err := s.observeExternalNode(provider, sourceID, name)
	if err != nil {
		resp.Error = err.Error()
		return resp, nil
	}

	// The caller may supply the fingerprint it read off the provider's
	// own screen. When it does, it is compared against what discovery
	// observed, and a disagreement is a refusal showing both values -
	// the same comparison ADR-0147 Part 2 makes for a joiner's
	// advertised fingerprints.
	if supplied := strings.TrimSpace(req.GetFingerprint()); supplied != "" {
		if observed.Fingerprint == "" {
			resp.Error = fmt.Sprintf("refusing to enroll %s: the supplied fingerprint %q cannot be checked, because the provider did not report a fingerprint for this host. Discovery saw status %q and no fingerprint, and a fingerprint an operator can read off a screen must be checkable against something", observed.Key, supplied, observed.Status)
			return resp, nil
		}
		if !fingerprintsEqual(supplied, observed.Fingerprint) {
			resp.Error = fmt.Sprintf("refusing to enroll %s: the supplied fingerprint %q does not match the observed fingerprint %q. One of them is a different host, or the same host under a different name; enrolling on a disagreement is how a re-keyed host becomes a duplicate", observed.Key, supplied, observed.Fingerprint)
			return resp, nil
		}
	}

	// ADR-0147 Part 4: the live-window check comes FIRST among the
	// substantive checks, before the authorization file and before any
	// raft command. It is the cheapest check available and the one an
	// operator most needs explained.
	window, live := s.liveColonyJoinWindow(ctx)
	if !live {
		resp.Error = fmt.Sprintf("refusing to enroll %s: %s", observed.Key, colonyWindowClosedRefusal)
		return resp, nil
	}

	// The authorizing act needs an API key and a Comb identity. An
	// absent identity is refused rather than recorded as an empty
	// string: two of these is exactly what the two-person rule counts,
	// and an empty key id is precisely the "nobody authenticated" case.
	keyID := callerAPIKeyID(ctx)
	if keyID == "" {
		resp.Error = fmt.Sprintf("refusing to enroll %s: this call carried no API key, so there is no first authorization to record. ADR-0147 Part 3 requires two distinct API keys presented to two distinct Combs, and a managerd with no API keys at all can supply neither", observed.Key)
		return resp, nil
	}
	nodeID := s.localNodeID()
	if nodeID == "" {
		resp.Error = fmt.Sprintf("refusing to enroll %s: this managerd could not read its own node_id, so it cannot state which Comb this authorization was presented to. Without that the two-person rule cannot be counted, and a rule that cannot be counted is not one", observed.Key)
		return resp, nil
	}

	// The root-owned authorization entry: the operator's own act,
	// outside the replicated state. This is the same file the Colony
	// join uses, and the same matching rule - an entry names a node
	// and a fingerprint.
	entry, refusal := s.externalEnrollmentAuthorizationEntry(observed)
	if refusal != nil {
		resp.Error = refusal.Error()
		return resp, nil
	}

	// Find the pending attempt for this host, if there is one.
	pending, found := s.pendingExternalEnrollment(ctx, observed.Key)
	if found && pending.GetStatus() == internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_PENDING {
		if pending.GetExpiresAtUnix() > 0 && pending.GetExpiresAtUnix() < time.Now().Unix() {
			// An expired attempt is not completable. The record is
			// left in place and reported; a new attempt starts
			// cleanly.
			resp.Error = fmt.Sprintf("refusing to enroll %s: the pending enrollment attempt %q expired at %s and cannot be completed. Start again from the first authorization", observed.Key, pending.GetRequestId(), time.Unix(pending.GetExpiresAtUnix(), 0).Format(time.RFC3339))
			return resp, nil
		}
		// A reopen of the join window invalidates every attempt
		// created under the previous one, exactly as it does for a
		// Colony join request.
		if !externalEnrollmentWindowCurrent(window, pending.GetWindowOpenedAtUnix()) {
			resp.Error = fmt.Sprintf("refusing to enroll %s: the pending enrollment attempt %q was created under an earlier join window, and a reopen invalidates every attempt made under the previous one; %s", observed.Key, pending.GetRequestId(), colonyWindowClosedRefusal)
			return resp, nil
		}
	} else {
		// No pending attempt. The FIRST authorizing act creates one.
		requestID, err := newExternalEnrollmentRequestID()
		if err != nil {
			resp.Error = fmt.Sprintf("refusing to enroll %s: %v", observed.Key, err)
			return resp, nil
		}
		created, appErr, leaderHint := s.applyExternalEnrollmentCommand(ctx, &internalpb.Command{
			Op: &internalpb.Command_CreatePendingExternalEnrollment{
				CreatePendingExternalEnrollment: &internalpb.CreatePendingExternalEnrollment{
					Enrollment: &internalpb.PendingExternalEnrollment{
						RequestId:          requestID,
						Provider:           observed.Key.Provider,
						SourceId:           observed.Key.SourceID,
						Name:               observed.Key.Name,
						Fingerprint:        observed.Fingerprint,
						ProviderStatus:     observed.Status,
						RequestedAtUnix:    time.Now().Unix(),
						ExpiresAtUnix:      time.Now().Add(externalEnrollmentTTL).Unix(),
						Status:             internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_PENDING,
						WindowOpenedAtUnix: window.GetOpenedAtUnix(),
					},
				},
			},
		}, req.GetTimeoutMs())
		if appErr != "" {
			resp.Error = appErr
			resp.LeaderHint = leaderHint
			return resp, nil
		}
		pending = created
	}

	// Record THIS authorizing act. The FSM refuses an act that cannot
	// satisfy the two-person rule against the one already recorded -
	// the same API key, or the same Comb - before it is written
	// anywhere.
	recorded, appErr, leaderHint := s.applyExternalEnrollmentCommand(ctx, &internalpb.Command{
		Op: &internalpb.Command_RecordExternalEnrollment{
			RecordExternalEnrollment: &internalpb.RecordExternalEnrollment{
				RequestId: pending.GetRequestId(),
				KeyId:     keyID,
				NodeId:    nodeID,
				AtUnix:    time.Now().Unix(),
			},
		},
	}, req.GetTimeoutMs())
	if appErr != "" {
		resp.Error = appErr
		resp.LeaderHint = leaderHint
		return resp, nil
	}

	// One act recorded. Deliberately admit nobody.
	if recorded.GetApproval_2() == nil {
		resp.AwaitingSecondAuthorization = true
		resp.SecondAuthorizationRequired = fmt.Sprintf(
			"authorization 1 of 2 recorded for enrollment %q: API key %q, presented to %s. Nothing has been enrolled - no host was admitted and no voter was added. Authorize it again from a DIFFERENT Comb using a DIFFERENT API key; one key twice, or two keys on one Comb, are refused by name.",
			recorded.GetRequestId(), keyID, nodeID)
		return resp, nil
	}

	// Both acts are in. Resolve the attempt as enrolled.
	resolved, appErr, leaderHint := s.applyExternalEnrollmentCommand(ctx, &internalpb.Command{
		Op: &internalpb.Command_ResolveExternalEnrollment{
			ResolveExternalEnrollment: &internalpb.ResolveExternalEnrollment{
				RequestId:      recorded.GetRequestId(),
				Status:         internalpb.ExternalEnrollmentStatus_EXTERNAL_ENROLLMENT_STATUS_ENROLLED,
				EnrolledAtUnix: time.Now().Unix(),
			},
		},
	}, req.GetTimeoutMs())
	if appErr != "" {
		resp.Error = appErr
		resp.LeaderHint = leaderHint
		return resp, nil
	}

	// The authorization entry is single-use. Spending it is recorded
	// against the SAME entry the Colony join spends, so one operator
	// action cannot admit both a Comb and an external host.
	if _, _, found, err := s.raft.GetAuthorizationUse(ctx, entry.ID); err != nil {
		resp.Error = fmt.Sprintf("refusing to complete the enrollment of %s: checking whether authorization entry %q was already used failed: %v - an enrollment cannot proceed on an unanswered question about whether that entry is still available", observed.Key, entry.ID, err)
		return resp, nil
	} else if found {
		resp.Error = fmt.Sprintf("refusing to complete the enrollment of %s: authorization entry %q was already used to admit something else. Entries are single-use", observed.Key, entry.ID)
		return resp, nil
	}

	enrollment := extnodes.Enrollment{
		Key: extnodes.Key{
			Provider: resolved.GetProvider(),
			SourceID: resolved.GetSourceId(),
			Name:     resolved.GetName(),
		},
		Fingerprint: resolved.GetFingerprint(),
		Status:      resolved.GetProviderStatus(),
		EnrolledAt:  time.Unix(resolved.GetExpiresAtUnix(), 0),
		Authorizations: []extnodes.Authorization{
			{KeyID: resolved.GetApproval_1().GetKeyId(), NodeID: resolved.GetApproval_1().GetNodeId(), AtUnix: resolved.GetApproval_1().GetAtUnix()},
			{KeyID: resolved.GetApproval_2().GetKeyId(), NodeID: resolved.GetApproval_2().GetNodeId(), AtUnix: resolved.GetApproval_2().GetAtUnix()},
		},
		EnrolledBy: "external-discovery",
	}
	enrollment.EnrolledAt = time.Now()

	stored, err := s.extNodes.Enroll(enrollment)
	if err != nil {
		resp.Error = fmt.Sprintf("refusing to complete the enrollment of %s: %v", observed.Key, err)
		return resp, nil
	}
	if err := s.extNodes.Save(); err != nil {
		resp.Error = fmt.Sprintf("refusing to report %s as enrolled: the enrollment was recorded in memory but could not be written to %s: %v. It is NOT durably enrolled until this is resolved", observed.Key, s.extNodes.Path(), err)
		return resp, nil
	}

	// The enrollment is a DISTINCT, VISIBLE act, distinguishable from a
	// manual join - ADR-0191 requires that an operator can tell
	// afterwards which hosts came from a provider and which were joined
	// by hand.
	log.Printf("managerd: enrolled external node %s (provider %s, source %q) into the Colony's reporting domain; authorizations: API key %q on %s, API key %q on %s. It is NOT a Colony member and holds no vote.",
		stored.Key, stored.Key.Provider, stored.Key.SourceID,
		stored.Authorizations[0].KeyID, stored.Authorizations[0].NodeID,
		stored.Authorizations[1].KeyID, stored.Authorizations[1].NodeID)

	resp.Enrollment = toRPCExternalNodeEnrollment(stored)
	return resp, nil
}

// externalEnrollmentAuthorizationEntry finds the operator's
// authorization entry for this host.
//
// It is the SAME root-owned store the Colony join uses, and the same
// matching rule. An entry names a node id and a fingerprint; for an
// external host the node id is the provider-scoped composite identity
// and the fingerprint is the provider's own per-node anchor.
func (s *Server) externalEnrollmentAuthorizationEntry(observed extnodes.Node) (joinauth.Entry, error) {
	load := s.joinAuthorizationRead
	if load == nil {
		load = func() (joinauth.Store, error) { return joinauth.Load(s.joinAuthorizationFile()) }
	}
	store, err := load()
	if err != nil {
		return joinauth.Entry{}, &authorizationLoadError{err: err}
	}
	entry, refusal := store.Find(observed.Key.String(), observed.Fingerprint, time.Now())
	if refusal != nil {
		return joinauth.Entry{}, refusal
	}
	return entry, nil
}

// externalEnrollmentWindowCurrent reports whether an enrollment attempt
// was created under the window that is live now.
//
// It is the join flow's own rule, applied to an enrollment attempt: a
// REOPEN of the Colony join window invalidates every attempt created
// under the previous one, so that an attempt cannot be completed long
// after the window it was made under has gone.
func externalEnrollmentWindowCurrent(window *internalpb.ColonyJoinWindow, openedAtUnix int64) bool {
	if window == nil {
		return false
	}
	return window.GetOpenedAtUnix() == openedAtUnix
}

// pendingExternalEnrollmentRequests returns the pending enrollment
// attempts from replicated state.
//
// It reads them from the FSM's own copy rather than from a local
// variable, because the attempt is replicated state and the two
// authorizing acts may be presented to different Colony members.
func (s *Server) pendingExternalEnrollmentRequests(ctx context.Context) []*internalpb.PendingExternalEnrollment {
	if s.raft == nil {
		return nil
	}
	resp, err := s.raft.ListPendingExternalEnrollmentsLocal(ctx)
	if err != nil || resp == nil {
		return nil
	}
	return resp.GetEnrollments()
}

// observeExternalNode returns what discovery observed for a host.
//
// The host MUST have been observed. An enrollment of a host discovery
// never saw is an operator enrolling a name from memory, and a
// misspelled name becomes a permanent record that looks exactly like a
// real one.
func (s *Server) observeExternalNode(provider, sourceID, name string) (extnodes.Node, error) {
	if s.discovery == nil {
		return extnodes.Node{}, fmt.Errorf("refusing to enroll %s/%s/%s: external discovery is not configured on this managerd, so no host by that name has been observed", provider, sourceID, name)
	}
	store, ok := s.discovery.store(provider)
	if !ok {
		return extnodes.Node{}, fmt.Errorf("refusing to enroll %s/%s/%s: no discovery provider named %q is configured on this managerd", provider, sourceID, name, provider)
	}
	key := extnodes.Key{Provider: provider, SourceID: sourceID, Name: name}
	for _, node := range store.Nodes() {
		nodeKey := node.Key()
		if nodeKey.Provider == key.Provider && nodeKey.SourceID == key.SourceID && nodeKey.Name == key.Name {
			return extnodes.Node{
				Key:         key,
				Fingerprint: node.Fingerprint,
				Status:      node.Status,
			}, nil
		}
	}
	return extnodes.Node{}, fmt.Errorf("refusing to enroll %s/%s/%s: no such host has been observed by the %s provider. A host must be discovered before it can be enrolled, so that the enrollment names something that exists rather than something an operator typed", provider, sourceID, name, provider)
}

// pendingExternalEnrollment finds the pending attempt for a host.
func (s *Server) pendingExternalEnrollment(ctx context.Context, key extnodes.Key) (*internalpb.PendingExternalEnrollment, bool) {
	requests := s.pendingExternalEnrollmentRequests(ctx)
	for _, req := range requests {
		if req.GetProvider() == key.Provider && req.GetSourceId() == key.SourceID && req.GetName() == key.Name {
			return req, true
		}
	}
	return nil, false
}

// applyExternalEnrollmentCommand submits a raft command and returns the
// attempt it produced.
func (s *Server) applyExternalEnrollmentCommand(ctx context.Context, cmd *internalpb.Command, timeoutMs uint32) (*internalpb.PendingExternalEnrollment, string, string) {
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
	enrollment := &internalpb.PendingExternalEnrollment{}
	if err := proto.Unmarshal(resp.GetResult(), enrollment); err != nil {
		return nil, err.Error(), ""
	}
	return enrollment, "", ""
}

// newExternalEnrollmentRequestID generates an attempt id.
//
// It is a random hex string rather than something derived from the
// host's identity: the identity is in the record itself, and an id
// derived from it would collide between two attempts at enrolling the
// same host - which is the normal case, since the second act attaches
// to the FIRST attempt rather than starting a new one.
func newExternalEnrollmentRequestID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating an enrollment request id: %w", err)
	}
	return "extenroll-" + hex.EncodeToString(buf), nil
}

// fingerprintsEqual compares two fingerprints.
//
// The comparison is case-insensitive and ignores separators, because
// the same certificate fingerprint is written "SHA256:AB:CD" in one
// place and "sha256:abcd" in another, and a refusal that turns on
// punctuation would be a refusal an operator cannot act on.
func fingerprintsEqual(a, b string) bool {
	normalize := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.TrimPrefix(s, "sha256:")
		s = strings.ReplaceAll(s, ":", "")
		return s
	}
	return normalize(a) == normalize(b)
}

// toRPCExternalNodeEnrollment converts one enrollment for the wire.
func toRPCExternalNodeEnrollment(e extnodes.Enrollment) *rpcpb.ExternalNodeEnrollment {
	if e.Key.Provider == "" && e.Key.Name == "" {
		return nil
	}
	out := &rpcpb.ExternalNodeEnrollment{
		Provider:       e.Key.Provider,
		SourceId:       e.Key.SourceID,
		Name:           e.Key.Name,
		Fingerprint:    e.Fingerprint,
		Status:         string(e.Status),
		ProviderStatus: e.Status,
		EnrolledAtUnix: e.EnrolledAt.Unix(),
		EnrolledBy:     e.EnrolledBy,
	}
	for _, a := range e.Authorizations {
		out.Authorizations = append(out.Authorizations, &rpcpb.ExternalNodeAuthorization{
			KeyId:  a.KeyID,
			NodeId: a.NodeID,
			AtUnix: a.AtUnix,
		})
	}
	return out
}

// GetExternalEnrollments implements rpcpb.ManagerServiceServer: the
// enrolled external hosts.
//
// Viewer-authorized, the same tier as GetExternalDiscovery: it reports
// what was admitted, not a verdict about anything an operator can
// change.
//
// It reports what THIS managerd knows. The enrollment record is local,
// so a member that has not yet applied the resolution does not claim
// the host is enrolled. That is visible lag, not disagreement - and the
// authorizing acts are replicated, so the ATTEMPTS are the same
// everywhere.
func (s *Server) GetExternalEnrollments(ctx context.Context, req *rpcpb.GetExternalEnrollmentsRequest) (*rpcpb.GetExternalEnrollmentsResponse, error) {
	resp := &rpcpb.GetExternalEnrollmentsResponse{}
	if s.extNodes == nil {
		return resp, nil
	}
	provider := req.GetProvider()
	for _, e := range s.extNodes.Enrollments() {
		if provider != "" && e.Key.Provider != provider {
			continue
		}
		enrollment := toRPCExternalNodeEnrollment(e)
		if enrollment == nil {
			continue
		}
		// The standing is the store's own, not the provider's status.
		// The provider's status is carried separately, verbatim.
		enrollment.Status = string(s.extNodes.Status(e.Key))
		resp.Enrollments = append(resp.Enrollments, enrollment)
	}
	return resp, nil
}

// RemoveExternalEnrollment implements rpcpb.ManagerServiceServer: it
// withdraws an enrollment.
//
// Admin-authorized.
//
// Removal is ALWAYS an explicit operator action and never a
// side-effect of a discovery scan. A host that vanished from a
// provider's listing is marked absent and kept, because deleting the
// record on that evidence destroys the history that makes the absence
// diagnosable.
func (s *Server) RemoveExternalEnrollment(ctx context.Context, req *rpcpb.RemoveExternalEnrollmentRequest) (*rpcpb.RemoveExternalEnrollmentResponse, error) {
	resp := &rpcpb.RemoveExternalEnrollmentResponse{}

	if req.GetConfirmPhrase() != removeExternalEnrollmentConfirmPhrase {
		resp.Error = fmt.Sprintf("refusing to remove: confirm_phrase must be exactly %q, and nothing was removed", removeExternalEnrollmentConfirmPhrase)
		return resp, nil
	}
	if s.extNodes == nil {
		resp.Error = "refusing to remove: external-node enrollment is not enabled on this managerd"
		return resp, nil
	}

	key := extnodes.Key{
		Provider: strings.TrimSpace(req.GetProvider()),
		SourceID: strings.TrimSpace(req.GetSourceId()),
		Name:     strings.TrimSpace(req.GetName()),
	}
	if key.Provider == "" || key.Name == "" {
		resp.Error = "refusing to remove: the provider and the host's name are both required"
		return resp, nil
	}

	if err := s.extNodes.Remove(key); err != nil {
		resp.Error = err.Error()
		return resp, nil
	}
	if err := s.extNodes.Save(); err != nil {
		resp.Error = fmt.Sprintf("refusing to report %s as removed: the removal was recorded in memory but could not be written to %s: %v. It is NOT durably removed until this is resolved", key, s.extNodes.Path(), err)
		return resp, nil
	}

	log.Printf("managerd: removed external node %s from the Colony's reporting domain. It was never a Colony member and holds no vote.", key)
	resp.Removed = true
	return resp, nil
}
