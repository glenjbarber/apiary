package raft

// ADR-0147 Part 4's replicated peer trust store.
//
// What these pin, in the order the ADR states it: that a pin is
// replicated state with a real lifetime, that a pin records what it
// actually pins, that is_voter is separate from the pin, that removal
// is real, and that the store moves the ADR-0143 digest.

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/hostcert"
)

// pinTestNow is the clock every fixture is evaluated at. Fixed, so a
// certificate this test generates is valid at the same instant on every
// run and the refusals below are about the rule under test rather than
// about the wall clock.
var pinTestNow = time.Now().Truncate(time.Second)

func generatePinTestCert(t *testing.T, name string, validity time.Duration) (certPEM, fingerprint string, notAfter int64) {
	t.Helper()
	pem, _, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName:  name + ".lab3.home.arpa",
		DNSNames:    []string{name, name + ".lab3.home.arpa"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		Validity:    validity,
		Now:         pinTestNow,
	})
	if err != nil {
		t.Fatalf("generating a test certificate for %s: %v", name, err)
	}
	info, err := hostcert.Evaluate(pem, hostcert.EvaluateOptions{
		Now:                pinTestNow,
		RequireDNSSAN:      true,
		RequireLoopbackSAN: true,
	})
	if err != nil {
		t.Fatalf("the generated test certificate for %s does not pass the gate the pin applies: %v", name, err)
	}
	return string(pem), info.Fingerprint, info.NotAfter.Unix()
}

func newPinTestPeer(t *testing.T, nodeID, combName string) *internalpb.TrustedPeer {
	t.Helper()
	certPEM, fingerprint, notAfter := generatePinTestCert(t, combName, 365*24*time.Hour)
	return &internalpb.TrustedPeer{
		NodeId:       nodeID,
		CombName:     combName,
		Fingerprint:  fingerprint,
		CertPem:      certPEM,
		NotAfterUnix: notAfter,
		PinnedAtUnix: pinTestNow.Unix(),
		PinnedBy:     "brood",
		IsVoter:      false,
	}
}

func pinCmd(peer *internalpb.TrustedPeer) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_PinTrustedPeer{
			PinTrustedPeer: &internalpb.PinTrustedPeer{Peer: peer},
		},
	}
}

func setVoterCmd(nodeID string, isVoter bool) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_SetTrustedPeerVoter{
			SetTrustedPeerVoter: &internalpb.SetTrustedPeerVoter{NodeId: nodeID, IsVoter: isVoter},
		},
	}
}

func unpinCmd(nodeID string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_UnpinTrustedPeer{
			UnpinTrustedPeer: &internalpb.UnpinTrustedPeer{NodeId: nodeID},
		},
	}
}

func applyPin(t *testing.T, fsm *FSM, index uint64, cmd *internalpb.Command) *FSMApplyResult {
	t.Helper()
	result := fsm.Apply(&raft.Log{Index: index, Data: mustMarshalCommand(t, cmd)})
	res, ok := result.(*FSMApplyResult)
	if !ok {
		t.Fatalf("Apply returned %T, want *FSMApplyResult", result)
	}
	return res
}

func refuseRefusal(t *testing.T, res *FSMApplyResult, wantContains string) {
	t.Helper()
	if res.Error == "" {
		t.Fatalf("command was accepted, want a refusal mentioning %q", wantContains)
	}
	if !strings.Contains(res.Error, wantContains) {
		t.Errorf("refusal %q does not mention %q, so the operator is not told which check refused", res.Error, wantContains)
	}
}

