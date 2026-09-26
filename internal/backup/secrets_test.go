package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/origincert"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

// # Layer 1: the allow-list projections are structurally incapable of
// carrying a secret

// TestSecretDenyListMatchesRealStructs is the test that makes the deny
// list contingent rather than decorative. It reflects over the three real
// configuration structs and requires that every deny-listed Go field
// actually exists on the struct it claims.
//
// The failure this catches is the important one: if somebody upstream
// renames nodeconfig.Config.PeerAPIKey, the deny list would quietly stop
// matching anything and the byte-level test below would still pass,
// because the fixture would still be populating a field named PeerAPIKey
// that no longer holds a credential. A stale deny list has to be a
// failing test.
func TestSecretDenyListMatchesRealStructs(t *testing.T) {
	structs := map[string]any{
		"nodeconfig.Config":     nodeconfig.Config{},
		"raftdconfig.Config":    raftdconfig.Config{},
		"frontendconfig.Config": frontendconfig.Config{},
	}
	for _, f := range append(append([]SecretField{}, DenyList...), secretPathAsSecretFields()...) {
		holder, ok := structs[f.StructName]
		if !ok {
			t.Errorf("deny list entry %s names struct %q, which this test does not know how to reflect over - a new source struct must be added here too", f.Kind, f.StructName)
			continue
		}
		rt := reflect.TypeOf(holder)
		sf, ok := rt.FieldByName(f.FieldName)
		if !ok {
			t.Errorf("deny list entry %s names %s.%s, which does not exist. Either the field was renamed upstream (in which case the deny list is now decorative and this backup is no longer excluding anything) or it never existed.", f.Kind, f.StructName, f.FieldName)
			continue
		}
		if f.JSONName != "" {
			tag := strings.Split(sf.Tag.Get("json"), ",")[0]
			if tag != f.JSONName {
				t.Errorf("deny list entry %s says the json name is %q but %s.%s is tagged %q - the audit matches on both spellings, so a mismatch means one of them is unguarded", f.Kind, f.JSONName, f.StructName, f.FieldName, tag)
			}
		}
		// The value must be a string. Every entry in both tables is a
		// string-valued field, and a deny list entry pointed at a
		// non-string field would match a name but never a value, which is
		// the shape of an exclusion that does not exclude.
		if sf.Type.Kind() != reflect.String {
			t.Errorf("deny list entry %s names %s.%s, which is a %s, not a string - the audit's zero-check is written for strings and this entry would not work", f.Kind, f.StructName, f.FieldName, sf.Type)
		}
	}
}

// secretPathAsSecretFields adapts the path table to the same shape, so
// one loop can check both tables against the real structs. The Kind
// types differ, which is deliberate - a path and an inline secret are
// not the same thing and should not pretend to be.
func secretPathAsSecretFields() []SecretField {
	out := make([]SecretField, 0, len(SecretPaths))
	for _, p := range SecretPaths {
		if !strings.HasPrefix(string(p.Kind), "origincert.") {
			out = append(out, SecretField{
				Kind:       SecretKind(p.Kind),
				StructName: p.StructName,
				FieldName:  p.FieldName,
				JSONName:   p.JSONName,
			})
		}
	}
	return out
}

// TestNodeConfigDenyListIsComplete is the completeness half: every field
// of nodeconfig.Config must be classified as exactly one of
//
//   - deny-listed (never projected),
//   - a recorded path to secret material, or
//   - an ordinary field that IS projected.
//
// It fails when a field is added upstream and nobody decided where it
// belongs. That is the difference between "the exclusion is a property
// of the type system" and "the exclusion is a convention somebody has
// been remembering", and it is the reason this package's structural
// claim can be made at all.
func TestNodeConfigDenyListIsComplete(t *testing.T) {
	assertEveryFieldClassified(t, "nodeconfig.Config", nodeconfig.Config{},
		reflect.TypeOf(NodeConfigRecord{}),
		map[string]string{
			"PeerAPIKey": "denied",
			"RaftdToken": "denied",
		},
		map[string]struct{}{
			// Recorded as paths, on the ADR's "record the path, never the
			// content" rule. The completeness test requires them to BE on
			// the projection, so "we told the operator where the material
			// belongs" is a checked claim and not an intention.
			"TLSKey":                          {},
			"CloudflareTokenFile":             {},
			"CloudflareTunnelCredentialsFile": {},
			"OriginCATokenFile":               {},
		})
}

