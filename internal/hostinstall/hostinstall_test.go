package hostinstall

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/joinauth"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

// fixtureNow is a fixed clock, so a generated certificate's dates are
// assertions about a known value rather than about whatever the machine
// running the tests thought the time was.
var fixtureNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// fixture is one test host: a directory tree, a Paths pointing at it,
// and the Options a caller would pass on a Comb called "brood" whose
// hostname is an FQDN, which is the normal case on a real network and
// the case the standalone detection has to get right.
type fixture struct {
	t    *testing.T
	root string
	opts Options
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	// Every path is inside the fixture. The defaults are deliberately
	// NOT used for any of them: a test that wrote to
	// /usr/local/etc/apiary because a default leaked through would
	// either fail on a non-root machine or, worse, pass on a root one
	// after touching the real host.
	return &fixture{
		t:    t,
		root: root,
		opts: Options{
			Paths: Paths{
				ConfigDir: filepath.Join(root, "etc", "apiary"),
				TLSDir:    filepath.Join(root, "etc", "apiary-tls"),
				DataDir:   filepath.Join(root, "db", "raftd"),
				ISODir:    filepath.Join(root, "db", "isos"),
				RunDir:    filepath.Join(root, "run"),
			},
			Hostname: "brood.lab3.home.arpa",
			Now:      fixtureNow,
		},
	}
}

func (f *fixture) path(parts ...string) string {
	return filepath.Join(append([]string{f.root}, parts...)...)
}

func (f *fixture) configPath(name string) string { return filepath.Join(f.opts.Paths.ConfigDir, name) }