// A pin of a real, evaluated certificate is recorded, and is replicated
// state rather than a field somewhere: it survives a snapshot/restore
// round trip byte for byte.
func TestPinTrustedPeerIsRecordedAndSurvivesRestore(t *testing.T) {
	fsm := NewFSM()
	peer := newPinTestPeer(t, "node02", "drone")

	res := applyPin(t, fsm, 1, pinCmd(peer))
	if res.Error != "" {
		t.Fatalf("Error = %q, want empty", res.Error)
	}
	if res.TrustedPeer.GetNodeId() != "node02" {
		t.Errorf("TrustedPeer node_id = %q, want node02", res.TrustedPeer.GetNodeId())
	}
	if got := fsm.ListTrustedPeers(); len(got) != 1 || got[0].GetFingerprint() != peer.GetFingerprint() {
		t.Fatalf("ListTrustedPeers = %+v, want the one pin just written", got)
	}

	encoded, err := proto.Marshal(fsm.SnapshotState())
	if err != nil {
		t.Fatalf("marshalling snapshot state: %v", err)
	}
	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(encoded))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got := restored.ListTrustedPeers()
	if len(got) != 1 {
		t.Fatalf("restored store has %d pins, want 1 - a restore that dropped the trust store leaves peer TLS with no anchor and no record of which Comb presented what", len(got))
	}
	if !proto.Equal(got[0], peer) {
		t.Errorf("restored pin = %+v, want it identical to the pinned %+v", got[0], peer)
	}
}

// The store is a trust anchor, so a pin whose recorded fingerprint is
// not the digest of the certificate it carries is refused rather than
// stored. This is the whole reason the FSM re-derives the fingerprint
// instead of trusting the writer: the derived peer-ca.pem is built from
// these entries, so a mismatch is a trust anchor for an identity
// nobody compared.
func TestPinTrustedPeerRefusesAFingerprintThatDoesNotMatchItsCertificate(t *testing.T) {
	fsm := NewFSM()
	peer := newPinTestPeer(t, "node02", "drone")
	_, otherFingerprint, _ := generatePinTestCert(t, "drone", 365*24*time.Hour)
	peer.Fingerprint = otherFingerprint

	refuseRefusal(t, applyPin(t, fsm, 1, pinCmd(peer)), "does not match the digest of the certificate supplied")
	if got := len(fsm.ListTrustedPeers()); got != 0 {
		t.Errorf("the store holds %d pins after a refused pin, want 0", got)
	}
}

// A pin claiming a longer life than its certificate has is refused for
// the same reason: it is how a store ends up still trusting a leaf
// after the leaf is dead.
func TestPinTrustedPeerRefusesAnExpiryTheCertificateDoesNotHave(t *testing.T) {
	fsm := NewFSM()
	peer := newPinTestPeer(t, "node02", "drone")
	peer.NotAfterUnix = peer.GetNotAfterUnix() + 86400

	refuseRefusal(t, applyPin(t, fsm, 1, pinCmd(peer)), "does not match the certificate's own NotAfter")
}

// The evaluation is fail-closed and it happens at the FSM boundary, on
// every voter, not only on the one that received the PEM. A certificate
// with no DNS SAN cannot be dialed, so a pin of it is a trust anchor
// for a peer nothing can reach.
func TestPinTrustedPeerRefusesACertificateTheGateRefuses(t *testing.T) {
	fsm := NewFSM()
	pem, _, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName:  "drone",
		DNSNames:    nil, // no resolvable name
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		Now:         pinTestNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := hostcert.Evaluate(pem, hostcert.EvaluateOptions{Now: pinTestNow, RequireLoopbackSAN: true})

	res := applyPin(t, fsm, 1, pinCmd(&internalpb.TrustedPeer{
		NodeId: "node02", CombName: "drone", Fingerprint: info.Fingerprint, CertPem: string(pem),
		NotAfterUnix: info.NotAfter.Unix(), PinnedAtUnix: pinTestNow.Unix(), PinnedBy: "brood",
	}))
	if !strings.Contains(res.Error, "no DNS subject alternative name") {
		t.Errorf("refusal = %q, want it to name the missing-DNS-SAN check", res.Error)
	}
}

// A pin is written before authorization, so it must not be able to
// claim membership. This is the conflation TrustedPeer.is_voter's own
// doc comment exists to prevent, and it is refused rather than
// accepted-and-ignored so a caller that tries cannot believe it worked.
func TestPinTrustedPeerRefusesToClaimVoterMembership(t *testing.T) {
	fsm := NewFSM()
	peer := newPinTestPeer(t, "node02", "drone")
	peer.IsVoter = true

	refuseRefusal(t, applyPin(t, fsm, 1, pinCmd(peer)), "membership is recorded by SetTrustedPeerVoter after AddVoter")
}

