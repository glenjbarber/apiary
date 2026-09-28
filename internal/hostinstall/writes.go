package hostinstall

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/glenjbarber/apiary/internal/commonconfig"
	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
	"github.com/glenjbarber/apiary/internal/restshimdconfig"
)

// planWrites turns the decisions already made into the specific field
// writes they imply, registering each one on the Plan. Nothing is
// written here; a file whose every field is already present gets no
// write registered at all, which is what makes a second run over this
// install's own output change nothing - not even a rewrite of
// identical bytes.
func (p *Plan) planWrites() {
	p.planDirectories()
	if p.pendingTLSDir {
		certPath, keyPath := p.opts.Paths.certFile(), p.opts.Paths.keyFile()
		p.writes = append(p.writes, func() error {
			if err := os.MkdirAll(p.opts.Paths.TLSDir, 0o700); err != nil {
				return fmt.Errorf("hostinstall: creating %s: %w", p.opts.Paths.TLSDir, err)
			}
			if err := writeFileAtomic(certPath, p.pendingCert, 0o644); err != nil {
				return err
			}
			// The key is written second, so a failure between the two
			// leaves a certificate with no key rather than a key with
			// no certificate. The next run refuses the first as a
			// half-pair, which is the correct thing for it to do.
			return writeFileAtomic(keyPath, p.pendingKey, 0o600)
		})
		p.add(ActionCreate, p.opts.Paths.TLSDir, "directory", "0700",
			"created by the certificate write above; the private key lives here, and it is never re-chmodded on a later run")
	}
	p.planCommonWrites()
	p.planRaftdWrites()
	p.planManagerdWrites()
	p.planFrontendWrites()
	p.planRestshimdWrites()
}

// planDirectories accounts for the directories the daemons expect. They
// are created 0700 when absent and left exactly as they are when
// present: an existing directory with wider permissions is something an
// operator chose, or something another tool needs, and silently
// tightening it is the same class of unrequested change this package
// exists to avoid.
func (p *Plan) planDirectories() {
	for _, dir := range []struct{ path, why string }{
		{p.opts.Paths.ConfigDir, "every daemon's own config file"},
		{p.opts.Paths.TLSDir, "this Comb's serving certificate and key"},
		{p.opts.Paths.DataDir, "raftd's log and stable store"},
		{p.opts.Paths.ISODir, "the ISOs this Comb serves to VMs"},
		{p.opts.Paths.RunDir, "the internal Unix sockets managerd and raftd use"},
	} {
		if p.pendingTLSDir && dir.path == p.opts.Paths.TLSDir {
			// Already accounted for by the TLS write above, which
			// creates it with the mode it needs.
			continue
		}
		info, err := os.Stat(dir.path)
		switch {
		case err == nil:
			p.add(ActionKeep, dir.path, "directory mode", fmt.Sprintf("%04o", info.Mode().Perm()),
				"already exists; its mode is left exactly as it is - "+dir.why)
		case os.IsNotExist(err):
			p.add(ActionCreate, dir.path, "directory", "0700", dir.why)
			path := dir.path
			p.writes = append(p.writes, func() error {
				if err := os.MkdirAll(path, 0o700); err != nil {
					return fmt.Errorf("hostinstall: creating %s: %w", path, err)
				}
				return nil
			})
		default:
			p.add(ActionRefuse, dir.path, "directory", "", err.Error())
		}
	}
}

