package hostpkg

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// appliedCollector wires a Collector whose fake Applier advances the
// scripted host state, so a successful upgrade is reflected in the
// post-condition read exactly as a real one would be. That is what lets
// the tests below assert on the property the whole design rests on: the
// outcome comes from the re-read, not from the command's exit status.
func appliedCollector(t *testing.T) (*Collector, *fakeRunner, *fakeApplier) {
	return collectorAdvancing(t, true)
}

// stalledCollector is appliedCollector with an Applier that reports
// success but changes nothing on the host - the shape of a pkg that
// exits 0 having quietly decided it had nothing to do.
func stalledCollector(t *testing.T) (*Collector, *fakeRunner, *fakeApplier) {
	return collectorAdvancing(t, false)
}

func collectorAdvancing(t *testing.T, advance bool) (*Collector, *fakeRunner, *fakeApplier) {
	t.Helper()
	runner := standardHost()
	applier := &fakeApplier{result: UpgradeResult{ExitCode: 0, Stdout: "Updating...\n"}}
	c := freshCollector(t, runner)
	// The privileged command's effect on the host is modelled honestly:
	// nothing happens until Upgrade is actually called, and what happens
	// then is the host genuinely moving forward. A test that wants a
	// different outcome simply leaves the runner alone (nothing applied)
	// or breaks it (unreadable).
	c.SetApplier(&advancingApplier{inner: applier, runner: runner, advance: advance})
	return c, runner, applier
}

// advancingApplier is a fake Applier that also moves the scripted host
// state forward when it is asked to upgrade, which is what a real pkg
// does. It exists so the post-condition read has something true to
// confirm. With advance=false it reports success and leaves the host
// alone, which is a distinct and important failure mode.
type advancingApplier struct {
	inner   *fakeApplier
	runner  *fakeRunner
	advance bool
}

func (a *advancingApplier) Upgrade(ctx context.Context, packages []string) (UpgradeResult, error) {
	res, err := a.inner.Upgrade(ctx, packages)
	if a.advance && err == nil && res.ExitCode == 0 {
		a.runner.replace(upgradedHostResponses())
	}
	return res, err
}

// TestApplySuccessIsDefinedByThePostConditionRead is the central test of
// the whole apply path: the command reports exit 0, and the outcome is
// still decided by a fresh read showing the packages are no longer
// outdated.
func TestApplySuccessIsDefinedByThePostConditionRead(t *testing.T) {
	c, _, applier := appliedCollector(t)

	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	plan := PlanFrom(inv, HostRisk{}, PlanOptions{})

	rec := c.Apply(context.Background(), ApplyRequest{Plan: plan, Phrase: plan.RequiredConfirmPhrase()})
	if rec.Outcome != ApplyApplied {
		t.Fatalf("Outcome = %q, want %q (detail: %s)", rec.Outcome, ApplyApplied, rec.Detail)
	}
	if !reflect.DeepEqual(applier.calls(), [][]string{{"bash", "curl"}}) {
		t.Errorf("privileged command named %v, want exactly [bash curl]", applier.calls())
	}
	if rec.Evidence == nil || !rec.Evidence.Observed {
		t.Fatal("a successful apply must carry the observation that proved it")
	}
	if rec.Evidence.Observed && (len(rec.Evidence.StillOutdated) != 0 || len(rec.Evidence.Unknown) != 0) {
		t.Errorf("Evidence = %+v, want nothing still outdated and nothing unconfirmed", rec.Evidence)
	}
	if rec.Seq == 0 {
		t.Error("Seq = 0, want a sequence number")
	}
	if rec.FinishedAt.Before(rec.StartedAt) {
		t.Error("FinishedAt precedes StartedAt")
	}
}