// A second pin for a live node_id is refused. An update path here would
// be a silent replacement of a certificate, which is exactly the act
// the operator's by-hand fingerprint comparison exists to make visible.
func TestPinTrustedPeerRefusesADuplicateNodeID(t *testing.T) {
	fsm := NewFSM()
	applyPin(t, fsm, 1, pinCmd(newPinTestPeer(t, "node02", "drone")))

	refuseRefusal(t, applyPin(t, fsm, 2, pinCmd(newPinTestPeer(t, "node02", "drone"))), "is already pinned")
	if got := len(fsm.ListTrustedPeers()); got != 1 {
		t.Errorf("the store holds %d pins after a refused duplicate, want 1 - the refusal must not have replaced the original", got)
	}
}

// Every required field is checked, and the refusals name what is
// missing rather than reporting a generic parse failure. One
// representative per field: the point is that a pin with an empty
// pinned_by (no attribution, so an incident review cannot say who
// trusted it) is refused at all.
func TestPinTrustedPeerRefusesIncompleteEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*internalpb.TrustedPeer)
		wantMsg string
	}{
		{"no node id", func(p *internalpb.TrustedPeer) { p.NodeId = "" }, "node_id must be set"},
		{"no comb name", func(p *internalpb.TrustedPeer) { p.CombName = "" }, "comb_name must be set"},
		{"no attributions", func(p *internalpb.TrustedPeer) { p.PinnedBy = "" }, "pinned_by must be set"},
		{"no pin time", func(p *internalpb.TrustedPeer) { p.PinnedAtUnix = 0 }, "pinned_at_unix must be set"},
		{"no fingerprint", func(p *internalpb.TrustedPeer) { p.Fingerprint = "" }, "fingerprint must be set"},
		{"no certificate", func(p *internalpb.TrustedPeer) { p.CertPem = "" }, "certificate is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsm := NewFSM()
			peer := newPinTestPeer(t, "node02", "drone")
			tc.mutate(peer)
			refuseRefusal(t, applyPin(t, fsm, 1, pinCmd(peer)), tc.wantMsg)
			if got := len(fsm.ListTrustedPeers()); got != 0 {
				t.Errorf("the store holds %d pins after a refusal, want 0", got)
			}
		})
	}
}

// The certificate is evaluated as of pinned_at_unix, never the wall
// clock, so that a replay years later reaches the same decision the
// original apply reached. Pinned with a time after the certificate has
// expired: refused, on the evidence in the log entry rather than on
// whatever day it happens to be replayed.
func TestPinEvaluationIsRelativeToPinTimeNotTheWallClock(t *testing.T) {
	certPEM, fingerprint, notAfter := generatePinTestCert(t, "drone", time.Hour)
	afterExpiry := notAfter + 1

	fsm := NewFSM()
	res := applyPin(t, fsm, 1, pinCmd(&internalpb.TrustedPeer{
		NodeId: "node02", CombName: "drone", Fingerprint: fingerprint, CertPem: certPEM,
		NotAfterUnix: notAfter, PinnedAtUnix: afterExpiry, PinnedBy: "brood",
	}))
	if !strings.Contains(res.Error, "certificate expired") {
		t.Errorf("refusal = %q, want it to name the expiry check, evaluated at pinned_at_unix", res.Error)
	}

	// The same certificate, pinned while it was still valid, is
	// accepted - and stays accepted on every later replay, because the
	// decision was made from the log entry.
	fresh := NewFSM()
	if res := applyPin(t, fresh, 1, pinCmd(&internalpb.TrustedPeer{
		NodeId: "node02", CombName: "drone", Fingerprint: fingerprint, CertPem: certPEM,
		NotAfterUnix: notAfter, PinnedAtUnix: notAfter - 1, PinnedBy: "brood",
	})); res.Error != "" {
		t.Fatalf("Error = %q, want empty - an evaluation relative to the wall clock would have rejected this pin years from now", res.Error)
	}
}

