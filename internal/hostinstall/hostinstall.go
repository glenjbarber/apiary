// Package hostinstall is the plan/apply logic behind `apiaryctl
// install`: it turns a bare FreeBSD host that has the Apiary binaries
// installed into a configured Comb, and it is written so that every
// decision it makes is reportable before any of them happen.
//
// The posture is the one internal/install already takes, for the same
// reason: the interesting output of an installer is what it decided
// NOT to do. A tool that prints "wrote 5 files" tells an operator
// nothing they did not already know; a tool that prints which of the
// forty fields in managerd.json it left alone, and why, is the one
// that can be trusted to be re-run on a host that is already in
// production. So New() reads and decides, writes nothing, and returns
// a plan whose every line is either an action or a refusal; Apply()
// then does exactly the writes the plan recorded, and nothing else.
//
// The load-bearing rule is preservation. An installer that overwrites
// is worse than no installer, because it converts "I have not
// configured this" into "I have configured this wrongly and did not
// tell you", and the damage is silent because the file is still valid
// JSON afterwards. Four consequences, all of them tested here:
//
//   - A file that exists and does not parse is refused outright, with
//     its path and the parse error. It may be a hand edit, a
//     half-finished change, or a partially written file from an
//     interrupted run, and there is no way to tell those apart from
//     the bytes.
//   - Present fields are never replaced. Only absent fields are
//     filled, and a field the file sets to its own default is
//     therefore still "present" and still preserved.
//   - A node_id that is already set is never changed, and a
//     disagreement between common.json and a daemon's own file is
//     reported and changes nothing. A different node_id is a different
//     raft member; writing one orphans existing state and produces a
//     node that can never win an election.
//   - Half a TLS pair is refused rather than completed, because a
//     half-pair is an incident to look at rather than a gap to fill.
//
// Every read goes through the same config packages the daemons
// themselves use - internal/nodeconfig, internal/raftdconfig,
// internal/frontendconfig, internal/restshimdconfig,
// internal/commonconfig - and every write goes back through their own
// Save, which is already atomic and 0600. A second list of fields and
// a second set of rules would drift from the first, and the drift
// would be silent: the daemon would validate a field the installer
// never knew about, or the installer would write one the daemon
// rejects.
package hostinstall

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/commonconfig"
	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
	"github.com/glenjbarber/apiary/internal/restshimdconfig"
)

// Paths are the directories a Comb's configuration lives in. They are
// a struct rather than constants so the whole installer can be pointed
// at a fixture directory, which is the only reason any of this is
// testable on the machine the tests actually run on.
type Paths struct {
	ConfigDir string // /usr/local/etc/apiary
	TLSDir    string // /usr/local/etc/apiary-tls
	DataDir   string // /var/db/apiary/raftd
	ISODir    string // /var/db/apiary/isos
	RunDir    string // /var/run/apiary
}

// DefaultPaths is where a pkg-installed FreeBSD system puts all of it.
func DefaultPaths() Paths {
	return Paths{
		ConfigDir: "/usr/local/etc/apiary",
		TLSDir:    "/usr/local/etc/apiary-tls",
		DataDir:   "/var/db/apiary/raftd",
		ISODir:    "/var/db/apiary/isos",
		RunDir:    "/var/run/apiary",
	}
}

// withDefaults fills any empty field from DefaultPaths, so a caller
// that only wants to move the config directory gets the rest of the
// real layout rather than five empty strings that become five writes
// relative to the working directory.
func (p Paths) withDefaults() Paths {
	d := DefaultPaths()
	if p.ConfigDir == "" {
		p.ConfigDir = d.ConfigDir
	}
	if p.TLSDir == "" {
		p.TLSDir = d.TLSDir
	}
	if p.DataDir == "" {
		p.DataDir = d.DataDir
	}
	if p.ISODir == "" {
		p.ISODir = d.ISODir
	}
	if p.RunDir == "" {
		p.RunDir = d.RunDir
	}
	return p
}

