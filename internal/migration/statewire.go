package migration

import (
	"fmt"
	"sort"
)

// WireRecord is the raft-state shape of a migration: the exact field
// set the control plane's state.proto must declare, mirrored here in
// plain Go so this package compiles, vets and tests today without any
// generated code.
//
// Why it exists: internalpb is owned centrally, because concurrent
// workers allocating field numbers against the same file is a
// predictable way to produce a schema that neither worker can build.
// Rather than block, this package defines the shape completely and
// leaves a single mechanical binding for whoever owns the proto. The
// binding is these two functions plus a ~20-line adapter; nothing else
// in this package touches a generated type, and nothing here depends
// on the generated names existing.
//
// Every field is safe to replicate. There is no guest memory, no open
// handle, no in-flight I/O, no key material, and no dataset contents
// here or anywhere below this type - the whole point of ADR-0128's
// design is that raft carries the intent to migrate and the provenance
// of the copy, never the running thing.
//
// Field numbering below is the numbering the proto must use, and it is
// stated here so the two cannot silently disagree:
//
//	GuestMigration (new top-level message)
//	  1 id                     string
//	  2 guest_kind             string   ("vm" | "jail")
//	  3 guest_id               string
//	  4 source_node_id         string
//	  5 target_node_id         string
//	  6 phase                  string
//	  7 outcome                string
//	  8 attempt                uint32
//	  9 attempt_token          uint64
//	 10 detail                 string
//	 11 created_unix           int64
//	 12 updated_unix           int64
//	 13 settled_unix           int64
//	 14 preflight_first_unix   int64
//	 15 preflight_last_unix    int64
//	 16 freeze_first_unix      int64
//	 17 freeze_last_unix       int64
//	 18 bulk_first_unix        int64
//	 19 bulk_last_unix         int64
//	 20 sync_first_unix        int64
//	 21 sync_last_unix         int64
//	 22 cutover_first_unix     int64
//	 23 cutover_last_unix      int64
//	 24 teardown_first_unix    int64
//	 25 teardown_last_unix     int64
//	 26 bytes_required         uint64
//	 27 bytes_moved            uint64
//	 28 send_from_token        uint64
//	 29 send_to_token          uint64
//	 30 target_already_replica bool
//	 31 verified_observed      bool
//	 32 verified_running       bool
//	 33 observed_guid          string
//	 34 expected_guid          string
//	 35 observed_ip            string
//	 36 expected_ip            string
//	 37 verified_at_unix       int64
//	 38 query_error            string
//	 39 observed_on_node_id    string
//	 40 stall_reason           string
//	 41 stall_detail           string
//	 42 stall_verdict          string
//	 43 stall_since_unix       int64
//	 44 evidence               repeated MigrationEvidence
//	 45 state_schema_version   uint32
//
// MigrationEvidence (new nested message)
//
//	1 rule             string
//	2 detail           string
//	3 observed_at_unix int64
//
// // The per-phase first/last timestamps are flat scalar pairs rather than
// a map field. proto3 maps of message values are legal, but a flat
// scalar per phase costs nothing, cannot be keyed by an attacker-chosen
// string, and keeps the FSM's apply path free of map iteration - whose
// order proto does not guarantee, and whose nondeterminism in a raft
// apply would be a real bug rather than a cosmetic one.
//
// WireSchemaVersion is bumped every time a field is added or its
// meaning changes. A record read at a version this build does not
// understand is REFUSED by FromWire rather than partially understood:
// silently dropping a field whose meaning we no longer know how to
// interpret is how a migration record ends up saying its guest never
// moved.
const WireSchemaVersion uint32 = 1

