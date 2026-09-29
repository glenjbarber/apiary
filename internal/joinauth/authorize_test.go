package joinauth

// The `apiaryctl join-authorize` decision, tested without root, without
// a Comb, and without a checkout - which is also the ADR's own test
// list item 9 ("apiaryctl join-authorize with no checkout present, on a
// fixture directory, asserting it writes atomically and prints the
// matched request before writing").

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pendingFixtures() []PendingRequest {
	return []PendingRequest{
		{
			RequestID: "jreq-other", NodeID: "comb-9",
			Fingerprint: "SHA256:99:99", RaftBindAddress: "comb-9.example:17600",
			RequestedAtUnix: 1699999000, ExpiresAtUnix: 1700009000, Status: "JOIN_REQUEST_STATUS_PENDING",
		},
		{
			RequestID: "jreq-target", NodeID: "comb-3",
			Fingerprint: testFingerprint, RaftBindAddress: "comb-3.example:17600",
			RequestedAtUnix: 1700000000, ExpiresAtUnix: 1700009000, Status: "JOIN_REQUEST_STATUS_PENDING",
		},
	}
}

func fixedNow() func() time.Time {
	at := time.Unix(1700000000, 0)
	return func() time.Time { return at }
}

func TestAuthorizePrintsTheMatchedRequestBeforeItWrites(t *testing.T) {
	// The ADR's requirement, and the reason the order is the order: the
	// deliberate comparison between the joining Comb's own screen and
	// the pending request has to happen in front of the operator. A
	// command that wrote first and printed afterwards would have made
	// the comparison a formality, because the entry would exist whether
	// or not anyone read the line.
	dir := t.TempDir()
	path := filepath.Join(dir, "join-authorizations.json")
	var out bytes.Buffer

	res, err := Authorize(AuthorizeOptions{
		Path: path, NodeID: "comb-3", Fingerprint: testFingerprint,
		Now: fixedNow(), Out: &out,
	}, pendingFixtures())
	if err != nil {
		t.Fatalf("Authorize() error: %v", err)
	}
	if !res.Created || res.Entry.ID == "" {
		t.Fatalf("Authorize() = %+v, want a created entry", res)
	}
	if res.Matched.RequestID != "jreq-target" {
		t.Errorf("Authorize() matched %q, want jreq-target", res.Matched.RequestID)
	}
	// Every identifying field of the matched request has to be on the
	// printed block. A comparison against one field of six is not a
	// comparison, and the fields most worth comparing are the ones a
	// short output would drop.
	for _, want := range []string{"jreq-target", "comb-3", "comb-3.example:17600", testFingerprint, "JOIN_REQUEST_STATUS_PENDING"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("printed report does not contain %q, so the operator cannot compare that field:\n%s", want, out.String())
		}
	}
	atRequest := strings.Index(out.String(), "request_id:       jreq-target")
	atWrite := strings.Index(out.String(), "Wrote entry")
	if atRequest < 0 || atWrite < 0 {
		t.Fatalf("the report is missing one half of the order assertion (request at %d, write at %d):\n%s", atRequest, atWrite, out.String())
	}
	if atRequest > atWrite {
		t.Error("the write was reported before the request it authorizes was printed; the report order IS the comparison, and it has to come first")
	}
}

func TestAuthorizeWrites0600AndIsReloadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "join-authorizations.json")
	if _, err := Authorize(AuthorizeOptions{
		Path: path, NodeID: "comb-3", Fingerprint: testFingerprint,
		Now: fixedNow(), Out: &bytes.Buffer{},
	}, pendingFixtures()); err != nil {
		t.Fatalf("Authorize() error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("Authorize() wrote mode %04o, want 0600", got)
	}
	store, err := Load(path)
	if err != nil {
		t.Fatalf("Load() after Authorize(): %v", err)
	}
	if len(store.Authorizations) != 1 {
		t.Fatalf("store holds %d entries, want 1: %+v", len(store.Authorizations), store.Authorizations)
	}
	e := store.Authorizations[0]
	if e.NodeID != "comb-3" || NormalizeFingerprint(e.Fingerprint) != NormalizeFingerprint(testFingerprint) {
		t.Errorf("entry = %+v, want node comb-3 and fingerprint %s", e, testFingerprint)
	}
	if got, want := e.ExpiresAtUnix, fixedNow()().Add(DefaultTTL).Unix(); got != want {
		t.Errorf("entry expires at %d, want %d (now + the 24h default)", got, want)
	}
}

func TestAuthorizeRespectsAnExplicitTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "join-authorizations.json")
	if _, err := Authorize(AuthorizeOptions{
		Path: path, NodeID: "comb-3", Fingerprint: testFingerprint,
		TTL: 90 * time.Minute, Now: fixedNow(), Out: &bytes.Buffer{},
	}, pendingFixtures()); err != nil {
		t.Fatalf("Authorize() error: %v", err)
	}
	store, err := Load(path)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got, want := store.Authorizations[0].ExpiresAtUnix, fixedNow()().Add(90*time.Minute).Unix(); got != want {
		t.Errorf("entry expires at %d, want %d", got, want)
	}
}

func TestAuthorizeWritesNothingWhenNoRequestMatches(t *testing.T) {
	// Every case here is a refusal that must leave the file alone, and
	// the file being absent afterwards is the assertion - a command that
	// created an empty store on a failed match would be creating the
	// one thing that looks like a working install.
	for _, tc := range []struct {
		name  string
		node  string
		fp    string
		reqID string
		want  string
	}{
		{"unknown comb", "comb-77", testFingerprint, "", "no pending join request"},
		{"right comb wrong certificate", "comb-3", "SHA256:00:00", "", "no pending join request"},
		{"narrowed to a request that is not there", "comb-3", testFingerprint, "jreq-nope", "no pending join request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "join-authorizations.json")
			_, err := Authorize(AuthorizeOptions{
				Path: path, NodeID: tc.node, Fingerprint: tc.fp, RequestID: tc.reqID,
				Now: fixedNow(), Out: &bytes.Buffer{},
			}, pendingFixtures())
			if err == nil {
				t.Fatal("Authorize() = nil error, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not contain %q", err, tc.want)
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Errorf("Authorize() created %s on a refusal; a refusal must change nothing at all", path)
			}
		})
	}
}

func TestAuthorizeRefusesWhenTwoRequestsMatch(t *testing.T) {
	// Two outstanding requests for the same identity is a situation an
	// operator should look at, not one this command resolves by picking
	// the newest: "the newest" is a guess about which Comb is really
	// there, and the whole point of the comparison is that a human
	// makes it.
	path := filepath.Join(t.TempDir(), "join-authorizations.json")
	pending := append(pendingFixtures(), PendingRequest{
		RequestID: "jreq-second", NodeID: "comb-3", Fingerprint: testFingerprint,
		RaftBindAddress: "comb-3.example:17600", RequestedAtUnix: 1700000100,
		ExpiresAtUnix: 1700009100, Status: "JOIN_REQUEST_STATUS_PENDING",
	})
	_, err := Authorize(AuthorizeOptions{
		Path: path, NodeID: "comb-3", Fingerprint: testFingerprint,
		Now: fixedNow(), Out: &bytes.Buffer{},
	}, pending)
	if err == nil {
		t.Fatal("Authorize() with two matching requests = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "--request-id") {
		t.Errorf("refusal %q does not name the flag that resolves the ambiguity", err)
	}
	for _, id := range []string{"jreq-target", "jreq-second"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("refusal %q does not name %s, so an operator cannot tell which two they are choosing between", err, id)
		}
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("Authorize() created the file while refusing to choose between two requests")
	}
}

func TestAuthorizeNarrowedToOneRequestSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "join-authorizations.json")
	pending := append(pendingFixtures(), PendingRequest{
		RequestID: "jreq-second", NodeID: "comb-3", Fingerprint: testFingerprint,
		RaftBindAddress: "comb-3.example:17600", RequestedAtUnix: 1700000100,
		ExpiresAtUnix: 1700009100, Status: "JOIN_REQUEST_STATUS_PENDING",
	})
	res, err := Authorize(AuthorizeOptions{
		Path: path, NodeID: "comb-3", Fingerprint: testFingerprint, RequestID: "jreq-second",
		Now: fixedNow(), Out: &bytes.Buffer{},
	}, pending)
	if err != nil {
		t.Fatalf("Authorize() error: %v", err)
	}
	if res.Matched.RequestID != "jreq-second" {
		t.Errorf("Authorize() matched %q, want the narrowed jreq-second", res.Matched.RequestID)
	}
}