func (p *Plan) planCommonWrites() {
	path := p.opts.Paths.commonConfig()
	if p.refused(path) {
		return
	}
	cfg := p.common
	changed := false
	if p.common.NodeID == "" {
		if p.NodeID == "" {
			p.add(ActionNeeds, path, "node_id", "",
				"not written, because the node_id could not be decided: see the node_id refusals above")
		} else {
			cfg.NodeID = p.NodeID
			changed = true
			p.add(ActionFill, path, "node_id", p.NodeID,
				"this is the value every daemon on this Comb shares (ADR-0112), derived from "+p.nodeIDSource())
		}
	} else {
		p.add(ActionKeep, path, "node_id", p.common.NodeID, "already set here; never changed")
	}
	if p.common.Hostname == "" {
		cfg.Hostname = p.hostname
		changed = true
		p.add(ActionFill, path, "hostname", p.hostname, "this Comb's name, shared by every daemon here")
	} else {
		p.add(ActionKeep, path, "hostname", p.common.Hostname, "already set here; never changed")
	}
	if !changed {
		return
	}
	p.writes = append(p.writes, func() error {
		return (&commonconfig.Manager{Path: path}).Save(cfg)
	})
}

// nodeIDSource names where a derived node_id came from, so the report
// says "taken from the host's FQDN" rather than presenting a guess in
// the same tone as a value it read from disk.
func (p *Plan) nodeIDSource() string {
	switch {
	case p.managerOwn.NodeID != "":
		return "managerd.json, which already had one"
	case p.raftdOwn.NodeID != "":
		return "raftd.json, which already had one"
	case p.common.NodeID != "":
		return "common.json, which already had one"
	case p.opts.FQDN != "":
		return "this host's FQDN; rename the Comb and this value has to change with it"
	default:
		return "this host's short hostname; rename the Comb and this value has to change with it"
	}
}

func (p *Plan) planRaftdWrites() {
	path := p.opts.Paths.raftdConfig()
	if p.refused(path) {
		return
	}
	cfg := p.raftdCfg
	changed := false
	changed = p.fill(path, "data_dir", p.raftdOwn.DataDir, &cfg.DataDir, p.opts.Paths.DataDir, "where raftd keeps its log, stable store and snapshots") || changed
	changed = p.fill(path, "socket", p.raftdOwn.Socket, &cfg.Socket, filepath.Join(p.opts.Paths.RunDir, "raftd.sock"), "the internal socket managerd reaches raftd through") || changed
	// node_id here is only ever filled where the FILE has none, which
	// is not the same test as "cfg.NodeID is empty": a node_id
	// inherited from common.json is already decided, and writing it
	// into raftd.json as well would be a second place to change it
	// later.
	if p.raftdOwn.NodeID == "" && cfg.NodeID == "" && p.NodeID != "" {
		cfg.NodeID = p.NodeID
		changed = true
		p.add(ActionFill, path, "node_id", p.NodeID,
			"this file had none; common.json is the shared home for it (ADR-0112), and this is written only so raftd's own file is complete on its own")
	} else {
		p.add(ActionKeep, path, "node_id", p.raftdOwn.NodeID,
			"already set in this file; never changed")
	}
	if !changed {
		return
	}
	p.writes = append(p.writes, func() error {
		return (&raftdconfig.Manager{Path: path}).Save(cfg)
	})
}