func TestRaftdConfigDenyListIsComplete(t *testing.T) {
	assertEveryFieldClassified(t, "raftdconfig.Config", raftdconfig.Config{},
		reflect.TypeOf(RaftdConfigRecord{}),
		map[string]string{
			"InternalToken": "denied",
		},
		map[string]struct{}{
			"RaftTLSKey": {},
		})
}

func TestFrontendConfigDenyListIsComplete(t *testing.T) {
	assertEveryFieldClassified(t, "frontendconfig.Config", frontendconfig.Config{},
		reflect.TypeOf(FrontendConfigRecord{}),
		map[string]string{
			"ManagerAPIKey": "denied",
		},
		map[string]struct{}{
			"TLSKey": {},
		})
}

func TestOriginCertInventoryDenyListIsComplete(t *testing.T) {
	// origincert.InventoryEntry has no inline secret - the key lives in
	// the file KeyPath names, beside the certificate WritePair wrote. The
	// classification still has to be explicit, so that adding a field to
	// InventoryEntry is a deliberate decision rather than an accident.
	assertEveryFieldClassified(t, "origincert.InventoryEntry", origincert.InventoryEntry{},
		reflect.TypeOf(OriginCertRecord{}),
		nil,
		map[string]struct{}{
			"KeyPath": {},
		})
}

// assertEveryFieldClassified checks, for one source struct and its
// projection, that the three classifications partition the source's
// fields with no field unaccounted for.
// denied names fields that must be ABSENT from the projection.
// recordedPaths names fields whose value is a path to secret material and
// which must therefore be PRESENT - the path is recorded, the content is
// not. Every other source field must be projected.
func assertEveryFieldClassified(t *testing.T, structName string, source any, projection reflect.Type, denied map[string]string, recordedPaths map[string]struct{}) {
	t.Helper()

	// What the projection carries, by source field name. The mapping is
	// by json tag where the tag matches, and the test is written so a
	// field present on BOTH sides is fine, a field only on the source is
	// the failure, and a field only on the projection is a bug in the
	// projection (it would be describing something the source does not
	// have).
	projected := map[string]struct{}{}
	rt := projection
	for i := 0; i < rt.NumField(); i++ {
		projected[rt.Field(i).Name] = struct{}{}
	}

	src := reflect.TypeOf(source)
	for i := 0; i < src.NumField(); i++ {
		f := src.Field(i)
		if f.PkgPath != "" {
			continue
		}
		_, isDenied := denied[f.Name]
		_, isPath := recordedPaths[f.Name]
		_, isProjected := projected[f.Name]
		switch {
		case isDenied && isProjected:
			t.Errorf("%s.%s is classified as a live credential but is ALSO a field on the projection type - a denied field the projection can hold is not denied", structName, f.Name)
		case isPath && !isProjected:
			t.Errorf("%s.%s is a path to secret material and must be recorded so a restore operator knows where it belongs, but it is not on the projection", structName, f.Name)
		case isDenied || isPath:
			if !fieldIsClassified(f.Name, jsonTagOf(f)) {
				t.Errorf("%s.%s is classified but is in neither DenyList nor SecretPaths - add it, so the byte guard and the completeness test both know about it", structName, f.Name)
			}
		case isProjected:
			// Correct: an ordinary field, projected.
		default:
			t.Errorf("%s.%s is neither deny-listed, a recorded secret path, nor a field on the projection. Every field must be deliberately classified: add it to the projection, or to DenyList/SecretPaths with a stated reason. A field nobody classified is a field nobody thought about, which is how a credential ends up in an archive.", structName, f.Name)
		}
	}

	// And the other direction: nothing on the projection that the source
	// does not have, which would mean the archive describes a setting
	// this node version does not have.
	for name := range projected {
		if _, ok := src.FieldByName(name); !ok {
			// NodeConfigRecord adds Peers, which is derived rather than
			// read from the config. It is the one documented addition.
			if structName == "nodeconfig.Config" && name == "Peers" {
				continue
			}
			t.Errorf("the projection has field %s, which %s does not have - a manifest would be describing a setting the source cannot produce", name, structName)
		}
	}
}

