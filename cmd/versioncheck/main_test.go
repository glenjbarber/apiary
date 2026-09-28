package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/buildgate"
)

// The comparison this command used to own now lives in
// internal/buildgate, and is tested there against a fake Comb's real
// files and real executables. What is left to test HERE is the half that
// is this command's own and would break silently if the mapping from a
// gate answer to a report line rotted: the verdict vocabulary, the
// detail column, the column widths, and the rule that only a genuine
// mismatch exits non-zero.

// The sample log lines and the parsing rules that read them moved to
// internal/buildgate with the comparison itself, and are tested there
// against a fake Comb's real files. What is left here is the half that is
// this command's own: the report it prints.

// TestServicesAreTheGates is the anti-drift guard for the one piece of
// this command's state that used to be its own: the list of daemons. A
// copy here could fall behind the gate's, and the symptom would be a tool
// that reports a Comb confirmed while never looking at the daemon most
// likely to be stale.
func TestServicesAreTheGates(t *testing.T) {
	if len(services) != len(buildgate.Services) {
		t.Fatalf("versioncheck checks %d services, the gate checks %d", len(services), len(buildgate.Services))
	}
	for i, name := range services {
		if buildgate.Services[i] != name {
			t.Errorf("services[%d] = %q, the gate's is %q", i, name, buildgate.Services[i])
		}
	}
}

// TestVerdictMappingCoversEveryGateStatus is the exhaustiveness check. A
// status added to the gate with no mapping here would fall through to
// the catch-all "unknown" and this command would report a new answer as
// "I could not tell", which is a lie in the fail-open direction.
func TestVerdictMappingCoversEveryGateStatus(t *testing.T) {
	// Each status, and every reason that can accompany an unobserved one.
	cases := []struct {
		service buildgate.Service
		want    verdict
	}{
		{buildgate.Service{Status: buildgate.TookEffect}, agree},
		{buildgate.Service{Status: buildgate.DirtyIDMatch}, sameDirty},
		{buildgate.Service{Status: buildgate.RunningStale}, differ},
		{buildgate.Service{Status: buildgate.NotRunning}, notRunning},
		{buildgate.Service{Status: buildgate.Unobserved, Reason: buildgate.ReasonBinaryPredatesVersionFlag}, predatesFlag},
		{buildgate.Service{Status: buildgate.Unobserved, Reason: buildgate.ReasonBinaryUnstamped}, unstamped},
		{buildgate.Service{Status: buildgate.Unobserved, Reason: buildgate.ReasonBinaryMissing}, missing},
		{buildgate.Service{Status: buildgate.Unobserved, Reason: buildgate.ReasonLogPredatesStamping}, unknown},
		{buildgate.Service{Status: buildgate.Unobserved, Reason: buildgate.ReasonLogUnreadable}, unknown},
		{buildgate.Service{Status: buildgate.Unobserved, Reason: buildgate.ReasonBinaryUnreadable}, unknown},
		{buildgate.Service{Status: buildgate.Unobserved, Reason: buildgate.ReasonNone}, unknown},
		// These two only ever accompany a status the table already maps,
		// and they are listed so that stays true: if a future change let
		// them ride on Unobserved, the switch would quietly fold them
		// into the catch-all and this row would fail.
		{buildgate.Service{Status: buildgate.NotRunning, Reason: buildgate.ReasonLogMissing}, notRunning},
		{buildgate.Service{Status: buildgate.DirtyIDMatch, Reason: buildgate.ReasonDirtyBuild}, sameDirty},
	}
	for _, c := range cases {
		if got := versioncheckOf(c.service); got != c.want {
			t.Errorf("versioncheckOf(%+v) = %q, want %q", c.service, got, c.want)
		}
	}

	// Every reason the gate can produce must have been exercised above,
	// or a new one would be silently absorbed by the catch-all.
	seen := map[buildgate.Reason]bool{}
	for _, c := range cases {
		seen[c.service.Reason] = true
	}
	for _, reason := range []buildgate.Reason{
		buildgate.ReasonNone, buildgate.ReasonDirtyBuild, buildgate.ReasonLogMissing,
		buildgate.ReasonLogUnreadable, buildgate.ReasonLogPredatesStamping,
		buildgate.ReasonBinaryMissing, buildgate.ReasonBinaryUnreadable,
		buildgate.ReasonBinaryPredatesVersionFlag, buildgate.ReasonBinaryUnstamped,
	} {
		if !seen[reason] {
			t.Errorf("reason %q is not covered by the mapping table, so its verdict is unpinned", reason)
		}
	}
	// And every status the gate can produce.
	seenStatus := map[buildgate.Status]bool{}
	for _, c := range cases {
		seenStatus[c.service.Status] = true
	}
	for _, status := range []buildgate.Status{
		buildgate.TookEffect, buildgate.DirtyIDMatch, buildgate.RunningStale,
		buildgate.Unobserved, buildgate.NotRunning,
	} {
		if !seenStatus[status] {
			t.Errorf("status %q is not covered by the mapping table, so its verdict is unpinned", status)
		}
	}
}

