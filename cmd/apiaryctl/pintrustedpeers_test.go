// Tests for apiaryctl pin-trusted-peers.
//
// What is under test here is ORDER, because that is what the command is
// for. Every refusal in pintrustedpeers.go is a statement about what
// must NOT have happened when it fires - no pin sent, no member skipped
// silently, no run reported as finished when it stopped at member
// three - and none of that can be checked by reading the returned
// value. It has to be checked against a record of every RPC the run
// made, which is what countingBackend is.
//
// The second half stands up a real gRPC server, because the counting
// backend proves the order and proves nothing about the wire. The wire
// carries the security properties: a certificate this command could
// type a fingerprint into, a voter claim it is not allowed to make, and
// an Admin credential that has to arrive without being on a command
// line. None of those can be observed by a stub that records structs.

package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostcert"
)

// ---------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------

// combCert issues a Comb serving certificate the way internal/hostcert
// issues one - short common name, qualified DNS SAN alongside it,
// IP:127.0.0.1 and nothing else - and writes it where the command can
// be pointed at it.
//
// It is hostcert.Generate rather than a checked-in PEM because the
// refusals under test are about what a certificate SAYS, and a fixture
// whose bytes never change cannot prove that a change in what they say
// was noticed.
func combCert(t *testing.T, dir, cn string, dnsNames ...string) string {
	t.Helper()
	certPEM, _, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName:  cn,
		DNSNames:    dnsNames,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	})
	if err != nil {
		t.Fatalf("issuing a certificate for %s: %v", cn, err)
	}
	path := filepath.Join(dir, cn+".pem")
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// bundle concatenates certificates into the one file --bundle names.
func bundle(t *testing.T, paths ...string) string {
	t.Helper()
	var buf bytes.Buffer
	for i, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		buf.Write(data)
		if i < len(paths)-1 {
			buf.WriteString("\n")
		}
	}
	path := filepath.Join(t.TempDir(), "peers.pem")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// countingBackend records every RPC and answers with whatever the test
// told it to.
type countingBackend struct {
	store     map[string]*internalpb.TrustedPeer
	listCalls int
	listErr   error
	pins      []*rpcpb.TrustedPeer
	// refusals maps a node_id to the refusal managerd would return for
	// it: a nil error and a non-empty string, which is the shape the
	// real RPC uses for an application-level refusal.
	refusals map[string]string
	// pinErr is a transport-level failure, the other kind entirely.
	pinErr error
	// fingerprint is what the "server" reports back. It is deliberately
	// NOT what the certificate hashes to: a run that reported its own
	// locally-computed value instead of the store's would pass a test
	// that only checked for a fingerprint.
	fingerprint string
}

func (b *countingBackend) ListTrustedPeers(context.Context) (map[string]*internalpb.TrustedPeer, error) {
	b.listCalls++
	if b.listErr != nil {
		return nil, b.listErr
	}
	out := make(map[string]*internalpb.TrustedPeer, len(b.store))
	for k, v := range b.store {
		out[k] = v
	}
	return out, nil
}

func (b *countingBackend) Pin(_ context.Context, peer *rpcpb.TrustedPeer) (*rpcpb.TrustedPeer, string, error) {
	b.pins = append(b.pins, proto.Clone(peer).(*rpcpb.TrustedPeer))
	if b.pinErr != nil {
		return nil, "", b.pinErr
	}
	if r, ok := b.refusals[peer.GetNodeId()]; ok {
		return nil, r, nil
	}
	return &rpcpb.TrustedPeer{
		NodeId:       peer.GetNodeId(),
		CombName:     peer.GetCombName(),
		CertPem:      peer.GetCertPem(),
		Fingerprint:  b.fingerprint,
		NotAfterUnix: time.Now().Add(90 * 24 * time.Hour).Unix(),
		PinnedBy:     "brood.lab3.home.arpa",
	}, "", nil
}

func (b *countingBackend) pinnedNodeIDs() []string {
	out := make([]string, 0, len(b.pins))
	for _, p := range b.pins {
		out = append(out, p.GetNodeId())
	}
	return out
}

func newCountingBackend() *countingBackend {
	return &countingBackend{
		store:       map[string]*internalpb.TrustedPeer{},
		fingerprint: "SHA256:11:22:33",
	}
}

// run executes the command and returns its status and both streams.
func runPin(t *testing.T, backend pinBackend, req pinRequest) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := pinTrustedPeers(context.Background(), backend, req, &out, &errOut)
	return code, out.String(), errOut.String()
}

// ---------------------------------------------------------------------
// Phase 1: nothing may go on the wire before every input is checked
// ---------------------------------------------------------------------

