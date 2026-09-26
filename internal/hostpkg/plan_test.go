package hostpkg

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func planFor(t *testing.T, runner *fakeRunner, risk HostRisk, opts PlanOptions) Plan {
	t.Helper()
	inv, err := freshCollector(t, runner).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	return PlanFrom(inv, risk, opts)
}

func changeFor(p Plan, name string) (Change, bool) {
	for _, c := range p.Changes {
		if c.Package.Name == name {
			return c, true
		}
	}
	return Change{}, false
}

func TestPlanFromDescribesEveryPackage(t *testing.T) {
	plan := planFor(t, standardHost(), HostRisk{}, PlanOptions{})

	if plan.Base != BaseActionNone {
		t.Errorf("Base = %q, want %q", plan.Base, BaseActionNone)
	}
	if plan.RebootRequired {
		t.Error("RebootRequired = true, want false: no base update is available")
	}
	if plan.Safety.Gate != GateClear {
		t.Errorf("Safety.Gate = %q, want %q on a host with no known hazards", plan.Safety.Gate, GateClear)
	}
	if len(plan.Changes) != 3 {
		t.Fatalf("got %d changes, want one per installed package: %+v", len(plan.Changes), plan.Changes)
	}

	bash, _ := changeFor(plan, "bash")
	if bash.Action != ActionUpgrade || bash.TargetVersion != "5.2.26" {
		t.Errorf("bash = %+v, want an upgrade to 5.2.26", bash)
	}
	nginx, _ := changeFor(plan, "nginx")
	if nginx.Action != ActionNone {
		t.Errorf("nginx = %+v, want no action", nginx)
	}
	if nginx.Restart != RestartNone {
		t.Errorf("nginx Restart = %q, want %q: a positively-determined no-op needs no restart", nginx.Restart, RestartNone)
	}
	if !reflect.DeepEqual(plan.Actionable(), []string{"bash", "curl"}) {
		t.Errorf("Actionable() = %v, want [bash curl]", plan.Actionable())
	}
	if got := plan.Summary(); got != "2 upgrades" {
		t.Errorf("Summary() = %q, want %q", got, "2 upgrades")
	}
}

// A package whose update state is unknown is a visible, explained
// question in the plan - never a silent absence, and never a "no action
// needed" verdict.
func TestPlanKeepsUnknownPackagesVisible(t *testing.T) {
	runner := newFakeRunner().
		on("uname -r", "15.0-RELEASE-p4\n").
		on("uname -v", "15.0-RELEASE-p4\n").
		on("pkg version", "FreeBSD 15.0-RELEASE-p4 [repo FreeBSD]: up to date\n").
		on("pkg query -e %n|%v|%o %n-%v", "bash|5.2.26|shells/bash\n").
		on("pkg outdated", "bash has updates available, ask again later\n")

	plan := planFor(t, runner, HostRisk{}, PlanOptions{})
	bash, ok := changeFor(plan, "bash")
	if !ok {
		t.Fatal("bash missing from the plan")
	}
	if bash.Action != ActionNone {
		t.Errorf("bash Action = %q, want %q", bash.Action, ActionNone)
	}
	if !strings.Contains(bash.Reason, "unknown") {
		t.Errorf("bash Reason = %q, want it to say the status is unknown", bash.Reason)
	}
	if bash.TargetVersion != "" {
		t.Errorf("bash TargetVersion = %q, want empty for a package with no known target", bash.TargetVersion)
	}
	if len(plan.Warnings) == 0 {
		t.Error("a plan built on a partial inventory must warn about it")
	}
	found := false
	for _, w := range plan.Warnings {
		if strings.Contains(w, "could not be determined") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one about undetermined facts", plan.Warnings)
	}
}

