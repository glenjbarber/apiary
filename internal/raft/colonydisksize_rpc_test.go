package raft

import (
	"context"
	"testing"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// TestGetColonyDiskSizeLocalAnswersFromTheNodesOwnCopy covers the
// RPC that ADR-0156 recorded as "declared but not implemented", and
// that had no test of its own.
//
// It is a local-only RPC: any node answers from its own FSM copy, and
// it is deliberately NOT leader-restricted, because the floor is
// raft-replicated and every node holds the same value. A follower
// answering is therefore correct, not a stale read.
func TestGetColonyDiskSizeLocalAnswersFromTheNodesOwnCopy(t *testing.T) {
	fsm := NewFSM()

	server := &Server{node: &Node{fsm: fsm}}
	resp, err := server.GetColonyDiskSizeLocal(context.Background(), &internalpb.GetColonyDiskSizeRequest{})
	if err != nil {
		t.Fatalf("GetColonyDiskSizeLocal() error = %v", err)
	}
	// A Colony with no floor recorded answers with the absence, not
	// with an error and not with a made-up number.
	if resp.GetColonyDiskSize().GetFloorMb() != 0 {
		t.Errorf("the floor is %d MB, want 0 for a Colony with no floor recorded", resp.GetColonyDiskSize().GetFloorMb())
	}

	// And once a floor IS recorded, the RPC reports it.
	applyFloor(t, fsm, 1, 1024)
	resp, err = server.GetColonyDiskSizeLocal(context.Background(), &internalpb.GetColonyDiskSizeRequest{})
	if err != nil {
		t.Fatalf("GetColonyDiskSizeLocal() error = %v", err)
	}
	if resp.GetColonyDiskSize().GetFloorMb() != 1024 {
		t.Errorf("the floor is %d MB, want 1024", resp.GetColonyDiskSize().GetFloorMb())
	}
	// The answer is the node's OWN copy, not a leader's: the same
	// value the FSM holds, which is what "local" means.
	if resp.GetColonyDiskSize().GetFloorMb() != fsm.ColonyDiskSizeFloorMB() {
		t.Error("the RPC's answer differs from the node's own FSM copy")
	}
}
