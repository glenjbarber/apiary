package manager

// ADR-0147 Part 3: the root-owned, single-use authorization an
// operator must give before a Comb may be admitted to this Colony.
//
// The whole design rests on one gap. An Admin-tier caller can already
// write config files as root (UpdateNodeConfig and its two siblings),
// restart services, and reach `raftd -reset`. What it cannot do is
// write an arbitrary root-owned file. So the authorization is a file
// managerd only ever READS, and the operator creates entries in it from
// a root shell - never through any RPC. See internal/joinauth for the
// store itself, and for why "no RPC writes it" is enforced by a test
// that walks the service descriptor rather than by review.
//
// This file is the read side and the gate. Three things happen on the
// approval path, in this order, and the order is the ADR's:
//
//  1. The live join window (Part 4), already checked in
//     ApproveJoinRequest ahead of every other gate.
//  2. THIS: a matching, unconsumed, unexpired entry. Without one there
//     is no voter, whatever API key is presented, and the refusal names
//     the entry needed and the Comb that must carry it.
//  3. The two-person rule: two distinct API keys, presented to two
//     distinct Combs. Both recorded in replicated state, so the Colony's
//     own history says which keys on which Combs admitted what.
//
// The reachability dial, the not-already-a-voter check and the join-log
// guardrail all still run, after this file's checks, exactly as ADR-0147
// Part 3 specifies: those three are Part 2's requirements and Part 3
// adds to them rather than replacing them.