// INTEGRATION - the whole of what the control plane has to do.
//
// Once api/internalpb/state.proto declares GuestMigration and
// MigrationEvidence as numbered above, plus two maps on
// FSMSnapshotState (migrations and migrations_by_guest), the binding is
// mechanical and is the ONLY place in this codebase that needs to know
// a generated type exists:
//
//	// 1. A read: proto -> WireRecord, field for field.
//	st := migration.MigrationState{
//	    Migrations: make(map[string]migration.WireRecord, len(snap.GetMigrations())),
//	    GuestIndex: make(map[string]string, len(snap.GetMigrationsByGuest())),
//	}
//	for id, m := range snap.GetMigrations() {
//	    st.Migrations[id] = wireFromInternal(m) // one field copy per the table above
//	}
//	for guest, id := range snap.GetMigrationsByGuest() {
//	    st.GuestIndex[guest] = id
//	}
//
//	// 2. The write, through the one validated entry point.
//	next, err := migration.Apply(st, cmd)
//	if err != nil {
//	    return rejection(err) // errors.Is against migration's sentinels
//	}
//	snap.Migrations[cmd.ID] = wireToInternal(next.Migrations[cmd.ID])
//	snap.MigrationsByGuest = next.GuestIndex
//
// The field copy is one small function per direction, mechanical from
// the numbering above. Everything else - the transition rules, the
// quorum gate, the fence, the honesty of the outcome - is already here
// and is tested without a proto, without a raft node and without a
// FreeBSD host.
// WireEvidence is one cited reason as it crosses the raft boundary. It
// is a value type on the wire, not a message, precisely so the
// evidence list has a stable, order-preserving representation: evidence
// is a chronology and must read back in the order it was recorded.
type WireEvidence struct {
	Rule           string
	Detail         string
	ObservedAtUnix int64
}

// WireRecord is the raft-state shape of a migration record. It is
// deliberately a flat struct of scalars with no pointers and no
// optional-ness beyond the zero value, because proto3's wire format is
// exactly that, and a conversion with pointer gymnastics is a
// conversion with bugs.
type WireRecord struct {
	ID        string
	GuestKind WorkloadKind
	GuestID   string

	SourceNodeID string
	TargetNodeID string

	Phase   Phase
	Outcome Outcome

	Attempt      uint32
	AttemptToken uint64
	Detail       string
	StateVersion uint32

	CreatedUnix int64
	UpdatedUnix int64
	SettledUnix int64

	PreflightFirstUnix int64
	PreflightLastUnix  int64
	FreezeFirstUnix    int64
	FreezeLastUnix     int64
	BulkFirstUnix      int64
	BulkLastUnix       int64
	SyncFirstUnix      int64
	SyncLastUnix       int64
	CutoverFirstUnix   int64
	CutoverLastUnix    int64
	TeardownFirstUnix  int64
	TeardownLastUnix   int64

	BytesRequired        uint64
	BytesMoved           uint64
	SendFromToken        uint64
	SendToToken          uint64
	TargetAlreadyReplica bool

	VerifiedObserved bool
	VerifiedRunning  bool
	ObservedGUID     string
	ExpectedGUID     string
	ObservedIP       string
	ExpectedIP       string
	VerifiedAtUnix   int64
	QueryError       string
	ObservedOnNodeID string

	StallReason    string
	StallDetail    string
	StallVerdict   string
	StallSinceUnix int64

	Evidence []WireEvidence
}

// timestampPairs maps each phase to the two wire fields carrying its
// first and last entry, so ToWire and FromWire cannot drift apart over
// six phases of hand-written field copying. A missing pair in this
// table is a compile-time-visible bug only in the sense that the phase
// silently loses its timestamps, which is why it is a table rather
// than a loop over PhaseOrder with arithmetic on field names.
func timestampPairs() []struct {
	Phase Phase
	First func(w *WireRecord) *int64
	Last  func(w *WireRecord) *int64
} {
	return []struct {
		Phase Phase
		First func(w *WireRecord) *int64
		Last  func(w *WireRecord) *int64
	}{
		{PhasePreflight, func(w *WireRecord) *int64 { return &w.PreflightFirstUnix }, func(w *WireRecord) *int64 { return &w.PreflightLastUnix }},
		{PhaseFreeze, func(w *WireRecord) *int64 { return &w.FreezeFirstUnix }, func(w *WireRecord) *int64 { return &w.FreezeLastUnix }},
		{PhaseBulk, func(w *WireRecord) *int64 { return &w.BulkFirstUnix }, func(w *WireRecord) *int64 { return &w.BulkLastUnix }},
		{PhaseSync, func(w *WireRecord) *int64 { return &w.SyncFirstUnix }, func(w *WireRecord) *int64 { return &w.SyncLastUnix }},
		{PhaseCutover, func(w *WireRecord) *int64 { return &w.CutoverFirstUnix }, func(w *WireRecord) *int64 { return &w.CutoverLastUnix }},
		{PhaseTeardown, func(w *WireRecord) *int64 { return &w.TeardownFirstUnix }, func(w *WireRecord) *int64 { return &w.TeardownLastUnix }},
	}
}

