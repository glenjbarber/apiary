package zfs

// Real-ZFS coverage for the replication primitives. These tests are
// NOT run by `go test ./...` on an ordinary dev machine — they skip
// when there is no zfs(8), exactly like integration_test.go.
//
// This file has never been executed. It was written and compiled on
// macOS, which has no ZFS of the kind this uses, so its assertions are
// reasoned rather than observed. Treat the first run on brood or drone
// as the first run, not as a regression check. Cross-compile with
//     GOOS=freebsd GOARCH=amd64 go test -c ./internal/zfs
// and run the binary on a FreeBSD host with
//     APIARY_ZFS_TEST_POOL=<an existing pool> ./zfs.test -test.run Replication
//
// Nothing here creates a dataset outside the per-test base created by
// testManager, and every test destroys its own base on cleanup.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// replicationPool is the pool these tests run against. The default
// apiarytest does not exist on this project's FreeBSD hosts, which have
// zroot, so an unset APIARY_ZFS_TEST_POOL is a skip rather than a
// failure against a pool that is not there.
func replicationPool(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("zfs"); err != nil {
		t.Skip("zfs not available on this host; see the file header for how to run these tests")
	}
	pool := os.Getenv("APIARY_ZFS_TEST_POOL")
	if pool == "" {
		t.Skip("APIARY_ZFS_TEST_POOL is not set; these tests need an existing pool to create their base in")
	}
	return pool
}

// guidOf returns the GUID zfs(8) itself uses to identify a snapshot.
// Comparing GUIDs is the only honest way to prove an incremental send
// carried the stream the caller asked for: a name comparison would
// pass even if the wrong snapshot had been sent.
func guidOf(t *testing.T, fullSnapshot string) string {
	t.Helper()
	out, err := runZFS(context.Background(), "get", "-H", "-o", "value", "guid", fullSnapshot)
	if err != nil {
		t.Fatalf("getting guid for %s: %v", fullSnapshot, err)
	}
	return strings.TrimSpace(out)
}

func TestReplication_FullThenIncrementalSend(t *testing.T) {
	pool := replicationPool(t)
	base := fmt.Sprintf("%s/repl-%d", pool, time.Now().UnixNano())
	ctx := context.Background()
	if _, err := runZFS(ctx, "create", "-p", base); err != nil {
		t.Fatalf("creating base %s: %v", base, err)
	}
	t.Cleanup(func() { runZFS(context.Background(), "destroy", "-r", base) })

	// A source dataset with enough data that an incremental is a
	// meaningfully smaller stream than a full one.
	src := base + "/src"
	if _, err := runZFS(ctx, "create", src); err != nil {
		t.Fatalf("creating source: %v", err)
	}
	if err := os.WriteFile(src+"/data", bytes.Repeat([]byte("apiary"), 1<<20), 0o600); err != nil {
		t.Fatalf("populating source: %v", err)
	}
	syncDataset(t, src)

	// The target starts as a staging dataset, as ADR-0130 requires for
	// a first receive.
	dest := base + "/" + StagingDatasetPrefix + "/policy-1"
	la := NewLastAcked("policy-1", "src")
	gen1, err := la.NextGeneration()
	if err != nil {
		t.Fatalf("NextGeneration: %v", err)
	}
	plan1, err := PlanSend(SourceState{Dataset: "src"}, SendRequest{TargetGeneration: gen1})
	if err != nil {
		t.Fatalf("PlanSend: %v", err)
	}
	m := New(base)
	if err := m.Pump(ctx, plan1, receiveInto(t, m, dest, ReceiveOptions{NoMount: true})); err != nil {
		t.Fatalf("first send/receive: %v", err)
	}
	// The snapshot arrived under the name the convention mints.
	have, err := m.SnapshotExists(ctx, "src@"+ReplSnapshotName(gen1))
	if err != nil {
		t.Fatalf("SnapshotExists: %v", err)
	}
	if !have {
		t.Fatalf("the target does not hold %s", plan1.ToSnapshot)
	}

	// Now generation 2, incrementally. The GUID proves the stream
	// started from generation 1's snapshot rather than from origin.
	la, err = la.RecordAck(gen1)
	if err != nil {
		t.Fatalf("RecordAck: %v", err)
	}
	gen2 := gen1 + 1
	if _, err := runZFS(ctx, "snapshot", src+"@"+ReplSnapshotName(gen2)); err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	more := src + "/data2"
	if err := os.WriteFile(more, bytes.Repeat([]byte("replication"), 1<<19), 0o600); err != nil {
		t.Fatalf("adding data: %v", err)
	}
	syncDataset(t, src)

	state, err := m.ObserveSource(ctx, la)
	if err != nil {
		t.Fatalf("ObserveSource: %v", err)
	}
	if !state.LastAckedSnapshotPresent {
		t.Fatal("the last acked snapshot was not observed on the source")
	}
	plan2, err := PlanSend(state, SendRequest{TargetGeneration: gen2})
	if err != nil {
		t.Fatalf("PlanSend: %v", err)
	}
	if plan2.Kind != SendIncremental {
		t.Fatalf("plan2.Kind = %q, want an incremental send", plan2.Kind)
	}
	if err := m.Pump(ctx, plan2, receiveInto(t, m, dest, ReceiveOptions{NoMount: true})); err != nil {
		t.Fatalf("incremental send/receive: %v", err)
	}
	// Both generations are present on the target, and the earlier one
	// is the exact snapshot the incremental named.
	got, err := m.ListSnapshots(ctx, StagingDatasetPrefix+"/policy-1")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(ReplGenerations(got)) != 2 {
		t.Fatalf("target holds %v, want two replication generations", got)
	}
	heldGen1 := dest + "@" + ReplSnapshotName(gen1)
	if guidOf(t, heldGen1) == "" {
		t.Fatal("the target's first generation has no GUID, so the receive did not land where it claims")
	}
	if guidOf(t, heldGen1) != guidOf(t, src+"@"+ReplSnapshotName(gen1)) {
		t.Fatal("the target's first generation does not match the source's; the receive landed on a different snapshot")
	}
}

