// Package deadman implements ADR-0101's dead-man's switch for
// uplink-bridged networking: an independent, OS-scheduled fallback that
// forcibly detaches a VM's tap from this node's own uplink bridge if
// nothing confirms the bridge is still healthy in time. It exists because
// nothing else in this codebase's own supervision (managerd's reconciler
// loop, daemon(8)'s -r auto-restart) can recover a node whose own
// management bridge is wedged by the very VM traffic being protected
// against - the process that would normally do the confirming might
// itself be the thing that's stuck. FreeBSD's at(8)/atd(8) (a separate,
// always-running base-system daemon, independent of managerd) is the
// scheduling primitive precisely because it keeps running regardless.
package deadman

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// defaultDelay is how long an unconfirmed tap join is given before it's
// auto-reverted, if Manager.Delay is zero.
const defaultDelay = 5 * time.Minute

// atRunner is the subset of real at(8)/atq(1)/atrm(1) behavior Manager
// needs, factored out so tests can inject a fake with no real OS scheduler
// calls - the same reasoning internal/cluster's Reconciler has for its own
// vlanManager/pfManager interfaces. execAtRunner satisfies this today.
type atRunner interface {
	// at schedules script to run after delay, returning the parsed at(8)
	// job ID.
	at(ctx context.Context, script string, delay time.Duration) (jobID int, err error)
	// atq lists the queue's own raw output (one job per line, job ID
	// first field) - jobPending parses it.
	atq(ctx context.Context) (string, error)
	// atrm cancels jobID. An already-fired or already-removed job is not
	// this method's problem to report - see ConfirmBridgeHealthy.
	atrm(ctx context.Context, jobID int) error
}

// Manager schedules and cancels at(8) jobs that forcibly remove a single
// VM tap from this node's own uplink bridge - see the package doc comment
// above and ADR-0101. Every operation is idempotent, the same convention
// internal/vlan and internal/cluster's own reconciler follow.
type Manager struct {
	// Delay before an unconfirmed tap join is auto-reverted. Defaults to
	// defaultDelay (5 minutes) if zero.
	Delay time.Duration

	// StateDir holds one small JSON file per (bridge, tap) pair recording
	// the pending revert job's at(8) job ID, so ConfirmBridgeHealthy can
	// find and cancel it later - required, no default.
	StateDir string

	// runner is nil in production (real() lazily builds an execAtRunner)
	// and set to a fake only by this package's own tests.
	runner atRunner
}

func (m *Manager) run() atRunner {
	if m.runner != nil {
		return m.runner
	}
	return execAtRunner{}
}

type tapState struct {
	// JobID is the at(8) job number for the pending revert of this tap,
	// as parsed from `at`'s own "job <N> at ..." confirmation line.
	JobID int `json:"job_id"`
}

func (m *Manager) delay() time.Duration {
	if m.Delay <= 0 {
		return defaultDelay
	}
	return m.Delay
}

// statePath returns the state file recording bridge/tap's pending revert
// job, if any. FreeBSD interface names (both bridge and tap) never
// contain '/', so joining them with one is a safe, collision-free key.
func (m *Manager) statePath(bridge, tap string) string {
	return filepath.Join(m.StateDir, "deadman-"+bridge+"-"+tap+".json")
}

// ArmTapRevert schedules (via at(8)) a job that removes tap from bridge
// unless ConfirmBridgeHealthy cancels it first - a no-op if a job is
// already pending for this exact (bridge, tap) pair, checked against
// atq(1) itself (not just this package's own state file) so a job
// surviving a managerd restart is still respected rather than silently
// double-armed.
func (m *Manager) ArmTapRevert(ctx context.Context, bridge, tap string) error {
	if m.StateDir == "" {
		return fmt.Errorf("deadman: StateDir must be set")
	}
	state, err := m.load(bridge, tap)
	if err != nil {
		return err
	}
	if state.JobID != 0 {
		pending, err := m.jobPending(ctx, state.JobID)
		if err != nil {
			return fmt.Errorf("checking pending revert job %d: %w", state.JobID, err)
		}
		if pending {
			return nil
		}
	}
	// The command is idempotent by construction - `ifconfig deletem` on
	// an interface that's already gone (e.g. the VM was cleanly deleted
	// before the revert fired) is a normal, harmless no-op. This never
	// touches the uplink NIC itself, only the single named tap - worst
	// case after a revert, this VM loses network and nothing else does.
	script := fmt.Sprintf("ifconfig %s deletem %s || true\n", bridge, tap)
	jobID, err := m.run().at(ctx, script, m.delay())
	if err != nil {
		return fmt.Errorf("scheduling revert of %s from %s: %w", tap, bridge, err)
	}
	return m.save(bridge, tap, tapState{JobID: jobID})
}

