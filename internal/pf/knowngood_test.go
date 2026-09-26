package pf

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the node-local last-known-good record: what is
// written after a successful load, what the four states mean, and
// which of them a caller must never mistake for "nothing to do". No
// pfctl, no network, no root - the record store is a directory of
// files, and the exec seam is the one Manager.verify/Apply use.

func newTestManager(t *testing.T) (*Manager, *[][]string) {
	t.Helper()
	dir := t.TempDir()
	calls := &[][]string{}
	return &Manager{
		KnownGoodDir: dir,
		exec: func(_ context.Context, stdin, name string, args ...string) (string, error) {
			*calls = append(*calls, append([]string{name, strings.Join(args, " ")}, stdin))
			return "", nil
		},
	}, calls
}

func TestKnownGood_RoundTripsTheExactRuleset(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	anchor := "apiary/vm-abc123"

	rules := []Rule{
		{Direction: "in", Action: "pass", Protocol: "tcp", PortRange: "22", Interface: "vtnet0", Any: true},
		{Direction: "out", Action: "block", Protocol: "udp", Source: "10.60.0.7", Destination: "8.8.8.8"},
	}
	body, err := RenderRules(rules)
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	if err := m.Apply(ctx, anchor, rules); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}

	rec, state, err := m.knownGood().load(anchor)
	if err != nil {
		t.Fatalf("load() error: %v", err)
	}
	if state != KnownGoodLoaded {
		t.Fatalf("state = %q, want %q", state, KnownGoodLoaded)
	}
	if rec.Body != body {
		t.Errorf("record body = %q, want %q", rec.Body, body)
	}
	if rec.Anchor != anchor {
		t.Errorf("record anchor = %q, want %q", rec.Anchor, anchor)
	}
	if rec.RecordedAt.IsZero() {
		t.Error("record has no timestamp; the last-known-good record must say when it was recorded")
	}

	// The anchor's own name is the file's identity: two anchors can
	// never overwrite each other's baseline.
	other, state, err := m.knownGood().load("apiary/vm-xyz789")
	if err != nil || state != KnownGoodAbsent {
		t.Errorf("a second anchor reads state %q (err %v), want %q", state, err, KnownGoodAbsent)
	}
	_ = other
}

// TestKnownGood_EmptyRulesetIsItsOwnState: an anchor that was
// successfully loaded with zero rules is a positive observation (the
// fail-open "everything allowed" default), and is a different fact
// from an anchor that was never loaded. Collapsing them would make
// "we know this VM is unfiltered" indistinguishable from "we have
// never looked at this VM".
func TestKnownGood_EmptyRulesetIsItsOwnState(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	anchor := "apiary/vm-empty"

	if err := m.Apply(ctx, anchor, nil); err != nil {
		t.Fatalf("Apply(nil) error: %v", err)
	}
	rec, state, err := m.knownGood().load(anchor)
	if err != nil {
		t.Fatalf("load() error: %v", err)
	}
	if state != KnownGoodEmpty {
		t.Fatalf("state = %q, want %q", state, KnownGoodEmpty)
	}
	if rec.Body != "" {
		t.Errorf("body = %q, want empty", rec.Body)
	}
	if rec.Detail == "" {
		t.Error("an empty record must say why it is empty")
	}
}

func TestKnownGood_NeverLoadedIsAbsentNotEmpty(t *testing.T) {
	m, _ := newTestManager(t)
	rec, state, err := m.knownGood().load("apiary/vm-never")
	if err != nil {
		t.Fatalf("a missing record must not be an error: %v", err)
	}
	if state != KnownGoodAbsent {
		t.Fatalf("state = %q, want %q", state, KnownGoodAbsent)
	}
	if rec.Body != "" || rec.RecordedAt.IsZero() == false {
		t.Errorf("absent record = %+v, want no body and no timestamp", rec)
	}
}

// TestKnownGood_CorruptRecordIsAStateNotAnEmptyRuleset: a truncated
// write, a half-typed `cat >`, or a disk that gave up mid-sector must
// never read as "the last known good ruleset was empty" - that is how
// a node ends up enforcing nothing while believing it knows exactly
// what it is enforcing.
func TestKnownGood_CorruptRecordIsAStateNotAnEmptyRuleset(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	anchor := "apiary/vm-corrupt"
	if err := m.Apply(ctx, anchor, []Rule{{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "22", Any: true}}); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	path, err := m.knownGood().path(anchor)
	if err != nil {
		t.Fatalf("path() error: %v", err)
	}
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}

	corruptions := map[string]string{
		"empty file":         "",
		"no header":          "block in proto tcp from any to any port 22\n",
		"truncated header":   "apiary-pf-known-good v1\nanchor apiary/vm-corrupt\n",
		"no blank line":      strings.Replace(string(good), "\nsha256", " sha256", 1),
		"body edited":        strings.Replace(string(good), "port 22", "port 80", 1),
		"header truncated":   string(good)[:len(string(good))/2],
		"unknown header key": strings.Replace(string(good), "recorded ", "recroded ", 1),
		"wrong anchor":       strings.Replace(string(good), "anchor "+anchor, "anchor apiary/vm-other", 1),
		"bad timestamp":      strings.Replace(string(good), "recorded 20", "recorded not-a-time 20", 1),
	}
	for name, content := range corruptions {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("WriteFile() error: %v", err)
			}
			rec, state, err := m.knownGood().load(anchor)
			if err != nil {
				t.Fatalf("a corrupt record must be a state, not an error: %v", err)
			}
			if state != KnownGoodCorrupt {
				t.Fatalf("state = %q, want %q (content %q)", state, KnownGoodCorrupt, content)
			}
			if rec.Body != "" {
				t.Errorf("a corrupt record returned body %q; a corrupt record provides no baseline at all", rec.Body)
			}
			if rec.Detail == "" {
				t.Error("a corrupt record must carry a reason, so the operator is told why it is unusable")
			}
		})
	}
}

