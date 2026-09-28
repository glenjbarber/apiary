package forcerestart_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/forcerestart"
	"github.com/glenjbarber/apiary/internal/restartplan"
)

// The pending-restart record is the only trace a force-restart leaves
// that anything else reads. The guardrail's 600-second cooldown - the
// window during which a second voter may not restart the same service -
// is fed by RestartRecord entries in the FSM, and the only thing that
// writes one is the confirm path. So a force-restart that left no
// record would leave the guardrail believing no restart had happened,
// and an operator could force-restart raftd on one Comb and be granted
// a coordinated raftd restart on another seconds later: exactly the
// window the cooldown exists to close.
//
// These cases pin the record itself, and pin the two cases where the
// code deliberately writes nothing at all - which are the two where
// writing something would be worse.

func managerdRecord() forcerestart.Service {
	return forcerestart.Service{Name: "managerd", Port: 17700}
}

// recordFixture builds a temp directory and the RecordPaths pointing
// into it, so nothing here needs root or /var/db.
type recordFixture struct {
	paths    forcerestart.RecordPaths
	warnings *strings.Builder
}

func newRecordFixture(t *testing.T) *recordFixture {
	t.Helper()
	dir := t.TempDir()
	return &recordFixture{
		paths: forcerestart.RecordPaths{
			Dir:          filepath.Join(dir, "guardrail"),
			RaftdJSON:    filepath.Join(dir, "raftd.json"),
			ManagerdJSON: filepath.Join(dir, "managerd.json"),
			CommonJSON:   filepath.Join(dir, "common.json"),
			Hostname:     "hostname-fallback",
		},
		warnings: &strings.Builder{},
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// writeRecord runs the real force-restart loop over a one-service plan,
// which is the only way to reach the record writer from outside the
// package. It is deliberately not a shortcut: a test that called the
// writer directly would prove the writer works and say nothing about
// the loop calling it before the restart it describes, which is the
// property that actually matters.
func (f *recordFixture) writeRecord(t *testing.T, service forcerestart.Service) {
	t.Helper()
	_, err := forcerestart.Run(forcerestart.Options{
		Plan:   []forcerestart.Service{service},
		Host:   newFakeHost(t, modeAll),
		Record: f.paths,
		Out:    f.warnings,
	})
	if err != nil {
		t.Fatalf("the one-service run failed, which these cases are not about: %v\n%s", err, f.warnings.String())
	}
}

func (f *recordFixture) load(t *testing.T, service string) (restartplan.PendingRestart, bool) {
	t.Helper()
	got, ok, err := restartplan.NewPendingStore(f.paths.Dir).Load(service)
	if err != nil {
		t.Fatalf("loading the record for %s: %v", service, err)
	}
	return got, ok
}

// TestForcedRecord_NamesTheVoterTheRestartedDaemonWouldReport pins the
// node-id resolution, and pins it by outcome rather than by asserting
// which files were opened.
//
// Two rules are at stake. The first is correctness: the record is read
// back by the restarted daemon's own startup confirm path, and the id
// in it is compared against raft membership, so it has to be the
// identity that daemon would report for itself. A managerd record
// carrying raftd's id on a Comb whose two configs disagree is a record
// that misnames the event it exists to record.
//
// The second is not writing nothing. The guardrail's cooldown is fed by
// this record, so a Comb left with no record is a Comb the guardrail
// believes has not restarted - the exact gap the record exists to close.
// A record naming a slightly wrong voter is inert; no record is a hole.
// Every fallback below exists for that reason.
func TestForcedRecord_NamesTheVoterTheRestartedDaemonWouldReport(t *testing.T) {
	for _, tc := range []struct {
		name     string
		service  forcerestart.Service
		raftd    string
		managerd string
		common   string
		want     string
	}{
		{
			name:     "raftd's restart is recorded under raftd's own id",
			service:  forcerestart.Service{Name: "raftd", Port: 17600},
			raftd:    `{"node_id":"from-raftd"}`,
			managerd: `{"node_id":"from-managerd"}`,
			common:   `{"node_id":"from-common"}`,
			want:     "from-raftd",
		},
		{
			name:     "managerd's restart is recorded under managerd's own id",
			service:  forcerestart.Service{Name: "managerd", Port: 17700},
			raftd:    `{"node_id":"from-raftd"}`,
			managerd: `{"node_id":"from-managerd"}`,
			common:   `{"node_id":"from-common"}`,
			want:     "from-managerd",
		},
		{
			// The case that distinguishes this from the shell it
			// replaces. There, raftd.json naming no id fell straight
			// through to managerd.json and skipped the shared file
			// entirely - so a record could name managerd's identity for
			// a raftd restart. Here raftd's own loader resolves it the
			// way raftd does, which is through ADR-0111's common.json.
			name:     "raftd with no id of its own resolves through the shared file, not past it",
			service:  forcerestart.Service{Name: "raftd", Port: 17600},
			raftd:    `{"data_dir":"/var/db/apiary/raftd"}`,
			managerd: `{"node_id":"from-managerd"}`,
			common:   `{"node_id":"from-common"}`,
			want:     "from-common",
		},
		{
			// The other daemon's own config is still consulted, so a
			// Comb whose raftd.json is damaged is not left with no
			// record at all.
			name:     "a damaged own-config falls through to the other daemon's",
			service:  forcerestart.Service{Name: "raftd", Port: 17600},
			raftd:    `{"node_id": "truncated`,
			managerd: `{"node_id":"from-managerd"}`,
			common:   `{"node_id":"from-common"}`,
			want:     "from-managerd",
		},
		{
			// A damaged common.json is a different and broader failure:
			// each loader consults it before reading the file it was
			// pointed at, so this poisons all of them and the record
			// lands on the hostname. Which is the right answer - a
			// Comb with a broken shared config file still gets a
			// record, and the operator gets one.
			name:     "a damaged shared file poisons every loader, so the hostname is reached",
			service:  forcerestart.Service{Name: "raftd", Port: 17600},
			raftd:    `{"node_id":"from-raftd"}`,
			managerd: `{"node_id":"from-managerd"}`,
			common:   `{"node_id": "also truncated`,
			want:     "hostname-fallback",
		},
		{
			name:     "a config that is not JSON at all falls through the same way",
			service:  forcerestart.Service{Name: "raftd", Port: 17600},
			raftd:    "node_id = from-raftd\n",
			managerd: `{"node_id":"from-managerd"}`,
			common:   `{"node_id":"from-common"}`,
			want:     "from-managerd",
		},
		{
			name:    "the shared file alone is enough",
			service: forcerestart.Service{Name: "managerd", Port: 17700},
			common:  `{"node_id":"from-common"}`,
			want:    "from-common",
		},
		{
			// A Comb set up before node_id was written into the config
			// looks exactly like this: no file sets it, and both daemons
			// fall back to os.Hostname. Declining to record here would
			// mean declining to do this command's one job on precisely
			// those hosts.
			name:    "the hostname is the last resort",
			service: forcerestart.Service{Name: "managerd", Port: 17700},
			want:    "hostname-fallback",
		},
		{
			// A shared file that sets a hostname but no node_id is not a
			// value. The key is absent, not empty.
			name:    "a shared file with a hostname but no node_id is not a value",
			service: forcerestart.Service{Name: "managerd", Port: 17700},
			common:  `{"hostname":"comb1"}`,
			want:    "hostname-fallback",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecordFixture(t)
			if tc.raftd != "" {
				writeFile(t, f.paths.RaftdJSON, tc.raftd)
			}
			if tc.managerd != "" {
				writeFile(t, f.paths.ManagerdJSON, tc.managerd)
			}
			if tc.common != "" {
				writeFile(t, f.paths.CommonJSON, tc.common)
			}
			f.writeRecord(t, tc.service)

			got, ok := f.load(t, tc.service.RCName())
			if !ok {
				t.Fatalf("no record was written\nwarnings:\n%s", f.warnings.String())
			}
			if got.NodeID != tc.want {
				t.Errorf("node_id = %q, want %q\nwarnings:\n%s", got.NodeID, tc.want, f.warnings.String())
			}
			// lease_id 0 is the whole trick, and it is asserted on the
			// value read back rather than left to the struct's zero
			// value: a non-zero id would release a lease somebody else
			// is holding, or silently skip the cooldown.
			if got.LeaseID != 0 {
				t.Errorf("lease_id = %d, want 0: a forced restart informs the cooldown and must never release a real lease", got.LeaseID)
			}
			if got.Service != tc.service.RCName() {
				t.Errorf("service = %q, want %q", got.Service, tc.service.RCName())
			}
		})
	}
}

// TestForcedRecord_DoesNotOverwriteAPendingLease covers the case where
// writing the record would do real harm.
//
// An existing pending record is a real lease in flight. Save overwrites,
// and clobbering it with lease_id 0 would strand that lease permanently:
// the daemon would confirm lease 0, the real lease would never match,
// and leases have no TTL. The only correct action is to leave it alone
// and say so loudly enough that the operator acts on it.
func TestForcedRecord_DoesNotOverwriteAPendingLease(t *testing.T) {
	f := newRecordFixture(t)
	writeFile(t, f.paths.CommonJSON, `{"node_id":"comb-under-test"}`)

	// A real lease, held by this node, in flight right now.
	store := restartplan.NewPendingStore(f.paths.Dir)
	if err := store.Save(restartplan.PendingRestart{Service: "apiary_managerd", NodeID: "comb-under-test", LeaseID: 4242}); err != nil {
		t.Fatalf("seeding a pending record: %v", err)
	}

	f.writeRecord(t, managerdRecord())

	got, ok := f.load(t, "apiary_managerd")
	if !ok {
		t.Fatal("the existing record was deleted")
	}
	if got.LeaseID != 4242 {
		t.Fatalf("lease_id = %d, want 4242: the existing lease's record was clobbered", got.LeaseID)
	}
	// Silence here would be the dangerous outcome, so the warning is
	// asserted for the parts that tell an operator what to do next.
	w := f.warnings.String()
	for _, want := range []string{
		"a pending-restart record already exists for apiary_managerd",
		"Leaving it untouched",
		"no TTL",
		// Matched on a phrase inside one line rather than across the
		// break: "clear it by" ends a line and "hand" starts the next,
		// so the obvious assertion would fail on wrapping alone.
		"confirmed no lease is held",
		// The consequence, stated: the cooldown is now not going to
		// learn about this restart, which is the whole reason the
		// record exists.
		"cooldown will NOT learn",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("the warning is missing %q\n---\n%s---", want, w)
		}
	}
}