func (p Paths) commonConfig() string    { return filepath.Join(p.ConfigDir, "common.json") }
func (p Paths) managerdConfig() string  { return filepath.Join(p.ConfigDir, "managerd.json") }
func (p Paths) raftdConfig() string     { return filepath.Join(p.ConfigDir, "raftd.json") }
func (p Paths) frontendConfig() string  { return filepath.Join(p.ConfigDir, "frontend.json") }
func (p Paths) restshimdConfig() string { return filepath.Join(p.ConfigDir, "restshimd.json") }
func (p Paths) certFile() string        { return filepath.Join(p.TLSDir, "cert.pem") }
func (p Paths) keyFile() string         { return filepath.Join(p.TLSDir, "key.pem") }

// The default listening addresses. They are the same values the
// Makefile's setup-quick writes, and each one is here for a stated
// reason rather than by inheritance:
//
//   - managerd's rpc_addr is the subject of ADR-0139. A loopback value
//     is right for a one-Comb Apiary because every daemon that dials
//     managerd is on the same host and the certificate carries
//     IP:127.0.0.1; a value peers can dial has to be a name the
//     certificate covers, never a wildcard and never a numeric LAN
//     address.
//   - the frontend's http_addr is the wildcard DELIBERATELY. The
//     browser is on the operator's machine, not on the Comb, so a
//     loopback-only UI serves nobody.
//   - restshimd's http_addr is loopback because it is a full read/write
//     control API with no authentication of its own.
const (
	defaultManagerPort  = "17700"
	defaultFrontendPort = "8080"
	defaultRestshimPort = "8081"
	defaultPAMService   = "apiary"
	loopbackHost        = "127.0.0.1"
)

// ActionKind is what a plan line says about one field or one file.
type ActionKind string

const (
	// ActionCreate is a file that does not exist and would be written.
	ActionCreate ActionKind = "create"
	// ActionFill is an absent field in an existing file that would be
	// filled.
	ActionFill ActionKind = "fill"
	// ActionKeep is a present field deliberately left alone, always
	// carrying the reason.
	ActionKeep ActionKind = "keep"
	// ActionRefuse is something the installer will not do at all, and
	// why. A refusal blocks writes to its own file and nothing else.
	ActionRefuse ActionKind = "refuse"
	// ActionNeeds is a value that could not be derived from anything
	// this program can see and that a human has to supply. These are
	// never guessed: a plausible default written into a valid config
	// file is the exact failure this package exists to prevent.
	ActionNeeds ActionKind = "needs"
)

// Action is one line of the report, and the unit Apply acts on. Field
// is empty for a whole-file line; Value is empty where there is
// nothing to show, which is a refusal and a need.
type Action struct {
	Kind   ActionKind
	Path   string
	Field  string
	Value  string
	Reason string
}

// Options is everything the installer is told. Every field that
// changes behaviour is a host fact a human has to supply, because
// nothing here can be derived from a config file that does not exist
// yet.
type Options struct {
	// Paths overrides the default directories. Every empty field falls
	// back to DefaultPaths.
	Paths Paths

	// ColonyMember, when set, is a host:port of a Colony member that
	// already exists. It is the one flag that changes what gets
	// written rather than only what gets reported: with it, this Comb
	// is a member of a Colony and its managerd address has to be
	// dialable by name.
	ColonyMember string

	// Hostname is this Comb's name. Empty means os.Hostname(). It is
	// overridable so a test can install onto a fixture directory
	// without depending on the machine it runs on.
	Hostname string

	// FQDN is the host's resolvable name, when it has one. Empty means
	// "derive it from Hostname if Hostname already looks like one", and
	// deliberately does not mean "look it up": a resolver answer that
	// changes between the report and the write is a certificate nobody
	// can reason about afterwards.
	FQDN string

	// ZFSBase, Uplink and BhyveBridge are host facts this program
	// cannot see. Absent, they are reported as needing a human rather
	// than guessed, because a plausible default written into a valid
	// config file is the failure mode this whole package is built
	// against.
	ZFSBase     string
	Uplink      string
	BhyveBridge string

	// BhyveBootROM is the UEFI firmware file bhyve boots from. The
	// package carrying it is sometimes the firmware package and
	// sometimes edk2-bhyve, so locating it is a package question rather
	// than a filesystem one: it is supplied or it is reported as a
	// need.
	BhyveBootROM string

	// RPCAddr overrides the address managerd listens on. The override
	// is checked against the certificate rather than trusted, because
	// the whole subject of ADR-0139 is an operator copying a bind
	// address into a field.
	RPCAddr string

	// FrontendHTTPAddr and RestshimdHTTPAddr override the two web
	// listeners. They default to the values the Makefile's own
	// variables defaulted to, so `make setup-quick NODE_HTTP_ADDR=...`
	// keeps working and keeps meaning what it meant.
	FrontendHTTPAddr  string
	RestshimdHTTPAddr string

	// Now overrides the clock, for tests.
	Now time.Time
}

