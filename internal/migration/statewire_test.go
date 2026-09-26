package migration

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestWireRoundTripPreservesEverything: the raft-state conversion is
// the only thing standing between this package and the proto, so a
// field that fails to survive a round trip is a field raft would
// silently drop on a snapshot restore.
func TestWireRoundTripPreservesEverything(t *testing.T) {
	// A record with every field populated, so nothing is exercised by
	// its zero value.
	r := advanceTo(t, newRecordAt("mig-wire", baseUnix), PhaseSync)
	r.Detail = "mid-copy, link is slow"
	var err error
	r, err = UpdateTransfer(r, Transfer{
		RequiredBytes:        1 << 30,
		MovedBytes:           1 << 29,
		FromToken:            41,
		ToToken:              42,
		TargetAlreadyReplica: false,
	})
	requireNoError(t, err, "update transfer")
	r, err = Stall(r, Stalled{Reason: "target-unreachable", Detail: "dial to comb-b timed out", Verdict: "unknown"}, baseUnix+200)
	requireNoError(t, err, "stall")
	r.Evidence = []Evidence{
		{Rule: "preflight-target-health", Detail: "comb-b health observed healthy", ObservedAtUnix: baseUnix + 1},
		{Rule: "freeze-timeout-budget", Detail: "operator budget is 15m", ObservedAtUnix: baseUnix + 2},
	}
	requireNoError(t, r.Validate(), "populated record")

	w, err := ToWire(r)
	requireNoError(t, err, "to wire")
	if w.StateVersion != WireSchemaVersion {
		t.Errorf("wire state version = %d, want %d", w.StateVersion, WireSchemaVersion)
	}

	back, err := FromWire(w)
	requireNoError(t, err, "from wire")
	if !reflect.DeepEqual(r, back) {
		t.Errorf("round trip lost data:\n got %+v\nwant %+v", back, r)
	}
}

// TestWireRoundTripOfEveryOutcome: the four distinguishable outcomes,
// plus abort, must each survive the wire. An outcome that decodes to
// something else is the whole bug this package guards against.
func TestWireRoundTripOfEveryOutcome(t *testing.T) {
	r := advanceTo(t, newRecordAt("mig-wire-outcomes", baseUnix), PhaseCutover)
	at := baseUnix + 700

	// Success.
	rec, err := RecordVerification(r, matchingVerification(at))
	requireNoError(t, err, "record verification")
	ok, err := Settle(rec, OutcomeObservedComplete, observedEvidence(at), "done", at)
	requireNoError(t, err, "settle success")

	cases := []struct {
		name string
		rec  Record
		want Outcome
	}{
		{"in flight", r, OutcomeInProgress},
		{"success", ok, OutcomeObservedComplete},
		{"unobserved", mustSettle(t, r, OutcomeUnobserved, "dial timeout", at), OutcomeUnobserved},
		{"unverified", mustSettle(t, r, OutcomeUnverified, "quorum unavailable", at), OutcomeUnverified},
		{"failed", mustSettle(t, r, OutcomeFailed, "zfs recv failed", at), OutcomeFailed},
		{"aborted", mustSettle(t, r, OutcomeAborted, "operator stopped it", at), OutcomeAborted},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, err := ToWire(tc.rec)
			requireNoError(t, err, "to wire")
			back, err := FromWire(w)
			requireNoError(t, err, "from wire")
			if back.Outcome != tc.want {
				t.Errorf("outcome did not survive the wire: got %s, want %s", back.Outcome, tc.want)
			}
			if back.Outcome.IsCompletionNeverObserved() != tc.want.IsCompletionNeverObserved() {
				t.Errorf("unknown-ness did not survive the wire for %s", tc.want)
			}
			if !reflect.DeepEqual(tc.rec, back) {
				t.Errorf("round trip lost data for %s:\n got %+v\nwant %+v", tc.want, back, tc.rec)
			}
		})
	}
}