// A bundle whose fourth block is a private key pins nothing at all -
// not three members, not zero RPCs of the store. This is the property
// the whole command is built around, and "nothing happened" has to be
// asserted on the RPC count rather than on the error text, because a
// message saying "nothing was pinned" is something any implementation
// can print.
func TestPinTrustedPeers_BundleWithAPrivateKeyPinsNothing(t *testing.T) {
	dir := t.TempDir()
	good := []string{
		combCert(t, dir, "brood", "brood", "brood.lab3.home.arpa"),
		combCert(t, dir, "drone", "drone", "drone.lab3.home.arpa"),
		combCert(t, dir, "buzz", "buzz", "buzz.lab3.home.arpa"),
	}
	certPEM, keyPEM, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName:  "sting",
		DNSNames:    []string{"sting", "sting.lab3.home.arpa"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	})
	if err != nil {
		t.Fatalf("issuing a certificate for sting: %v", err)
	}
	path := filepath.Join(dir, "peers.pem")
	var buf bytes.Buffer
	for _, p := range good {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		buf.Write(data)
	}
	buf.Write(keyPEM) // a bundle that picked up the private key
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	_ = certPEM

	backend := newCountingBackend()
	code, _, errOut := runPin(t, backend, pinRequest{Bundle: path})

	if code == 0 {
		t.Errorf("exit status 0 on a bundle holding a private key; want a refusal")
	}
	if backend.listCalls != 0 {
		t.Errorf("read the trust store %d times; the bundle is refused before any RPC at all, so it must be 0", backend.listCalls)
	}
	if len(backend.pins) != 0 {
		t.Errorf("sent %d pins (%v); a bundle with one bad block must send none", len(backend.pins), backend.pinnedNodeIDs())
	}
	if !strings.Contains(errOut, "PRIVATE KEY") {
		t.Errorf("refusal does not name the block it found: %s", errOut)
	}
}

// Two certificates claiming the same node_id is two pins for one key,
// and the second is refused by the FSM only AFTER the first has been
// written. It has to be caught here instead.
func TestPinTrustedPeers_TwoCertificatesForOneNodeIDPinNothing(t *testing.T) {
	dir := t.TempDir()
	first := combCert(t, dir, "brood", "brood", "brood.lab3.home.arpa")
	second := combCert(t, dir, "brood", "brood", "brood.lab3.home.arpa")

	backend := newCountingBackend()
	code, _, errOut := runPin(t, backend, pinRequest{Bundle: bundle(t, first, second)})

	if code == 0 {
		t.Errorf("exit status 0 on a bundle naming one member twice; want a refusal")
	}
	if backend.listCalls != 0 || len(backend.pins) != 0 {
		t.Errorf("made %d store reads and %d pins; want 0 and 0", backend.listCalls, len(backend.pins))
	}
	if !strings.Contains(errOut, "twice") {
		t.Errorf("refusal does not say the name was repeated: %s", errOut)
	}
}

// A short common name with two qualified SANs is a guess, and a guess
// written into the trust store is a lie five members later.
func TestPinTrustedPeers_AmbiguousShortCommonNamePinsNothing(t *testing.T) {
	dir := t.TempDir()
	ambiguous := combCert(t, dir, "brood", "brood", "brood.lab3.home.arpa", "brood.other.example")
	other := combCert(t, dir, "drone", "drone", "drone.lab3.home.arpa")

	backend := newCountingBackend()
	code, _, errOut := runPin(t, backend, pinRequest{Bundle: bundle(t, other, ambiguous)})

	if code == 0 {
		t.Errorf("exit status 0 on an ambiguous certificate; want a refusal")
	}
	if backend.listCalls != 0 || len(backend.pins) != 0 {
		t.Errorf("made %d store reads and %d pins; want 0 and 0", backend.listCalls, len(backend.pins))
	}
	// The refusal has to say WHICH name it would have had to pick, or
	// the operator cannot tell a mis-collected bundle from a bad one.
	if !strings.Contains(errOut, "brood.other.example") || !strings.Contains(errOut, "brood.lab3.home.arpa") {
		t.Errorf("refusal does not list the candidate names: %s", errOut)
	}
}