// is_voter is separate from the pin: AddVoter promotes it, and the
// promotion is an explicit recorded act.
func TestSetTrustedPeerVoterPromotesAnExistingPin(t *testing.T) {
	fsm := NewFSM()
	applyPin(t, fsm, 1, pinCmd(newPinTestPeer(t, "node02", "drone")))

	res := applyPin(t, fsm, 2, setVoterCmd("node02", true))
	if res.Error != "" {
		t.Fatalf("Error = %q, want empty", res.Error)
	}
	peers := fsm.ListTrustedPeers()
	if len(peers) != 1 || !peers[0].GetIsVoter() {
		t.Errorf("store = %+v, want one pin with is_voter true", peers)
	}
}

// A Comb is made a raft member by AddVoter, which happens before its
// certificate is ever evaluated. Recording a pin for it here would mean
// trusting a certificate nobody compared, so it is refused.
func TestSetTrustedPeerVoterRefusesANodeThatIsNotPinned(t *testing.T) {
	fsm := NewFSM()
	refuseRefusal(t, applyPin(t, fsm, 1, setVoterCmd("node02", true)), "is not pinned")
	if got := len(fsm.ListTrustedPeers()); got != 0 {
		t.Errorf("the refused promotion created a pin anyway: %d entries", got)
	}
}

// Removal is real. A store that can only grow accumulates dead
// certificates until one expires by accident.
func TestUnpinTrustedPeerRemovesTheEntry(t *testing.T) {
	fsm := NewFSM()
	applyPin(t, fsm, 1, pinCmd(newPinTestPeer(t, "node02", "drone")))

	res := applyPin(t, fsm, 2, unpinCmd("node02"))
	if res.Error != "" {
		t.Fatalf("Error = %q, want empty", res.Error)
	}
	if res.TrustedPeer.GetNodeId() != "node02" {
		t.Errorf("UnpinTrustedPeer echoed %+v, want the removed entry so the caller can report what it dropped", res.TrustedPeer)
	}
	if got := len(fsm.ListTrustedPeers()); got != 0 {
		t.Errorf("store holds %d pins after an unpin, want 0", got)
	}
}

// "Remove this" cannot be a silent no-op an operator reads as "it is
// gone".
func TestUnpinTrustedPeerRefusesAnUnknownNode(t *testing.T) {
	fsm := NewFSM()
	refuseRefusal(t, applyPin(t, fsm, 1, unpinCmd("node02")), "is not pinned")
}