func mustSettle(t *testing.T, r Record, outcome Outcome, detail string, at int64) Record {
	t.Helper()
	ev := []Evidence(nil)
	if outcome.IsFailure() {
		ev = failureEvidence(detail, at)
	}
	out, err := Settle(r, outcome, ev, detail, at)
	requireNoError(t, err, "settle to "+string(outcome))
	return out
}

// TestFromWireRefusesACorruptRecord: the untrusted direction. A record
// that has been corrupted into "phase cutover, outcome complete, no
// verification" is the most dangerous object this feature could read,
// and a decoder that returned it would hand the reconciler a licence to
// start a guest in two places.
func TestFromWireRefusesACorruptRecord(t *testing.T) {
	good := newRecordAt("mig-corrupt", baseUnix)

	corruptions := map[string]func(w *WireRecord){
		"claims success with no verification": func(w *WireRecord) {
			w.Outcome = OutcomeObservedComplete
			w.SettledUnix = baseUnix + 10
		},
		"claims success with a mismatched guid": func(w *WireRecord) {
			w.Outcome = OutcomeObservedComplete
			w.SettledUnix = baseUnix + 10
			w.VerifiedObserved = true
			w.VerifiedRunning = true
			w.ObservedGUID = "guid-other"
			w.ExpectedGUID = "guid-abc"
			w.ObservedIP = "10.0.0.5"
			w.ExpectedIP = "10.0.0.5"
		},
		"unknown phase":                        func(w *WireRecord) { w.Phase = "halfway" },
		"unknown outcome":                      func(w *WireRecord) { w.Outcome = "probably_fine" },
		"no outcome at all":                    func(w *WireRecord) { w.Outcome = "" },
		"zero token":                           func(w *WireRecord) { w.AttemptToken = 0 },
		"zero attempt":                         func(w *WireRecord) { w.Attempt = 0 },
		"source equals target":                 func(w *WireRecord) { w.TargetNodeID = w.SourceNodeID },
		"no source":                            func(w *WireRecord) { w.SourceNodeID = "" },
		"no guest kind":                        func(w *WireRecord) { w.GuestKind = "container" },
		"bytes moved past the requirement":     func(w *WireRecord) { w.BytesRequired = 10; w.BytesMoved = 20 },
		"resume token behind where it started": func(w *WireRecord) { w.SendFromToken = 9; w.SendToToken = 3 },
		"terminal with no settle timestamp":    func(w *WireRecord) { w.Outcome = OutcomeFailed; w.SettledUnix = 0 },
		"settled and stalled at once": func(w *WireRecord) {
			w.Outcome = OutcomeFailed
			w.SettledUnix = baseUnix + 10
			w.StallReason = "target-unreachable"
			w.StallDetail = "dial timed out"
		},
		"failures with no evidence": func(w *WireRecord) {
			w.Outcome = OutcomeFailed
			w.SettledUnix = baseUnix + 10
		},
		"schema version this build does not know": func(w *WireRecord) { w.StateVersion = WireSchemaVersion + 1 },
	}

	for name, mutate := range corruptions {
		t.Run(name, func(t *testing.T) {
			w, err := ToWire(good)
			requireNoError(t, err, "to wire")
			mutate(&w)
			back, err := FromWire(w)
			if err == nil {
				t.Fatalf("a corrupt record decoded successfully: %+v", back)
			}
			// The whole point: it is an error, not a best-effort read.
			if back.ID != "" {
				t.Errorf("a refused decode still returned a record: %+v", back)
			}
		})
	}
}

// TestFromWireAcceptsAnUnversionedRecord: a record written by an older
// build that predates the version field is the current schema, and must
// not be rejected for the absence of a field that says "version 1".
func TestFromWireAcceptsAnUnversionedRecord(t *testing.T) {
	r := newRecordAt("mig-unversioned", baseUnix)
	w, err := ToWire(r)
	requireNoError(t, err, "to wire")
	w.StateVersion = 0
	back, err := FromWire(w)
	requireNoError(t, err, "from wire")
	if back.ID != r.ID || back.Phase != r.Phase {
		t.Errorf("unversioned record did not decode: %+v", back)
	}
}

