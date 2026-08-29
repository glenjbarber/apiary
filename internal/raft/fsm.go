package raft

import (
	"encoding/json"
	"io"
	"sync"

	"github.com/hashicorp/raft"
)

// FSMApplyResult is returned from Node.Apply, echoing back what the FSM
// recorded for the applied log entry.
type FSMApplyResult struct {
	Index   uint64
	Payload []byte
}

// fsmState is the minimal state persisted by FSM, and is what gets
// (de)serialized on Snapshot/Restore. v1 does not interpret the payload
// beyond storing the most recently applied one, since the real
// ephemeral-state schema (VM defs, membership, node ownership) has not
// been designed yet — Apply's payload is intentionally opaque bytes.
type fsmState struct {
	LastIndex   uint64 `json:"last_index"`
	LastPayload []byte `json:"last_payload"`
}

// FSM is a minimal raft.FSM implementation for v1. It does not interpret
// Apply payloads; it only tracks the most recently applied entry, which is
// enough to prove the raft/persistence/protocol stack end-to-end.
type FSM struct {
	mu    sync.Mutex
	state fsmState
}

var _ raft.FSM = (*FSM)(nil)

// NewFSM returns an empty FSM.
func NewFSM() *FSM {
	return &FSM{}
}

// Apply implements raft.FSM.
func (f *FSM) Apply(log *raft.Log) interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.state.LastIndex = log.Index
	f.state.LastPayload = append([]byte(nil), log.Data...)

	return &FSMApplyResult{
		Index:   log.Index,
		Payload: append([]byte(nil), log.Data...),
	}
}

// AppliedIndex returns the index of the most recently applied log entry.
func (f *FSM) AppliedIndex() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.LastIndex
}

// Snapshot implements raft.FSM.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	stateCopy := fsmState{
		LastIndex:   f.state.LastIndex,
		LastPayload: append([]byte(nil), f.state.LastPayload...),
	}
	return &fsmSnapshot{state: stateCopy}, nil
}

// Restore implements raft.FSM.
func (f *FSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()

	var state fsmState
	if err := json.NewDecoder(rc).Decode(&state); err != nil {
		return err
	}

	f.mu.Lock()
	f.state = state
	f.mu.Unlock()
	return nil
}

// fsmSnapshot implements raft.FSMSnapshot by JSON-encoding fsmState, which
// matches the architecture's framing of ephemeral state as small,
// JSON-shaped facts.
type fsmSnapshot struct {
	state fsmState
}

var _ raft.FSMSnapshot = (*fsmSnapshot)(nil)

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	err := json.NewEncoder(sink).Encode(s.state)
	if err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}
