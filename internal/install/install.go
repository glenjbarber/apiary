// Package install implements Apiary's host preflight/provisioning checks -
// the "is this FreeBSD host ready to run raftd/managerd/frontend/
// restshimd" question every ADR up to this point has only ever answered in
// prose (vmm.ko/nmdm.ko loaded, bhyve-firmware/dnsmasq installed, a ZFS
// pool present, pf enabled with an apiary/* anchor, a bridge for the
// uplink NIC, /etc/rc.conf permissions, PAM service files - see
// docs/adr/0082-apiary-installer-preflight.md for the full list and its
// sources). Unlike internal/assumecheck (ADR-0055), which continuously
// monitors an already-running cluster's raft/peer health, this package
// only ever inspects (and optionally provisions) the local host, and is
// meant to run once, before any daemon or raft state exists.
//
// Every check is tiered by Risk. RiskSafe fixes (kldload, pkg install,
// sysrc, chmod, appending a known-safe config line after taking a backup)
// run under a plain -apply. RiskNetwork - creating or modifying a bridge
// interface and attaching the uplink NIC to it - never runs under -apply
// alone: ADR-0022 documents a live incident where exactly this operation
// nearly cost the operator their own SSH session, so it requires its own
// separate, exact-phrase-gated flag (mirroring cmd/raftd's -reset/-restore
// precedent). RiskManualOnly checks (PAM service files, ZFS pool
// existence, the hastd source patch, UNIX accounts) have no Apply at all -
// permanently, the same "export-only, not planned to grow" boundary
// internal/hostconfig draws around itself, not a v1 gap.
package install

import "context"

// Status is a check's observed outcome.
type Status string

const (
	StatusOK            Status = "ok"
	StatusMissing       Status = "missing"
	StatusMisconfigured Status = "misconfigured"
	StatusManual        Status = "manual"
	StatusUnknown       Status = "unknown"
)

// Result is one check's outcome. FixHint is always populated when Status
// is not StatusOK, whether or not the check is auto-fixable, so a
// RiskManualOnly finding is exactly as actionable to a reader as an
// auto-fixable one.
type Result struct {
	ID      string
	Status  Status
	Detail  string
	FixHint string
}

// Risk gates whether, and under what confirmation, a Check's Apply may
// run. See the package doc comment.
type Risk string

const (
	RiskSafe       Risk = "safe"
	RiskNetwork    Risk = "network"
	RiskManualOnly Risk = "manual-only"
)

// Options carries every operator-supplied value a check's Probe/Apply may
// need. Zero values mean "not supplied" - checks that require a specific
// option (e.g. bhyve-bridge needs VLANUplink) report StatusManual with a
// FixHint naming the missing flag rather than guessing.
type Options struct {
	ZFSPool          string
	ZFSBase          string
	BhyveFirmwarePkg string
	VLANUplink       string
	BhyveBridge      string
	EnableNAT        bool
	EnableHAST       bool
	PAMService       string
}

// Runner executes a real host command, returning its stdout and stderr
// separately (mirroring every other internal/* package's own runCmd
// convention) so a Probe can distinguish "command failed" from "command
// succeeded but reported the condition is false." Tests substitute
// fakeRunner; production code uses NewExecRunner.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
}

// Check is one preflight check. Apply is nil when Risk is
// RiskManualOnly - there is deliberately nothing to call.
type Check struct {
	ID          string
	Description string
	Risk        Risk
	// Applicable reports whether this check applies at all given opt
	// (e.g. gateway-enable only matters when EnableNAT is set, pam-service
	// only when PAMService is non-empty). A nil Applicable always applies.
	Applicable func(opt Options) bool
	Probe      func(ctx context.Context, r Runner, opt Options) Result
	Apply      func(ctx context.Context, r Runner, opt Options) error
}

// All returns every registered check, in the fixed order they should be
// run and reported.
func All() []Check {
	return append([]Check(nil), registry...)
}
