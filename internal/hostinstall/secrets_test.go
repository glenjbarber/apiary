package hostinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

// Issues #83 and #84: four secrets this installer did not used to
// provision at all - restart-guardrail-token, raftd.json's
// internal_token, managerd.json's raftd_token, and
// managerd.json's known_peer_addresses - left every fresh Comb starting
// with none of them, exactly the gap the live multi-Comb join
// surfaced. These tests prove the gap and then prove the fix.

func (f *fixture) restartGuardrailTokenPath() string {
	return f.opts.Paths.restartGuardrailTokenFile()
}

// --- issue #83: restart-guardrail-token ---

// A standalone install has no other Comb's copy of this file to match,
// so it generates one, and two different fresh hosts never generate the
// same value.
func TestRestartGuardrailTokenGeneratedOnStandaloneInstall(t *testing.T) {
	f := newFixture(t)
	p := f.installed()

	path := f.restartGuardrailTokenPath()
	if !f.exists(path) {
		t.Fatalf("restart-guardrail-token was not created:\n%s", report(p))
	}
	assertMode(t, path, 0o600)
	body := strings.TrimSpace(f.read(path))
	if body == "" {
		t.Error("restart-guardrail-token is empty")
	}

	f2 := newFixture(t)
	f2.installed()
	body2 := strings.TrimSpace(f2.read(f2.restartGuardrailTokenPath()))
	if body == body2 {
		t.Error("two independent standalone installs generated the identical token; it must be unguessable")
	}
}