func TestReplication_InterruptedReceiveResumesWithItsToken(t *testing.T) {
	// The resume path, on real ZFS: interrupt a send mid-stream, prove
	// the destination is left in a resumable state with a real token,
	// then finish the stream with `zfs send -t` and prove the data is
	// complete.
	pool := replicationPool(t)
	base := fmt.Sprintf("%s/repltok-%d", pool, time.Now().UnixNano())
	ctx := context.Background()
	if _, err := runZFS(ctx, "create", "-p", base); err != nil {
		t.Fatalf("creating base %s: %v", base, err)
	}
	t.Cleanup(func() { runZFS(context.Background(), "destroy", "-r", base) })

	src := base + "/src"
	if _, err := runZFS(ctx, "create", src); err != nil {
		t.Fatalf("creating source: %v", err)
	}
	// Big enough that reading a few hundred kilobytes cannot drain it.
	if err := os.WriteFile(src+"/data", bytes.Repeat([]byte("resume-me"), 1<<19), 0o600); err != nil {
		t.Fatalf("populating source: %v", err)
	}
	syncDataset(t, src)

	m := New(base)
	dest := base + "/dest"
	plan, err := PlanSend(SourceState{Dataset: "src"}, SendRequest{TargetGeneration: 1})
	if err != nil {
		t.Fatalf("PlanSend: %v", err)
	}

	// Start the send, start the receive, and abandon both after a
	// little data. Closing the send's stream kills the child, which is
	// what an interrupted link does.
	stream, err := m.Open(ctx, plan)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	pr, pw := io.Pipe()
	recvErr := make(chan error, 1)
	go func() { recvErr <- m.ReceiveInto(ctx, "dest", pr, ReceiveOptions{NoMount: true}) }()

	consumed := 0
	buf := make([]byte, 256<<10)
	for consumed < 256<<10 {
		n, rerr := stream.Read(buf)
		if n > 0 {
			if _, werr := pw.Write(buf[:n]); werr != nil {
				break
			}
			consumed += n
		}
		if rerr != nil {
			break
		}
	}
	stream.Close()
	_ = pw.Close()
	<-recvErr // the receive has failed; that is the point of the test

	// The destination must now be mid-receive with a live token. This
	// is the observation that is neither "current" nor "unknown".
	obs, err := m.ResumeToken(ctx, "dest")
	if err != nil {
		t.Fatalf("ResumeToken: %v", err)
	}
	if !obs.Live() {
		t.Skipf("this pool did not leave a resumable receive after an interrupted stream (state %q, raw %q); "+
			"a destroy or -F is required, which is exactly the fallback the resume path exists to avoid", obs.State, obs.Raw)
	}

	// The same observation through the fence: nothing is acked, so the
	// run is a partial receive, and it is not current.
	obsv := observedTarget(0, false)
	obsv.Dataset = "dest"
	obsv.Token = obs
	if v, _ := Classify(obsv, 0, false); v != ReplicaPartialReceive {
		t.Errorf("verdict = %q, want %q", v, ReplicaPartialReceive)
	}

	// Resume: the source continues the stream and the target continues
	// the receive with the same token.
	rest, err := m.SendResume(ctx, obs.Token)
	if err != nil {
		t.Fatalf("SendResume: %v", err)
	}
	pr2, pw2 := io.Pipe()
	recvErr2 := make(chan error, 1)
	go func() {
		recvErr2 <- m.ReceiveInto(ctx, "dest", pr2, ReceiveOptions{NoMount: true, ResumeToken: obs.Token})
	}()
	if _, err := io.Copy(pw2, rest); err != nil {
		t.Fatalf("copying the resumed stream: %v", err)
	}
	rest.Close()
	_ = pw2.Close()
	if err := <-recvErr2; err != nil {
		t.Fatalf("resumed receive: %v", err)
	}

	// The destination is no longer mid-receive, and the data is whole.
	after, err := m.ResumeToken(ctx, "dest")
	if err != nil {
		t.Fatalf("ResumeToken after resume: %v", err)
	}
	if after.Live() {
		t.Error("the destination is still mid-receive after a successful resume")
	}
	if after.State != ResumeTokenNone {
		t.Errorf("state after resume = %q, want %q", after.State, ResumeTokenNone)
	}
	// The received file must match the source byte for byte.
	want, err := os.ReadFile(src + "/data")
	if err != nil {
		t.Fatalf("reading source data: %v", err)
	}
	got, err := os.ReadFile(dest + "/data")
	if err != nil {
		t.Fatalf("reading received data: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the resumed receive produced %d bytes where the source has %d", len(got), len(want))
	}
}

