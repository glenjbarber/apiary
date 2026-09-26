package zfs

// The resume-token state machine and the receive-side safety rules.
//
// Two separate claims are under test here, and both are ones a
// replication feature gets wrong in the same way: that a state nobody
// observed is reported as a state that was checked and found fine, and
// that a destination which is inconvenient gets overwritten anyway.

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestResumeTokenConfirmedStates(t *testing.T) {
	// zfs(8) answered in three distinguishable ways and the difference
	// between them is the whole feature: "-" means nothing to resume,
	// a value means an interrupted receive, and "we could not ask" is
	// neither.
	cases := []struct {
		name      string
		out       string
		wantState ResumeTokenState
		wantToken string
	}{
		{"zfs's own no-token value", "-", ResumeTokenNone, ""},
		{"empty output", "", ResumeTokenNone, ""},
		{"a real token", "1-abcdef0123456789abcdef", ResumeTokenLive, "1-abcdef0123456789abcdef"},
		{"a long base64 token", strings.Repeat("a1b2c3d4", 32), ResumeTokenLive, strings.Repeat("a1b2c3d4", 32)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.out
			r := &fakeRunner{run: func(args []string) (string, error) { return out, nil }}
			m := newFakeManager("zroot/apiary", r)
			obs, err := m.ResumeToken(context.Background(), "vm-1")
			if err != nil {
				t.Fatalf("ResumeToken: %v", err)
			}
			if obs.State != tc.wantState {
				t.Errorf("State = %q, want %q", obs.State, tc.wantState)
			}
			if obs.Token != tc.wantToken {
				t.Errorf("Token = %q, want %q", obs.Token, tc.wantToken)
			}
			if obs.Dataset != "vm-1" {
				t.Errorf("Dataset = %q", obs.Dataset)
			}
			if !obs.Observed() {
				t.Error("Observed() = false after a successful query")
			}
			if obs.Live() != (tc.wantState == ResumeTokenLive) {
				t.Errorf("Live() = %v for state %q", obs.Live(), obs.State)
			}
			// The exact query is the documented one, fully qualified.
			ran := r.ran()
			want := []string{"get", "-H", "-o", "value", "receive_resume_token", "zroot/apiary/vm-1"}
			if len(ran) != 1 || !equalArgs(ran[0], want...) {
				t.Errorf("query = %v, want %v", ran, want)
			}
			// A token must never be invented from an absent one.
			if tc.wantState == ResumeTokenNone && obs.Token != "" {
				t.Errorf("a confirmed absence produced token %q", obs.Token)
			}
		})
	}
}

func TestResumeTokenUnreadableOutputIsUnknown(t *testing.T) {
	// Output this code does not recognise is not a confirmation of
	// anything. A new zfs(8) format must be reported as undetermined,
	// not as a destination in a clean state — the same discipline
	// internal/cluster/simulate.go applies to an unrecognised hastd
	// role.
	for _, out := range []string{"null", "none", "(null)", "unavailable", "unknown", "line one\nline two", "tab\there", strings.Repeat("x", 5000)} {
		r := &fakeRunner{run: func(args []string) (string, error) { return out, nil }}
		m := newFakeManager("zroot/apiary", r)
		obs, err := m.ResumeToken(context.Background(), "vm-1")
		if !IsUnobserved(err) {
			t.Errorf("ResumeToken(%q) err = %v, want an *UnobservedError", out, err)
		}
		if obs.State != ResumeTokenUnknown {
			t.Errorf("ResumeToken(%q) State = %q, want %q", out, obs.State, ResumeTokenUnknown)
		}
		if obs.Token != "" {
			t.Errorf("ResumeToken(%q) produced a usable token %q", out, obs.Token)
		}
		if obs.Observed() {
			t.Errorf("ResumeToken(%q) claims an observed state", out)
		}
	}
}

