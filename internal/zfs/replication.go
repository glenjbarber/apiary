package zfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// This file is the transport primitive layer for ADR-0130's ZFS dataset
// replication: incremental `zfs send`, the `<dataset>@apiary-repl-<gen>`
// snapshot convention, the generation fence that refuses to send from a
// snapshot older than the last acked point, resume tokens, and the
// receive-side safety rules.
//
// It deliberately contains NO policy, NO scheduling, NO lease state and
// NO transport. Those need raft (api/internalpb/state.proto) and
// managerd RPCs that this task must not touch. What lives here is the
// part that can be decided locally and honestly: which `zfs send` to
// run, whether a stream is allowed to exist at all, and what a target's
// own observation actually proves.
//
// Three states, everywhere:
//
//   - success, established by a positive statement from zfs(8);
//   - failure, established by a non-zero exit that IS a statement
//     about the dataset ("dataset does not exist", "destination has
//     more recent snapshots");
//   - unknown/unobserved, when no usable answer was produced at all.
//     That is *UnobservedError, and it is never rounded toward either
//     neighbour. See internal/cluster/simulate.go's replica_unobserved
//     doc comment, which this package follows verbatim.

// SnapshotPrefix is the fixed snapshot-name prefix ADR-0130 reserves for
// replication: every replication snapshot is
// `<dataset>@apiary-repl-<gen:08d>`. Zero-padding to 8 digits means the
// names sort lexicographically in generation order, so `zfs list` output
// and a plain string sort both agree with the numeric order.
const SnapshotPrefix = "apiary-repl-"

// snapshotGenRE matches a replication snapshot short name and captures
// the zero-padded generation. Anchored: the name must be exactly the
// prefix plus exactly 8 digits, so a hand-made "apiary-repl-7" or
// "apiary-repl-000000007-extra" is not silently accepted as a
// generation.
var snapshotGenRE = regexp.MustCompile(`^` + SnapshotPrefix + `([0-9]{8})$`)

// ReplSnapshotName returns the snapshot short name for a generation,
// e.g. ReplSnapshotName(7) == "apiary-repl-00000007".
func ReplSnapshotName(gen uint32) string {
	return fmt.Sprintf("%s%08d", SnapshotPrefix, gen)
}

