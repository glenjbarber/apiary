package assumptions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testKey(dep string) Key {
	return Key{Kind: "peer_manager_rpc_succeeded", SubjectKind: SubjectKindNode, DependencyID: dep, ObservedByNodeID: "node-a"}
}

func TestManager_Load_MissingFileIsNotAnError(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "assumptions.json")}
	snap, hist, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snap != nil || hist != nil {
		t.Fatalf("Load() on missing file = %v, %v; want nil, nil", snap, hist)
	}
	if degraded, _ := m.Degraded(); degraded {
		t.Errorf("Degraded() = true on missing file; want false")
	}
}

func TestManager_Append_TransitionAlwaysRecordedRegardlessOfDetail(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "assumptions.json")}
	k := testKey("node-b")
	t0 := time.Now()

	must(t, m.Append([]Result{{Key: k, ObservedStatus: StatusTrue, Detail: "addr 10.0.0.1", LastObservedAt: t0}}, time.Hour, 200, 30*24*time.Hour))
	must(t, m.Append([]Result{{Key: k, ObservedStatus: StatusFalse, ReasonCode: "connection_refused", Detail: "addr 10.0.0.2", LastObservedAt: t0.Add(time.Second)}}, time.Hour, 200, 30*24*time.Hour))

	_, hist, err := m.Load()
	must(t, err)
	if len(hist) != 2 {
		t.Fatalf("history len = %d, want 2 (both entries are transitions)", len(hist))
	}
	if hist[1].ObservedStatus != StatusFalse || hist[1].ReasonCode != "connection_refused" {
		t.Errorf("second history entry = %+v, want the transitioned value", hist[1])
	}
}

func TestManager_Append_UnchangedResultThrottledToHeartbeat(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "assumptions.json")}
	k := testKey("node-b")
	t0 := time.Now()

	must(t, m.Append([]Result{{Key: k, ObservedStatus: StatusTrue, LastObservedAt: t0}}, time.Hour, 200, 30*24*time.Hour))
	// Same status/reason, only 1 minute later - well under the 1h heartbeat.
	must(t, m.Append([]Result{{Key: k, ObservedStatus: StatusTrue, LastObservedAt: t0.Add(time.Minute)}}, time.Hour, 200, 30*24*time.Hour))

	snap, hist, err := m.Load()
	must(t, err)
	if len(hist) != 1 {
		t.Fatalf("history len = %d, want 1 (second tick is unchanged, throttled)", len(hist))
	}
	// The core regression test: the SNAPSHOT's LastObservedAt still
	// advances every tick even though the journal didn't grow.
	if !snap[0].LastObservedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("snapshot LastObservedAt = %v, want %v (must advance every tick, not throttled)", snap[0].LastObservedAt, t0.Add(time.Minute))
	}

	// Now advance past the heartbeat interval with still no change - a
	// new journal entry should appear.
	must(t, m.Append([]Result{{Key: k, ObservedStatus: StatusTrue, LastObservedAt: t0.Add(2 * time.Hour)}}, time.Hour, 200, 30*24*time.Hour))
	_, hist, err = m.Load()
	must(t, err)
	if len(hist) != 2 {
		t.Fatalf("history len = %d, want 2 after heartbeat interval elapsed", len(hist))
	}
}

func TestManager_Append_PruneByCountPerKey(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "assumptions.json")}
	k1, k2 := testKey("node-b"), testKey("node-c")
	t0 := time.Now()

	// k1 flaps every tick (10 transitions); k2 stays constant.
	for i := 0; i < 10; i++ {
		status := StatusTrue
		if i%2 == 0 {
			status = StatusFalse
		}
		must(t, m.Append([]Result{
			{Key: k1, ObservedStatus: status, LastObservedAt: t0.Add(time.Duration(i) * time.Minute)},
			{Key: k2, ObservedStatus: StatusTrue, LastObservedAt: t0.Add(time.Duration(i) * time.Minute)},
		}, time.Hour, 3, 30*24*time.Hour))
	}

	_, hist, err := m.Load()
	must(t, err)
	var k1Count, k2Count int
	for _, h := range hist {
		if h.Key == k1 {
			k1Count++
		}
		if h.Key == k2 {
			k2Count++
		}
	}
	if k1Count != 3 {
		t.Errorf("k1 (noisy key) history count = %d, want 3 (bounded by historyLimit)", k1Count)
	}
	if k2Count == 0 {
		t.Errorf("k2 (quiet key) history count = 0 - a noisy key must not evict another key's history")
	}
}

func TestManager_Append_PruneByMaxAge(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "assumptions.json")}
	k := testKey("node-b")
	old := time.Now().Add(-48 * time.Hour)

	must(t, m.Append([]Result{{Key: k, ObservedStatus: StatusTrue, LastObservedAt: old}}, time.Hour, 200, 24*time.Hour))
	must(t, m.Append([]Result{{Key: k, ObservedStatus: StatusFalse, LastObservedAt: time.Now()}}, time.Hour, 200, 24*time.Hour))

	_, hist, err := m.Load()
	must(t, err)
	for _, h := range hist {
		if h.RecordedAt.Equal(old) {
			t.Errorf("history entry older than maxAge was not pruned: %+v", h)
		}
	}
}

func TestManager_LoadAppend_ConcurrentAccess(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "assumptions.json")}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = m.Append([]Result{{Key: testKey("node-b"), ObservedStatus: StatusTrue, LastObservedAt: time.Now()}}, time.Hour, 50, 24*time.Hour)
		}(i)
		go func() {
			defer wg.Done()
			_, _, _ = m.Load()
		}()
	}
	wg.Wait()

	snap, _, err := m.Load()
	must(t, err)
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1 (no torn/corrupt state after concurrent access)", len(snap))
	}
}