// Plan is a read, a set of decisions, and a list of the writes those
// decisions imply. Construct one with New, report it with Report, and
// carry it out with Apply.
type Plan struct {
	opts Options
	acts []Action

	// writes are the side effects the decisions imply, run by Apply in
	// this order and nowhere else. Keeping them as closures rather
	// than as a switch on a file name is what makes "a file that is
	// fully preserved is not written at all" true by construction:
	// no write is registered for it.
	writes []func() error

	// blocked is the set of files nothing may be written to, which is
	// NOT the same as "has a refusal somewhere in the report". A
	// refusal about one field - an rpc_addr no certificate covers -
	// is a line in the report and leaves the rest of the file alone,
	// because a Comb with a good config file and one address needing
	// attention is much closer to running than a Comb with no config
	// file at all. A refusal about the file itself - it does not
	// parse, it carries fields this binary does not know, or its
	// node_id is in dispute with another file's - blocks it.
	blocked map[string]bool

	// What the host is called, decided once so the generated
	// certificate and the derived node_id cannot disagree about it.
	short    string
	hostname string

	// What the config files say, split in two because the split is
	// the whole installer. "own" is what the file itself sets; "cfg"
	// is what the daemon will see after its own Load has applied
	// defaults and the common.json fallback. A field can be set in cfg
	// and empty in own, and that is precisely the case this tool fills.
	common     commonconfig.Config
	managerOwn nodeconfig.Config
	managerCfg nodeconfig.Config
	raftdOwn   raftdconfig.Config
	raftdCfg   raftdconfig.Config
	frontOwn   frontendconfig.Config
	frontCfg   frontendconfig.Config
	restOwn    restshimdconfig.Config
	restCfg    restshimdconfig.Config

	// The certificate, whether it was found on disk or planned, and
	// the bytes to write when it was planned. pendingCert stays nil
	// when an existing pair was kept, which is what makes a re-run a
	// no-op rather than a rewrite.
	pendingCert   []byte
	pendingKey    []byte
	pendingTLSDir bool

	// The derived facts a caller may want to assert on, and which the
	// tests assert on directly rather than re-parsing files to find
	// out what the installer decided.
	NodeID     string
	RPCAddr    string
	Colony     bool
	CertExists bool
	Cert       hostcert.Info
}

// New reads the host and decides what an install would do. It writes
// nothing: not a directory, not a file, not a temporary file. Every
// refusal it records blocks writes to its own file only, so a
// malformed frontend.json does not stop the TLS pair being issued.
func New(opts Options) (*Plan, error) {
	opts.Paths = opts.Paths.withDefaults()
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	p := &Plan{opts: opts}
	p.resolveHostNames()
	// The config files are read before anything is decided, because
	// two of the decisions - whether this Comb is standalone, and what
	// its node_id already is - are facts those files hold. Deciding
	// first and reading second would mean deciding them from a file
	// that was never opened.
	if err := p.loadConfigs(); err != nil {
		return nil, err
	}
	if err := p.planTLS(); err != nil {
		return nil, err
	}
	if err := p.planIdentity(); err != nil {
		return nil, err
	}
	p.planRPCAddr()
	p.planWrites()
	return p, nil
}

// Actions returns the plan's lines, in the order they were decided.
func (p *Plan) Actions() []Action { return p.acts }