// A certificate that cannot be evaluated at all - here an expired one -
// is refused in the preflight for the same reason, and the refusal has
// to name its position so a five-certificate bundle can be found.
func TestPinTrustedPeers_ExpiredCertificatePinsNothingAndIsLocated(t *testing.T) {
	dir := t.TempDir()
	good := combCert(t, dir, "drone", "drone", "drone.lab3.home.arpa")
	expired, _, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName:  "sting",
		DNSNames:    []string{"sting", "sting.lab3.home.arpa"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		Validity:    time.Hour,
		Now:         time.Now().Add(-48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("issuing an expired certificate: %v", err)
	}
	expiredPath := filepath.Join(dir, "sting.pem")
	if err := os.WriteFile(expiredPath, expired, 0o600); err != nil {
		t.Fatalf("writing %s: %v", expiredPath, err)
	}

	backend := newCountingBackend()
	code, _, errOut := runPin(t, backend, pinRequest{Bundle: bundle(t, good, expiredPath)})

	if code == 0 {
		t.Errorf("exit status 0 on a bundle holding an expired certificate; want a refusal")
	}
	if backend.listCalls != 0 || len(backend.pins) != 0 {
		t.Errorf("made %d store reads and %d pins; want 0 and 0", backend.listCalls, len(backend.pins))
	}
	if !strings.Contains(errOut, "certificate 2 of 2") {
		t.Errorf("refusal does not locate the bad certificate in the bundle: %s", errOut)
	}
}

// A store that cannot be read is a refusal before anything is pinned,
// for the same reason: the conflict check lives in that read.
func TestPinTrustedPeers_UnreadableStorePinsNothing(t *testing.T) {
	dir := t.TempDir()
	path := combCert(t, dir, "brood", "brood", "brood.lab3.home.arpa")

	backend := newCountingBackend()
	backend.listErr = fmt.Errorf("raftd is not listening")
	code, _, errOut := runPin(t, backend, pinRequest{Bundle: bundle(t, path)})

	if code == 0 {
		t.Errorf("exit status 0 when the store could not be read; want a refusal")
	}
	if len(backend.pins) != 0 {
		t.Errorf("sent %d pins without having read the store", len(backend.pins))
	}
	if !strings.Contains(errOut, "raftd is not listening") {
		t.Errorf("refusal does not carry the underlying cause: %s", errOut)
	}
}

// A pin cannot be replaced. A member already holding a different
// certificate is therefore refused BEFORE the first pin, because the
// FSM's own refusal would arrive after the others were written.
func TestPinTrustedPeers_MemberPinnedToADifferentCertificatePinsNothing(t *testing.T) {
	dir := t.TempDir()
	brood := combCert(t, dir, "brood", "brood", "brood.lab3.home.arpa")
	drone := combCert(t, dir, "drone", "drone", "drone.lab3.home.arpa")

	backend := newCountingBackend()
	backend.store["brood.lab3.home.arpa"] = &internalpb.TrustedPeer{
		NodeId:      "brood.lab3.home.arpa",
		Fingerprint: "SHA256:99:99:99",
	}

	code, _, errOut := runPin(t, backend, pinRequest{Bundle: bundle(t, drone, brood)})

	if code == 0 {
		t.Errorf("exit status 0 on a member already pinned to a different certificate; want a refusal")
	}
	if len(backend.pins) != 0 {
		t.Errorf("sent %d pins (%v); the conflict was knowable from the store read", len(backend.pins), backend.pinnedNodeIDs())
	}
	if !strings.Contains(errOut, "SHA256:99:99:99") {
		t.Errorf("refusal does not name the certificate already held: %s", errOut)
	}
}

// ---------------------------------------------------------------------
// Phase 2: the order, and what the run reports
// ---------------------------------------------------------------------

// The ordinary case: a short common name becomes the qualified name the
// Colony uses as its raft identity, and members are pinned in the order
// the operator listed them.
func TestPinTrustedPeers_DerivesTheQualifiedNodeIDAndPinsInOrder(t *testing.T) {
	dir := t.TempDir()
	brood := combCert(t, dir, "brood", "brood", "brood.lab3.home.arpa")
	drone := combCert(t, dir, "drone", "drone", "drone.lab3.home.arpa")

	backend := newCountingBackend()
	code, out, errOut := runPin(t, backend, pinRequest{Bundle: bundle(t, brood, drone)})

	if code != 0 {
		t.Fatalf("exit status %d on a clean bundle; want 0\n%s", code, errOut)
	}
	want := []string{"brood.lab3.home.arpa", "drone.lab3.home.arpa"}
	if got := backend.pinnedNodeIDs(); !equalStrings(got, want) {
		t.Errorf("pinned %v; want %v", got, want)
	}
	// The comb name is the certificate's own short name: a label, and
	// not the identity, which is why it is allowed to differ.
	if got := backend.pins[0].GetCombName(); got != "brood" {
		t.Errorf("comb name %q; want the certificate's own common name %q", got, "brood")
	}
	// The report must carry the store's fingerprint, not the one this
	// process computed: the store's is the one peer-ca.pem publishes.
	if !strings.Contains(out, "SHA256:11:22:33") {
		t.Errorf("report does not carry the value the server returned:\n%s", out)
	}
	if !strings.Contains(out, "pinned 2, already pinned 0, of 2 members") {
		t.Errorf("report summary is wrong:\n%s", out)
	}
	if strings.Contains(out, "REFUSED") || strings.Contains(out, "FAILED") {
		t.Errorf("a clean run reported a refusal:\n%s", out)
	}
}

// A member already holding THIS certificate is reported and skipped,
// not pinned again and not refused. This is what makes re-running the
// command after a partial run safe.
func TestPinTrustedPeers_AlreadyPinnedIsReportedAndSkipped(t *testing.T) {
	dir := t.TempDir()
	brood := combCert(t, dir, "brood", "brood", "brood.lab3.home.arpa")
	drone := combCert(t, dir, "drone", "drone", "drone.lab3.home.arpa")

	backend := newCountingBackend()
	backend.store["brood.lab3.home.arpa"] = &internalpb.TrustedPeer{
		NodeId:      "brood.lab3.home.arpa",
		Fingerprint: fingerprintOf(t, brood),
	}

	code, out, errOut := runPin(t, backend, pinRequest{Bundle: bundle(t, brood, drone)})

	if code != 0 {
		t.Fatalf("exit status %d when one member was already pinned; want 0\n%s", code, errOut)
	}
	if got := backend.pinnedNodeIDs(); !equalStrings(got, []string{"drone.lab3.home.arpa"}) {
		t.Errorf("pinned %v; want only the member that was not already held", got)
	}
	if !strings.Contains(out, "already pinned to this certificate") {
		t.Errorf("report does not say the member was already held:\n%s", out)
	}
	if !strings.Contains(out, "pinned 1, already pinned 1, of 2 members") {
		t.Errorf("report summary is wrong:\n%s", out)
	}
	// The report is in the operator's order, not in an order invented
	// by the code: an operator reading it needs to find each member
	// where they put it.
	if strings.Index(out, "brood.lab3.home.arpa") > strings.Index(out, "drone.lab3.home.arpa") {
		t.Errorf("report lists members out of the order they were given:\n%s", out)
	}
}

// A run that stops mid-sequence says so, exits non-zero, and sends
// nothing further. The FSM refuses to replace a pin, so there is no
// rollback to attempt and the only useful thing is an accurate account
// of where it stopped.
func TestPinTrustedPeers_StopsAtTheFirstRefusal(t *testing.T) {
	dir := t.TempDir()
	brood := combCert(t, dir, "brood", "brood", "brood.lab3.home.arpa")
	drone := combCert(t, dir, "drone", "drone", "drone.lab3.home.arpa")
	sting := combCert(t, dir, "sting", "sting", "sting.lab3.home.arpa")

	backend := newCountingBackend()
	backend.refusals = map[string]string{
		"drone.lab3.home.arpa": "the FSM already holds a different pin for this node_id",
	}

	code, out, errOut := runPin(t, backend, pinRequest{Bundle: bundle(t, brood, drone, sting)})

	if code == 0 {
		t.Errorf("exit status 0 on a run that stopped at member two; a script would read this as success")
	}
	if got := backend.pinnedNodeIDs(); !equalStrings(got, []string{"brood.lab3.home.arpa", "drone.lab3.home.arpa"}) {
		t.Errorf("submitted %v; want the first two and nothing after the refusal", got)
	}
	if !strings.Contains(out, "REFUSED: the FSM already holds a different pin") {
		t.Errorf("report does not carry managerd's own words:\n%s", out)
	}
	if !strings.Contains(errOut, "did NOT finish") {
		t.Errorf("a stopped run did not say so on stderr:\n%s", errOut)
	}
	if strings.Contains(out, "pinned 2, already pinned 0, of 3 members") && !strings.Contains(errOut, "of 3") {
		t.Errorf("a stopped run reported a completed summary as if it had finished:\n%s", out)
	}
	// sting was never sent, so its absence from the report is the
	// report's way of saying it is NOT pinned.
	if strings.Contains(out, "sting.lab3.home.arpa") {
		t.Errorf("report mentions a member that was never submitted:\n%s", out)
	}
}

// A transport failure is not a refusal, and must not be rendered as
// one: an operator reading REFUSED would go looking for a certificate
// problem that is not there.
func TestPinTrustedPeers_TransportFailureIsNotRenderedAsARefusal(t *testing.T) {
	dir := t.TempDir()
	brood := combCert(t, dir, "brood", "brood", "brood.lab3.home.arpa")

	backend := newCountingBackend()
	backend.pinErr = fmt.Errorf("connection refused")

	code, out, _ := runPin(t, backend, pinRequest{Bundle: bundle(t, brood)})

	if code == 0 {
		t.Errorf("exit status 0 on a transport failure; want non-zero")
	}
	if !strings.Contains(out, "FAILED: connection refused") {
		t.Errorf("transport failure not reported as a failure:\n%s", out)
	}
	if strings.Contains(out, "REFUSED") {
		t.Errorf("a transport failure was rendered as a refusal:\n%s", out)
	}
}

// ---------------------------------------------------------------------
// Nothing here can invent trust
// ---------------------------------------------------------------------

// The command has no flag for a fingerprint, an expiry, a pinner, a pin
// time or a voter claim, and the message it puts on the wire has to
// reflect that. A command that could type a fingerprint would be a
// command that could type a lie the store then publishes as a trust
// anchor, so the wire message is checked field by field.
func TestPinTrustedPeers_NeverSendsADerivedValueOrAVoterClaim(t *testing.T) {
	dir := t.TempDir()
	brood := combCert(t, dir, "brood", "brood", "brood.lab3.home.arpa")

	backend := newCountingBackend()
	if code, _, errOut := runPin(t, backend, pinRequest{Bundle: bundle(t, brood)}); code != 0 {
		t.Fatalf("exit status %d; want 0\n%s", code, errOut)
	}
	if len(backend.pins) != 1 {
		t.Fatalf("submitted %d pins; want 1", len(backend.pins))
	}
	sent := backend.pins[0]
	if sent.GetFingerprint() != "" {
		t.Errorf("sent fingerprint %q; it must be derived by managerd from the certificate", sent.GetFingerprint())
	}
	if sent.GetNotAfterUnix() != 0 {
		t.Errorf("sent not_after_unix %d; it must be derived by managerd from the certificate", sent.GetNotAfterUnix())
	}
	if sent.GetPinnedBy() != "" {
		t.Errorf("sent pinned_by %q; it must be the accepting member's own node ID", sent.GetPinnedBy())
	}
	if sent.GetIsVoter() {
		t.Errorf("sent is_voter true; a pin is a trust anchor and never a membership claim")
	}
	if sent.GetCertPem() == "" {
		t.Errorf("sent no certificate")
	}
	if sent.GetPinnedAtUnix() == 0 {
		t.Errorf("sent no pinned_at_unix; the FSM evaluates the certificate as of that instant")
	}
}

// ---------------------------------------------------------------------
// The named mode
// ---------------------------------------------------------------------

// --node is an identity the operator typed, and it wins even when it
// disagrees with the certificate. Recording what was actually pinned is
// the whole point of the mode; quietly "correcting" the operator would
// pin something other than what they asked for.
func TestPinTrustedPeers_NamedModePinsUnderTheNodeGiven(t *testing.T) {
	dir := t.TempDir()
	// A certificate that says "drone", named as brood.
	cert := combCert(t, dir, "drone", "drone", "drone.lab3.home.arpa")

	backend := newCountingBackend()
	code, _, errOut := runPin(t, backend, pinRequest{
		Nodes: []string{"brood.lab3.home.arpa"},
		Certs: []string{cert},
	})

	if code != 0 {
		t.Fatalf("exit status %d; want 0\n%s", code, errOut)
	}
	if got := backend.pinnedNodeIDs(); !equalStrings(got, []string{"brood.lab3.home.arpa"}) {
		t.Errorf("pinned %v; want the --node value, not the certificate's own name", got)
	}
}

// An unpaired --node or --cert is a refusal, not a member to skip: a
// five-member Colony where one is silently missing is a Colony the
// operator believes is fully trusted and is not.
func TestPinTrustedPeers_UnpairedNodeAndCertPinsNothing(t *testing.T) {
	dir := t.TempDir()
	brood := combCert(t, dir, "brood", "brood", "brood.lab3.home.arpa")

	backend := newCountingBackend()
	code, _, errOut := runPin(t, backend, pinRequest{
		Nodes: []string{"brood.lab3.home.arpa", "drone.lab3.home.arpa"},
		Certs: []string{brood},
	})

	if code == 0 {
		t.Errorf("exit status 0 with two --node and one --cert; want a refusal")
	}
	if backend.listCalls != 0 || len(backend.pins) != 0 {
		t.Errorf("made %d store reads and %d pins; want 0 and 0", backend.listCalls, len(backend.pins))
	}
	if !strings.Contains(errOut, "same number of times") {
		t.Errorf("refusal does not say what is wrong: %s", errOut)
	}
}

// ---------------------------------------------------------------------
// The wire: a real gRPC server, for the values that only exist there
// ---------------------------------------------------------------------

// stubManager serves PinPeerCertificate and nothing else. It is a stub,
// not a managerd: what is under test is what the command SENDS and how
// it reads the answer, and a real managerd would bring raft and bbolt
// in behind it for nothing.
type stubManager struct {
	rpcpb.UnimplementedManagerServiceServer

	mu     sync.Mutex
	seen   []*rpcpb.TrustedPeer
	auth   string
	refuse map[string]string
	// answerFingerprint is what the "store" will hold. It is not the
	// certificate's own fingerprint, so a report that used the local
	// value instead of the server's is detectable.
	answerFingerprint string
}

func (s *stubManager) PinPeerCertificate(ctx context.Context, req *rpcpb.PinPeerCertificateRequest) (*rpcpb.PinPeerCertificateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, proto.Clone(req.GetPeer()).(*rpcpb.TrustedPeer))
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("authorization"); len(vals) > 0 {
			s.auth = vals[0]
		}
	}
	if r, ok := s.refuse[req.GetPeer().GetNodeId()]; ok {
		return &rpcpb.PinPeerCertificateResponse{Error: r}, nil
	}
	return &rpcpb.PinPeerCertificateResponse{
		Peer: &rpcpb.TrustedPeer{
			NodeId:       req.GetPeer().GetNodeId(),
			CombName:     req.GetPeer().GetCombName(),
			CertPem:      req.GetPeer().GetCertPem(),
			Fingerprint:  s.answerFingerprint,
			NotAfterUnix: time.Now().Add(90 * 24 * time.Hour).Unix(),
			PinnedBy:     "brood.lab3.home.arpa",
		},
	}, nil
}