// ToWire converts a record to its raft-state shape. It is total for any
// record that passes Validate, and it is the ONLY place in this package
// that knows what a migration looks like on the wire, so the proto
// binding has exactly one thing to keep in step.
func ToWire(r Record) (WireRecord, error) {
	if err := r.Validate(); err != nil {
		return WireRecord{}, err
	}
	w := WireRecord{
		ID:                   r.ID,
		GuestKind:            r.Guest.Kind,
		GuestID:              r.Guest.ID,
		SourceNodeID:         r.SourceID,
		TargetNodeID:         r.TargetID,
		Phase:                r.Phase,
		Outcome:              r.Outcome,
		Attempt:              r.Attempt,
		AttemptToken:         r.Token,
		Detail:               r.Detail,
		StateVersion:         WireSchemaVersion,
		CreatedUnix:          r.Timestamps.CreatedUnix,
		UpdatedUnix:          r.Timestamps.UpdatedUnix,
		SettledUnix:          r.Timestamps.SettledUnix,
		BytesRequired:        r.Transfer.RequiredBytes,
		BytesMoved:           r.Transfer.MovedBytes,
		SendFromToken:        r.Transfer.FromToken,
		SendToToken:          r.Transfer.ToToken,
		TargetAlreadyReplica: r.Transfer.TargetAlreadyReplica,
		VerifiedObserved:     r.Verification.Observed,
		VerifiedRunning:      r.Verification.GuestRunning,
		ObservedGUID:         r.Verification.ObservedGUID,
		ExpectedGUID:         r.Verification.ExpectedGUID,
		ObservedIP:           r.Verification.ObservedIP,
		ExpectedIP:           r.Verification.ExpectedIP,
		VerifiedAtUnix:       r.Verification.ObservedAt,
		QueryError:           r.Verification.QueryError,
		ObservedOnNodeID:     r.Verification.ObservedOn,
	}
	for _, p := range timestampPairs() {
		if v, ok := r.Timestamps.FirstEnteredUnix[p.Phase]; ok {
			*p.First(&w) = v
		}
		if v, ok := r.Timestamps.LastEnteredUnix[p.Phase]; ok {
			*p.Last(&w) = v
		}
	}
	if r.Stall != nil {
		w.StallReason = r.Stall.Reason
		w.StallDetail = r.Stall.Detail
		w.StallVerdict = r.Stall.Verdict
		w.StallSinceUnix = r.Stall.SinceUnix
	}
	if len(r.Evidence) > 0 {
		w.Evidence = make([]WireEvidence, 0, len(r.Evidence))
		for _, e := range r.Evidence {
			w.Evidence = append(w.Evidence, WireEvidence{Rule: e.Rule, Detail: e.Detail, ObservedAtUnix: e.ObservedAtUnix})
		}
	}
	return w, nil
}