// Refusals returns just the lines that blocked a write.
func (p *Plan) Refusals() []Action {
	var out []Action
	for _, a := range p.acts {
		if a.Kind == ActionRefuse {
			out = append(out, a)
		}
	}
	return out
}

// Needs returns just the values that a human still has to supply.
func (p *Plan) Needs() []Action {
	var out []Action
	for _, a := range p.acts {
		if a.Kind == ActionNeeds {
			out = append(out, a)
		}
	}
	return out
}

// Dir returns the config directory this plan was made against, which
// the CLI needs for its summary line and the tests need to find the
// files they assert about.
func (p *Plan) Dir() string { return p.opts.Paths.ConfigDir }

func (p *Plan) add(kind ActionKind, path, field, value, reason string) {
	p.acts = append(p.acts, Action{Kind: kind, Path: path, Field: field, Value: value, Reason: reason})
}

// refuseFile records a refusal that blocks every write to a file, and
// is the only way to block one. Every other refusal is a report line.
func (p *Plan) refuseFile(path, field, reason string) {
	if p.blocked == nil {
		p.blocked = map[string]bool{}
	}
	p.blocked[path] = true
	p.add(ActionRefuse, path, field, "", reason)
}

// refused reports whether a file is blocked, so the writers below skip
// it entirely rather than half-filling it.
func (p *Plan) refused(path string) bool { return p.blocked[path] }

// loadConfigs reads the five config files. Each is read twice on
// purpose: once through the owning package's Loader, which decides
// whether the file is usable at all, and once into that same package's
// Config type, which is the only way to learn which fields the FILE
// sets rather than which fields the file will end up with after
// defaults and the common.json fallback. The second read uses the
// package's own field set, so "what fields exist" is never a second
// list maintained here.
func (p *Plan) loadConfigs() error {
	commonPath := p.opts.Paths.commonConfig()
	if _, err := (&commonconfig.Manager{Path: commonPath}).Load(); err != nil {
		// The package's own Loader decides whether the file is usable,
		// so that "does not parse" means here exactly what it means to
		// every daemon that will read the file afterwards. Its error
		// already names the path and the parse failure, which is the
		// whole message this refusal needs.
		p.refuseFile(commonPath, "", err.Error())
	} else if err := loadOwn(commonPath, &p.common); err != nil {
		p.refuseFile(commonPath, "", err.Error())
	}
	if err := p.loadManagerd(); err != nil {
		return err
	}
	if err := p.loadRaftd(); err != nil {
		return err
	}
	if err := p.loadFrontend(); err != nil {
		return err
	}
	if err := p.loadRestshimd(); err != nil {
		return err
	}
	return nil
}

func (p *Plan) loadManagerd() error {
	path := p.opts.Paths.managerdConfig()
	cfg, err := (&nodeconfig.Manager{Path: path}).Load()
	if err != nil {
		p.refuseFile(path, "", err.Error())
		return nil
	}
	p.managerCfg = cfg
	if err := loadOwn(path, &p.managerOwn); err != nil {
		p.refuseFile(path, "", err.Error())
	}
	return nil
}

func (p *Plan) loadRaftd() error {
	path := p.opts.Paths.raftdConfig()
	cfg, err := (&raftdconfig.Manager{Path: path}).Load()
	if err != nil {
		p.refuseFile(path, "", err.Error())
		return nil
	}
	p.raftdCfg = cfg
	if err := loadOwn(path, &p.raftdOwn); err != nil {
		p.refuseFile(path, "", err.Error())
	}
	return nil
}

func (p *Plan) loadFrontend() error {
	path := p.opts.Paths.frontendConfig()
	cfg, err := (&frontendconfig.Manager{Path: path}).Load()
	if err != nil {
		p.refuseFile(path, "", err.Error())
		return nil
	}
	p.frontCfg = cfg
	if err := loadOwn(path, &p.frontOwn); err != nil {
		p.refuseFile(path, "", err.Error())
	}
	return nil
}

func (p *Plan) loadRestshimd() error {
	path := p.opts.Paths.restshimdConfig()
	cfg, err := (&restshimdconfig.Manager{Path: path}).Load()
	if err != nil {
		p.refuseFile(path, "", err.Error())
		return nil
	}
	p.restCfg = cfg
	if err := loadOwn(path, &p.restOwn); err != nil {
		p.refuseFile(path, "", err.Error())
	}
	return nil
}