func TestResumeTokenFailedQueryIsUnknown(t *testing.T) {
	r := &fakeRunner{run: func(args []string) (string, error) {
		return "", errors.New("zfs get -H -o value receive_resume_token zroot/apiary/vm-1: cannot open control socket: Operation not permitted")
	}}
	m := newFakeManager("zroot/apiary", r)
	obs, err := m.ResumeToken(context.Background(), "vm-1")
	if !IsUnobserved(err) {
		t.Fatalf("err = %v, want an *UnobservedError", err)
	}
	if obs.State != ResumeTokenUnknown {
		t.Errorf("State = %q, want %q", obs.State, ResumeTokenUnknown)
	}
	if obs.Detail == "" {
		t.Error("an unobserved token query carried no Detail for the operator")
	}
	if !strings.Contains(obs.Detail, "control socket") {
		t.Errorf("Detail = %q, want it to carry zfs's own reason", obs.Detail)
	}
}

func TestResumeTokenMissingDestinationIsAConfirmedAbsence(t *testing.T) {
	// A destination that does not exist cannot be mid-receive, so
	// there is nothing to resume into. That is a positive statement,
	// not silence, and it is the normal first-receive case.
	r := &fakeRunner{run: func(args []string) (string, error) {
		return "", zfsErr("get", "-H", "-o", "value", "receive_resume_token", "zroot/apiary/vm-1")
	}}
	m := newFakeManager("zroot/apiary", r)
	obs, err := m.ResumeToken(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("ResumeToken on a missing destination = %v, want no error", err)
	}
	if obs.State != ResumeTokenNone || obs.Live() {
		t.Errorf("observation = %+v, want a confirmed ResumeTokenNone", obs)
	}
	if obs.Detail == "" {
		t.Error("the confirmed absence carries no explanation")
	}
}

func TestResumeTokenValidatesTheDatasetName(t *testing.T) {
	r := &fakeRunner{run: func(args []string) (string, error) { return "-", nil }}
	m := newFakeManager("zroot/apiary", r)
	for _, bad := range []string{"", "../escape", "/absolute", "a//b", "a/./b", "a/../b"} {
		if _, err := m.ResumeToken(context.Background(), bad); err == nil {
			t.Errorf("ResumeToken(%q) returned no error", bad)
		}
	}
	if n := len(r.ran()); n != 0 {
		t.Errorf("%d queries were run despite invalid names", n)
	}
	// A staging dataset is dot-prefixed and must still work: it is the
	// normal first-receive destination.
	if _, err := m.ResumeToken(context.Background(), StagingDatasetPrefix+"/policy-1"); err != nil {
		t.Errorf("ResumeToken on a staging dataset: %v", err)
	}
}

func TestTokenObservationConstructorsNeverLeaveStateUnset(t *testing.T) {
	// A caller building an observation by hand has to name the state,
	// which is what keeps the zero value meaning "never asked".
	if got := ObservedNoToken("vm-1"); got.State != ResumeTokenNone || got.Live() || !got.Observed() {
		t.Errorf("ObservedNoToken = %+v", got)
	}
	live := ObservedLiveToken("vm-1", "1-token")
	if live.State != ResumeTokenLive || !live.Live() || live.Token != "1-token" {
		t.Errorf("ObservedLiveToken = %+v", live)
	}
	unknown := ObservedUnknownToken("vm-1", "garbage", "the query timed out")
	if unknown.State != ResumeTokenUnknown || unknown.Observed() || unknown.Live() {
		t.Errorf("ObservedUnknownToken = %+v", unknown)
	}
	if unknown.Raw != "garbage" || unknown.Detail != "the query timed out" {
		t.Errorf("ObservedUnknownToken dropped the raw evidence: %+v", unknown)
	}
}