// FromWire converts a raft-state record back into a Record, RE-VALIDATING
// it fully. This is the untrusted direction: the bytes came out of raft
// state, which on a hostile or merely buggy peer means they are whatever
// that peer put there.
//
// A record that fails validation is an error, never a best-effort
// partial read. A migration record that has been corrupted into
// "phase cutover, outcome observed_complete, no verification" is the
// single most dangerous object this feature could read, and a decoder
// that returned it anyway would have handed the reconciler a licence to
// start a guest in two places.
func FromWire(w WireRecord) (Record, error) {
	if w.StateVersion != 0 && w.StateVersion != WireSchemaVersion {
		return Record{}, fmt.Errorf("migration: record %s carries state schema version %d, which this build does not understand (it knows %d) - refusing to partially interpret a record whose meaning may have changed",
			w.ID, w.StateVersion, WireSchemaVersion)
	}
	r := Record{
		ID:       w.ID,
		Guest:    Workload{Kind: w.GuestKind, ID: w.GuestID},
		SourceID: w.SourceNodeID,
		TargetID: w.TargetNodeID,
		Phase:    w.Phase,
		Outcome:  w.Outcome,
		Attempt:  w.Attempt,
		Token:    w.AttemptToken,
		Detail:   w.Detail,
		Timestamps: Timestamps{
			CreatedUnix: w.CreatedUnix,
			UpdatedUnix: w.UpdatedUnix,
			SettledUnix: w.SettledUnix,
		},
		Transfer: Transfer{
			RequiredBytes:        w.BytesRequired,
			MovedBytes:           w.BytesMoved,
			FromToken:            w.SendFromToken,
			ToToken:              w.SendToToken,
			TargetAlreadyReplica: w.TargetAlreadyReplica,
		},
		Verification: Verification{
			Observed:     w.VerifiedObserved,
			GuestRunning: w.VerifiedRunning,
			ObservedGUID: w.ObservedGUID,
			ExpectedGUID: w.ExpectedGUID,
			ObservedIP:   w.ObservedIP,
			ExpectedIP:   w.ExpectedIP,
			ObservedAt:   w.VerifiedAtUnix,
			QueryError:   w.QueryError,
			ObservedOn:   w.ObservedOnNodeID,
		},
	}
	r.Timestamps.normalize()
	for _, p := range timestampPairs() {
		if v := *p.First(&w); v != 0 {
			r.Timestamps.FirstEnteredUnix[p.Phase] = v
		}
		if v := *p.Last(&w); v != 0 {
			r.Timestamps.LastEnteredUnix[p.Phase] = v
		}
	}
	if w.StallReason != "" {
		r.Stall = &Stalled{
			Reason:    w.StallReason,
			Detail:    w.StallDetail,
			Verdict:   w.StallVerdict,
			SinceUnix: w.StallSinceUnix,
		}
	}
	for _, e := range w.Evidence {
		r.Evidence = append(r.Evidence, Evidence{Rule: e.Rule, Detail: e.Detail, ObservedAtUnix: e.ObservedAtUnix})
	}
	if err := r.Validate(); err != nil {
		return Record{}, err
	}
	return r, nil
}

// GuestIndexKey is the raft key holding "which migration, if any, owns
// this guest". The guest index is what makes "is this guest migrating?"
// a single point read on the ordinary reconciler's every tick rather
// than a scan of every migration record - and it is the check the fence
// in fence.go depends on.
func GuestIndexKey(g Workload) string {
	return string(g.Kind) + "/" + g.ID
}

// MigrationState is the migration slice of raft state: one map of
// records by id, and one index map from guest to record id.
//
// It is a plain Go struct rather than a generated proto type for the
// same reason this package exists: the control plane owns the proto,
// and a two-field struct is the whole of what has to be adapted. The
// binding is a handful of lines that copy between this struct and the
// two maps in the generated FSMSnapshotState, and it belongs to
// whoever owns the proto.
type MigrationState struct {
	// Migrations is the record set, keyed by record id.
	Migrations map[string]WireRecord
	// GuestIndex maps GuestIndexKey(guest) to the id of the migration
	// that currently HOLDS that guest, which is what the ordinary
	// reconciler consults on every tick.
	//
	// The entry is held until the record stops fencing, not merely
	// until it settles. A record whose completion was never observed
	// has settled but still fences (see FenceHeld), and dropping its
	// index entry at settle time would silently unfence the guest -
	// which is the exact failure the fence exists to prevent. Only a
	// record that has resolved to a known outcome, or been aborted,
	// releases the index. The record itself is kept for history
	// either way; releasing the guest is not the same as forgetting
	// the migration.
	GuestIndex map[string]string
}

// NewMigrationState returns an empty, ready-to-use state. FSM code
// should use this rather than a bare struct literal so the maps are
// never nil.
func NewMigrationState() *MigrationState {
	return &MigrationState{
		Migrations: map[string]WireRecord{},
		GuestIndex: map[string]string{},
	}
}