// loadOwn parses a config file into out, treating a missing file as
// "nothing set", exactly as every one of these packages' Loaders does.
// The caller has already run the owning package's Loader, which is what
// decided the file is usable at all.
//
// A file carrying a key this binary's Config type does not know is
// refused, and this is a stronger rule than "does not parse". Two of
// these loaders are lenient about unknown keys, so a managerd.json
// written by a NEWER managerd - or one with a field name misspelled -
// would load here perfectly well and then be rewritten without the
// field, silently deleting an operator's setting to produce a file that
// still parses. Refusing is the only answer that keeps the installer's
// promise to leave a Comb it does not understand completely alone.
func loadOwn[T any](path string, out *T) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("hostinstall: reading %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("hostinstall: parsing %s: %w", path, err)
	}
	return checkKnownKeys(path, raw, *out)
}

// checkKnownKeys refuses a file whose JSON keys are not all in the
// Config type's own field set. The set is read from the type by
// reflection rather than written out here, so a field added to any
// config package is automatically known to the next installer without
// anyone remembering to update a list - which is the whole reason this
// package reads and writes through the config packages instead of
// keeping its own.
func checkKnownKeys(path string, raw []byte, cfg any) error {
	var present map[string]json.RawMessage
	if err := json.Unmarshal(raw, &present); err != nil {
		return nil // Load already reported the parse failure with better wording.
	}
	known := map[string]bool{}
	t := reflect.TypeOf(cfg)
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		known[name] = true
	}
	var unknown []string
	for k := range present {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("hostinstall: %s carries %d field(s) this binary does not know (%s); refusing rather than rewriting, because saving it would delete them - either it was written by a newer managerd, or a field name is misspelled",
		path, len(unknown), strings.Join(unknown, ", "))
}

// resolveHostNames works out the short hostname and the FQDN once.
func (p *Plan) resolveHostNames() {
	host := strings.TrimSpace(p.opts.Hostname)
	if host == "" {
		host, _ = os.Hostname()
	}
	p.short = host
	if i := strings.Index(host, "."); i > 0 {
		p.short = host[:i]
		if p.opts.FQDN == "" {
			p.opts.FQDN = host
		}
	}
	p.opts.FQDN = strings.TrimSpace(p.opts.FQDN)
	if p.opts.FQDN == p.short {
		// A name that is its own first label is not an FQDN, and
		// carrying it in a certificate twice under two roles is
		// confusing rather than harmless.
		p.opts.FQDN = ""
	}
}