// write puts a file in place, creating its directory, which is how a
// test seeds the "this already exists" cases.
func (f *fixture) write(path, body string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(path string) string {
	f.t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func (f *fixture) exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (f *fixture) plan() *Plan {
	f.t.Helper()
	p, err := New(f.opts)
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	return p
}

func (f *fixture) apply() error {
	f.t.Helper()
	return f.plan().Apply()
}

// installed applies a plan and fails the test if anything was refused,
// for the tests whose subject is what got written rather than what
// did not.
func (f *fixture) installed() *Plan {
	f.t.Helper()
	p := f.plan()
	if err := p.Apply(); err != nil {
		f.t.Fatalf("Apply on a fresh fixture: %v", err)
	}
	return p
}

// report renders a plan the way the command does.
func report(p *Plan) string {
	var buf bytes.Buffer
	p.Report(&buf)
	return buf.String()
}

// A fresh host gets every file, and every one of them is readable only
// by root. This is the baseline the rest of the file departs from.
func TestFreshHostIsFullyConfigured(t *testing.T) {
	f := newFixture(t)
	p := f.installed()

	if p.NodeID != "brood.lab3.home.arpa" {
		t.Errorf("NodeID = %q, want the FQDN", p.NodeID)
	}
	if p.RPCAddr != "127.0.0.1:17700" {
		t.Errorf("RPCAddr = %q, want loopback for a standalone Comb", p.RPCAddr)
	}
	if p.Colony {
		t.Error("Colony = true on a host with no member, no non-loopback raft_bind and no rpc_addr of its own")
	}

	common := f.read(f.configPath("common.json"))
	if !strings.Contains(common, `"node_id": "brood.lab3.home.arpa"`) {
		t.Errorf("common.json does not carry the node_id:\n%s", common)
	}
	managerd := f.read(f.configPath("managerd.json"))
	for _, want := range []string{
		`"rpc_addr": "127.0.0.1:17700"`,
		`"tls_cert"`,
		`"tls_key"`,
		`"pam_service": "apiary"`,
	} {
		if !strings.Contains(managerd, want) {
			t.Errorf("managerd.json is missing %s:\n%s", want, managerd)
		}
	}
	frontend := f.read(f.configPath("frontend.json"))
	if !strings.Contains(frontend, `"manager_addr": "127.0.0.1:17700"`) {
		t.Errorf("frontend.json manager_addr is not a local dial:\n%s", frontend)
	}
	if !strings.Contains(frontend, `"http_addr": "0.0.0.0:8080"`) {
		t.Errorf("frontend.json http_addr is not the wildcard, and the wildcard is deliberate (the browser is not on the Comb):\n%s", frontend)
	}
	rest := f.read(f.configPath("restshimd.json"))
	if !strings.Contains(rest, `"http_addr": "127.0.0.1:8081"`) {
		t.Errorf("restshimd.json http_addr is not loopback, and it is a read/write control API with no authentication:\n%s", rest)
	}
}

// Every file this tool writes is 0600 and every directory it creates is
// 0700. The private key is the reason the modes are not negotiable,
// and a config file can hold raftd's internal token and restshimd's
// manager API key, so none of them is negotiable either.
func TestWrittenFilesAre0600AndDirectoriesAre0700(t *testing.T) {
	f := newFixture(t)
	f.installed()

	for _, name := range []string{"common.json", "managerd.json", "raftd.json", "frontend.json", "restshimd.json"} {
		assertMode(t, f.configPath(name), 0o600)
	}
	assertMode(t, filepath.Join(f.opts.Paths.TLSDir, "key.pem"), 0o600)
	// The certificate is world-readable on purpose: every Comb that
	// dials this one has to read it, and it is public material.
	assertMode(t, filepath.Join(f.opts.Paths.TLSDir, "cert.pem"), 0o644)
	assertMode(t, f.opts.Paths.TLSDir, 0o700)
	assertMode(t, f.opts.Paths.ConfigDir, 0o700)
	assertMode(t, f.opts.Paths.DataDir, 0o700)
	assertMode(t, f.opts.Paths.ISODir, 0o700)
	assertMode(t, f.opts.Paths.RunDir, 0o700)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}

// The generated certificate is checked by parsing what was actually
// written, not by asking the generator what it thinks it made.
func TestGeneratedCertificateCarriesLoopbackAndBothHostnames(t *testing.T) {
	f := newFixture(t)
	f.installed()

	certPEM, err := os.ReadFile(filepath.Join(f.opts.Paths.TLSDir, "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(f.opts.Paths.TLSDir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("cert.pem is not a PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("the written certificate does not parse: %v", err)
	}

	// The local half of the evaluation gate: the certificate must
	// belong to the key beside it, and hostcert.Evaluate is what says
	// so, so this asserts the gate rather than restating it.
	info, err := hostcert.Evaluate(certPEM, hostcert.EvaluateOptions{
		Now: fixtureNow, RequireLoopbackSAN: true, RequireDNSSAN: true, KeyPEM: keyPEM,
	})
	if err != nil {
		t.Fatalf("the written pair fails its own gate: %v", err)
	}

	wantDNS := map[string]bool{"brood": true, "brood.lab3.home.arpa": true}
	for _, n := range cert.DNSNames {
		delete(wantDNS, n)
	}
	if len(wantDNS) != 0 {
		t.Errorf("certificate is missing DNS SANs %v; it has %v", wantDNS, cert.DNSNames)
	}
	if len(cert.IPAddresses) != 1 || !cert.IPAddresses[0].IsLoopback() {
		t.Errorf("certificate IP SANs = %v, want exactly 127.0.0.1", cert.IPAddresses)
	}
	// ADR-0139: a numeric LAN address in a certificate is a value that
	// works until something dials it, and then fails verification for a
	// reason that reads like a network fault.
	for _, ip := range cert.IPAddresses {
		if !ip.IsLoopback() {
			t.Errorf("certificate carries a non-loopback IP SAN %s, which is the ADR-0139 failure by construction", ip)
		}
	}
	// 3650 days, unchanged from setup-tls: shortening it would create
	// an expiry cliff with no renewal path.
	wantSpan := 3650 * 24 * time.Hour
	if got := cert.NotAfter.Sub(cert.NotBefore); got < wantSpan-time.Hour || got > wantSpan+time.Hour {
		t.Errorf("certificate lifetime = %s, want about %s", got, wantSpan)
	}
	if info.Fingerprint == "" || !strings.HasPrefix(info.Fingerprint, "SHA256:") {
		t.Errorf("fingerprint = %q, want a SHA256: fingerprint for the operator to pin", info.Fingerprint)
	}
}

// A half pair is refused rather than completed. Completing it would
// issue a certificate that does not match the key already on disk,
// which is a Comb that cannot start and an incident to explain.
func TestHalfATLSPairIsRefusedAndNamesWhichFileIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present string
		missing string
	}{
		{"certificate without a key", "cert.pem", "key.pem"},
		{"key without a certificate", "key.pem", "cert.pem"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.write(filepath.Join(f.opts.Paths.TLSDir, tc.present), "-----BEGIN CERTIFICATE-----\nnope\n-----END CERTIFICATE-----\n")
			p := f.plan()

			if !strings.Contains(report(p), "half-pair") {
				t.Errorf("the refusal does not say this is a half-pair:\n%s", report(p))
			}
			if !strings.Contains(report(p), tc.missing) {
				t.Errorf("the refusal does not name the missing file %s:\n%s", tc.missing, report(p))
			}
			if !strings.Contains(p.summary(), "NOT READY") {
				t.Errorf("a Comb with no usable certificate is not reported ready, with or without a colony member: %q", p.summary())
			}
			if err := p.Apply(); err == nil {
				t.Error("Apply returned no error despite a refusal")
			}
			// The file that was there is untouched, and the missing one
			// was not invented.
			if got := f.read(filepath.Join(f.opts.Paths.TLSDir, tc.present)); !strings.Contains(got, "nope") {
				t.Errorf("the existing file was modified: %q", got)
			}
			if f.exists(filepath.Join(f.opts.Paths.TLSDir, "cert.pem")) && tc.present != "cert.pem" {
				t.Error("a certificate was written despite the half-pair refusal")
			}
		})
	}
}