// TestForcedRecord_RefusesAnUnreadablePendingRecord: a record this code
// cannot parse is not a free slot. Overwriting it would destroy the
// evidence that something is wrong, and a corrupt pending file is
// exactly the state where an operator needs to see it rather than have
// it replaced by a tidy new one.
func TestForcedRecord_RefusesAnUnreadablePendingRecord(t *testing.T) {
	f := newRecordFixture(t)
	writeFile(t, f.paths.CommonJSON, `{"node_id":"comb-under-test"}`)
	if err := os.MkdirAll(f.paths.Dir, 0o700); err != nil {
		t.Fatalf("creating the record directory: %v", err)
	}
	corrupt := filepath.Join(f.paths.Dir, "pending-restart-apiary_managerd.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing the corrupt record: %v", err)
	}

	f.writeRecord(t, managerdRecord())

	body, err := os.ReadFile(corrupt)
	if err != nil {
		t.Fatalf("the corrupt record was removed: %v", err)
	}
	if string(body) != "{not json" {
		t.Errorf("the corrupt record was rewritten:\n%s", body)
	}
	if !strings.Contains(f.warnings.String(), "could not read the existing pending-restart record") {
		t.Errorf("an unreadable record was not reported\n---\n%s---", f.warnings.String())
	}
}

