package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/install"
)

// fakeRunner is an install.Runner that records the commands a check asked
// for and answers with canned output. It never touches the host, which is
// what lets these tests run as an unprivileged user.
type fakeRunner struct {
	calls []string
	// out maps a command name to the stdout that command returns.
	out map[string]string
	// err maps a command name to the error that command returns.
	err map[string]error
}

func (f *fakeRunner) Run(_ context.Context, name string, _ ...string) (string, string, error) {
	f.calls = append(f.calls, name)
	if err, ok := f.err[name]; ok {
		return "", "", err
	}
	return f.out[name], "", nil
}

// check is one synthetic install.Check whose probe outcome and apply
// behaviour the test controls directly.
//
// The probe reports probeBefore on the first call and probeAfter on every
// later call. That models a real fix: the check still fails when it is
// first probed, and passes once the apply has actually changed the host.
// A check whose fix does not take effect keeps reporting its original
// status, because probeAfter defaults to probeBefore.
type check struct {
	id          string
	risk        install.Risk
	probeBefore install.Result
	probeAfter  install.Result
	applyErr    error
	applyCalls  *int
}

func (c check) asInstallCheck() install.Check {
	probes := 0
	return install.Check{
		ID:          c.id,
		Description: c.id,
		Risk:        c.risk,
		Probe: func(context.Context, install.Runner, install.Options) install.Result {
			probes++
			if probes == 1 {
				return c.probeBefore
			}
			return c.probeAfter
		},
		Apply: func(context.Context, install.Runner, install.Options) error {
			if c.applyCalls != nil {
				*c.applyCalls++
			}
			return c.applyErr
		},
	}
}

// okResult is a passing result for the given check ID.
func okResult(id string) install.Result {
	return install.Result{ID: id, Status: install.StatusOK, Detail: "fine"}
}

// runCommand runs the command with the given arguments against the given
// checks and runner, returning the exit code, whatever was written, and
// any error.
func runCommand(t *testing.T, args []string, checks []install.Check, runner install.Runner) (int, string, error) {
	t.Helper()
	var buf bytes.Buffer
	code, err := runWith(args, &buf, runner, checks)
	return code, buf.String(), err
}