func TestObserveTokenReconcilesLiveAgainstStored(t *testing.T) {
	// The cross is not a sum. zfs(8) is authoritative about whether a
	// receive is in progress; the local store is authoritative about
	// what this node previously saw. Every combination has to land on
	// exactly one of the four states, and "unknown" wins over both.
	const live = "1-aaaa"
	const other = "1-bbbb"
	cases := []struct {
		name      string
		live      ResumeTokenObservation
		stored    TokenRecord
		wantState ResumeTokenState
		wantToken string
	}{
		{
			name:      "both agree",
			live:      ObservedLiveToken("vm-1", live),
			stored:    TokenRecord{PolicyID: "p", State: ResumeTokenLive, Token: live},
			wantState: ResumeTokenLive,
			wantToken: live,
		},
		{
			name:      "the destination is authoritative when the store is behind",
			live:      ObservedLiveToken("vm-1", live),
			stored:    TokenRecord{PolicyID: "p", State: ResumeTokenNone},
			wantState: ResumeTokenLive,
			wantToken: live,
		},
		{
			name:      "the destination is authoritative when the store disagrees",
			live:      ObservedLiveToken("vm-1", live),
			stored:    TokenRecord{PolicyID: "p", State: ResumeTokenLive, Token: other},
			wantState: ResumeTokenLive,
			wantToken: live,
		},
		{
			name:      "a stored token the destination no longer reports is stale",
			live:      ObservedNoToken("vm-1"),
			stored:    TokenRecord{PolicyID: "p", State: ResumeTokenLive, Token: live},
			wantState: ResumeTokenStale,
			// The stale token is carried so a caller can report it, but
			// Usable() is false so it can never be handed to zfs.
			wantToken: live,
		},
		{
			name:      "nothing anywhere",
			live:      ObservedNoToken("vm-1"),
			stored:    TokenRecord{PolicyID: "p", State: ResumeTokenNone},
			wantState: ResumeTokenNone,
		},
		{
			name:      "an unanswered live query poisons the answer",
			live:      ObservedUnknownToken("vm-1", "", "timed out"),
			stored:    TokenRecord{PolicyID: "p", State: ResumeTokenNone},
			wantState: ResumeTokenUnknown,
		},
		{
			name:      "an unreadable store poisons the answer too",
			live:      ObservedNoToken("vm-1"),
			stored:    TokenRecord{PolicyID: "p", State: ResumeTokenUnknown},
			wantState: ResumeTokenUnknown,
		},
		{
			name:      "two unknowns are still unknown, not none",
			live:      ObservedUnknownToken("vm-1", "", "timed out"),
			stored:    TokenRecord{PolicyID: "p", State: ResumeTokenUnknown},
			wantState: ResumeTokenUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ObserveToken("policy-1", tc.live, tc.stored)
			if got.State != tc.wantState {
				t.Errorf("State = %q, want %q", got.State, tc.wantState)
			}
			if got.Usable() != (tc.wantState == ResumeTokenLive) {
				t.Errorf("Usable() = %v for state %q", got.Usable(), got.State)
			}
			if tc.wantState == ResumeTokenLive && got.Token != tc.wantToken {
				t.Errorf("Token = %q, want %q", got.Token, tc.wantToken)
			}
			if got.Detail == "" {
				t.Error("Detail is empty; every reconciled state needs one operator-readable line")
			}
			if got.PolicyID != "policy-1" {
				t.Errorf("PolicyID = %q", got.PolicyID)
			}
			// The two inputs are carried verbatim, never merged.
			if got.Live.State != tc.live.State || got.Stored.State != tc.stored.State {
				t.Errorf("inputs were not carried verbatim: %+v", got)
			}
			// A stale token must be visible but not usable: that is
			// the difference between "clear this from the store" and
			// "resume with it".
			if tc.wantState == ResumeTokenStale && got.Usable() {
				t.Error("a stale token is reported as usable")
			}
		})
	}
}

