// Command apiaryinstall is Apiary's host preflight/provisioning tool - it
// runs before any of raftd/managerd/frontend/restshimd, checking (and,
// with -apply, fixing) the FreeBSD host prerequisites every ADR up to
// docs/adr/0082-apiary-installer-preflight.md has only ever documented in
// prose. See internal/install's package doc comment for the full design
// and its three-tier risk model.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/glenjbarber/apiary/internal/buildinfo"
	"github.com/glenjbarber/apiary/internal/install"
)

// networkChangeConfirmPhrase is the exact value -apply-network must be
// given to actually create/modify a bridge interface and attach the
// uplink NIC to it. Matches cmd/raftd's -reset/-restore precedent: the
// flag's own value IS the phrase, so a bare -apply-network with no value
// (or the wrong value) does nothing. ADR-0022 documents a live incident
// where this exact operation nearly cost the operator their own SSH
// session - the phrase gate exists specifically because of that.
const networkChangeConfirmPhrase = "yes-modify-network"

func main() {
	code, err := run(os.Args[1:], os.Stdout)
	if err != nil {
		// The flag package has already printed its own complaint and the
		// usage text on stderr, so this only adds the one-line summary.
		log.Print(err)
	}
	if code != 0 {
		os.Exit(code)
	}
}

const (
	// exitFailure is the process exit code run() reports when at least one
	// check did not come back ok (or merely informational). It is a named
	// constant rather than a bare 1 so a test can assert the exact
	// contract without hard-coding a literal that could drift.
	exitFailure = 1

	// exitUsage is the process exit code for a bad command line. It
	// matches what the flag package's own ExitOnError mode has always
	// produced for this command, so a caller that scripts around
	// apiaryinstall sees no change in behaviour.
	exitUsage = 2
)

// run is the command's whole body, made testable by taking its arguments
// and its output writer rather than reading os.Args and writing to
// process stdout directly.
//
// Everything it depends on that touches the host is injected: the checks
// come from checks, the command runner from runner, and the output goes
// to out. That is what lets cmd/apiaryinstall/main_test.go exercise the
// command end to end - flag handling, the network-confirmation gate, the
// apply risk gate, output formatting and the exit code - without running
// a single real command on the machine running the test.
//
// A nil runner means "use the real one". A nil checks slice means "use
// the full registry", because that is the production default and tests
// that do not care about a specific check want the real set.
func run(args []string, out io.Writer) (int, error) {
	return runWith(args, out, nil, nil)
}