// planTLS decides what happens to the serving certificate. Three
// outcomes, and the middle one is the important one:
//
//   - a valid pair exists: keep it, and report its SANs, expiry and
//     fingerprint, because those are the facts every other part of
//     this tool is about to assume;
//   - exactly one of the pair exists: refuse, and say which one is
//     missing, because a half-pair is an incident to look at;
//   - neither exists: plan a new pair, and say what it will carry
//     without claiming a fingerprint, because at report time no
//     certificate has been issued and printing a digest of bytes that
//     do not exist would be a lie an operator could copy.
func (p *Plan) planTLS() error {
	certPath, keyPath := p.opts.Paths.certFile(), p.opts.Paths.keyFile()
	certExists, err := fileExists(certPath)
	if err != nil {
		return err
	}
	keyExists, err := fileExists(keyPath)
	if err != nil {
		return err
	}

	switch {
	case certExists && keyExists:
		certPEM, err := os.ReadFile(certPath)
		if err != nil {
			return fmt.Errorf("hostinstall: reading %s: %w", certPath, err)
		}
		keyPEM, err := os.ReadFile(keyPath)
		if err != nil {
			return fmt.Errorf("hostinstall: reading %s: %w", keyPath, err)
		}
		info, err := hostcert.Evaluate(certPEM, hostcert.EvaluateOptions{
			Now:                p.opts.Now,
			RequireLoopbackSAN: true,
			RequireDNSSAN:      true,
			KeyPEM:             keyPEM,
		})
		if err != nil {
			p.refuseTLS(certPath, keyPath, err.Error())
			return nil
		}
		p.Cert = info
		p.CertExists = true
		p.add(ActionKeep, certPath, "certificate", info.Fingerprint,
			"a usable pair is already here; it is left exactly as it is")
		p.add(ActionKeep, keyPath, "private key", "(not shown)",
			"never printed, never rewritten")
		p.add(ActionKeep, certPath, "validity", info.Expiry(),
			fmt.Sprintf("subject %s, %s-%d, %s", info.Subject, info.KeyType, info.KeyBits, info.Signature))
		p.add(ActionKeep, certPath, "subject alternative names", info.SANs(),
			"these are the names every peer Comb will verify this Comb against")
		return nil

	case certExists != keyExists:
		missing, present := keyPath, certPath
		if keyExists {
			missing, present = certPath, keyPath
		}
		p.refuseTLS(certPath, keyPath, fmt.Sprintf(
			"%s exists but %s does not: a half-pair is an incident to look at, not a gap to fill, and completing it here would issue a certificate that does not match the key already on disk",
			present, missing))
		return nil
	}

	// Neither exists. Plan the pair now, in memory, so the report and
	// the address selection can both be exact about what it will
	// carry. The bytes are discarded: nothing reaches the disk until
	// Apply runs.
	certPEM, keyPEM, err := p.generateCert()
	if err != nil {
		p.refuseTLS(certPath, keyPath, err.Error())
		return nil
	}
	info, err := hostcert.Evaluate(certPEM, hostcert.EvaluateOptions{
		Now:                p.opts.Now,
		RequireLoopbackSAN: true,
		RequireDNSSAN:      true,
		KeyPEM:             keyPEM,
	})
	if err != nil {
		// A pair this package generated failing its own gate is a bug
		// here rather than a host problem, so it is returned rather
		// than recorded as a refusal an operator would be invited to
		// act on.
		return fmt.Errorf("hostinstall: the certificate this installer generated failed its own gate: %w", err)
	}
	p.Cert = info
	p.pendingCert, p.pendingKey = certPEM, keyPEM
	p.pendingTLSDir = true
	p.add(ActionCreate, certPath, "certificate", info.SANs(),
		"self-signed ECDSA P-256, "+info.Expiry()+"; no fingerprint is shown because none has been issued yet, and the one on disk after --apply is a different certificate from this one")
	p.add(ActionCreate, keyPath, "private key", "(not shown)",
		"generated at apply time, 0600, never printed")
	return nil
}

func (p *Plan) refuseTLS(certPath, keyPath, reason string) {
	p.add(ActionRefuse, certPath, "certificate", "", reason)
	p.add(ActionRefuse, keyPath, "private key", "", "left alone together with the certificate it belongs to")
}

// generateCert issues the pair this install would write. The SANs are
// the short hostname, the FQDN when there is one, and loopback - and
// deliberately not the host's LAN address, which is ADR-0139's subject:
// a numeric LAN address in a certificate is a value that works until
// something dials it, and then fails verification for a reason that
// reads like a network fault.
func (p *Plan) generateCert() (certPEM, keyPEM []byte, err error) {
	return hostcert.Generate(hostcert.GenerateOptions{
		CommonName:  p.short,
		DNSNames:    p.dnsNames(),
		IPAddresses: []net.IP{net.ParseIP(loopbackHost)},
		Now:         p.opts.Now,
	})
}

func (p *Plan) dnsNames() []string {
	names := []string{p.short}
	if p.opts.FQDN != "" {
		names = append(names, p.opts.FQDN)
	}
	return names
}