// TestOnlyAMismatchIsDIFFERENTBUILD is the exit-status rule, stated as a
// table so it cannot rot: the command exits 1 on a genuine mixed state
// and 0 on everything else, INCLUDING every "cannot tell". An
// inconclusive check must never fail a deploy by accident, and equally an
// inconclusive check must never pass one silently - which is why the gate
// package, not this exit code, is what a controller uses.
func TestOnlyAMismatchIsDIFFERENTBUILD(t *testing.T) {
	for _, status := range []buildgate.Status{
		buildgate.TookEffect, buildgate.DirtyIDMatch, buildgate.Unobserved, buildgate.NotRunning,
	} {
		service := buildgate.Service{Status: status}
		if got := versioncheckOf(service); got == differ {
			t.Errorf("status %q maps to DIFFERENT BUILD, want only RunningStale to", status)
		}
	}
	if got := versioncheckOf(buildgate.Service{Status: buildgate.RunningStale}); got != differ {
		t.Errorf("RunningStale maps to %q, want %q", got, differ)
	}
}

// TestVerdictsAreDistinctValues guards against two verdicts collapsing to
// the same string, which would make the report silently lie.
func TestVerdictsAreDistinctValues(t *testing.T) {
	all := map[verdict]bool{}
	for _, v := range []verdict{
		agree, differ, unknown, notRunning, unstamped, noStampLine, missing, predatesFlag, sameDirty,
	} {
		if all[v] {
			t.Errorf("duplicate verdict %q", v)
		}
		all[v] = true
		if v == "" {
			t.Error("a verdict has an empty string, which renders as a blank column")
		}
	}
}

// TestDetailNamesBothBuildsWhenTheyDiffer is the UX contract: when the
// verdict is DIFFERENT, the reader must see which is which without
// re-running the tool, and the detail column must say what to do about
// it.
func TestDetailNamesBothBuildsWhenTheyDiffer(t *testing.T) {
	service := buildgate.Service{
		Name: "raftd", Status: buildgate.RunningStale,
		OnDisk: "107fdf6b414a", Running: "a9879963600c",
	}
	d := detail(versioncheckOf(service), service)
	for _, want := range []string{"running", "on disk", "restart", "a9879963600c", "107fdf6b414a"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail() = %q, want it to mention %q", d, want)
		}
	}
	if d := detail(agree, buildgate.Service{Status: buildgate.TookEffect}); d != "" {
		t.Errorf("detail(agree) = %q, want empty", d)
	}
}

// TestDetailCarriesTheIdsTheVerdictWasMadeFrom pins the one substantive
// change in this file. The detail column used to be filled by a SECOND,
// independent read of the binary and the log, after the verdict had
// already been decided from a first read. Between a restart landing
// inside that window, the two disagreed and the report showed one build
// in its verdict and another in its evidence. The ids now travel with
// the verdict, so the two cannot.
func TestDetailCarriesTheIdsTheVerdictWasMadeFrom(t *testing.T) {
	service := buildgate.Service{
		Name: "raftd", Status: buildgate.RunningStale,
		OnDisk: "107fdf6b414a", Running: "a9879963600c",
	}
	d := detail(differ, service)
	if !strings.Contains(d, service.Running) {
		t.Errorf("detail() = %q, does not carry the Running id the verdict was made from (%q)", d, service.Running)
	}
	if !strings.Contains(d, service.OnDisk) {
		t.Errorf("detail() = %q, does not carry the OnDisk id the verdict was made from (%q)", d, service.OnDisk)
	}
}

