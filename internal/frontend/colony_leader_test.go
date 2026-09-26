package frontend

import (
	"errors"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

var leaderTestNow = time.Date(2026, 9, 26, 2, 30, 0, 0, time.UTC)

// reachable builds a StatusResponse in which managerd did reach raftd, so a
// test only has to state the raft facts it actually cares about.
func reachable(state, leaderID, nodeID string, isLeader bool) *rpcpb.StatusResponse {
	return &rpcpb.StatusResponse{
		ManagerNodeId:    "brood.lab3.home.arpa",
		RaftReachable:    true,
		RaftState:        state,
		RaftLeaderId:     leaderID,
		RaftNodeId:       nodeID,
		RaftIsLeader:     isLeader,
		RaftLastLogIndex: 126,
		RaftAppliedIndex: 126,
	}
}

// TestColonyLeaderFromStatus covers every state the indicator can reach, and
// above all the unknown/none split: raft's own LeaderWithID doc says an empty
// leader ID means "no current leader OR leader unknown", so a follower with no
// leader and a Comb that could not read raft at all must never render the
// same thing.
func TestColonyLeaderFromStatus(t *testing.T) {
	tests := []struct {
		name      string
		status    *rpcpb.StatusResponse
		err       error
		wantState string
		wantID    string
		wantLocal bool
		wantSeen  bool
	}{
		{
			name:      "managerd unreachable means unknown, not absent",
			status:    nil,
			err:       errors.New("connection refused"),
			wantState: colonyLeaderUnknown,
			wantSeen:  false,
		},
		{
			name:      "nil response means unknown",
			status:    nil,
			err:       nil,
			wantState: colonyLeaderUnknown,
			wantSeen:  false,
		},
		{
			// The single most important case. raftd is down, so nothing at
			// all is known about leadership. Reporting "no leader" here
			// would be a conclusion invented from silence.
			name: "raftd unreachable means unknown, not absent",
			status: &rpcpb.StatusResponse{
				ManagerNodeId: "brood.lab3.home.arpa",
				RaftReachable: false,
				RaftError:     "raft: connection refused",
			},
			wantState: colonyLeaderUnknown,
			wantSeen:  false,
		},
		{
			name:      "raftd unreachable with no error text still means unknown",
			status:    &rpcpb.StatusResponse{ManagerNodeId: "brood", RaftReachable: false},
			wantState: colonyLeaderUnknown,
			wantSeen:  false,
		},
		{
			name:      "a follower reporting a leader",
			status:    reachable("Follower", "drone.lab3.home.arpa", "brood.lab3.home.arpa", false),
			wantState: colonyLeaderKnown,
			wantID:    "drone.lab3.home.arpa",
			wantSeen:  true,
		},
		{
			name:      "the local node is the leader",
			status:    reachable("Leader", "brood.lab3.home.arpa", "brood.lab3.home.arpa", true),
			wantState: colonyLeaderKnown,
			wantID:    "brood.lab3.home.arpa",
			wantLocal: true,
			wantSeen:  true,
		},
		{
			// raft sets its own leader ID on becoming leader, so is_leader is
			// the authoritative local statement and the node ID is preferred
			// even if the reported leader ID were somehow different.
			name:      "is_leader wins over a conflicting leader id",
			status:    reachable("Leader", "stale.lab3.home.arpa", "brood.lab3.home.arpa", true),
			wantState: colonyLeaderKnown,
			wantID:    "brood.lab3.home.arpa",
			wantLocal: true,
			wantSeen:  true,
		},
		{
			// A leader that cannot name itself still asserted leadership, so
			// the verdict stays known - but the gap is made visible.
			name:      "a nameless leader is still known, with a stated gap",
			status:    reachable("Leader", "", "", true),
			wantState: colonyLeaderKnown,
			wantID:    "",
			wantLocal: true,
			wantSeen:  true,
		},
		{
			// A settled follower with no leader is the genuine absence: this
			// node has contact with raft and there is nobody to lead.
			name:      "a settled follower with no leader is a real absence",
			status:    reachable("Follower", "", "brood.lab3.home.arpa", false),
			wantState: colonyLeaderNone,
			wantSeen:  true,
		},
		{
			name:      "a candidate is electing, not absent",
			status:    reachable("Candidate", "", "brood.lab3.home.arpa", false),
			wantState: colonyLeaderElecting,
			wantSeen:  true,
		},
		{
			name:      "a shut-down raftd is neither known nor absent",
			status:    reachable("Shutdown", "", "brood.lab3.home.arpa", false),
			wantState: colonyLeaderShutdown,
			wantSeen:  true,
		},
		{
			// Unreachable in practice, and explicitly not guessed at.
			name:      "state Leader with no leader id is an inconsistent read",
			status:    reachable("Leader", "", "brood.lab3.home.arpa", false),
			wantState: colonyLeaderUnknown,
			wantSeen:  true,
		},
		{
			name:      "an empty raft state is unknown",
			status:    reachable("", "", "brood.lab3.home.arpa", false),
			wantState: colonyLeaderUnknown,
			wantSeen:  true,
		},
		{
			// A newer raftd could report a state this build has never seen.
			name:      "an unrecognized raft state is named, not collapsed",
			status:    reachable("Observer", "", "brood.lab3.home.arpa", false),
			wantState: colonyLeaderUnknown,
			wantSeen:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := colonyLeaderFromStatus(tc.status, tc.err, leaderTestNow)
			if got.State != tc.wantState {
				t.Errorf("State = %q, want %q (Detail: %s)", got.State, tc.wantState, got.Detail)
			}
			if got.NodeID != tc.wantID {
				t.Errorf("NodeID = %q, want %q", got.NodeID, tc.wantID)
			}
			if got.IsLocal != tc.wantLocal {
				t.Errorf("IsLocal = %v, want %v", got.IsLocal, tc.wantLocal)
			}
			if got.Observed != tc.wantSeen {
				t.Errorf("Observed = %v, want %v", got.Observed, tc.wantSeen)
			}
			if got.ObservedAt != leaderTestNow {
				t.Errorf("ObservedAt = %v, want %v", got.ObservedAt, leaderTestNow)
			}
			// Every non-known state must explain itself. A bare "unknown"
			// with no reason is the thing this whole feature exists to avoid.
			if tc.wantState != colonyLeaderKnown && got.Detail == "" {
				t.Error("Detail is empty for a non-known state; the operator would have no reason to act on")
			}
		})
	}
}

