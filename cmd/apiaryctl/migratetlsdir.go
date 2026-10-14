package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/glenjbarber/apiary/internal/tlsmigrate"
)

// runMigrateTLSDir is `apiaryctl migrate-tls-dir` (issue #99): the
// one-command move of an existing Comb's serving certificate and key
// from the old /usr/local/etc/apiary-tls to the new default,
// /usr/local/etc/apiary/tls, rewriting managerd.json/frontend.json/
// restshimd.json to match and restarting managerd and frontend (and
// restshimd, if its own config was touched) in the safe order.
//
// Report-only by default, exactly like `apiaryctl install`: with no
// --apply it reads the three config files and prints what it would
// move and rewrite, and changes nothing. A Comb already on the new
// layout, or one with no TLS paths set at all, reports a no-op and
// exits 0.
func runMigrateTLSDir(args []string) int {
	fs := flag.NewFlagSet("apiaryctl migrate-tls-dir", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var apply, skipRestart bool
	var configDir string
	fs.BoolVar(&apply, "apply", false, "perform the move and rewrite the config files; without it, this is a report and changes nothing")
	fs.BoolVar(&skipRestart, "skip-restart", false, "rewrite the files but do not restart managerd/frontend/restshimd - use this if you would rather restart them yourself")
	fs.StringVar(&configDir, "config-dir", "", "defaults to /usr/local/etc/apiary")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: apiaryctl migrate-tls-dir [--apply] [--skip-restart] [--config-dir dir]

Moves this Comb's serving certificate and key from the old
/usr/local/etc/apiary-tls to the new default,
/usr/local/etc/apiary/tls (issue #99), and rewrites managerd.json,
frontend.json and restshimd.json's tls_cert/tls_key/manager_tls_ca
fields to match. Never prints or logs the private key's contents -
only paths.

Report-only by default: with no --apply it writes nothing at all. A
Comb whose config already names the new directory, or whose config
names no TLS path at all, reports a no-op and exits 0 either way.

After a successful --apply that moved anything, managerd and frontend
(and restshimd, if its own config was touched) are restarted in that
order, unless --skip-restart is given. If this Comb renews its
certificate via an acme.sh (or similar) renewal hook, that hook's
install path is NOT found or rewritten here - see the note this
command prints, and update it by hand.
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "apiaryctl migrate-tls-dir: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if apply && os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "apiaryctl migrate-tls-dir: --apply moves files under /usr/local/etc/apiary and restarts root-owned services, so it needs an effective uid of 0.\nRe-run without --apply to see the plan first; it needs no privileges at all.")
		return 1
	}

	paths := tlsmigrate.Paths{ConfigDir: configDir}
	plan, err := tlsmigrate.New(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apiaryctl migrate-tls-dir: %v\n", err)
		return 1
	}

	if plan.NoOp {
		fmt.Printf("apiaryctl migrate-tls-dir: nothing to do - %s\n", plan.NoOpWhy)
		return 0
	}

	fmt.Printf("apiaryctl migrate-tls-dir: plan\n\n")
	fmt.Printf("  move %v from %s to %s\n", plan.Moves(), plan.OldTLSDir, plan.NewTLSDir)
	for _, f := range plan.Files() {
		fmt.Printf("  rewrite %s\n", f)
	}
	fmt.Println()

	if !apply {
		fmt.Println("nothing was changed: this was a report. Re-run with --apply to carry it out.")
		return 0
	}

	if err := plan.Apply(); err != nil {
		fmt.Fprintf(os.Stderr, "\napiaryctl migrate-tls-dir: %v\n", err)
		return 1
	}
	fmt.Printf("\nmoved %v to %s and rewrote %v.\n", plan.Moves(), plan.NewTLSDir, plan.Files())

	if skipRestart {
		fmt.Println("\n--skip-restart given: managerd/frontend/restshimd were NOT restarted. Restart them yourself for the move to take effect.")
	} else {
		restarted, err := plan.Restart(tlsmigrate.ServiceRestarter{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "\napiaryctl migrate-tls-dir: restarted %v, then failed: %v\n", restarted, err)
			fmt.Fprintln(os.Stderr, "  The config files are already rewritten to the new path. Finish restarting the rest by hand: service <name> restart.")
			return 1
		}
		fmt.Printf("restarted %v.\n", restarted)
	}

	fmt.Println()
	fmt.Println(tlsmigrate.AcmeHookNote)
	return 0
}