func jsonTagOf(f reflect.StructField) string {
	return strings.Split(f.Tag.Get("json"), ",")[0]
}

func fieldIsClassified(goName, jsonName string) bool {
	if _, ok := denyIndex[strings.ToLower(goName)]; ok {
		return true
	}
	if jsonName != "" {
		if _, ok := denyIndex[strings.ToLower(jsonName)]; ok {
			return true
		}
	}
	if _, ok := pathIndex[strings.ToLower(goName)]; ok {
		return true
	}
	if jsonName != "" {
		if _, ok := pathIndex[strings.ToLower(jsonName)]; ok {
			return true
		}
	}
	return false
}

// TestProjectedRecordsHaveNoSecretShapedField proves the structural half
// in the most direct way available: the destination types cannot hold a
// secret, because they have no field that could.
func TestProjectedRecordsHaveNoSecretShapedField(t *testing.T) {
	for _, v := range []any{
		NodeConfigRecord{}, RaftdConfigRecord{}, FrontendConfigRecord{}, OriginCertRecord{},
	} {
		if err := AuditSecretFree(v); err != nil {
			t.Errorf("%T: %v", v, err)
		}
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			name := rt.Field(i).Name
			if _, denied := denyIndex[strings.ToLower(name)]; denied {
				t.Errorf("%T has field %s, which is on the deny list", v, name)
			}
			if _, isPath := pathIndex[strings.ToLower(name)]; !isPath && looksLikeSecretField(name) {
				t.Errorf("%T has field %s, whose name is secret-shaped and which is not classified in SecretPaths - either classify it or rename it, so a reader of this file can tell a path from a credential", v, name)
			}
		}
	}
}

// # Layer 2: the reflection audit

func TestAuditSecretFreeRefusesTheRealConfigStructs(t *testing.T) {
	node, _ := fixtureNodeConfig(t)
	raftd, _ := fixtureRaftdConfig(t)
	front, _ := fixtureFrontendConfig(t)

	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"nodeconfig", node, "nodeconfig.peer_api_key"},
		{"raftdconfig", raftd, "raftdconfig.internal_token"},
		{"frontendconfig", front, "frontendconfig.manager_api_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := AuditSecretFree(tc.value)
			if err == nil {
				t.Fatalf("AuditSecretFree accepted a fully populated %s carrying a live credential", tc.name)
			}
			if !errIs(t, err, ErrSecretExposed) {
				t.Errorf("error is %v, want ErrSecretExposed", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name the credential kind %q: %v", tc.want, err)
			}
		})
	}
}

func TestAuditSecretFreeAcceptsTheProjections(t *testing.T) {
	node, _ := fixtureNodeConfig(t)
	raftd, _ := fixtureRaftdConfig(t)
	front, _ := fixtureFrontendConfig(t)
	certs := fixtureOriginCerts()

	if err := AuditSecretFree(BuildNodeConfigRecord(node, []string{"comb-b"})); err != nil {
		t.Errorf("NodeConfigRecord: %v", err)
	}
	if err := AuditSecretFree(BuildRaftdConfigRecord(raftd)); err != nil {
		t.Errorf("RaftdConfigRecord: %v", err)
	}
	if err := AuditSecretFree(BuildFrontendConfigRecord(front)); err != nil {
		t.Errorf("FrontendConfigRecord: %v", err)
	}
	if err := AuditSecretFree(BuildOriginCertRecords(certs)); err != nil {
		t.Errorf("OriginCertRecords: %v", err)
	}
}