// runWith is run() with every host-touching dependency supplied. runner
// nil selects install.NewExecRunner(); checks nil selects install.All().
//
// The FlagSet's own output (usage text and a bad-flag complaint) goes to
// stderr, where the flag package's default ExitOnError mode would have put
// it. The results - the table or the JSON - go to out, which is stdout in
// production and a buffer under test.
func runWith(args []string, out io.Writer, runner install.Runner, checks []install.Check) (int, error) {
	// A private FlagSet, not flag.CommandLine: the global flag set is
	// process-wide state that a test cannot reset, and the flag package
	// rejects a second Parse of a set that has already been parsed.
	fs := flag.NewFlagSet("apiaryinstall", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	apply := fs.Bool("apply", false, "perform RiskSafe fixes for any check that isn't ok (kldload+persist, pkg install, sysrc, chmod, pf.conf anchor) - never touches network topology, see -apply-network")
	applyNetwork := fs.String("apply-network", "", fmt.Sprintf("create/modify the bhyve bridge and attach -vlan-uplink to it - a real risk to this host's own network reachability (ADR-0022). Must be exactly %q or nothing happens", networkChangeConfirmPhrase))
	zfsPool := fs.String("zfs-pool", "zroot", "ZFS pool Apiary's datasets should live under")
	zfsBase := fs.String("zfs-base", "", "dataset managerd's own -zfs-base provisions VM/jail datasets under (default \"<zfs-pool>/apiary\", matching managerd's own default) - a fresh pool has no child datasets yet, so this is checked separately from -zfs-pool")
	bhyveFirmwarePkg := fs.String("bhyve-firmware-pkg", "bhyve-firmware", "package providing bhyve's UEFI firmware")
	vlanUplink := fs.String("vlan-uplink", "", "this node's real uplink NIC (e.g. em0) - required for the vlan-uplink and bhyve-bridge checks to run at all")
	bhyveBridge := fs.String("bhyve-bridge", "", "bridge interface name bhyve VM taps should attach to (e.g. bridge0) - requires -vlan-uplink too")
	enableNAT := fs.Bool("enable-nat", false, "also check gateway_enable/IP forwarding, for self-hosted outbound NAT (ADR-0048)")
	enableHAST := fs.Bool("enable-hast", false, "also check hastd_enable, for HAST-replicated disks (ADR-0026) - report-only; FreeBSD 15.1-RELEASE or later needs no patch (ADR-0152), earlier releases need the hast_proto_recv_hdr source patch for FreeBSD bug 298085, which Apiary never applies automatically")
	pamService := fs.String("pam-service", "", "PAM service name cmd/frontend will use (ADR-0030) - report-only, Apiary never generates PAM/account configuration itself")
	allowUplinkBridging := fs.String("allow-uplink-bridging", "", "report-only: check this node's -bhyve-bridge/-vlan-uplink are ready for managerd's own allow_uplink_bridging config setting (ADR-0101) - pass the same confirmation phrase managerd expects, or anything non-empty; Apiary never sets managerd's config itself")
	jsonOutput := fs.Bool("json", false, "emit results as JSON instead of a table")
	// Bound to a local, not to buildinfo's package-level variable: this
	// command's body may run more than once in a single process (under
	// test), and a shared flag destination would make one run's -version
	// answer leak into the next.
	var versionRequested bool
	buildinfo.RegisterVersionFlagTo(fs, &versionRequested)
	if err := fs.Parse(args); err != nil {
		return exitUsage, err
	}

	// Checked before ANY other work, so `-version` answers on a
	// machine with no /var/db/apiary, no config, and no
	// privileges. That is the whole point: you want to ask a
	// deployed binary what it is when it is the only thing left
	// to ask.
	if versionRequested {
		fmt.Fprint(out, buildinfo.Report("apiaryinstall"))
		return 0, nil
	}

	opt := install.Options{
		ZFSPool:             *zfsPool,
		ZFSBase:             *zfsBase,
		BhyveFirmwarePkg:    *bhyveFirmwarePkg,
		VLANUplink:          *vlanUplink,
		BhyveBridge:         *bhyveBridge,
		EnableNAT:           *enableNAT,
		EnableHAST:          *enableHAST,
		PAMService:          *pamService,
		AllowUplinkBridging: *allowUplinkBridging,
	}
	networkConfirmed := *applyNetwork != "" && *applyNetwork == networkChangeConfirmPhrase
	if *applyNetwork != "" && !networkConfirmed {
		return exitFailure, fmt.Errorf("-apply-network value %q does not match the required confirmation phrase %q - nothing was done", *applyNetwork, networkChangeConfirmPhrase)
	}

	ctx := context.Background()
	if runner == nil {
		runner = install.NewExecRunner()
	}
	if checks == nil {
		checks = install.All()
	}

	var results []install.Result
	failed := false
	for _, c := range checks {
		if c.Applicable != nil && !c.Applicable(opt) {
			continue
		}
		shouldApply := (*apply && c.Risk == install.RiskSafe) || (networkConfirmed && c.Risk == install.RiskNetwork)
		if shouldApply && c.Apply != nil {
			res := c.Probe(ctx, runner, opt)
			// StatusInformational is deliberately treated like StatusOK
			// here, same as the failed/ok bookkeeping below: there is
			// nothing to apply, and calling Apply anyway would hit the
			// same non-conflict it already reported and refuse, turning
			// a non-issue into an apparent "apply failed" one.
			if res.Status != install.StatusOK && res.Status != install.StatusInformational {
				if err := c.Apply(ctx, runner, opt); err != nil {
					res.Detail = fmt.Sprintf("apply failed: %v", err)
				} else {
					res = c.Probe(ctx, runner, opt)
				}
			}
			results = append(results, res)
		} else {
			results = append(results, c.Probe(ctx, runner, opt))
		}
		if last := results[len(results)-1].Status; last != install.StatusOK && last != install.StatusInformational {
			failed = true
		}
	}

	if *jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return exitFailure, err
		}
	} else {
		printTable(out, results)
	}

	if failed {
		return exitFailure, nil
	}
	return 0, nil
}

// printTable writes the human-readable summary. It takes its writer as an
// argument rather than writing to os.Stdout so a test can read what the
// operator would have seen.
func printTable(out io.Writer, results []install.Result) {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tDETAIL\tFIX HINT")
	ok, total := 0, len(results)
	for _, r := range results {
		// StatusInformational counts toward "ok" in this summary line,
		// same as the failed/apply bookkeeping in run() above: it is
		// never a problem for an operator to resolve, just a fact worth
		// naming - see StatusInformational's own doc comment.
		if r.Status == install.StatusOK || r.Status == install.StatusInformational {
			ok++
		}
		hints := strings.Split(r.FixHint, "\n")
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.ID, r.Status, r.Detail, hints[0])
		for _, hint := range hints[1:] {
			// Keep a multi-line fix hint in the FIX HINT column rather than
			// making its continuation lines look like separate checks.
			fmt.Fprintf(w, "\t\t\t%s\n", hint)
		}
	}
	w.Flush()
	fmt.Fprintf(out, "\n%d/%d checks ok\n", ok, total)
}