// The restart requirement defaults to unknown, never to none. Getting
// this wrong is how an operator restarts a host's package set and
// discovers afterwards that a service needed a restart too.
func TestPlanRestartDefaultsToUnknown(t *testing.T) {
	plan := planFor(t, standardHost(), HostRisk{}, PlanOptions{})
	for _, name := range []string{"bash", "curl"} {
		change, _ := changeFor(plan, name)
		if change.Restart != RestartUnknown {
			t.Errorf("%s Restart = %q, want %q for a package no policy names", name, change.Restart, RestartUnknown)
		}
	}
}

func TestPlanRestartPolicyIsHonoured(t *testing.T) {
	plan := planFor(t, standardHost(), HostRisk{}, PlanOptions{
		Restart: RestartPolicy{"bash": "the bash rc script must be re-sourced"},
	})
	bash, _ := changeFor(plan, "bash")
	if bash.Restart != RestartRequired {
		t.Errorf("bash Restart = %q, want %q", bash.Restart, RestartRequired)
	}
}

// A base system update is a reboot, and that is a definitive statement
// rather than a policy-dependent guess - unlike a per-package restart.
func TestPlanBaseUpdateRequiresReboot(t *testing.T) {
	runner := newFakeRunner().
		on("uname -r", "15.0-RELEASE-p4\n").
		on("uname -v", "15.0-RELEASE-p4\n").
		on("pkg version", "FreeBSD 15.0-RELEASE-p4 [repo FreeBSD]: has updates available\n").
		on("pkg query -e %n|%v|%o %n-%v", "bash|5.2.26|shells/bash\n").
		on("pkg outdated", "")

	plan := planFor(t, runner, HostRisk{}, PlanOptions{})
	if plan.Base != BaseActionUpdate {
		t.Fatalf("Base = %q, want %q", plan.Base, BaseActionUpdate)
	}
	if !plan.RebootRequired {
		t.Error("RebootRequired = false, want true: a base system update needs a reboot")
	}
	if !strings.Contains(plan.BaseReason, "base system update is available") {
		t.Errorf("BaseReason = %q, want it to quote the observed evidence", plan.BaseReason)
	}
	if got := plan.Summary(); !strings.Contains(got, "base system update") || !strings.Contains(got, "reboot") {
		t.Errorf("Summary() = %q, want it to name the base update and the reboot", got)
	}
}

func TestPlanUnknownBaseIsNotNoChange(t *testing.T) {
	runner := newFakeRunner().
		on("uname -r", "15.0-RELEASE-p4\n").
		on("uname -v", "15.0-RELEASE-p4\n").
		onErr("pkg version", "pkg: command not found", errNotFound).
		on("pkg query -e %n|%v|%o %n-%v", "bash|5.2.26|shells/bash\n").
		on("pkg outdated", "")

	plan := planFor(t, runner, HostRisk{}, PlanOptions{})
	if plan.Base != BaseActionUnknown {
		t.Errorf("Base = %q, want %q: 'nothing to do' and 'cannot tell' must not share a value", plan.Base, BaseActionUnknown)
	}
	if plan.RebootRequired {
		t.Error("RebootRequired = true, want false: nothing was observed to require a reboot")
	}
	if plan.BaseReason == "" {
		t.Error("BaseReason must explain an unknown base")
	}
	if !reflect.DeepEqual(plan.Actionable(), []string(nil)) && len(plan.Actionable()) != 0 {
		t.Errorf("Actionable() = %v, want empty", plan.Actionable())
	}
}