// The pin lifecycle is inside the log entry that settles the request,
// not a command a caller has to remember to send afterwards. Approval
// promotes the pin to a member pin; a terminal state that never became
// a member drops it, so the store does not accumulate one entry per
// Comb that ever asked.
func TestPinLifecycleFollowsTheJoinRequest(t *testing.T) {
	liveExpiry := time.Now().Add(15 * time.Minute).Unix()

	t.Run("approve promotes the pin", func(t *testing.T) {
		fsm := NewFSM()
		applyPin(t, fsm, 1, pinCmd(newPinTestPeer(t, "node02", "drone")))
		applyPin(t, fsm, 2, createPendingJoinRequestCmd("req-1", "node02", "10.0.0.9:17701", "code", liveExpiry))
		applyPin(t, fsm, 3, approvePendingJoinRequestCmd("req-1"))

		peers := fsm.ListTrustedPeers()
		if len(peers) != 1 || !peers[0].GetIsVoter() {
			t.Errorf("store after approval = %+v, want the pin promoted to a member pin", peers)
		}
	})

	t.Run("reject drops the pin", func(t *testing.T) {
		fsm := NewFSM()
		applyPin(t, fsm, 1, pinCmd(newPinTestPeer(t, "node02", "drone")))
		applyPin(t, fsm, 2, createPendingJoinRequestCmd("req-1", "node02", "10.0.0.9:17701", "code", liveExpiry))
		applyPin(t, fsm, 3, rejectPendingJoinRequestCmd("req-1"))

		if got := len(fsm.ListTrustedPeers()); got != 0 {
			t.Errorf("store after rejection holds %d pins, want 0 - a refused request must not leave a standing trust anchor", got)
		}
	})

	t.Run("cancel drops the pin", func(t *testing.T) {
		fsm := NewFSM()
		applyPin(t, fsm, 1, pinCmd(newPinTestPeer(t, "node02", "drone")))
		applyPin(t, fsm, 2, createPendingJoinRequestCmd("req-1", "node02", "10.0.0.9:17701", "code", liveExpiry))
		applyPin(t, fsm, 3, &internalpb.Command{Op: &internalpb.Command_CancelPendingJoinRequest{
			CancelPendingJoinRequest: &internalpb.CancelPendingJoinRequest{RequestId: "req-1"},
		}})

		if got := len(fsm.ListTrustedPeers()); got != 0 {
			t.Errorf("store after cancellation holds %d pins, want 0", got)
		}
	})

	t.Run("purge drops an unpromoted pin", func(t *testing.T) {
		fsm := NewFSM()
		applyPin(t, fsm, 1, pinCmd(newPinTestPeer(t, "node02", "drone")))
		applyPin(t, fsm, 2, createPendingJoinRequestCmd("req-1", "node02", "10.0.0.9:17701", "code", liveExpiry))
		applyPin(t, fsm, 3, purgeJoinRequestCmd("req-1"))

		if got := len(fsm.ListTrustedPeers()); got != 0 {
			t.Errorf("store after a purge holds %d pins, want 0", got)
		}
	})

	// The mirror of the last case, and the one that would be a serious
	// bug: an Admin tidying up a stale request must not take a live
	// member's trust anchor with it.
	t.Run("purge keeps a promoted pin", func(t *testing.T) {
		fsm := NewFSM()
		applyPin(t, fsm, 1, pinCmd(newPinTestPeer(t, "node02", "drone")))
		applyPin(t, fsm, 2, createPendingJoinRequestCmd("req-1", "node02", "10.0.0.9:17701", "code", liveExpiry))
		applyPin(t, fsm, 3, approvePendingJoinRequestCmd("req-1"))
		applyPin(t, fsm, 4, purgeJoinRequestCmd("req-1"))

		peers := fsm.ListTrustedPeers()
		if len(peers) != 1 || !peers[0].GetIsVoter() {
			t.Errorf("store after purging an approved request = %+v, want the member pin kept - the request is stale, the member is not", peers)
		}
	})
}

// A Colony upgraded from a pre-ADR-0147 build already has members, and
// their certificates were never pinned. Their approvals must still work
// - refusing them would make the upgrade path worse than the bug - and
// the pin must be absent rather than invented, because inventing one
// would trust a certificate nobody compared.
func TestApprovingAnUnpinnedJoinRequestSucceedsAndPinsNothing(t *testing.T) {
	fsm := NewFSM()
	expires := time.Now().Add(15 * time.Minute).Unix()
	applyPin(t, fsm, 1, createPendingJoinRequestCmd("req-1", "node02", "10.0.0.9:17701", "code", expires))

	res := applyPin(t, fsm, 2, approvePendingJoinRequestCmd("req-1"))
	if res.Error != "" {
		t.Fatalf("Error = %q, want empty - an unpinned member of a pre-ADR-0147 Colony must still be approvable", res.Error)
	}
	if got := len(fsm.ListTrustedPeers()); got != 0 {
		t.Errorf("approval invented %d pin(s), want 0 - no certificate was ever compared on this path", got)
	}
}

// Expiry is a predicate, not a sweep. An expired pin is still in the
// store, so a reader can say "this peer's certificate expired" instead
// of the handshake failing against a host that is in no trust store at
// all.
func TestTrustedPeerExpiredIsAPredicateNotASweep(t *testing.T) {
	fsm := NewFSM()
	peer := newPinTestPeer(t, "node02", "drone")
	applyPin(t, fsm, 1, pinCmd(peer))

	if TrustedPeerExpired(peer, pinTestNow.Unix()) {
		t.Error("TrustedPeerExpired = true at pin time, want false")
	}
	if !TrustedPeerExpired(peer, peer.GetNotAfterUnix()) {
		t.Error("TrustedPeerExpired = false exactly at NotAfter, want true - an expired pin is expired at its own deadline")
	}
	if got := len(fsm.ListTrustedPeers()); got != 1 {
		t.Errorf("the store holds %d pins after expiry, want 1 - the entry is kept so the refusal has a name", got)
	}
}