// TestKnownGood_RecordIsAtomic: a reader must see either the old
// record or the new one, never a half-written one. The observable
// part of that, testable without a crash, is that no temp file is left
// behind and that the file is replaced by rename (so an interrupted
// record() leaves the previous baseline intact rather than a truncated
// one).
func TestKnownGood_RecordIsAtomic(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	anchor := "apiary/vm-atomic"

	first := []Rule{{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "22", Any: true}}
	if err := m.Apply(ctx, anchor, first); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	second := []Rule{{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "23", Any: true}}
	if err := m.Apply(ctx, anchor, second); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}

	path, err := m.knownGood().path(anchor)
	if err != nil {
		t.Fatalf("path() error: %v", err)
	}
	rec, state, err := m.knownGood().load(anchor)
	if err != nil || state != KnownGoodLoaded {
		t.Fatalf("state = %q (err %v), want %q", state, err, KnownGoodLoaded)
	}
	if !strings.Contains(rec.Body, "port 23") || strings.Contains(rec.Body, "port 22") {
		t.Errorf("record body = %q, want only the second ruleset", rec.Body)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir() error: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temp file was left behind: %s", e.Name())
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if perm := info.Mode().Perm(); perm != knownGoodMode {
		t.Errorf("record mode = %o, want %o", perm, knownGoodMode)
	}
}

// TestKnownGood_FlushForgetsTheRecord: a flushed anchor's
// last-known-good ruleset is the empty one, and the honest encoding of
// that here is the absence of a baseline - the same state a
// never-loaded anchor is in.
func TestKnownGood_FlushForgetsTheRecord(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	anchor := "apiary/vm-gone"

	if err := m.Apply(ctx, anchor, []Rule{{Direction: "in", Action: "block", Any: true}}); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	if err := m.Flush(ctx, anchor); err != nil {
		t.Fatalf("Flush() error: %v", err)
	}
	_, state, err := m.knownGood().load(anchor)
	if err != nil {
		t.Fatalf("load() error: %v", err)
	}
	if state != KnownGoodAbsent {
		t.Errorf("state after Flush = %q, want %q", state, KnownGoodAbsent)
	}

	// Flushing an anchor that was never populated is still a success,
	// including when there is no record to remove.
	if err := m.Flush(ctx, "apiary/vm-never-existed"); err != nil {
		t.Errorf("Flush() of an unknown anchor = %v, want nil (idempotent teardown)", err)
	}
}

// TestKnownGood_NoRecordOnAFailedLoad: the record is the *last
// successfully loaded* ruleset, so a load that pfctl refused must not
// overwrite the previous one. If it did, a failed load would silently
// become the new baseline and every later drift check would be
// comparing against a ruleset pf never took.
func TestKnownGood_NoRecordOnAFailedLoad(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	anchor := "apiary/vm-failing"

	if err := m.Apply(ctx, anchor, []Rule{{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "22", Any: true}}); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	before, _, err := m.knownGood().load(anchor)
	if err != nil {
		t.Fatalf("load() error: %v", err)
	}

	m.exec = func(_ context.Context, _ string, name string, _ ...string) (string, error) {
		return "", &execError{name: name, msg: "pfctl: syntax error"}
	}
	if err := m.Apply(ctx, anchor, []Rule{{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "23", Any: true}}); err == nil {
		t.Fatal("Apply() with a failing pfctl = nil error, want one")
	}
	after, state, err := m.knownGood().load(anchor)
	if err != nil {
		t.Fatalf("load() error: %v", err)
	}
	if state != KnownGoodLoaded {
		t.Fatalf("state = %q, want the previous record to survive a failed load", state)
	}
	if after.Body != before.Body {
		t.Errorf("a failed load changed the record: %q -> %q", before.Body, after.Body)
	}
}

// TestKnownGood_RejectsAnAnchorThatCouldEscapeTheDirectory: anchor
// names are built by this project, so this is defence in depth - but
// the failure mode without it (a "record" written outside the store)
// is not.
func TestKnownGood_RejectsAnAnchorThatCouldEscapeTheDirectory(t *testing.T) {
	s := knownGoodStore{dir: "/var/db/apiary/pf"}
	for _, anchor := range []string{
		"",
		"/etc/pf.conf",
		"../../etc/pf.conf",
		"apiary/../../etc/pf.conf",
		"apiary//vm-1",
		"apiary/./vm-1",
		"apiary\n/vm-1",
	} {
		if _, err := s.path(anchor); err == nil {
			t.Errorf("path(%q) = nil error, want a rejection", anchor)
		}
	}
	if _, err := s.path("apiary/vm-1"); err != nil {
		t.Errorf("path(%q) error: %v, want nil", "apiary/vm-1", err)
	}
}

// execError is the shape runCmdStdin's errors have, so the fake exec
// in these tests exercises the same branches a real pfctl failure
// would.
type execError struct {
	name string
	msg  string
}

func (e *execError) Error() string { return e.name + ": " + e.msg }