// normalize makes the maps usable on a zero-valued MigrationState, so a
// caller that decoded a snapshot with no migrations map at all - a
// cluster that has never run one - does not panic on first write.
func (s *MigrationState) normalize() {
	if s.Migrations == nil {
		s.Migrations = map[string]WireRecord{}
	}
	if s.GuestIndex == nil {
		s.GuestIndex = map[string]string{}
	}
}

// Lookup returns a record by id, already decoded and validated. A
// record that fails to decode is treated as ABSENT rather than returned
// as a zero value - and DecodeErrors names it, because a record that
// silently disappeared from the fence is a guest that starts in two
// places, which is the one failure this whole package exists to
// prevent.
//
// It is a value receiver on purpose: reading state must not mutate it.
// An earlier shape cached the last decode error on the state, which
// made a read into a write and made "did anything fail to decode?"
// depend on what had been read most recently.
func (s MigrationState) Lookup(id string) (Record, bool) {
	w, ok := s.Migrations[id]
	if !ok {
		return Record{}, false
	}
	r, err := FromWire(w)
	if err != nil {
		return Record{}, false
	}
	return r, true
}

// DecodeErrors names every record in raft state that this build cannot
// read, sorted by id so the list is stable. An operator needs to be able
// to find out that raft is holding a migration record this build cannot
// interpret; a decode failure on one node and not another is exactly
// the kind of divergence that is otherwise invisible.
func (s MigrationState) DecodeErrors() []error {
	ids := make([]string, 0, len(s.Migrations))
	for id := range s.Migrations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []error
	for _, id := range ids {
		if _, err := FromWire(s.Migrations[id]); err != nil {
			out = append(out, fmt.Errorf("raft state holds record %s but it does not decode: %w", id, err))
		}
	}
	return out
}

// FencingFor returns the record currently holding a guest - the one
// whose presence means the ordinary reconcilers must not touch it. That
// includes a record that has settled with an unknown outcome, because
// an unknown completion is precisely the case where the guest must not
// be resumed on a guess.
//
// It is named for what it answers rather than for the record's
// liveness: a caller that asked "is this still migrating?" and got
// false for a guest fenced by an unobserved migration would be badly
// misled.
func (s MigrationState) FencingFor(g Workload) (Record, bool) {
	id, ok := s.GuestIndex[GuestIndexKey(g)]
	if !ok {
		return Record{}, false
	}
	r, ok := s.Lookup(id)
	if !ok {
		return Record{}, false
	}
	return r, FenceHeld(r)
}

// All returns every decodable record, ordered by SortRecords. Records
// that fail to decode are skipped and named in LastDecodeError - an
// operator must be able to find out that raft is holding a migration
// record this build cannot read.
func (s MigrationState) All() []Record {
	out := make([]Record, 0, len(s.Migrations))
	for id := range s.Migrations {
		if r, ok := s.Lookup(id); ok {
			out = append(out, r)
		}
	}
	SortRecords(out)
	return out
}

// Live returns only the records that still fence something.
func (s MigrationState) Live() []Record {
	all := s.All()
	out := make([]Record, 0, len(all))
	for _, r := range all {
		if r.Outcome == OutcomeInProgress {
			out = append(out, r)
		}
	}
	return out
}

// FenceFor is the reconciler's one call: it answers "may I touch this
// guest", reading the index itself so a caller cannot accidentally
// consult a record belonging to a different guest.
func (s MigrationState) FenceFor(g Workload) FenceDecision {
	r, ok := s.FencingFor(g)
	if !ok {
		return EvaluateFence(g, nil)
	}
	return EvaluateFence(g, &r)
}