// TestColonyLeaderUnknownDetailNamesRaftError pins that the underlying
// managerd raft_error reaches the operator verbatim - the whole reason raftd
// being down is diagnosable at all.
func TestColonyLeaderUnknownDetailNamesRaftError(t *testing.T) {
	got := colonyLeaderFromStatus(&rpcpb.StatusResponse{
		RaftReachable: false,
		RaftError:     "raft: connect: connection refused",
	}, nil, leaderTestNow)

	if got.State != colonyLeaderUnknown {
		t.Fatalf("State = %q, want %q", got.State, colonyLeaderUnknown)
	}
	if want := "connection refused"; !strings.Contains(got.Detail, want) {
		t.Errorf("Detail = %q, want it to contain %q", got.Detail, want)
	}
}

// TestColonyLeaderUnrecognizedStateDetailIsVerbatim checks an unknown future
// raft state is quoted rather than summarized away, so an operator can search
// for it.
func TestColonyLeaderUnrecognizedStateDetailIsVerbatim(t *testing.T) {
	got := colonyLeaderFromStatus(reachable("Learner", "", "brood", false), nil, leaderTestNow)
	if got.State != colonyLeaderUnknown {
		t.Fatalf("State = %q, want %q", got.State, colonyLeaderUnknown)
	}
	if want := "Learner"; !strings.Contains(got.Detail, want) {
		t.Errorf("Detail = %q, want it to contain %q", got.Detail, want)
	}
}

// TestColonyLeaderLabelNeverLooksLikeAnID guards the rendering boundary: a
// node ID is shown raw, every other state is named in words. Without this a
// future edit could render an empty or placeholder value in the position an
// operator reads as "this node is the leader".
func TestColonyLeaderLabelNeverLooksLikeAnID(t *testing.T) {
	unknown := colonyLeaderView{State: colonyLeaderUnknown}.Label()
	if unknown == "" {
		t.Fatal("unknown state has an empty label")
	}
	if strings.Contains(unknown, ".lab3.home.arpa") {
		t.Errorf("unknown label %q looks like a node ID", unknown)
	}

	known := colonyLeaderView{State: colonyLeaderKnown, NodeID: "drone.lab3.home.arpa"}.Label()
	if known != "drone.lab3.home.arpa" {
		t.Errorf("known label = %q, want the bare node ID", known)
	}

	// A known state with an empty ID must not render as an empty badge.
	if nameless := (colonyLeaderView{State: colonyLeaderKnown}).Label(); nameless == "" {
		t.Error("a known leader with no ID renders an empty label")
	}
}