import (
	"context"
	"errors"
	"fmt"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/joinauth"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

// joinAuthorizationLoader reads the root-owned store. An interface
// rather than a direct call into internal/joinauth so a test can drive
// every refusal - a missing file, an unreadable one, a malformed one -
// without a fixture directory, and so the set of refusals this code can
// produce is a closed, enumerable set rather than whatever a filesystem
// happens to produce today.
type joinAuthorizationLoader func() (joinauth.Store, error)

// setJoinAuthorizationPath points this managerd at the root-owned
// authorization store. Empty means joinauth.DefaultPath.
//
// A setter rather than a constructor parameter for the same reason
// SetAssumptionRegister and SetColonyJoinWindowSeconds exist: NewServer's
// signature is load-bearing across managerd and its many focused tests,
// and adding a parameter to it would churn every one of them for a
// value with exactly one production setting.
func (s *Server) setJoinAuthorizationPath(path string) { s.joinAuthorizationsPath = path }

// joinAuthorizationFile resolves the store's path, defaulting.
func (s *Server) joinAuthorizationFile() string {
	if s.joinAuthorizationsPath != "" {
		return s.joinAuthorizationsPath
	}
	return joinauth.DefaultPath
}

// localNodeID is this Comb's own raft identity, read from the same node
// config every other identity check in this package uses.
//
// Used for two things Part 3 needs: stamping approving_node_id on the
// way in, and naming the Comb in a refusal. Both need the value this
// Comb states about ITSELF, never one a caller supplied.
func (s *Server) localNodeID() string {
	if s.nodeConfig != nil {
		if cfg, err := s.nodeConfig.Load(); err == nil && cfg.NodeID != "" {
			return cfg.NodeID
		}
	}
	// s.nodeID is managerd's own identity as passed to NewServer, which
	// on a real Comb names this same machine (ADR-0112 makes common.json
	// the shared owner of exactly that value). It is the fallback
	// because a managerd that cannot read its own config must not be
	// unable to state which Comb it is: refusing the approval would be
	// correct, but refusing it with a message that says "this managerd
	// could not read its own node_id" when it plainly could - through
	// the flag it was started with - would be a wrong diagnosis, and a
	// wrong diagnosis is worse than a refusal.
	return s.nodeID
}

// joinAuthorization is what authorizePendingJoin returns on success: the
// request as replicated state now has it, plus the entry that will be
// recorded as spent.
//
// awaitingSecond is the case that is neither success nor failure, and it
// is a field rather than an error because "your first approval is
// recorded and you now need a second" is the single state an operator
// most needs to be told plainly. Reporting it as an error would teach
// operators that approving is broken; reporting it as success would
// teach them that the Comb joined.
type joinAuthorization struct {
	// record is the request as the FSM holds it after this call's
	// authorizing act was applied. It is what the response carries, so
	// an operator sees the recorded pair rather than a stale copy.
	record *internalpb.PendingJoinRequest

	// id and consumedAt are the record of use written on the approve
	// command. They are empty unless the two-person rule is satisfied.
	id         string
	consumedAt int64

	awaitingSecond  bool
	awaitingMessage string
}

// authorizePendingJoin runs ADR-0147 Part 3's two gates and returns the
// authorization to spend, or a refusal naming exactly what to do.
//
// The order inside is deliberate and matches the ADR: the FILE first,
// then the REPLICATED record of its use, then the two-person rule. The
// file is the operator's act and the most likely thing to be missing;
// the replicated record is what stops a spent entry being spent twice
// after a leadership change; the two-person rule is last because it is
// the one an operator may legitimately have to come back for.
//
// It is called on the leader only, from ApproveJoinRequest, after the
// window gate and before AddVoter. Nothing here admits anybody: the
// only irreversible call in this package is still ahead of it.
func (s *Server) authorizePendingJoin(ctx context.Context, pending *internalpb.PendingJoinRequest, req *rpcpb.ApproveJoinRequestRequest) (*joinAuthorization, error) {
	// Gate 1: the operator's own act, in the root-owned file.
	entry, err := s.authorizationEntry(pending)
	if err != nil {
		return nil, s.authorizationRefusal(err, pending)
	}

	// Gate 2: the replicated record of that entry's use.
	//
	// The file cannot answer this and that is the whole reason the
	// replicated record exists. Only the leader reads the file, so after
	// a leadership change the new leader may be holding a copy that
	// still lists an entry as available when a previous leader already
	// spent it. Read from raftd, the answer is the same on every Comb
	// and in every election.
	spentBy, spentAt, found, err := s.raft.GetAuthorizationUse(ctx, entry.ID)
	if err != nil {
		return nil, fmt.Errorf("refusing to approve %q: checking whether authorization entry %q was already used: %v - an approval cannot proceed on an unanswered question about whether this entry is still available",
			pending.GetRequestId(), entry.ID, err)
	}
	if found {
		return nil, fmt.Errorf(
			"refusing to approve %q: authorization entry %q was already used to admit join request %q at %s. Entries are single-use, and the record of that use is replicated, so it does not matter that this Comb's copy of %s may still list the entry as available",
			pending.GetRequestId(), entry.ID, spentBy,
			time.Unix(spentAt, 0).Format(time.RFC3339), s.joinAuthorizationFile())
	}

	// Gate 3: the two-person rule, recorded in replicated state.
	keyID := callerAPIKeyID(ctx)
	nodeID := req.GetApprovingNodeId()
	switch {
	case keyID == "":
		return nil, fmt.Errorf(
			"refusing to approve %q: this call carried no API key, so there is no first authorization to record. ADR-0147 Part 3 requires two distinct API keys presented to two distinct Combs, and a managerd with no API keys at all (where authentication is a no-op) can supply neither",
			pending.GetRequestId())
	case nodeID == "":
		return nil, fmt.Errorf(
			"refusing to approve %q: this managerd could not read its own node_id, so it cannot state which Comb this authorization was presented to. Without that the two-person rule cannot be counted, and a rule that cannot be counted is not one",
			pending.GetRequestId())
	}

	record, appErr, _ := s.recordJoinApproval(ctx, pending.GetRequestId(), keyID, nodeID, req.GetTimeoutMs())
	if appErr != "" {
		return nil, fmt.Errorf("refusing to approve %q: %s", pending.GetRequestId(), appErr)
	}

	first, second := record.GetApproval_1(), record.GetApproval_2()
	if second == nil {
		return &joinAuthorization{
			record:          record,
			awaitingSecond:  true,
			awaitingMessage: secondAuthorizationMessage(record),
		}, nil
	}
	if !distinctAuthorizations(first, second) {
		return nil, errors.New(distinctAuthorizationRefusal(record))
	}
	return &joinAuthorization{record: record, id: entry.ID, consumedAt: time.Now().Unix()}, nil
}

// distinctAuthorizations is internal/raft's own definition of ADR-0147
// Part 3's two-person rule, reused rather than reimplemented: two
// different API keys, presented to two different Combs.
//
// Delegating matters more than it looks. The rule is enforced in the
// FSM (which must reach the identical decision on every replica) and
// reported on by this layer; a second copy of the predicate here would
// be one more place for the two to disagree, and the disagreement
// would only ever show up as an approval that the FSM accepts and this
// layer refuses, or the reverse.
func distinctAuthorizations(a, b *internalpb.JoinApprovalAttestation) bool {
	return raftnode.AuthorizationsDistinct(a, b)
}

// authorizationEntry resolves the one entry that would authorize this
// pending request, and refuses by name if there is none.
//
// The store is read fresh on every call rather than cached at startup.
// A cache would be faster and would be wrong: the operator is expected
// to create the entry while the approval form sits open, and a managerd
// that read the file once at boot would keep refusing for the lifetime
// of the process. This is a local file read on a path that already
// leads to a raft membership change, so the cost is not what matters
// here; the freshness is.
func (s *Server) authorizationEntry(pending *internalpb.PendingJoinRequest) (joinauth.Entry, error) {
	load := s.joinAuthorizationRead
	if load == nil {
		load = func() (joinauth.Store, error) { return joinauth.Load(s.joinAuthorizationFile()) }
	}
	store, err := load()
	if err != nil {
		// Absent, unreadable and malformed are one situation with one
		// answer, and the answer is in the error: internal/joinauth
		// already names the path, which is the thing an operator needs.
		return joinauth.Entry{}, &authorizationLoadError{err: err}
	}
	entry, refusal := store.Find(pending.GetNodeId(), pending.GetTlsCertFingerprint(), time.Now())
	if refusal != nil {
		return joinauth.Entry{}, refusal
	}
	return entry, nil
}

// authorizationLoadError distinguishes "the store could not be read"
// from "the store was read and refused". A type rather than a string
// because the two get different sentences: a load failure names the
// file and says to install it, while a refusal names the entry and says
// to create it.
type authorizationLoadError struct{ err error }

func (e *authorizationLoadError) Error() string { return e.err.Error() }
func (e *authorizationLoadError) Unwrap() error { return e.err }

// authorizationRefusal renders a load failure or a match refusal as the
// sentence ApproveJoinRequest refuses with.
//
// It always names all three of: what was missing, the values that
// identify the entry, and WHICH COMB must carry it. The last is the one
// that is easy to leave out and the one the ADR singles out - only the
// leader reads the file, so an entry created on the wrong Comb is an
// entry that does not exist, and a refusal that does not say so sends an
// operator to create one that will silently never be read.
//
// The command is printed verbatim so it can be copied rather than
// reconstructed, and the fingerprint is shown as the request recorded
// it, which is the form internal/joinauth normalizes and compares.
func (s *Server) authorizationRefusal(err error, pending *internalpb.PendingJoinRequest) error {
	leader := s.localNodeID()
	where := "the Comb that is currently the Colony's leader (this managerd could not read its own node_id, so it cannot name which one that is)"
	if leader != "" {
		where = fmt.Sprintf("the Colony's current leader (%s)", leader)
	}
	command := fmt.Sprintf("apiaryctl join-authorize --node-id %q --fingerprint %q",
		pending.GetNodeId(), pending.GetTlsCertFingerprint())

	var loadErr *authorizationLoadError
	if errors.As(err, &loadErr) {
		return fmt.Errorf(
			"refusing to approve %q: the operator authorization store could not be read: %v. Until it can be, no join can be approved on this Comb - which is the intended direction of failure. `apiaryctl install` creates the file empty on every Comb; run it there, and create the entry itself on %s, in %s, as root: %s",
			pending.GetRequestId(), loadErr.err, where, s.joinAuthorizationFile(), command)
	}
	return fmt.Errorf(
		"refusing to approve %q: %v. An entry authorizing this join has to exist on %s, in %s, because only the leader reads that file - an entry created on any other Comb is an entry nobody reads. To create it, on that Comb, as root: %s",
		pending.GetRequestId(), err, where, s.joinAuthorizationFile(), command)
}

// recordJoinApproval writes ONE authorizing act into replicated state,
// filling the request's first usable slot. It resolves nothing and
// admits nobody; see internal/raft's applyRecordJoinApproval.
func (s *Server) recordJoinApproval(ctx context.Context, requestID, keyID, nodeID string, timeoutMs uint32) (*internalpb.PendingJoinRequest, string, string) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_RecordJoinApproval{
			RecordJoinApproval: &internalpb.RecordJoinApproval{
				RequestId: requestID,
				KeyId:     keyID,
				NodeId:    nodeID,
				AtUnix:    time.Now().Unix(),
			},
		},
	}
	return s.applyJoinRequestCommand(ctx, cmd, timeoutMs)
}