func TestManagerObserveTokenForPrefersTheLiveError(t *testing.T) {
	// The returned error and the reconciled state must not contradict
	// each other: an unobserved answer stays unobserved in State no
	// matter which error the caller ends up looking at.
	r := &fakeRunner{run: func(args []string) (string, error) { return "", errors.New("zfs get: I/O error") }}
	m := newFakeManager("zroot/apiary", r)
	store := NewFileTokenStore(t.TempDir())
	got, err := m.ObserveTokenFor(context.Background(), store, "policy-1", "vm-1")
	if !IsUnobserved(err) {
		t.Fatalf("err = %v, want the live failure surfaced", err)
	}
	if got.State != ResumeTokenUnknown {
		t.Errorf("State = %q while err = %v", got.State, err)
	}
	if got.Stored.State != ResumeTokenNone {
		t.Errorf("Stored = %+v; the store must still be consulted so the operator sees what this node remembered", got.Stored)
	}

	// A store failure is reported when the live query succeeded.
	dir := t.TempDir()
	if err := store.Save("policy-2", "1-cccc"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	broken := NewFileTokenStore(dir)
	// A directory where the token file belongs is an unreadable store,
	// which is unknown — not an empty store.
	if err := os.MkdirAll(dir+"/policy-3.token", 0o700); err != nil {
		t.Fatalf("prep: %v", err)
	}
	r2 := &fakeRunner{run: func(args []string) (string, error) { return "-", nil }}
	m2 := newFakeManager("zroot/apiary", r2)
	got2, err := m2.ObserveTokenFor(context.Background(), broken, "policy-3", "vm-1")
	if !IsUnobserved(err) {
		t.Fatalf("err = %v, want the store failure surfaced as unobserved", err)
	}
	if got2.State != ResumeTokenUnknown {
		t.Errorf("State = %q, want %q", got2.State, ResumeTokenUnknown)
	}
}

func TestManagerObserveTokenForWithNoStore(t *testing.T) {
	// A nil store is a legitimate call: there is nothing to reconcile
	// against, and it must not be confused with a store that holds
	// nothing.
	r := &fakeRunner{run: func(args []string) (string, error) { return "-", nil }}
	m := newFakeManager("zroot/apiary", r)
	got, err := m.ObserveTokenFor(context.Background(), nil, "policy-1", "vm-1")
	if err != nil {
		t.Fatalf("ObserveTokenFor with no store: %v", err)
	}
	if got.State != ResumeTokenNone {
		t.Errorf("State = %q, want %q", got.State, ResumeTokenNone)
	}
}

func TestReceiveIntoNeverForceOverwrites(t *testing.T) {
	// ADR-0130's receive rule, asserted as an argument vector: nothing
	// short of an explicit human decision produces -F. Every row here
	// is a reason a replication run might be tempted to force, and
	// every row must come out without it.
	cases := []struct {
		name     string
		tokenOut string
		tokenErr error
		opts     ReceiveOptions
		wantArgs []string
	}{
		{
			name:     "a clean destination gets a bare receive",
			tokenOut: "-",
			opts:     ReceiveOptions{NoMount: true},
			wantArgs: []string{"receive", "-u", "zroot/apiary/vm-1"},
		},
		{
			name:     "a clean destination with no flags is still bare",
			tokenOut: "-",
			wantArgs: []string{"receive", "zroot/apiary/vm-1"},
		},
		{
			name:     "a live token is resumed with that exact token",
			tokenOut: "1-eeee",
			opts:     ReceiveOptions{NoMount: true, ResumeToken: "1-eeee"},
			wantArgs: []string{"receive", "-u", "-t", "1-eeee", "zroot/apiary/vm-1"},
		},
		{
			name:     "SkipResumeCheck trusts the caller and never adds -F",
			tokenOut: "-",
			opts:     ReceiveOptions{SkipResumeCheck: true},
			wantArgs: []string{"receive", "zroot/apiary/vm-1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.tokenOut
			terr := tc.tokenErr
			r := &fakeRunner{
				run: func(args []string) (string, error) { return out, terr },
				recv: func(args []string, stdin io.Reader) error {
					_, _ = io.Copy(io.Discard, stdin)
					return nil
				},
			}
			m := newFakeManager("zroot/apiary", r)
			if err := m.ReceiveInto(context.Background(), "vm-1", strings.NewReader("stream"), tc.opts); err != nil {
				t.Fatalf("ReceiveInto: %v", err)
			}
			got := r.received()
			if len(got) != 1 {
				t.Fatalf("%d receives were run, want 1", len(got))
			}
			if !equalArgs(got[0], tc.wantArgs...) {
				t.Errorf("args = %v, want %v", got[0], tc.wantArgs)
			}
			for _, a := range got[0] {
				if a == "-F" {
					t.Fatalf("args %v contain -F with no explicit human decision", got[0])
				}
			}
		})
	}
}

func TestReceiveIntoRefusesRatherThanForces(t *testing.T) {
	// Every refusal below must spawn no command at all. A refusal that
	// still ran `zfs receive` would be a refusal in name only.
	cases := []struct {
		name     string
		tokenOut string
		tokenErr error
		opts     ReceiveOptions
		wantErr  error
	}{
		{
			name:     "a live token with nothing to continue it",
			tokenOut: "1-ffff",
			opts:     ReceiveOptions{NoMount: true},
			wantErr:  ErrDestResumable,
		},
		{
			name:     "a token that does not match the destination's live token",
			tokenOut: "1-ffff",
			opts:     ReceiveOptions{ResumeToken: "1-different"},
			wantErr:  nil, // a plain error, asserted by message
		},
		{
			name:     "a token supplied for a destination with no receive in progress",
			tokenOut: "-",
			opts:     ReceiveOptions{ResumeToken: "1-dead"},
			wantErr:  ErrResumeTokenStale,
		},
		{
			name:     "an unobservable destination is refused, not guessed at",
			tokenErr: errors.New("zfs get: I/O error"),
			opts:     ReceiveOptions{NoMount: true},
			wantErr:  &UnobservedError{},
		},
		{
			name:     "force and resume together are a caller bug",
			tokenOut: "1-ffff",
			opts:     ReceiveOptions{Force: true, ResumeToken: "1-ffff"},
			wantErr:  nil, // a plain error, asserted by message
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.tokenOut
			terr := tc.tokenErr
			spawned := false
			r := &fakeRunner{
				run:  func(args []string) (string, error) { return out, terr },
				recv: func(args []string, stdin io.Reader) error { spawned = true; return nil },
			}
			m := newFakeManager("zroot/apiary", r)
			err := m.ReceiveInto(context.Background(), "vm-1", strings.NewReader("stream"), tc.opts)
			if err == nil {
				t.Fatalf("ReceiveInto returned nil")
			}
			if spawned {
				t.Error("a refused receive still spawned `zfs receive`")
			}
			if n := len(r.received()); n != 0 {
				t.Errorf("%d receive commands were recorded", n)
			}
			switch want := tc.wantErr.(type) {
			case nil:
				// asserted by text below
			case *UnobservedError:
				_ = want
				if !IsUnobserved(err) {
					t.Errorf("err = %v, want an *UnobservedError", err)
				}
			default:
				if !errors.Is(err, want) {
					t.Errorf("errors.Is(%v, %v) = false", err, want)
				}
			}
		})
	}
}