// A valid pair that is already on disk is left exactly as it is, and
// the report tells the operator what the rest of the tool is about to
// assume about it.
func TestAnExistingUsablePairIsPreservedAndReported(t *testing.T) {
	f := newFixture(t)
	certPEM, keyPEM, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName:  "brood.lab3.home.arpa",
		DNSNames:    []string{"brood", "brood.lab3.home.arpa"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		Now:         fixtureNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(f.opts.Paths.TLSDir, "cert.pem")
	keyPath := filepath.Join(f.opts.Paths.TLSDir, "key.pem")
	f.write(certPath, string(certPEM))
	f.write(keyPath, string(keyPEM))

	p := f.installed()
	if !p.CertExists {
		t.Error("CertExists = false with a usable pair on disk")
	}
	r := report(p)
	if !strings.Contains(r, "brood.lab3.home.arpa") {
		t.Errorf("the report does not name the certificate's SANs:\n%s", r)
	}
	if !strings.Contains(r, p.Cert.Fingerprint) {
		t.Errorf("the report does not carry the existing certificate's fingerprint, which is the fact an operator pins:\n%s", r)
	}
	if got := f.read(certPath); got != string(certPEM) {
		t.Error("the existing certificate was rewritten")
	}
	if got := f.read(keyPath); got != string(keyPEM) {
		t.Error("the existing private key was rewritten")
	}
}

// A pair whose certificate does not match its key is refused. This is
// the local half of the gate, and the reason the gate reads the key at
// all on this Comb.
func TestAMismatchedPairIsRefusedByName(t *testing.T) {
	f := newFixture(t)
	certPEM, _, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName: "brood", DNSNames: []string{"brood"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, Now: fixtureNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, otherKey, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName: "drone", DNSNames: []string{"drone"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, Now: fixtureNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.opts.Paths.TLSDir, "cert.pem"), string(certPEM))
	f.write(filepath.Join(f.opts.Paths.TLSDir, "key.pem"), string(otherKey))

	r := report(f.plan())
	if !strings.Contains(r, "does not match") {
		t.Errorf("the refusal does not say the key does not match the certificate:\n%s", r)
	}
}

// A file that exists and does not parse is refused with its path, and
// nothing in it is touched. A malformed file is a hand edit, a
// half-finished change or a partially written file from an interrupted
// run, and there is no way to tell those apart from the bytes.
func TestAMalformedConfigFileIsRefusedWithItsPath(t *testing.T) {
	f := newFixture(t)
	f.write(f.configPath("frontend.json"), `{"manager_addr": "127.0.0.1:17700"`)

	p := f.plan()
	r := report(p)
	if !strings.Contains(r, "REFUSE") {
		t.Fatalf("a malformed file produced no refusal:\n%s", r)
	}
	if !strings.Contains(r, "frontend.json") {
		t.Errorf("the refusal does not name the file:\n%s", r)
	}
	if err := p.Apply(); err == nil {
		t.Error("Apply returned no error despite a refusal")
	}
	if got := f.read(f.configPath("frontend.json")); !strings.Contains(got, `"manager_addr"`) {
		t.Errorf("the malformed file was overwritten: %q", got)
	}
	// A malformed frontend.json blocks frontend.json and nothing else:
	// refusing to issue a certificate because one JSON file is broken
	// would be a refusal an operator cannot act on.
	if !f.exists(filepath.Join(f.opts.Paths.TLSDir, "cert.pem")) {
		t.Error("a malformed frontend.json stopped the TLS pair from being issued, which is not the file's business")
	}
}

// A present field is never replaced, and a field the file sets to its
// own default counts as present. That second half is the one that
// matters: "present" has to mean present, not "present and different
// from what I would have written".
func TestPresentFieldsAreNeverReplaced(t *testing.T) {
	f := newFixture(t)
	f.write(f.configPath("managerd.json"), `{
  "node_id": "a-name-an-operator-chose",
  "rpc_addr": "10.62.0.2:17700",
  "http_addr_is_not_here": ""
}`)
	// The unknown field above is a second, independent refusal: the
	// frontendconfig and nodeconfig loaders reject unknown keys, so
	// this is really a test that a file the daemon would refuse is
	// refused here too. Replace it with a valid document and keep the
	// real case separate.
	f.write(f.configPath("managerd.json"), `{
  "node_id": "a-name-an-operator-chose",
  "rpc_addr": "10.62.0.2:17700",
  "zfs_base": "tank/apiary"
}`)

	p := f.installed()
	body := f.read(f.configPath("managerd.json"))
	if !strings.Contains(body, `"node_id": "a-name-an-operator-chose"`) {
		t.Errorf("an existing node_id was replaced:\n%s", body)
	}
	if !strings.Contains(body, `"rpc_addr": "10.62.0.2:17700"`) {
		t.Errorf("an existing rpc_addr was replaced:\n%s", body)
	}
	if !strings.Contains(body, `"zfs_base": "tank/apiary"`) {
		t.Errorf("an existing zfs_base was replaced:\n%s", body)
	}
	if p.NodeID != "a-name-an-operator-chose" {
		t.Errorf("NodeID = %q, want the value the file already had", p.NodeID)
	}
	// And the tool agrees that this Comb is not standalone, because
	// its own rpc_addr already names a non-loopback host.
	if !p.Colony {
		t.Error("a Comb whose own rpc_addr names a non-loopback host was treated as standalone")
	}
}

// An unknown field is refused rather than silently dropped, because
// the daemon's own loader would refuse the same file and an installer
// that wrote it back without the field would destroy the operator's
// intent.
func TestAnUnknownFieldIsRefusedRatherThanDropped(t *testing.T) {
	f := newFixture(t)
	f.write(f.configPath("managerd.json"), `{"node_id": "brood", "uplnk": "em0"}`)

	p := f.plan()
	if !strings.Contains(report(p), "REFUSE") {
		t.Errorf("a file with an unknown field was not refused:\n%s", report(p))
	}
	if err := p.Apply(); err == nil {
		t.Error("Apply returned no error despite a refusal")
	}
}

// Two files with two different node_ids is a disagreement, and the
// answer is to change nothing: writing either one orphans the other's
// state and produces a node that can never win an election.
func TestTwoNodeIDsIsADisagreementAndChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.write(f.configPath("common.json"), `{"node_id": "from-common"}`)
	f.write(f.configPath("raftd.json"), `{"node_id": "from-raftd"}`)

	p := f.plan()
	r := report(p)
	if !strings.Contains(r, "two different node_id") {
		t.Errorf("the report does not describe the disagreement:\n%s", r)
	}
	if !strings.Contains(r, "from-common") || !strings.Contains(r, "from-raftd") {
		t.Errorf("the report does not name both values, so the operator cannot see which is which:\n%s", r)
	}
	if err := p.Apply(); err == nil {
		t.Error("Apply returned no error despite a refusal")
	}
	if got := f.read(f.configPath("common.json")); !strings.Contains(got, "from-common") {
		t.Errorf("common.json was changed: %q", got)
	}
	if got := f.read(f.configPath("raftd.json")); !strings.Contains(got, "from-raftd") {
		t.Errorf("raftd.json was changed: %q", got)
	}
	if p.NodeID != "" {
		t.Errorf("NodeID = %q, want empty when the host's identity is in dispute", p.NodeID)
	}
}

