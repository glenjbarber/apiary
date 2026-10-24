// ADR-0156 Stage 1: the Colony's identity - the state, the one command
// that sets it, and the accessor that serves it.
//
// Why this is replicated raft state rather than a per-Comb config value:
// "which Colony am I in" is only answerable if every voter derives the
// same answer from the same bytes. Two independently bootstrapped raftd
// instances each form their own single-node cluster, each believes it
// is the only Colony, and NOTHING in the system can detect it - that is
// the verified gap ADR-0156 records. An identity held in each node's
// own configuration file would not close it: the two nodes would still
// hold two different identities, and each would still be certain it was
// alone. Replicated through the log, every voter of ONE Colony derives
// the SAME identity, so a node that joins the wrong Colony can be
// caught at admission rather than silently coexisting with it.
//
// Why the "already set" comparison lives HERE rather than in a managerd
// handler: a handler check is advisory. It runs on one process, before
// the command enters the log, and protects only the path that went
// through it. The same command can arrive from a peer-forwarded write,
// from a raftd invoked directly, or from a future client that was not
// in the handler's mind when the check was written. The apply function
// is the only place that runs, in the same order, on every replica: it
// sees the committed log AND the state it is about to change. That is
// the same posture applySetColonyDiskSize and applyPinTrustedPeer take
// at the FSM boundary.
//
// Why the identity is ONCE and not updatable: a Colony's identity is
// what every other fact is anchored to. If it could be rewritten, then
// "the Colony I joined" and "the Colony I am in now" could differ, and
// the difference would be invisible - there would be no record of the
// old value to compare against. A node that wants a different identity
// is not changing this Colony's identity; it is forming a new one,
// which is a different operation with a different answer (ADR-0156's
// restore semantics).
//
// Why the ID is a random UUIDv4 rather than a hash of the Colony CA
// fingerprint: keeping the two anchors INDEPENDENT is what lets the
// Colony CA be rotated without the Colony's identity changing, and it
// is what stops a certificate from silently becoming the Colony's name.
// A fingerprint-derived ID would couple the Colony's identity to a
// value that ADR-0156's CA-rotation problem explicitly wants to be
// able to change.

package raft

import (
	"crypto/rand"
	"fmt"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// applySetColonyIdentity establishes the Colony's identity, and is the
// only way it is ever set. Two refusals, both by name and both naming
// the two identities involved:
//
//   - colony_id is empty. Not treated as "unset": there is exactly one
//     way to express "no identity recorded" in this design, and it is
//     the absence of any SetColonyIdentity entry in the log. An empty
//     ID would be a second way to say the same thing, and the two could
//     disagree.
//   - this Colony already has an identity. A second SetColonyIdentity
//     is refused by name rather than ignored, so that a node trying to
//     re-assert an identity learns that it lost the race instead of
//     believing it succeeded.
//
// A repeated SetColonyIdentity is NOT idempotent-and-successful. It is
// refused, and the refusal names both values.
func (f *FSM) applySetColonyIdentity(index uint64, req *internalpb.SetColonyIdentity) *FSMApplyResult {
	if req.GetColonyId() == "" {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"SetColonyIdentity: refusing an empty colony_id; there is exactly one way to say \"no Colony identity recorded\" and it is the absence of any SetColonyIdentity in this Colony's log, so an empty ID is not an unset marker")}
	}

	// The caller must hold f.mu. Every apply function in this FSM is
	// called from Apply, which takes it once for the whole dispatch,
	// so no re-locking happens here - see the f.mu doc comment.
	current := f.colonyIdentity
	if current.GetColonyId() != "" {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"SetColonyIdentity: refusing to change this Colony's identity from %q to %q because a Colony identity is set exactly once and can never be rewritten; a node that wants a different identity is forming a new Colony, not joining this one",
			current.GetColonyId(), req.GetColonyId())}
	}

	f.colonyIdentity = &internalpb.ColonyIdentity{
		ColonyId:    req.GetColonyId(),
		SetBy:       req.GetSetBy(),
		CreatedUnix: req.GetCreatedUnix(),
	}
	return &FSMApplyResult{Index: index, ColonyIdentity: cloneColonyIdentity(f.colonyIdentity)}
}

// ColonyIdentityState returns a copy of the Colony's identity, or nil if
// this Colony has never had one set. A copy, because the callers here
// are on the request path rather than the apply path, and every other
// FSM accessor here hands out a clone for the same reason.
func (f *FSM) ColonyIdentityState() *internalpb.ColonyIdentity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneColonyIdentity(f.colonyIdentity)
}

// ColonyID returns this Colony's identity, or the empty string if it has
// never had one set.
//
// The empty string is a REAL state, not an error: it means "this node
// has not applied a SetColonyIdentity", which is true of every node
// built before ADR-0156 and of a node that has just been restored from
// a pre-ADR-0156 snapshot. A caller that needs to tell "no identity"
// apart from "this node is not reachable" has to ask a different
// question for each; collapsing them here would hide the very
// disagreement this value exists to expose.
func (f *FSM) ColonyID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.colonyIdentity.GetColonyId()
}

// cloneColonyIdentity deep-copies through proto.Clone rather than by
// value, for the reason cloneColonyDiskSize's own comment gives: a
// generated message carries a sync.Mutex in its MessageState, and
// `c := *s` copies it.
func cloneColonyIdentity(s *internalpb.ColonyIdentity) *internalpb.ColonyIdentity {
	if s == nil {
		return nil
	}
	return proto.Clone(s).(*internalpb.ColonyIdentity)
}

// newColonyID generates a new random UUIDv4 for a Colony's identity.
//
// crypto/rand rather than math/rand: this value is an identity that
// distinguishes one Colony from another, and a predictable identity is
// guessable, which is not a property a security anchor should have.
//
// The UUIDv4 shape (version nibble 4, variant bits 10) is set
// explicitly rather than by hand-formatting hex, because a malformed
// UUID is worse than no UUID - it would be carried, compared, and
// displayed as though it were a real one.
func newColonyID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("reading random bytes for a Colony identity: %w", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10

	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}