// A Comb joining an existing Colony cannot safely invent this value:
// every other Comb's managerd already holds a copy, and a forwarded
// confirmation is checked against the LEADER's copy, so an independently
// generated one would silently never match. With no explicit value
// supplied, the install reports a NEED and writes nothing.
func TestRestartGuardrailTokenNeededOnColonyJoinWithoutExplicitValue(t *testing.T) {
	f := newFixture(t)
	f.opts.ColonyMember = "drone.lab3.home.arpa:17700"
	p := f.plan()

	path := f.restartGuardrailTokenPath()
	needs := p.Needs()
	found := false
	for _, n := range needs {
		if n.Path == path {
			found = true
			if !strings.Contains(n.Reason, "--restart-guardrail-token") {
				t.Errorf("the need does not say how to supply the value:\n%s", n.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("restart-guardrail-token is not reported as a need on a --colony-member install:\n%s", report(p))
	}
	if err := p.Apply(); err != nil {
		t.Fatalf("Apply failed on a plain need: %v", err)
	}
	if f.exists(path) {
		t.Error("restart-guardrail-token was written despite nothing being supplied to match the rest of the Colony")
	}
}

// Supplying the value explicitly (copied by the operator from another
// Comb) writes it byte-for-byte, on a Colony-member install.
func TestRestartGuardrailTokenWrittenExplicitlyOnColonyJoin(t *testing.T) {
	f := newFixture(t)
	f.opts.ColonyMember = "drone.lab3.home.arpa:17700"
	f.opts.RestartGuardrailToken = "the-exact-colony-wide-value"
	p := f.installed()

	path := f.restartGuardrailTokenPath()
	assertMode(t, path, 0o600)
	if got := strings.TrimSpace(f.read(path)); got != "the-exact-colony-wide-value" {
		t.Errorf("restart-guardrail-token = %q, want the exact supplied value", got)
	}
	for _, n := range p.Needs() {
		if n.Path == path {
			t.Errorf("restart-guardrail-token is still reported as a need after a value was supplied: %q", n.Reason)
		}
	}
}

// A file already on disk is preserved exactly - not even rewritten to
// identical bytes - the same rule join-authorizations.json already
// follows, because this is a credential an operator or a previous
// install already put there.
func TestRestartGuardrailTokenPreservedWhenAlreadyPresent(t *testing.T) {
	f := newFixture(t)
	path := f.restartGuardrailTokenPath()
	f.write(path, "already-here\n")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	p := f.plan()
	if err := p.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("an existing restart-guardrail-token was rewritten")
	}
	if got := f.read(path); got != "already-here\n" {
		t.Errorf("restart-guardrail-token changed: %q", got)
	}
}

// A file on disk with the wrong mode is reported, not silently
// rechmodded - tightening permissions on a file an operator may be
// reading right now is an unrequested change, matching
// join-authorizations.json's own rule.
func TestRestartGuardrailTokenWrongModeIsRefusedNotFixed(t *testing.T) {
	f := newFixture(t)
	path := f.restartGuardrailTokenPath()
	f.write(path, "already-here\n")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	p := f.plan()
	var found bool
	for _, a := range p.Refusals() {
		if a.Path == path && a.Field == "mode" {
			found = true
			if a.Value != "0644" {
				t.Errorf("the refusal's value = %q, want the mode found (0644)", a.Value)
			}
		}
	}
	if !found {
		t.Fatalf("restart-guardrail-token's wrong mode was not reported as a refusal:\n%s", report(p))
	}
	if err := p.Apply(); err == nil {
		t.Error("Apply returned no error despite the mode refusal")
	}
	assertMode(t, path, 0o644)
}

// --- issue #84, half 1: raftd.json internal_token / managerd.json raftd_token ---

// Unlike the restart-guardrail token above, this pair never leaves the
// host - raftd and managerd are always co-located - so a fresh host with
// neither file setting one gets a freshly generated value in BOTH files,
// and they match.
func TestInternalTokenGeneratedAndPairedOnFreshHost(t *testing.T) {
	f := newFixture(t)
	f.installed()

	raftdCfg, err := (&raftdconfig.Manager{Path: f.configPath("raftd.json")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	managerCfg, err := (&nodeconfig.Manager{Path: f.configPath("managerd.json")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if raftdCfg.InternalToken == "" {
		t.Error("raftd.json internal_token was not generated")
	}
	if managerCfg.RaftdToken == "" {
		t.Error("managerd.json raftd_token was not generated")
	}
	if raftdCfg.InternalToken != managerCfg.RaftdToken {
		t.Errorf("raftd.json internal_token (%q) and managerd.json raftd_token (%q) do not match", raftdCfg.InternalToken, managerCfg.RaftdToken)
	}
}

// When raftd.json already sets internal_token and managerd.json does
// not yet set raftd_token, the existing value is copied across rather
// than a second, independent one being generated - the two must dial
// the same local socket with the same credential.
func TestInternalTokenCopiedFromRaftdWhenManagerdUnset(t *testing.T) {
	f := newFixture(t)
	f.write(f.configPath("raftd.json"), `{"internal_token": "raftd-already-has-this"}`)

	p := f.installed()
	if !strings.Contains(report(p), "already sets internal_token") {
		t.Errorf("the report does not say the value was copied from raftd.json:\n%s", report(p))
	}
	managerCfg, err := (&nodeconfig.Manager{Path: f.configPath("managerd.json")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if managerCfg.RaftdToken != "raftd-already-has-this" {
		t.Errorf("managerd.json raftd_token = %q, want the value copied from raftd.json", managerCfg.RaftdToken)
	}
	// raftd.json's own value must be untouched.
	raftdCfg, err := (&raftdconfig.Manager{Path: f.configPath("raftd.json")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if raftdCfg.InternalToken != "raftd-already-has-this" {
		t.Errorf("raftd.json internal_token changed to %q", raftdCfg.InternalToken)
	}
}

// The symmetric case: managerd.json already sets raftd_token, raftd.json
// does not yet set internal_token.
func TestInternalTokenCopiedFromManagerdWhenRaftdUnset(t *testing.T) {
	f := newFixture(t)
	f.write(f.configPath("managerd.json"), `{"raftd_token": "managerd-already-has-this"}`)

	p := f.installed()
	if !strings.Contains(report(p), "already sets raftd_token") {
		t.Errorf("the report does not say the value was copied from managerd.json:\n%s", report(p))
	}
	raftdCfg, err := (&raftdconfig.Manager{Path: f.configPath("raftd.json")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if raftdCfg.InternalToken != "managerd-already-has-this" {
		t.Errorf("raftd.json internal_token = %q, want the value copied from managerd.json", raftdCfg.InternalToken)
	}
}

// Both files already agreeing is left exactly alone - once every other
// field a fresh install would otherwise fill is already in place too, a
// second run touches neither file, not even to identical bytes. (A
// first run on a file carrying ONLY a matching token still fills every
// other absent field around it, which is a real write for reasons that
// have nothing to do with the token pairing - this test isolates the
// token-pairing decision itself by re-running once everything else is
// already settled.)
func TestInternalTokenKeptWhenBothAlreadyMatch(t *testing.T) {
	f := newFixture(t)
	f.installed()

	raftdBefore, err := os.Lstat(f.configPath("raftd.json"))
	if err != nil {
		t.Fatal(err)
	}
	managerdBefore, err := os.Lstat(f.configPath("managerd.json"))
	if err != nil {
		t.Fatal(err)
	}

	p2 := f.plan()
	if !strings.Contains(report(p2), "matches managerd.json's raftd_token") {
		t.Errorf("the second run's report does not say the pair already matches:\n%s", report(p2))
	}
	if err := p2.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	raftdAfter, err := os.Lstat(f.configPath("raftd.json"))
	if err != nil {
		t.Fatal(err)
	}
	managerdAfter, err := os.Lstat(f.configPath("managerd.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(raftdBefore, raftdAfter) {
		t.Error("raftd.json was rewritten on a second run despite both tokens already matching")
	}
	if !os.SameFile(managerdBefore, managerdAfter) {
		t.Error("managerd.json was rewritten on a second run despite both tokens already matching")
	}
}

// Both files setting DIFFERENT values is a live misconfiguration -
// managerd's dial to raftd's own internal socket will fail - and this
// installer refuses to silently pick a winner. It is reported, and
// nothing is written to either file.
func TestInternalTokenMismatchIsRefusedAndLeftAlone(t *testing.T) {
	f := newFixture(t)
	f.write(f.configPath("raftd.json"), `{"internal_token": "value-one"}`)
	f.write(f.configPath("managerd.json"), `{"raftd_token": "value-two"}`)

	p := f.plan()
	r := report(p)
	if !strings.Contains(r, "do not match") {
		t.Errorf("the refusal does not say the two values disagree:\n%s", r)
	}
	if err := p.Apply(); err == nil {
		t.Error("Apply returned no error despite the mismatch refusal")
	}
	raftdCfg, err := (&raftdconfig.Manager{Path: f.configPath("raftd.json")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if raftdCfg.InternalToken != "value-one" {
		t.Errorf("raftd.json internal_token changed to %q", raftdCfg.InternalToken)
	}
	managerCfg, err := (&nodeconfig.Manager{Path: f.configPath("managerd.json")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if managerCfg.RaftdToken != "value-two" {
		t.Errorf("managerd.json raftd_token changed to %q", managerCfg.RaftdToken)
	}
}

// --- issue #84, half 2: managerd.json known_peer_addresses ---

// A standalone install leaves known_peer_addresses unset, which is
// ADR-0097's own correct unconfigured default (accept any
// target_address) - there is no Colony member to seed it with, and
// forcing a value would break ordinary single-Comb bootstrap.
func TestKnownPeerAddressesNotSetOnStandaloneInstall(t *testing.T) {
	f := newFixture(t)
	f.installed()

	managerCfg, err := (&nodeconfig.Manager{Path: f.configPath("managerd.json")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if managerCfg.KnownPeerAddresses != "" {
		t.Errorf("known_peer_addresses = %q on a standalone install, want unset", managerCfg.KnownPeerAddresses)
	}
}

// A Colony-member install seeds known_peer_addresses with the one
// member it was told about, closing ADR-0097's SSRF-shaped gap for this
// Comb's own unauthenticated join RPCs.
func TestKnownPeerAddressesSeededOnColonyJoin(t *testing.T) {
	f := newFixture(t)
	f.opts.ColonyMember = "drone.lab3.home.arpa:17700"
	f.installed()

	managerCfg, err := (&nodeconfig.Manager{Path: f.configPath("managerd.json")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if managerCfg.KnownPeerAddresses != "drone.lab3.home.arpa:17700" {
		t.Errorf("known_peer_addresses = %q, want the Colony member this install was told about", managerCfg.KnownPeerAddresses)
	}
}

// An already-set known_peer_addresses is left exactly alone - the
// preservation rule every other field in this package already follows.
func TestKnownPeerAddressesPreservedWhenAlreadySet(t *testing.T) {
	f := newFixture(t)
	f.opts.ColonyMember = "drone.lab3.home.arpa:17700"
	f.write(f.configPath("managerd.json"), `{"known_peer_addresses": "hive1.lab3.home.arpa:17700,hive2.lab3.home.arpa:17700"}`)

	p := f.installed()
	if !strings.Contains(report(p), "already set in this file") {
		t.Errorf("the report does not say known_peer_addresses was preserved:\n%s", report(p))
	}
	managerCfg, err := (&nodeconfig.Manager{Path: f.configPath("managerd.json")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	want := "hive1.lab3.home.arpa:17700,hive2.lab3.home.arpa:17700"
	if managerCfg.KnownPeerAddresses != want {
		t.Errorf("known_peer_addresses = %q, want the pre-existing value %q preserved", managerCfg.KnownPeerAddresses, want)
	}
}

// Sanity check that the fixture's own path helper matches the real
// on-disk layout apiaryctl writes to in production.
func TestRestartGuardrailTokenPathMatchesManagerdsFixedConstant(t *testing.T) {
	p := DefaultPaths()
	got := p.restartGuardrailTokenFile()
	want := filepath.Join("/usr/local/etc/apiary", "restart-guardrail-token")
	if got != want {
		t.Errorf("restartGuardrailTokenFile() = %q, want %q (must match cmd/managerd/main.go's own restartGuardrailTokenPath)", got, want)
	}
}