// A node_id present in exactly one file is not a disagreement, and the
// value is propagated to the others rather than invented.
func TestASingleExistingNodeIDIsPropagated(t *testing.T) {
	f := newFixture(t)
	f.write(f.configPath("raftd.json"), `{"node_id": "the-real-one"}`)
	f.installed()

	if body := f.read(f.configPath("common.json")); !strings.Contains(body, `"node_id": "the-real-one"`) {
		t.Errorf("common.json did not take the one node_id that exists:\n%s", body)
	}
	if body := f.read(f.configPath("managerd.json")); !strings.Contains(body, `"node_id": "the-real-one"`) {
		t.Errorf("managerd.json did not take the one node_id that exists:\n%s", body)
	}
}

// A Colony member gets a name from its own certificate and never an
// invented one and never a numeric address.
func TestColonyMemberWritesANameTakenFromTheCertificate(t *testing.T) {
	f := newFixture(t)
	f.opts.ColonyMember = "drone.lab3.home.arpa:17700"
	p := f.installed()

	if !p.Colony {
		t.Error("Colony = false with --colony-member given")
	}
	// The FQDN is preferred over the short name, because the FQDN is
	// the one that resolves from another host without a local search
	// domain.
	if p.RPCAddr != "brood.lab3.home.arpa:17700" {
		t.Errorf("RPCAddr = %q, want the FQDN from the certificate", p.RPCAddr)
	}
	if !strings.Contains(f.read(f.configPath("managerd.json")), `"rpc_addr": "brood.lab3.home.arpa:17700"`) {
		t.Errorf("managerd.json does not carry the resolvable name:\n%s", f.read(f.configPath("managerd.json")))
	}
}

// A certificate with no DNS name cannot be dialled by name, so a
// Colony install refuses rather than writing an address the
// certificate does not cover. This is ADR-0139's incident, and the
// refusal prints the SANs so the operator can see what was found.
func TestACertificateWithNoUsableNameIsRefusedAndPrintsItsSANs(t *testing.T) {
	f := newFixture(t)
	certPEM, keyPEM, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName:  "brood",
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		Now:         fixtureNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.opts.Paths.TLSDir, "cert.pem"), string(certPEM))
	f.write(filepath.Join(f.opts.Paths.TLSDir, "key.pem"), string(keyPEM))
	f.opts.ColonyMember = "drone.lab3.home.arpa:17700"

	p := f.plan()
	r := report(p)
	if !strings.Contains(r, "no usable DNS name") {
		t.Errorf("the refusal does not say why:\n%s", r)
	}
	if !strings.Contains(r, "IP:127.0.0.1") {
		t.Errorf("the refusal does not print the SANs it found, which is the fact the operator needs:\n%s", r)
	}
	if p.RPCAddr != "" {
		t.Errorf("RPCAddr = %q, want empty when the install refused to write one", p.RPCAddr)
	}
	if err := p.Apply(); err == nil {
		t.Error("Apply returned no error despite a refusal")
	}
	if body := f.read(f.configPath("managerd.json")); strings.Contains(body, "rpc_addr") {
		t.Errorf("an rpc_addr was written despite the refusal:\n%s", body)
	}
}

// The idempotence property, stated as bytes: a second run over this
// install's own output changes nothing at all. Not "the same values" -
// nothing, so the test can catch a rewrite of identical bytes.
func TestASecondRunOverItsOwnOutputChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.installed()

	before := snapshot(t, f.root)
	if err := f.apply(); err != nil {
		t.Fatalf("a second run was refused: %v", err)
	}
	after := snapshot(t, f.root)
	if len(before) != len(after) {
		t.Fatalf("a second run changed the file set:\nbefore %v\nafter  %v", before, after)
	}
	for path, body := range before {
		if after[path] != body {
			t.Errorf("%s changed on a second run", path)
		}
	}
	if p := f.plan(); len(p.writes) != 0 {
		var names []string
		for _, a := range p.Actions() {
			if a.Kind == ActionCreate || a.Kind == ActionFill {
				names = append(names, a.Field+"="+a.Value)
			}
		}
		t.Errorf("a second run planned %d writes: %v", len(p.writes), names)
	}
}