// ParseReplSnapshotName returns the generation encoded in a replication
// snapshot short name. ok is false for any name that is not exactly an
// apiary-repl snapshot of 8 digits, which includes every non-replication
// snapshot in the pool (templates, VM rollback points) and every
// apiary-repl-* name this function did not mint — including a
// 9-or-more-digit one, which is why no generation above
// MaxReplGeneration is ever minted.
//
// Surrounding whitespace is trimmed first, exactly as every other line
// this package reads out of zfs(8) is trimmed; the anchoring then
// applies to what remains. A snapshot whose short name genuinely
// begins or ends with a space is therefore not distinguished from a
// padded one, which is the right trade: ZFS snapshot names of this
// shape are minted by this package, and accepting a padded name is
// worth more than rejecting a pathological dataset.
func ParseReplSnapshotName(name string) (gen uint32, ok bool) {
	m := snapshotGenRE.FindStringSubmatch(strings.TrimSpace(name))
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseUint(m[1], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

// IsReplSnapshot reports whether name is a replication snapshot short
// name of any generation.
func IsReplSnapshot(name string) bool {
	_, ok := ParseReplSnapshotName(name)
	return ok
}

// FenceReason is the outcome of the generation fence. It is a real
// enumeration, not a bool, because "why was this refused" is the
// operator-facing answer and there are four genuinely different reasons.
type FenceReason string

const (
	// FenceAllowNoHistory means nothing has ever been acked for this
	// dataset, so the only correct send is a full one.
	FenceAllowNoHistory FenceReason = "allow_no_history"

	// FenceAllowIncremental means the requested generation is strictly
	// newer than the last acked one, so an incremental send from the
	// last acked snapshot is the correct transport.
	FenceAllowIncremental FenceReason = "allow_incremental"

	// FenceAllowResume means a live resume token observed on the
	// target belongs to this exact pending generation, so the
	// interrupted stream is continued rather than restarted.
	FenceAllowResume FenceReason = "allow_resume"

	// FenceRefuseStale means the requested generation is not strictly
	// newer than the last acked generation. This is the split-brain
	// fence: a stream that would move the target backwards, or replay
	// a generation it already holds, is refused. Nothing here ever
	// downgrades this to a warning.
	FenceRefuseStale FenceReason = "refuse_stale"

	// FenceRefuseNoSnapshot means the fence would permit an
	// incremental send but the last acked snapshot is not available
	// to send from, so the plan escalates to a full `-I` send from
	// the newest older replication snapshot this source still has
	// instead of guessing. Escalation is reported, never silent.
	FenceRefuseNoSnapshot FenceReason = "refuse_incremental_source_missing"

	// FenceRefuseNoGeneration means the run asked to send a stream
	// carrying no generation at all (0), or a generation too large for
	// the 8-digit snapshot name to represent. Generation 0 is this
	// package's "nothing has been acked" sentinel, so a stream minted
	// at 0 would be indistinguishable from "no replication has ever
	// happened" to every later comparison — which would silently
	// reopen the fence. A generation past MaxReplGeneration mints a
	// snapshot name this package can never parse back, so every later
	// comparison would think the snapshot was gone. Both are refused
	// rather than minted.
	FenceRefuseNoGeneration FenceReason = "refuse_no_generation"

	// FenceRefuseNoOrigin means the last acked generation is known
	// but there is no snapshot on this source that a full resync
	// could start from, so the only stream left is a plain
	// `zfs send` from origin. That stream cannot be applied by a
	// destination that already holds a snapshot, and the plan says
	// so in its Note rather than letting the failure look like a
	// broken target.
	FenceRefuseNoOrigin FenceReason = "refuse_no_resync_origin"
)

// ErrFenceRefused is the sentinel every generation-fence refusal wraps,
// so a caller can errors.Is it without matching on message text.
var ErrFenceRefused = errors.New("zfs: generation fence refused the send")

// FenceError is the typed refusal from the generation fence. Requested
// and LastAcked are the two generations that made the decision, and
// LastAckedValid distinguishes "acked generation 0" from "never acked
// anything".
type FenceError struct {
	Reason         FenceReason
	Requested      uint32
	LastAcked      uint32
	LastAckedValid bool
	// Dataset is the base-relative dataset whose fence refused.
	Dataset string
	// Detail is the specific reason for the non-stale refusals, in one
	// operator-readable line. It is evidence, never a decision input.
	Detail string
}

func (e *FenceError) Error() string {
	if e == nil {
		return "zfs: generation fence refused the send"
	}
	switch e.Reason {
	case FenceRefuseStale:
		if e.LastAckedValid {
			return fmt.Sprintf("zfs: generation fence refused %s at generation %d: last acked generation is already %d, and a stream must move the target strictly forward", e.Dataset, e.Requested, e.LastAcked)
		}
		return fmt.Sprintf("zfs: generation fence refused %s: requested generation %d is not a valid starting point", e.Dataset, e.Requested)
	case FenceRefuseNoSnapshot:
		return fmt.Sprintf("zfs: %s cannot be sent incrementally: last acked generation %d has no usable snapshot on this source, so a full send is required", e.Dataset, e.LastAcked)
	case FenceRefuseNoGeneration:
		if e.Detail != "" {
			return fmt.Sprintf("zfs: generation fence refused %s at generation %d: %s", e.Dataset, e.Requested, e.Detail)
		}
		return fmt.Sprintf("zfs: generation fence refused %s at generation %d: that generation cannot be represented as a replication snapshot name", e.Dataset, e.Requested)
	case FenceRefuseNoOrigin:
		return fmt.Sprintf("zfs: %s cannot be sent incrementally: the last acked generation is %d but this source holds no replication snapshot at or before it, so the only remaining stream is a full send from origin that a destination holding a snapshot will reject, and it must not be forced", e.Dataset, e.LastAcked)
	default:
		return fmt.Sprintf("zfs: generation fence refused %s: %s", e.Dataset, e.Reason)
	}
}

func (e *FenceError) Unwrap() error { return ErrFenceRefused }

// SendKind names which `zfs send` invocation a plan calls for.
type SendKind string

const (
	// SendFull is a send of the whole dataset: `zfs send <to>`.
	SendFull SendKind = "full"

	// SendFullIncremental is `zfs send -I <from> <to>`. On FreeBSD
	// (and OpenZFS generally) -I means "send everything from <from>
	// to <to>, including all intermediate snapshots", so it carries
	// the dataset's whole replication history in one stream rather
	// than just the delta. ADR-0130 uses this form for a first
	// receive that has a known origin, and for a full resync after an
	// incremental failure, precisely because the target cannot apply a
	// delta chain it never received.
	SendFullIncremental SendKind = "full_incremental"

	// SendIncremental is `zfs send -i <from> <to>`: a single-dataset
	// delta from one snapshot to another, carrying no intermediate
	// snapshots. This is the steady-state transport for ADR-0130 —
	// every run after the first — because it is the smallest stream
	// that can move the target one generation forward.
	SendIncremental SendKind = "incremental"

	// SendResume is `zfs send -t <token>`: continue an interrupted
	// stream using the resume token the destination minted. It is
	// only valid for the exact (from, to) pair the token was minted
	// for, which is why the fence checks the generation binding
	// before selecting it.
	SendResume SendKind = "resume"
)

// SourceState is what a replication source knows about one dataset: the
// last generation the target acknowledged, and the snapshot that
// generation corresponds to. It is the input to the fence.
//
// HaveLastAcked distinguishes "generation 0 was acked" from "nothing has
// ever been acked", which is a real distinction — the first decides
// between a full and an incremental send.
type SourceState struct {
	// Dataset is the base-relative dataset being replicated.
	Dataset string

	// LastAckedGeneration is the highest generation the target has
	// confirmed holding.
	LastAckedGeneration uint32

	// HaveLastAcked is false when no generation has ever been acked.
	HaveLastAcked bool

	// LastAckedSnapshot is the base-relative "dataset@snapshot" of
	// LastAckedGeneration. Empty (or unset-able) when unknown, which
	// forces a full send rather than an incremental from a guess.
	LastAckedSnapshot string

	// LastAckedSnapshotPresent is the answer to "does
	// LastAckedSnapshot still exist on this source?". It is only
	// consulted after the fence has already permitted an incremental,
	// and a false here escalates to a full send (ADR-0130: "if the
	// last acked snapshot is itself gone, falls back to a full -I
	// send"). When false and LastAckedSnapshot is set, callers should
	// treat the full resync as an operator-visible event.
	LastAckedSnapshotPresent bool

	// FallbackFromSnapshot is the base-relative "dataset@snapshot" of
	// the NEWEST replication snapshot this source still holds whose
	// generation is at or below LastAckedGeneration, or "" when there
	// is none.
	//
	// It is meaningful only when LastAckedSnapshotPresent is false —
	// that is, only when the plan needs a resync origin — so
	// Manager.ObserveSource fills it in only then. It is what makes
	// the escalation above real rather than nominal: `-i <absent
	// snapshot>` and `-I <absent snapshot>` both fail with "previous
	// snapshot not found", so when the last acked snapshot is gone the
	// only resync a destination holding that generation can actually
	// apply is a `-I` from a snapshot it already has or has
	// legitimately skipped over. Picking the newest such snapshot
	// keeps the resync stream as small as the local evidence allows.
	// A caller that only knows the last acked point may leave it empty,
	// which escalates to a plain full send with a Note instead.
	FallbackFromSnapshot string
}

// SendRequest is one run's request to move a target to
// TargetGeneration.
type SendRequest struct {
	// TargetGeneration is the generation this run will create and
	// send. It must be strictly greater than the source's last acked
	// generation or the fence refuses.
	TargetGeneration uint32

	// ObservedResumeToken is the resume token the TARGET reported for
	// this dataset on its most recent observation, if any. It is empty
	// when the target reported none ("-") or when the target's token
	// query returned nothing usable — the caller must not invent a
	// token, and must not pass one it inferred. See
	// ResumeTokenObservation for how the three cases are told apart.
	ObservedResumeToken string
}

// SendPlan is the decided, argument-complete description of one send.
// It is a value, not a command: PlanSend performs no I/O, so the whole
// fence is testable, and Open is the only thing that touches zfs(8).
type SendPlan struct {
	// Kind is which send this plan runs.
	Kind SendKind

	// Fence is why the fence allowed (or escalated) this send.
	Fence FenceReason

	// Dataset is the base-relative dataset.
	Dataset string

	// ToGeneration is the generation this send produces.
	ToGeneration uint32

	// ToSnapshot is the base-relative "dataset@snapshot" being sent.
	ToSnapshot string

	// FromSnapshot is the base-relative incremental origin. Empty for
	// a resume, and empty for a full send that has no origin snapshot.
	FromSnapshot string

	// Token is the resume token, set only for SendResume.
	Token string

	// ResumedFromToken is true when the plan continues an interrupted
	// stream rather than starting a new one, so a caller can tell a
	// resumed run from a fresh one in its own evidence.
	ResumedFromToken bool

	// EscalatedFromIncremental is true when the plan started as an
	// incremental and had to become a full send. ADR-0130 requires
	// this to be visible (full_resync_count) rather than a quiet
	// recovery.
	EscalatedFromIncremental bool

	// Note carries any non-fatal observation the planner made —
	// currently only a discarded resume token. It is surfaced in
	// evidence text rather than being dropped, because a discarded
	// token means the destination may still be in a resumable
	// receive state for a different stream.
	Note string

	// Args is the exact `zfs send` argument vector this plan
	// describes, with base-relative dataset names, ready to be
	// inspected. It is exported so the argument construction itself is
	// assertable in tests without executing anything.
	//
	// It is NOT what Open runs: Open qualifies every dataset name
	// through Manager.path first, so a plan can never escape Base. The
	// qualified form is exactly these args with each relative
	// "dataset@snapshot" replaced by "Base/dataset@snapshot"; a
	// resume plan's args carry no dataset at all, because a token is
	// opaque zfs(8) state.
	Args []string
}

// PlanSend decides which send a run needs, or refuses.
//
// The decision order, and the reasoning:
//
//  1. The requested generation must be strictly greater than the last
//     acked generation. Equal or lower is FenceRefuseStale. This is
//     the fence that makes split-brain impossible: a source that has
//     lost its authority can still reach the target, but it cannot
//     produce a stream that moves the target backwards or replays a
//     generation it already holds. There is no force flag and no
//     "warn and proceed".
//  2. Generation 0 is the "nothing acked" sentinel, so it is never
//     minted as a stream's generation (FenceRefuseNoGeneration).
//  3. A resume token is used only when it belongs to the exact pending
//     generation — that is, when the pending generation is the
//     immediate successor of the last acked one, or the first
//     generation of a policy that has never acked anything. A token
//     observed for any other generation is not a token this run can
//     continue, and the plan escalates to the ordinary incremental
//     instead. The discarded token is reported in the plan, never
//     dropped quietly.
//  4. If there is a last acked generation and its snapshot is present,
//     the send is `zfs send -i <from> <to>`.
//  5. Otherwise the send is full: `zfs send -I <from> <to>` from the
//     newest older replication snapshot this source still has when one
//     is known, and a plain `zfs send <to>` from origin when none is.
//
// A generation GAP (requesting gen 12 when 7 was acked) is allowed, and
// deliberately so. The invariant that matters is strict monotonicity of
// what the target ends up holding, which a gap preserves; a gap just
// means intermediate generations were never sent, which is wasted
// history, not corruption. Refusing gaps would wedge a dataset whose
// runs were skipped or failed, which is a far worse failure than
// skipping them.
func PlanSend(state SourceState, req SendRequest) (SendPlan, error) {
	if state.Dataset == "" {
		return SendPlan{}, fmt.Errorf("zfs: replication source state needs a dataset")
	}
	toGen := req.TargetGeneration
	toSnap := state.Dataset + "@" + ReplSnapshotName(toGen)
	plan := SendPlan{Dataset: state.Dataset, ToGeneration: toGen, ToSnapshot: toSnap}

	// 1. the fence proper.
	if state.HaveLastAcked && toGen <= state.LastAckedGeneration {
		return SendPlan{}, &FenceError{
			Reason:         FenceRefuseStale,
			Requested:      toGen,
			LastAcked:      state.LastAckedGeneration,
			LastAckedValid: true,
			Dataset:        state.Dataset,
		}
	}

	// 2. a generation this package cannot represent as a snapshot
	// name is never minted. Generation 0 is the "nothing acked"
	// sentinel; anything past MaxReplGeneration would produce a
	// 9-or-more-digit name that ParseReplSnapshotName rejects, so
	// every later comparison would conclude the snapshot was gone.
	if toGen == 0 {
		return SendPlan{}, &FenceError{
			Reason:    FenceRefuseNoGeneration,
			Requested: toGen,
			Dataset:   state.Dataset,
			Detail:    "generation 0 is the 'nothing has been acked' sentinel and must never be sent, because a stream carrying it would be indistinguishable from no replication at all",
		}
	}
	if toGen > MaxReplGeneration {
		return SendPlan{}, &FenceError{
			Reason:    FenceRefuseNoGeneration,
			Requested: toGen,
			Dataset:   state.Dataset,
			Detail:    fmt.Sprintf("the %q snapshot convention is 8 zero-padded digits, so generation %d has no representable snapshot name and every later check would conclude the snapshot was gone", SnapshotPrefix, MaxReplGeneration),
		}
	}

	// 3. resume, but only for the stream that is actually in flight:
	// the immediate successor of the last acked generation, or the
	// first generation of a policy that has never acked anything. A
	// token is minted by the destination's own interrupted receive,
	// so it can only continue the one stream that receive belongs to.
	token := strings.TrimSpace(req.ObservedResumeToken)
	if token != "" {
		if resumeBindsToPending(state, toGen) {
			plan.Kind = SendResume
			plan.Fence = FenceAllowResume
			plan.Token = token
			plan.ResumedFromToken = true
			plan.Args = []string{"send", "-t", token}
			return plan, nil
		}
		// A token for any other generation cannot continue this run.
		// Fall through to the normal decision, and record the discard
		// so the receive side knows the destination may still be
		// holding a resumable state for a *different* stream.
		plan.Note = fmt.Sprintf("discarded resume token: it was minted for a stream other than generation %d to %d, so it cannot continue this run", state.LastAckedGeneration, toGen)
	}

	// 4. incremental, when there is a last acked snapshot to send from.
	if state.HaveLastAcked && state.LastAckedSnapshot != "" && state.LastAckedSnapshotPresent {
		plan.Kind = SendIncremental
		plan.Fence = FenceAllowIncremental
		plan.FromSnapshot = state.LastAckedSnapshot
		plan.Args = []string{"send", "-i", state.LastAckedSnapshot, toSnap}
		return plan, nil
	}

	// 5. full. The last acked generation existed, so any full send
	// here is an escalation of an incremental that could not be
	// produced, and it is flagged as one.
	if !state.HaveLastAcked {
		plan.Kind = SendFull
		plan.Fence = FenceAllowNoHistory
		plan.Args = []string{"send", toSnap}
		return plan, nil
	}

	plan.EscalatedFromIncremental = true
	if fallback := strings.TrimSpace(state.FallbackFromSnapshot); fallback != "" && fallback != state.LastAckedSnapshot {
		// A `-I` from a replication snapshot the target is known to
		// be at or behind is the only resync it can apply. This is the
		// ADR-0130 full-resync path: operator-visible, never quiet.
		plan.Kind = SendFullIncremental
		plan.Fence = FenceRefuseNoSnapshot
		plan.FromSnapshot = fallback
		plan.Args = []string{"send", "-I", fallback, toSnap}
		plan.Note = appendNote(plan.Note, fmt.Sprintf(
			"full resync from %s: the last acked snapshot %s is no longer on this source, so the delta cannot be produced and the whole chain from %s is being resent",
			fallback, orNone(state.LastAckedSnapshot), fallback))
		return plan, nil
	}
	plan.Kind = SendFull
	plan.Fence = FenceRefuseNoOrigin
	plan.Args = []string{"send", toSnap}
	plan.Note = appendNote(plan.Note, fmt.Sprintf(
		"full send from origin: this source holds no replication snapshot at or before the last acked generation %d, so no incremental stream can be produced; a destination that already holds a snapshot will reject this and it must not be forced",
		state.LastAckedGeneration))
	return plan, nil
}

// resumeBindsToPending reports whether a live resume token observed on
// the destination can only belong to the stream this run is about to
// send.
//
// The token was minted by the destination's own interrupted receive,
// so it describes exactly one in-flight stream. It is usable when this
// run is that stream: either the pending generation immediately
// follows the last acked one, or nothing has ever been acked and this
// is the first generation (the only stream a first receive could have
// been). In every other case the in-flight receive belongs to a
// different generation, and continuing it with a stream from here
// would splice two unrelated streams together.
func resumeBindsToPending(state SourceState, toGen uint32) bool {
	if !state.HaveLastAcked {
		return toGen == 1
	}
	return toGen == state.LastAckedGeneration+1
}

func appendNote(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unknown)"
	}
	return s
}

