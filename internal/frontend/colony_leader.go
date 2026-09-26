package frontend

import (
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// colonyLeaderStatusTimeout bounds the header indicator's own Status call.
// It is deliberately short and separate from internal/manager's own
// per-probe budgets: the indicator runs on every page render, so it must
// never be the reason a page takes noticeably longer to appear. Exceeding it
// yields the unknown verdict, which is the correct outcome - a leader reading
// that arrived too late to be current is not evidence of anything.
const colonyLeaderStatusTimeout = 2 * time.Second

// Colony leader states. These are deliberately distinct words rather than a
// single "leader: none" fallback, because the underlying evidence genuinely
// differs in each case and collapsing them is the exact failure ADR-0056
// exists to prevent.
const (
	// colonyLeaderKnown means raft reported a specific leader node ID.
	colonyLeaderKnown = "known"

	// colonyLeaderUnknown means the leader could NOT be established - no
	// observation was possible. This is never "there is no leader": it is
	// "nobody could tell us", and the two must not share a rendering.
	colonyLeaderUnknown = "unknown"

	// colonyLeaderNone means raft was read successfully and reports no
	// current leader from a stable, non-electing state. This is a real
	// observed absence (e.g. quorum is unavailable), not silence.
	colonyLeaderNone = "none"

	// colonyLeaderElecting means this Comb's own raftd is currently a
	// candidate, so a new leader is actively being chosen right now.
	colonyLeaderElecting = "electing"

	// colonyLeaderShutdown means this Comb's own raftd is shutting down or
	// already shut down, so it has no leadership to report.
	colonyLeaderShutdown = "shutdown"
)

// colonyLeaderView is the whole contract behind the header's Colony-leader
// indicator. It carries a raw observation and a conclusion, kept separate on
// purpose: NodeID/RaftState/ObservedAt are what was read, State/Detail is what
// was concluded from it.
//
// The indicator is a point-in-time read of ONE raftd - this Comb's own. It is
// not a cluster-wide agreement. A follower that has lost contact with the
// current leader can still report the previous one, so the view names its own
// source rather than presenting itself as the Colony's single truth. See
// ADR-0123.
type colonyLeaderView struct {
	// State is one of the colonyLeader* constants above.
	State string

	// NodeID is the leader's raft node ID, non-empty only when State is
	// colonyLeaderKnown.
	NodeID string

	// IsLocal reports that this Comb's own raftd states it is the leader.
	// It is taken from raft_is_leader directly rather than by comparing
	// NodeID to anything: manager_node_id is documented to possibly differ
	// from the raft node ID, so an ID comparison would be a guess, while
	// raft's own "I am the leader" is the actual fact.
	IsLocal bool

	// ManagerNodeID is the managerd identity serving this page, and
	// LocalRaftNodeID this Comb's own raft node ID. Both are shown so the
	// operator can tell which node produced the reading, and to make an ID
	// mismatch visible instead of silently confusing the two.
	ManagerNodeID   string
	LocalRaftNodeID string

	// RaftState is this Comb's own raft state as reported ("Follower",
	// "Candidate", "Leader", "Shutdown"), verbatim and untranslated. It is
	// retained even when State is colonyLeaderKnown, since "I am a follower
	// reporting node X" and "I am node X" are different facts.
	RaftState string

	// Detail explains the non-obvious states in one operator-readable
	// sentence, carrying the underlying reason verbatim where one exists
	// (e.g. managerd's own raft_error). Empty for a plain known leader,
	// which needs no explanation.
	Detail string

	// Observed reports whether raft state was actually read. False means
	// the indicator has no evidence at all and must render as unknown
	// rather than as an absence.
	Observed bool

	// ObservedAt is when the read happened, so a template can show the age
	// of the reading rather than implying it is continuously true.
	ObservedAt time.Time
}

// BadgeClass maps State to a CSS class from layout.html's badge vocabulary.
// Kept here so the template never has to reason about evidence, only render
// a verdict it was handed.
func (v colonyLeaderView) BadgeClass() string {
	switch v.State {
	case colonyLeaderKnown:
		return "ready"
	case colonyLeaderElecting:
		return "degraded"
	case colonyLeaderShutdown:
		return "stopped"
	case colonyLeaderNone:
		return "error"
	default:
		return "unknown"
	}
}

// Label is the short text form of the verdict. A known leader shows its node
// ID directly, since that is the whole question the operator is asking; every
// other state names itself in words so it can never be mistaken for an ID.
//
// A known leader whose ID could not be determined still gets words rather than
// an empty badge: the verdict really is "known" (raft asserted leadership),
// but showing nothing there would look like the indicator had failed to load.
func (v colonyLeaderView) Label() string {
	if v.State == colonyLeaderKnown {
		if v.NodeID == "" {
			return "Leader (no ID reported)"
		}
		return v.NodeID
	}
	switch v.State {
	case colonyLeaderUnknown:
		return "Leader unknown"
	case colonyLeaderNone:
		return "No leader"
	case colonyLeaderElecting:
		return "Electing"
	case colonyLeaderShutdown:
		return "Raftd down"
	default:
		return "Leader unknown"
	}
}

// IsLeaderNode reports whether nodeID is the leader this view actually
// observed. It is false for every non-known state, so a caller marking
// something "the leader" (a Combs list row, say) can never do so from an
// unobserved or in-progress reading.
func (v colonyLeaderView) IsLeaderNode(nodeID string) bool {
	return v.State == colonyLeaderKnown && v.NodeID == nodeID
}

// SourceLine names the reader, so the number is never presented as a
// cluster-wide fact it is not.
func (v colonyLeaderView) SourceLine() string {
	local := v.LocalRaftNodeID
	if local == "" {
		local = v.ManagerNodeID
	}
	if local == "" {
		return "Colony leader"
	}
	return "Colony leader - read by " + local
}

// colonyLeaderFromStatus derives the indicator from exactly one managerd
// Status call. It is pure so every state above is reachable by test without a
// live cluster, and so the unknown/none split cannot regress silently.
//
// statusErr is passed in rather than fetched here: the caller owns all I/O,
// matching how internal/health separates SignalsFrom from its callers. A
// caller that already has a StatusResponse passes it plus a nil error, so
// the header indicator costs no extra RPC on pages that fetched one anyway.
func colonyLeaderFromStatus(status *rpcpb.StatusResponse, statusErr error, now time.Time) colonyLeaderView {
	v := colonyLeaderView{ObservedAt: now}

	if statusErr != nil {
		// managerd itself was unreachable. Nothing was observed at all.
		v.State = colonyLeaderUnknown
		v.Detail = "could not read this Comb's managerd: " + statusErr.Error()
		return v
	}
	if status == nil {
		v.State = colonyLeaderUnknown
		v.Detail = "managerd returned no status response"
		return v
	}

	v.ManagerNodeID = status.GetManagerNodeId()
	v.LocalRaftNodeID = status.GetRaftNodeId()
	v.RaftState = status.GetRaftState()

	if !status.GetRaftReachable() {
		// The critical case. managerd answered but could not reach raftd, so
		// there is no evidence about leadership whatsoever. Reporting "no
		// leader" here would be a fabricated conclusion from silence, which
		// is the precise mistake this state exists to prevent.
		v.State = colonyLeaderUnknown
		detail := status.GetRaftError()
		if detail == "" {
			detail = "raftd is not reachable over its internal socket"
		}
		v.Detail = "could not reach this Comb's raftd (" + detail + "), so the current leader is unobserved"
		return v
	}

	// From here raft state was genuinely read.
	v.Observed = true

	if status.GetRaftIsLeader() {
		// raft's own "I am the leader" is authoritative and needs no ID
		// comparison. Prefer this node's raft ID for display, falling back
		// to whatever the reported leader ID is if the ID is somehow empty.
		v.State = colonyLeaderKnown
		v.NodeID = status.GetRaftNodeId()
		v.IsLocal = true
		if v.NodeID == "" {
			v.NodeID = status.GetRaftLeaderId()
		}
		if v.NodeID == "" {
			// A leader that cannot name itself. Keep the "known" verdict -
			// raft did assert leadership - but make the gap visible.
			v.Detail = "this Comb's raftd reports itself leader but named no node ID"
		}
		return v
	}

	if id := status.GetRaftLeaderId(); id != "" {
		v.State = colonyLeaderKnown
		v.NodeID = id
		v.IsLocal = status.GetManagerNodeId() != "" && id == status.GetManagerNodeId()
		return v
	}

	// No leader ID, and we are demonstrably not the leader. raft's own
	// LeaderWithID doc says an empty ID means "no current leader OR leader
	// unknown", so raft_state is the only thing that can separate a real
	// absence from an unsettled one.
	switch status.GetRaftState() {
	case "Follower":
		// A settled follower with no leader is the genuine absence case:
		// this node has contact with raft and there is nobody to lead.
		v.State = colonyLeaderNone
		v.Detail = "this Comb's raftd reports no current leader (it is a follower with no leader known)"
	case "Candidate":
		v.State = colonyLeaderElecting
		v.Detail = "this Comb's raftd is a candidate, so a leader is being elected right now"
	case "Shutdown":
		v.State = colonyLeaderShutdown
		v.Detail = "this Comb's raftd is shut down, so it reports no leadership"
	case "Leader":
		// Unreachable in practice: raft sets its own leader ID when it
		// becomes leader, so raft_is_leader would be true above. Treated as
		// unknown rather than guessed, per ADR-0056's rule that an
		// unrecognized observation is never read as a healthy one.
		v.State = colonyLeaderUnknown
		v.Detail = "this Comb's raftd reports state Leader with no leader ID, which is an inconsistent observation"
	case "":
		v.State = colonyLeaderUnknown
		v.Detail = "this Comb's raftd reported no raft state, so the current leader is unobserved"
	default:
		// A newer raftd could report a state this build has never heard of.
		// Name it verbatim rather than collapsing it into a verdict.
		v.State = colonyLeaderUnknown
		v.Detail = "this Comb's raftd reported an unrecognized raft state (" + status.GetRaftState() + "), so the current leader is unobserved"
	}
	return v
}