// ConfirmBridgeHealthy cancels (via atrm(1)) the pending revert job for
// (bridge, tap), if any - called once a full reconcile tick completes
// without error and raft connectivity still succeeds, per ADR-0101. A
// later new tap join on the same bridge re-arms a fresh job independently
// - confirming one tap says nothing about a different one.
func (m *Manager) ConfirmBridgeHealthy(ctx context.Context, bridge, tap string) error {
	state, err := m.load(bridge, tap)
	if err != nil {
		return err
	}
	if state.JobID == 0 {
		return nil
	}
	if err := m.run().atrm(ctx, state.JobID); err != nil {
		// The job may have already fired (nothing left to cancel) or been
		// removed some other way - either way there's no pending risk
		// left to report, so this specific failure mode isn't fatal.
		if !strings.Contains(strings.ToLower(err.Error()), "no such job") {
			return fmt.Errorf("cancelling revert job %d for %s/%s: %w", state.JobID, bridge, tap, err)
		}
	}
	return os.Remove(m.statePath(bridge, tap))
}

func (m *Manager) load(bridge, tap string) (tapState, error) {
	data, err := os.ReadFile(m.statePath(bridge, tap))
	if err != nil {
		if os.IsNotExist(err) {
			return tapState{}, nil
		}
		return tapState{}, fmt.Errorf("reading deadman state for %s/%s: %w", bridge, tap, err)
	}
	var state tapState
	if err := json.Unmarshal(data, &state); err != nil {
		return tapState{}, fmt.Errorf("parsing deadman state for %s/%s: %w", bridge, tap, err)
	}
	return state, nil
}

func (m *Manager) save(bridge, tap string, state tapState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp := m.statePath(bridge, tap) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing deadman state for %s/%s: %w", bridge, tap, err)
	}
	return os.Rename(tmp, m.statePath(bridge, tap))
}

// jobPending reports whether jobID still appears in atq(1)'s own queue -
// checked (rather than trusting this package's own state file alone) so a
// job that already fired, or was removed by an operator running atrm(1)
// by hand, is never mistaken for still being armed.
func (m *Manager) jobPending(ctx context.Context, jobID int) (bool, error) {
	out, err := m.run().atq(ctx)
	if err != nil {
		return false, err
	}
	want := strconv.Itoa(jobID)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		// Real FreeBSD atq(1) output is "<date...> <owner> <queue> <job#>"
		// (plus a leading "Date ... Job#" header line) - the job number is
		// the LAST field on a job line, never the first (confirmed live
		// against apiverse/apiarium: "Tue Sep 15 02:10:00 UTC 2026	root
		// c	1"). Checking fields[0] here, as an earlier version of this
		// function did, always compared against a date token and could
		// never match - jobPending would always report false, defeating
		// ArmTapRevert's own idempotency check.
		if len(fields) > 0 && fields[len(fields)-1] == want {
			return true, nil
		}
	}
	return false, nil
}

// jobLinePattern matches at(1)'s own job-confirmation line on stderr, e.g.
// "Job 2 will be executed using /bin/sh". Case-insensitive because real
// FreeBSD at(1) capitalizes "Job" - confirmed live against apiverse/
// apiarium, where a case-sensitive lowercase "job" pattern never matched
// at all, so ArmTapRevert could never successfully parse a job ID on
// either host.
var jobLinePattern = regexp.MustCompile(`(?i)job\s+(\d+)`)

// execAtRunner is the real, production atRunner - it shells out to
// FreeBSD base-system at(8)/atq(1)/atrm(1).
type execAtRunner struct{}

// at schedules `at now + <N> minutes` (rounding delay up to at least one
// minute - at(1) has no sub-minute resolution) with script on stdin,
// returning the job ID at(1) prints in its own "job <N> at <time>..."
// confirmation line, written to stderr rather than stdout.
func (execAtRunner) at(ctx context.Context, script string, delay time.Duration) (int, error) {
	minutes := int(delay.Round(time.Minute) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	cmd := exec.CommandContext(ctx, "at", "now", "+", strconv.Itoa(minutes), "minutes")
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return 0, fmt.Errorf("at: %s", msg)
	}
	match := jobLinePattern.FindStringSubmatch(stderr.String())
	if match == nil {
		return 0, fmt.Errorf("at: could not parse job id from output: %q", stderr.String())
	}
	id, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, fmt.Errorf("at: invalid job id %q: %w", match[1], err)
	}
	return id, nil
}

func (execAtRunner) atq(ctx context.Context) (string, error) {
	return runCmd(ctx, "atq")
}

func (execAtRunner) atrm(ctx context.Context, jobID int) error {
	_, err := runCmd(ctx, "atrm", strconv.Itoa(jobID))
	return err
}

// runCmd executes `name args...` and waits for it to exit, returning
// trimmed stdout. On failure, the returned error includes stderr. Own
// private copy, same convention internal/vlan/internal/hast/internal/bhyve
// each keep rather than sharing one.
func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}