func TestReceiveIntoRefusalMessages(t *testing.T) {
	// The two text-only refusals, pinned so an operator-facing change
	// to either is a deliberate one.
	r := &fakeRunner{run: func(args []string) (string, error) { return "1-ffff", nil }}
	m := newFakeManager("zroot/apiary", r)
	err := m.ReceiveInto(context.Background(), "vm-1", strings.NewReader("x"), ReceiveOptions{ResumeToken: "1-different"})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("mismatched token err = %v", err)
	}

	r2 := &fakeRunner{run: func(args []string) (string, error) { return "1-ffff", nil }}
	m2 := newFakeManager("zroot/apiary", r2)
	err = m2.ReceiveInto(context.Background(), "vm-1", strings.NewReader("x"), ReceiveOptions{Force: true, ResumeToken: "1-ffff"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("force+resume err = %v", err)
	}
}

func TestReceiveIntoForceIsOnlyReachableExplicitly(t *testing.T) {
	// -F exists, because an operator may legitimately need to discard
	// a destination's newer snapshots. It is reachable only by asking
	// for it, in the same call, and it is documented as a decision
	// rather than a fallback.
	r := &fakeRunner{
		run:  func(args []string) (string, error) { return "-", nil },
		recv: func(args []string, stdin io.Reader) error { _, _ = io.Copy(io.Discard, stdin); return nil },
	}
	m := newFakeManager("zroot/apiary", r)
	if err := m.ReceiveInto(context.Background(), "vm-1", strings.NewReader("x"), ReceiveOptions{Force: true}); err != nil {
		t.Fatalf("ReceiveInto with Force: %v", err)
	}
	got := r.received()
	if len(got) != 1 || !equalArgs(got[0], "receive", "-F", "zroot/apiary/vm-1") {
		t.Errorf("args = %v", got)
	}
}