// Both callers of the store are byte comparators: the derived
// peer-ca.pem writer has to produce the same file on every Comb, and a
// page that reorders itself between renders looks like a store that is
// mutating under the operator. Map iteration order is randomised per
// range, so an unsorted reader would fail this intermittently.
func TestListTrustedPeersIsSortedByNodeID(t *testing.T) {
	fsm := NewFSM()
	applyPin(t, fsm, 1, pinCmd(newPinTestPeer(t, "node03", "sting")))
	applyPin(t, fsm, 2, pinCmd(newPinTestPeer(t, "node01", "brood")))
	applyPin(t, fsm, 3, pinCmd(newPinTestPeer(t, "node02", "drone")))

	for i := 0; i < 50; i++ {
		peers := fsm.ListTrustedPeers()
		if len(peers) != 3 {
			t.Fatalf("ListTrustedPeers returned %d pins, want 3", len(peers))
		}
		if peers[0].GetNodeId() != "node01" || peers[1].GetNodeId() != "node02" || peers[2].GetNodeId() != "node03" {
			t.Fatalf("ListTrustedPeers order = %s,%s,%s, want node01,node02,node03", peers[0].GetNodeId(), peers[1].GetNodeId(), peers[2].GetNodeId())
		}
	}
}

// The returned entries are copies. A caller that mutated one would
// otherwise be editing replicated state without a log entry, which is
// the one thing this store cannot tolerate.
func TestListTrustedPeersReturnsCopies(t *testing.T) {
	fsm := NewFSM()
	applyPin(t, fsm, 1, pinCmd(newPinTestPeer(t, "node02", "drone")))

	fsm.ListTrustedPeers()[0].CombName = "somewhere-else"

	if got := fsm.ListTrustedPeers()[0].GetCombName(); got != "drone" {
		t.Errorf("comb_name = %q after a caller mutated the returned entry, want drone", got)
	}
}

// ADR-0143: the store is replicated state, so it moves the canonical
// digest. Two voters whose digests agree hold identical trust stores;
// two that differ do not, which is what makes "is this Colony's
// membership agreeing about who it trusts" a checkable fact.
func TestTrustedPeerStoreMovesTheStateDigest(t *testing.T) {
	empty := NewFSM()
	before := empty.StateDigest()

	fsm := NewFSM()
	applyPin(t, fsm, 1, pinCmd(newPinTestPeer(t, "node02", "drone")))
	pinned := fsm.StateDigest()
	if pinned == before {
		t.Error("StateDigest unchanged after a pin was written, want it to move with replicated state")
	}

	// A change of content moves it again, and a rejected command does
	// not move it at all - the property that makes a digest comparable
	// across voters.
	applyPin(t, fsm, 2, setVoterCmd("node02", true))
	promoted := fsm.StateDigest()
	if promoted == pinned {
		t.Error("StateDigest unchanged after a promotion, want it to move")
	}
	applyPin(t, fsm, 3, pinCmd(newPinTestPeer(t, "node02", "drone")))
	if got := fsm.StateDigest(); got != promoted {
		t.Errorf("StateDigest = %s after a rejected pin, want it unchanged at %s", got, promoted)
	}

	applyPin(t, fsm, 4, unpinCmd("node02"))
	if got := fsm.StateDigest(); got != before {
		t.Errorf("StateDigest = %s after the last pin was removed, want it back to the empty-state digest %s", got, before)
	}
}

// The digest has to describe the trust store specifically, not merely
// move: two states that differ only in WHICH certificate is pinned are
// exactly the pair a digest exists to catch.
func TestStateDigestDistinguishesWhichCertificateIsPinned(t *testing.T) {
	drone := NewFSM()
	applyPin(t, drone, 1, pinCmd(newPinTestPeer(t, "node02", "drone")))

	sting := NewFSM()
	applyPin(t, sting, 1, pinCmd(newPinTestPeer(t, "node02", "sting")))

	if drone.StateDigest() == sting.StateDigest() {
		t.Error("two colonies that pinned different certificates under the same node ID digest the same, want them to differ")
	}
}