// snapshot reads every file under root, keyed by its path relative to
// root, so a comparison is about content rather than about mtimes.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = string(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The values this program cannot see are reported as needing a human
// rather than guessed. A plausible default written into a valid config
// file is the failure this whole package exists to prevent, because
// nothing downstream can tell a guess from a decision.
func TestUnknowableHostFactsAreReportedAsNeedsNotGuessed(t *testing.T) {
	f := newFixture(t)
	p := f.installed()

	r := report(p)
	for _, want := range []string{"zfs_base", "uplink", "bhyve_bridge", "bhyve_bootrom", "nat_uplink"} {
		if !strings.Contains(r, want) {
			t.Errorf("the report does not mention %s at all:\n%s", want, r)
		}
	}
	body := f.read(f.configPath("managerd.json"))
	for _, field := range []string{`"zfs_base"`, `"uplink"`, `"bhyve_bridge"`, `"bhyve_bootrom"`, `"nat_uplink"`} {
		if strings.Contains(body, field) {
			t.Errorf("%s was written without being supplied:\n%s", field, body)
		}
	}
	// And supplying one is honoured rather than refused.
	if len(p.Needs()) == 0 {
		t.Error("Needs() is empty on a host that genuinely needs values supplied")
	}
}

func TestSuppliedHostFactsAreWritten(t *testing.T) {
	f := newFixture(t)
	f.opts.ZFSBase = "zroot/apiary"
	f.opts.Uplink = "em0"
	f.opts.BhyveBridge = "bridge0"
	f.installed()

	body := f.read(f.configPath("managerd.json"))
	for _, want := range []string{`"zfs_base": "zroot/apiary"`, `"uplink": "em0"`, `"bhyve_bridge": "bridge0"`} {
		if !strings.Contains(body, want) {
			t.Errorf("a supplied host fact was not written (%s):\n%s", want, body)
		}
	}
}

// peer_tls_hostname_map is not written, and the report says why. ADR-0115
// derives it from raft membership at runtime; a value written at install
// time is a second list that goes stale the moment membership changes.
func TestPeerTLSHostnameMapIsNeverWrittenAndTheReportSaysWhy(t *testing.T) {
	f := newFixture(t)
	f.installed()

	if body := f.read(f.configPath("managerd.json")); strings.Contains(body, "peer_tls_hostname_map") {
		t.Errorf("a derived map was written at install time:\n%s", body)
	}
	if !strings.Contains(report(f.plan()), "ADR-0115") {
		t.Error("the report does not say why peer_tls_hostname_map was left alone")
	}
}

// No secret reaches the report. The one value the plan holds for the
// private key is a literal, and the key's own bytes must not appear
// anywhere in what an operator might paste into a ticket.
func TestTheReportNeverPrintsKeyMaterial(t *testing.T) {
	f := newFixture(t)
	f.installed()

	keyPEM, err := os.ReadFile(filepath.Join(f.opts.Paths.TLSDir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		t.Fatal("key.pem is not a PEM block")
	}
	// The body, not the armour: the header is a constant that appears
	// in the report by design.
	if strings.Contains(report(f.plan()), string(block.Bytes)) {
		t.Error("the report contains the private key's DER body")
	}
	if !strings.Contains(report(f.plan()), "(not shown)") {
		t.Error("the report does not say the key is withheld, so its absence looks like an oversight")
	}
}

// An existing directory with wider permissions is left alone. Tightening
// it would be the same class of unrequested change this package exists to
// avoid, even when the wider mode looks like a mistake.
func TestAnExistingDirectoryIsNotRechmodded(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(f.opts.Paths.RunDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.installed()
	assertMode(t, f.opts.Paths.RunDir, 0o755)
}

// The loaders the installer uses are the daemons' own, so a config the
// installer wrote is one the daemon will accept. This asserts it through
// the daemon's Loader rather than through a second parser.
func TestEverythingWrittenPassesItsOwnDaemonsLoader(t *testing.T) {
	f := newFixture(t)
	f.installed()

	if _, err := (&nodeconfig.Manager{Path: f.configPath("managerd.json")}).Load(); err != nil {
		t.Errorf("managerd.json does not load through internal/nodeconfig: %v", err)
	}
	if _, err := (&raftdconfig.Manager{Path: f.configPath("raftd.json")}).Load(); err != nil {
		t.Errorf("raftd.json does not load through internal/raftdconfig: %v", err)
	}
}

// An explicitly supplied rpc_addr is checked rather than trusted, and
// the three refusals are the three ways ADR-0139 has already been got
// wrong. A wildcard is a legal bind address and an illegal destination;
// a numeric LAN address is not covered by any certificate this system
// issues; a name the certificate does not carry cannot be verified by
// any peer.
func TestAnExplicitRPCAddrIsCheckedAgainstTheCertificate(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		want    string
		refused bool
	}{
		{"a numeric LAN address", "10.62.0.2:17700", "no IP SAN for a LAN address", true},
		{"the wildcard", "0.0.0.0:17700", "no IP SAN for a LAN address", true},
		{"a name the certificate does not carry", "other.example.com:17700", "does not cover", true},
		{"something that is not an address", "17700", "cannot be checked", true},
		{"the host's own FQDN", "brood.lab3.home.arpa:17700", "", false},
		{"the host's own short name", "brood:17700", "", false},
		{"loopback, which the certificate does carry", "127.0.0.1:17700", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.opts.RPCAddr = tc.addr
			p := f.plan()
			if tc.refused {
				if p.RPCAddr != "" {
					t.Errorf("RPCAddr = %q, want empty when the override was refused", p.RPCAddr)
				}
				if !strings.Contains(report(p), tc.want) {
					t.Errorf("the refusal does not say %q:\n%s", tc.want, report(p))
				}
				return
			}
			if p.RPCAddr == "" {
				t.Fatalf("a legal override was refused:\n%s", report(p))
			}
			if err := p.Apply(); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if body := f.read(f.configPath("managerd.json")); !strings.Contains(body, `"rpc_addr": "`+p.RPCAddr+`"`) {
				t.Errorf("managerd.json does not carry the checked override %q:\n%s", p.RPCAddr, body)
			}
		})
	}
}