// Open starts the send described by the plan, qualifying every
// base-relative dataset name with the Manager's Base exactly as the
// pre-existing Send does, and streaming stdout live.
//
// The caller MUST Close the returned Stream: Close waits for `zfs send`
// to exit and turns a non-zero exit into an error. A run that abandons
// the stream leaves a child process behind and loses the only signal
// that the send failed.
func (m *Manager) Open(ctx context.Context, plan SendPlan) (Stream, error) {
	args, err := m.qualifyPlan(plan)
	if err != nil {
		return nil, err
	}
	return m.runnerOrDefault().Start(ctx, args...)
}

// qualifyPlan renders a plan into a real `zfs send` argument vector by
// expanding the base-relative dataset names through path()/snapshotPath
// so a plan can never escape Base and the emitted arguments are exactly
// what Open will run.
func (m *Manager) qualifyPlan(plan SendPlan) ([]string, error) {
	toSnap, err := m.snapshotPath(plan.ToSnapshot)
	if err != nil {
		return nil, err
	}
	switch plan.Kind {
	case SendResume:
		token := strings.TrimSpace(plan.Token)
		if token == "" {
			return nil, fmt.Errorf("zfs: resume plan for %s has an empty token", plan.Dataset)
		}
		return []string{"send", "-t", token}, nil
	case SendIncremental, SendFullIncremental:
		from, err := m.snapshotPath(plan.FromSnapshot)
		if err != nil {
			return nil, err
		}
		flag := "-i"
		if plan.Kind == SendFullIncremental {
			flag = "-I"
		}
		return []string{"send", flag, from, toSnap}, nil
	case SendFull:
		return []string{"send", toSnap}, nil
	default:
		return nil, fmt.Errorf("zfs: unknown send kind %q", plan.Kind)
	}
}

