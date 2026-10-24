// ADR-0156 Stage 1: managerd's view of this Colony's identity.
//
// The value lives in REPLICATED state, not in managerd's configuration.
// That distinction is the whole point of the feature: two independently
// bootstrapped raftd instances each form their own single-node cluster,
// and each would answer "which Colony am I in" from its own config file
// with a different value - which is exactly the condition ADR-0156
// records as verified and undetectable. Read from replicated state,
// every voter of ONE Colony answers with the SAME value, and two voters
// of DIFFERENT colonies answer with different values, so the difference
// becomes visible instead of silent.

package manager

import "context"

// localColonyID returns this Colony's identity, or the empty string if
// this node has none yet.
//
// The empty string is a REAL state, not an error: it means this node
// has applied no SetColonyIdentity, which is true of every node built
// before ADR-0156 and of a node restored from a pre-ADR-0156 snapshot.
//
// The comparison that uses this value treats the two cases
// differently on purpose, and the difference is the safety property:
//
//   - A joining node that CLAIMS an identity is compared against this
//     Colony's. If this Colony has no identity either, the claim is
//     refused - a node cannot join an unnamed Colony by asserting a
//     name, because there is nothing on the other side to compare it
//     against, and the check would be theatre.
//   - A joining node that claims NOTHING is accepted unchanged, exactly
//     as every pre-ADR-0156 join was. Such a member inherits the
//     Colony's identity from the Colony rather than asserting one.
//
// A raftd that cannot be reached returns the empty string too. That is
// deliberately NOT distinguished here: the caller that needs to know
// "this Colony has no identity" from "I could not find out" is the
// status path, which reports them separately, and a join request that
// is refused because a transient raftd read failed would be refused for
// the wrong reason - the operator would go looking at their Colony
// identity when the actual fault was a socket.
func (s *Server) localColonyID(ctx context.Context) string {
	if s.raft == nil {
		return ""
	}
	status, err := s.raft.Status(ctx)
	if err != nil {
		return ""
	}
	return status.GetColonyId()
}