func TestReceiveIntoPipesStdinAndSurfacesZfsStderr(t *testing.T) {
	// A receive that zfs(8) rejects is a failure with a diagnosable
	// message, not silence, and the stream must actually reach it.
	const payload = "zfs-replication-stream"
	r := &fakeRunner{
		run:  func(args []string) (string, error) { return "-", nil },
		recv: func(args []string, stdin io.Reader) error { return nil },
	}
	m := newFakeManager("zroot/apiary", r)
	if err := m.ReceiveInto(context.Background(), "vm-1", strings.NewReader(payload), ReceiveOptions{SkipResumeCheck: true}); err != nil {
		t.Fatalf("ReceiveInto: %v", err)
	}
	if got := r.receivedStdin(); len(got) != 1 || got[0] != payload {
		t.Errorf("stdin = %q, want the stream verbatim", got)
	}

	r2 := &fakeRunner{
		run:  func(args []string) (string, error) { return "-", nil },
		recv: func(args []string, stdin io.Reader) error { return nil },
	}
	m2 := newFakeManager("zroot/apiary", r2)
	err := m2.ReceiveInto(context.Background(), "vm-1", strings.NewReader("x"), ReceiveOptions{SkipResumeCheck: true})
	if err != nil {
		t.Fatalf("ReceiveInto: %v", err)
	}
	// A rejected receive is a failure with zfs's own words attached, so
	// the reason the destination refused is still diagnosable.
	rejected := commandFailure(context.Background(), []string{"receive", "zroot/apiary/vm-1"},
		"cannot receive incremental stream: destination has snapshots (eg. apiary-repl-00000007) must destroy them to overwrite it",
		errors.New("exit status 1"))
	if IsUnobserved(rejected) {
		t.Errorf("a zfs(8) statement was classified unobserved: %v", rejected)
	}
	if !strings.Contains(rejected.Error(), "must destroy them to overwrite it") {
		t.Errorf("the rejection lost zfs's own reason: %v", rejected)
	}
}

func TestReceiveIntoValidatesTheDatasetName(t *testing.T) {
	r := &fakeRunner{run: func(args []string) (string, error) { return "-", nil }}
	m := newFakeManager("zroot/apiary", r)
	for _, bad := range []string{"", "../escape", "/abs", "a//b"} {
		if err := m.ReceiveInto(context.Background(), bad, strings.NewReader("x"), ReceiveOptions{SkipResumeCheck: true}); err == nil {
			t.Errorf("ReceiveInto(%q) returned no error", bad)
		}
	}
	if n := len(r.ran()) + len(r.received()); n != 0 {
		t.Errorf("%d commands were run despite invalid dataset names", n)
	}
}