// planIdentity works out this Comb's node_id and hostname, and records
// where each came from. The order is the ADR's: an existing value
// first, in common.json and then in the daemon's own file, then the
// host's FQDN, then the short hostname.
//
// The last two steps are a fallback for a genuinely fresh host and are
// reported as exactly that. A node_id derived from a hostname is right
// on a single-node Colony and wrong the moment this Comb is renamed,
// which is why it is never written silently over a file that already
// carries an identity.
func (p *Plan) planIdentity() error {
	commonPath := p.opts.Paths.commonConfig()
	if p.refused(commonPath) {
		return nil
	}
	own := p.daemonNodeIDs()
	ids := map[string]string{}
	for _, d := range own {
		if d.id != "" {
			ids[d.path] = d.id
		}
	}
	if p.common.NodeID != "" {
		ids[commonPath] = p.common.NodeID
	}
	if disagree, detail := nodeIDDisagreement(ids); disagree {
		// A dispute about this Comb's identity blocks every file the
		// dispute touches, not just the node_id fields in it: writing
		// a correct raftd_socket beside a contested identity produces
		// a Comb that looks configured and is not.
		for path, id := range ids {
			p.refuseFile(path, "node_id", detail+fmt.Sprintf(" (this file says %q)", id))
		}
		return nil
	}

	p.NodeID = firstNonEmpty(own[0].id, own[1].id, p.opts.FQDN, p.short)
	p.hostname = firstNonEmpty(p.common.Hostname, p.opts.FQDN, p.short)
	return nil
}

// daemonNodeID is one daemon config file's own node_id and its path.
type daemonNodeID struct {
	path string
	id   string
}

func (p *Plan) daemonNodeIDs() [2]daemonNodeID {
	return [2]daemonNodeID{
		{p.opts.Paths.managerdConfig(), p.managerOwn.NodeID},
		{p.opts.Paths.raftdConfig(), p.raftdOwn.NodeID},
	}
}

// nodeIDDisagreement reports whether two files carry two different
// node_ids, and says so in a sentence an operator can act on. The same
// value in two files is not a disagreement, and one populated file is
// not a disagreement either.
func nodeIDDisagreement(ids map[string]string) (bool, string) {
	first, firstPath := "", ""
	for path, id := range ids {
		if first == "" {
			first, firstPath = id, path
			continue
		}
		if id != first {
			return true, fmt.Sprintf(
				"this Comb has two different node_id values (%s is %q and %s is %q); a different node_id is a different raft member, so writing one would orphan the other's state and produce a node that can never win an election - nothing has been changed",
				firstPath, first, path, id)
		}
	}
	return false, ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// planRPCAddr is ADR-0139 turned from a comment into code.
//
// Standalone means loopback, because every daemon that dials managerd
// is on this host and the certificate carries IP:127.0.0.1. Multi-Comb
// means a name, taken from the certificate's own DNS SANs, because a
// peer that dials a name gets verification and a peer that dials a
// numeric LAN address gets a failure that reads like a network fault.
// The tool never invents a name: if the certificate carries no usable
// one, the install refuses and prints the SANs it found, because
// writing an address the certificate does not cover reproduces exactly
// the incident this exists to make impossible.
func (p *Plan) planRPCAddr() {
	if override := strings.TrimSpace(p.opts.RPCAddr); override != "" {
		p.planRPCAddrOverride(override)
		return
	}
	if p.standalone() {
		p.RPCAddr = loopbackHost + ":" + defaultManagerPort
		p.add(ActionKeep, p.opts.Paths.managerdConfig(), "rpc_addr", p.RPCAddr,
			"standalone: nothing off this host dials managerd, and loopback is the value managerd would have used with no config file at all")
		return
	}
	p.Colony = true
	name := p.colonyName()
	if name == "" {
		p.add(ActionRefuse, p.opts.Paths.managerdConfig(), "rpc_addr", "",
			fmt.Sprintf("this Comb is part of a Colony, so managerd needs an address its peers can dial and verify, but the certificate carries no usable DNS name (found: %s) - refusing to write an address the certificate does not cover", p.Cert.SANs()))
		return
	}
	p.RPCAddr = name + ":" + defaultManagerPort
	p.add(ActionFill, p.opts.Paths.managerdConfig(), "rpc_addr", p.RPCAddr,
		"a Colony member: the name is taken from this Comb's own certificate, never invented")
}

// planRPCAddrOverride handles an explicitly supplied rpc_addr, which
// is what `make setup-quick NODE_RPC_ADDR=...` has always passed. The
// override is checked rather than trusted, and the checks are the
// reasons ADR-0139 exists:
//
//   - a numeric address that is not loopback is refused outright,
//     because a Comb's certificate carries no IP SAN for its LAN
//     address, so every peer that dialled it would fail verification
//     for a reason that reads like a network fault;
//   - a wildcard is refused, because it is a legal bind address and an
//     illegal destination, and this value is read out of a config file
//     that other daemons dial;
//   - a name is accepted only if this Comb's own certificate covers it,
//     so an operator cannot write a name that no peer will be able to
//     verify.
func (p *Plan) planRPCAddrOverride(override string) {
	path := p.opts.Paths.managerdConfig()
	host, port, err := net.SplitHostPort(override)
	if err != nil {
		p.add(ActionRefuse, path, "rpc_addr", override,
			fmt.Sprintf("--rpc-addr %q is not a host:port, so it cannot be checked against anything: %v", override, err))
		return
	}
	if port == "" {
		port = defaultManagerPort
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			p.add(ActionRefuse, path, "rpc_addr", override,
				fmt.Sprintf("--rpc-addr is the numeric address %s, and this Comb's certificate carries no IP SAN for a LAN address, so every peer that dialled it would fail verification (ADR-0139); pass a name the certificate covers instead", host))
			return
		}
	} else if !p.certHasDNS(host) {
		p.add(ActionRefuse, path, "rpc_addr", override,
			fmt.Sprintf("--rpc-addr names %q, which this Comb's certificate does not cover (it carries: %s), so no peer would be able to verify this Comb under that name", host, p.Cert.SANs()))
		return
	}
	p.Colony = !isLoopbackHostPort(override)
	p.RPCAddr = net.JoinHostPort(host, port)
	p.add(ActionFill, path, "rpc_addr", p.RPCAddr,
		"supplied with --rpc-addr and checked against this Comb's own certificate rather than trusted")
}

