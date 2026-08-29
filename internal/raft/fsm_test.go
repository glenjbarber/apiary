package raft

import (
	"bytes"
	"io"
	"testing"

	"github.com/hashicorp/raft"
)

// fakeSnapshotSink is a minimal in-memory raft.SnapshotSink for testing
// FSMSnapshot.Persist without a real raft.FileSnapshotStore.
type fakeSnapshotSink struct {
	bytes.Buffer
}

func (s *fakeSnapshotSink) ID() string    { return "test" }
func (s *fakeSnapshotSink) Cancel() error { return nil }
func (s *fakeSnapshotSink) Close() error  { return nil }

func TestFSM_Apply(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: []byte("hello")})

	applyResult, ok := result.(*FSMApplyResult)
	if !ok {
		t.Fatalf("Apply returned %T, want *FSMApplyResult", result)
	}
	if applyResult.Index != 1 {
		t.Errorf("Index = %d, want 1", applyResult.Index)
	}
	if string(applyResult.Payload) != "hello" {
		t.Errorf("Payload = %q, want %q", applyResult.Payload, "hello")
	}
	if got := fsm.AppliedIndex(); got != 1 {
		t.Errorf("AppliedIndex() = %d, want 1", got)
	}

	fsm.Apply(&raft.Log{Index: 2, Data: []byte("world")})
	if got := fsm.AppliedIndex(); got != 2 {
		t.Errorf("AppliedIndex() = %d, want 2", got)
	}
}

func TestFSM_SnapshotRestore(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 5, Data: []byte("snapshot-me")})

	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}

	sink := &fakeSnapshotSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("Persist() error: %v", err)
	}

	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}

	if got := restored.AppliedIndex(); got != 5 {
		t.Errorf("restored AppliedIndex() = %d, want 5", got)
	}
	if got := restored.state.LastPayload; string(got) != "snapshot-me" {
		t.Errorf("restored LastPayload = %q, want %q", got, "snapshot-me")
	}
}