func TestManager_Load_CorruptFileIsQuarantinedNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assumptions.json")
	if err := os.WriteFile(path, []byte("not valid json{{{"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Path: path}

	snap, hist, err := m.Load()
	must(t, err)
	if snap != nil || hist != nil {
		t.Errorf("Load() on corrupt file = %v, %v; want nil, nil (fresh start)", snap, hist)
	}
	degraded, detail := m.Degraded()
	if !degraded {
		t.Fatal("Degraded() = false after loading a corrupt file")
	}
	if !strings.Contains(detail, "corrupt") {
		t.Errorf("Degraded() detail = %q, want it to mention corruption", detail)
	}

	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("found %d quarantined files, want 1", len(matches))
	}
	body, _ := os.ReadFile(matches[0])
	if string(body) != "not valid json{{{" {
		t.Error("quarantined file does not preserve the original bytes")
	}

	// The very next Append must not have overwritten the quarantined
	// file - it should still be there afterward.
	must(t, m.Append([]Result{{Key: testKey("node-b"), ObservedStatus: StatusTrue, LastObservedAt: time.Now()}}, time.Hour, 50, 24*time.Hour))
	matchesAfter, _ := filepath.Glob(path + ".corrupt-*")
	if len(matchesAfter) != 1 {
		t.Errorf("quarantined file count changed after Append: %d", len(matchesAfter))
	}
}

func TestManager_Load_UnrecognizedSchemaVersionIsQuarantined(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assumptions.json")
	body, _ := json.Marshal(fileEnvelope{SchemaVersion: 99, Snapshot: []Result{{Key: testKey("node-b"), ObservedStatus: StatusTrue}}})
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Path: path}

	snap, _, err := m.Load()
	must(t, err)
	if snap != nil {
		t.Error("an unrecognized future schema_version must not be parsed as current data")
	}
	if degraded, _ := m.Degraded(); !degraded {
		t.Error("Degraded() = false after loading an unrecognized schema_version")
	}
}

func TestManager_QuarantineWarning_SurvivesAcrossManagerInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assumptions.json")
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := &Manager{Path: path}
	if _, _, err := first.Load(); err != nil {
		t.Fatal(err)
	}

	// A fresh Manager (simulating a managerd restart) with no NEW
	// corruption event must still see the warning, because the
	// quarantined file is still on disk.
	second := &Manager{Path: path}
	if _, _, err := second.Load(); err != nil {
		t.Fatal(err)
	}
	if degraded, _ := second.Degraded(); !degraded {
		t.Error("a fresh Manager instance must still report the storage warning while a quarantined file remains on disk")
	}

	// Removing the quarantined file and reloading must clear the
	// warning on its own - no separate acknowledgement step exists.
	matches, _ := filepath.Glob(path + ".corrupt-*")
	for _, m := range matches {
		must(t, os.Remove(m))
	}
	if _, _, err := second.Load(); err != nil {
		t.Fatal(err)
	}
	if degraded, _ := second.Degraded(); degraded {
		t.Error("Degraded() must self-clear once the quarantined file is removed, with no separate acknowledgement")
	}
}

func TestManager_Load_WidePermissionsSetWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assumptions.json")
	body, _ := json.Marshal(fileEnvelope{SchemaVersion: currentSchemaVersion})
	if err := os.WriteFile(path, body, 0o644); err != nil { // wider than 0600
		t.Fatal(err)
	}
	m := &Manager{Path: path}
	if _, _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	degraded, detail := m.Degraded()
	if !degraded {
		t.Error("Degraded() = false for a file with permissions wider than 0600")
	}
	if !strings.Contains(detail, "permissions") {
		t.Errorf("detail = %q, want it to mention permissions", detail)
	}
}

func TestManager_Append_WritesFileMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assumptions.json")
	m := &Manager{Path: path}
	must(t, m.Append([]Result{{Key: testKey("node-b"), ObservedStatus: StatusTrue, LastObservedAt: time.Now()}}, time.Hour, 50, 24*time.Hour))
	info, err := os.Stat(path)
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %#o, want 0600", info.Mode().Perm())
	}
}

func TestClampDetail(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		check func(string) bool
	}{
		{"strips control characters", "hello\x00\x01world", func(out string) bool { return out == "helloworld" }},
		{"redacts bearer token", "failed: Authorization: Bearer abc123xyz", func(out string) bool {
			return strings.Contains(out, "[REDACTED]") && !strings.Contains(out, "abc123xyz")
		}},
		{"truncates long text", strings.Repeat("x", MaxDetailLen+100), func(out string) bool {
			return len(out) <= MaxDetailLen+len("...[truncated]") && strings.HasSuffix(out, "...[truncated]")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := ClampDetail(tt.in)
			if !tt.check(out) {
				t.Errorf("ClampDetail(%q) = %q, failed check", tt.in, out)
			}
		})
	}
}

func TestLatestPerKey_DeterministicAndOnePerKey(t *testing.T) {
	k1, k2 := testKey("node-b"), testKey("node-c")
	in := []Result{
		{Key: k1, ObservedStatus: StatusFalse, LastObservedAt: time.Unix(100, 0)},
		{Key: k1, ObservedStatus: StatusTrue, LastObservedAt: time.Unix(200, 0)},
		{Key: k2, ObservedStatus: StatusUnknown, LastObservedAt: time.Unix(150, 0)},
	}
	out := LatestPerKey(in)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2 (one per key)", len(out))
	}
	for _, r := range out {
		if r.Key == k1 && r.ObservedStatus != StatusTrue {
			t.Errorf("k1's latest should be the later, StatusTrue entry, got %v", r.ObservedStatus)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