// TestDirtyAgreementSaysWhyItIsWeaker is the other half of the -dirty
// contract: a downgraded verdict that does not say what is missing is
// just a different string, not a more honest answer.
func TestDirtyAgreementSaysWhyItIsWeaker(t *testing.T) {
	service := buildgate.Service{
		Name: "raftd", Status: buildgate.DirtyIDMatch,
		OnDisk: "9c43262d358a-dirty", Running: "9c43262d358a-dirty",
	}
	if got := versioncheckOf(service); got != sameDirty {
		t.Fatalf("versioncheckOf() = %q, want %q", got, sameDirty)
	}
	d := detail(sameDirty, service)
	for _, want := range []string{"dirty", "sha256"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail(sameDirty) = %q, want it to mention %q", d, want)
		}
	}
}

// TestDetailIsEmptyForTheQuietVerdicts keeps -quiet meaningful. It skips
// rows whose verdict is `agree`, so anything that prints a detail for
// `agree` would show a line with an explanation of nothing.
func TestDetailIsEmptyForTheQuietVerdicts(t *testing.T) {
	if d := detail(agree, buildgate.Service{Status: buildgate.TookEffect}); d != "" {
		t.Errorf("detail(agree) = %q, want empty", d)
	}
	if d := detail(notRunning, buildgate.Service{Status: buildgate.NotRunning}); d != "" {
		t.Errorf("detail(notRunning) = %q, want empty", d)
	}
}

// TestReportColumnsHoldForEveryVerdict is the last piece of the UX that
// belongs to this command rather than to the library: the row format. A
// verdict longer than the column would push the detail off its own line
// and turn a report an operator skims into one they have to parse, so the
// width is checked against the longest verdict that can actually occur.
func TestReportColumnsHoldForEveryVerdict(t *testing.T) {
	longest := verdict("")
	for _, v := range []verdict{
		agree, differ, unknown, notRunning, missing, predatesFlag, unstamped, noStampLine, sameDirty,
	} {
		if len(v) > len(longest) {
			longest = v
		}
	}
	row := fmt.Sprintf("    %-10s %-38s %s", "restshimd", longest, "some evidence")
	if !strings.Contains(row, "restshimd  ") {
		t.Errorf("row = %q, want the service name in a padded 10-wide column", row)
	}
	// The whole reason the detail is a separate column: an unexplained
	// row is the one thing an operator cannot act on, and it has to fit
	// beside the verdict rather than replace it.
	if !strings.HasSuffix(row, "some evidence") {
		t.Errorf("row = %q, want the detail last", row)
	}

	// Alignment is checked over the verdicts that FIT the 38-wide column,
	// because for those the detail column starts at a fixed offset and
	// the report is skimmable.
	//
	// Two verdict strings are wider than 38 and were before the
	// extraction - noStampLine at 41 and sameDirty at 44 - so their
	// detail starts a few characters further right. This is recorded
	// rather than quietly repainted: fixing it means either changing a
	// verdict string this tool has always printed or changing the column
	// width, and both are a change to the report an operator is used to.
	// The overflow is cosmetic, not a lie, and it is asserted here so
	// that a future widening cannot happen unnoticed.
	detailColumn := -1
	for _, v := range []verdict{agree, differ, unknown, notRunning, missing, predatesFlag, unstamped} {
		row := fmt.Sprintf("    %-10s %-38s %s", "raftd", v, "some evidence")
		at := strings.Index(row, "some evidence")
		if at < 0 {
			t.Fatalf("row = %q, want it to carry a detail", row)
		}
		if detailColumn == -1 {
			detailColumn = at
		} else if at != detailColumn {
			t.Errorf("row = %q, detail starts at offset %d, want %d for every verdict that fits the column",
				row, at, detailColumn)
		}
	}
	for _, v := range []verdict{noStampLine, sameDirty} {
		if len(v) <= 38 {
			t.Errorf("verdict %q is now %d wide, so the documented 38-wide overflow is gone and the comment above is stale",
				v, len(v))
		}
	}

}