// withMigration returns a copy of s with rec stored, and the guest index
// updated to match the record's liveness. Taking a copy and returning
// it (rather than mutating in place) is what lets Apply hand back the
// original state on a rejection, and is why there is no partial
// application to clean up.
func (s MigrationState) withMigration(rec Record) MigrationState {
	out := MigrationState{
		Migrations: make(map[string]WireRecord, len(s.Migrations)+1),
		GuestIndex: make(map[string]string, len(s.GuestIndex)+1),
	}
	for k, v := range s.Migrations {
		out.Migrations[k] = v
	}
	for k, v := range s.GuestIndex {
		out.GuestIndex[k] = v
	}

	w, err := ToWire(rec)
	if err != nil {
		// Unreachable: every mutation path validates before it gets
		// here, and a record that cannot be written is not written.
		// Surfacing it as a nil map entry would be worse - a silently
		// missing record is a guest that is no longer fenced.
		panic(fmt.Sprintf("migration: internal error: validated record %s cannot be encoded: %v", rec.ID, err))
	}
	out.Migrations[rec.ID] = w

	key := GuestIndexKey(rec.Guest)
	if FenceHeld(rec) {
		// Still holding the guest, whether because the migration is
		// running or because its completion was never observed.
		out.GuestIndex[key] = rec.ID
	} else {
		// Resolved: the guest is free. The record stays in Migrations
		// for history, and is garbage-collected on the same schedule
		// as any other raft key with a lifecycle - releasing the guest
		// is not forgetting the migration.
		if cur, ok := out.GuestIndex[key]; !ok || cur == rec.ID {
			delete(out.GuestIndex, key)
		}
	}
	return out
}

// Purge removes a record and its index entry outright. It refuses to
// drop the index entry of a record that is still fencing, because a
// collection bug that unfenced a guest would be a silent one, and the
// ADR names a GC bug in this area as exactly that. It is the
// garbage-collection path ADR-0128 requires: a migration record with a
// lifecycle that is never collected is a silent-leak bug, and this is
// the one function in the package allowed to forget a migration.
func (s MigrationState) Purge(id string) (MigrationState, error) {
	if r, ok := s.Lookup(id); ok && FenceHeld(r) {
		return s, fmt.Errorf("migration: refusing to collect record %s while it is still holding %s (outcome %q) - collecting it would unfence the guest with no record left to say why",
			id, r.Guest, r.Outcome)
	}
	out := MigrationState{
		Migrations: make(map[string]WireRecord, len(s.Migrations)),
		GuestIndex: make(map[string]string, len(s.GuestIndex)),
	}
	for k, v := range s.Migrations {
		if k != id {
			out.Migrations[k] = v
		}
	}
	for k, v := range s.GuestIndex {
		if v != id {
			out.GuestIndex[k] = v
		}
	}
	return out, nil
}

// newRecord builds the initial record for a start command. It is at
// preflight with attempt 1 and the leader's freshly-minted token, and
// nowhere else: a record is never created at any other phase, which is
// enforced in ValidateCommand and re-asserted here by construction.
func newRecord(cmd Command) Record {
	ts := newTimestamps(cmd.AtUnix)
	ts.Entered(PhasePreflight, cmd.AtUnix)
	rec := Record{
		ID:       cmd.ID,
		Guest:    cmd.Guest,
		SourceID: cmd.SourceID,
		TargetID: cmd.TargetID,
		Phase:    PhasePreflight,
		Outcome:  OutcomeInProgress,
		Attempt:  1,
		Token:    cmd.Token,
		Detail:   cmd.Detail,
	}
	rec.Timestamps = ts
	return rec
}

// SortedGuestKeys is the deterministic order of the guest index's keys.
// It exists so a test, a CLI listing, or a diagnostics dump never
// reports the same set in a different order twice - Go's map iteration
// is randomized, and a record ordering that changes between calls is
// indistinguishable from a record that is appearing and disappearing.
func (s MigrationState) SortedGuestKeys() []string {
	keys := make([]string, 0, len(s.GuestIndex))
	for k := range s.GuestIndex {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Describe renders a one-line summary of the whole state, for a log
// line at startup or in a diagnostic. Deterministic, and it names the
// counts rather than dumping the contents - a log line full of record
// ids hides the one number an operator is looking for.
func (s *MigrationState) Describe() string {
	all := s.All()
	live := 0
	fenced := 0
	for _, r := range all {
		if r.Outcome == OutcomeInProgress {
			live++
		}
		if FenceHeld(r) {
			fenced++
		}
	}
	note := ""
	if errs := s.DecodeErrors(); len(errs) > 0 {
		note = fmt.Sprintf(" (with %d undecodable record(s), starting: %v)", len(errs), errs[0])
	}
	return fmt.Sprintf("%d migration records, %d in flight, %d fencing a guest%s",
		len(all), live, fenced, note)
}
