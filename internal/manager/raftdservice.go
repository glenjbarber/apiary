package manager

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// raftdBinaryPath mirrors etc/rc.d/apiary_raftd's own raftd_bin variable -
// the fixed path every apiary_raftd rc.d invocation execs, duplicated here
// (rather than shared) since one is a shell variable and the other a Go
// constant with no common package to hold both.
const raftdBinaryPath = "/usr/local/libexec/apiary/raftd"

// raftdResetConfirmPhrase mirrors cmd/raftd/main.go's own
// resetConfirmPhrase exactly - duplicated because cmd/raftd is package
// main and cannot be imported. Keep these two values in sync; a mismatch
// here would make every ConvertStandaloneToJoiner call fail closed
// (raftd's own exact-match check would simply reject the phrase), never
// silently do the wrong thing.
const raftdResetConfirmPhrase = "yes-wipe-raft-state"

// raftdConversionController is the narrow, testable seam
// ConvertStandaloneToJoiner (ADR-0105) uses to stop, reset, and restart
// this node's own raftd, and to confirm it came back up listening at a
// new raft_bind address. Modeled on nodeServiceController's own
// interface-plus-rc-implementation-plus-fake-for-tests shape.
type raftdConversionController interface {
	Stop(ctx context.Context) error
	// Reset moves the existing raft state directory aside via raftd's own
	// -reset one-shot mode (never reimplemented here) and returns the
	// timestamped backup path raftd itself reports.
	Reset(ctx context.Context) (backupPath string, err error)
	Start(ctx context.Context) error
	// IsListening reports whether addr is currently accepting TCP
	// connections - the same reachability primitive dialReachable already
	// uses for ADR-0097's join-approval preflight, applied here to confirm
	// raftd actually came up before this Comb ever submits a join request
	// against it.
	IsListening(ctx context.Context, addr string) (bool, error)
}

type rcRaftdConversionAdapter struct{}

func (rcRaftdConversionAdapter) Stop(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "service", "apiary_raftd", "stop").CombinedOutput()
	if err != nil {
		return fmt.Errorf("service apiary_raftd stop: %s", commandError(err, out))
	}
	return nil
}

func (rcRaftdConversionAdapter) Start(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "service", "apiary_raftd", "start").CombinedOutput()
	if err != nil {
		return fmt.Errorf("service apiary_raftd start: %s", commandError(err, out))
	}
	return nil
}

// resetBackupPathPattern extracts the backup path raftd's own resetDataDir
// logs on success ("raftd: moved existing raft state from <dataDir> to
// <backup>") - see cmd/raftd/main.go's resetDataDir. Parsed from combined
// output rather than recomputed independently here, since recomputing the
// same "<dataDir>.reset-backup-<unix-ts>" format in two places risks the
// two clocks/formats drifting apart; raftd's own log line is the single
// source of truth for what it actually did.
var resetBackupPathPattern = regexp.MustCompile(`moved existing raft state from \S+ to (\S+)`)

func (rcRaftdConversionAdapter) Reset(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, raftdBinaryPath, "-reset", raftdResetConfirmPhrase).CombinedOutput()
	text := string(out)
	if err != nil {
		return "", fmt.Errorf("%s -reset: %s", raftdBinaryPath, commandError(err, out))
	}
	if m := resetBackupPathPattern.FindStringSubmatch(text); m != nil {
		return m[1], nil
	}
	// No "moved" line means raftd's own resetDataDir found nothing at
	// dataDir to move (see its own os.IsNotExist branch) - not an error,
	// just nothing to report as a backup. Callers of Reset are expected to
	// have already confirmed existing state via raftnode.HasExistingState
	// before calling this, so reaching this branch in practice would mean
	// that check and raftd's own view of the data directory disagree -
	// worth surfacing, not silently swallowing.
	return "", fmt.Errorf("%s -reset completed but reported no existing state moved aside: %s", raftdBinaryPath, strings.TrimSpace(text))
}

func (rcRaftdConversionAdapter) IsListening(ctx context.Context, addr string) (bool, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, err
	}
	return true, conn.Close()
}

// raftdListenRetryInterval/raftdListenRetryTimeout bound how long
// ConvertStandaloneToJoiner waits for raftd to actually bind its new
// raft_bind address after Start - a freshly restarted daemon needs a
// moment, but this must still fail closed rather than assume success if
// it never comes up at all.
const (
	raftdListenRetryInterval = 250 * time.Millisecond
	raftdListenRetryTimeout  = 10 * time.Second
)

// waitForRaftdListening polls IsListening until addr accepts connections
// or raftdListenRetryTimeout elapses. Never used to paper over a real
// startup failure: Start's own error, if any, is still checked and
// returned separately before this is ever called.
func waitForRaftdListening(ctx context.Context, ctrl raftdConversionController, addr string) error {
	deadline := time.Now().Add(raftdListenRetryTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := ctrl.IsListening(ctx, addr)
		if ok {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(raftdListenRetryInterval):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("raftd did not come up listening at %s within %s: %w", addr, raftdListenRetryTimeout, lastErr)
	}
	return fmt.Errorf("raftd did not come up listening at %s within %s", addr, raftdListenRetryTimeout)
}