// The inverse of the above, and the one that actually protects an
// operator: pkg exits 0, and the packages are still outdated. That is a
// failure, not a success.
func TestApplyExitZeroWithPackagesStillOutdatedIsFailure(t *testing.T) {
	c, _, _ := stalledCollector(t)

	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	plan := PlanFrom(inv, HostRisk{}, PlanOptions{})

	// The post-condition read still sees the original host.
	rec := c.Apply(context.Background(), ApplyRequest{Plan: plan, Phrase: plan.RequiredConfirmPhrase()})
	if rec.Outcome == ApplyApplied {
		t.Fatalf("Outcome = %q, want a failure: the fake host never changed, so nothing was applied", rec.Outcome)
	}
	if rec.Outcome != ApplyFailed {
		t.Errorf("Outcome = %q, want %q", rec.Outcome, ApplyFailed)
	}
	if rec.Evidence == nil || len(rec.Evidence.StillOutdated) != 2 {
		t.Errorf("Evidence = %+v, want both packages still outdated", rec.Evidence)
	}
	if !strings.Contains(rec.Detail, "did not do what was asked") {
		t.Errorf("Detail = %q, want it to call out pkg reporting success without doing the work", rec.Detail)
	}
}

// A post-condition read that cannot be made is unknown. Never success,
// never failure.
func TestApplyUnreadablePostConditionIsUnknownNotSuccess(t *testing.T) {
	c, runner, _ := appliedCollector(t)

	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	plan := PlanFrom(inv, HostRisk{}, PlanOptions{})

	// pkg itself breaks the moment the command runs - the very specific
	// and quite real case where the upgrade landed but the host can no
	// longer be asked about itself.
	c.SetApplier(applierFunc(func(ctx context.Context, packages []string) (UpgradeResult, error) {
		res, err := (&fakeApplier{result: UpgradeResult{ExitCode: 0}}).Upgrade(ctx, packages)
		runner.replace(map[string]response{
			"uname -r":                    {stdout: "15.0-RELEASE-p4\n"},
			"uname -v":                    {stdout: "15.0-RELEASE-p4\n"},
			"pkg version":                 {stderr: "pkg: cannot open database", err: errors.New("exit status 1")},
			"pkg query -e %n|%v|%o %n-%v": {stderr: "pkg: cannot open database", err: errors.New("exit status 1")},
			"pkg outdated":                {stderr: "pkg: cannot open database", err: errors.New("exit status 1")},
		})
		return res, err
	}))

	rec := c.Apply(context.Background(), ApplyRequest{Plan: plan, Phrase: plan.RequiredConfirmPhrase()})
	if rec.Outcome != ApplyUnknown {
		t.Fatalf("Outcome = %q, want %q: an unreadable post-read is 'could not determine', not success", rec.Outcome, ApplyUnknown)
	}
	if rec.Outcome == ApplyApplied {
		t.Fatal("an apply whose outcome could not be read was reported as applied")
	}
	if !strings.Contains(rec.Detail, "could not be confirmed") && !strings.Contains(rec.Detail, "not success") {
		t.Errorf("Detail = %q, want it to say the result is unknown and not success", rec.Detail)
	}
	if len(rec.Evidence.Unknown) == 0 {
		t.Errorf("Evidence = %+v, want the unconfirmed packages named", rec.Evidence)
	}
}

// pkg installs a version the plan never named - a held package, a
// distfile change, a locally patched port. Every package is current, so
// the naive check would call this a success. It is not the upgrade that
// was previewed, and it gets its own verdict.
func TestApplyDivergentVersionIsNotReportedAsApplied(t *testing.T) {
	c, runner, _ := appliedCollector(t)

	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	plan := PlanFrom(inv, HostRisk{}, PlanOptions{})

	// bash lands somewhere other than the planned 5.2.26; curl lands
	// exactly where it was planned to.
	diverged := upgradedHostResponses()
	diverged["pkg query -e %n|%v|%o %n-%v"] = response{stdout: strings.Join([]string{
		"bash|5.3.0|shells/bash",
		"curl|8.14.1|www/curl",
		"nginx|1.26.0|www/nginx",
	}, "\n") + "\n"}

	c.SetApplier(applierFunc(func(ctx context.Context, packages []string) (UpgradeResult, error) {
		res, err := (&fakeApplier{result: UpgradeResult{ExitCode: 0}}).Upgrade(ctx, packages)
		runner.replace(diverged)
		return res, err
	}))

	rec := c.Apply(context.Background(), ApplyRequest{Plan: plan, Phrase: plan.RequiredConfirmPhrase()})
	if rec.Outcome != ApplyDiverged {
		t.Fatalf("Outcome = %q, want %q (detail: %s)", rec.Outcome, ApplyDiverged, rec.Detail)
	}
	if rec.Evidence == nil || len(rec.Evidence.Divergent) != 1 {
		t.Fatalf("Evidence = %+v, want exactly one divergent package", rec.Evidence)
	}
	if !strings.Contains(rec.Evidence.Divergent[0], "installed 5.3.0, planned 5.2.26") {
		t.Errorf("Divergent = %v, want the installed and planned versions named", rec.Evidence.Divergent)
	}
	if !strings.Contains(rec.Detail, "did not name") {
		t.Errorf("Detail = %q, want it to say the version was not the one previewed", rec.Detail)
	}
}