func TestAuthorizeRefusesADuplicateLiveEntry(t *testing.T) {
	// Two identical entries would BOTH be matchable, which looks
	// harmless and is not: the second would be spendable by a second
	// request for the same identity, which is the single-use property
	// the entry exists to provide.
	dir := t.TempDir()
	path := filepath.Join(dir, "join-authorizations.json")
	opts := AuthorizeOptions{Path: path, NodeID: "comb-3", Fingerprint: testFingerprint, Now: fixedNow(), Out: &bytes.Buffer{}}
	if _, err := Authorize(opts, pendingFixtures()); err != nil {
		t.Fatalf("first Authorize() error: %v", err)
	}
	_, err := Authorize(opts, pendingFixtures())
	if err == nil {
		t.Fatal("second Authorize() for the same node_id and fingerprint = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "already holds entry") {
		t.Errorf("refusal %q does not say the entry already exists", err)
	}
	store, loadErr := Load(path)
	if loadErr != nil {
		t.Fatalf("Load(): %v", loadErr)
	}
	if len(store.Authorizations) != 1 {
		t.Errorf("store holds %d entries after a refused duplicate, want 1", len(store.Authorizations))
	}
}

func TestAuthorizeRefusesAMalformedStoreAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "join-authorizations.json")
	writeStore(t, path, `{"authorizations": [`)
	_, err := Authorize(AuthorizeOptions{
		Path: path, NodeID: "comb-3", Fingerprint: testFingerprint,
		Now: fixedNow(), Out: &bytes.Buffer{},
	}, pendingFixtures())
	if err == nil {
		t.Fatal("Authorize() over a malformed store = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("refusal %q does not name the file it could not write", err)
	}
}

func TestAuthorizeRequiresBothIdentityValues(t *testing.T) {
	for _, tc := range []struct{ name, node, fp, want string }{
		{"no node id", "", testFingerprint, "--node-id is required"},
		{"no fingerprint", "comb-3", "", "--fingerprint is required"},
		{"whitespace node id", "   ", testFingerprint, "--node-id is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "join-authorizations.json")
			_, err := Authorize(AuthorizeOptions{Path: path, NodeID: tc.node, Fingerprint: tc.fp, Now: fixedNow()}, pendingFixtures())
			if err == nil {
				t.Fatal("Authorize() = nil error, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not contain %q", err, tc.want)
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Error("Authorize() created the file despite a missing required flag")
			}
		})
	}
}

func TestAuthorizeWarnsThatOnlyTheLeaderReadsTheStore(t *testing.T) {
	// An entry on a follower is an entry no approval will ever consult.
	// A command that wrote one without saying so would leave an
	// operator watching an approval that refuses for a reason the
	// command had just prevented them from noticing.
	var out bytes.Buffer
	if _, err := Authorize(AuthorizeOptions{
		Path:   filepath.Join(t.TempDir(), "join-authorizations.json"),
		NodeID: "comb-3", Fingerprint: testFingerprint, Now: fixedNow(), Out: &out,
	}, pendingFixtures()); err != nil {
		t.Fatalf("Authorize() error: %v", err)
	}
	if !strings.Contains(out.String(), "LEADER") {
		t.Errorf("report does not warn that only the leader reads the store:\n%s", out.String())
	}
}

func TestAuthorizeDescribesWhatIsPendingOnAMiss(t *testing.T) {
	// A refusal that says only "no match" is not diagnosable from the
	// refusal. The pending list is what turns "it did not work" into
	// "the request was made against a different member, or has already
	// been resolved".
	path := filepath.Join(t.TempDir(), "join-authorizations.json")
	_, err := Authorize(AuthorizeOptions{
		Path: path, NodeID: "comb-77", Fingerprint: testFingerprint,
		Now: fixedNow(), Out: &bytes.Buffer{},
	}, pendingFixtures())
	if err == nil {
		t.Fatal("Authorize() = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "jreq-target") {
		t.Errorf("refusal %q does not list what IS pending:\n%v", err, err)
	}
}

func TestAuthorizeSaysSoWhenNothingIsPendingAtAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "join-authorizations.json")
	_, err := Authorize(AuthorizeOptions{
		Path: path, NodeID: "comb-3", Fingerprint: testFingerprint, Now: fixedNow(),
	}, nil)
	if err == nil {
		t.Fatal("Authorize() with no pending requests = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "no pending join requests on this Comb at all") {
		t.Errorf("refusal %q does not distinguish an empty Colony from a miss", err)
	}
}