func (p *Plan) planManagerdWrites() {
	path := p.opts.Paths.managerdConfig()
	if p.refused(path) {
		return
	}
	cfg := p.managerCfg
	changed := false
	changed = p.fill(path, "raftd_socket", p.managerOwn.RaftdSocket, &cfg.RaftdSocket, filepath.Join(p.opts.Paths.RunDir, "raftd.sock"), "the internal socket raftd listens on") || changed
	changed = p.fill(path, "rpc_addr", p.managerOwn.RPCAddr, &cfg.RPCAddr, p.RPCAddr, "managerd's own listening address (ADR-0139)") || changed
	changed = p.fill(path, "iso_dir", p.managerOwn.ISODir, &cfg.ISODir, p.opts.Paths.ISODir, "where this Comb keeps the ISOs it serves to VMs") || changed
	changed = p.fill(path, "pam_service", p.managerOwn.PAMService, &cfg.PAMService, defaultPAMService,
		"the PAM policy name; the policy FILE under /etc/pam.d is apiaryinstall's job, and this tool does not write into /etc") || changed
	changed = p.needHostFact(path, "zfs_base", p.managerOwn.ZFSBase, &cfg.ZFSBase, p.opts.ZFSBase,
		"the ZFS dataset VMs are created under. No pool can be derived from a config file, and a wrong one is a working config file that is wrong, so nothing is written without --zfs-base") || changed
	changed = p.needHostFact(path, "uplink", p.managerOwn.Uplink, &cfg.Uplink, p.opts.Uplink,
		"the physical interface VLAN traffic is tagged on. Only apiaryinstall can answer this, by asking the host, so nothing is written without --vlan-uplink") || changed
	changed = p.needHostFact(path, "bhyve_bridge", p.managerOwn.BhyveBridge, &cfg.BhyveBridge, p.opts.BhyveBridge,
		"the bridge VMs attach to. This is a host topology fact, and a bridge that does not exist is a silently wrong config, so nothing is written without --bhyve-bridge") || changed
	changed = p.needHostFact(path, "bhyve_bootrom", p.managerOwn.BhyveBootROM, &cfg.BhyveBootROM, p.opts.BhyveBootROM,
		"the UEFI firmware file bhyve boots from. The package that carries it is sometimes the firmware package and sometimes edk2-bhyve, so locating it is a package question, not a filesystem one, and nothing is written without --bhyve-bootrom") || changed

	if p.tlsUsable() {
		changed = p.fill(path, "tls_cert", p.managerOwn.TLSCert, &cfg.TLSCert, p.opts.Paths.certFile(), "this Comb's serving certificate") || changed
		changed = p.fill(path, "tls_key", p.managerOwn.TLSKey, &cfg.TLSKey, p.opts.Paths.keyFile(), "this Comb's serving key") || changed
	} else {
		for _, f := range []struct{ field, path string }{
			{"tls_cert", p.opts.Paths.certFile()},
			{"tls_key", p.opts.Paths.keyFile()},
		} {
			p.add(ActionNeeds, path, f.field, "",
				"not written, because this Comb has no usable serving certificate and pointing managerd at a file that is not there would make it fail to start rather than fail to secure itself - see the certificate refusal above")
		}
	}

	if p.managerOwn.NodeID == "" && cfg.NodeID == "" && p.NodeID != "" {
		cfg.NodeID = p.NodeID
		changed = true
		p.add(ActionFill, path, "node_id", p.NodeID,
			"this file had none; common.json is the shared home for it (ADR-0112), and this is written only so managerd's own file is complete on its own")
	} else {
		p.add(ActionKeep, path, "node_id", p.managerOwn.NodeID,
			"already set in this file; never changed")
	}

	// peer_tls_hostname_map is deliberately not written. ADR-0115
	// derives it from raft membership at runtime and refreshes it
	// periodically, so a value written at install time is a second
	// list that goes stale the moment membership changes - and before
	// this Comb has joined there is no membership to derive from at
	// all. The join flow is where that gap is closed.
	p.add(ActionKeep, path, "peer_tls_hostname_map", p.managerOwn.PeerTLSHostnameMap,
		"not written by design: ADR-0115 derives this from raft membership at runtime and manual entries take precedence, so an installer-written list would be a second source that goes stale; a Comb that has not joined has no membership to derive one from, and the join flow supplies it")

	p.add(ActionNeeds, path, "nat_uplink", cfg.NATUplink,
		"the interface a self-hosted VM reaches the network through; not derivable here")

	if !changed {
		return
	}
	p.writes = append(p.writes, func() error {
		return (&nodeconfig.Manager{Path: path}).Save(cfg)
	})
}