// SendIncremental starts `zfs send -i from to` — a single-dataset delta
// carrying no intermediate snapshots. This is ADR-0130's steady-state
// transport. from and to are base-relative "dataset@snapshot" names.
func (m *Manager) SendIncremental(ctx context.Context, from, to string) (Stream, error) {
	fromFull, err := m.snapshotPath(from)
	if err != nil {
		return nil, err
	}
	toFull, err := m.snapshotPath(to)
	if err != nil {
		return nil, err
	}
	return m.runnerOrDefault().Start(ctx, "send", "-i", fromFull, toFull)
}

// SendAllIncremental starts `zfs send -I from to` — from and to
// inclusive, with every intermediate snapshot. ADR-0130 uses this for
// a first receive with a known origin and for a full resync after an
// incremental failure, because a target that never received the
// intermediate snapshots cannot apply a plain -i chain.
func (m *Manager) SendAllIncremental(ctx context.Context, from, to string) (Stream, error) {
	fromFull, err := m.snapshotPath(from)
	if err != nil {
		return nil, err
	}
	toFull, err := m.snapshotPath(to)
	if err != nil {
		return nil, err
	}
	return m.runnerOrDefault().Start(ctx, "send", "-I", fromFull, toFull)
}

// SendResume starts `zfs send -t <token>`, continuing an interrupted
// stream from the token the destination minted. The token is opaque
// zfs(8) state and is never parsed here; an empty or whitespace-only
// token is refused rather than turned into a malformed command.
func (m *Manager) SendResume(ctx context.Context, token string) (Stream, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("zfs: resume token must not be empty")
	}
	return m.runnerOrDefault().Start(ctx, "send", "-t", token)
}

// SendFrom starts the full-history send `zfs send -I <from> <to>` for
// the base-relative snapshots from and to.
func (m *Manager) SendFrom(ctx context.Context, from, to string) (Stream, error) {
	return m.SendAllIncremental(ctx, from, to)
}

// ResumeTokenState is the resume-token state machine's vocabulary.
// It is deliberately four states, not a string-or-empty and not a bool:
// the difference between "the destination says there is no token" and
// "we could not ask" is the entire point of this feature's evidence
// discipline.
type ResumeTokenState string

const (
	// ResumeTokenNone is a CONFIRMED absence: zfs(8) answered, and the
	// answer was "-" (zfs's own "no token" value). The destination is
	// not in a resumable receive state.
	ResumeTokenNone ResumeTokenState = "none"

	// ResumeTokenLive is a CONFIRMED token: zfs(8) answered with a
	// non-"-" value, meaning a receive on that destination was
	// interrupted and is resumable.
	ResumeTokenLive ResumeTokenState = "live"

	// ResumeTokenUnknown is the absence of an answer. A failed or
	// cancelled `zfs get`, or output that could not be understood, is
	// unknown — never "no token", and never "the receive is fine".
	ResumeTokenUnknown ResumeTokenState = "unknown"

	// ResumeTokenStale is a locally persisted token that the live
	// destination no longer reports: the destination dataset was
	// destroyed or the token expired. Confirmed dead, and safe to
	// clear from the local store.
	ResumeTokenStale ResumeTokenState = "stale"
)

// ResumeTokenObservation is one observation of a destination dataset's
// receive_resume_token property. State carries the three-way answer;
// Token is populated only when State is ResumeTokenLive; Raw is zfs(8)'s
// own text, kept for the operator-facing evidence string.
type ResumeTokenObservation struct {
	// Dataset is the base-relative dataset queried.
	Dataset string
	// State is the three/four-way verdict on the token.
	State ResumeTokenState
	// Token is the live token, set only when State is ResumeTokenLive.
	Token string
	// Raw is zfs(8)'s verbatim output, "-" included, or the failure
	// text when State is ResumeTokenUnknown.
	Raw string
	// Detail explains a non-live state in one operator-readable line.
	Detail string
}

// Live reports whether a usable token was observed. It is false for
// ResumeTokenUnknown as well as ResumeTokenNone, because "no usable
// token" and "no token" are different claims about the destination.
func (o ResumeTokenObservation) Live() bool { return o.State == ResumeTokenLive }

// Observed reports whether a token query produced a usable answer at
// all. It is false for ResumeTokenUnknown AND for the zero value,
// because an observation that carries no state at all is a caller that
// never asked, and "never asked" is not "asked, and there is none".
func (o ResumeTokenObservation) Observed() bool {
	switch o.State {
	case ResumeTokenNone, ResumeTokenLive, ResumeTokenStale:
		return true
	default:
		return false
	}
}

// ObservedNoToken returns the confirmed-absence observation: zfs(8)
// answered and reported no resumable receive. It is a constructor
// rather than a struct literal so that a caller building an
// observation by hand has to name the state, which is what keeps the
// zero value meaning "never asked".
func ObservedNoToken(dataset string) ResumeTokenObservation {
	return ResumeTokenObservation{
		Dataset: dataset,
		State:   ResumeTokenNone,
		Raw:     "-",
		Detail:  "zfs reports no receive_resume_token: this destination is not in a resumable receive state",
	}
}

// ObservedLiveToken returns the confirmed-token observation for a
// destination whose receive was interrupted and can be resumed.
func ObservedLiveToken(dataset, token string) ResumeTokenObservation {
	return ResumeTokenObservation{
		Dataset: dataset,
		State:   ResumeTokenLive,
		Token:   token,
		Raw:     token,
		Detail:  "a receive on this destination was interrupted and is resumable",
	}
}

// ObservedUnknownToken returns the "could not check" observation. raw
// and detail carry whatever zfs(8) or the transport actually said, so
// an unanswerable query is still visible in evidence text instead of
// being flattened into a bare flag.
func ObservedUnknownToken(dataset, raw, detail string) ResumeTokenObservation {
	return ResumeTokenObservation{
		Dataset: dataset,
		State:   ResumeTokenUnknown,
		Raw:     raw,
		Detail:  detail,
	}
}