// An override of a PRESENT field is still not written, because an
// override is a default, not an instruction to overwrite.
func TestAnExplicitRPCAddrDoesNotOverwriteAFieldThatIsAlreadySet(t *testing.T) {
	f := newFixture(t)
	f.write(f.configPath("managerd.json"), `{"rpc_addr": "kept.lab3.home.arpa:17700"}`)
	f.opts.RPCAddr = "brood.lab3.home.arpa:17700"
	f.installed()
	if body := f.read(f.configPath("managerd.json")); !strings.Contains(body, "kept.lab3.home.arpa:17700") {
		t.Errorf("a present rpc_addr was replaced:\n%s", body)
	}
}

// The web listeners are overridable, so `make setup-quick
// NODE_HTTP_ADDR=...` keeps meaning what it meant, and the defaults
// are still the Makefile's own.
func TestTheWebListenersAreOverridable(t *testing.T) {
	f := newFixture(t)
	f.opts.FrontendHTTPAddr = "10.62.0.2:9090"
	f.opts.RestshimdHTTPAddr = "127.0.0.1:9999"
	f.installed()

	if body := f.read(f.configPath("frontend.json")); !strings.Contains(body, `"http_addr": "10.62.0.2:9090"`) {
		t.Errorf("the frontend override was ignored:\n%s", body)
	}
	if body := f.read(f.configPath("restshimd.json")); !strings.Contains(body, `"http_addr": "127.0.0.1:9999"`) {
		t.Errorf("the restshimd override was ignored:\n%s", body)
	}
}

func TestTheBootROMIsWrittenOnlyWhenSupplied(t *testing.T) {
	f := newFixture(t)
	f.installed()
	if body := f.read(f.configPath("managerd.json")); strings.Contains(body, "bhyve_bootrom") {
		t.Errorf("a bootrom path was written without being supplied:\n%s", body)
	}

	g := newFixture(t)
	g.opts.BhyveBootROM = "/usr/local/share/uefi-firmware/BHYVE_UEFI.fd"
	g.installed()
	if body := g.read(g.configPath("managerd.json")); !strings.Contains(body, "/usr/local/share/uefi-firmware/BHYVE_UEFI.fd") {
		t.Errorf("a supplied bootrom path was not written:\n%s", body)
	}
}

// The last line of the report is the one an operator acts on, and it
// must never describe a refused Comb as configured. The other tests
// here check that refusals happen; this one checks that the summary
// cannot paper over one, which is the failure mode a mutation of the
// summary itself would produce and nothing else would catch.
func TestASummaryNeverClaimsAConfiguredCombAfterARefusal(t *testing.T) {
	cases := []struct {
		name  string
		seed  func(f *fixture)
		colon string
	}{
		{
			name: "a file that does not parse",
			seed: func(f *fixture) { f.write(f.configPath("frontend.json"), `{"manager_addr":`) },
		},
		{
			name: "a file with a field this binary does not know",
			seed: func(f *fixture) { f.write(f.configPath("managerd.json"), `{"uplnk": "em0"}`) },
		},
		{
			name: "two different node_ids",
			seed: func(f *fixture) {
				f.write(f.configPath("common.json"), `{"node_id": "one"}`)
				f.write(f.configPath("raftd.json"), `{"node_id": "two"}`)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.seed(f)
			p := f.plan()
			if len(p.Refusals()) == 0 {
				t.Fatalf("the fixture produced no refusal at all:\n%s", report(p))
			}
			if got := p.summary(); strings.HasPrefix(got, "configured") {
				t.Errorf("summary() = %q, which reads as a configured Comb after a refusal", got)
			}
		})
	}
}

// A Comb with a usable certificate and nothing refused but values
// still missing is the one case that may say "configured", and it has
// to say what is still outstanding rather than stop at the word.
func TestASummaryNamesWhatIsStillNeeded(t *testing.T) {
	f := newFixture(t)
	p := f.installed()
	got := p.summary()
	if !strings.HasPrefix(got, "configured") {
		t.Fatalf("summary() = %q, want a configured Comb with outstanding values", got)
	}
	if !strings.Contains(got, "needing a human") {
		t.Errorf("summary() = %q, want it to say that values are still needed", got)
	}
}