func (s *stubManager) requests() []*rpcpb.TrustedPeer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rpcpb.TrustedPeer(nil), s.seen...)
}

func (s *stubManager) authorization() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth
}

// storeOverride replaces the one half of the backend that reads the
// LOCAL raftd socket. That half is exercised for real in
// internal/localraft, against a real socket there; duplicating that
// machinery here would test the same dial twice and the pin path not at
// all, which is the half these cases are about.
type storeOverride struct {
	*managerdBackend
	store map[string]*internalpb.TrustedPeer
}

func (s storeOverride) ListTrustedPeers(context.Context) (map[string]*internalpb.TrustedPeer, error) {
	return s.store, nil
}

// serveManagerd stands the stub up on a real listener and returns a
// backend pointed at it.
func serveManagerd(t *testing.T, s *stubManager, apiKey string) pinBackend {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	srv := grpc.NewServer()
	rpcpb.RegisterManagerServiceServer(srv, s)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialling the stub: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return storeOverride{
		managerdBackend: &managerdBackend{
			client:    rpcpb.NewManagerServiceClient(conn),
			apiKey:    apiKey,
			transport: "the test's own plaintext listener",
		},
		store: map[string]*internalpb.TrustedPeer{},
	}
}

// The values above are checked against a struct-copying stub. This is
// the same run over a real socket, and it is the only place the
// certificate bytes, the Admin credential and the emptiness of the
// derived fields are observed as bytes on a wire rather than as fields
// a method was handed.
func TestManagerdBackend_PinTravelsOverARealRPC(t *testing.T) {
	dir := t.TempDir()
	brood := combCert(t, dir, "brood", "brood", "brood.lab3.home.arpa")
	drone := combCert(t, dir, "drone", "drone", "drone.lab3.home.arpa")

	stub := &stubManager{answerFingerprint: "SHA256:AA:BB", refuse: map[string]string{}}
	backend := serveManagerd(t, stub, "")

	code, out, errOut := runPin(t, backend, pinRequest{Bundle: bundle(t, brood, drone)})
	if code != 0 {
		t.Fatalf("exit status %d over a real socket; want 0\n%s", code, errOut)
	}

	seen := stub.requests()
	if len(seen) != 2 {
		t.Fatalf("the server saw %d requests; want 2", len(seen))
	}
	broodPEM, err := os.ReadFile(brood)
	if err != nil {
		t.Fatalf("reading %s: %v", brood, err)
	}
	if !strings.Contains(seen[0].GetCertPem(), "BEGIN CERTIFICATE") {
		t.Errorf("the certificate did not arrive as PEM:\n%s", seen[0].GetCertPem())
	}
	if normalisePEM(string(broodPEM)) != normalisePEM(seen[0].GetCertPem()) {
		t.Errorf("the certificate that arrived is not the one given:\ngiven  %q\narrived %q", normalisePEM(string(broodPEM)), normalisePEM(seen[0].GetCertPem()))
	}
	for i, p := range seen {
		if p.GetFingerprint() != "" || p.GetNotAfterUnix() != 0 || p.GetPinnedBy() != "" || p.GetIsVoter() {
			t.Errorf("request %d carried a derived value it had no business sending: %v", i, p)
		}
	}
	// The report shows what the STORE now holds, which is the server's
	// answer and not this process's own computation of the same fact.
	if !strings.Contains(out, "SHA256:AA:BB") {
		t.Errorf("the report does not carry the server's fingerprint:\n%s", out)
	}
}

