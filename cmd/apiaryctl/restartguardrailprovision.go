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
// It does NOT push the token to any other Comb over the network. The
// restart-guardrail token is deliberately the one secret in this
// codebase that never crosses an RPC in either direction (see
// internal/manager/auth.go's own reserveRestartLeaseMethod doc
// comment: two earlier designs that tried to make a credential like
// this one RPC-mintable both turned out not to actually close off an
// ordinary Admin API key's path to it). Building a new wire path that
// carries this value - even once, even over TLS - is exactly the kind
// of change that doc comment warns deserves real design scrutiny, not
// a mechanical addition under this issue's own time box. So this
// command prints the exact, safe way to finish the job by hand: copy
// the file it just created, byte-for-byte, to every other Comb's own
// /usr/local/etc/apiary/restart-guardrail-token over a channel the
// operator already trusts (scp over the Colony's own management
// network, or hand-typed from this terminal), then restart managerd on
// every Comb - the token is read once at startup (see
// cmd/managerd/main.go), so nothing short of a restart picks it up.
//
// Full join-channel provisioning (issue #98's requirement 2 - a joiner
// receiving this token automatically, over the encrypted join RPC, the
// same way #80's peer_api_key is meant to work) is the right place for
// automatic cross-Comb distribution to eventually live, once that
// channel exists for a secret of this kind. It does not exist yet
// today for ANY secret in this codebase - #80's own peer_api_key is
// also hand-configured, not pushed over any join RPC - so there is no
// existing mechanism for this command to reuse, and inventing the
// first one as a side effect of a provisioning command would be
// backwards: the channel's own design should be reviewed on its own
// issue, not introduced here as plumbing for a different command.
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

Generating the value is the easy half. The hard half - and the part
this command does NOT do - is getting the byte-identical value onto
every OTHER Comb: every Comb in a Colony must hold the exact same
token, because a follower's forwarded confirmation presents it to the
leader, which checks it against its own copy. This command prints the
value's path (never the value itself) and the exact next step:

  1. Copy `+restartGuardrailProvisionPath+` from this Comb to the identical
     path on every other Comb in the Colony, byte-for-byte, over a
     channel you already trust (scp over the management network is
     the usual choice).
  2. Restart managerd on every Comb, including this one. The token is
     read exactly once, at managerd startup (see cmd/managerd/main.go),
     so nothing short of a restart picks up a value written after the
     process was already running.

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
		fmt.Fprintln(errOut, "If this Colony's other Combs do not already match it, copy THIS file to them instead of generating a new one.")
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
	fmt.Fprintln(out, "This Comb is not done yet. Two steps remain, on every Comb in the Colony:")
	fmt.Fprintf(out, "  1. Copy %s from THIS Comb, byte-for-byte, onto every other Comb at the identical path, over a channel you already trust (e.g. scp).\n", path)
	fmt.Fprintln(out, "  2. Restart managerd on every Comb, including this one - the token is read once at startup and a running managerd will not notice a file that appears after it started.")
	fmt.Fprintln(out, "\nNothing was sent over the network by this command. See `apiaryctl help` and issue #98 for why.")
	return 0
}