// A listener bound to this host's own NAME does not serve 127.0.0.1, so
// on a Colony member frontend and restshimd must dial that same name.
// Dialing loopback there produces a web UI that cannot reach the API it
// exists to show, while managerd's own listener looks perfectly healthy
// - which is exactly what the listener being up hides.
func TestLocalDialsFollowTheAddressManagerdIsActuallyGiven(t *testing.T) {
	standalone := newFixture(t)
	standalone.installed()
	if got := standalone.read(standalone.configPath("frontend.json")); !strings.Contains(got, `"manager_addr": "127.0.0.1:17700"`) {
		t.Errorf("a standalone Comb should dial loopback:\n%s", got)
	}
	if got := standalone.read(standalone.configPath("restshimd.json")); !strings.Contains(got, `"manager_addr": "127.0.0.1:17700"`) {
		t.Errorf("a standalone Comb should dial loopback:\n%s", got)
	}

	colony := newFixture(t)
	colony.opts.ColonyMember = "drone.lab3.home.arpa:17700"
	colony.installed()
	for _, name := range []string{"frontend.json", "restshimd.json"} {
		body := colony.read(colony.configPath(name))
		if !strings.Contains(body, `"manager_addr": "brood.lab3.home.arpa:17700"`) {
			t.Errorf("%s does not dial the address managerd is actually bound to:\n%s", name, body)
		}
		if strings.Contains(body, `"manager_addr": "127.0.0.1`) {
			t.Errorf("%s dials loopback, which a listener bound to this host's own name does not serve:\n%s", name, body)
		}
	}
}