// managerd reports an application-level refusal in the response's error
// field, not in a gRPC status. Reading it as either a success or a
// transport error is the failure mode, so it is checked here over a
// real socket.
func TestManagerdBackend_ApplicationRefusalIsReportedAsARefusal(t *testing.T) {
	dir := t.TempDir()
	brood := combCert(t, dir, "brood", "brood", "brood.lab3.home.arpa")

	stub := &stubManager{
		answerFingerprint: "SHA256:AA:BB",
		refuse:            map[string]string{"brood.lab3.home.arpa": "certificate is not valid as of the pinned_at_unix it carries"},
	}
	backend := serveManagerd(t, stub, "")

	code, out, _ := runPin(t, backend, pinRequest{Bundle: bundle(t, brood)})

	if code == 0 {
		t.Errorf("exit status 0 on a refusal from managerd; want non-zero")
	}
	if !strings.Contains(out, "REFUSED: certificate is not valid") {
		t.Errorf("managerd's own words did not reach the report:\n%s", out)
	}
}

// The Admin credential arrives as metadata on the RPC, never as a
// command-line argument, so that it never appears in `ps` output or a
// shell history. The wiring is only observable on the wire.
func TestManagerdBackend_AdminCredentialTravelsAsMetadata(t *testing.T) {
	dir := t.TempDir()
	brood := combCert(t, dir, "brood", "brood", "brood.lab3.home.arpa")

	stub := &stubManager{answerFingerprint: "SHA256:AA:BB", refuse: map[string]string{}}
	backend := serveManagerd(t, stub, "s3cret-admin-key")

	if code, _, errOut := runPin(t, backend, pinRequest{Bundle: bundle(t, brood)}); code != 0 {
		t.Fatalf("exit status %d; want 0\n%s", code, errOut)
	}
	if got := stub.authorization(); got != "Bearer s3cret-admin-key" {
		t.Errorf("the server saw authorization %q; want the bearer token from the environment", got)
	}
}