// secondAuthorizationMessage is what ApproveJoinRequest returns when
// one act is recorded and a second is still needed.
func secondAuthorizationMessage(record *internalpb.PendingJoinRequest) string {
	first := record.GetApproval_1()
	return fmt.Sprintf(
		"authorization 1 of 2 recorded for %q: API key %q, presented to %s. Nothing has been added to the Colony - the request is still pending, and %s did not add a voter. Approve it again from a DIFFERENT Comb using a DIFFERENT API key; one key twice, or two keys on one Comb, are refused by name.",
		record.GetRequestId(), first.GetKeyId(), first.GetNodeId(), "AddVoter")
}

// distinctAuthorizationRefusal explains why two recorded acts do not
// satisfy ADR-0147 Part 3's two-person rule.
//
// The two cases are named separately because they are different
// operator mistakes with different fixes. One key twice is a single
// Admin's second click; two keys on one Comb is an Admin who minted a
// second key. Neither is a misconfiguration to be corrected, and
// "refused" alone would leave an operator hunting for a setting.
func distinctAuthorizationRefusal(record *internalpb.PendingJoinRequest) string {
	first, second := record.GetApproval_1(), record.GetApproval_2()
	if first.GetKeyId() == second.GetKeyId() {
		return fmt.Sprintf(
			"refusing to approve %q: both recorded authorizations used the SAME API key (%q), presented to %s and then to %s. Two DISTINCT keys are required, on two DISTINCT Combs, so that one credential cannot admit a Comb on its own",
			record.GetRequestId(), first.GetKeyId(), first.GetNodeId(), second.GetNodeId())
	}
	return fmt.Sprintf(
		"refusing to approve %q: both recorded authorizations were presented to the SAME Comb (%s), as key %q and then as key %q. Two keys on one Comb is still one Admin on one Comb; the second authorization has to be presented to a different member of the Colony",
		record.GetRequestId(), first.GetNodeId(), first.GetKeyId(), second.GetKeyId())
}

// fromInternalJoinApproval converts one attestation for the external
// API's own type. Field-for-field, with no logic to get wrong, which is
// why it is a function and not inlined into convert.go.
func fromInternalJoinApproval(a *internalpb.JoinApprovalAttestation) *rpcpb.JoinApprovalAttestation {
	if a == nil {
		return nil
	}
	return &rpcpb.JoinApprovalAttestation{KeyId: a.GetKeyId(), NodeId: a.GetNodeId(), AtUnix: a.GetAtUnix()}
}
