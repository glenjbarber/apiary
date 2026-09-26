package zfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// CommandRunner is the seam between this package and the local zfs(8)
// binary. Every method the replication layer added in replication.go
// goes through it rather than calling exec directly, so the whole
// package — including the generation fence, the resume-token state
// machine, and the verdict classification — is unit-testable on a
// machine with no zfs(8) and no pool at all.
//
// The interface deliberately mirrors what the pre-existing code did
// with exec.CommandContext in two shapes: Run for small,
// fully-buffered queries (zfs list/get), and Start for the streaming
// `zfs send` cases where stdout can be arbitrarily large and must be
// handed back as a live pipe (ADR-0089's original Send, and the
// incremental/resume sends added alongside it).
type CommandRunner interface {
	// Run executes `zfs <args...>` and returns trimmed stdout. On a
	// non-zero exit it returns an error carrying zfs(8)'s stderr,
	// matching the pre-existing runZFS behaviour so existing callers
	// keep the same diagnosable messages.
	Run(ctx context.Context, args ...string) (stdout string, err error)

	// Start begins `zfs <args...>` and returns a live handle on its
	// stdout. The caller MUST Close the handle; Close waits for the
	// subprocess and reports a non-zero exit as an error. Abandoning
	// the handle leaks a child process, so a start/stream/close shape
	// is the only streaming option offered.
	Start(ctx context.Context, args ...string) (Stream, error)
}

// Stream is a running `zfs send`'s stdout. Read streams the send
// payload live; Close waits for the subprocess and surfaces a non-zero
// exit (with its captured stderr). This is deliberately the same
// contract as the ReadCloser the pre-existing sendReadCloser returns,
// so the replication push path can hand either to the same reader.
type Stream interface {
	io.ReadCloser
}

// execRunner is the production CommandRunner: real exec.CommandContext
// against the zfs(8) in PATH.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "zfs", args...)
	boundWait(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", commandFailure(ctx, args, stderr.String(), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// commandFailure is the single place this package decides whether a
// failed `zfs` invocation produced a *statement about the dataset* or
// produced no answer at all.
//
// The distinction is the whole evidence contract. A non-zero exit with
// zfs(8)'s own stderr — "dataset does not exist", "destination has
// more recent snapshots" — is a statement: it is a failure, and the
// caller must see it as one. Everything else — the binary is missing,
// the context was cancelled and the child was killed, the fork
// failed — is silence, and silence is *UnobservedError. Reporting a
// killed child as a zfs failure would invent a diagnosis; reporting it
// as success would invent health. Both are forbidden.
func commandFailure(ctx context.Context, args []string, stderr string, err error) error {
	cmdline := strings.Join(args, " ")
	if ctx != nil && ctx.Err() != nil {
		return &UnobservedError{
			Op:     cmdline,
			Detail: "zfs was terminated before it could answer: " + ctx.Err().Error(),
			Err:    err,
		}
	}
	// *exec.Error is exec's own "could not start the binary" report.
	// An *exec.ExitError means the binary did run and did exit
	// non-zero, which is a statement even when stderr was empty.
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return &UnobservedError{Op: cmdline, Detail: "zfs could not be executed: " + execErr.Error(), Err: err}
	}
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("zfs %s: %s", cmdline, msg)
}