// ---------------------------------------------------------------------
// How the target is dialled
// ---------------------------------------------------------------------

// A trust anchor the operator named and that is not there is a refusal.
// Silently continuing without it would be a command that encrypts to
// whoever answers, which is not what asking for an anchor means.
func TestPinDialOption_RefusesAAnchorItWasNamedThatIsNotThere(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-bundle.pem")
	_, _, err := pinDialOption("brood.lab3.home.arpa:17700", missing, "")
	if err == nil {
		t.Fatal("a named trust anchor that does not exist was accepted; want a refusal")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("the refusal does not name the file: %v", err)
	}
}

// An anchor that exists gets a TLS dial and says so, because "how did
// these certificates travel" is a question this command should answer
// in the same terminal as the fingerprints.
func TestPinDialOption_UsesTLSWhenAnAnchorExists(t *testing.T) {
	dir := t.TempDir()
	anchor := filepath.Join(dir, "peer-ca.pem")
	certPEM, _, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName:  "brood.lab3.home.arpa",
		DNSNames:    []string{"brood.lab3.home.arpa"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	})
	if err != nil {
		t.Fatalf("issuing an anchor: %v", err)
	}
	if err := os.WriteFile(anchor, certPEM, 0o600); err != nil {
		t.Fatalf("writing %s: %v", anchor, err)
	}

	opt, transport, err := pinDialOption("brood.lab3.home.arpa:17700", anchor, "")
	if err != nil {
		t.Fatalf("a real anchor was refused: %v", err)
	}
	if opt == nil {
		t.Error("no dial option was produced")
	}
	if !strings.Contains(transport, "TLS") || !strings.Contains(transport, "brood.lab3.home.arpa") {
		t.Errorf("transport description is %q; it must say what was verified against what", transport)
	}
}

