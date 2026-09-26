package zfs

// The remaining edges: what every refusal says, and what happens when
// the filesystem under the token store misbehaves. Both are places
// where a plausible-looking default would quietly do the wrong thing.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestErrorMessagesSayWhatToDo(t *testing.T) {
	// An error an operator reads has to name the thing and the next
	// step. A bare sentinel string is a bug report waiting to happen.
	live := ObservedLiveToken("vm-1", "1-abc")
	cases := []struct {
		name  string
		err   error
		wants []string
	}{
		{
			name:  "the fence on a stale generation",
			err:   (&FenceError{Reason: FenceRefuseStale, Requested: 3, LastAcked: 7, LastAckedValid: true, Dataset: "vm-1"}),
			wants: []string{"vm-1", "3", "7", "strictly forward"},
		},
		{
			name:  "the fence with no history at all",
			err:   (&FenceError{Reason: FenceRefuseStale, Requested: 3, Dataset: "vm-1"}),
			wants: []string{"vm-1", "not a valid starting point"},
		},
		{
			name:  "the fence on a missing incremental source",
			err:   (&FenceError{Reason: FenceRefuseNoSnapshot, LastAcked: 9, LastAckedValid: true, Dataset: "vm-1"}),
			wants: []string{"vm-1", "9", "full send"},
		},
		{
			name:  "the fence on an unrepresentable generation",
			err:   (&FenceError{Reason: FenceRefuseNoGeneration, Requested: 0, Dataset: "vm-1", Detail: "generation 0 is reserved"}),
			wants: []string{"vm-1", "reserved"},
		},
		{
			name:  "the fence on an unrepresentable generation with no detail",
			err:   (&FenceError{Reason: FenceRefuseNoGeneration, Requested: MaxReplGeneration + 1, Dataset: "vm-1"}),
			wants: []string{"cannot be represented as a replication snapshot name"},
		},
		{
			name:  "the fence with no usable resync origin",
			err:   (&FenceError{Reason: FenceRefuseNoOrigin, LastAcked: 9, LastAckedValid: true, Dataset: "vm-1"}),
			wants: []string{"vm-1", "9", "reject", "must not be forced"},
		},
		{
			name:  "the fence on an unknown reason",
			err:   (&FenceError{Reason: "some_future_reason", Dataset: "vm-1"}),
			wants: []string{"vm-1", "some_future_reason"},
		},
		{
			name:  "a destination that is mid-receive",
			err:   (&ResumableDestError{Dataset: "vm-1", State: ResumeTokenLive, Detail: live.Detail}),
			wants: []string{"vm-1", "mid-receive", "resume", "-F"},
		},
		{
			name:  "a destination strictly ahead",
			err:   (&DestinationAheadError{Dataset: "vm-1", Held: 12, StreamOrigin: 7}),
			wants: []string{"vm-1", "apiary-repl-00000012", "origin 7", "backwards", "never with an automatic -F"},
		},
		{
			name:  "a destination ahead of a full send",
			err:   (&DestinationAheadError{Dataset: "vm-1", Held: 12, FromOrigin: true}),
			wants: []string{"a full send with no incremental origin"},
		},
		{
			name:  "an unobserved query",
			err:   (&UnobservedError{Op: "get -H -o value receive_resume_token pool/ds", Detail: "I/O error", Err: io.EOF}),
			wants: []string{"no usable answer", "receive_resume_token", "I/O error"},
		},
		{
			name:  "an unobserved query with no detail",
			err:   &UnobservedError{Op: "list -t snapshot pool/ds"},
			wants: []string{"no usable answer", "list -t snapshot"},
		},
		{
			name:  "a refused receive with no observation",
			err:   &UnobservedError{Op: "receive pool/ds"},
			wants: []string{"no usable answer"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.err.Error()
			for _, w := range tc.wants {
				if !strings.Contains(msg, w) {
					t.Errorf("message %q does not mention %q", msg, w)
				}
			}
			if strings.TrimSpace(msg) == "" {
				t.Error("an error message is empty")
			}
		})
	}
}

func TestUnwrapChainsAreConsistent(t *testing.T) {
	// Every typed error must be reachable by sentinel from any wrapping
	// context, or a caller checking errors.Is at the top of a run gets
	// a different answer from one checking at the bottom.
	if !errors.Is((&FenceError{}), ErrFenceRefused) {
		t.Error("FenceError does not unwrap to ErrFenceRefused")
	}
	if !errors.Is((&ResumableDestError{}), ErrDestResumable) {
		t.Error("ResumableDestError does not unwrap to ErrDestResumable")
	}
	if !errors.Is((&DestinationAheadError{}), ErrDestinationAhead) {
		t.Error("DestinationAheadError does not unwrap to ErrDestinationAhead")
	}
	var u *UnobservedError
	if !errors.As(wrap(wrap(&UnobservedError{})), &u) {
		t.Error("a doubly-wrapped observation was not found")
	}
	if errors.As(errors.New("plain"), &u) {
		t.Error("a plain error was matched as an observation")
	}
}

