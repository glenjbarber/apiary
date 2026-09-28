package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/glenjbarber/apiary/internal/hostinstall"
)

// installFlags is the whole surface of `apiaryctl install`, and it is
// small on purpose. The tool's value is that it decides correctly
// without being told much; a flag that overrides a decision the
// installer can make is a flag that lets an operator install something
// they did not mean to.
type installFlags struct {
	apply        bool
	colonyMember string
	zfsBase      string
	uplink       string
	bhyveBridge  string
}

func runInstall(args []string) int {
	fs := flag.NewFlagSet("apiaryctl install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var f installFlags
	fs.BoolVar(&f.apply, "apply", false, "perform the writes; without it, this is a report and changes nothing")
	fs.StringVar(&f.colonyMember, "colony-member", "", "host:port of a Colony member that already exists, so this Comb is configured to join rather than to stand alone")
	fs.StringVar(&f.zfsBase, "zfs-base", "", "the ZFS dataset VMs are created under, e.g. zroot/apiary; not written without it")
	fs.StringVar(&f.uplink, "vlan-uplink", "", "the physical interface VLAN traffic is tagged on, e.g. em0; not written without it")
	fs.StringVar(&f.bhyveBridge, "bhyve-bridge", "", "the bridge VMs attach to, e.g. bridge0; not written without it")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: apiaryctl install [--apply] [--colony-member host:port]
                            [--zfs-base dataset] [--vlan-uplink iface] [--bhyve-bridge iface]

Generates configuration, a TLS serving certificate, and this Comb's
identity, from an installed binary and a root shell. It needs no
checkout, no Makefile and no source file of any kind, because a Comb
is not a machine that has one.

Report-only by default: with no --apply it writes nothing at all, and
what it prints is a list of every file it would create, every field it
would fill, every field it is leaving alone and why, and every value
it could not derive and needs from you.

What it will not do, in either mode: overwrite a field that is already
set, change a node_id, complete half a TLS pair, or write an address
the certificate does not cover. A file that exists and does not parse,
or that carries fields this binary does not know, is refused whole and
named in the report.

Exit status is 0 only when nothing was refused. A refusal still leaves
everything else written, so a second run finishes the job.
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "apiaryctl install: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if f.apply && os.Geteuid() != 0 {
		fmt.Fprintf(os.Stderr, "apiaryctl install: --apply writes /usr/local/etc/apiary and /usr/local/etc/apiary-tls as root, so it needs an effective uid of 0.\nRe-run without --apply to see the plan first; it needs no privileges at all.\n")
		return 1
	}

	plan, err := hostinstall.New(hostinstall.Options{
		ColonyMember: f.colonyMember,
		ZFSBase:      f.zfsBase,
		Uplink:       f.uplink,
		BhyveBridge:  f.bhyveBridge,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "apiaryctl install: %v\n", err)
		return 1
	}
	plan.Report(os.Stdout)
	if !f.apply {
		fmt.Fprintln(os.Stdout, "\nnothing was changed: this was a report. Re-run with --apply to carry it out.")
		return 0
	}
	if err := plan.Apply(); err != nil {
		fmt.Fprintf(os.Stderr, "\napiaryctl install: %v\n", err)
		var refused *hostinstall.RefusedError
		if errors.As(err, &refused) {
			fmt.Fprintln(os.Stderr, "The rest of the install was carried out. Fix what is named above and run it again; the second run changes only what is still missing.")
			return 1
		}
		return 1
	}
	fmt.Fprintln(os.Stdout, "\nStart the services with: service apiary_raftd start && service apiary_managerd start && service apiary_frontend start && service apiary_rest_shimd start")
	return 0
}
