// Command apiaryctl is the operator-facing command line for a Comb.
//
// It is a separate installed binary at /usr/local/libexec/apiary/apiaryctl
// rather than something reached through a checkout, and that is the
// whole reason it exists. A Combs' source tree was never there to be
// relied on - the Combs are not development machines, which is exactly
// why there is no checkout to run a make target from. Anything an
// operator has to type during an incident has to be a file on the host,
// at a fixed path, runnable from any root shell with nothing else
// present. See docs/adr/0136-local-cli.md.
//
// It is its own process and not a mode of managerd or raftd because it
// restarts both of them: the invoking process has to survive the
// children it is about to kill. The same rule ADR-0146 states from the
// other side - managerd must never restart itself - is why this cannot
// be folded into managerd either.
//
// SCOPE, deliberately narrow. Three subcommands, force-restart, install
// and join-authorize, and no socket, no terminal UI, and no command
// group that has not been asked for: a command that is present and wrong
// is worse than one that is absent, and the larger design in ADR-0136
// stays proposed until it is built.
//
// install is the second, and it exists for the same reason force-restart
// does: both are things an operator has to type on a Comb that has no
// checkout. `make setup-quick` wrote seven config files with printf and
// generated a certificate by shelling out to openssl, and neither is
// something that can be typed on a machine where the only Apiary files
// are the installed binaries. See ADR-0147 Part 1.
//
// Usage:
//
//	apiaryctl install [--apply]
//	apiaryctl join-authorize --node-id ID --fingerprint FP
//	apiaryctl force-restart
//	apiaryctl help
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/glenjbarber/apiary/internal/buildinfo"
)

const usage = `apiaryctl - operator commands for an apiary Comb.

Usage:
  apiaryctl install          generate this Comb's configuration, TLS
                            serving certificate and identity. Report
                            only unless --apply. Needs a root shell and
                            no checkout.
  apiaryctl join-authorize authorize one Comb to join this Colony, by
                            writing a single-use entry into the
                            root-owned store. Root only, no checkout
  apiaryctl force-restart   restart managerd then raftd on this Comb,
                            confirming each by its own listener port
  apiaryctl -version        report this binary's build identity
  apiaryctl help            this message

force-restart is root-only and touches only this Comb. It takes no
lease and coordinates with nothing; see the warning it prints, and use
the Machine page's per-service control for a coordinated restart.

join-authorize is root-only because the store it writes is root-owned,
and that ownership is the security property rather than an incidental
detail: it is the one thing an Admin of this Colony cannot do.

install is root-only with --apply and needs no privileges without it.
It never overwrites a field that is already set, and a run that refuses
anything exits non-zero while still doing the rest.
`

func main() {
	// -version is registered before anything else and answered before
	// any subcommand is considered, so it works on a machine with no
	// /var/db/apiary, no config, and no privileges. That is the whole
	// point: you want to ask a deployed binary what it is when it is
	// the only thing left to ask. `make version` runs it against every
	// built binary, so it is not optional either.
	buildinfo.RegisterVersionFlag(flag.CommandLine)
	flag.Parse()
	if buildinfo.VersionRequested() {
		fmt.Print(buildinfo.Report("apiaryctl"))
		return
	}
	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch args[0] {
	case "install":
		os.Exit(runInstall(args[1:]))
	case "join-authorize":
		os.Exit(runJoinAuthorize(args[1:]))
	case "force-restart":
		os.Exit(runForceRestart(args[1:]))
	case "help", "-h", "--help":
		fmt.Print(usage)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "apiaryctl: unknown subcommand %q\n\n%s", args[0], usage)
		os.Exit(2)
	}
}
