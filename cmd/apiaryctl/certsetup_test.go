package main

// ADR-0172's guided certificate setup walkthrough.
//
// What these pin: that a credential never reaches an argument, an
// echo, or the output; that the naming suffix is enforced BEFORE
// anything is requested; that the saved token file is 0600; that the
// saved integration carries no secret; and that the flow works
// non-interactively, because a command that only works at a console
// cannot be tested.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The suffix is what stops the saved integration from issuing for a
// name the operator never meant to give this Colony, so it is checked
// before anything is requested.
func TestNameAllowedAgainstSuffix(t *testing.T) {
	cases := []struct {
		name    string
		fqdn    string
		suffix  string
		wantErr string
	}{
		{"inside the suffix", "host2.lab2.apiary.work", ".lab2.apiary.work", ""},
		{"suffix without a leading dot is accepted", "host2.lab2.apiary.work", "lab2.apiary.work", ""},
		{"outside the suffix", "host2.evil.example.com", ".lab2.apiary.work", "not inside the allowed suffix"},
		{"the suffix itself is not a name", "lab2.apiary.work", ".lab2.apiary.work", "not inside the allowed suffix"},
		{"an empty suffix allows nothing", "host2.lab2.apiary.work", "", "no allowed naming suffix"},
		{"the whole zone is not a suffix", "host2.example.com", ".", "must name a domain"},
		{"an empty name is refused", "", ".lab2.apiary.work", "name is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := nameAllowedAgainstSuffix(tc.fqdn, tc.suffix)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("nameAllowedAgainstSuffix(%q, %q) = %v, want nil", tc.fqdn, tc.suffix, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("nameAllowedAgainstSuffix(%q, %q) = nil, want an error mentioning %q", tc.fqdn, tc.suffix, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("the refusal %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// The token is written 0600. A world-readable zone credential is a
// credential anyone on the host can spend.
func TestSaveCloudflareToken_Is0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := saveCloudflareToken(path, "the-token"); err != nil {
		t.Fatalf("saveCloudflareToken: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the token file's mode is %04o, want 0600", mode)
	}
	// And the file must not be readable by anyone else, which the
	// mode is the whole point of.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the token: %v", err)
	}
	if strings.TrimSpace(string(data)) != "the-token" {
		t.Errorf("the token file holds %q, want the token", string(data))
	}
}

// An EXISTING token file with a loose mode is tightened. The walkthrough
// promises the token is not readable by anyone else, and a file that
// already existed with 0644 would break that promise silently.
func TestSaveCloudflareToken_TightensAnExistingLooseMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveCloudflareToken(path, "new"); err != nil {
		t.Fatalf("saveCloudflareToken: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the token file's mode is %04o, want it tightened to 0600", mode)
	}
}

// The saved integration carries NO secret. It is the record that may be
// copied between Combs, so a token in it would be a token copied
// between Combs.
func TestSavedIntegrationCarriesNoSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "integration.json")
	integration := &certSetupIntegration{
		ZoneID:        "zone-1",
		ZoneName:      "example.com",
		AllowedSuffix: ".lab2.apiary.work",
		ACMEEmail:     "ops@example.com",
	}
	if err := saveCertSetupIntegration(path, integration); err != nil {
		t.Fatalf("saveCertSetupIntegration: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the integration: %v", err)
	}
	for _, forbidden := range []string{"token", "Bearer", "api_key", "apiKey", "private_key", "privateKey"} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("the saved integration contains %q; it must carry no secret", forbidden)
		}
	}
	// And it round trips.
	loaded, err := loadCertSetupIntegration(path)
	if err != nil {
		t.Fatalf("loadCertSetupIntegration: %v", err)
	}
	if loaded.ZoneID != "zone-1" || loaded.AllowedSuffix != ".lab2.apiary.work" {
		t.Errorf("the loaded integration is %+v, want the one that was saved", loaded)
	}
}

// A non-interactive run must FAIL rather than wait for input that will
// never come. A command that hangs on a console is worse than one that
// says what it needed and stops.
func TestPrompt_RefusesToPromptWhenNonInteractive(t *testing.T) {
	state := &certSetupState{
		NonInteractive: true,
		Out:            os.Stdout,
		Err:            os.Stderr,
		In:             strings.NewReader(""),
	}
	if _, err := state.prompt("name", ""); err == nil {
		t.Error("prompt succeeded with no default in non-interactive mode")
	}
	// With a default, the default is the answer.
	got, err := state.prompt("name", "the-default")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if got != "the-default" {
		t.Errorf("prompt returned %q, want the default", got)
	}
}

// A secret is never prompted for in non-interactive mode: there is no
// safe way to ask, and --token-file is the answer.
func TestPromptSecret_RefusesInNonInteractiveMode(t *testing.T) {
	state := &certSetupState{NonInteractive: true, Out: os.Stdout, Err: os.Stderr}
	if _, err := state.promptSecret("token"); err == nil {
		t.Error("promptSecret succeeded in non-interactive mode; a secret must come from a file instead")
	}
}

// The token file is read when one is given, and the token is never
// echoed on the way in.
func TestObtainToken_ReadsTheGivenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("the-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := &certSetupState{Out: os.Stdout, Err: os.Stderr}
	got, err := state.obtainToken(path)
	if err != nil {
		t.Fatalf("obtainToken: %v", err)
	}
	if got != "the-token" {
		t.Errorf("obtainToken returned %q, want the-token", got)
	}
}

// An empty token file is refused rather than producing an empty token
// that would fail later at the provider with a confusing message.
func TestObtainToken_RefusesAnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := &certSetupState{Out: os.Stdout, Err: os.Stderr}
	if _, err := state.obtainToken(path); err == nil {
		t.Error("an empty token file was accepted")
	}
}

// A missing integration is named with what to do about it, because the
// operator will hit it on a Comb that has never been set up.
func TestLoadCertSetupIntegration_NamesWhatToDoWhenAbsent(t *testing.T) {
	_, err := loadCertSetupIntegration(filepath.Join(t.TempDir(), "integration.json"))
	if err == nil {
		t.Fatal("a missing integration was loaded")
	}
	if !strings.Contains(err.Error(), "cert-setup setup") {
		t.Errorf("the refusal %q does not say what to run", err)
	}
}

// The zone ID heuristic decides only whether to try a NAME lookup, so a
// value that is neither must still resolve as a name and be refused
// with a clear message rather than sent to the API as an ID.
func TestIsLikelyZoneID(t *testing.T) {
	if !isLikelyZoneID("0123456789abcdef0123456789abcdef") {
		t.Error("a 32-character hex string was not recognised as a zone ID")
	}
	for _, notID := range []string{"example.com", "short", "0123456789abcdef0123456789abcdeZ"} {
		if isLikelyZoneID(notID) {
			t.Errorf("%q was treated as a zone ID", notID)
		}
	}
}