func TestApplyRefusesWrongPhrase(t *testing.T) {
	c, _, applier := appliedCollector(t)

	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	plan := PlanFrom(inv, HostRisk{}, PlanOptions{})

	for _, phrase := range []string{"", "yes", "YES-UPDATE-PACKAGES", ConfirmPhrase + " "} {
		rec := c.Apply(context.Background(), ApplyRequest{Plan: plan, Phrase: phrase})
		if rec.Outcome != ApplyRefused {
			t.Errorf("phrase %q: Outcome = %q, want %q", phrase, rec.Outcome, ApplyRefused)
		}
		if len(applier.calls()) != 0 {
			t.Fatalf("phrase %q reached the privileged boundary", phrase)
		}
		if rec.Command != nil {
			t.Errorf("phrase %q recorded a command %v, want none", phrase, rec.Command)
		}
		if rec.ExitCode != -1 {
			t.Errorf("phrase %q recorded exit %d, want -1 (no command ran)", phrase, rec.ExitCode)
		}
		if !strings.Contains(rec.Detail, "nothing was run") {
			t.Errorf("phrase %q: Detail = %q, want it to say nothing was run", phrase, rec.Detail)
		}
	}
}

// A plan gated to caution requires a different phrase, so an operator
// who rehearsed the flow on a safe host cannot muscle through on a HAST
// primary.
func TestApplyHazardGateRequiresItsOwnPhrase(t *testing.T) {
	c, _, applier := appliedCollector(t)

	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	plan := PlanFrom(inv, HostRisk{HASTRole: "primary", HASTResources: []string{"vm-abc"}}, PlanOptions{})
	if plan.RequiredConfirmPhrase() != ConfirmHazardPhrase {
		t.Fatalf("RequiredConfirmPhrase() = %q, want %q", plan.RequiredConfirmPhrase(), ConfirmHazardPhrase)
	}

	rec := c.Apply(context.Background(), ApplyRequest{Plan: plan, Phrase: ConfirmPhrase})
	if rec.Outcome != ApplyRefused {
		t.Errorf("Outcome = %q, want %q: the ordinary phrase must not apply to a hazard-gated plan", rec.Outcome, ApplyRefused)
	}
	if len(applier.calls()) != 0 {
		t.Fatal("the ordinary phrase reached the privileged boundary on a HAST primary")
	}

	rec = c.Apply(context.Background(), ApplyRequest{Plan: plan, Phrase: ConfirmHazardPhrase})
	if rec.Outcome != ApplyApplied {
		t.Errorf("Outcome = %q, want %q (detail: %s)", rec.Outcome, ApplyApplied, rec.Detail)
	}
}

func TestApplyRefusesWithoutRoot(t *testing.T) {
	runner := standardHost()
	c := NewCollector(Options{
		Runner:    runner,
		Privilege: fakePrivilege{root: false, detail: "effective uid is 501"},
		Now:       func() time.Time { return baseTime },
		stat:      fixedStat{mod: baseTime.Add(-time.Hour)},
	})
	applier := &fakeApplier{}
	c.SetApplier(applier)

	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	plan := PlanFrom(inv, HostRisk{}, PlanOptions{})

	rec := c.Apply(context.Background(), ApplyRequest{Plan: plan, Phrase: plan.RequiredConfirmPhrase()})
	if rec.Outcome != ApplyRefused {
		t.Fatalf("Outcome = %q, want %q", rec.Outcome, ApplyRefused)
	}
	if len(applier.calls()) != 0 {
		t.Fatal("a non-root apply reached the privileged boundary")
	}
	if rec.Privilege != "effective uid is 501" {
		t.Errorf("Privilege = %q, want the check's own detail recorded", rec.Privilege)
	}
	if !strings.Contains(rec.Detail, "nothing was run") {
		t.Errorf("Detail = %q, want it to say nothing was run", rec.Detail)
	}
}

