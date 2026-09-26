package jailnet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// converged is a node where jail "web" is completely healthy: a
// recorded epair pair that exists, is up, is on bridge1, and whose jail
// side carries exactly the assigned address and gateway. Every
// "nothing changed" assertion starts from here.
func converged(t *testing.T) (*Reconciler, *fakeRunner, *fakeBridge, *fakeJail, string) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "jail-epairs.json")
	if err := os.WriteFile(statePath, []byte(`{"epairs":{"web":{"host_side":"epair0a","jail_side":"epair0b","bridge":"bridge1"}}}`), 0o644); err != nil {
		t.Fatalf("seeding state file: %v", err)
	}
	runner := newFakeRunner().withState("epair0a", true, "bridge1").withState("epair0b", true, "")
	bridge := newFakeBridge(runner).withBridge("bridge1")
	j := newFakeJail().up("web", "epair0b", []string{"10.0.1.5/24"}, "10.0.1.1")
	r := &Reconciler{Bridge: bridge, Jail: j, Runner: runner, StatePath: statePath}
	return r, runner, bridge, j, statePath
}

func testAddressing() Addressing {
	return Addressing{Bridge: "bridge1", Interface: "epair0b", IP: "10.0.1.5", PrefixLen: 24, Gateway: "10.0.1.1"}
}

// TestEnsure_InSyncIsIdempotent is the property that makes calling this
// on every reconcile tick affordable. A converged node must be observed
// and left completely alone: no bridge creation, no new epair, no
// ifconfig writes, no in-jail addressing write. Three passes must look
// exactly like one.
func TestEnsure_InSyncIsIdempotent(t *testing.T) {
	r, runner, _, j, _ := converged(t)

	for i := 0; i < 3; i++ {
		res := r.Ensure(t.Context(), "web", testAddressing())
		if res.Verdict != VerdictInSync {
			t.Fatalf("pass %d: Verdict = %q (%s), want %q", i+1, res.Verdict, res.Detail, VerdictInSync)
		}
		if !res.Observed {
			t.Errorf("pass %d: Observed = false, want true for a fully-observed pass", i+1)
		}
		if len(res.Findings) != 0 || len(res.Repairs) != 0 {
			t.Errorf("pass %d: Findings=%v Repairs=%v, want both empty", i+1, res.Findings, res.Repairs)
		}
	}

	if n := runner.count(); n != 3 {
		t.Errorf("ran %d host commands over 3 passes, want 3 (one ifconfig observation each, nothing else)", n)
	}
	for _, forbidden := range []string{"ifconfig epair0a up", "ifconfig bridge1 addm epair0a", "ifconfig bridge1 deletem epair0a"} {
		if n := runner.ran(forbidden); n != 0 {
			t.Errorf("command %q ran %d times, want 0: a converged node must not be touched", forbidden, n)
		}
	}
	if got := runner.epairHostSides(); got != 1 {
		t.Errorf("node has %d host-side epairs after 3 passes, want 1", got)
	}
	if n := j.fixCount(); n != 0 {
		t.Errorf("in-jail addressing was rewritten %d times, want 0", n)
	}
}

