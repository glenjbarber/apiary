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

// The -advice rendering: the narrow form `make update` prints, over the
// same read and the same four services. What is testable here is the
// half that is this command's own and would rot silently - the state
// word for every status the gate can produce, the rule that no evidence
// never renders as health, and the columns holding.

// TestAdviceStateCoversEveryGateStatus is the exhaustiveness check for
// the second rendering, and it is the same guard the verdict table above
// has. A status added to the gate with no state word would fall through
// adviceState's catch-all to "unknown", which reports a new answer as "I
// could not tell" - a lie, and the same lie in the same direction, which
// is why it is pinned in both places.
func TestAdviceStateCoversEveryGateStatus(t *testing.T) {
	cases := []struct {
		service buildgate.Service
		want    string
	}{
		{buildgate.Service{Status: buildgate.TookEffect}, "current"},
		{buildgate.Service{Status: buildgate.DirtyIDMatch}, "unverified"},
		{buildgate.Service{Status: buildgate.RunningStale}, "stale"},
		{buildgate.Service{Status: buildgate.NotRunning}, "not running"},
		{buildgate.Service{Status: buildgate.Unobserved, Reason: buildgate.ReasonBinaryPredatesVersionFlag}, "unknown"},
		{buildgate.Service{Status: buildgate.Unobserved, Reason: buildgate.ReasonBinaryUnstamped}, "unknown"},
		{buildgate.Service{Status: buildgate.Unobserved, Reason: buildgate.ReasonBinaryMissing}, "unknown"},
		{buildgate.Service{Status: buildgate.Unobserved, Reason: buildgate.ReasonLogPredatesStamping}, "unknown"},
		{buildgate.Service{Status: buildgate.Unobserved, Reason: buildgate.ReasonLogUnreadable}, "unknown"},
		{buildgate.Service{Status: buildgate.Unobserved, Reason: buildgate.ReasonBinaryUnreadable}, "unknown"},
		{buildgate.Service{Status: buildgate.Unobserved}, "unknown"},
		{buildgate.Service{Status: buildgate.NotRunning, Reason: buildgate.ReasonLogMissing}, "not running"},
		{buildgate.Service{Status: buildgate.DirtyIDMatch, Reason: buildgate.ReasonDirtyBuild}, "unverified"},
	}
	for _, c := range cases {
		if got := adviceState(c.service); got != c.want {
			t.Errorf("adviceState(%+v) = %q, want %q", c.service, got, c.want)
		}
	}

	// Every status and every reason the gate can produce has to appear
	// above, or a new one is absorbed by the catch-all with nothing to
	// notice.
	seenStatus := map[buildgate.Status]bool{}
	seenReason := map[buildgate.Reason]bool{}
	for _, c := range cases {
		seenStatus[c.service.Status] = true
		seenReason[c.service.Reason] = true
	}
	for _, status := range []buildgate.Status{
		buildgate.TookEffect, buildgate.DirtyIDMatch, buildgate.RunningStale,
		buildgate.Unobserved, buildgate.NotRunning,
	} {
		if !seenStatus[status] {
			t.Errorf("status %q has no state word, so it renders as whatever the catch-all says", status)
		}
	}
	for _, reason := range []buildgate.Reason{
		buildgate.ReasonNone, buildgate.ReasonDirtyBuild, buildgate.ReasonLogMissing,
		buildgate.ReasonLogUnreadable, buildgate.ReasonLogPredatesStamping,
		buildgate.ReasonBinaryMissing, buildgate.ReasonBinaryUnreadable,
		buildgate.ReasonBinaryPredatesVersionFlag, buildgate.ReasonBinaryUnstamped,
	} {
		if !seenReason[reason] {
			t.Errorf("reason %q is not covered by the state table, so its rendering is unpinned", reason)
		}
	}
}

// TestOnlyTookEffectIsCurrent is the one claim in the advice rendering
// an operator can act on by doing nothing, so it is the one that must
// have exactly one source. Unobserved in particular is the status this
// project keeps insisting is neither health nor failure, and a report
// that put it in the confirmed column would undo that at the one moment
// somebody is reading a deploy's output.
func TestOnlyTookEffectIsCurrent(t *testing.T) {
	for _, status := range []buildgate.Status{
		buildgate.DirtyIDMatch, buildgate.RunningStale, buildgate.NotRunning, buildgate.Unobserved,
	} {
		service := buildgate.Service{Status: status, Reason: buildgate.ReasonLogMissing}
		if got := adviceState(service); got == "current" {
			t.Errorf("status %q renders as %q; only TookEffect may", status, got)
		}
	}
	if got := adviceState(buildgate.Service{Status: buildgate.TookEffect}); got != "current" {
		t.Errorf("TookEffect renders as %q, want %q", got, "current")
	}
}