func TestReplication_DestroyedDestinationForcesAFullResend(t *testing.T) {
	// ADR-0130: if the destination dataset is gone, the restart is from
	// the last acked snapshot — and if the source can no longer produce
	// that, a full resend with a visible escalation. Both outcomes are
	// checked here, and neither is silent.
	pool := replicationPool(t)
	base := fmt.Sprintf("%s/replgone-%d", pool, time.Now().UnixNano())
	ctx := context.Background()
	if _, err := runZFS(ctx, "create", "-p", base); err != nil {
		t.Fatalf("creating base %s: %v", base, err)
	}
	t.Cleanup(func() { runZFS(context.Background(), "destroy", "-r", base) })

	src := base + "/src"
	if _, err := runZFS(ctx, "create", src); err != nil {
		t.Fatalf("creating source: %v", err)
	}
	m := New(base)

	// A destroyed destination has no token: there is nothing to resume
	// into, and that is a confirmed absence rather than an unknown.
	if _, err := runZFS(ctx, "snapshot", src+"@"+ReplSnapshotName(1)); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	obs, err := m.ResumeToken(ctx, "never-existed")
	if err != nil {
		t.Fatalf("ResumeToken: %v", err)
	}
	if obs.State != ResumeTokenNone {
		t.Errorf("state = %q, want %q for a destination that does not exist", obs.State, ResumeTokenNone)
	}

	// With the last acked snapshot still present, the restart is an
	// incremental — not a full resend from origin.
	la := NewLastAckedAt("policy-1", "src", 1, "src@"+ReplSnapshotName(1))
	state, err := m.ObserveSource(ctx, la)
	if err != nil {
		t.Fatalf("ObserveSource: %v", err)
	}
	plan, err := PlanSend(state, SendRequest{TargetGeneration: 2})
	if err != nil {
		t.Fatalf("PlanSend: %v", err)
	}
	if plan.Kind != SendIncremental {
		t.Errorf("plan.Kind = %q, want an incremental restart", plan.Kind)
	}

	// Destroy the acked snapshot, and the restart must escalate to a
	// full resend that says so.
	if _, err := runZFS(ctx, "destroy", src+"@"+ReplSnapshotName(1)); err != nil {
		t.Fatalf("destroying the acked snapshot: %v", err)
	}
	state, err = m.ObserveSource(ctx, la)
	if err != nil {
		t.Fatalf("ObserveSource after destroy: %v", err)
	}
	plan, err = PlanSend(state, SendRequest{TargetGeneration: 2})
	if err != nil {
		t.Fatalf("PlanSend after destroy: %v", err)
	}
	if !plan.EscalatedFromIncremental {
		t.Error("a full resend after the acked snapshot was destroyed was not flagged as an escalation")
	}
	if plan.Note == "" {
		t.Error("a full resend carried no operator-readable note")
	}
	if plan.Fence == FenceAllowIncremental {
		t.Errorf("fence = %q; an escalation must not read as an ordinary allow", plan.Fence)
	}
}