func TestApplyRefusesRemovals(t *testing.T) {
	c, _, applier := appliedCollector(t)

	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	plan := PlanFrom(inv, HostRisk{}, PlanOptions{})
	// Splice a removal in, as a future version's planner might.
	plan.Changes = append(plan.Changes, Change{Action: ActionRemove, Package: InstalledPackage{Name: "nginx"}, Reason: "obsolete"})

	rec := c.Apply(context.Background(), ApplyRequest{Plan: plan, Phrase: ConfirmPhrase})
	if rec.Outcome != ApplyRefused {
		t.Fatalf("Outcome = %q, want %q", rec.Outcome, ApplyRefused)
	}
	if len(applier.calls()) != 0 {
		t.Fatal("a plan containing a removal reached the privileged boundary")
	}
	if !strings.Contains(rec.Detail, "removing packages is not supported") {
		t.Errorf("Detail = %q, want it to name the removal refusal", rec.Detail)
	}
}

func TestApplyRefusesEmptyPlan(t *testing.T) {
	c, _, applier := appliedCollector(t)

	rec := c.Apply(context.Background(), ApplyRequest{Plan: Plan{}, Phrase: ConfirmPhrase})
	if rec.Outcome != ApplyRefused {
		t.Fatalf("Outcome = %q, want %q", rec.Outcome, ApplyRefused)
	}
	if len(applier.calls()) != 0 {
		t.Fatal("a zero plan reached the privileged boundary")
	}
	if !strings.Contains(rec.Detail, "no inventory fingerprint") {
		t.Errorf("Detail = %q, want it to say the plan was never derived from a real inventory", rec.Detail)
	}
}

func TestApplyRefusesWhenNothingToDo(t *testing.T) {
	runner := newFakeRunner().
		on("uname -r", "15.0-RELEASE-p4\n").
		on("uname -v", "15.0-RELEASE-p4\n").
		on("pkg version", "FreeBSD 15.0-RELEASE-p4 [repo FreeBSD]: up to date\n").
		on("pkg query -e %n|%v|%o %n-%v", "bash|5.2.26|shells/bash\n").
		on("pkg outdated", "")
	c := freshCollector(t, runner)
	applier := &fakeApplier{}
	c.SetApplier(applier)

	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	plan := PlanFrom(inv, HostRisk{}, PlanOptions{})

	rec := c.Apply(context.Background(), ApplyRequest{Plan: plan, Phrase: plan.RequiredConfirmPhrase()})
	if rec.Outcome != ApplyRefused {
		t.Fatalf("Outcome = %q, want %q", rec.Outcome, ApplyRefused)
	}
	if len(applier.calls()) != 0 {
		t.Fatal("an up-to-date plan reached the privileged boundary")
	}
}

// A host that changed between the preview and the apply must be
// refused, not silently upgraded under the old plan.
func TestApplyRefusesStalePlan(t *testing.T) {
	c, runner, applier := appliedCollector(t)

	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	plan := PlanFrom(inv, HostRisk{}, PlanOptions{})

	// The host moves on: a third package now has an update too.
	moved := standardHostResponses()
	moved["pkg outdated"] = response{stdout: strings.Join([]string{
		"bash                        <   bash-5.2.15",
		"                                >   bash-5.2.26",
		"curl                        <   curl-8.9.1",
		"                                >   curl-8.14.1",
		"nginx                       <   nginx-1.26.0",
		"                                >   nginx-1.27.0",
	}, "\n") + "\n"}
	runner.replace(moved)

	rec := c.Apply(context.Background(), ApplyRequest{Plan: plan, Phrase: plan.RequiredConfirmPhrase()})
	if rec.Outcome != ApplyRefused {
		t.Fatalf("Outcome = %q, want %q", rec.Outcome, ApplyRefused)
	}
	if len(applier.calls()) != 0 {
		t.Fatal("a stale plan reached the privileged boundary")
	}
	if !strings.Contains(rec.Detail, "no longer matches") {
		t.Errorf("Detail = %q, want it to say the inventory changed", rec.Detail)
	}
	if !strings.Contains(rec.Detail, "regenerate the plan") {
		t.Errorf("Detail = %q, want it to tell the operator what to do next", rec.Detail)
	}
}