// TestAdviceStatesAreDistinct guards two state words collapsing into one
// string, which would make the report say the same thing about two
// different states.
func TestAdviceStatesAreDistinct(t *testing.T) {
	all := map[string]bool{}
	for _, s := range []buildgate.Status{
		buildgate.TookEffect, buildgate.DirtyIDMatch, buildgate.RunningStale,
		buildgate.NotRunning, buildgate.Unobserved,
	} {
		word := adviceState(buildgate.Service{Status: s})
		if all[word] {
			t.Errorf("two statuses render as %q", word)
		}
		all[word] = true
		if word == "" {
			t.Error("a state word is the empty string, which renders as a blank column")
		}
	}
}

// TestNoEvidenceIsNeverReportedAsHealthyOrFailed is the contract from the
// other end: not only does an unobserved daemon get a state word that is
// not "current", the sentence beside it says in words that nothing was
// established. Silence in a deploy report reads as good news precisely
// because the two daemons above it took the new build, so the sentence
// has to make the gap explicit rather than leave it to the reader.
func TestNoEvidenceIsNeverReportedAsHealthyOrFailed(t *testing.T) {
	for _, reason := range []buildgate.Reason{
		buildgate.ReasonNone, buildgate.ReasonLogMissing, buildgate.ReasonLogUnreadable,
		buildgate.ReasonLogPredatesStamping, buildgate.ReasonBinaryMissing,
		buildgate.ReasonBinaryUnreadable, buildgate.ReasonBinaryPredatesVersionFlag,
		buildgate.ReasonBinaryUnstamped,
	} {
		service := buildgate.Service{
			Name: "raftd", Status: buildgate.Unobserved, Reason: reason,
		}
		sentence := adviceSentence(service)
		if !strings.Contains(sentence, "neither confirmed nor failed") {
			t.Errorf("unobserved (%s) renders as %q, which does not say what no evidence means", reason, sentence)
		}
		if strings.Contains(sentence, "running the build on disk") {
			t.Errorf("unobserved (%s) renders as %q, which claims the daemon is on the disk build", reason, sentence)
		}
		// The reason is what makes the row actionable - an absent binary
		// and an unreadable log want opposite things done - so it travels
		// with the row rather than being left in the gate.
		if reason != buildgate.ReasonNone && !strings.Contains(sentence, string(reason)) {
			t.Errorf("unobserved (%s) renders as %q, which drops the reason the operator needs", reason, sentence)
		}
	}
	// A hand-built Service with no reason must not render a sentence that
	// opens with a semicolon; the gate always sets one, and this is the
	// line that keeps a hand-built one from shipping that way.
	if s := adviceSentence(buildgate.Service{Status: buildgate.Unobserved}); !strings.Contains(s, "no evidence") {
		t.Errorf("an unobserved row with no reason renders as %q, want the fallback spelled out", s)
	}
}

// TestNotRunningIsNeitherCurrentNorStale: a daemon with no process is not
// on the new build, and it is also not running the old one. Collapsing
// it into "stale" would send an operator to force-restart a Comb whose
// raftd is simply down, which is a different act with a different blast
// radius.
func TestNotRunningIsNeitherCurrentNorStale(t *testing.T) {
	service := buildgate.Service{
		Name: "raftd", Status: buildgate.NotRunning, Reason: buildgate.ReasonLogMissing,
		OnDisk: "107fdf6b414a",
	}
	if got := adviceState(service); got != "not running" {
		t.Errorf("adviceState() = %q, want %q", got, "not running")
	}
	sentence := adviceSentence(service)
	for _, want := range []string{"no process", string(buildgate.ReasonLogMissing)} {
		if !strings.Contains(sentence, want) {
			t.Errorf("adviceSentence() = %q, want it to mention %q", sentence, want)
		}
	}
	if strings.Contains(sentence, "installed but not restarted") {
		t.Errorf("adviceSentence() = %q, which describes a daemon that is not running at all", sentence)
	}
}