// A flag value that is not the confirmation phrase must be refused, and
// the refusal must happen before any check runs. The phrase gate exists
// because -apply-network can take the host's own network reachability
// away (ADR-0022).
func TestApplyNetworkWrongPhraseIsRefusedBeforeAnyCheckRuns(t *testing.T) {
	applied := 0
	checks := []install.Check{
		check{
			id:          "network-thing",
			risk:        install.RiskNetwork,
			probeBefore: install.Result{ID: "network-thing", Status: install.StatusOK},
			applyCalls:  &applied,
		}.asInstallCheck(),
	}
	runner := &fakeRunner{}

	code, _, err := runCommand(t, []string{"-apply-network", "not-the-phrase"}, checks, runner)

	if err == nil {
		t.Fatal("runWith() with a wrong -apply-network phrase = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), networkChangeConfirmPhrase) {
		t.Errorf("error = %q, want it to name the required confirmation phrase", err)
	}
	if code == 0 {
		t.Errorf("exit code = 0, want a non-zero code for a refused confirmation")
	}
	if applied != 0 {
		t.Errorf("apply ran %d times, want 0: nothing may happen before the phrase check passes", applied)
	}
	if len(runner.calls) != 0 {
		t.Errorf("the runner was called with %v, want no commands at all", runner.calls)
	}
}

// -apply-network with an empty value must do nothing at all, including
// not being an error: an empty value is not an attempt, it is an absent
// value, and the confirmation gate must stay shut.
func TestApplyNetworkEmptyValueDoesNothing(t *testing.T) {
	applied := 0
	checks := []install.Check{
		check{
			id:          "network-thing",
			risk:        install.RiskNetwork,
			probeBefore: install.Result{ID: "network-thing", Status: install.StatusOK},
			applyCalls:  &applied,
		}.asInstallCheck(),
	}

	code, _, err := runCommand(t, []string{"-apply-network", ""}, checks, &fakeRunner{})

	if err != nil {
		t.Fatalf("runWith() with an empty -apply-network = %v, want no error", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if applied != 0 {
		t.Errorf("a RiskNetwork check was applied %d times, want 0 without the confirmation phrase", applied)
	}
}

// The exact confirmation phrase must enable the network-risk apply path.
func TestApplyNetworkExactPhraseAppliesNetworkRiskChecks(t *testing.T) {
	applied := 0
	checks := []install.Check{
		check{
			id:          "network-thing",
			risk:        install.RiskNetwork,
			probeBefore: install.Result{ID: "network-thing", Status: install.StatusMissing},
			probeAfter:  install.Result{ID: "network-thing", Status: install.StatusOK},
			applyCalls:  &applied,
		}.asInstallCheck(),
	}

	code, _, err := runCommand(t, []string{"-apply-network", networkChangeConfirmPhrase}, checks, &fakeRunner{})

	if err != nil {
		t.Fatalf("runWith() with the correct phrase = %v, want no error", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if applied != 1 {
		t.Errorf("apply ran %d times, want 1 once the phrase was given", applied)
	}
}

// -apply must fix RiskSafe checks and must not touch RiskNetwork ones,
// even when a network check is already failing.
func TestApplyFixesSafeChecksAndLeavesNetworkChecksAlone(t *testing.T) {
	safeApplied, networkApplied := 0, 0
	checks := []install.Check{
		check{
			id:          "safe-thing",
			risk:        install.RiskSafe,
			probeBefore: install.Result{ID: "safe-thing", Status: install.StatusMissing},
			probeAfter:  install.Result{ID: "safe-thing", Status: install.StatusOK},
			applyCalls:  &safeApplied,
		}.asInstallCheck(),
		check{
			id:          "network-thing",
			risk:        install.RiskNetwork,
			probeBefore: install.Result{ID: "network-thing", Status: install.StatusMissing},
			probeAfter:  install.Result{ID: "network-thing", Status: install.StatusOK},
			applyCalls:  &networkApplied,
		}.asInstallCheck(),
	}

	code, out, err := runCommand(t, []string{"-apply"}, checks, &fakeRunner{})

	if err != nil {
		t.Fatalf("runWith() = %v, want no error", err)
	}
	if safeApplied != 1 {
		t.Errorf("the RiskSafe check was applied %d times, want 1", safeApplied)
	}
	if networkApplied != 0 {
		t.Errorf("the RiskNetwork check was applied %d times, want 0 without -apply-network", networkApplied)
	}
	// The network check was never applied, so it is still missing, and the
	// command must say so: a risk-gated fix that did not happen is a
	// finding the operator has to see, not a silent success.
	if code != exitFailure {
		t.Errorf("exit code = %d, want %d: the unapplied network check is still missing", code, exitFailure)
	}
	if !strings.Contains(out, "network-thing") {
		t.Errorf("output does not name the check that was left alone:\n%s", out)
	}
}

// A check that is already ok must not be applied at all: applying it
// would hit the same non-conflict it just reported and refuse, turning a
// non-issue into an apparent failure.
func TestApplySkipsChecksThatAreAlreadyOK(t *testing.T) {
	applied := 0
	checks := []install.Check{
		check{
			id:          "fine-thing",
			risk:        install.RiskSafe,
			probeBefore: okResult("fine-thing"),
			applyCalls:  &applied,
		}.asInstallCheck(),
	}

	if _, _, err := runCommand(t, []string{"-apply"}, checks, &fakeRunner{}); err != nil {
		t.Fatalf("runWith() = %v, want no error", err)
	}
	if applied != 0 {
		t.Errorf("an already-ok check was applied %d times, want 0", applied)
	}
}

// An informational finding is not a problem to resolve, so it must not be
// applied and must not make the command fail.
func TestInformationalStatusIsNeitherAppliedNorAFailure(t *testing.T) {
	applied := 0
	checks := []install.Check{
		check{
			id:          "info-thing",
			risk:        install.RiskSafe,
			probeBefore: install.Result{ID: "info-thing", Status: install.StatusInformational, Detail: "just so you know"},
			applyCalls:  &applied,
		}.asInstallCheck(),
	}

	code, _, err := runCommand(t, []string{"-apply"}, checks, &fakeRunner{})

	if err != nil {
		t.Fatalf("runWith() = %v, want no error", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0: an informational finding is not a failure", code)
	}
	if applied != 0 {
		t.Errorf("an informational check was applied %d times, want 0", applied)
	}
}

// A check that does not come back ok must make the command exit non-zero,
// because that is the whole signal an operator or a script reads.
func TestFailingCheckProducesAFailureExitCode(t *testing.T) {
	checks := []install.Check{
		{
			ID:   "broken-thing",
			Risk: install.RiskManualOnly,
			Probe: func(context.Context, install.Runner, install.Options) install.Result {
				return install.Result{ID: "broken-thing", Status: install.StatusMisconfigured, Detail: "wrong", FixHint: "fix it"}
			},
		},
	}

	code, _, err := runCommand(t, nil, checks, &fakeRunner{})

	if err != nil {
		t.Fatalf("runWith() = %v, want no error: a failing check is a result, not a crash", err)
	}
	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
}

// An apply that fails must be reported as an apply failure in that
// check's detail, and must make the command exit non-zero.
func TestApplyFailureIsReportedAndFails(t *testing.T) {
	checks := []install.Check{
		check{
			id:          "apply-fails",
			risk:        install.RiskSafe,
			probeBefore: install.Result{ID: "apply-fails", Status: install.StatusMissing},
			applyErr:    context.DeadlineExceeded,
		}.asInstallCheck(),
	}

	code, out, err := runCommand(t, []string{"-apply", "-json"}, checks, &fakeRunner{})

	if err != nil {
		t.Fatalf("runWith() = %v, want no error", err)
	}
	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(out, "apply failed") {
		t.Errorf("output = %q, want it to say the apply failed", out)
	}
}

// The JSON output must decode into the same result list the table output
// describes, so a script can consume either form.
func TestJSONOutputDecodesIntoResults(t *testing.T) {
	checks := []install.Check{
		{
			ID:   "one",
			Risk: install.RiskManualOnly,
			Probe: func(context.Context, install.Runner, install.Options) install.Result {
				return install.Result{ID: "one", Status: install.StatusOK, Detail: "all good"}
			},
		},
		{
			ID:   "two",
			Risk: install.RiskManualOnly,
			Probe: func(context.Context, install.Runner, install.Options) install.Result {
				return install.Result{ID: "two", Status: install.StatusManual, Detail: "do it by hand", FixHint: "read the docs"}
			},
		},
	}

	code, out, err := runCommand(t, []string{"-json"}, checks, &fakeRunner{})
	if err != nil {
		t.Fatalf("runWith() = %v, want no error", err)
	}
	if code != exitFailure {
		t.Errorf("exit code = %d, want %d: one check did not come back ok", code, exitFailure)
	}

	var got []install.Result
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decoding the JSON output %q: %v", out, err)
	}
	if len(got) != 2 {
		t.Fatalf("decoded %d results, want 2", len(got))
	}
	if got[0].ID != "one" || got[0].Status != install.StatusOK {
		t.Errorf("result[0] = %+v, want ID one with status ok", got[0])
	}
	if got[1].ID != "two" || got[1].Status != install.StatusManual || got[1].FixHint != "read the docs" {
		t.Errorf("result[1] = %+v, want ID two, status manual, and its fix hint", got[1])
	}
}

// The table output must name every check, its status and the summary
// count, and must keep a multi-line fix hint in one column rather than
// letting it look like extra checks.
func TestTableOutputNamesChecksAndCountsThem(t *testing.T) {
	checks := []install.Check{
		{
			ID:   "good-thing",
			Risk: install.RiskManualOnly,
			Probe: func(context.Context, install.Runner, install.Options) install.Result {
				return install.Result{ID: "good-thing", Status: install.StatusOK, Detail: "fine"}
			},
		},
		{
			ID:   "bad-thing",
			Risk: install.RiskManualOnly,
			Probe: func(context.Context, install.Runner, install.Options) install.Result {
				return install.Result{ID: "bad-thing", Status: install.StatusManual, Detail: "hand fix", FixHint: "first line\nsecond line"}
			},
		},
	}

	_, out, err := runCommand(t, nil, checks, &fakeRunner{})
	if err != nil {
		t.Fatalf("runWith() = %v, want no error", err)
	}

	for _, want := range []string{"ID", "STATUS", "DETAIL", "FIX HINT", "good-thing", "bad-thing", "ok", "manual", "first line", "second line"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "1/2 checks ok") {
		t.Errorf("table output is missing the summary line:\n%s", out)
	}
}

// -version must answer without running a single check. That is the whole
// point of the flag: a deployed binary whose host has no /var/db/apiary
// and no privileges should still be able to say what it is.
func TestVersionAnswersWithoutRunningAnyCheck(t *testing.T) {
	runner := &fakeRunner{}

	code, out, err := runCommand(t, []string{"-version"}, nil, runner)

	if err != nil {
		t.Fatalf("runWith() -version = %v, want no error", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "apiaryinstall") {
		t.Errorf("version output = %q, want it to name the program", out)
	}
	if len(runner.calls) != 0 {
		t.Errorf("the runner was called with %v, want no commands for -version", runner.calls)
	}
}

// The version flag must not leak from one invocation to the next in the
// same process. A shared flag destination would make a later run answer
// with the version report even though -version was not passed.
func TestVersionFlagDoesNotLeakBetweenRuns(t *testing.T) {
	runner := &fakeRunner{}

	if code, _, err := runCommand(t, []string{"-version"}, nil, runner); err != nil || code != 0 {
		t.Fatalf("first runWith() -version = (%d, %v), want (0, nil)", code, err)
	}

	checks := []install.Check{
		{
			ID:   "still-runs",
			Risk: install.RiskManualOnly,
			Probe: func(context.Context, install.Runner, install.Options) install.Result {
				return install.Result{ID: "still-runs", Status: install.StatusOK}
			},
		},
	}
	code, out, err := runCommand(t, nil, checks, runner)
	if err != nil {
		t.Fatalf("second runWith() = %v, want no error", err)
	}
	if code != 0 {
		t.Errorf("second run exit code = %d, want 0", code)
	}
	if strings.Contains(out, "apiaryinstall ") && !strings.Contains(out, "still-runs") {
		t.Errorf("second run printed the version report: %q", out)
	}
	if !strings.Contains(out, "still-runs") {
		t.Errorf("second run did not run the check: %q", out)
	}
}

// The flags the operator supplies must reach the checks as Options.
func TestFlagsReachTheChecksAsOptions(t *testing.T) {
	var seen install.Options
	checks := []install.Check{
		{
			ID:   "opts",
			Risk: install.RiskManualOnly,
			Probe: func(_ context.Context, _ install.Runner, opt install.Options) install.Result {
				seen = opt
				return install.Result{ID: "opts", Status: install.StatusOK}
			},
		},
	}

	_, _, err := runCommand(t, []string{
		"-zfs-pool", "tank",
		"-zfs-base", "tank/apiary",
		"-bhyve-firmware-pkg", "fw",
		"-vlan-uplink", "em0",
		"-bhyve-bridge", "bridge0",
		"-enable-nat",
		"-enable-hast",
		"-pam-service", "apiary",
		"-allow-uplink-bridging", "yes",
	}, checks, &fakeRunner{})
	if err != nil {
		t.Fatalf("runWith() = %v, want no error", err)
	}

	want := install.Options{
		ZFSPool:             "tank",
		ZFSBase:             "tank/apiary",
		BhyveFirmwarePkg:    "fw",
		VLANUplink:          "em0",
		BhyveBridge:         "bridge0",
		EnableNAT:           true,
		EnableHAST:          true,
		PAMService:          "apiary",
		AllowUplinkBridging: "yes",
	}
	if seen != want {
		t.Errorf("options = %+v, want %+v", seen, want)
	}
}

// A check that does not apply to the given options must be skipped
// entirely, not probed and reported.
func TestInapplicableChecksAreSkipped(t *testing.T) {
	probed := false
	checks := []install.Check{
		{
			ID:   "nat-only",
			Risk: install.RiskManualOnly,
			Probe: func(context.Context, install.Runner, install.Options) install.Result {
				probed = true
				return install.Result{ID: "nat-only", Status: install.StatusOK}
			},
			Applicable: func(opt install.Options) bool { return opt.EnableNAT },
		},
	}

	code, out, err := runCommand(t, nil, checks, &fakeRunner{})
	if err != nil {
		t.Fatalf("runWith() = %v, want no error", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0: the only check was skipped", code)
	}
	if probed {
		t.Error("an inapplicable check was probed")
	}
	if strings.Contains(out, "nat-only") {
		t.Errorf("output names a skipped check:\n%s", out)
	}
}

// An unknown flag must be an error, not a silent success, and it must
// carry the flag package's own usage exit code so a caller scripting
// around this command sees no behaviour change.
func TestUnknownFlagIsAnError(t *testing.T) {
	code, _, err := runCommand(t, []string{"-not-a-real-flag"}, nil, &fakeRunner{})
	if err == nil {
		t.Fatal("runWith() with an unknown flag = nil error, want one")
	}
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d for a bad command line", code, exitUsage)
	}
}