// A base system update is a reboot, not a package install. v1 refuses it
// rather than half-implementing the most dangerous part.
func TestApplyRefusesBaseSystemUpdate(t *testing.T) {
	c, _, applier := appliedCollector(t)

	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	plan := PlanFrom(inv, HostRisk{}, PlanOptions{})
	plan.Base = BaseActionUpdate
	plan.RebootRequired = true

	rec := c.Apply(context.Background(), ApplyRequest{Plan: plan, Phrase: plan.RequiredConfirmPhrase()})
	if rec.Outcome != ApplyRefused {
		t.Fatalf("Outcome = %q, want %q", rec.Outcome, ApplyRefused)
	}
	if len(applier.calls()) != 0 {
		t.Fatal("a base system update ran a package command")
	}
	if !strings.Contains(rec.Detail, "does not perform base system updates") {
		t.Errorf("Detail = %q, want it to name the base-system refusal", rec.Detail)
	}
}

// Every attempt is recorded, including every refusal. An audit that only
// keeps successes cannot answer "did anyone try".
func TestRecordsIncludeRefusals(t *testing.T) {
	c, _, _ := appliedCollector(t)

	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	plan := PlanFrom(inv, HostRisk{}, PlanOptions{})

	c.Apply(context.Background(), ApplyRequest{Plan: plan, Phrase: "wrong"})
	c.Apply(context.Background(), ApplyRequest{Plan: plan, Phrase: plan.RequiredConfirmPhrase()})
	c.Apply(context.Background(), ApplyRequest{Plan: plan, Phrase: "also wrong"})

	records := c.Records()
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3 - every attempt, refusals included", len(records))
	}
	for i, rec := range records {
		if rec.Seq != i+1 {
			t.Errorf("record %d has Seq %d, want %d", i, rec.Seq, i+1)
		}
		if rec.Detail == "" {
			t.Errorf("record %d has no detail", i)
		}
	}
	if records[0].Outcome != ApplyRefused || records[1].Outcome != ApplyApplied || records[2].Outcome != ApplyRefused {
		t.Errorf("outcomes = %q %q %q, want refused applied refused",
			records[0].Outcome, records[1].Outcome, records[2].Outcome)
	}
	// The phrase each attempt used is recorded, so "typed the wrong
	// thing" is distinguishable from "confirmed and it went wrong".
	if records[0].Phrase != "wrong" || records[1].Phrase != ConfirmPhrase {
		t.Errorf("phrases = %q %q, want them recorded verbatim", records[0].Phrase, records[1].Phrase)
	}
	// Records() hands back a copy; mutating it must not corrupt history.
	records[0].Outcome = ApplyApplied
	if c.Records()[0].Outcome != ApplyRefused {
		t.Error("Records() returned the internal slice rather than a copy")
	}
}

// The exact privileged command is a pure function of the package list,
// and this test is the audit of the whole package's blast radius.
func TestPkgUpgradeCommand(t *testing.T) {
	a := NewRunnerApplier()
	got, err := a.CommandFor([]string{"bash", "curl"})
	if err != nil {
		t.Fatalf("CommandFor() error: %v", err)
	}
	want := []string{"pkg", "upgrade", "-y", "--no-repo-update", "bash", "curl"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CommandFor() = %v, want %v", got, want)
	}
	// --no-repo-update is not decoration: it is what keeps a privileged
	// command from running `pkg update` behind the operator's back.
	if !containsString(got, "--no-repo-update") {
		t.Error("the upgrade command must refuse to refresh the catalogue itself")
	}
	if strings.Contains(strings.Join(got, " "), "autoremove") || containsString(got, "remove") {
		t.Error("the upgrade command must not carry a removal flag")
	}
}