func (execRunner) Start(ctx context.Context, args ...string) (Stream, error) {
	cmd := exec.CommandContext(ctx, "zfs", args...)
	boundWait(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, unobservedf(strings.Join(args, " "), err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, unobservedf(strings.Join(args, " "), err)
	}
	return &execSendStream{stdout: stdout, cmd: cmd, stderr: &stderr, ctx: ctx, args: args}, nil
}

// execSendStream is the replication layer's handle on a running
// `zfs send`: a live stdout pipe whose Close waits for the child and
// then routes the failure through the same commandFailure decision Run
// and receive use.
//
// It is a sibling of the pre-existing sendReadCloser, not a
// replacement, and the reason is the evidence contract. sendReadCloser
// flattens every failure into "zfs send: <stderr or errno>", which is
// right for its pre-existing caller and wrong here: a send whose
// context was cancelled exits non-zero with an EMPTY stderr, and read
// literally that is "zfs send: signal: killed" — a fabricated zfs
// diagnosis that would make a cancelled run look like a rejected send.
// A killed child is silence, and silence is *UnobservedError.
type execSendStream struct {
	stdout io.ReadCloser
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	ctx    context.Context
	args   []string
}

func (s *execSendStream) Read(p []byte) (int, error) { return s.stdout.Read(p) }

func (s *execSendStream) Close() error {
	s.stdout.Close()
	err := s.cmd.Wait()
	if err == nil {
		return nil
	}
	return commandFailure(s.ctx, s.args, s.stderr.String(), err)
}

// defaultRunner is what a Manager built by New uses.
func defaultRunner() CommandRunner { return execRunner{} }

// UnobservedError marks an operation that produced no usable answer —
// an exec failure that is not a zfs(8) statement about the dataset (the
// binary is missing, the context was cancelled, the peer is
// unreachable), or output that could not be parsed into the shape the
// caller needed.
//
// This is the load-bearing third state in this package. `internal/
// cluster/simulate.go` already insists that "a query was attempted and
// returned nothing usable" is neither healthy nor failed, and
// replica_unobserved exists there to name it. Replication inherits that
// discipline: an UnobservedError must never be folded into
// "success" or into "failed", and any classification built on top of
// it must report unobserved. Callers should test for it with
// errors.As, not by string matching.
type UnobservedError struct {
	// Op is the zfs invocation that could not be answered, e.g.
	// "get -H -o value receive_resume_token pool/ds".
	Op string
	// Detail is the underlying reason, kept verbatim for the operator.
	Detail string
	// Err is the wrapped cause, if any.
	Err error
}

func (e *UnobservedError) Error() string {
	if e == nil {
		return "zfs: unobserved"
	}
	base := fmt.Sprintf("zfs: no usable answer from %q", e.Op)
	if e.Detail != "" {
		base += ": " + e.Detail
	}
	return base
}

func (e *UnobservedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// unobservedf builds an UnobservedError for a command whose failure is
// not a statement about the dataset.
func unobservedf(op string, err error) error {
	return &UnobservedError{Op: op, Detail: detailOf(err), Err: err}
}

func detailOf(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	// Strip the "zfs <args>: " prefix this package's own runners add, so
	// the operator-facing text is zfs(8)'s statement itself where there
	// is one.
	//
	// The cut is at the FIRST ": " after the leading "zfs ", not the
	// last one: zfs(8) messages routinely contain further colons
	// ("cannot open 'pool/ds': dataset does not exist"), and cutting at
	// the last one would reduce an entire sentence to its last word.
	// If some future zfs argument contained ": " the result would be a
	// little more text than strictly needed, which is the harmless
	// direction to be wrong in: this string is evidence, never a
	// decision input.
	if strings.HasPrefix(msg, "zfs ") {
		if i := strings.Index(msg, ": "); i >= 0 {
			return strings.TrimSpace(msg[i+2:])
		}
	}
	return msg
}

// streamWaitDelay bounds how long a killed zfs child may keep this
// process waiting on its I/O before the wait is abandoned.
//
// exec.CommandContext kills the direct child, and the pipes a streaming
// command holds stay open as long as ANY process holds them. For zfs
// (8) alone that is not an issue — it is one process with no children —
// but a hung wait here is the worst shape this package can have: a
// replication receive that never returns would hold its own
// reconciliation slot forever with no verdict to show for it. So the
// wait is bounded, and exceeding the bound is an error like any other,
// classified by the same rule.
//
// It is a var so a test can shorten it; production never reassigns it.
var streamWaitDelay = 20 * time.Second

// boundWait sets WaitDelay on a command whose I/O must not be able to
// hang the caller.
func boundWait(cmd *exec.Cmd) { cmd.WaitDelay = streamWaitDelay }
