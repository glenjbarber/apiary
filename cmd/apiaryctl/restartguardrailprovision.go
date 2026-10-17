// apiaryctl restart-guardrail-provision is issue #98's one-command fix
// for requirement 3: an existing Colony that has no restart-guardrail
// token anywhere has a supported way to get one, rather than the
// previous dead end (standalone install generates one; a
// --colony-member install only tells the operator to copy the file from
// another Comb, which is no help when no Comb has it).
//
// SCOPE, deliberately narrow, and said so rather than silently assumed:
// this command generates a fresh token on THIS Comb only, the same way
// a standalone install does (internal/hostinstall's own generateToken:
// 32 random bytes, base64.RawURLEncoding), and never overwrites an
// existing non-empty file - the same preservation rule
// planRestartGuardrailToken already follows, because this is a live
// credential and the file holding it is not this command's to
// second-guess once something has put one there.
//
// It does NOT push the token to already-running Combs. The join flow
// delivers it to a newly approved Comb in an application-encrypted
// envelope, and peer-credential-backfill uses the same certificate-bound
// handoff for existing pinned members. This command remains local-only;
// restart managerd here, then run the backfill command on each member.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/glenjbarber/apiary/internal/hostinstall"
)

// restartGuardrailProvisionPath is the same fixed path every other
// reader and writer of this file already agrees on (cmd/managerd's and
// cmd/raftd's own restartGuardrailTokenPath constants,
// internal/manager's own copy, and internal/hostinstall's
// restartGuardrailTokenFile()).
const restartGuardrailProvisionPath = "/usr/local/etc/apiary/restart-guardrail-token"

func runRestartGuardrailProvision(args []string) int {
	fs := flag.NewFlagSet("apiaryctl restart-guardrail-provision", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: apiaryctl restart-guardrail-provision

Generate ADR-0103's restart-guardrail shared secret on THIS Comb, for a
Colony where no Comb has one yet (issue #98). Refuses outright, and
writes nothing, if `+restartGuardrailProvisionPath+` already exists here -
this is a live credential, and a file an operator or a previous install
already put there is not this command's to second-guess.

Generating the value is the easy half. The backfill command delivers it
to existing members in encrypted envelopes addressed to their pinned
manager TLS certificates. This command prints the value's path (never
the value itself) and the exact next step:

  1. Restart managerd on this Comb. The token is
     read exactly once, at managerd startup (see cmd/managerd/main.go),
     so nothing short of a restart picks up a value written after the
     process was already running.
  2. Run apiaryctl peer-credential-backfill on each existing Comb.
     Followers without a token file retrieve it securely from the
     current leader.

Root only: the file this writes is root-owned, mode 0600.
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "apiaryctl restart-guardrail-provision: takes no arguments, got %q\n", fs.Arg(0))
		return 2
	}

	if os.Geteuid() != 0 {
		fmt.Fprint(os.Stderr, `apiaryctl restart-guardrail-provision: must run as root.

It writes `+restartGuardrailProvisionPath+`, root-owned at mode 0600. Run it
from a root shell, or with sudo.
`)
		return 1
	}

	return restartGuardrailProvision(restartGuardrailProvisionPath, os.Stdout, os.Stderr)
}

// restartGuardrailProvision is the command minus flag parsing and the
// root check, so it can be tested against a temp path without actually
// needing root or the real /usr/local/etc/apiary.
func restartGuardrailProvision(path string, out, errOut *os.File) int {
	if info, err := os.Stat(path); err == nil {
		fmt.Fprintf(errOut, "apiaryctl restart-guardrail-provision: %s already exists (mode %04o) - refusing to touch it.\n", path, info.Mode().Perm())
		fmt.Fprintln(errOut, "This is a live credential; a file already there is not this command's to second-guess.")
		fmt.Fprintln(errOut, "If another Comb has a different token, do not replace this one. Reconcile the existing token values before running the backfill.")
		return 1
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(errOut, "apiaryctl restart-guardrail-provision: checking %s: %v\n", path, err)
		return 1
	}

	token, err := hostinstall.GenerateRestartGuardrailToken()
	if err != nil {
		fmt.Fprintf(errOut, "apiaryctl restart-guardrail-provision: generating a random token: %v\n", err)
		return 1
	}
	if err := hostinstall.WriteRestartGuardrailToken(path, token); err != nil {
		fmt.Fprintf(errOut, "apiaryctl restart-guardrail-provision: %v\n", err)
		return 1
	}

	fmt.Fprintf(out, "apiaryctl restart-guardrail-provision: generated a new restart-guardrail token at %s (mode 0600). The value is never printed.\n\n", path)
	fmt.Fprintln(out, "Next steps:")
	fmt.Fprintln(out, "  1. Restart managerd on this Comb so the leader loads the new token.")
	fmt.Fprintln(out, "  2. Run apiaryctl peer-credential-backfill on every existing member; followers retrieve the token in an envelope encrypted to their pinned TLS certificate.")
	fmt.Fprintln(out, "\nNothing was sent over the network by this command.")
	return 0
}