// planFrontendWrites and planRestshimdWrites are one function each
// rather than a shared one, because the two files differ in a way that
// matters: the frontend is reached from the operator's browser and so
// binds the wildcard deliberately, while restshimd is a full read/write
// control API with no authentication of its own and so is loopback-only.
// A shared helper would have had to take a boolean saying which of
// those two it was, which is the flag-flipping mistake
// internal/addrpolicy exists to prevent.
func (p *Plan) planFrontendWrites() {
	path := p.opts.Paths.frontendConfig()
	if p.refused(path) {
		return
	}
	cfg := p.frontCfg
	changed := false
	changed = p.fill(path, "manager_addr", p.frontOwn.ManagerAddr, &cfg.ManagerAddr, loopbackHost+":"+defaultManagerPort,
		"a LOCAL dial: frontend runs on this Comb and always reaches managerd over loopback, which is the one address whose certificate SAN is present (ADR-0139)") || changed
	changed = p.fill(path, "http_addr", p.frontOwn.HTTPAddr, &cfg.HTTPAddr, p.frontendHTTPAddr(),
		"the web UI, and the wildcard here is DELIBERATE: the browser is on the operator's machine, not on this Comb, so a loopback-only UI serves nobody") || changed
	if p.tlsUsable() {
		if !p.frontOwn.ManagerTLS {
			cfg.ManagerTLS = true
			changed = true
			p.add(ActionFill, path, "manager_tls", "true", "managerd serves TLS on this Comb, so the dial has to be TLS too (Part 1: TLS is on by default everywhere)")
		}
		changed = p.fill(path, "manager_tls_ca", p.frontOwn.ManagerTLSCA, &cfg.ManagerTLSCA, p.opts.Paths.certFile(),
			"managerd's certificate is self-signed, so the dial needs the certificate itself as its trust anchor") || changed
	}
	if !changed {
		return
	}
	p.writes = append(p.writes, func() error {
		return (&frontendconfig.Manager{Path: path}).Save(cfg)
	})
}

func (p *Plan) planRestshimdWrites() {
	path := p.opts.Paths.restshimdConfig()
	if p.refused(path) {
		return
	}
	cfg := p.restCfg
	changed := false
	changed = p.fill(path, "manager_addr", p.restOwn.ManagerAddr, &cfg.ManagerAddr, loopbackHost+":"+defaultManagerPort,
		"a LOCAL dial, and the loopback default is also what internal/restshimdconfig would have used with no config file at all") || changed
	changed = p.fill(path, "http_addr", p.restOwn.HTTPAddr, &cfg.HTTPAddr, p.restshimdHTTPAddr(),
		"a full read/write control API - create, update and delete VMs, jails and networks, migrate, upload ISOs - with NO AUTHENTICATION OF ITS OWN, so it stays on loopback and is reached over an SSH forward") || changed
	if p.tlsUsable() {
		if !p.restOwn.ManagerTLS {
			cfg.ManagerTLS = true
			changed = true
			p.add(ActionFill, path, "manager_tls", "true", "managerd serves TLS on this Comb, so the dial has to be TLS too")
		}
		changed = p.fill(path, "manager_tls_ca", p.frontOwn.ManagerTLSCA, &cfg.ManagerTLSCA, p.opts.Paths.certFile(),
			"managerd's certificate is self-signed, so the dial needs the certificate itself as its trust anchor") || changed
	}
	if !changed {
		return
	}
	p.writes = append(p.writes, func() error {
		return (&restshimdconfig.Manager{Path: path}).Save(cfg)
	})
}

// frontendHTTPAddr and restshimdHTTPAddr are the overridable web
// listeners. The defaults are the Makefile's own, so a host that
// overrode NODE_HTTP_ADDR through setup-quick keeps the value it had.
func (p *Plan) frontendHTTPAddr() string {
	if v := strings.TrimSpace(p.opts.FrontendHTTPAddr); v != "" {
		return v
	}
	return "0.0.0.0:" + defaultFrontendPort
}

func (p *Plan) restshimdHTTPAddr() string {
	if v := strings.TrimSpace(p.opts.RestshimdHTTPAddr); v != "" {
		return v
	}
	return loopbackHost + ":" + defaultRestshimPort
}