// TestUndecodableRecordStopsFencingAndSaysSo: an undecodable record
// is treated as absent - a caller cannot be handed a half-understood
// record - and DecodeErrors names it, because a record that silently
// vanished from the fence is a guest that starts in two places.
func TestUndecodableRecordStopsFencingAndSaysSo(t *testing.T) {
	st, _, _ := stateWithMigration(t, "mig-bad", PhaseBulk)

	st.Migrations["mig-bad"] = WireRecord{ID: "mig-bad", StateVersion: WireSchemaVersion + 1}

	if _, ok := st.Lookup("mig-bad"); ok {
		t.Error("an undecodable record was returned as a usable record")
	}
	errs := st.DecodeErrors()
	if len(errs) != 1 {
		t.Fatalf("DecodeErrors returned %d errors, want 1: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "mig-bad") {
		t.Errorf("the decode error does not name the record: %v", errs[0])
	}
	// A healthy record alongside it still decodes, and a healthy guest
	// is still unfenced.
	if d := st.FenceFor(Workload{Kind: WorkloadKindJail, ID: "other"}); d.Blocked {
		t.Error("an undecodable record fenced an unrelated guest")
	}
	if !strings.Contains(st.Describe(), "undecodable") {
		t.Errorf("Describe does not mention the undecodable record: %s", st.Describe())
	}
	// And the good record in the same state is unaffected.
	good, _, _ := stateWithMigration(t, "mig-good", PhaseSync)
	good.Migrations["mig-bad"] = WireRecord{ID: "mig-bad", Phase: "nonsense"}
	if len(good.DecodeErrors()) != 1 {
		t.Errorf("a corrupt record alongside good ones was not reported: %v", good.DecodeErrors())
	}
	if r, ok := good.Lookup("mig-good"); !ok || r.Phase != PhaseSync {
		t.Error("a good record stopped decoding because a bad one was present")
	}
}

// TestGuestIndexLifecycle: the index is the reconciler's single point
// read, so it is asserted through the whole lifecycle - start, settle
// known, settle unknown - rather than at one moment.
func TestGuestIndexLifecycle(t *testing.T) {
	st := *NewMigrationState()
	c := startCmd()
	next, err := Apply(st, c)
	requireNoError(t, err, "start")
	st = next

	if keys := st.SortedGuestKeys(); len(keys) != 1 || keys[0] != "vm/"+testVM {
		t.Errorf("guest index keys = %v, want [vm/%s]", keys, testVM)
	}
	if GuestIndexKey(jailGuest()) == GuestIndexKey(testGuest()) {
		t.Error("a jail and a VM with the same id produced the same index key")
	}

	rec, _ := st.Lookup(c.ID)
	next, err = Apply(st, Command{
		Kind: CommandSettle, ID: c.ID, Token: rec.Token,
		Verification: Verification{Observed: false, QueryError: "dial timeout"},
		AtUnix:       baseUnix + 100,
	})
	requireNoError(t, err, "settle unobserved")
	st = next
	if len(st.GuestIndex) != 1 {
		t.Errorf("an unobserved settle released the guest index: %v", st.SortedGuestKeys())
	}

	// A second start while the first is unresolved is refused.
	second := startCmd()
	second.ID = "mig-while-unknown"
	second.Token = testToken8
	if _, err := Apply(st, second); !errorsIs(err, ErrGuestAlreadyMigrating) {
		t.Errorf("a second start while a migration is unresolved: err = %v, want ErrGuestAlreadyMigrating", err)
	}

	// Resolving the record - as a failure, since the guest came back -
	// releases the guest and permits a new migration.
	rec, _ = st.Lookup(c.ID)
	next, err = Apply(st, Command{
		Kind: CommandSettle, ID: c.ID, Token: rec.Token,
		FailureDetail: "the target was found to have the guest running after all - a stale read was corrected",
		Evidence:      failureEvidence("corrected by a direct re-read of the target", baseUnix+200),
		AtUnix:        baseUnix + 200,
	})
	if err == nil {
		t.Fatal("a record that already settled was settled again")
	}
	_ = next
}

// TestPurgeRefusesToDropAFence: garbage collection of a migration is
// required (a lifecycle that is never collected is a silent leak), and
// collecting a record that still holds a guest is worse than leaking.
func TestPurgeRefusesToDropAFence(t *testing.T) {
	st, _, _ := stateWithMigration(t, "mig-purge", PhaseBulk)
	held, err := st.Purge("mig-purge")
	if err == nil {
		t.Fatal("purging a record that still fences its guest was allowed")
	}
	if !strings.Contains(err.Error(), "unfence") {
		t.Errorf("purge refusal does not say why: %v", err)
	}
	if len(held.Migrations) != 1 {
		t.Error("a refused purge dropped the record anyway")
	}
	if _, ok := held.FencingFor(testGuest()); !ok {
		t.Error("a refused purge unfenced the guest anyway")
	}

	// A settled, resolved record collects cleanly.
	rec, _ := st.Lookup("mig-purge")
	next, err := Apply(st, Command{
		Kind: CommandSettle, ID: "mig-purge", Token: rec.Token,
		FailureDetail: "the target could not be reached and the operator gave up",
		Evidence:      failureEvidence("operator abandoned the move after the link failed", baseUnix+300),
		AtUnix:        baseUnix + 300,
	})
	requireNoError(t, err, "settle to failed")
	purged, err := next.Purge("mig-purge")
	requireNoError(t, err, "purge of a resolved record")
	if len(purged.Migrations) != 0 {
		t.Errorf("purge left %d records", len(purged.Migrations))
	}
	if len(purged.GuestIndex) != 0 {
		t.Errorf("purge left guest index entries: %v", purged.SortedGuestKeys())
	}
	if d := purged.FenceFor(testGuest()); d.Blocked {
		t.Error("a fully purged state still fences a guest")
	}
}

// TestZeroValuedStateIsUsable: an FSM that has never run a migration
// decodes a snapshot with no migrations map at all, and must not panic
// on the first write.
func TestZeroValuedStateIsUsable(t *testing.T) {
	var st MigrationState
	if d := st.FenceFor(testGuest()); d.Blocked {
		t.Error("a zero-valued state fenced a guest")
	}
	if len(st.All()) != 0 {
		t.Error("a zero-valued state reported records")
	}
	if errs := st.DecodeErrors(); len(errs) != 0 {
		t.Errorf("DecodeErrors on an empty state returned %v", errs)
	}
	if d := st.Describe(); d != "0 migration records, 0 in flight, 0 fencing a guest" {
		t.Errorf("Describe on an empty state = %q", d)
	}
	if keys := st.SortedGuestKeys(); len(keys) != 0 {
		t.Errorf("SortedGuestKeys on an empty state = %v", keys)
	}
	next, err := Apply(st, startCmd())
	requireNoError(t, err, "start against a zero-valued state")
	if _, ok := next.Lookup("mig-cmd"); !ok {
		t.Error("the record was not stored")
	}
}

// TestAllAndLiveAreDeterministic: Go randomizes map iteration, so a
// listing that reorders itself between calls is indistinguishable from
// one where records are appearing and disappearing.
func TestAllAndLiveAreDeterministic(t *testing.T) {
	st := *NewMigrationState()
	for i, id := range []string{"mig-c", "mig-a", "mig-d", "mig-b"} {
		c := startCmd()
		c.ID = id
		c.Guest = Workload{Kind: WorkloadKindJail, ID: id}
		c.AtUnix = baseUnix + int64(i)
		next, err := Apply(st, c)
		requireNoError(t, err, "start "+id)
		st = next
	}

	first := st.Describe()
	all := st.All()
	if len(all) != 4 {
		t.Fatalf("All returned %d records, want 4", len(all))
	}
	for i := 1; i < 20; i++ {
		again := st.All()
		if len(again) != len(all) {
			t.Fatalf("All returned %d records on a later call, want %d", len(again), len(all))
		}
		for j := range all {
			if again[j].ID != all[j].ID {
				t.Fatalf("All reordered between calls at index %d: %s vs %s", j, again[j].ID, all[j].ID)
			}
		}
	}
	if got := st.Describe(); got != first {
		t.Errorf("Describe is not deterministic: %q then %q", first, got)
	}
	// Creation order, not insertion order: mig-c was created first.
	if all[0].ID != "mig-c" {
		t.Errorf("All did not sort by creation time: first is %s, want mig-c", all[0].ID)
	}
	if len(st.Live()) != 4 {
		t.Errorf("Live returned %d, want 4 in-flight records", len(st.Live()))
	}
}

// TestSortRecordsIsStableForEqualTimestamps: two records created in the
// same second must still have a defined order.
func TestSortRecordsIsStableForEqualTimestamps(t *testing.T) {
	recs := []Record{
		{ID: "mig-z", Timestamps: Timestamps{CreatedUnix: baseUnix}},
		{ID: "mig-a", Timestamps: Timestamps{CreatedUnix: baseUnix}},
		{ID: "mig-m", Timestamps: Timestamps{CreatedUnix: baseUnix + 1}},
	}
	SortRecords(recs)
	if recs[0].ID != "mig-a" || recs[1].ID != "mig-z" || recs[2].ID != "mig-m" {
		t.Errorf("SortRecords produced %s, %s, %s; want mig-a, mig-z, mig-m", recs[0].ID, recs[1].ID, recs[2].ID)
	}
}

// TestRecordValidateRejectsAHopelessRecord: the record's own integrity
// check, over the identity fields a hand-built or replayed value gets
// wrong.
func TestRecordValidateRejectsAHopelessRecord(t *testing.T) {
	good := newRecordAt("mig-valid", baseUnix)
	requireNoError(t, good.Validate(), "valid record")

	cases := map[string]func(r *Record){
		"no id":                func(r *Record) { r.ID = "" },
		"blank id":             func(r *Record) { r.ID = "   " },
		"no guest id":          func(r *Record) { r.Guest.ID = "" },
		"blank guest id":       func(r *Record) { r.Guest.ID = "  " },
		"bad guest kind":       func(r *Record) { r.Guest.Kind = "pod" },
		"no source":            func(r *Record) { r.SourceID = "" },
		"no target":            func(r *Record) { r.TargetID = "" },
		"self migration":       func(r *Record) { r.TargetID = r.SourceID },
		"bad phase":            func(r *Record) { r.Phase = "transferring" },
		"no outcome":           func(r *Record) { r.Outcome = "" },
		"bad outcome":          func(r *Record) { r.Outcome = "done" },
		"zero attempt":         func(r *Record) { r.Attempt = 0 },
		"zero token":           func(r *Record) { r.Token = 0 },
		"stall with no reason": func(r *Record) { r.Stall = &Stalled{Detail: "something"} },
		"no creation time":     func(r *Record) { r.Timestamps.CreatedUnix = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := good.Clone()
			mutate(&r)
			if err := r.Validate(); err == nil {
				t.Errorf("a record with %s passed Validate", name)
			}
		})
	}
}