// ResumeToken(ctx, destName) reads the destination's
// receive_resume_token and classifies it.
//
// This is the single place the three states are produced from a raw
// command, and it is where the ADR-0130 rule lives: "-" means no
// token, a non-"-" value means a confirmed interrupted receive, and
// anything else means unknown. A missing destination dataset is the
// confirmed no-token case (there is nothing to resume into) and is
// reported as ResumeTokenNone with a Detail, never as an error.
//
// destName is validated exactly like every other dataset name, and the
// full Base-relative path is used, matching the pre-existing property
// getters.
func (m *Manager) ResumeToken(ctx context.Context, destName string) (ResumeTokenObservation, error) {
	full, err := m.path(destName)
	if err != nil {
		return ResumeTokenObservation{}, err
	}
	args := []string{"get", "-H", "-o", "value", "receive_resume_token", full}
	out, err := m.runnerOrDefault().Run(ctx, args...)
	if err != nil {
		// A destination that does not exist at all is a confirmed
		// "no resumable receive in progress", not an unknown. Any
		// other failure is unknown: we asked and got nothing usable.
		if strings.Contains(detailOf(err), "dataset does not exist") {
			return ResumeTokenObservation{
				Dataset: destName,
				State:   ResumeTokenNone,
				Raw:     "-",
				Detail:  "destination dataset does not exist, so no receive is in progress",
			}, nil
		}
		obs := ResumeTokenObservation{
			Dataset: destName,
			State:   ResumeTokenUnknown,
			Detail:  "zfs get receive_resume_token could not be answered: " + detailOf(err),
		}
		return obs, &UnobservedError{Op: strings.Join(args, " "), Detail: obs.Detail, Err: err}
	}

	raw := strings.TrimSpace(out)
	switch {
	case raw == "", raw == "-":
		return ResumeTokenObservation{
			Dataset: destName,
			State:   ResumeTokenNone,
			Raw:     raw,
			Detail:  "zfs reports no receive_resume_token: this destination is not in a resumable receive state",
		}, nil
	case !isPlausibleToken(raw):
		// Output we cannot interpret is unknown, not absence. This
		// is the same discipline internal/cluster/simulate.go applies
		// to an unrecognised hastd role: a new zfs(8) format must not
		// be reported as a confirmed healthy destination.
		obs := ResumeTokenObservation{
			Dataset: destName,
			State:   ResumeTokenUnknown,
			Raw:     raw,
			Detail:  "zfs reported a receive_resume_token this code does not recognise, so the destination's resume state is undetermined",
		}
		return obs, &UnobservedError{Op: strings.Join(args, " "), Detail: obs.Detail}
	default:
		return ResumeTokenObservation{
			Dataset: destName,
			State:   ResumeTokenLive,
			Token:   raw,
			Raw:     raw,
			Detail:  "a receive on this destination was interrupted and is resumable",
		}, nil
	}
}