func TestFileTokenStoreSurvivesFilesystemFailures(t *testing.T) {
	// Every one of these is a Save that must fail loudly. The dangerous
	// failure is a Save that appears to succeed while the token is not
	// on disk, because the next run would then believe there is nothing
	// to resume and restart a full receive onto a half-populated
	// dataset.
	t.Run("the directory path is a file", func(t *testing.T) {
		dir := t.TempDir()
		notADir := filepath.Join(dir, "file-not-dir")
		if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
			t.Fatalf("prep: %v", err)
		}
		store := NewFileTokenStore(notADir)
		if err := store.Save("policy-1", "1-token"); err == nil {
			t.Error("Save into a path that is a file returned no error")
		}
	})

	t.Run("the token path is a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "policy-1.token"), 0o700); err != nil {
			t.Fatalf("prep: %v", err)
		}
		store := NewFileTokenStore(dir)
		if err := store.Save("policy-1", "1-token"); err == nil {
			t.Error("Save over a directory returned no error")
		}
		// And the temporary file the write used must not be left
		// behind, or the directory fills up with the debris of every
		// failed run.
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("readdir: %v", err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp") {
				t.Errorf("a failed Save left %s behind", e.Name())
			}
		}
	})

	t.Run("a read-only directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root, which ignores directory permissions")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) })
		store := NewFileTokenStore(filepath.Join(dir, "sub"))
		if err := store.Save("policy-1", "1-token"); err == nil {
			t.Error("Save into a read-only parent returned no error")
		}
	})

	t.Run("delete of a path that is a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "policy-1.token"), 0o700); err != nil {
			t.Fatalf("prep: %v", err)
		}
		store := NewFileTokenStore(dir)
		if err := store.Delete("policy-1"); err == nil {
			t.Error("Delete of a directory returned no error")
		}
	})
}

func TestSendHelpersRefuseInvalidSnapshotNames(t *testing.T) {
	// The base-relative validation is what keeps a dataset name from
	// raft addressing anything outside Base, so every entry point
	// applies it before starting a command.
	r := &fakeRunner{start: func(args []string) (Stream, error) { return &fakeSend{}, nil }}
	m := newFakeManager("zroot/apiary", r)
	bad := []string{"", "vm-1", "vm-1@", "../escape@apiary-repl-00000001", "/abs@apiary-repl-00000001", "vm-1@with/slash"}
	for _, from := range bad {
		if _, err := m.SendIncremental(context.Background(), from, "vm-1@apiary-repl-00000002"); err == nil {
			t.Errorf("SendIncremental(from=%q) returned no error", from)
		}
		if _, err := m.SendAllIncremental(context.Background(), "vm-1@apiary-repl-00000001", from); err == nil {
			t.Errorf("SendAllIncremental(to=%q) returned no error", from)
		}
	}
	if n := len(r.sent()); n != 0 {
		t.Errorf("%d commands were started for invalid names", n)
	}
}

func TestObserveTokenReportsAnEmptyLiveDetail(t *testing.T) {
	// The unknown branch falls back to describing the store, and both
	// halves of that sentence have to be reachable.
	combined := ObserveToken("policy-1", ResumeTokenObservation{Dataset: "vm-1", State: ResumeTokenUnknown},
		TokenRecord{PolicyID: "policy-1", State: ResumeTokenUnknown})
	if combined.State != ResumeTokenUnknown {
		t.Fatalf("State = %q", combined.State)
	}
	if !strings.Contains(combined.Detail, "could not be read") {
		t.Errorf("Detail = %q, want it to describe the unreadable store", combined.Detail)
	}
	// A live stored token whose validity could not be checked gets its
	// own sentence.
	combined2 := ObserveToken("policy-1", ResumeTokenObservation{Dataset: "vm-1", State: ResumeTokenUnknown},
		TokenRecord{PolicyID: "policy-1", State: ResumeTokenLive, Token: "1-x"})
	if !strings.Contains(combined2.Detail, "stored token") {
		t.Errorf("Detail = %q", combined2.Detail)
	}
	// A live observation with no detail still reconciles, because the
	// state carries the meaning.
	live := ObserveToken("policy-1", ResumeTokenObservation{Dataset: "vm-1", State: ResumeTokenLive, Token: "1-x"},
		TokenRecord{PolicyID: "policy-1", State: ResumeTokenLive, Token: "1-x"})
	if live.State != ResumeTokenLive || live.Token != "1-x" {
		t.Errorf("live = %+v", live)
	}
}