// A DERIVED anchor that is absent means this Colony has pinned nothing
// yet, which is the state this command is run from. Refusing there
// would make the command unrunnable everywhere it is needed, so it
// falls back to plaintext and says so out loud.
func TestPinDialOption_DerivedAnchorAbsentFallsBackToPlaintextAndSaysSo(t *testing.T) {
	if _, err := os.Stat("/usr/local/etc/apiary/peer-ca.pem"); err == nil {
		t.Skip("this machine has the derived peer CA, so the absent-anchor case cannot be exercised here")
	}
	_, transport, err := pinDialOption("brood.lab3.home.arpa:17700", "", "")
	if err != nil {
		t.Fatalf("a Colony with no derived anchor was refused: %v", err)
	}
	if !strings.Contains(transport, "plaintext") {
		t.Errorf("transport description is %q; a run with no trust anchor must not read as a secure one", transport)
	}
}

// A target with no port has no name in it to verify a certificate
// against, and guessing one would be exactly the ambiguity the rest of
// this file refuses.
func TestPinDialOption_RefusesATargetWithNoPort(t *testing.T) {
	_, _, err := pinDialOption("brood.lab3.home.arpa", "", "")
	if err == nil {
		t.Fatal("a target with no port was accepted; want a refusal")
	}
	if !strings.Contains(err.Error(), "HOST:PORT") {
		t.Errorf("the refusal does not say what a target has to look like: %v", err)
	}
}

