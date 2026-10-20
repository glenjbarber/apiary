package manager

// Issue #85's regression: an authorization entry that a join spent must
// not survive on the Combs that did not consume it. The spend is
// replicated, so every Comb can drop its own copy by asking raft, and
// an entry nobody spent must be left alone.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/joinauth"
)

const authzTestOtherFingerprint = "SHA256:AA:BB:CC:DD:EE:FF:00:11:22"

func storeHasEntry(t *testing.T, path, id string) bool {
	t.Helper()
	store, err := joinauth.Load(path)
	if err != nil {
		t.Fatalf("loading %s: %v", path, err)
	}
	for _, e := range store.Authorizations {
		if e.ID == id {
			return true
		}
	}
	return false
}

func TestJoinAuthorization_SpentEntryIsRemovedFromEveryComb(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	leader := newAuthzComb(t, socket, "comb-leader")
	leader.openWindow(t)
	keyOne, _ := leader.key(t, "", "admin-1")
	leader.seedRequestWithRealJoiner(t, "jreq-1", "comb-3", authzTestFingerprint)
	spentID := leader.authorize(t, "comb-3", authzTestFingerprint)
	// An unrelated, unspent entry for another joining Comb. Pruning must
	// never touch it.
	unspentID := leader.authorize(t, "comb-4", authzTestOtherFingerprint)

	other := newAuthzComb(t, socket, "comb-other")
	copyStoreFile(t, leader.storePath, other.storePath)
	keyTwo, _ := other.key(t, keyOne, "admin-2")
	staleLeader := filepath.Join(t.TempDir(), "stale-leader.json")
	staleOther := filepath.Join(t.TempDir(), "stale-other.json")
	copyStoreFile(t, leader.storePath, staleLeader)
	copyStoreFile(t, other.storePath, staleOther)

	if resp := leader.approve(t, "jreq-1", keyOne); !resp.GetAwaitingSecondAuthorization() {
		t.Fatalf("first approval = %+v, want awaiting a second authorization", resp)
	}
	if resp := other.approve(t, "jreq-1", keyTwo); resp.GetError() != "" {
		t.Fatalf("completing approval: %s", resp.GetError())
	}

	// Put the pre-approval copies back, as a Comb that never ran the
	// prune would hold them, then let each Comb prune its own.
	copyStoreFile(t, staleLeader, leader.storePath)
	copyStoreFile(t, staleOther, other.storePath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, c := range []*authzComb{leader, other} {
		removed, err := c.srv.PruneSpentJoinAuthorizations(ctx)
		if err != nil {
			t.Fatalf("%s: PruneSpentJoinAuthorizations() error: %v", c.name, err)
		}
		if len(removed) != 1 || removed[0] != spentID {
			t.Errorf("%s: removed = %v, want exactly [%s]", c.name, removed, spentID)
		}
		if storeHasEntry(t, c.storePath, spentID) {
			t.Errorf("%s: spent entry %s is still in the store after the join completed", c.name, spentID)
		}
		if !storeHasEntry(t, c.storePath, unspentID) {
			t.Errorf("%s: unspent entry %s was removed; pruning must only remove spent entries", c.name, unspentID)
		}
		again, err := c.srv.PruneSpentJoinAuthorizations(ctx)
		if err != nil || len(again) != 0 {
			t.Errorf("%s: second prune = (%v, %v), want a no-op", c.name, again, err)
		}
	}
}

func TestJoinAuthorization_PruneWithNoStoreIsANoOp(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	c := newAuthzComb(t, socket, "comb-fresh")
	c.srv.setJoinAuthorizationPath(filepath.Join(t.TempDir(), "absent.json"))

	removed, err := c.srv.PruneSpentJoinAuthorizations(context.Background())
	if err != nil || len(removed) != 0 {
		t.Errorf("PruneSpentJoinAuthorizations() with no store = (%v, %v), want (nil, nil): a Comb that was never installed has nothing to prune", removed, err)
	}
	if _, statErr := os.Stat(c.srv.joinAuthorizationFile()); !os.IsNotExist(statErr) {
		t.Errorf("prune created the store file (stat err = %v); only root's install and join-authorize may create it", statErr)
	}
}