// TestWorkloadKindIsClosed: ADR-0128 names VM and jail. Coercing an
// unknown kind to a VM would journal a migration of something this code
// has never heard of.
func TestWorkloadKindIsClosed(t *testing.T) {
	if WorkloadKindVM.Valid() != true || WorkloadKindJail.Valid() != true {
		t.Error("a real workload kind was reported invalid")
	}
	for _, k := range []WorkloadKind{"", "VM", "pod", "lxc", "vm "} {
		if k.Valid() {
			t.Errorf("workload kind %q was reported valid", k)
		}
	}
	if (Workload{}).valid() {
		t.Error("an empty workload was reported valid")
	}
	if (Workload{Kind: WorkloadKindVM}).valid() {
		t.Error("a workload with a kind but no id was reported valid")
	}
}

// TestPhaseIsClosed: an unrecognized phase reports -1 from Index rather
// than 0, so a caller cannot treat "not a phase" as "the first phase".
func TestPhaseIsClosed(t *testing.T) {
	if Phase("nonsense").Valid() {
		t.Error("an unrecognized phase was reported valid")
	}
	if got := Phase("nonsense").Index(); got != -1 {
		t.Errorf("unrecognized phase index = %d, want -1", got)
	}
	if Phase("").Index() != -1 {
		t.Error("the empty phase reported an index")
	}
	for i, p := range PhaseOrder {
		if !p.Valid() {
			t.Errorf("phase %s in PhaseOrder is not valid", p)
		}
		if p.Index() != i {
			t.Errorf("phase %s reports index %d, want %d", p, i, i)
		}
	}
	if !PhaseTeardown.IsLast() {
		t.Error("teardown does not report itself as the last phase")
	}
	for _, p := range PhaseOrder[:len(PhaseOrder)-1] {
		if p.IsLast() {
			t.Errorf("phase %s reports itself as last", p)
		}
	}
}