func TestPlanReinstallIsOnlyEverRequested(t *testing.T) {
	plan := planFor(t, standardHost(), HostRisk{}, PlanOptions{
		Reinstall: ReinstallPolicy{"nginx": "rebuilt locally against a patched option set"},
	})
	nginx, _ := changeFor(plan, "nginx")
	if nginx.Action != ActionReinstall {
		t.Fatalf("nginx Action = %q, want %q", nginx.Action, ActionReinstall)
	}
	if nginx.TargetVersion != "1.26.0" {
		t.Errorf("nginx TargetVersion = %q, want its own current version for a reinstall", nginx.TargetVersion)
	}
	if nginx.Reason != "rebuilt locally against a patched option set" {
		t.Errorf("nginx Reason = %q, want the caller's stated reason verbatim", nginx.Reason)
	}
	if !reflect.DeepEqual(plan.Actionable(), []string{"bash", "curl", "nginx"}) {
		t.Errorf("Actionable() = %v, want a reinstall to be actionable", plan.Actionable())
	}
	// A reinstall of a package that is ALSO outdated is an upgrade, not
	// a reinstall: the operator's real intent is the newer version.
	both := planFor(t, standardHost(), HostRisk{}, PlanOptions{Reinstall: ReinstallPolicy{"bash": "please rebuild"}})
	bash, _ := changeFor(both, "bash")
	if bash.Action != ActionUpgrade || bash.TargetVersion != "5.2.26" {
		t.Errorf("bash = %+v, want the upgrade to win over the reinstall request", bash)
	}
}

func TestPlanHazardGating(t *testing.T) {
	cases := map[string]struct {
		risk       HostRisk
		wantGate   Gate
		wantHazard Hazard
		wantPhrase string
	}{
		"no hazards": {
			risk:       HostRisk{},
			wantGate:   GateClear,
			wantPhrase: ConfirmPhrase,
		},
		"HAST primary": {
			risk:       HostRisk{HASTRole: "primary", HASTResources: []string{"vm-abc"}},
			wantGate:   GateCaution,
			wantHazard: HazardHAST,
			wantPhrase: ConfirmHazardPhrase,
		},
		"HAST secondary": {
			risk:       HostRisk{HASTRole: "secondary"},
			wantGate:   GateCaution,
			wantHazard: HazardHAST,
			wantPhrase: ConfirmHazardPhrase,
		},
		"ZFS pools": {
			risk:       HostRisk{ZFSImportedPools: []string{"tank", "zroot"}},
			wantGate:   GateCaution,
			wantHazard: HazardZFS,
			wantPhrase: ConfirmHazardPhrase,
		},
		"raft voter": {
			risk:       HostRisk{RaftVoter: true},
			wantGate:   GateCaution,
			wantHazard: HazardRaftVoter,
			wantPhrase: ConfirmHazardPhrase,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			plan := planFor(t, standardHost(), tc.risk, PlanOptions{})
			if plan.Safety.Gate != tc.wantGate {
				t.Errorf("Gate = %q, want %q", plan.Safety.Gate, tc.wantGate)
			}
			if tc.wantHazard != "" && !containsHazard(plan.Safety.Hazards, tc.wantHazard) {
				t.Errorf("Hazards = %v, want it to include %q", plan.Safety.Hazards, tc.wantHazard)
			}
			if got := plan.RequiredConfirmPhrase(); got != tc.wantPhrase {
				t.Errorf("RequiredConfirmPhrase() = %q, want %q", got, tc.wantPhrase)
			}
		})
	}
}

// HAST role "init" is a real pkg/hastd state that is not a live role. It
// is named rather than treated as safe.
func TestPlanUnrecognisedHASTRoleIsNotTreatedAsSafe(t *testing.T) {
	plan := planFor(t, standardHost(), HostRisk{HASTRole: "init"}, PlanOptions{})
	if plan.Safety.Gate != GateClear && plan.Safety.Gate != GateCaution {
		t.Fatalf("Gate = %q", plan.Safety.Gate)
	}
	found := false
	for _, note := range plan.Safety.Notes {
		if strings.Contains(note, "init") && strings.Contains(note, "unestablished") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want one naming role \"init\" as unestablished", plan.Safety.Notes)
	}
}

// A fact the caller could not establish is a caution, not a clean bill
// of health. "We could not check" is never "checked and fine" - the
// same rule internal/cluster/simulate.go applies to reachability.
func TestPlanUnestablishedFactsForceCaution(t *testing.T) {
	plan := planFor(t, standardHost(), HostRisk{Unknowns: []string{"HAST status could not be read"}}, PlanOptions{})
	if plan.Safety.Gate != GateCaution {
		t.Errorf("Gate = %q, want %q", plan.Safety.Gate, GateCaution)
	}
	if plan.RequiredConfirmPhrase() != ConfirmHazardPhrase {
		t.Errorf("RequiredConfirmPhrase() = %q, want %q", plan.RequiredConfirmPhrase(), ConfirmHazardPhrase)
	}
	joined := strings.Join(plan.Safety.Notes, " ")
	if !strings.Contains(joined, "HAST status could not be read") {
		t.Errorf("notes = %v, want the unestablished fact named", plan.Safety.Notes)
	}
}