func TestAuditSecretFreeRefusesAnUnclassifiedSecretShapedField(t *testing.T) {
	// The alarm that catches a field nobody has classified. It is not in
	// the deny list, so the message must be the "somebody must classify
	// this" one, not the "you copied a known credential" one.
	// EncryptionKey is the case the suffix rule exists for: a bare
	// "…Key" field. It cannot be caught by a substring rule for "key",
	// because five fields in this codebase legitimately end in Key while
	// holding a path - so the rule is a suffix rule paired with the
	// SecretPaths table.
	type future struct {
		NodeID          string
		EncryptionKey   string
		UpstreamWebhook string
	}
	err := AuditSecretFree(future{NodeID: "comb-a", EncryptionKey: "live", UpstreamWebhook: "https://example.test"})
	if err == nil {
		t.Fatal("a populated field named EncryptionKey was accepted")
	}
	if !errIs(t, err, ErrSecretExposed) {
		t.Errorf("error is %v, want ErrSecretExposed", err)
	}
	if !containsAll(err.Error(), "EncryptionKey", "must be deny-listed") {
		t.Errorf("the message does not say what has to happen next: %v", err)
	}
}

func TestAuditSecretFreeIgnoresEmptySecretShapedFields(t *testing.T) {
	// A config on a single-node install with no peer key has an empty
	// PeerAPIKey. Refusing that would make the audit unusable, and an
	// audit nobody can run is an audit nobody runs.
	type future struct{ SomeToken string }
	if err := AuditSecretFree(future{}); err != nil {
		t.Errorf("an empty token field was rejected: %v", err)
	}
	if err := AuditSecretFree(future{SomeToken: "x"}); err == nil {
		t.Error("a populated token field was accepted")
	}
}

func TestAuditSecretFreeFollowsPointersSlicesAndMaps(t *testing.T) {
	type inner struct{ Token string }
	type outer struct {
		Items []inner
		ByID  map[string]inner
		Ptr   *inner
	}
	// Each of these is a route a secret could take into a manifest that
	// the projections do not currently use, and each must be caught.
	for name, v := range map[string]any{
		"slice": outer{Items: []inner{{Token: "live"}}},
		"map":   outer{ByID: map[string]inner{"a": {Token: "live"}}},
		"ptr":   outer{Ptr: &inner{Token: "live"}},
	} {
		if err := AuditSecretFree(v); err == nil {
			t.Errorf("a secret reached through a %s was accepted", name)
		}
	}
}

func TestAuditSecretFreeIgnoresByteSlices(t *testing.T) {
	// Digests and marshalled payloads are []byte and are exactly what
	// should not be pattern-matched; walking them would be both slow and
	// wrong.
	type withPayload struct {
		Digest  []byte
		Payload []byte
	}
	if err := AuditSecretFree(withPayload{Digest: []byte("abcd"), Payload: []byte{0, 1, 2, 3}}); err != nil {
		t.Errorf("byte slices were audited as records: %v", err)
	}
}

func TestAuditSecretFreeTerminatesOnCycles(t *testing.T) {
	type node struct {
		Name string
		Next *node
	}
	a := &node{Name: "a"}
	a.Next = a
	if err := AuditSecretFree(a); err != nil {
		t.Errorf("a self-referential value was not handled: %v", err)
	}
}

func TestAuditSecretFreeRefusesAValueTooDeepToAudit(t *testing.T) {
	// A manifest record nested past the audit's depth is a record nobody
	// reviewed, and reporting that is more honest than reporting "clean"
	// after silently stopping.
	type deep struct {
		Next *deep
		Name string
	}
	var root deep
	cur := &root
	for i := 0; i < maxAuditDepth+5; i++ {
		cur.Next = &deep{}
		cur = cur.Next
	}
	err := AuditSecretFree(&root)
	if err == nil {
		t.Fatal("a value nested past the audit depth was reported as clean")
	}
	if !errIs(t, err, ErrSecretExposed) {
		t.Errorf("error is %v, want ErrSecretExposed", err)
	}
}