// isPlausibleToken applies a deliberately weak structural check to a
// resume token: printable, single-line, and not one of the values zfs
// itself uses to mean "nothing here". A real token is long base64-ish
// text; the point is only to catch a multi-line or placeholder answer
// so it is classified as unknown instead of being handed to `zfs send -t`.
func isPlausibleToken(s string) bool {
	if s == "" || len(s) > 4096 {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	switch strings.ToLower(s) {
	case "-", "none", "null", "(null)", "unavailable", "unknown":
		return false
	}
	return true
}

// ErrDestResumable is the sentinel ReceiveInto returns when the
// destination is mid-receive and the caller gave it neither a resume
// token nor an explicit -F rollback. ZFS itself would reject a bare
// receive on such a dataset; refusing before spawning the command
// turns a confusing downstream error into a typed, actionable one.
var ErrDestResumable = errors.New("zfs: destination has a live receive resume token")

// ErrResumeTokenStale is the sentinel ReceiveInto returns when the
// caller supplied a resume token but the destination itself reports no
// receive in progress. The token is dead — the destination was
// destroyed, or the token expired — and continuing to treat it as
// usable would restart a full receive on top of a half-populated
// dataset.
//
// It is a distinct sentinel from ErrDestResumable on purpose: one
// means "there is an interrupted receive and you have not addressed
// it", the other means "the address you were given no longer exists".
// Both are failures, and neither is ever reported as success or as
// unobserved, because both were established by a positive answer from
// zfs(8).
var ErrResumeTokenStale = errors.New("zfs: the supplied resume token is stale: the destination reports no receive in progress")

// ResumableDestError carries the live token found on a destination that
// was refused a bare receive. The token itself is NOT included, because
// this error is routinely surfaced in status text; the caller that
// wants the token re-observes it through ResumeToken or the local token
// store.
type ResumableDestError struct {
	Dataset string
	State   ResumeTokenState
	Detail  string
}

func (e *ResumableDestError) Error() string {
	if e == nil {
		return ErrDestResumable.Error()
	}
	return fmt.Sprintf("%s: %s is mid-receive; resume it with its token or overwrite it with -F", ErrDestResumable.Error(), e.Dataset)
}

func (e *ResumableDestError) Unwrap() error { return ErrDestResumable }

// ReceiveOptions configures ReceiveInto. Every flag is opt-in and
// off by default: ADR-0130's receive path runs with -u so the source's
// mountpoint is never used, and deliberately never with -F, because a
// force-rollback throws away the destination's data and must be an
// explicit human decision.
type ReceiveOptions struct {
	// NoMount passes -u: the received dataset is left unmounted so the
	// source's absolute mountpoint path is never used. The target sets
	// its own mountpoint afterwards from its own configuration.
	NoMount bool

	// Force passes -F, which rolls the destination back to the
	// snapshot the incoming stream starts from. This destroys the
	// destination's newer data and is the only way past a
	// destination that has snapshots newer than the stream. Nothing
	// in this package sets it implicitly; a caller must ask.
	Force bool

	// ResumeToken passes -t, continuing an interrupted receive. It is
	// mutually exclusive with Force: a -F receive has nothing to
	// resume, and asking for both is a caller bug, rejected here
	// rather than passed to zfs(8) to sort out.
	ResumeToken string

	// SkipResumeCheck disables the pre-flight receive_resume_token
	// query. It exists for the one caller that has just observed the
	// token itself and would otherwise pay for a second query, and it
	// makes the caller responsible for having done so. With it set,
	// an unobserved token state is no longer detected here.
	SkipResumeCheck bool
}

// ReceiveInto runs `zfs receive <dest>` with its stdin fed from r,
// blocking until r is fully drained, with the same Base-relative
// dataset validation and the same stderr-surfaced error format
// ("zfs receive <dataset>: <stderr>") as the pre-existing Receive, so
// failures stay exactly as diagnosable as they were.
//
// It is a sibling of Receive, not a replacement: Receive's signature is
// untouched and still used by ADR-0089's template push.
//
// Pre-flight: unless SkipResumeCheck is set, this always observes the
// destination's receive_resume_token first, because ADR-0130's failure
// modes are explicit that "the next receive on that dataset must be a
// -t resume or a -F overwrite, never a bare receive". The three
// outcomes are kept distinct:
//
//   - a live token with no ResumeToken and no Force -> ErrDestResumable;
//   - no token -> the receive proceeds;
//   - the token could not be observed -> the receive is REFUSED with
//     an *UnobservedError. Refusing is the conservative choice and it
//     is the honest one: proceeding would mean starting a bare receive
//     while not knowing whether the destination is mid-receive.
func (m *Manager) ReceiveInto(ctx context.Context, destName string, r io.Reader, opts ReceiveOptions) error {
	full, err := m.path(destName)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(opts.ResumeToken)
	if token != "" && opts.Force {
		return fmt.Errorf("zfs receive %s: opts.Force and opts.ResumeToken are mutually exclusive", full)
	}

	if !opts.SkipResumeCheck {
		obs, err := m.ResumeToken(ctx, destName)
		switch {
		case err != nil && obs.State != ResumeTokenNone:
			// Unknown, or a dataset-does-not-exist answer we could not
			// use. Refuse: a bare receive onto a destination whose
			// resume state we do not know is exactly the operation ZFS
			// rejects, and guessing would be dishonest.
			return &UnobservedError{
				Op:     "receive " + full,
				Detail: "refusing to receive into " + full + ": its receive_resume_token could not be observed (" + obs.Detail + ")",
				Err:    err,
			}
		case obs.State == ResumeTokenNone:
			// Confirmed: nothing to resume. If the caller supplied a
			// token anyway, the token is dead (the destination was
			// destroyed or it expired) and zfs(8) would reject it.
			if token != "" {
				return fmt.Errorf("%w: %s", ErrResumeTokenStale, full)
			}
		case obs.Live():
			// A resumable receive is in progress.
			if token != "" {
				if token != obs.Token {
					return fmt.Errorf("zfs receive %s: the supplied resume token does not match the destination's live receive_resume_token", full)
				}
			} else if !opts.Force {
				return &ResumableDestError{Dataset: destName, State: obs.State, Detail: obs.Detail}
			}
		}
	}

	args := []string{"receive"}
	if opts.NoMount {
		args = append(args, "-u")
	}
	if opts.Force {
		args = append(args, "-F")
	}
	if token != "" {
		args = append(args, "-t", token)
	}
	args = append(args, full)

	cmd := m.runnerOrDefault()
	if starter, ok := cmd.(receiveStarter); ok {
		return starter.receive(ctx, args, r)
	}
	// A CommandRunner that cannot stream stdin into a receive is not
	// usable for one; fail loudly rather than handing zfs(8) an
	// unread stream.
	return fmt.Errorf("zfs: runner %T cannot pipe stdin into %s", cmd, strings.Join(args, " "))
}

// ErrDestinationAhead is the sentinel CheckDestination returns when
// the destination already holds a snapshot NEWER than the stream
// being sent, so the stream would move it backwards.
//
// There is no force flag on this path, deliberately and structurally:
// CheckDestination returns an error, and the only thing a caller can
// do with that error is refuse the receive. The one legitimate way
// past it — `zfs receive -F`, which destroys the destination's newer
// data — is a human decision reached by setting ReceiveOptions.Force
// with full knowledge of what that dataset holds, and it is not
// reachable from an observation.
var ErrDestinationAhead = errors.New("zfs: destination already holds newer snapshots than the incoming stream")

// DestinationAheadError carries the two generations that made the
// refusal, so the operator can see exactly how far apart they are and
// decide what the destination's newer data actually is.
type DestinationAheadError struct {
	Dataset string
	// Held is the highest replication generation on the destination.
	Held uint32
	// StreamOrigin is the generation the incoming stream starts from,
	// 0 for a stream from origin.
	StreamOrigin uint32
	// FromOrigin is true when the incoming stream is a plain full send
	// with no incremental origin at all.
	FromOrigin bool
	// Reason is one operator-readable line.
	Reason string
}

func (e *DestinationAheadError) Error() string {
	if e == nil {
		return ErrDestinationAhead.Error()
	}
	what := fmt.Sprintf("origin %d", e.StreamOrigin)
	if e.FromOrigin {
		what = "origin (a full send with no incremental origin)"
	}
	return fmt.Sprintf("%s: %s holds apiary-repl-%08d and the incoming stream starts from %s, so receiving it would move the destination backwards; resolve the destination's newer snapshots deliberately, and never with an automatic -F", ErrDestinationAhead.Error(), e.Dataset, e.Held, what)
}

func (e *DestinationAheadError) Unwrap() error { return ErrDestinationAhead }

// CheckDestination is the target-side admission check: may this stream
// be received onto this destination at all?
//
// obs is the target's own observation of itself, gathered node-locally
// and never forwarded through raft (ADR-0130 §6). origin is the
// generation the incoming stream starts from, and haveOrigin false
// means the stream is a plain full send from origin, which has no
// generation to compare.
//
// The three refusals, and each is a distinct fact:
//
//   - the destination's own state could not be observed ->
//     *UnobservedError. Receiving anyway would mean starting a receive
//     onto a dataset whose resume state is unknown, which is exactly
//     the operation ZFS rejects and exactly the guess this package
//     refuses to make.
//   - the destination is mid-receive -> ErrDestResumable. The
//     in-flight stream must be continued with its own token or
//     discarded by a human; a fresh receive is never the answer.
//   - the destination is strictly ahead of the stream's origin ->
//     ErrDestinationAhead. This is the "destination already has newer
//     snapshots" case, and it is refused, never forced.
//
// A destination whose newest snapshot EQUALS the stream's origin is
// allowed: that is the ordinary incremental case, and it is the only
// way an incremental stream can ever be received.
func CheckDestination(obs TargetObservation, origin uint32, haveOrigin bool) error {
	if !obs.DatasetObserved {
		detail := obs.Detail
		if detail == "" {
			detail = "the target did not report whether its dataset exists"
		}
		return &UnobservedError{
			Op:     "receive " + obs.Dataset,
			Detail: "refusing to receive onto " + obs.Dataset + ": its state could not be observed (" + detail + ")",
		}
	}
	if !obs.Token.Observed() {
		detail := obs.Token.Detail
		if detail == "" {
			detail = "no receive_resume_token query result was supplied"
		}
		return &UnobservedError{
			Op:     "receive " + obs.Dataset,
			Detail: "refusing to receive onto " + obs.Dataset + ": its receive_resume_token state is unknown (" + detail + ")",
		}
	}
	if obs.Token.Live() {
		return &ResumableDestError{Dataset: obs.Dataset, State: obs.Token.State, Detail: obs.Token.Detail}
	}
	if !obs.DatasetExists || !obs.HaveHeld {
		// Nothing to be ahead of. A first receive into a fresh
		// destination is the normal path.
		return nil
	}
	if haveOrigin && obs.HeldGeneration > origin {
		return &DestinationAheadError{
			Dataset:      obs.Dataset,
			Held:         obs.HeldGeneration,
			StreamOrigin: origin,
			Reason:       "the destination is strictly ahead of the stream's origin",
		}
	}
	return nil
}

// receiveStarter is the stdin-streaming half of CommandRunner that
// ReceiveInto needs. It is separate from CommandRunner because the
// pure query/stream surface is enough for everything except a receive,
// and requiring it of every runner would make the fakes worse.
type receiveStarter interface {
	receive(ctx context.Context, args []string, stdin io.Reader) error
}

func (execRunner) receive(ctx context.Context, args []string, stdin io.Reader) error {
	cmd := exec.CommandContext(ctx, "zfs", args...)
	boundWait(cmd)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Same three-way decision as Run: a non-zero exit carrying
		// zfs(8)'s own stderr ("destination has more recent
		// snapshots", "could not find snapshot") is a statement about
		// the dataset and stays a plain failure; a killed or
		// unrunnable child is silence and is unobserved. This matters
		// most on receive, because a receive killed mid-stream leaves
		// a *resumable* destination: reporting that as a clean
		// failure would invite a caller to retry with -F.
		return commandFailure(ctx, args, stderr.String(), err)
	}
	return nil
}

// ReplicaVerdict is the replication-side answer vocabulary, kept
// deliberately separate from internal/cluster's RecoveryVerdict so a
// HAST answer can never be read as a replication answer and vice versa
// (ADR-0130 §6). The values are the ADR's own.
//
// The load-bearing rules, inherited verbatim from ADR-0130 and
// internal/cluster/simulate.go:
//
//  1. ReplicaUnobserved means a query was attempted and returned
//     nothing usable. Not healthy, not failed, not in-sync, not
//     out-of-sync.
//  2. A stale replica never masquerades as healthy: "the target
//     answered" is not "the target is current". Freshness is the
//     generation comparison, and only the generation comparison.
type ReplicaVerdict string