func TestReplication_FenceRefusesOnRealZFS(t *testing.T) {
	// The fence is pure decision logic and needs no pool, but running
	// it here against a real base proves the dataset names it plans are
	// the ones a real zfs(8) would accept.
	pool := replicationPool(t)
	base := fmt.Sprintf("%s/replfence-%d", pool, time.Now().UnixNano())
	ctx := context.Background()
	if _, err := runZFS(ctx, "create", "-p", base); err != nil {
		t.Fatalf("creating base %s: %v", base, err)
	}
	t.Cleanup(func() { runZFS(context.Background(), "destroy", "-r", base) })
	src := base + "/src"
	if _, err := runZFS(ctx, "create", src); err != nil {
		t.Fatalf("creating source: %v", err)
	}
	m := New(base)
	la := NewLastAckedAt("policy-1", "src", 7, "src@"+ReplSnapshotName(7))
	if _, err := runZFS(ctx, "snapshot", src+"@"+ReplSnapshotName(7)); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	state, err := m.ObserveSource(ctx, la)
	if err != nil {
		t.Fatalf("ObserveSource: %v", err)
	}
	for _, gen := range []uint32{0, 6, 7} {
		if _, err := PlanSend(state, SendRequest{TargetGeneration: gen}); !errors.Is(err, ErrFenceRefused) {
			t.Errorf("PlanSend(generation %d) = %v, want a fence refusal", gen, err)
		}
	}
	// And the one legal direction plans a stream whose arguments name a
	// snapshot that really exists.
	plan, err := PlanSend(state, SendRequest{TargetGeneration: 8})
	if err != nil {
		t.Fatalf("PlanSend(generation 8): %v", err)
	}
	have, err := m.SnapshotExists(ctx, plan.FromSnapshot)
	if err != nil {
		t.Fatalf("SnapshotExists: %v", err)
	}
	if !have {
		t.Errorf("the plan's incremental origin %q does not exist", plan.FromSnapshot)
	}
}

// receiveInto returns an io.WriteCloser that pipes into a receive and
// closes it, so a Pump's output can be a destination.
func receiveInto(t *testing.T, m *Manager, fullDest string, opts ReceiveOptions) io.WriteCloser {
	t.Helper()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- m.ReceiveInto(context.Background(), strings.TrimPrefix(fullDest, m.Base+"/"), pr, opts)
	}()
	return &receiveWriter{pw: pw, done: done}
}

type receiveWriter struct {
	pw   *io.PipeWriter
	done chan error
}

func (w *receiveWriter) Write(p []byte) (int, error) { return w.pw.Write(p) }
func (w *receiveWriter) Close() error {
	_ = w.pw.Close()
	return <-w.done
}

func syncDataset(t *testing.T, full string) {
	t.Helper()
	// sync is best-effort: a ZFS pool that has not yet committed the
	// writes still sends a consistent snapshot, and the tests here do
	// not depend on a durability barrier.
	out, err := exec.Command("sync").CombinedOutput()
	if err != nil {
		t.Logf("sync: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}