// # Layer 3: the live-value byte guard

func TestSanitizerWatchesEveryLiveSecret(t *testing.T) {
	node, _ := fixtureNodeConfig(t)
	raftd, _ := fixtureRaftdConfig(t)
	front, _ := fixtureFrontendConfig(t)

	s := NewSanitizer(&node, &raftd, &front)
	if !s.Armed() {
		t.Fatal("the byte guard is not armed for a node with all four credentials set")
	}
	if got := s.Watched(); got != 4 {
		t.Errorf("watching %d values, want 4 - one per deny-listed inline credential", got)
	}
	if len(s.Ignored) != 0 {
		t.Errorf("values were dropped from the watchlist: %v", s.Ignored)
	}
}

func TestSanitizerIsNotArmedWhenNoSecretIsSet(t *testing.T) {
	// "There is nothing to check" and "everything was checked and clean"
	// are different results, and Guard refuses to pretend otherwise.
	s := NewSanitizer(&nodeconfig.Config{}, &raftdconfig.Config{}, &frontendconfig.Config{})
	if s.Armed() {
		t.Fatal("the guard claims to be armed with nothing to watch")
	}
	err := s.Guard("some bytes", []byte("anything"))
	if err == nil {
		t.Fatal("an unarmed guard accepted a check, which would report a check that never happened")
	}
	if !containsAll(err.Error(), "not armed", "refusing to publish") {
		t.Errorf("the refusal does not say what it refused to do: %v", err)
	}
}

func TestSanitizerGuardCatchesAValueInTheBytes(t *testing.T) {
	node, _ := fixtureNodeConfig(t)
	raftd, _ := fixtureRaftdConfig(t)
	front, _ := fixtureFrontendConfig(t)
	s := NewSanitizer(&node, &raftd, &front)

	err := s.Guard("artifacts/node-config.json", []byte(`{"peer_api_key":"`+node.PeerAPIKey+`"}`))
	if err == nil {
		t.Fatal("the byte guard did not find a live credential in the bytes it was given")
	}
	if !errIs(t, err, ErrSecretExposed) {
		t.Errorf("error is %v, want ErrSecretExposed", err)
	}
	if !containsAll(err.Error(), "byte offset", "no part of the generation was published") {
		t.Errorf("the message does not say where and what was not done: %v", err)
	}
}

func TestSanitizerRefusesToRegisterAnUnusableValue(t *testing.T) {
	s := NewSanitizer(&nodeconfig.Config{}, &raftdconfig.Config{}, &frontendconfig.Config{})
	if err := s.AddSecret("origincert.key", ""); err == nil {
		t.Error("an empty secret was registered, so a caller could believe it had armed the guard when it had not")
	}
	if err := s.AddSecret("origincert.key", "short"); err == nil {
		t.Error("a too-short secret was registered; grepping for it would match unrelated bytes and fail backups at random")
	}
	if err := s.AddSecret("origincert.key", originCertKeyPEM); err != nil {
		t.Errorf("a real-looking PEM was refused: %v", err)
	}
	if !s.Armed() {
		t.Error("the guard is not armed after a successful registration")
	}
	if err := s.Guard("artifacts/certs.json", []byte("prefix "+originCertKeyPEM)); err == nil {
		t.Error("a registered key PEM was not found in the bytes")
	}
}

func TestSanitizerIgnoresShortValuesAndSaysSo(t *testing.T) {
	s := NewSanitizer(&nodeconfig.Config{PeerAPIKey: "abc"}, &raftdconfig.Config{}, &frontendconfig.Config{})
	if s.Armed() {
		t.Fatal("a three-character 'secret' armed the guard")
	}
	if len(s.Ignored) != 1 {
		t.Errorf("Ignored = %v, want the dropped value reported rather than silently forgotten", s.Ignored)
	}
}