// standalone decides whether this Comb is on its own. Three signals,
// and the third is a correction to the ADR's original wording, kept
// here because it is the difference between an installer that is
// idempotent and one that is not.
//
// The ADR named "the certificate carries a DNS SAN for a name that is
// not this host's short name" as the third signal. It cannot be used
// as written: Part 1 puts the FQDN into the certificate's SANs, so a
// single-node Comb whose hostname is an FQDN - the normal case on a
// real network - would read as a Colony member on its own second run
// and rewrite rpc_addr from loopback to a name. The signal used
// instead is managerd.json's own rpc_addr already naming a non-loopback
// host: a fact about what was configured, rather than an inference
// from a certificate.
func (p *Plan) standalone() bool {
	if strings.TrimSpace(p.opts.ColonyMember) != "" {
		return false
	}
	if p.raftdOwn.RaftBind != "" && !isLoopbackHostPort(p.raftdOwn.RaftBind) {
		return false
	}
	if p.managerOwn.RPCAddr != "" && !isLoopbackHostPort(p.managerOwn.RPCAddr) {
		return false
	}
	return true
}

// colonyName picks the name peers will dial, from this Comb's own
// certificate and in a stated order: the FQDN, then the short
// hostname, then whatever DNS SAN the certificate happens to carry.
// The FQDN comes first because it is the one that resolves from
// another host without a local search domain.
func (p *Plan) colonyName() string {
	if p.opts.FQDN != "" && p.certHasDNS(p.opts.FQDN) {
		return p.opts.FQDN
	}
	if p.certHasDNS(p.short) {
		return p.short
	}
	if len(p.Cert.DNSNames) > 0 {
		return p.Cert.DNSNames[0]
	}
	return ""
}

func (p *Plan) certHasDNS(name string) bool {
	for _, d := range p.Cert.DNSNames {
		if strings.EqualFold(d, name) {
			return true
		}
	}
	return false
}

// isLoopbackHostPort reports whether a host:port names this host
// itself. A host that does not parse is not loopback, which is the
// conservative answer: it pushes the installer toward a resolvable
// name rather than toward a bind address peers cannot verify.
func isLoopbackHostPort(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("hostinstall: checking %s: %w", path, err)
}