// TestEnsure_ColdStartProvisionsAndThenSettles walks the first-time
// provisioning path end to end and then confirms the second pass finds
// nothing to do - which is the only way to know the first pass
// actually converged rather than merely appearing to.
func TestEnsure_ColdStartProvisionsAndThenSettles(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "jail-epairs.json")
	runner := newFakeRunner()
	bridge := newFakeBridge(runner)
	j := newFakeJail().up("web", "epair0b", []string{"10.0.1.5/24"}, "10.0.1.1")
	r := &Reconciler{Bridge: bridge, Jail: j, Runner: runner, StatePath: statePath}

	res := r.Ensure(t.Context(), "web", testAddressing())
	if !res.Observed {
		t.Fatalf("Observed = false (%s), want true", res.Detail)
	}
	// First-time provisioning is a repair, not a no-op: this pass
	// created a bridge and an epair pair. The second pass below is the
	// one that has to come back in_sync.
	if res.Verdict != VerdictRepaired {
		t.Errorf("Verdict = %q (%s), want %q", res.Verdict, res.Detail, VerdictRepaired)
	}
	if res.HostSide != "epair0a" || res.JailSide != "epair0b" {
		t.Errorf("pair = %s/%s, want epair0a/epair0b", res.HostSide, res.JailSide)
	}
	if !bridge.has("bridge1") {
		t.Error("bridge1 was not created")
	}
	if got := runner.memberOf("epair0a"); got != "bridge1" {
		t.Errorf("epair0a is on bridge %q, want bridge1", got)
	}

	// The record must be on disk, and readable in the shape both this
	// package and internal/cluster's Stage 1 code use.
	body, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("reading state file: %v", err)
	}
	for _, want := range []string{`"epairs"`, `"host_side": "epair0a"`, `"jail_side": "epair0b"`, `"bridge": "bridge1"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("state file %s does not contain %q", body, want)
		}
	}

	// Second pass must be a no-op, using the *same* runner view of the
	// interface the first pass created.
	before := runner.count()
	res2 := r.Ensure(t.Context(), "web", testAddressing())
	if res2.Verdict != VerdictInSync || len(res2.Repairs) != 0 {
		t.Errorf("second pass = %q with repairs %v, want in_sync with none (%s)", res2.Verdict, res2.Repairs, res2.Detail)
	}
	if got := runner.count(); got != before+1 {
		t.Errorf("second pass ran %d commands, want 1 (the ifconfig observation)", got-before)
	}
	if bridge.pairCount() != 1 {
		t.Errorf("node has %d epairs, want 1: a second pass must not have leaked another", bridge.pairCount())
	}
}

// TestEnsure_HostRepairIsNotReportedAsInSync covers the split between
// the two success verdicts. A pass that had to rebuild a dead epair
// pair and then found the jail's own addressing already correct has
// still changed the node, and reporting plain in_sync would hide a jail
// that needs rebuilding after every reboot - which is the whole problem
// this package exists to make visible.
func TestEnsure_HostRepairIsNotReportedAsInSync(t *testing.T) {
	r, runner, _, _, _ := converged(t)
	runner.without("epair0a")

	res := r.Ensure(t.Context(), "web", testAddressing())
	if res.Verdict != VerdictRepaired {
		t.Errorf("Verdict = %q, want %q: the epair was recreated even though the jail's addressing was already correct", res.Verdict, VerdictRepaired)
	}
	if !res.Observed {
		t.Errorf("Observed = false, want true (%s)", res.Detail)
	}
	if len(res.Repairs) == 0 {
		t.Error("Repairs is empty, but a pair was created")
	}
}

// TestEnsure_RebootRecreatesEpair is the single highest-value case in
// this package, and the one Stage 1 got wrong: an epair(4) pair does
// not survive a reboot, so the recorded names come back dead and every
// subsequent tick would hand jail(8) an interface that does not exist.
// A pair that is recorded but absent must be re-provisioned, the
// record rewritten, and nothing else touched.
func TestEnsure_RebootRecreatesEpair(t *testing.T) {
	r, runner, bridge, _, statePath := converged(t)
	// The reboot: the recorded interface is gone. The record is not -
	// which is exactly the state a node comes up in, because epair(4)
	// pairs are not persistent and nothing recreates them at boot.
	runner.without("epair0a")
	runner.without("epair0b")

	res := r.Ensure(t.Context(), "web", testAddressing())
	if !res.Observed {
		t.Fatalf("Observed = false (%s), want true: an absent interface is a definite answer", res.Detail)
	}
	if !has(res.Findings, FindingEpairMissing) {
		t.Errorf("Findings = %v, want it to include %q", res.Findings, FindingEpairMissing)
	}
	// FreeBSD re-uses the lowest free epair number, so the new pair may
	// well be called epair0a again. What matters is that a pair was
	// actually created and recorded, not what it was named - asserting
	// on the name would be asserting on FreeBSD's allocator.
	if bridge.pairCount() != 1 {
		t.Errorf("node has %d epairs, want 1: the dead pair's names are gone, so exactly one fresh pair is required", bridge.pairCount())
	}
	if !runner.has(res.HostSide) {
		t.Errorf("HostSide = %q, which does not exist on the node", res.HostSide)
	}

	// The record must have been rewritten, or the next tick would
	// repeat this whole repair forever.
	state, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("reloading state: %v", err)
	}
	rec, present, usable := state.Lookup("web")
	if !present || !usable {
		t.Fatalf("Lookup(web) = %+v, present=%v usable=%v, want a usable new record", rec, present, usable)
	}
	if rec.HostSide != res.HostSide {
		t.Errorf("recorded host side = %q, want %q (the one just created)", rec.HostSide, res.HostSide)
	}
}

// TestEnsure_DriftMatrix walks every drift this package claims to
// repair, one at a time, from a converged baseline, and asserts both
// the finding and the node state afterwards.
func TestEnsure_DriftMatrix(t *testing.T) {
	tests := []struct {
		name       string
		corrupt    func(*fakeRunner, *fakeBridge, *fakeJail)
		want       Finding
		wantRepair bool
		check      func(*testing.T, *Reconciler, *fakeRunner, *fakeBridge, *fakeJail, Result)
	}{
		{
			name: "host side is down",
			corrupt: func(r *fakeRunner, b *fakeBridge, j *fakeJail) {
				r.withState("epair0a", false, "bridge1")
			},
			want: FindingHostSideDown,
			check: func(t *testing.T, _ *Reconciler, r *fakeRunner, _ *fakeBridge, _ *fakeJail, _ Result) {
				if n := r.ran("ifconfig epair0a up"); n != 1 {
					t.Errorf("ran `ifconfig epair0a up` %d times, want 1", n)
				}
			},
		},
		{
			name: "bridge was destroyed and came back with no members",
			corrupt: func(r *fakeRunner, b *fakeBridge, j *fakeJail) {
				r.withState("epair0a", true, "")
			},
			want: FindingBridgeMembershipWrong,
			check: func(t *testing.T, _ *Reconciler, r *fakeRunner, _ *fakeBridge, _ *fakeJail, _ Result) {
				if got := r.memberOf("epair0a"); got != "bridge1" {
					t.Errorf("epair0a is on bridge %q, want bridge1", got)
				}
			},
		},
		{
			name: "host side is on the wrong bridge entirely",
			corrupt: func(r *fakeRunner, b *fakeBridge, j *fakeJail) {
				r.withState("epair0a", true, "bridge9")
				b.withBridge("bridge9")
			},
			want: FindingBridgeMembershipWrong,
			check: func(t *testing.T, _ *Reconciler, r *fakeRunner, b *fakeBridge, _ *fakeJail, _ Result) {
				// Membership is exclusive: the jail must not be left
				// reachable from a network it was never attached to.
				if n := r.ran("ifconfig bridge9 deletem epair0a"); n != 1 {
					t.Errorf("ran `ifconfig bridge9 deletem epair0a` %d times, want 1", n)
				}
				if got := r.memberOf("epair0a"); got != "bridge1" {
					t.Errorf("epair0a is on bridge %q, want bridge1", got)
				}
			},
		},
		{
			name: "the jail's own address is missing",
			corrupt: func(r *fakeRunner, b *fakeBridge, j *fakeJail) {
				j.up("web", "epair0b", nil, "10.0.1.1")
			},
			want: FindingAddressWrong,
			check: func(t *testing.T, _ *Reconciler, _ *fakeRunner, _ *fakeBridge, j *fakeJail, _ Result) {
				if n := j.fixCount(); n != 1 {
					t.Errorf("in-jail addressing was repaired %d times, want 1", n)
				}
			},
		},
		{
			name: "the jail's own address has the wrong prefix length",
			corrupt: func(r *fakeRunner, b *fakeBridge, j *fakeJail) {
				j.up("web", "epair0b", []string{"10.0.1.5/16"}, "10.0.1.1")
			},
			want: FindingAddressWrong,
			check: func(t *testing.T, _ *Reconciler, _ *fakeRunner, _ *fakeBridge, j *fakeJail, _ Result) {
				if got := j.addresses("web"); len(got) != 1 || got[0] != "10.0.1.5/24" {
					t.Errorf("jail addresses = %v, want exactly [10.0.1.5/24]", got)
				}
			},
		},
		{
			name: "something else put a second address on the jail's interface",
			corrupt: func(r *fakeRunner, b *fakeBridge, j *fakeJail) {
				j.up("web", "epair0b", []string{"10.0.1.5/24", "192.168.99.9/24"}, "10.0.1.1")
			},
			want: FindingForeignAddress,
			check: func(t *testing.T, _ *Reconciler, _ *fakeRunner, _ *fakeBridge, j *fakeJail, _ Result) {
				if got := j.addresses("web"); len(got) != 1 || got[0] != "10.0.1.5/24" {
					t.Errorf("jail addresses = %v, want the unassigned one removed", got)
				}
			},
		},
		{
			name: "the jail's default route is missing",
			corrupt: func(r *fakeRunner, b *fakeBridge, j *fakeJail) {
				j.up("web", "epair0b", []string{"10.0.1.5/24"}, "")
			},
			want: FindingRouteWrong,
			check: func(t *testing.T, _ *Reconciler, _ *fakeRunner, _ *fakeBridge, j *fakeJail, _ Result) {
				if got := j.route("web"); got != "10.0.1.1" {
					t.Errorf("jail default route = %q, want 10.0.1.1", got)
				}
			},
		},
		{
			name: "the jail's default route points at the wrong gateway",
			corrupt: func(r *fakeRunner, b *fakeBridge, j *fakeJail) {
				j.up("web", "epair0b", []string{"10.0.1.5/24"}, "10.0.1.254")
			},
			want: FindingRouteWrong,
			check: func(t *testing.T, _ *Reconciler, _ *fakeRunner, _ *fakeBridge, j *fakeJail, _ Result) {
				if got := j.route("web"); got != "10.0.1.1" {
					t.Errorf("jail default route = %q, want 10.0.1.1", got)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, runner, bridge, j, _ := converged(t)
			tt.corrupt(runner, bridge, j)

			res := r.Ensure(t.Context(), "web", testAddressing())
			if !res.Observed {
				t.Fatalf("Observed = false (%s), want true", res.Detail)
			}
			if !has(res.Findings, tt.want) {
				t.Errorf("Findings = %v, want it to include %q (%s)", res.Findings, tt.want, res.Detail)
			}
			if res.Verdict != VerdictRepaired {
				t.Errorf("Verdict = %q, want %q", res.Verdict, VerdictRepaired)
			}
			if len(res.Repairs) == 0 {
				t.Errorf("Repairs is empty, want a description of what changed")
			}
			tt.check(t, r, runner, bridge, j, res)

			// Every repaired case must converge: a second pass has
			// nothing left to do. This is the assertion that catches a
			// repair which only looks like one.
			res2 := r.Ensure(t.Context(), "web", testAddressing())
			if res2.Verdict != VerdictInSync || len(res2.Repairs) != 0 {
				t.Errorf("second pass = %q with repairs %v, want in_sync with none (%s)", res2.Verdict, res2.Repairs, res2.Detail)
			}
		})
	}
}

// TestEnsure_JailRunningWithoutItsInterfaceNeedsRestart covers the one
// drift that cannot be repaired in place. The verdict must be drifted
// with FindingRestartRequired, and crucially must NOT be repaired or
// reported as fine - a reconciler that "succeeds" here leaves the jail
// permanently without an address and reports success forever.
func TestEnsure_JailRunningWithoutItsInterfaceNeedsRestart(t *testing.T) {
	r, _, _, j, _ := converged(t)
	j.upWithoutIface("web", "epair0b")

	res := r.Ensure(t.Context(), "web", testAddressing())
	if res.Verdict != VerdictDrifted {
		t.Errorf("Verdict = %q, want %q", res.Verdict, VerdictDrifted)
	}
	if !has(res.Findings, FindingRestartRequired) {
		t.Errorf("Findings = %v, want %q", res.Findings, FindingRestartRequired)
	}
	if !strings.Contains(res.Detail, "restart") {
		t.Errorf("Detail = %q, want it to name the restart that is actually required", res.Detail)
	}
	if j.fixCount() != 0 {
		t.Errorf("in-jail addressing was written %d times, want 0: there is no interface to write it to", j.fixCount())
	}
	// It must be stable, not oscillate: a second pass reaches the same
	// verdict rather than converging to in_sync by luck.
	res2 := r.Ensure(t.Context(), "web", testAddressing())
	if res2.Verdict != VerdictDrifted {
		t.Errorf("second pass Verdict = %q, want the same %q", res2.Verdict, VerdictDrifted)
	}
}

// TestEnsure_UnknownIsNeverInSync is the honesty matrix. Every way this
// package can fail to find out must produce unknown, with Observed
// false and Detail naming the reason - never in_sync, never
// "drifted", never a repair. Each of these is a distinct real failure
// mode, and getting any one of them wrong means Apiary acts on a
// conclusion it never reached.
func TestEnsure_UnknownIsNeverInSync(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*Reconciler, *fakeRunner, *fakeBridge, *fakeJail, string)
		want    string // a substring Detail must contain
	}{
		{
			name: "ifconfig cannot run at all",
			corrupt: func(r *Reconciler, run *fakeRunner, b *fakeBridge, j *fakeJail, p string) {
				run.failAll("ifconfig: ioctl: Operation not permitted")
			},
			want: "could not be determined",
		},
		{
			name: "ifconfig itself is not installed",
			corrupt: func(r *Reconciler, run *fakeRunner, b *fakeBridge, j *fakeJail, p string) {
				run.failAll("sh: ifconfig: not found")
			},
			want: "could not be determined",
		},
		{
			name: "the interface observation is not an absent-object answer",
			corrupt: func(r *Reconciler, run *fakeRunner, b *fakeBridge, j *fakeJail, p string) {
				run.fail("ifconfig epair0a", "ifconfig: ioctl: Operation not permitted")
			},
			want: "could not be determined",
		},
		{
			name: "the record file is corrupt",
			corrupt: func(r *Reconciler, run *fakeRunner, b *fakeBridge, j *fakeJail, p string) {
				if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
					t.Fatalf("corrupting state file: %v", err)
				}
			},
			want: "could not be determined",
		},
		{
			name: "jail existence cannot be checked",
			corrupt: func(r *Reconciler, run *fakeRunner, b *fakeBridge, j *fakeJail, p string) {
				j.failList = "jls: permission denied"
			},
			want: "could not be determined",
		},
		{
			name: "the in-jail view cannot be read",
			corrupt: func(r *Reconciler, run *fakeRunner, b *fakeBridge, j *fakeJail, p string) {
				j.failObs = "jexec: operation not permitted"
			},
			want: "could not be determined",
		},
		{
			name: "the bridge driver fails",
			corrupt: func(r *Reconciler, run *fakeRunner, b *fakeBridge, j *fakeJail, p string) {
				run.withState("epair0a", true, "")
				b.failMem = "ifconfig: ioctl: Operation not permitted"
			},
			want: "could not be determined",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, runner, bridge, j, statePath := converged(t)
			tt.corrupt(r, runner, bridge, j, statePath)

			res := r.Ensure(t.Context(), "web", testAddressing())
			if res.Verdict != VerdictUnknown {
				t.Errorf("Verdict = %q, want %q", res.Verdict, VerdictUnknown)
			}
			if res.Observed {
				t.Error("Observed = true, want false: nothing was established")
			}
			if !res.Unknown() {
				t.Error("Unknown() = false, want true")
			}
			if !strings.Contains(res.Detail, tt.want) {
				t.Errorf("Detail = %q, want it to contain %q", res.Detail, tt.want)
			}
			// Findings gathered before the failure are real and are
			// reported; what must never appear is a finding that
			// required the observation that failed.
			for _, f := range res.Findings {
				switch f {
				case FindingAddressWrong, FindingRouteWrong, FindingForeignAddress, FindingRestartRequired:
					t.Errorf("Findings includes %q, which requires the observation that failed: a finding derived from a failed observation is a false accusation", f)
				}
			}
		})
	}
}

// TestEnsure_MissingDriverIsUnknown confirms a node without bridge or
// jail support says so, rather than reporting a VNET jail's networking
// as fine because there was nothing configured to check it with.
func TestEnsure_MissingDriverIsUnknown(t *testing.T) {
	_, runner, bridge, j, statePath := converged(t)

	for _, tt := range []struct {
		name string
		r    *Reconciler
	}{
		{name: "no bridge driver", r: &Reconciler{Jail: j, Runner: runner, StatePath: statePath}},
		{name: "no jail driver", r: &Reconciler{Bridge: bridge, Runner: runner, StatePath: statePath}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := tt.r.Ensure(t.Context(), "web", testAddressing())
			if res.Verdict != VerdictUnknown || res.Observed {
				t.Errorf("Ensure() = %q observed=%v, want unknown/unobserved", res.Verdict, res.Observed)
			}
		})
	}
}

// TestEnsure_NotRunningIsNotUnknown confirms the distinction that keeps
// normal provisioning from being reported as a problem: a jail that has
// not been created yet is a definite, expected state.
func TestEnsure_NotRunningIsNotUnknown(t *testing.T) {
	r, _, _, j, _ := converged(t)
	j.stopped("web")

	res := r.Ensure(t.Context(), "web", testAddressing())
	if res.Verdict != VerdictNotRunning {
		t.Errorf("Verdict = %q, want %q", res.Verdict, VerdictNotRunning)
	}
	if !res.Observed {
		t.Error("Observed = false, want true: not-running is a definite answer, not a failure to look")
	}
	if res.Unknown() {
		t.Error("Unknown() = true, want false")
	}
}

// TestEnsure_UnallocatedJailIsLeftAlone confirms the ADR-0117
// uplink_bridged carve-out: a jail on a network that skips allocation
// manages its own addressing, so the reconciler must not observe a
// "wrong" address and delete it.
func TestEnsure_UnallocatedJailIsLeftAlone(t *testing.T) {
	r, _, _, j, _ := converged(t)
	// The jail configured itself something completely unlike the
	// subnet, which is exactly what a jail doing its own DHCP would
	// look like.
	j.up("web", "epair0b", []string{"169.254.7.7/16"}, "")

	res := r.Ensure(t.Context(), "web", Addressing{Bridge: "bridge1", Interface: "epair0b"})
	if res.Verdict != VerdictInSync {
		t.Errorf("Verdict = %q, want %q (%s)", res.Verdict, VerdictInSync, res.Detail)
	}
	if len(res.Findings) != 0 {
		t.Errorf("Findings = %v, want none: an unallocated jail's addressing is not Apiary's to judge", res.Findings)
	}
	if j.fixCount() != 0 {
		t.Errorf("in-jail addressing was written %d times, want 0", j.fixCount())
	}
}

// TestEnsure_AdoptsAStage1Record confirms upgrade compatibility: a
// record written by internal/cluster's Stage 1 reconciler has no bridge
// field at all, and must be adopted rather than treated as unusable -
// otherwise every VNET jail on an upgraded node would get a second
// epair on the first tick.
func TestEnsure_AdoptsAStage1Record(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "jail-epairs.json")
	stage1 := `{"epairs":{"web":{"host_side":"epair0a","jail_side":"epair0b"}}}`
	if err := os.WriteFile(statePath, []byte(stage1), 0o644); err != nil {
		t.Fatalf("seeding Stage 1 state file: %v", err)
	}
	runner := newFakeRunner().withState("epair0a", true, "bridge1").withState("epair0b", true, "")
	bridge := newFakeBridge(runner).withBridge("bridge1")
	j := newFakeJail().up("web", "epair0b", []string{"10.0.1.5/24"}, "10.0.1.1")
	r := &Reconciler{Bridge: bridge, Jail: j, Runner: runner, StatePath: statePath}

	// Adopting a Stage 1 record is itself a repair - the record had no
	// bridge field, so this pass backfilled it - but no new interface
	// may be created to do it.
	res := r.Ensure(t.Context(), "web", testAddressing())
	if res.Verdict != VerdictRepaired {
		t.Errorf("Verdict = %q (%s), want %q", res.Verdict, res.Detail, VerdictRepaired)
	}
	if res.HostSide != "epair0a" {
		t.Errorf("HostSide = %q, want epair0a", res.HostSide)
	}
	if got := runner.epairHostSides(); got != 1 {
		t.Errorf("node has %d host-side epairs, want 1: a Stage 1 record must be adopted, not duplicated", got)
	}
	// And the backfill must not be re-reported on the next pass.
	if res2 := r.Ensure(t.Context(), "web", testAddressing()); res2.Verdict != VerdictInSync {
		t.Errorf("second pass = %q (%s), want %q", res2.Verdict, res2.Detail, VerdictInSync)
	}
	if res.HostSide != "epair0a" {
		t.Errorf("HostSide = %q, want epair0a", res.HostSide)
	}
}

// TestEnsure_CorruptRecordIsRepairedNotTrusted confirms that a record
// naming an impossible epair pair is discarded rather than handed to
// jail(8), and that the corruption is surfaced as its own finding so
// an operator learns the file was damaged.
func TestEnsure_CorruptRecordIsRepairedNotTrusted(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "jail-epairs.json")
	if err := os.WriteFile(statePath, []byte(`{"epairs":{"web":{"host_side":"eth9","jail_side":"eth9"}}}`), 0o644); err != nil {
		t.Fatalf("seeding corrupt state file: %v", err)
	}
	runner := newFakeRunner()
	bridge := newFakeBridge(runner).withBridge("bridge1")
	j := newFakeJail().up("web", "epair0b", []string{"10.0.1.5/24"}, "10.0.1.1")
	r := &Reconciler{Bridge: bridge, Jail: j, Runner: runner, StatePath: statePath}

	res := r.Ensure(t.Context(), "web", testAddressing())
	if !res.Observed {
		t.Fatalf("Observed = false (%s), want true", res.Detail)
	}
	if !has(res.Findings, FindingEpairRecordCorrupt) {
		t.Errorf("Findings = %v, want it to include %q", res.Findings, FindingEpairRecordCorrupt)
	}
	if res.HostSide == "eth9" {
		t.Error("HostSide = eth9, want a real epair: an impossible record must never reach jail(8)")
	}
	if got := runner.epairHostSides(); got != 1 {
		t.Errorf("node has %d host-side epairs, want exactly 1", got)
	}
}

// TestEnsure_FailedRecordWriteDoesNotLeakInterfaces is the ordering
// constraint that keeps a failed write from costing a node an
// interface per tick: the pair is created, the record is written, and
// only if the write fails is the pair destroyed again.
func TestEnsure_FailedRecordWriteDoesNotLeakInterfaces(t *testing.T) {
	// A read-only directory makes the write fail deterministically with
	// no special-casing in the reconciler, while the read still
	// succeeds - which is precisely the window this test is about: the
	// pair is created, and the record of it cannot be written.
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatalf("creating read-only dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if os.Geteuid() == 0 {
		t.Skip("running as root, which ignores directory permissions")
	}
	runner := newFakeRunner()
	bridge := newFakeBridge(runner).withBridge("bridge1")
	j := newFakeJail()
	r := &Reconciler{Bridge: bridge, Jail: j, Runner: runner, StatePath: filepath.Join(dir, "state.json")}

	res := r.Ensure(t.Context(), "web", testAddressing())
	if res.Verdict != VerdictUnknown {
		t.Errorf("Verdict = %q, want %q: a record that could not be written is unknown, not a success", res.Verdict, VerdictUnknown)
	}
	if res.Observed {
		t.Error("Observed = true, want false")
	}
	if bridge.pairCount() != 0 {
		t.Errorf("node has %d epairs after a failed record write, want 0: the unrecorded pair must be destroyed", bridge.pairCount())
	}
	if !strings.Contains(res.Detail, "destroyed") {
		t.Errorf("Detail = %q, want it to say the pair was destroyed so the next tick starts clean", res.Detail)
	}
}

// TestEnsure_JailMovedToAnotherNetwork confirms a jail whose network
// changed is moved rather than duplicated, and that the record is
// corrected so the move is not re-reported forever.
func TestEnsure_JailMovedToAnotherNetwork(t *testing.T) {
	r, runner, bridge, _, statePath := converged(t)
	bridge.withBridge("bridge2")
	runner.withState("epair0a", true, "bridge2")

	moved := testAddressing()
	moved.Bridge = "bridge2"
	res := r.Ensure(t.Context(), "web", moved)
	if !has(res.Findings, FindingBridgeMembershipWrong) {
		t.Errorf("Findings = %v, want %q", res.Findings, FindingBridgeMembershipWrong)
	}
	if got := runner.memberOf("epair0a"); got != "bridge2" {
		t.Errorf("epair0a is on bridge %q, want bridge2", got)
	}
	if got := runner.epairHostSides(); got != 1 {
		t.Errorf("node has %d host-side epairs, want 1: a move must reuse the pair, not make another", got)
	}
	state, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("reloading state: %v", err)
	}
	if got := state.Epairs["web"].Bridge; got != "bridge2" {
		t.Errorf("recorded bridge = %q, want bridge2", got)
	}
	// And the move must not be re-reported on the next pass.
	res2 := r.Ensure(t.Context(), "web", moved)
	if len(res2.Findings) != 0 {
		t.Errorf("second pass Findings = %v, want none (%s)", res2.Findings, res2.Detail)
	}
}

func has(findings []Finding, want Finding) bool {
	for _, f := range findings {
		if f == want {
			return true
		}
	}
	return false
}