// # The byte-level proof: no live secret appears anywhere in a generation

// TestNoLiveSecretAppearsAnywhereInAGeneration is the test the ADR calls
// "not optional and not skippable-by-env-var", and it is the one that
// actually answers the question rather than a proxy for it.
//
// It builds a generation from three fully-populated fixture configs
// carrying unmistakable credential values, a certificate key PEM, and an
// empty-secrets sanity fixture, then greps EVERY byte of EVERY file in
// the generation - the manifest and every artifact - for each of those
// live values.
//
// It also asserts the opposite direction, which is what stops the test
// passing vacuously: ordinary configuration values from the same fixtures
// MUST be present. A record that were simply empty would satisfy every
// "this secret is absent" assertion, and this is the assertion that says
// the record is real.
func TestNoLiveSecretAppearsAnywhereInAGeneration(t *testing.T) {
	node, nodeValues := fixtureNodeConfig(t)
	raftd, raftdValues := fixtureRaftdConfig(t)
	front, frontValues := fixtureFrontendConfig(t)
	certs := fixtureOriginCerts()

	dir := t.TempDir()
	store := NewStore(dir, WithClock(fixedClock(time.Unix(1780000000, 0), time.Second)))

	nodeRec, err := json.Marshal(BuildNodeConfigRecord(node, []string{"comb-b", "comb-c"}))
	if err != nil {
		t.Fatal(err)
	}
	raftdRec, err := json.Marshal(BuildRaftdConfigRecord(raftd))
	if err != nil {
		t.Fatal(err)
	}
	frontRec, err := json.Marshal(BuildFrontendConfigRecord(front))
	if err != nil {
		t.Fatal(err)
	}
	certRec, err := json.Marshal(BuildOriginCertRecords(certs))
	if err != nil {
		t.Fatal(err)
	}

	gen, err := store.Begin("nightly", "gen-000001")
	if err != nil {
		t.Fatal(err)
	}
	writes := []struct {
		id   string
		body []byte
	}{
		{"node-config", nodeRec},
		{"raftd-config", raftdRec},
		{"frontend-config", frontRec},
		{"origin-certs", certRec},
	}
	var artifacts []Artifact
	for _, w := range writes {
		a, err := gen.WriteBytes(t.Context(), ArtifactSpec{ID: w.id, Kind: KindNodeConfig}, w.body)
		if err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, a)
	}
	m := sampleManifest()
	m.PolicyID, m.GenerationID = "nightly", "gen-000001"
	m.Artifacts = artifacts
	m.SecretGaps = SecretGaps()
	if err := gen.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}

	// The live values that must not appear anywhere.
	live := map[string]string{
		"nodeconfig.PeerAPIKey":        node.PeerAPIKey,
		"nodeconfig.RaftdToken":        node.RaftdToken,
		"frontendconfig.ManagerAPIKey": front.ManagerAPIKey,
		"raftdconfig.InternalToken":    raftd.InternalToken,
		"origincert key PEM":           originCertKeyPEM,
	}
	for where, value := range live {
		if len(value) < 8 {
			t.Fatalf("the fixture's %s is too short to be a meaningful grep target", where)
		}
		assertAbsentEverywhere(t, gen.Dir(), where, value)
	}

	// And the opposite direction: ordinary values from the same configs
	// MUST be there, or the absence assertions above prove only that the
	// records are empty.
	for _, where := range []struct{ field, value string }{
		{"nodeconfig.NodeID", nodeValues["NodeID"]},
		{"nodeconfig.Uplink", nodeValues["Uplink"]},
		{"nodeconfig.ZFSBase", nodeValues["ZFSBase"]},
		{"nodeconfig.TLSKey path", nodeValues["TLSKey"]},
		{"nodeconfig.CloudflareTokenFile path", nodeValues["CloudflareTokenFile"]},
		{"raftdconfig.DataDir", raftdValues["DataDir"]},
		{"raftdconfig.RaftTLSKey path", raftdValues["RaftTLSKey"]},
		{"frontendconfig.ManagerAddr", frontValues["ManagerAddr"]},
		{"frontendconfig.TLSKey path", frontValues["TLSKey"]},
		{"origincert.KeyPath", certs[0].KeyPath},
		{"origincert.CertPath", certs[0].CertPath},
	} {
		assertPresentSomewhere(t, gen.Dir(), where.field, where.value)
	}

	// The secret-path VALUES are recorded as paths, so the manifest's gap
	// list must name every one of them. Silence about a missing secret is
	// how a restored node comes up mysteriously unauthenticated.
	gaps := m.SecretGaps
	for _, want := range []SecretKind{SecretPeerAPIKey, SecretRaftdToken, SecretManagerAPIKey, SecretRaftdInternalToken} {
		found := false
		for _, g := range gaps {
			if g.Kind == string(want) {
				found = true
				if g.Why == "" || g.LiveCopyLocation == "" || g.RestoreProcedure == "" {
					t.Errorf("gap %s does not say why, where the live copy is, and what to do about it: %+v", want, g)
				}
			}
		}
		if !found {
			t.Errorf("the manifest's secret_gaps does not name %s; absence is recorded, not implied", want)
		}
	}
	for _, want := range SecretPaths {
		found := false
		for _, g := range gaps {
			if g.Kind == string(want.Kind) {
				found = true
			}
		}
		if !found {
			t.Errorf("the manifest's secret_gaps does not name the path field %s", want.Kind)
		}
	}
}