// TestForcedRecord_ReportPathIsNotNeededForTheRecord pins the property
// that makes this command usable at all: the record's on-disk shape is
// byte-for-byte the one managerd's own RestartConfirmStore writes, and
// the one raftd reads back on its next startup. internal/restartplan
// holds a test asserting that equivalence between its own type and
// internal/manager's; this one asserts the third writer agrees, on the
// bytes, with both.
//
// A drift here is silent and severe: a record raftd cannot parse is a
// restart that can never confirm, and a record whose service name does
// not match is a record nothing will ever read.
func TestForcedRecord_ReportPathIsNotNeededForTheRecord(t *testing.T) {
	f := newRecordFixture(t)
	writeFile(t, f.paths.CommonJSON, `{"node_id":"comb-under-test"}`)
	f.writeRecord(t, managerdRecord())

	raw, err := os.ReadFile(filepath.Join(f.paths.Dir, "pending-restart-apiary_managerd.json"))
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	want := `{"service":"apiary_managerd","node_id":"comb-under-test","lease_id":0}`
	if strings.TrimSpace(string(raw)) != want {
		t.Errorf("record bytes:\n got %s\nwant %s", strings.TrimSpace(string(raw)), want)
	}

	info, err := os.Stat(filepath.Join(f.paths.Dir, "pending-restart-apiary_managerd.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("record mode = %v, want 0600", info.Mode().Perm())
	}
	if dirInfo, err := os.Stat(f.paths.Dir); err == nil && dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("record directory mode = %v, want 0700", dirInfo.Mode().Perm())
	}
}