// TestTimestampsSinceReportsNeverEnteredHonestly: a phase that never
// happened must not read as a zero-length elapsed time, which is a
// fabricated measurement rather than an absent one.
func TestTimestampsSinceReportsNeverEnteredHonestly(t *testing.T) {
	r := newRecordAt("mig-never", baseUnix)
	if _, ok := r.Timestamps.Since(PhaseBulk, baseTime.Add(time.Hour)); ok {
		t.Error("a phase that was never entered reported an elapsed time")
	}
	// Preflight WAS entered at baseUnix, and baseTime is that same
	// instant, so its elapsed time is a real zero rather than an
	// absent measurement.
	d, ok := r.Timestamps.Since(PhasePreflight, baseTime)
	if !ok || d != 0 {
		t.Errorf("preflight since = %v (ok=%v), want 0 (it was entered at the record's own creation time)", d, ok)
	}

	// A zero first-entered value is treated as "never" too, not as
	// 1970.
	var ts Timestamps
	ts.normalize()
	ts.FirstEnteredUnix[PhaseBulk] = 0
	if _, ok := ts.Since(PhaseBulk, baseTime); ok {
		t.Error("a zero timestamp was reported as a real entry time")
	}
}

// TestEnteredKeepsTheEarliestTimestamp: a phase that is frozen,
// backed off, and frozen again must not look less-frozen than it is.
func TestEnteredKeepsTheEarliestTimestamp(t *testing.T) {
	var ts Timestamps
	ts.Entered(PhaseFreeze, baseUnix+100)
	ts.Entered(PhaseFreeze, baseUnix+50)
	ts.Entered(PhaseFreeze, baseUnix+300)
	if got := ts.FirstEnteredUnix[PhaseFreeze]; got != baseUnix+50 {
		t.Errorf("first-entered = %d, want the earliest %d", got, baseUnix+50)
	}
	if got := ts.LastEnteredUnix[PhaseFreeze]; got != baseUnix+300 {
		t.Errorf("last-entered = %d, want the latest %d", got, baseUnix+300)
	}
	if ts.UpdatedUnix != baseUnix+300 {
		t.Errorf("updated = %d, want %d", ts.UpdatedUnix, baseUnix+300)
	}
}
