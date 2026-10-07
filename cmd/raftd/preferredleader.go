package main

import (
	"context"
	"errors"
	"log"
	"time"

	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

// preferredLeaderTickInterval is how often this process asks its own
// Node whether a transfer toward the configured preferred leader is
// due. It is independent of, and shorter than,
// raftnode.PreferredLeaderCooldown - polling often costs nothing (the
// common no-op path is a single raft.State() read) and lets a transfer
// happen promptly once the cooldown clears, rather than only on some
// coarser schedule of its own.
const preferredLeaderTickInterval = 10 * time.Second

// runPreferredLeaderLoop ticks PreferredLeaderTransfer for the
// lifetime of ctx. It never returns an error to its caller: a failed
// or skipped transfer attempt is logged and the loop keeps going,
// exactly as it should for a background bias toward a preference, not
// a required operation - the alternative (treating a transfer failure
// as process-fatal) would mean a preferred node's extended absence
// could take down otherwise-healthy raftd processes on every other
// Comb, which would be a far worse outcome than leadership simply
// staying put.
func runPreferredLeaderLoop(ctx context.Context, node *raftnode.Node, preferredID string) {
	log.Printf("raftd: preferred-leader biasing enabled toward %q (check every %s, cooldown %s)",
		preferredID, preferredLeaderTickInterval, raftnode.PreferredLeaderCooldown)

	ticker := time.NewTicker(preferredLeaderTickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			result, err := node.PreferredLeaderTransfer(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("raftd: preferred-leader transfer: %s (%v)", result.Detail, err)
				continue
			}
			if result.Attempted {
				log.Printf("raftd: preferred-leader transfer: %s", result.Detail)
			}
		}
	}
}