// --peer-name is how a target that is a bare IP, or a certificate
// naming something other than the address, is verified against a real
// name. It overrides the address; it never replaces the anchor.
func TestPinDialOption_PeerNameOverridesTheAddressHost(t *testing.T) {
	dir := t.TempDir()
	anchor := filepath.Join(dir, "peer-ca.pem")
	certPEM, _, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName:  "brood.lab3.home.arpa",
		DNSNames:    []string{"brood.lab3.home.arpa"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	})
	if err != nil {
		t.Fatalf("issuing an anchor: %v", err)
	}
	if err := os.WriteFile(anchor, certPEM, 0o600); err != nil {
		t.Fatalf("writing %s: %v", anchor, err)
	}
	_, transport, err := pinDialOption("10.90.0.94:17700", anchor, "brood.lab3.home.arpa")
	if err != nil {
		t.Fatalf("a named peer with a real anchor was refused: %v", err)
	}
	if !strings.Contains(transport, `"brood.lab3.home.arpa"`) {
		t.Errorf("transport description is %q; --peer-name did not reach it", transport)
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func fingerprintOf(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	info, err := hostcert.Evaluate(data, hostcert.EvaluateOptions{RequireDNSSAN: true, RequireLoopbackSAN: true})
	if err != nil {
		t.Fatalf("evaluating %s: %v", path, err)
	}
	return info.Fingerprint
}

// normalisePEM reduces a PEM to the certificate body, so a comparison
// is about which certificate arrived rather than about line endings or
// the trailing newline a file happens to have.
func normalisePEM(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\r\n", "\n")), "\n")
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------

// A subcommand that is written but not wired into main() is not a
// subcommand. These run the real binary, so a case removed from the
// switch fails here even though every line of it is still in the source.
//
// This is a text assertion, and the reason is stated in
// forcerestart_test.go: the text is the deliverable. There is no code
// path that can be executed to find out what an operator is told before
// running a command, because the telling IS the command.
func TestPinTrustedPeers_IsDispatchedAndExplainsItself(t *testing.T) {
	out := apiaryctlOutput(t, "pin-trusted-peers", "--help")
	if strings.Contains(out, "unknown subcommand") {
		t.Fatalf("apiaryctl pin-trusted-peers is not dispatched:\n%s", out)
	}
	if !strings.Contains(out, "usage: apiaryctl pin-trusted-peers") {
		t.Errorf("the subcommand's own usage was not printed:\n%s", out)
	}
	// The four refusals are what makes this command safe to hand an
	// operator, so help that does not mention them is help that
	// describes a command nobody should run.
	for _, want := range []string{
		"--bundle",
		"--target",
		"There is no flag for a fingerprint",
		"pins nothing at all",
		"reads the store and never writes to it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the usage text does not say %q:\n%s", want, out)
		}
	}
}

// `apiaryctl help` is the page an operator finds first, and a command
// missing from it is a command they will never find.
func TestUsage_ListsPinTrustedPeers(t *testing.T) {
	out := apiaryctlOutput(t, "help")
	if !strings.Contains(out, "apiaryctl pin-trusted-peers") {
		t.Errorf("`apiaryctl help` does not list pin-trusted-peers:\n%s", out)
	}
}

// --target is required and is checked before anything is opened, so the
// refusal is reachable without root and says which address shape it
// wants rather than failing on a dial.
func TestPinTrustedPeers_RefusesWithoutATarget(t *testing.T) {
	out := apiaryctlOutput(t, "pin-trusted-peers", "--bundle", "/nonexistent/peers.pem")
	if strings.Contains(out, "unknown subcommand") {
		t.Fatalf("apiaryctl pin-trusted-peers is not dispatched:\n%s", out)
	}
	if !strings.Contains(out, "--target is required") {
		t.Errorf("a run with no --target did not say so:\n%s", out)
	}
}

// The two ways of naming the members are mutually exclusive. Using both
// would leave the answer to whichever ran first, which is a store
// written from an ambiguous request.
func TestPinTrustedPeers_RefusesBundleAndNamedModesTogether(t *testing.T) {
	out := apiaryctlOutput(t, "pin-trusted-peers",
		"--target", "brood.lab3.home.arpa:17700",
		"--bundle", "/nonexistent/peers.pem",
		"--node", "brood.lab3.home.arpa", "--cert", "/nonexistent/brood.pem")
	if !strings.Contains(out, "Use one") {
		t.Errorf("--bundle together with --node/--cert was accepted:\n%s", out)
	}
}

// Needing root is said plainly, before any file is read and before any
// socket is opened, so that a non-root operator learns it immediately.
func TestPinTrustedPeers_RefusesWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this test is about the refusal a non-root caller gets")
	}
	out := apiaryctlOutput(t, "pin-trusted-peers",
		"--target", "brood.lab3.home.arpa:17700", "--bundle", "/nonexistent/peers.pem")
	if !strings.Contains(out, "must run as root") {
		t.Errorf("a non-root run did not say it needs root:\n%s", out)
	}
	if strings.Contains(out, "peers.pem") && strings.Contains(out, "certificate") {
		t.Errorf("a non-root run read the certificates before refusing:\n%s", out)
	}
}