// TestColonyLeaderSourceLineNamesItsReader checks the indicator always says
// which node produced the reading. It is one raftd's view, not a cluster-wide
// agreement, and a follower that lost the leader can still report the old
// one - so an unattributed number would overstate what was proven.
func TestColonyLeaderSourceLineNamesItsReader(t *testing.T) {
	got := colonyLeaderFromStatus(reachable("Follower", "drone.lab3.home.arpa", "brood.lab3.home.arpa", false), nil, leaderTestNow)
	if want := "brood.lab3.home.arpa"; !strings.Contains(got.SourceLine(), want) {
		t.Errorf("SourceLine() = %q, want it to name the reading node %q", got.SourceLine(), want)
	}

	// With no IDs at all it still produces something presentable.
	if bare := (colonyLeaderView{}).SourceLine(); bare == "" {
		t.Error("SourceLine() is empty for a view carrying no node IDs")
	}
}

// TestColonyLeaderBadgeClassMapsEveryState is a guard against a new state
// being added without a badge class, which would silently render as
// unstyled/missing rather than as a deliberate choice.
func TestColonyLeaderBadgeClassMapsEveryState(t *testing.T) {
	for _, state := range []string{
		colonyLeaderKnown, colonyLeaderUnknown, colonyLeaderNone,
		colonyLeaderElecting, colonyLeaderShutdown, "something-new",
	} {
		if class := (colonyLeaderView{State: state}).BadgeClass(); class == "" {
			t.Errorf("State %q produced an empty badge class", state)
		}
	}
	// unknown must never share a class with known: a reader who can only see
	// color must not read "we could not tell" as "all is well".
	if (colonyLeaderView{State: colonyLeaderUnknown}).BadgeClass() == (colonyLeaderView{State: colonyLeaderKnown}).BadgeClass() {
		t.Error("unknown and known share a badge class; the two must be visually distinct")
	}
}

// TestColonyLeaderIsLeaderNodeOnlyWhenObserved checks the predicate the Combs
// list uses to mark a row. An unknown, electing, shut-down, or genuinely
// leaderless reading must mark nothing, rather than marking an arbitrary row.
func TestColonyLeaderIsLeaderNodeOnlyWhenObserved(t *testing.T) {
	tests := []struct {
		name   string
		status *rpcpb.StatusResponse
		nodeID string
		want   bool
	}{
		{"the reported leader's own row", reachable("Follower", "drone.lab3.home.arpa", "brood", false), "drone.lab3.home.arpa", true},
		{"a different node's row", reachable("Follower", "drone.lab3.home.arpa", "brood", false), "sting.lab3.home.arpa", false},
		// The Combs list is keyed by KnownNodeIds, which comes from raft's
		// own Servers, so a self-leader matches on its RAFT node ID. A node's
		// raft ID and managerd ID are normally the same string, but
		// StatusResponse's own doc comment warns they may differ - so this
		// pins that the raft ID is the one that has to line up.
		{"this Comb's own row when it leads itself", reachable("Leader", "brood.lab3.home.arpa", "brood.lab3.home.arpa", true), "brood.lab3.home.arpa", true},
		{"a self-leader is matched by raft id, not managerd id", &rpcpb.StatusResponse{
			ManagerNodeId: "brood-manager", RaftReachable: true,
			RaftState: "Leader", RaftIsLeader: true, RaftNodeId: "brood-raft", RaftLeaderId: "brood-raft",
		}, "brood-raft", true},
		{"no leader observed", reachable("Follower", "", "brood", false), "brood.lab3.home.arpa", false},
		{"electing", reachable("Candidate", "", "brood", false), "brood.lab3.home.arpa", false},
		{"raftd shut down", reachable("Shutdown", "", "brood", false), "brood.lab3.home.arpa", false},
		{"raftd unreachable", &rpcpb.StatusResponse{RaftReachable: false}, "brood.lab3.home.arpa", false},
		{"unrecognized raft state", reachable("Learner", "", "brood", false), "brood.lab3.home.arpa", false},
		{"no status at all", nil, "brood.lab3.home.arpa", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := colonyLeaderFromStatus(tc.status, nil, leaderTestNow).IsLeaderNode(tc.nodeID)
			if got != tc.want {
				t.Errorf("IsLeaderNode(%q) = %v, want %v", tc.nodeID, got, tc.want)
			}
		})
	}
}
