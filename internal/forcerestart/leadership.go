package forcerestart

import (
	"context"
	"fmt"
	"time"

	"github.com/glenjbarber/apiary/internal/localraft"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

// leadership.go is the one question force-restart asks before it
// touches anything: is this Comb the Colony's current leader?
//
// It is a refusal, not a warning, and it is the third thing this
// command refuses on, after root and a plan with no unknown port. The
// two earlier refusals are about the command being able to do its job.
// This one is about the job being the wrong one here.
//
// WHY IT IS A REFUSAL AND NOT A PROMPT
//
// force-restart already tells its operator, in the banner, not to
// restart raftd on the leader. That was a sentence and an assumption.
// This is a check. Restarting raftd on the leader is the one restart
// the Colony cannot absorb: raft applies every write on the leader, and
// on a multi-Comb Colony a leader that goes down without handing over
// can cost the Colony its quorum outright. The command's whole purpose
// is to be run without reading anything first, by someone whose
// attention is on a service that is down, which is exactly the state
// in which a human is least able to also hold "and which Comb is the
// leader" in mind.
//
// It is deliberately not a prompt and not a flag. A prompt is answered
// by reflex under pressure, a flag is passed by a script that was
// written once, and a command whose dangerous mode is a word on a
// command line is a command whose dangerous mode will eventually be on
// a command line in a script. There is no override here at all.
//
// It is also not a peer check - "is some other Comb the leader" is not
// the question. The question is whether THIS Comb is the leader, and
// only this Comb's own raftd can answer it. A follower can be a
// heartbeat behind, and a Comb whose raftd cannot answer is a Comb
// whose answer is unknown, which is treated as a possible leader
// below.

// Leadership is what this Comb's own raftd said about itself when
// force-restart asked.
type Leadership struct {
	// IsLeader is raftd's own answer for itself. It is the one field
	// anything in this package refuses on.
	IsLeader bool

	// NodeID is the raft identity this Comb reports, carried so a
	// refusal can name the thing that has to be dealt with rather than
	// only saying no.
	NodeID string

	// RaftState is raft's own name for the current state, so the
	// message can say what it saw and not only what it concluded.
	RaftState string
}

// localRaft is the production source of Leadership: this Comb's own
// raftd, over the unix socket its raftd.json names.
//
// It is a struct rather than the bare hostCommand so that ConfigPath
// and Timeout are stated fields a test can point at, and so the
// production default is visible in one place instead of being a
// literal buried in a method body.
type localRaft struct {
	// ConfigPath is raftd.json, empty meaning raftdconfig.DefaultPath -
	// the same file raftd reads when it is started with no -config, and
	// resolved through raftd's own loader, so common.json is consulted
	// on the same terms raftd consults it.
	ConfigPath string

	// Timeout bounds the whole exchange. Zero means
	// localraft.DefaultTimeout.
	Timeout time.Duration
}

func (l localRaft) Leadership() (Leadership, error) {
	timeout := l.Timeout
	if timeout <= 0 {
		timeout = localraft.DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	st, err := localraft.Query(ctx, l.ConfigPath)
	if err != nil {
		return Leadership{}, err
	}
	return Leadership{IsLeader: st.IsLeader, NodeID: st.NodeID, RaftState: st.RaftState}, nil
}

// newLeadershipProbe is the production Host's Leadership source, built
// from the same config path the record writer already resolves. Sharing
// that path is not tidiness: the record written after a restart names
// this Comb's raft identity, and a leader check that read a different
// raftd.json would be asking a different Colony.
func newLeadershipProbe() localRaft {
	return localRaft{ConfigPath: raftdconfig.DefaultPath}
}

// ErrIsLeader reports a refusal because this Comb IS the Colony's
// current leader. It is a refusal that happened before the first
// restart, so a caller can report it with the same certainty as
// "nothing was touched".
type ErrIsLeader struct {
	// NodeID is this Comb's raft identity, empty if raftd named none.
	NodeID string

	// RaftState is what raftd called its state when it said yes,
	// normally "leader".
	RaftState string
}

func (e *ErrIsLeader) Error() string {
	return fmt.Sprintf("this Comb is the Colony's current leader (raft state %s)", e.RaftState)
}

// ErrLeaderUnknown reports a refusal because whether this Comb is the
// leader could not be determined at all.
//
// It is deliberately the same outcome as ErrIsLeader as far as the run
// is concerned, and that is the whole point of having it. "I could not
// check" is not "the check passed". A raftd that is wedged, a socket
// path in raftd.json that names some other Comb's socket, and an
// internal token that does not match all look, from here, like a Comb
// that has not answered. Every one of them is a Comb whose leadership
// is not established, and restarting raftd on a Comb whose leadership
// is not established is a coin toss with the Colony's quorum on it.
type ErrLeaderUnknown struct {
	Err error
}

func (e *ErrLeaderUnknown) Error() string {
	return fmt.Sprintf("could not determine whether this Comb is the Colony's leader: %v", e.Err)
}

func (e *ErrLeaderUnknown) Unwrap() error { return e.Err }