// assertAbsentEverywhere greps every file under root for value.
func assertAbsentEverywhere(t *testing.T, root, what, value string) {
	t.Helper()
	walkErr := walkFiles(root, func(path string, body []byte) error {
		if strings.Contains(string(body), value) {
			return fmt.Errorf("the live value for %s appears in %s", what, path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("%v", walkErr)
	}
}

func assertPresentSomewhere(t *testing.T, root, what, value string) {
	t.Helper()
	found := false
	walkErr := walkFiles(root, func(path string, body []byte) error {
		if strings.Contains(string(body), value) {
			found = true
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("%v", walkErr)
	}
	if !found {
		t.Fatalf("the ordinary value for %s (%q) is not anywhere in the generation. The records are empty, which would make every absence assertion above pass without proving anything - a backup that recorded nothing has excluded every secret by storing no configuration at all.", what, value)
	}
}

func walkFiles(root string, fn func(path string, body []byte) error) error {
	real := OSFS{}
	return filepathWalk(root, func(p string, isDir bool) error {
		if isDir {
			return nil
		}
		b, err := real.ReadFile(p)
		if err != nil {
			return err
		}
		return fn(p, b)
	})
}

// # The deny list itself

func TestDenyListNamesTheFourLiveCredentials(t *testing.T) {
	want := map[SecretKind]struct{ structName, field string }{
		SecretPeerAPIKey:         {"nodeconfig.Config", "PeerAPIKey"},
		SecretRaftdToken:         {"nodeconfig.Config", "RaftdToken"},
		SecretManagerAPIKey:      {"frontendconfig.Config", "ManagerAPIKey"},
		SecretRaftdInternalToken: {"raftdconfig.Config", "InternalToken"},
	}
	got := map[SecretKind]struct{ structName, field string }{}
	for _, f := range DenyList {
		if _, dup := got[f.Kind]; dup {
			t.Errorf("deny list names %s twice", f.Kind)
		}
		got[f.Kind] = struct{ structName, field string }{f.StructName, f.FieldName}
	}
	for kind, loc := range want {
		g, ok := got[kind]
		if !ok {
			t.Errorf("DenyList does not include %s", kind)
			continue
		}
		if g != loc {
			t.Errorf("DenyList entry %s points at %s.%s, want %s.%s", kind, g.structName, g.field, loc.structName, loc.field)
		}
	}
	if len(got) != len(want) {
		sorted := make([]string, 0, len(got))
		for k := range got {
			sorted = append(sorted, string(k))
		}
		sort.Strings(sorted)
		t.Errorf("DenyList has %d entries, want exactly the %d known live credentials: %v", len(got), len(want), sorted)
	}
}

func TestSortedSecretKindsIsStable(t *testing.T) {
	kinds := sortedSecretKinds()
	if len(kinds) != len(DenyList) {
		t.Fatalf("got %d kinds, want %d", len(kinds), len(DenyList))
	}
	for i := 1; i < len(kinds); i++ {
		if kinds[i-1] > kinds[i] {
			t.Fatalf("sortedSecretKinds is not sorted: %v", kinds)
		}
	}
}

func TestSecretGapsIsAFreshSlice(t *testing.T) {
	// A caller that mutates the returned slice must not change what the
	// next manifest carries.
	a := SecretGaps()
	if len(a) == 0 {
		t.Fatal("SecretGaps is empty; every manifest would claim it excluded nothing")
	}
	a[0].Kind = "tampered"
	b := SecretGaps()
	if b[0].Kind == "tampered" {
		t.Fatal("SecretGaps returns a shared slice; a caller can corrupt every later manifest's gap list")
	}
}

func TestSecretGapsCoverBothTables(t *testing.T) {
	gaps := SecretGaps()
	if len(gaps) != len(DenyList)+len(SecretPaths) {
		t.Errorf("SecretGaps has %d entries, want %d inline secrets + %d secret paths = %d",
			len(gaps), len(DenyList), len(SecretPaths), len(DenyList)+len(SecretPaths))
	}
	seen := map[string]struct{}{}
	for _, g := range gaps {
		if _, dup := seen[g.Kind]; dup {
			t.Errorf("gap %s appears twice", g.Kind)
		}
		seen[g.Kind] = struct{}{}
		if g.Why == "" || g.LiveCopyLocation == "" || g.RestoreProcedure == "" {
			t.Errorf("gap %s is not fully described: %+v", g.Kind, g)
		}
	}
}

func TestSanitizerErrorsOnANilReceiverRatherThanPassingSilently(t *testing.T) {
	var s *Sanitizer
	if s.Armed() {
		t.Error("a nil Sanitizer claims to be armed")
	}
	if s.Watched() != 0 {
		t.Error("a nil Sanitizer reports watched values")
	}
	if err := s.Guard("x", []byte("y")); err == nil {
		t.Error("a nil Sanitizer accepted a check")
	}
}

func TestAuditSecretFreeRejectsTheLiveStructsThroughAValue(t *testing.T) {
	// The audit's exact-match arm, exercised through a wrapper struct so
	// the refusal names a path a reader can follow.
	node, _ := fixtureNodeConfig(t)
	wrapper := struct {
		Source string
		Config nodeconfig.Config
	}{Source: "comb-a", Config: node}
	err := AuditSecretFree(wrapper)
	if err == nil {
		t.Fatal("a nested live config was accepted")
	}
	if !strings.Contains(err.Error(), "Config.") {
		t.Errorf("the refusal does not name the path it found: %v", err)
	}
}

func TestErrorsAreDistinguishable(t *testing.T) {
	// The sentinels have to be separately matchable, because callers act
	// on them differently: a secret exposure aborts the capture and pages
	// someone, a checksum mismatch marks a generation bad, and a missing
	// artifact is re-captured.
	pairs := []struct{ a, b error }{
		{ErrArtifactMissing, ErrChecksumMismatch},
		{ErrSecretExposed, ErrArtifactMissing},
		{ErrConsistencyNotQuiesced, ErrSkewExceeded},
		{ErrUnknownFormatVersion, ErrMissingFormatVersion},
		{ErrManifestShadowed, ErrGenerationCommitted},
		{ErrArtifactSetMismatch, ErrInvalidManifest},
	}
	for _, p := range pairs {
		if errors.Is(p.a, p.b) {
			t.Errorf("%v and %v are not distinguishable by errors.Is", p.a, p.b)
		}
	}
}