const (
	// ReplicaCurrent: the target was queried, holds the expected
	// generation, and reports no live resume token.
	ReplicaCurrent ReplicaVerdict = "replica_current"

	// ReplicaStale: the target was queried and demonstrably holds an
	// older generation — or demonstrably holds nothing at all when a
	// generation was expected.
	ReplicaStale ReplicaVerdict = "replica_stale"

	// ReplicaPartialReceive: the target was queried and reports a live
	// receive_resume_token. A CONFIRMED interrupted receive, distinct
	// from both "current" and "could not check".
	ReplicaPartialReceive ReplicaVerdict = "replica_partial_receive"

	// ReplicaUnobserved: a query was attempted and returned nothing
	// usable. The silence is a state with its own verdict.
	ReplicaUnobserved ReplicaVerdict = "replica_unobserved"

	// ReplicaUnprotected: no policy exists for this dataset.
	ReplicaUnprotected ReplicaVerdict = "replica_unprotected"

	// ReplicaDisabled: a policy exists but is disabled — by the
	// feature flag or a key verifier mismatch.
	ReplicaDisabled ReplicaVerdict = "replica_disabled"

	// ReplicaAheadOfSource: the target was queried and holds a
	// generation STRICTLY NEWER than the one the source expects it to
	// hold. This is not in ADR-0130's table and is added here on
	// purpose: it is a confirmed, resolved observation that is neither
	// current nor stale, and collapsing it into either neighbour would
	// be the exact dishonesty the surrounding rules forbid. The usual
	// cause is an operator RepointReplicationPolicy having been
	// committed elsewhere, or a source whose view of last_acked is
	// behind the target.
	ReplicaAheadOfSource ReplicaVerdict = "replica_ahead_of_source"
)

// TargetObservation is what a target node reports about the replica it
// holds, gathered node-locally and never forwarded through raft
// (ADR-0130 §6). Zero values are NOT valid observations; the caller
// must set Observed to say whether a query happened at all.
type TargetObservation struct {
	// Dataset is the base-relative dataset on the target.
	Dataset string

	// NodeID is the node that produced this observation, for evidence
	// text.
	NodeID string

	// Observed is false when no query was attempted, or the query
	// produced nothing usable. This is the only field that can produce
	// ReplicaUnobserved, and it is what keeps "we never looked"
	// separate from "we looked and the copy is old".
	Observed bool

	// DatasetObserved is true when the target's own dataset existence
	// was actually established. When Observed is true but this is
	// false, the dataset's existence is genuinely undetermined and the
	// classification is unobserved, not stale.
	DatasetObserved bool

	// DatasetExists is meaningful only when DatasetObserved.
	DatasetExists bool

	// HeldGeneration is the highest apiary-repl-<gen> snapshot the
	// target's dataset holds. Meaningful only when HaveHeld.
	HaveHeld       bool
	HeldGeneration uint32

	// Token is the target's receive_resume_token observation.
	Token ResumeTokenObservation

	// Detail is the observation's own raw/failure text, kept for
	// evidence. It is never used to upgrade a verdict.
	Detail string
}

// Verdict classifies a target observation against the generation the
// source expects it to hold.
//
// expectedGeneration is the source's own last_acked_generation. Pass 0
// with expectedKnown=false to say "nothing is expected on the target
// yet" (a policy that has never completed a run), which is the only
// case in which an absent dataset counts as current.
func Classify(obs TargetObservation, expectedGeneration uint32, expectedKnown bool) (ReplicaVerdict, string) {
	if !obs.Observed {
		return ReplicaUnobserved, explainUnobserved(obs, "the target was not queried at all")
	}
	if obs.Token.State == ResumeTokenUnknown {
		return ReplicaUnobserved, explainUnobserved(obs, "the target answered, but its receive_resume_token state could not be established: "+obs.Token.Detail)
	}
	if !obs.Token.Observed() {
		// The zero value: a caller that read the generation and the
		// dataset but never asked about the resume token. Treating
		// that as "no token" would let an unanswered question
		// upgrade a partial observation into a verdict, which is the
		// one thing this function exists to prevent.
		return ReplicaUnobserved, explainUnobserved(obs, "the target's receive_resume_token state was never established, and never asking is not the same as asking and hearing that there is none")
	}
	if !obs.DatasetObserved {
		return ReplicaUnobserved, explainUnobserved(obs, "the target answered, but whether the dataset exists there could not be established: "+obs.Detail)
	}
	// A live resume token is a confirmed interrupted receive. It wins
	// over the generation comparison, because a destination mid-
	// receive is not holding the last acked generation cleanly no
	// matter what snapshots it lists.
	if obs.Token.Live() {
		return ReplicaPartialReceive, "the target holds a live receive_resume_token: a receive was interrupted on this dataset and is resumable, so the copy is not in a settled state"
	}
	if !obs.DatasetExists {
		if !expectedKnown || expectedGeneration == 0 {
			return ReplicaCurrent, "the target has no dataset for this replication stream and none was expected yet: there is nothing to be current about, and that is a confirmed fact rather than an unobserved one"
		}
		return ReplicaStale, fmt.Sprintf("the target's dataset does not exist, but generation %d was expected there: this is a confirmed absence, so the target holds nothing", expectedGeneration)
	}
	if !obs.HaveHeld {
		if !expectedKnown || expectedGeneration == 0 {
			return ReplicaCurrent, "the target holds the dataset and nothing is expected of it yet: the copy is as current as this policy has ever been"
		}
		return ReplicaStale, fmt.Sprintf("the target holds the dataset but no apiary-repl snapshot, while generation %d was expected there", expectedGeneration)
	}
	switch {
	case obs.HeldGeneration == expectedGeneration:
		return ReplicaCurrent, fmt.Sprintf("the target holds apiary-repl-%08d, which is the last acked generation", obs.HeldGeneration)
	case obs.HeldGeneration < expectedGeneration:
		return ReplicaStale, fmt.Sprintf("the target holds apiary-repl-%08d while generation %d is the last acked generation: it is %d generation(s) behind", obs.HeldGeneration, expectedGeneration, expectedGeneration-obs.HeldGeneration)
	default:
		return ReplicaAheadOfSource, fmt.Sprintf("the target holds apiary-repl-%08d, which is NEWER than the last acked generation %d: the source's own view is behind the target, or a repoint was committed elsewhere", obs.HeldGeneration, expectedGeneration)
	}
}

func explainUnobserved(obs TargetObservation, why string) string {
	who := obs.NodeID
	if who == "" {
		who = obs.Dataset
	}
	if obs.Detail == "" {
		return fmt.Sprintf("%s could not be observed: %s - this is 'could not check', not 'checked and fine'", who, why)
	}
	return fmt.Sprintf("%s could not be observed: %s (%s) - this is 'could not check', not 'checked and fine'", who, why, obs.Detail)
}

// HighestReplGeneration returns the highest replication generation
// among the given snapshot short names, and whether any was found.
// Non-replication snapshots are ignored rather than treated as
// generation 0, so a VM's own rollback snapshots cannot masquerade as
// replication history.
func HighestReplGeneration(snapshots []string) (uint32, bool) {
	var best uint32
	found := false
	for _, s := range snapshots {
		gen, ok := ParseReplSnapshotName(s)
		if !ok {
			continue
		}
		if !found || gen > best {
			best, found = gen, true
		}
	}
	return best, found
}