// Nothing that is not a plain package name may ever reach the argv.
func TestPkgUpgradeCommandRefusesUnsafeNames(t *testing.T) {
	a := NewRunnerApplier()
	for _, name := range []string{"", "-rf", "bash;rm -rf /", "../etc/passwd", "a b", "a\nb", "a|b", ".", "a:b", "a\\b"} {
		got, err := a.CommandFor([]string{"bash", name})
		if err == nil {
			t.Errorf("CommandFor(%q) = %v, want an error", name, got)
		}
		if got != nil {
			t.Errorf("CommandFor(%q) returned argv %v alongside its error", name, got)
		}
	}
	if _, err := a.CommandFor(nil); !errors.Is(err, ErrNothingToDo) {
		t.Errorf("CommandFor(nil) error = %v, want ErrNothingToDo", err)
	}
}

// RunnerApplier re-validates at the privileged boundary rather than
// trusting Apply to have done it.
func TestRunnerApplierRevalidatesNames(t *testing.T) {
	var ran string
	a := &RunnerApplier{
		runner: runnerFunc(func(_ context.Context, name string, args ...string) (string, string, error) {
			ran = strings.TrimSpace(name + " " + strings.Join(args, " "))
			return "", "", nil
		}),
	}
	if _, err := a.Upgrade(context.Background(), []string{"bash", "--no-repo-update"}); err == nil {
		t.Error("Upgrade accepted a flag-shaped package name")
	}
	if ran != "" {
		t.Errorf("Upgrade ran %q despite refusing the name", ran)
	}
}

func TestRunnerApplierRecordsCommand(t *testing.T) {
	a := &RunnerApplier{
		runner: runnerFunc(func(context.Context, string, ...string) (string, string, error) {
			return "Updating...\n", "some warning\n", nil
		}),
	}
	res, err := a.Upgrade(context.Background(), []string{"bash"})
	if err != nil {
		t.Fatalf("Upgrade() error: %v", err)
	}
	if !reflect.DeepEqual(res.Command, []string{"pkg", "upgrade", "-y", "--no-repo-update", "bash"}) {
		t.Errorf("Command = %v, want the audited argv", res.Command)
	}
	if res.Stdout != "Updating...\n" || res.Stderr != "some warning\n" {
		t.Errorf("res = %+v, want the command's own output captured verbatim", res)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestRunnerApplierExitCodeOfNonExitError(t *testing.T) {
	if got := exitCodeOf(errors.New("could not start")); got != -1 {
		t.Errorf("exitCodeOf() = %d, want -1 for an error with no exit status", got)
	}
}

func TestOutputTailIsMarkedWhenTruncated(t *testing.T) {
	short := strings.Repeat("a", 10)
	if got := outputTail(short); got != short {
		t.Errorf("outputTail(short) = %q, want it unchanged", got)
	}
	long := strings.Repeat("b", outputTailLimit+500)
	got := outputTail(long)
	if len(got) >= len(long) {
		t.Error("outputTail did not truncate")
	}
	if !strings.Contains(got, "[truncated: 500 more bytes]") {
		t.Errorf("outputTail = %q, want the truncation stated explicitly - a silently trimmed log reads like a complete one", got)
	}
}

func TestShortFingerprint(t *testing.T) {
	if got := short("0123456789abcdef"); got != "0123456789ab" {
		t.Errorf("short() = %q, want 12 characters", got)
	}
	if got := short("abc"); got != "abc" {
		t.Errorf("short(short) = %q, want it unchanged", got)
	}
}

// runnerFunc adapts a function to Runner.
type runnerFunc func(ctx context.Context, name string, args ...string) (string, string, error)

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	return f(ctx, name, args...)
}

// applierFunc adapts a function to Applier.
type applierFunc func(ctx context.Context, packages []string) (UpgradeResult, error)

func (f applierFunc) Upgrade(ctx context.Context, packages []string) (UpgradeResult, error) {
	return f(ctx, packages)
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