// tlsUsable reports whether this Comb ends the install with a serving
// certificate it can point a daemon at. It is the single fact behind
// Part 4's rule that a Comb with no usable certificate is not ready,
// and it is checked before any tls_* field is written rather than
// after, because a config file naming a certificate that is not there
// is a Comb that will not start.
func (p *Plan) tlsUsable() bool {
	return p.CertExists || p.pendingTLSDir
}

// fill fills one string field, recording either the fill or the reason
// it was left alone.
//
// The presence test is on own - what the FILE set - and the write is
// into dst - what the daemon will see. That split is the whole
// installer, and getting it wrong is not a cosmetic bug: raftd's and
// frontend's and restshimd's Loaders start from their own Defaults(),
// so a missing field comes back already populated with the value the
// daemon would have used anyway. Deciding on that value would make
// every one of those fields read as "already set here", the report
// would claim to have preserved fields the file never contained, and
// a host whose data directory is not the default would keep the
// default forever with nothing ever saying so.
//
// An empty want means the value could not be derived at all, which is
// reported as a need rather than written as an empty string: a config
// file with an explicit empty field and one with the field absent are
// not the same file to the next person reading it.
//
// Note what is NOT a reason to skip: a value already in effect from a
// package default. Defaults are what the daemon would have used with no
// config file at all, and this tool's job is to write down the layout
// this Comb actually runs, which is the same thing on a stock host and
// a different thing on a host whose directories were moved. Only a
// value some OTHER FILE chose is left alone, and each of those is
// handled where it is known rather than by a general rule here.
func (p *Plan) fill(path, field, own string, dst *string, want, reason string) bool {
	if own != "" {
		p.add(ActionKeep, path, field, own, "already set in this file; left exactly as it is")
		return false
	}
	if want == "" {
		p.add(ActionNeeds, path, field, "", reason)
		return false
	}
	*dst = want
	p.add(ActionFill, path, field, want, reason)
	return true
}

// needHostFact reports a value that only an operator can supply, and
// fills it when they did.
func (p *Plan) needHostFact(path, field, own string, dst *string, want, reason string) bool {
	if own != "" {
		p.add(ActionKeep, path, field, own, "already set in this file; left exactly as it is")
		return false
	}
	if want == "" {
		p.add(ActionNeeds, path, field, "", reason)
		return false
	}
	*dst = want
	p.add(ActionFill, path, field, want, reason)
	return true
}

// RefusedError is returned by Apply when the install completed the
// writes it was allowed to make but refused others. It is an error so
// a script sees a non-zero exit, and it is an error rather than a
// silent success because "configured" and "fully configured" are
// different claims and only the operator can tell which one they got.
type RefusedError struct {
	Paths []string
}