// CheckDestination is the target-side admission gate. It has no force
// flag and no way to produce one, which is the point.
func TestCheckDestination(t *testing.T) {
	noToken := ObservedNoToken("vm-1")
	liveToken := ObservedLiveToken("vm-1", "1-9999")
	base := func() TargetObservation {
		return TargetObservation{Dataset: "vm-1", NodeID: "comb-2", Observed: true, DatasetObserved: true, DatasetExists: true, Token: noToken}
	}
	cases := []struct {
		name       string
		mutate     func(o *TargetObservation)
		origin     uint32
		haveOrigin bool
		wantErr    error
	}{
		{name: "an ordinary incremental onto a matching destination", origin: 7, haveOrigin: true},
		{name: "a full send onto a fresh destination", mutate: func(o *TargetObservation) { o.DatasetExists = false }},
		{name: "a full send onto a dataset with no replication snapshots", mutate: func(o *TargetObservation) { o.HaveHeld = false }},
		{name: "a full send onto a destination at generation 1", mutate: func(o *TargetObservation) { o.HaveHeld, o.HeldGeneration = true, 1 }, origin: 0},
		{
			name:       "the destination is strictly ahead of the stream's origin",
			mutate:     func(o *TargetObservation) { o.HaveHeld, o.HeldGeneration = true, 9 },
			origin:     7,
			haveOrigin: true,
			wantErr:    ErrDestinationAhead,
		},
		{
			name:    "the destination is mid-receive",
			mutate:  func(o *TargetObservation) { o.Token = liveToken },
			wantErr: ErrDestResumable,
		},
		{
			name:    "the destination's own state is unobserved",
			mutate:  func(o *TargetObservation) { o.DatasetObserved = false; o.Detail = "the RPC timed out" },
			wantErr: &UnobservedError{},
		},
		{
			name:   "the destination's resume state was never asked about",
			mutate: func(o *TargetObservation) { o.Token = ResumeTokenObservation{} },
			// The zero value: never asking is not the same as asking
			// and hearing there is none.
			wantErr: &UnobservedError{},
		},
		{
			name:    "the destination's resume state could not be established",
			mutate:  func(o *TargetObservation) { o.Token = ObservedUnknownToken("vm-1", "", "timed out") },
			wantErr: &UnobservedError{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs := base()
			if tc.mutate != nil {
				tc.mutate(&obs)
			}
			err := CheckDestination(obs, tc.origin, tc.haveOrigin)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("CheckDestination = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckDestination = nil, want %v", tc.wantErr)
			}
			if _, ok := tc.wantErr.(*UnobservedError); ok {
				if !IsUnobserved(err) {
					t.Errorf("err = %v, want an *UnobservedError", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("errors.Is(%v, %v) = false", err, tc.wantErr)
			}
			if errors.Is(err, ErrDestinationAhead) {
				var dae *DestinationAheadError
				if !errors.As(err, &dae) {
					t.Fatalf("err = %v, want a *DestinationAheadError", err)
				}
				if dae.Held != 9 || dae.StreamOrigin != 7 {
					t.Errorf("DestinationAheadError = %+v, want held 9 origin 7", dae)
				}
				// The refusal must say what not to do.
				if !strings.Contains(dae.Error(), "never with an automatic -F") {
					t.Errorf("the refusal does not forbid an automatic force: %q", dae.Error())
				}
			}
		})
	}
}

func TestCheckDestinationEqualGenerationIsAllowed(t *testing.T) {
	// The destination holding exactly the stream's origin is the
	// ordinary incremental case, and it must not be refused — refusing
	// it would make every incremental receive impossible.
	obs := TargetObservation{
		Dataset: "vm-1", NodeID: "comb-2", Observed: true, DatasetObserved: true, DatasetExists: true,
		HaveHeld: true, HeldGeneration: 7, Token: ObservedNoToken("vm-1"),
	}
	if err := CheckDestination(obs, 7, true); err != nil {
		t.Errorf("CheckDestination(origin 7, held 7) = %v, want nil", err)
	}
	// A destination BEHIND the origin is fine too: a full resync is
	// exactly the way to bring it forward.
	if err := CheckDestination(obs, 7, true); err != nil {
		t.Errorf("CheckDestination = %v", err)
	}
	behind := obs
	behind.HeldGeneration = 3
	if err := CheckDestination(behind, 7, true); err != nil {
		t.Errorf("a destination behind the origin is a resync, not a refusal: %v", err)
	}
	// And a plain full send (no origin) is judged only on the resume
	// state, because it has no generation to be behind.
	if err := CheckDestination(obs, 0, false); err != nil {
		t.Errorf("a full send onto a destination holding a snapshot = %v, want nil (zfs decides, and -F is not automatic)", err)
	}
}
