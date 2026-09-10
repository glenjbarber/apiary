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
	"log"
	"os"
	"text/tabwriter"

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
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	apply := flag.Bool("apply", false, "perform RiskSafe fixes for any check that isn't ok (kldload+persist, pkg install, sysrc, chmod, pf.conf anchor) - never touches network topology, see -apply-network")
	applyNetwork := flag.String("apply-network", "", fmt.Sprintf("create/modify the bhyve bridge and attach -vlan-uplink to it - a real risk to this host's own network reachability (ADR-0022). Must be exactly %q or nothing happens", networkChangeConfirmPhrase))
	zfsPool := flag.String("zfs-pool", "zroot", "ZFS pool Apiary's datasets should live under")
	zfsBase := flag.String("zfs-base", "", "dataset managerd's own -zfs-base provisions VM/jail datasets under (default \"<zfs-pool>/apiary\", matching managerd's own default) - a fresh pool has no child datasets yet, so this is checked separately from -zfs-pool")
	bhyveFirmwarePkg := flag.String("bhyve-firmware-pkg", "bhyve-firmware", "package providing bhyve's UEFI firmware")
	vlanUplink := flag.String("vlan-uplink", "", "this node's real uplink NIC (e.g. em0) - required for the vlan-uplink and bhyve-bridge checks to run at all")
	bhyveBridge := flag.String("bhyve-bridge", "", "bridge interface name bhyve VM taps should attach to (e.g. bridge0) - requires -vlan-uplink too")
	enableNAT := flag.Bool("enable-nat", false, "also check gateway_enable/IP forwarding, for self-hosted outbound NAT (ADR-0048)")
	enableHAST := flag.Bool("enable-hast", false, "also check hastd_enable, for HAST-replicated disks (ADR-0026) - report-only, the hastd source patch is never applied automatically")
	pamService := flag.String("pam-service", "", "PAM service name cmd/frontend will use (ADR-0030) - report-only, Apiary never generates PAM/account configuration itself")
	jsonOutput := flag.Bool("json", false, "emit results as JSON instead of a table")
	flag.Parse()

	opt := install.Options{
		ZFSPool:          *zfsPool,
		ZFSBase:          *zfsBase,
		BhyveFirmwarePkg: *bhyveFirmwarePkg,
		VLANUplink:       *vlanUplink,
		BhyveBridge:      *bhyveBridge,
		EnableNAT:        *enableNAT,
		EnableHAST:       *enableHAST,
		PAMService:       *pamService,
	}
	networkConfirmed := *applyNetwork != "" && *applyNetwork == networkChangeConfirmPhrase
	if *applyNetwork != "" && !networkConfirmed {
		return fmt.Errorf("-apply-network value %q does not match the required confirmation phrase %q - nothing was done", *applyNetwork, networkChangeConfirmPhrase)
	}

	ctx := context.Background()
	runner := install.NewExecRunner()

	var results []install.Result
	failed := false
	for _, c := range install.All() {
		if c.Applicable != nil && !c.Applicable(opt) {
			continue
		}
		shouldApply := (*apply && c.Risk == install.RiskSafe) || (networkConfirmed && c.Risk == install.RiskNetwork)
		if shouldApply && c.Apply != nil {
			res := c.Probe(ctx, runner, opt)
			if res.Status != install.StatusOK {
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
		if len(results) > 0 && results[len(results)-1].Status != install.StatusOK {
			failed = true
		}
	}

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return err
		}
	} else {
		printTable(results)
	}

	if failed {
		os.Exit(1)
	}
	return nil
}

func printTable(results []install.Result) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tDETAIL\tFIX HINT")
	ok, total := 0, len(results)
	for _, r := range results {
		if r.Status == install.StatusOK {
			ok++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.ID, r.Status, r.Detail, r.FixHint)
	}
	w.Flush()
	fmt.Printf("\n%d/%d checks ok\n", ok, total)
}