// Every plan, whatever it found, states v1's own limits.
func TestPlanAlwaysStatesVersionLimits(t *testing.T) {
	plan := planFor(t, standardHost(), HostRisk{}, PlanOptions{})
	joined := strings.Join(plan.Warnings, " ")
	for _, want := range []string{"never removes a package", "never runs `pkg update`", "no scheduled or unattended upgrade path"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings = %v, want one mentioning %q", plan.Warnings, want)
		}
	}
}

func TestPlanFingerprintIsCarried(t *testing.T) {
	c := freshCollector(t, standardHost())
	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	plan := PlanFrom(inv, HostRisk{}, PlanOptions{})
	if plan.InventoryFingerprint != inv.Fingerprint() {
		t.Errorf("InventoryFingerprint = %q, want the inventory's own %q", plan.InventoryFingerprint, inv.Fingerprint())
	}
	if !plan.CreatedAt.Equal(inv.ObservedAt) {
		t.Errorf("CreatedAt = %s, want the inventory's ObservedAt %s", plan.CreatedAt, inv.ObservedAt)
	}
}

// A long HAST resource list is counted rather than dumped, so a note
// stays readable in a table cell.
func TestPlanSafetyNoteSummarisesLongLists(t *testing.T) {
	pools := make([]string, 0, 6)
	for i := range 6 {
		pools = append(pools, string(rune('a'+i))+"-pool")
	}
	plan := planFor(t, standardHost(), HostRisk{ZFSImportedPools: pools}, PlanOptions{})
	joined := strings.Join(plan.Safety.Notes, " ")
	if !strings.Contains(joined, "6 of them") || !strings.Contains(joined, "and 4 more") {
		t.Errorf("notes = %v, want a counted summary of the pool list", plan.Safety.Notes)
	}
}

func TestPlanHasRemoval(t *testing.T) {
	if (Plan{}).HasRemoval() {
		t.Error("an empty plan reports a removal")
	}
	withRemoval := Plan{Changes: []Change{{Action: ActionRemove}}}
	if !withRemoval.HasRemoval() {
		t.Error("HasRemoval() = false, want true")
	}
}

func TestPlanWithNoChanges(t *testing.T) {
	runner := newFakeRunner().
		on("uname -r", "15.0-RELEASE-p4\n").
		on("uname -v", "15.0-RELEASE-p4\n").
		on("pkg version", "FreeBSD 15.0-RELEASE-p4 [repo FreeBSD]: up to date\n").
		on("pkg query -e %n|%v|%o %n-%v", "bash|5.2.26|shells/bash\n").
		on("pkg outdated", "")

	plan := planFor(t, runner, HostRisk{}, PlanOptions{})
	if len(plan.Actionable()) != 0 {
		t.Errorf("Actionable() = %v, want empty", plan.Actionable())
	}
	if got := plan.Summary(); got != "no known changes" {
		t.Errorf("Summary() = %q, want %q", got, "no known changes")
	}
	if plan.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set even for an empty plan")
	}
}

func TestPlanTimestampComesFromTheInventory(t *testing.T) {
	inv := Inventory{ObservedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	if got := PlanFrom(inv, HostRisk{}, PlanOptions{}).CreatedAt; !got.Equal(inv.ObservedAt) {
		t.Errorf("CreatedAt = %s, want %s", got, inv.ObservedAt)
	}
}

func containsHazard(hazards []Hazard, want Hazard) bool {
	for _, h := range hazards {
		if h == want {
			return true
		}
	}
	return false
}