func (e *RefusedError) Error() string {
	return "hostinstall: " + strings.Join(e.Paths, ", ") + " " +
		plural(len(e.Paths), "was", "were") + " left alone as described above"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Apply performs the writes the plan recorded, in order, and returns
// the first failure. It does not re-read anything and it does not
// re-decide: a plan is a plan, and a second opinion halfway through
// would make the report and the result disagree.
func (p *Plan) Apply() error {
	for _, w := range p.writes {
		if err := w(); err != nil {
			return err
		}
	}
	if r := p.Refusals(); len(r) > 0 {
		seen := map[string]bool{}
		var paths []string
		for _, a := range r {
			if !seen[a.Path] {
				seen[a.Path] = true
				paths = append(paths, a.Path)
			}
		}
		return &RefusedError{Paths: paths}
	}
	return nil
}

// Report renders the plan. Every line says what was decided, and a
// refusal or a need says what has to happen instead. Nothing here can
// print key material: the only value the plan holds for key.pem is the
// literal "(not shown)".
func (p *Plan) Report(w io.Writer) {
	fmt.Fprintf(w, "apiaryctl install: plan for %s\n", p.opts.Paths.ConfigDir)
	if p.opts.ColonyMember != "" {
		fmt.Fprintf(w, "  a Colony member exists at %s, so this Comb is configured to join rather than to stand alone\n", p.opts.ColonyMember)
	}
	fmt.Fprintln(w)
	for _, a := range p.acts {
		writeAction(w, a)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s\n", p.summary())
}

// writeAction renders one line, indented and prefixed by its kind so
// that a report can be skimmed for the refusals and the needs, which
// are the two kinds an operator has to do something about.
func writeAction(w io.Writer, a Action) {
	switch a.Kind {
	case ActionCreate:
		fmt.Fprintf(w, "  create  %s\n", a.Path)
	case ActionFill:
		fmt.Fprintf(w, "  fill    %s: %s = %s\n", a.Path, a.Field, a.Value)
	case ActionKeep:
		fmt.Fprintf(w, "  keep    %s: %s", a.Path, a.Field)
		if a.Value != "" {
			fmt.Fprintf(w, " = %s", a.Value)
		}
		fmt.Fprintln(w)
	case ActionRefuse:
		fmt.Fprintf(w, "  REFUSE  %s", a.Path)
		if a.Field != "" {
			fmt.Fprintf(w, ": %s", a.Field)
		}
		fmt.Fprintf(w, "\n          %s\n", a.Reason)
	case ActionNeeds:
		fmt.Fprintf(w, "  NEEDS   %s: %s\n", a.Path, a.Field)
		fmt.Fprintf(w, "          %s\n", a.Reason)
	}
	if a.Reason != "" && a.Kind != ActionRefuse && a.Kind != ActionNeeds {
		fmt.Fprintf(w, "          %s\n", a.Reason)
	}
}

// summary is the last line, and it is deliberately the one that says
// whether this Comb is ready. Part 4's rule is that a Comb with no
// usable certificate is not ready whatever else is true, because the
// first thing the join flow needs is something to pin.
func (p *Plan) summary() string {
	if !p.tlsUsable() {
		return "NOT READY: this Comb has no usable serving certificate, and TLS is not optional on any Comb. See the refusal above."
	}
	if r := p.Refusals(); len(r) > 0 {
		return fmt.Sprintf("%s refused, so this Comb is not fully configured. Everything else above was still done.", plural(len(r), "1 file was", fmt.Sprintf("%d files were", len(r))))
	}
	if n := p.Needs(); len(n) > 0 {
		return fmt.Sprintf("configured, with %s still needing a human. Every Comb is now TLS by default; the join itself is the next step (see docs/add-node-to-colony.md).", plural(len(n), "1 value", fmt.Sprintf("%d values", len(n))))
	}
	return "configured, and nothing is left to supply."
}

// writeFileAtomic writes body to path through a temporary file in the
// same directory, chmods it, and renames it into place, so a config
// file is never briefly readable at the wrong mode and never
// truncated in place. The cert is 0644 because every Comb that dials
// this one reads it; the key is 0600 because nothing but root should
// ever have.
func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".hostinstall-*.tmp")
	if err != nil {
		return fmt.Errorf("hostinstall: creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // a no-op once renamed
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("hostinstall: writing temp file for %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("hostinstall: syncing temp file for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("hostinstall: closing temp file for %s: %w", path, err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return fmt.Errorf("hostinstall: setting permissions on %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("hostinstall: renaming into place at %s: %w", path, err)
	}
	return nil
}

// ErrNotRoot is returned by the command layer when --apply is asked
// for without root. It lives here rather than in cmd/apiaryctl so the
// rule is stated once, next to the writes it protects.
var ErrNotRoot = errors.New("apiaryctl: --apply writes /usr/local/etc/apiary and /usr/local/etc/apiary-tls as root, so it needs an effective uid of 0")