// ADR-0147 Part 3's authorization store is the one file in this
// package that is an authority rather than a setting, so the rule that
// governs it is the strictest form of the rule the rest of the package
// runs on: a file that is already there is left exactly as it is, mode
// and bytes both, and a file that is there but wrong is refused by name
// rather than corrected. The cases below are the four branches
// planJoinAuthorizationStore actually has, plus a fifth - a stat that
// fails for any reason other than "not there" - because "leave it
// alone" has to hold for a file this program cannot even look at.
//
// The mode is asserted on the file after Apply rather than on the plan
// line, because a create line carries no mode: the mode lives in the
// write the plan registered, so the only honest way to check it is to
// apply and look. Ownership is not asserted at all, and cannot be: the
// fixture is owned by whoever runs the test, and "root-owned" on a real
// Comb is a consequence of --apply refusing to run without an effective
// uid of 0, which is ErrNotRoot's job rather than this function's.
func TestPlanJoinAuthorizationStore(t *testing.T) {
	populated := `{
  "authorizations": [
    {
      "id": "auth-0123456789abcdef",
      "node_id": "drone.lab3.home.arpa",
      "fingerprint": "SHA256:AA:BB",
      "expires_at_unix": 1790000000
    },
    {
      "id": "auth-fedcba9876543210",
      "node_id": "wasp.lab3.home.arpa",
      "fingerprint": "SHA256:CC:DD",
      "expires_at_unix": 1790000001
    }
  ]
}
`

	// storeLines returns every line the plan recorded about the store,
	// which is the whole of what this function contributes to the
	// report. Filtering to the path keeps a failure pointing at the
	// store rather than at the twenty other lines around it.
	storeLines := func(p *Plan, path string) []Action {
		var out []Action
		for _, a := range p.Actions() {
			if a.Path == path {
				out = append(out, a)
			}
		}
		return out
	}

	for _, tc := range []struct {
		name string
		// seed puts the store on disk. It returns the mode the file
		// must still have after Apply, and whether to check a mode at
		// all - the unstatable case has none worth checking.
		seed func(t *testing.T, f *fixture, path string) (os.FileMode, bool)
		// want is the one line about the store the plan must record.
		wantKind   ActionKind
		wantField  string
		wantValue  string
		wantReason []string
		// wantWritten is whether Apply must have created the file.
		wantWritten bool
		// wantApplyErr is whether Apply must have failed. A refusal
		// makes the whole install exit non-zero, which is asserted
		// rather than left to a summary test: for this one file the
		// exit status is most of what an operator learns.
		wantApplyErr bool
	}{
		{
			// The happy path: a fresh Comb has the store before it
			// needs one, empty, so "no Comb is authorized yet" is a
			// fact about a file that exists rather than about a file
			// that is missing.
			name:        "absent",
			wantKind:    ActionCreate,
			wantField:   "authorization entries",
			wantValue:   "0",
			wantReason:  []string{"join-authorize", "created empty"},
			wantWritten: true,
		},
		{
			// The second run over this install's own output. The file
			// is already there and correct, so it is kept and nothing
			// is registered to write it.
			name: "already present and correct",
			seed: func(t *testing.T, f *fixture, path string) (os.FileMode, bool) {
				f.write(path, "{\n  \"authorizations\": []\n}\n")
				return 0o600, true
			},
			wantKind:   ActionKeep,
			wantField:  "authorization entries",
			wantValue:  "0",
			wantReason: []string{"already exists", "never rewrite it"},
		},
		{
			// The same file with an operator's entries in it. The count
			// is reported, because an operator reading the report has
			// to be able to see that the file was read at all.
			name: "already present with entries",
			seed: func(t *testing.T, f *fixture, path string) (os.FileMode, bool) {
				f.write(path, populated)
				return 0o600, true
			},
			wantKind:   ActionKeep,
			wantField:  "authorization entries",
			wantValue:  "2",
			wantReason: []string{"already exists", "never rewrite it"},
		},
		{
			// Present, but not readable by root alone. The installer
			// refuses and says so by name rather than chmodding it,
			// because the whole authority claim of this file is that
			// only root can write it, and silently tightening a file an
			// operator may be reading is the class of unrequested
			// change this package exists to avoid.
			name: "present with the wrong mode",
			seed: func(t *testing.T, f *fixture, path string) (os.FileMode, bool) {
				f.write(path, "{\n  \"authorizations\": []\n}\n")
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
				return 0o644, true
			},
			wantKind:     ActionRefuse,
			wantField:    "mode",
			wantValue:    "0644",
			wantReason:   []string{"only root can write it", "not chmodded here"},
			wantApplyErr: true,
		},
		{
			// Present, readable only by its owner, and unparseable. It
			// is reported rather than fixed, because the contents are
			// the operator's deliberate edits and becoming a second
			// authority over the same bytes is what this package
			// refuses to do. The file is still left exactly as it is,
			// which is the whole point.
			name: "present and malformed",
			seed: func(t *testing.T, f *fixture, path string) (os.FileMode, bool) {
				f.write(path, `{"authorizations": [`)
				return 0o600, true
			},
			wantKind:     ActionRefuse,
			wantField:    "contents",
			wantReason:   []string{"left exactly as it is", "join-authorize"},
			wantApplyErr: true,
		},
		{
			// A path this program cannot stat at all: a symlink pointing
			// at itself is ELOOP, not ENOENT, so it must not be
			// mistaken for an absent store and created over.
			name: "not statable",
			seed: func(t *testing.T, f *fixture, path string) (os.FileMode, bool) {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Base(path), path); err != nil {
					t.Fatal(err)
				}
				return 0, false
			},
			wantKind:     ActionRefuse,
			wantField:    "authorization entries",
			wantReason:   []string{"join-authorizations.json"},
			wantApplyErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			path := f.opts.Paths.joinAuthorizations()

			var seededMode os.FileMode
			var checkMode bool
			if tc.seed != nil {
				seededMode, checkMode = tc.seed(t, f, path)
			}
			// The inode before the plan runs, so "not rewritten" can be
			// asserted as a fact about the file rather than as a claim
			// about a list of internal closures. writeFileAtomic always
			// renames into place, so a rewrite necessarily replaces the
			// inode; an unchanged one is a rewrite that did not happen.
			// Lstat rather than Stat, because the unstatable case is a
			// symlink and os.Stat on it is the very failure under test.
			var before os.FileInfo
			if tc.seed != nil {
				var err error
				if before, err = os.Lstat(path); err != nil {
					t.Fatalf("lstat %s: %v", path, err)
				}
			}

			p := f.plan()
			got := storeLines(p, path)
			if len(got) != 1 {
				t.Fatalf("the plan recorded %d lines about %s, want exactly 1: %v", len(got), path, got)
			}
			if got[0].Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q (reason %q)", got[0].Kind, tc.wantKind, got[0].Reason)
			}
			if got[0].Field != tc.wantField {
				t.Errorf("field = %q, want %q", got[0].Field, tc.wantField)
			}
			if got[0].Value != tc.wantValue {
				t.Errorf("value = %q, want %q", got[0].Value, tc.wantValue)
			}
			for _, want := range tc.wantReason {
				if !strings.Contains(got[0].Reason, want) {
					t.Errorf("reason = %q, want it to mention %q", got[0].Reason, want)
				}
			}
			if tc.wantKind == ActionRefuse {
				if len(p.Refusals()) == 0 {
					t.Errorf("Refusals() is empty; a store that is wrong is a refusal the operator has to see")
				}
			} else if len(p.Refusals()) != 0 {
				t.Errorf("Refusals() = %v, want none from a store that was created or kept", p.Refusals())
			}
			for _, n := range p.Needs() {
				if n.Path == path {
					t.Errorf("the store is recorded as a need (%q); it is created empty, so there is nothing for a human to supply", n.Reason)
				}
			}

			// Create means Apply must produce the file. Every other case
			// means Apply must not touch it, and Apply is expected to
			// fail on the refusals.
			err := p.Apply()
			if (err != nil) != tc.wantApplyErr {
				t.Errorf("Apply returned %v, wantApplyErr = %v", err, tc.wantApplyErr)
			}
			if tc.wantWritten {
				if !f.exists(path) {
					t.Fatalf("Apply did not create %s", path)
				}
				// The intended mode, checked on the file the plan's
				// write actually produced rather than on the plan line,
				// because the line does not carry it.
				assertMode(t, path, 0o600)
				// The bytes the installer writes have to be a store
				// joinauth.Load accepts, or the very first
				// `apiaryctl join-authorize` would be refused against a
				// file this tool created.
				store, loadErr := joinauth.Load(path)
				if loadErr != nil {
					t.Fatalf("the store the installer wrote does not load through internal/joinauth: %v", loadErr)
				}
				if len(store.Authorizations) != 0 {
					t.Errorf("the store the installer wrote holds %d entries, want an empty one: no Comb is authorized to join until an operator says so", len(store.Authorizations))
				}
				return
			}
			after, statErr := os.Lstat(path)
			if statErr != nil {
				t.Fatalf("lstat %s after Apply: %v", path, statErr)
			}
			if !os.SameFile(before, after) {
				t.Errorf("Apply replaced %s; a store that was already on disk must not be rewritten, not even to identical bytes", path)
			}
			if checkMode {
				if got := after.Mode().Perm(); got != seededMode {
					t.Errorf("%s mode = %04o after Apply, want the %04o it was seeded with", path, got, seededMode)
				}
			}
		})
	}
}