// TestAdviceCarriesTheIdsTheVerdictWasMadeFrom pins the one thing a
// reader cannot reconstruct for themselves. "stale" says that two builds
// disagree; which two is the whole content of the report, and it is read
// once, in Confirm, so it is printed from the Service rather than read
// again.
func TestAdviceCarriesTheIdsTheVerdictWasMadeFrom(t *testing.T) {
	service := buildgate.Service{
		Name: "managerd", Status: buildgate.RunningStale,
		Running: "a9879963600c", OnDisk: "107fdf6b414a",
	}
	sentence := adviceSentence(service)
	for _, want := range []string{service.Running, service.OnDisk, "installed but not restarted"} {
		if !strings.Contains(sentence, want) {
			t.Errorf("adviceSentence() = %q, want it to mention %q", sentence, want)
		}
	}
}

// TestAdviceDirtyRowNamesTheByteGap: a -dirty pair is agreement about
// the commit and nothing else, and the row has to say so. A row that
// merely read "unverified" without the reason would be a different
// string rather than a more honest answer.
func TestAdviceDirtyRowNamesTheByteGap(t *testing.T) {
	service := buildgate.Service{
		Name: "frontend", Status: buildgate.DirtyIDMatch,
		Running: "9c43262d358a-dirty", OnDisk: "9c43262d358a-dirty",
	}
	for _, want := range []string{"dirty", "sha256", service.Running, service.OnDisk} {
		if !strings.Contains(adviceSentence(service), want) {
			t.Errorf("adviceSentence() = %q, want it to mention %q", adviceSentence(service), want)
		}
	}
}

// TestAdviceColumnsHoldForEveryState is the row format, which is the
// part of a report an operator skims rather than parses. Every state word
// has to leave the sentence at the same offset, including "not running",
// which is exactly as wide as the column - a word one over would push
// the whole report's third column out by one and make the two blocks
// this file prints look like different tools.
func TestAdviceColumnsHoldForEveryState(t *testing.T) {
	states := map[buildgate.Status]bool{}
	for _, s := range []buildgate.Status{
		buildgate.TookEffect, buildgate.DirtyIDMatch, buildgate.RunningStale,
		buildgate.NotRunning, buildgate.Unobserved,
	} {
		states[s] = true
	}
	for status := range states {
		service := buildgate.Service{Name: "restshimd", Status: status}
		row := fmt.Sprintf("  %-10s %-11s %s", service.Name, adviceState(service), adviceSentence(service))
		if want := "  restshimd  "; !strings.HasPrefix(row, want) {
			t.Errorf("row = %q, want it to start with a two-space indent and a 10-wide name column", row)
		}
		if at := strings.Index(row, adviceSentence(service)); at != 25 {
			t.Errorf("row = %q, sentence starts at %d, want 25 for every state", row, at)
		}
	}
	// The widest state word fills its column exactly rather than
	// overflowing it, which is asserted so that a longer word added later
	// cannot quietly break the alignment the line above just checked.
	if got := adviceState(buildgate.Service{Status: buildgate.NotRunning}); len(got) != 11 {
		t.Errorf("the longest state word is %d wide (%q), so the 11-wide column this test assumes is wrong", len(got), got)
	}
}

// TestTheTwoRenderingsAgreeOnWhatNeedsWork is the anti-drift guard
// between the two reports this command prints. The full report's verdict
// column and the advice form's state word are separate switches over the
// same Status, and the exit status - which `make update` reads as "the
// report ran" - is keyed on the Status a third time. Nothing stops the
// three from disagreeing except this.
func TestTheTwoRenderingsAgreeOnWhatNeedsWork(t *testing.T) {
	for _, status := range []buildgate.Status{
		buildgate.TookEffect, buildgate.DirtyIDMatch, buildgate.RunningStale,
		buildgate.NotRunning, buildgate.Unobserved,
	} {
		service := buildgate.Service{Status: status}
		stale := adviceState(service) == "stale"
		if differ := versioncheckOf(service) == differ; stale != differ {
			t.Errorf("status %q: -advice says stale=%v, the report says DIFFERENT BUILD=%v", status, stale, differ)
		}
		// TookEffect is the only status the exit status counts, and the
		// only one a gate Confirms().
		if confirms := service.Status.Confirms(); (adviceState(service) == "current") != confirms {
			t.Errorf("status %q: -advice says current=%v, the gate confirms=%v", status, adviceState(service) == "current", confirms)
		}
	}
}