// ReplGenerations returns the replication generations present among the
// given snapshot short names, ascending. Used to report what a target
// actually holds without re-deriving the ordering from string sorts.
func ReplGenerations(snapshots []string) []uint32 {
	var gens []uint32
	seen := make(map[uint32]bool)
	for _, s := range snapshots {
		if gen, ok := ParseReplSnapshotName(s); ok && !seen[gen] {
			seen[gen] = true
			gens = append(gens, gen)
		}
	}
	sort.Slice(gens, func(i, j int) bool { return gens[i] < gens[j] })
	return gens
}

// SendOutcome is the run-level answer to "did that stream happen",
// and it has the same three states as everything else in this package.
// The distinction is the reason it is a type rather than a bool: a
// replication run that was cut off mid-stream, or whose `zfs send`
// could not be started, has NOT failed in any way an operator should
// act on, and it has certainly not succeeded. Reporting it as either
// is how a data-protection feature starts telling lies.
type SendOutcome string

const (
	// SendSucceeded: the stream ran and `zfs send` exited zero, which
	// is a positive statement from zfs(8). It says nothing about the
	// destination having applied it — that is the target's own ack,
	// and only the target can produce one.
	SendSucceeded SendOutcome = "send_succeeded"

	// SendFailed: zfs(8) ran and rejected the request, with its own
	// message. The generation is NOT acked and must not advance.
	SendFailed SendOutcome = "send_failed"

	// SendUnobserved: no usable answer. The send could not be started,
	// or it was cut off before zfs(8) could judge it. Neither success
	// nor failure: the next run must re-observe rather than assume.
	SendUnobserved SendOutcome = "send_unobserved"
)

// ClassifySendError maps a run's error onto exactly one of the three
// outcomes. The rule is deliberately mechanical and does no string
// matching: an *UnobservedError anywhere in the chain is unobserved,
// anything else non-nil is a failure, and nil is success.
func ClassifySendError(err error) SendOutcome {
	switch {
	case err == nil:
		return SendSucceeded
	case IsUnobserved(err):
		return SendUnobserved
	default:
		return SendFailed
	}
}

// IsUnobserved reports whether err is, or wraps, an *UnobservedError —
// that is, whether it means "nothing usable was observed" rather than a
// statement about the dataset. It exists so no caller outside this
// package has to reimplement the question with a type assertion that
// silently answers false for a wrapped error.
func IsUnobserved(err error) bool {
	var u *UnobservedError
	return errors.As(err, &u)
}

// Pump runs a plan's send to completion: it starts the stream, copies
// it into w, and ALWAYS closes the stream, so the one signal that
// distinguishes "the send ran" from "the send was cut off" is never
// lost to a forgotten Close.
//
// The copy error takes precedence over the close error, because it is
// the earlier evidence: if the destination link died, the send's exit
// status describes a stream nobody was listening to.
//
// w is the transport (an io.Pipe writer on the target side, an io.Pipe
// reader on this side, a file in a backup context). Pump knows nothing
// about it and adds no buffering, so a stream of any size costs one
// buffer.
func (m *Manager) Pump(ctx context.Context, plan SendPlan, w io.Writer) error {
	stream, err := m.Open(ctx, plan)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(w, stream)
	closeErr := stream.Close()
	if copyErr != nil {
		// A transport failure, not a statement about the dataset. If
		// the context ended, the send child was killed too and its
		// exit status is meaningless, so either way this is silence.
		detail := "the replication stream was not delivered in full, so whether zfs send succeeded is undetermined: " + copyErr.Error()
		if ctx.Err() != nil {
			detail = "the replication stream was interrupted before zfs could finish it: " + ctx.Err().Error()
		}
		return &UnobservedError{
			Op:     strings.Join(plan.Args, " "),
			Detail: detail,
			Err:    copyErr,
		}
	}
	return closeErr
}

// ObserveSource answers the one question the generation fence cannot
// answer for itself: what does this source actually still hold?
//
// It lists the source dataset's own snapshots through the injected
// CommandRunner and fills in SourceState's two filesystem facts —
// whether the last acked snapshot is present, and the newest older
// replication snapshot a full resync could start from.
//
// It is deliberately NOT the pre-existing Manager.ListSnapshots: that
// one calls runZFS directly, and the replication layer's own
// observation has to go through the runner so the fence is testable on
// a host with no zfs(8) at all. The parsing rules match — non-
// recursive, short names, and a dataset that does not exist yet is an
// empty list rather than an error.
//
// An unanswerable list is *UnobservedError, not an empty list. An empty
// list means "this dataset genuinely has no replication snapshots",
// which escalates to a full send; an unobserved list means the fence
// must decide nothing, because the difference between the two is the
// difference between a planned full resend and a data-loss-shaped
// silence.
func (m *Manager) ObserveSource(ctx context.Context, la LastAcked) (SourceState, error) {
	state := la.SourceState(false)
	if state.Dataset == "" {
		return SourceState{}, fmt.Errorf("zfs: cannot observe a replication source with no dataset")
	}
	full, err := m.path(state.Dataset)
	if err != nil {
		return SourceState{}, err
	}
	args := []string{"list", "-H", "-o", "name", "-t", "snapshot", full}
	out, err := m.runnerOrDefault().Run(ctx, args...)
	if err != nil {
		if strings.Contains(detailOf(err), "dataset does not exist") {
			// Confirmed: the source dataset is not there. Nothing is
			// present, so nothing can be sent from and the plan
			// escalates to a full send. A confirmed fact, not an
			// unanswered question.
			return state, nil
		}
		return SourceState{}, &UnobservedError{
			Op:     strings.Join(args, " "),
			Detail: "the source's own snapshot list could not be observed, so the generation fence cannot decide what this run may send: " + detailOf(err),
			Err:    err,
		}
	}
	prefix := full + "@"
	present := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		present[strings.TrimPrefix(line, prefix)] = true
	}
	if state.LastAckedSnapshot != "" {
		_, snap, ok := strings.Cut(state.LastAckedSnapshot, "@")
		state.LastAckedSnapshotPresent = ok && present[snap]
	}
	// Only worth computing when the fence will actually need it. With
	// the last acked snapshot present the run is incremental and the
	// resync origin is ignored, so leaving it empty keeps the
	// escalation state honest rather than half-populated.
	if !state.LastAckedSnapshotPresent {
		state.FallbackFromSnapshot = resyncOrigin(present, state)
	}
	return state, nil
}

// resyncOrigin picks the resync origin: the newest replication
// snapshot the source still holds whose generation is at or below the
// last acked one, excluding the last acked snapshot itself. A candidate
// in that range is one the destination is known to be at or ahead of,
// so a `-I` stream from it is something the destination can actually
// apply; and the last acked snapshot is excluded because the caller
// only reaches this when it is absent, and naming it here would
// produce the `-I <absent snapshot>` that fails with "previous snapshot
// not found".
func resyncOrigin(present map[string]bool, state SourceState) string {
	if !state.HaveLastAcked {
		return ""
	}
	best := uint32(0)
	found := false
	for name := range present {
		gen, ok := ParseReplSnapshotName(name)
		if !ok || gen > state.LastAckedGeneration {
			continue
		}
		if name == snapshotShortName(state.LastAckedSnapshot) {
			continue
		}
		if !found || gen > best {
			best, found = gen, true
		}
	}
	if !found {
		return ""
	}
	return state.Dataset + "@" + ReplSnapshotName(best)
}

// snapshotShortName returns the part of a "dataset@snapshot" name
// after the "@", or the whole string when there is no "@".
func snapshotShortName(datasetSnapshot string) string {
	_, snap, ok := strings.Cut(datasetSnapshot, "@")
	if !ok {
		return datasetSnapshot
	}
	return snap
}
