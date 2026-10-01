// apiaryctl pin-trusted-peers backfills ADR-0147 Part 4's replicated
// peer trust store on a Colony that predates it.
//
// WHY THIS EXISTS, in one fact: the only writer of a pin is the join
// flow, and the join flow only ever ran for a Comb that joined AFTER
// the trust store existed. A Colony that was built before it has five
// members, five serving certificates, and an empty store, so the
// derived /usr/local/etc/apiary/peer-ca.pem has nothing in it and
// peer TLS has no anchors to verify against. There is no upgrade step
// that fixes that, because the members' certificates are already
// issued and the store is keyed on identities that already exist. The
// operator path is this command, and it is deliberately a command
// rather than a page: the thing an operator has to do is look at five
// certificates, not fill in five forms.
//
// FOUR REFUSALS carry the design, and each of them exists because the
// failure it prevents is silent.
//
// **Nothing here can invent trust.** There is no flag for a
// fingerprint, an expiry, a pinner or a pin time, and there is no
// argument through which one could be passed: the fingerprint and the
// expiry are re-derived by managerd and again by every FSM that applies
// the entry, from the certificate itself, and pinned_by is the
// accepting member's own node ID. A tool that could type a fingerprint
// would be a tool that could type a lie that the store then publishes
// as a trust anchor.
//
// **The preflight is all-or-nothing.** Every input certificate is
// parsed, evaluated and matched against the store BEFORE the first
// PinPeerCertificate is sent. A bundle whose fourth certificate is bad
// produces zero RPCs, not three pins and a message. Pins cannot be
// replaced by the FSM - it refuses a second pin for a node_id - so a
// partial run is not a state this Colony can be put back out of, and
// that asymmetry is the whole reason the refusals are here rather than
// in the caller.
//
// **Ambiguity is refused, not resolved.** In bundle mode the node ID
// comes off the certificate: its common name, cross-checked against
// its own SANs, extended to the one qualified name that a Colony
// actually uses as a raft identity when the common name is a bare
// short hostname. A certificate that does not say which name it is,
// or that names two equally plausible ones, is refused.
//
// **Voter state is not this command's business.** It reads the store
// and never writes to it, and PinTrustedPeer's own FSM arm refuses
// is_voter on a pin. These members' membership predates the store and
// lives in raft's configuration; a pin here says "this certificate is
// trusted", never "this Comb is a member", and the two are separate
// records in the state proto for that reason.

package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/localraft"
	"github.com/glenjbarber/apiary/internal/peerca"
	"github.com/glenjbarber/apiary/internal/tlsdial"
)

// managerAPIKeyEnv names the environment variable carrying an Admin
// API key for the --target member, and the same name
// internal/frontendconfig and cmd/restshimd already use for exactly
// this: a managerd client credential that was not minted at the
// command line.
//
// It is an environment variable and not a flag on purpose. A flag
// value is in the operator's shell history and in `ps` output for the
// life of the process, and this credential is an Admin of the whole
// Colony. It is also optional, and the command works without it: a
// Colony that has never created an API key lets every call through
// unauthenticated, which is the ordinary case for the pre-ADR-0147
// Colonies this command exists to serve.
const managerAPIKeyEnv = "APIARY_MANAGER_API_KEY"

// pinTimeout bounds one whole run's worth of RPCs. Long enough for a
// five-member Colony on a slow link, short enough that an unreachable
// member is a bounded pause rather than a hang.
const pinTimeout = 60 * time.Second

// pinTarget is one member this command is about to pin, decided
// entirely from local material before any RPC is sent.
//
// The three identity fields are DERIVED and are the only ones the
// command sets on the wire. Fingerprint and NotAfterUnix are absent
// from the message on purpose: managerd re-derives both from CertPem
// and overwrites whatever arrived, and leaving them unset here means
// there is exactly one place they can come from.
type pinTarget struct {
	nodeID      string
	combName    string
	certPEM     string
	fingerprint string
	notAfter    time.Time
}

// where names the file a pinTarget came from, for the refusal that
// has to name it. "peers.pem" alone would leave an operator with a
// bundle of five certificates and no idea which one the message meant.
func (t pinTarget) where(source string) string {
	return source + " (" + t.nodeID + ")"
}

// pinBackend is the two things this command does with a Colony: read
// the trust store, and write one pin into it.
//
// An interface, and the reason is testability of the refusals rather
// than tidiness. Every property this file is built around is a
// property of the ORDER of operations relative to the network - a
// bundle with a bad fourth certificate must produce zero RPCs - and a
// function that reaches for a real socket cannot have that proven
// without a real socket. The tests inject a backend that counts, and
// one of them injects a real gRPC server for the wire itself.
type pinBackend interface {
	// ListTrustedPeers returns the store as it stands, keyed by node ID.
	ListTrustedPeers(ctx context.Context) (map[string]*internalpb.TrustedPeer, error)

	// Pin submits one pin and returns the server's own record of it.
	// An error here is transport or authorization; a REFUSAL is a
	// non-empty refusal string on a nil error, because that is how
	// managerd reports every application-level refusal. The two are
	// kept apart because an operator needs to know which of them
	// happened: one is "this Colony will not take this pin", the other
	// is "this tool could not reach it".
	Pin(ctx context.Context, peer *rpcpb.TrustedPeer) (*rpcpb.TrustedPeer, string, error)
}

// pinOutcome is what happened to one member, and is what the final
// report is built from. Kept rather than printed-as-you-go so that a
// run which stops at member three can still say precisely which three
// and which two.
type pinOutcome struct {
	target pinTarget
	// already is true when the store already held this exact
	// certificate for this node ID. Not a failure and not a new pin:
	// the FSM refuses to replace a pin, so a repeat is a no-op the
	// operator is told about rather than an error.
	already bool
	// refusal is managerd's own word, or "" when the pin succeeded.
	//
	// It never carries a success. An earlier draft had one field
	// holding either, with "pinned" as its success value, and asked
	// whether a member was refused by testing that field for
	// emptiness - which made "pinned" read as a refusal, stopped every
	// run after its first member, and printed REFUSED: pinned. A field
	// that can hold both outcomes cannot be tested for one of them, so
	// it holds only the bad one.
	refusal string
	// err is the transport or authorization failure, if any.
	err error
}

// refused reports whether something is a refusal rather than a success.
// managerd reports application refusals in the message rather than in
// a gRPC status, so the string is the only signal there is.
func refused(refusal string) bool { return refusal != "" }

// ok reports whether one member reached the end state the operator
// wanted: pinned, or already holding this exact certificate.
func (o pinOutcome) ok() bool { return o.err == nil && !refused(o.refusal) }

// runPinTrustedPeers is the whole subcommand: parse, refuse on
// anything malformed, then hand off to pinTrustedPeers, which is where
// every behaviour above actually lives and is tested.
func runPinTrustedPeers(args []string) int {
	fs := flag.NewFlagSet("apiaryctl pin-trusted-peers", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		target      string
		bundle      string
		raftdConfig string
		peerCA      string
		peerName    string
		nodes       repeatedFlag
		certs       repeatedFlag
	)
	fs.StringVar(&target, "target", "", "host:port of ANY member of this Colony, not only its leader (required)")
	fs.StringVar(&bundle, "bundle", "", "a PEM file holding every member's serving certificate; each one's identity is read off the certificate itself")
	fs.Var(&nodes, "node", "a member's raft node_id, for the mode where identity is named rather than derived; repeatable, paired with --cert")
	fs.Var(&certs, "cert", "the PEM file holding that member's serving certificate; repeatable, paired with --node")
	fs.StringVar(&raftdConfig, "raftd-config", "", "path to this Comb's own raftd.json, to read its local copy of the trust store; defaults to the raftd default")
	fs.StringVar(&peerCA, "peer-ca", "", "PEM file of trust anchors for the --target member's certificate; defaults to "+peerca.DefaultPath+", the file managerd derives from its own copy of the store")
	fs.StringVar(&peerName, "peer-name", "", "the name to verify the --target member's certificate against; defaults to the host in --target, and is needed only when that is a bare IP or the certificate names something else")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: apiaryctl pin-trusted-peers --target HOST:PORT --bundle FILE
       apiaryctl pin-trusted-peers --target HOST:PORT --node ID --cert FILE [--node ID --cert FILE ...]

Backfill the replicated peer trust store (ADR-0147 Part 4) on a Colony
that predates it, by pinning each member's verified serving
certificate.

  --bundle FILE    every member's certificate in one PEM file. Each
                   certificate's node_id is derived from its own common
                   name, cross-checked against its SANs. A certificate
                   that does not say which name it is, a name repeated
                   in the bundle, and a file with no certificates in
                   it are all refusals
  --node ID        a member's raft node_id, named rather than derived.
                   Repeatable, and every --node needs a --cert and
                   every --cert needs a --node
  --cert FILE      that member's certificate. Same rule, and the
                   certificate's own subject is NOT used as an
                   identity: it is pinned under the --node you named
  --target H:P     any member of this Colony, not only its leader
  --peer-ca FILE   trust anchors for the target's certificate. The
                   default is the file managerd derives from this
                   Colony's own trust store. A DERIVED file that is
                   absent is not an error - it is what this Colony
                   looks like before anything is pinned - and the run
                   then goes out in plaintext, and says so. A file
                   you named yourself and that is missing IS an error
  --peer-name NAME verify the target's certificate against NAME rather
                   than against the host in --target

There is no flag for a fingerprint, an expiry, a pinner or a pin time.
The fingerprint and the expiry are derived from the certificate by
managerd and again by every member applying the log entry, and the
pinner is the accepting member's own node ID. A tool that could type
a fingerprint would be a tool that could type a lie the store then
publishes as a trust anchor.

Every input is parsed and checked before the first pin is sent, so a
bundle with one bad certificate pins nothing at all. This command
reads the store and never writes to it: it does not add, remove or
alter raft membership, and a Comb's voter state is not its business.

The store is read through this Comb's OWN raftd over its local socket,
so run it on a member of the Colony, as root.
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "apiaryctl pin-trusted-peers: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "apiaryctl pin-trusted-peers: --target is required. Name ANY member of THIS Colony: the pin is replicated, so it does not have to be the leader, and it does have to be a member of the Colony the certificates belong to.")
		return 2
	}
	switch {
	case bundle != "" && (len(nodes) > 0 || len(certs) > 0):
		fmt.Fprintln(os.Stderr, "apiaryctl pin-trusted-peers: --bundle and --node/--cert are two ways of saying the same list, and using both leaves the answer to whichever ran first. Use one.")
		return 2
	case bundle == "" && len(nodes) == 0 && len(certs) == 0:
		fmt.Fprintln(os.Stderr, "apiaryctl pin-trusted-peers: nothing to pin. Pass --bundle FILE for a Colony's whole certificate set, or --node ID --cert FILE for the members you are naming one at a time.")
		return 2
	}

	// Root only, and the same check and the same reasoning as
	// force-restart: the local raftd socket this command reads the
	// store through is mode 0660 inside a 0700 directory, and the whole
	// command is an operation on the Colony's trust anchors. Nothing
	// here discovers permissions on the way to failing.
	if os.Geteuid() != 0 {
		fmt.Fprint(os.Stderr, `apiaryctl pin-trusted-peers: must run as root.

It reads this Comb's own raftd socket, which is 0660 inside a 0700
directory, and it writes the Colony's trust store. Run it from a root
shell on a member of the Colony, or with sudo.
`)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), pinTimeout)
	defer cancel()

	backend, err := dialPinBackend(ctx, target, raftdConfig, peerCA, peerName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apiaryctl pin-trusted-peers: %v\n", err)
		return 1
	}
	defer backend.Close()

	// Said before anything is pinned, because "how did these five
	// certificates just travel" is a question an operator asks of
	// this command afterwards, and the honest answer has to be in the
	// same terminal as the fingerprints.
	fmt.Fprintf(os.Stderr, "apiaryctl pin-trusted-peers: submitting to %s over %s, reading this Comb's own trust store from its raftd\n", target, backend.transport)

	return pinTrustedPeers(ctx, backend, pinRequest{
		Bundle: bundle,
		Nodes:  nodes,
		Certs:  certs,
	}, os.Stdout, os.Stderr)
}

// pinRequest is the whole input surface, as values rather than flags,
// so that both modes are the same code path and a test can build one
// without going through the parser.
type pinRequest struct {
	Bundle string
	Nodes  []string
	Certs  []string
}

// pinTrustedPeers is the command, minus flag parsing and the root
// check, and it is the whole of the behaviour: derive, validate,
// refuse, then pin in order and report what is true afterwards.
func pinTrustedPeers(ctx context.Context, backend pinBackend, req pinRequest, out, errOut io.Writer) int {
	// ---- Phase 1: everything below happens before the first RPC. ----
	targets, err := planPins(req)
	if err != nil {
		fmt.Fprintf(errOut, "apiaryctl pin-trusted-peers: %v\n", err)
		fmt.Fprintln(errOut, "Nothing was pinned. Every input is checked before the first pin is sent, so a bundle with one bad certificate leaves the store exactly as it was.")
		return 1
	}

	// The store is read here, before anything is written, and for two
	// reasons rather than one. The obvious one is to report "already
	// pinned" honestly instead of discovering it as a refusal halfway
	// through a five-member run. The load-bearing one is the other
	// direction: a node that is already pinned to a DIFFERENT
	// certificate cannot be pinned again at all, and finding that out
	// from the FSM's refusal halfway through would mean the earlier
	// members are now pinned and this one is not.
	existing, err := backend.ListTrustedPeers(ctx)
	if err != nil {
		fmt.Fprintf(errOut, "apiaryctl pin-trusted-peers: reading this Colony's trust store: %v\n", err)
		fmt.Fprintln(errOut, "Nothing was pinned. The store is read before any pin is sent, so that a member already holding a different certificate is caught here rather than halfway through.")
		return 1
	}
	if err := refuseAgainstStore(targets, existing); err != nil {
		fmt.Fprintf(errOut, "apiaryctl pin-trusted-peers: %v\n", err)
		fmt.Fprintln(errOut, "Nothing was pinned.")
		return 1
	}

	// ---- Phase 2: the pins, in the order the operator gave them. ----
	//
	// One pass, so that the report below is in the operator's order
	// rather than in an order invented here: a member already holding
	// this certificate takes its place in the sequence without an RPC,
	// and the first member that actually refuses ends the run.
	outcomes := make([]pinOutcome, 0, len(targets))
	for _, t := range targets {
		if outcome, skip := alreadyPinned(t, existing); skip {
			outcomes = append(outcomes, outcome)
			continue
		}
		o := pinOne(ctx, backend, t)
		outcomes = append(outcomes, o)
		if !o.ok() {
			// Stop at the first failure. There is no rollback to
			// attempt and there could not be one: the FSM refuses to
			// replace a pin, so unpinning four members because the
			// fifth failed would be a far larger and less reversible
			// event than the one being reported. The report below
			// says exactly where it stopped, which is the only thing
			// an operator can act on.
			break
		}
	}
	return report(out, errOut, outcomes, len(targets))
}

// planPins turns a request into the ordered list of members to pin,
// reading every certificate and refusing on the first thing that is
// wrong. It touches no network: this is the all-or-nothing
// preflight, and a caller that skips it has skipped the property.
func planPins(req pinRequest) ([]pinTarget, error) {
	if req.Bundle != "" {
		return planBundlePins(req.Bundle)
	}
	return planExplicitPins(req.Nodes, req.Certs)
}

// planBundlePins is bundle mode: identity comes off the certificate.
func planBundlePins(path string) ([]pinTarget, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the bundle %s: %w", path, err)
	}
	certs, err := splitPEMCertificates(data)
	if err != nil {
		return nil, fmt.Errorf("bundle %s: %w", path, err)
	}
	// A bundle of nothing is a refusal, not an empty success. The
	// operator asked to pin a Colony's members and named no members;
	// exiting 0 with "pinned 0" is indistinguishable in a log from a
	// run that did its job.
	if len(certs) == 0 {
		return nil, fmt.Errorf("bundle %s contains no CERTIFICATE blocks. A bundle is every member's certificate in one file; an empty one pins nothing and says so by succeeding, which is the one outcome an operator would not notice", path)
	}
	targets := make([]pinTarget, 0, len(certs))
	for i, c := range certs {
		info, err := hostcert.Evaluate(c.pem, hostcert.EvaluateOptions{RequireDNSSAN: true, RequireLoopbackSAN: true})
		if err != nil {
			return nil, fmt.Errorf("bundle %s: certificate %d of %d: %v", path, i+1, len(certs), err)
		}
		cn, err := subjectCommonName(c.pem)
		if err != nil {
			return nil, fmt.Errorf("bundle %s: certificate %d of %d (%s): %v", path, i+1, len(certs), c.source, err)
		}
		nodeID, err := deriveNodeID(cn, info)
		if err != nil {
			return nil, fmt.Errorf("bundle %s: certificate %d of %d (%s): %v", path, i+1, len(certs), c.source, err)
		}
		targets = append(targets, newPinTarget(nodeID, cn, info, c.pem))
	}
	if err := refuseDuplicateNodeIDs(targets, "bundle"); err != nil {
		return nil, err
	}
	return targets, nil
}

// planExplicitPins is the named mode: the operator says which member
// each certificate is for, and nothing is inferred.
//
// The certificate is still evaluated, and still has to be a Comb
// serving certificate, because managerd and the FSM will both refuse
// anything else and a refusal from the middle of a five-member run is
// a worse place to learn it. Only the IDENTITY is taken from the
// flag: a certificate whose subject says "drone" can be pinned under
// --node brood, and that is the operator's call to make and this
// command's job to record faithfully.
func planExplicitPins(nodes, certs []string) ([]pinTarget, error) {
	if len(nodes) != len(certs) {
		return nil, fmt.Errorf("--node and --cert must be given the same number of times, and they were given %d and %d. Every named member needs its certificate and every certificate needs its member; a pair that is missing one of the two is a member this run would silently skip", len(nodes), len(certs))
	}
	if len(nodes) == 0 {
		return nil, errors.New("nothing to pin: no --node/--cert pair was given")
	}
	targets := make([]pinTarget, 0, len(nodes))
	for i, path := range certs {
		nodeID := strings.TrimSpace(nodes[i])
		if nodeID == "" {
			return nil, fmt.Errorf("--node %d is empty. A pin is keyed on node_id, and the store's own refusal for an empty one is the least useful thing an operator can be handed here", i+1)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("--node %s: reading its certificate %s: %w", nodeID, path, err)
		}
		pemBytes, err := onePEMCertificate(data)
		if err != nil {
			return nil, fmt.Errorf("--node %s: %s: %w", nodeID, path, err)
		}
		info, err := hostcert.Evaluate(pemBytes, hostcert.EvaluateOptions{RequireDNSSAN: true, RequireLoopbackSAN: true})
		if err != nil {
			return nil, fmt.Errorf("--node %s: %s: %v", nodeID, path, err)
		}
		// The comb name is a label and never an identity here, so it
		// is read off the certificate rather than derived. It may be
		// empty - a certificate with no CN is refused in bundle mode
		// and perfectly pinnable in this one, where the operator has
		// said what it is - and newPinTarget falls back to the node ID.
		cn, err := subjectCommonName(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("--node %s: %s: %v", nodeID, path, err)
		}
		targets = append(targets, newPinTarget(nodeID, cn, info, pemBytes))
	}
	if err := refuseDuplicateNodeIDs(targets, "request"); err != nil {
		return nil, err
	}
	return targets, nil
}

// newPinTarget builds the one record this command ever sends. Note
// what is not in it: no fingerprint, no not_after, no pinned_by, no
// pinned_at, no is_voter. Every one of those is either re-derived from
// the certificate on the other side or assigned by the member
// accepting the pin, and a value this command could have supplied is
// a value that could have been wrong.
func newPinTarget(nodeID, comb string, info hostcert.Info, certPEM []byte) pinTarget {
	// The comb name is the certificate's own subject common name when
	// there is one, and the node ID otherwise. It is the descriptive
	// label the state proto says nothing may be refused or routed on,
	// so the honest value is the one the certificate states about
	// itself; the FQDN an operator named is a better label than
	// nothing when a certificate has no subject at all.
	if strings.TrimSpace(comb) == "" {
		comb = nodeID
	}
	return pinTarget{
		nodeID:      nodeID,
		combName:    comb,
		certPEM:     string(certPEM),
		fingerprint: info.Fingerprint,
		notAfter:    info.NotAfter,
	}
}

// refuseDuplicateNodeIDs is one of the two bundle-mode refusals that
// cannot be left to the FSM.
//
// The store is keyed on node_id, so a bundle naming the same member
// twice is two pins for one key. The second would be refused by the
// FSM - and it would be refused AFTER the first had been written,
// which is exactly the partial state this command is built to avoid.
// Two certificates claiming the same node_id is also a real thing
// worth naming: it is what a mis-collected bundle looks like.
func refuseDuplicateNodeIDs(targets []pinTarget, source string) error {
	seen := make(map[string]string, len(targets))
	for _, t := range targets {
		if first, dup := seen[t.nodeID]; dup {
			return fmt.Errorf("%s names node_id %q twice: once from %s and once from %s. The store is keyed on node_id, so the second pin would be refused after the first had already been written", source, t.nodeID, first, t.where(source))
		}
		seen[t.nodeID] = t.where(source)
	}
	return nil
}

// refuseAgainstStore is the refusal the store itself forces, raised
// before the first pin rather than halfway through.
//
// A pin cannot be replaced: the FSM's own arm says so by name, and
// UnpinPeerCertificate is the only way out, which is a separate
// decision this command should not make for an operator. Learning
// that from the middle of a run is how a Colony ends up with three new
// pins and an unresolved fourth.
func refuseAgainstStore(targets []pinTarget, existing map[string]*internalpb.TrustedPeer) error {
	for _, t := range targets {
		held, ok := existing[t.nodeID]
		if !ok {
			continue
		}
		if held.GetFingerprint() == t.fingerprint {
			continue
		}
		return fmt.Errorf("%s is already pinned to a DIFFERENT certificate (%s, expiring %s). A pin cannot be replaced: unpin it first, with `apiaryctl` calling UnpinPeerCertificate, which is a separate and visible act. This command will not unpin anything, and pinning the new certificate anyway would leave the store publishing one identity while verifying another",
			t.nodeID, held.GetFingerprint(), unixTime(held.GetNotAfterUnix()))
	}
	return nil
}

// alreadyPinned reports the outcome for a member the store already
// holds under this exact certificate: no RPC is sent for it, and the
// report says so rather than claiming a pin happened.
//
// It is deliberately a separate function from refuseAgainstStore even
// though both read the one store read. The store was read before any
// pin was sent, and asking it a second question later - even the same
// question - is how a command ends up disagreeing with itself about
// what it saw.
func alreadyPinned(t pinTarget, existing map[string]*internalpb.TrustedPeer) (pinOutcome, bool) {
	held, ok := existing[t.nodeID]
	if !ok || held.GetFingerprint() != t.fingerprint {
		return pinOutcome{}, false
	}
	return pinOutcome{target: t, already: true}, true
}

// pinOne submits a single pin and records what came back.
//
// pinned_at_unix is the one value this command puts on the wire that
// is not in the certificate, and it is the wall clock at the moment
// of the call, not an operator's to choose: the FSM evaluates the
// certificate AS OF that instant, on every voter and on every replay,
// and a time a caller could choose is a time a caller could choose to
// be in the past, which is a way of pinning an expired certificate.
func pinOne(ctx context.Context, backend pinBackend, t pinTarget) pinOutcome {
	resp, refusal, err := backend.Pin(ctx, &rpcpb.TrustedPeer{
		NodeId:       t.nodeID,
		CombName:     t.combName,
		CertPem:      t.certPEM,
		PinnedAtUnix: time.Now().Unix(),
		// IsVoter is left false and said to be false in the store's
		// own doc comment. The FSM refuses a pin that claims
		// membership, because these members' membership is raft's
		// configuration and predates this store entirely.
		IsVoter: false,
	})
	o := pinOutcome{target: t, refusal: refusal, err: err}
	if !o.ok() {
		return o
	}
	if resp.GetNodeId() != t.nodeID {
		o.err = fmt.Errorf("managerd answered about %q, not %q", resp.GetNodeId(), t.nodeID)
		return o
	}
	// The server's own values, not ours. The fingerprint the operator
	// reads here is the one the store is holding, which is the one the
	// derived peer-ca.pem will publish.
	o.target.fingerprint = resp.GetFingerprint()
	if resp.GetNotAfterUnix() > 0 {
		o.target.notAfter = time.Unix(resp.GetNotAfterUnix(), 0).UTC()
	}
	return o
}

// report prints what is true now, which after a failure mid-sequence
// is the only thing that matters, and returns the exit status.
//
// The status is the return value rather than a constant 0 because a
// run that stopped at member three and a run that pinned all five
// produce the same exit code otherwise, and the stopped one is the
// only case an operator's script has to notice.
func report(out, errOut io.Writer, outcomes []pinOutcome, planned int) int {
	pinned, already := 0, 0
	stopped := false
	for _, o := range outcomes {
		switch {
		case !o.ok():
			stopped = true
		case o.already:
			already++
		default:
			pinned++
		}
		fmt.Fprintf(out, "%s\n", o.target.nodeID)
		fmt.Fprintf(out, "  comb name    %s\n", o.target.combName)
		fmt.Fprintf(out, "  fingerprint  %s\n", o.target.fingerprint)
		fmt.Fprintf(out, "  not after    %s\n", o.target.notAfter.UTC().Format("2006-01-02 15:04:05Z"))
		switch {
		case o.err != nil:
			fmt.Fprintf(out, "  result       FAILED: %v\n", o.err)
		case refused(o.refusal):
			fmt.Fprintf(out, "  result       REFUSED: %s\n", o.refusal)
		case o.already:
			fmt.Fprintf(out, "  result       already pinned to this certificate\n")
		default:
			fmt.Fprintf(out, "  result       pinned\n")
		}
	}
	summary := fmt.Sprintf("pinned %d, already pinned %d, of %d members", pinned, already, planned)
	if len(outcomes) < planned {
		stopped = true
	}
	if stopped {
		fmt.Fprintf(errOut, "\napiaryctl pin-trusted-peers: this run did NOT finish. %s.\n", summary)
		fmt.Fprintln(errOut, "  The members above are whole: a pin is written whole or not at all.")
		fmt.Fprintln(errOut, "  The ones not listed are NOT pinned, and not partially pinned either.")
		fmt.Fprintf(errOut, "  Fix the cause and run this again. A member already pinned to this\n")
		fmt.Fprintln(errOut, "  certificate is reported and skipped, so a second run is safe.")
		fmt.Fprintln(errOut, "  Voter state was not touched: this command reads the store and pins")
		fmt.Fprintln(errOut, "  certificates.")
		return 1
	}
	fmt.Fprintf(out, "\n%s.\n", summary)
	fmt.Fprintln(out, "Voter state was not touched: this command reads the store and pins certificates.")
	return 0
}

// managerdBackend is the real pinBackend: the external ManagerService
// client, plus the local raftd read that has no external equivalent.
type managerdBackend struct {
	client  rpcpb.ManagerServiceClient
	apiKey  string
	raftCfg string
	// transport describes how the pins are travelling, in words an
	// operator can check against what they expected. See
	// pinDialOption for why it is worth a field rather than a log
	// line inside the dial.
	transport string
	closed    func()
}

// dialPinBackend opens both halves. target is any member of the
// Colony; raftdConfig names this Comb's own socket.
//
// The local raftd is probed here, before the certificates are read,
// rather than at first use. The refusal that matters to an operator is
// "this is not a member of the Colony", and that is worth saying
// before the certificates are read, not after.
func dialPinBackend(ctx context.Context, target, raftdConfig, peerCA, peerName string) (*managerdBackend, error) {
	dialOpt, transport, err := pinDialOption(target, peerCA, peerName)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(target, dialOpt)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", target, err)
	}
	closeFn := func() { conn.Close() }
	if _, err := localraft.Query(ctx, raftdConfig); err != nil {
		closeFn()
		return nil, fmt.Errorf("reading this Comb's own raftd, which is where the trust store is read from: %w - run this on a member of the Colony", err)
	}
	return &managerdBackend{
		client:    rpcpb.NewManagerServiceClient(conn),
		apiKey:    os.Getenv(managerAPIKeyEnv),
		raftCfg:   raftdConfig,
		transport: transport,
		closed:    closeFn,
	}, nil
}

// pinDialOption decides how to reach the --target member's managerd.
//
// This deliberately does NOT reuse join-introduce's dialTargetManager,
// and the difference is the whole point of the function. That dial is
// plaintext and unattached because a JOINER has no Colony credential
// yet, by definition, and must not send one to an address it typed.
// Here the situation is the opposite on both counts: this Comb is
// already a member of the Colony, and the target is a member of it
// too. managerd serves TLS whenever tls_cert and tls_key are set -
// every Apiary deployment configured by `apiaryctl install` sets both,
// and the testbed Combs have them - so a plaintext dial here would
// fail its handshake against exactly the Colony this command exists to
// repair. Reusing the joiner's dial would have made the command
// unrunnable everywhere it is needed.
//
// The trust anchors are the derived peer CA file managerd writes from
// its own replicated copy of the store, and the rules about a missing
// one are internal/manager's ResolvePeerCAPool's, restated here rather
// than imported: apiaryctl does not import the manager package, and a
// second copy of the rule is still one rule if the comment says which
// one it is. A DERIVED file that does not exist means this Colony has
// pinned nothing yet, which is the state this command is run from, so
// the run goes out in plaintext and says so rather than refusing. A
// file the operator named themselves and that is missing IS a refusal:
// they asked for a specific anchor and there is not one.
//
// The one-way TLS here is managerd's own: it presents a certificate
// and asks for nothing back. The client proves who it is by being the
// process that can write the Colony's trust store, which is the
// root-owned local raft socket dialed in dialPinBackend.
func pinDialOption(target, peerCA, peerName string) (grpc.DialOption, string, error) {
	if peerName == "" {
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			return nil, "", fmt.Errorf("--target %q is not HOST:PORT, so there is no name in it to verify a certificate against: %w. Give it host:port, and --peer-name if the certificate names something other than that host", target, err)
		}
		peerName = host
	}
	caPath, derived := peerCA, peerCA == ""
	if derived {
		caPath = peerca.DefaultPath
	}
	if _, err := os.Stat(caPath); err != nil {
		if derived && errors.Is(err, fs.ErrNotExist) {
			return grpc.WithTransportCredentials(insecure.NewCredentials()),
				fmt.Sprintf("plaintext, because this Colony has no trust anchors at %s yet", caPath), nil
		}
		return nil, "", fmt.Errorf("reading the trust anchors %s: %w. Name a file that exists, or pass no --peer-ca at all to use the derived one", caPath, err)
	}
	// One copy of the TLS rules, not a second one built here: the
	// package that builds this option exists precisely so that a
	// caller cannot verify the target with one set of settings and
	// then dial it with another.
	opt, err := tlsdial.ManagerDialOption(true, caPath, peerName)
	if err != nil {
		return nil, "", fmt.Errorf("trusting %s for %s: %w", caPath, peerName, err)
	}
	return opt, fmt.Sprintf("TLS, verifying %q against %s", peerName, caPath), nil
}

func (b *managerdBackend) Close() {
	if b.closed != nil {
		b.closed()
	}
}

func (b *managerdBackend) ListTrustedPeers(ctx context.Context) (map[string]*internalpb.TrustedPeer, error) {
	return localraft.ListTrustedPeers(ctx, b.raftCfg)
}

func (b *managerdBackend) Pin(ctx context.Context, peer *rpcpb.TrustedPeer) (*rpcpb.TrustedPeer, string, error) {
	if b.apiKey != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+b.apiKey)
	}
	resp, err := b.client.PinPeerCertificate(ctx, &rpcpb.PinPeerCertificateRequest{Peer: peer})
	if err != nil {
		if code := status.Code(err); code == codes.Unauthenticated || code == codes.PermissionDenied {
			return nil, "", fmt.Errorf("managerd refused the credential (this RPC is Admin-tier): %w. Export %s with an Admin key if this Colony has API-key auth turned on, which it does the moment its first key was ever created", err, managerAPIKeyEnv)
		}
		return nil, "", err
	}
	return resp.GetPeer(), resp.GetError(), nil
}

// pemCertificate is one CERTIFICATE block and where in a file it was
// found, so a refusal in a bundle of five can name the fifth.
type pemCertificate struct {
	pem    []byte
	source string
}

// splitPEMCertificates pulls every CERTIFICATE block out of a bundle.
//
// One block, not one file: a bundle is the point, and a file holding
// five certificates is what mode 1 is for. Anything that is not a
// CERTIFICATE block is refused rather than skipped, because a bundle
// with a key in it is a bundle whose operator has misunderstood what
// it is, and quietly ignoring the private key would be the kindest
// possible thing to do with a mistake this consequential.
func splitPEMCertificates(data []byte) ([]pemCertificate, error) {
	var out []pemCertificate
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("holds a %q block, and this command pins certificates, not keys. Read the certificate off the Comb with something that writes certificates", block.Type)
		}
		if len(block.Headers) > 0 {
			return nil, errors.New("holds a CERTIFICATE block with headers, which nothing here generates and nothing here can interpret")
		}
		out = append(out, pemCertificate{
			pem:    pem.EncodeToMemory(block),
			source: fmt.Sprintf("certificate %d in the bundle", len(out)+1),
		})
	}
	if len(out) == 0 && strings.TrimSpace(string(data)) != "" {
		return nil, errors.New("contains no PEM CERTIFICATE block at all")
	}
	return out, nil
}

// onePEMCertificate is the explicit mode's reader: exactly one
// certificate, and a refusal naming which of the three near-misses it
// is, because a two-certificate file passed to --cert is a different
// operator problem from a truncated one.
func onePEMCertificate(data []byte) ([]byte, error) {
	block, rest := pem.Decode(data)
	if block == nil {
		return nil, errors.New("does not contain a PEM CERTIFICATE block")
	}
	if block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("holds a %q block, and --cert names a certificate", block.Type)
	}
	if trimmed := strings.TrimSpace(string(rest)); trimmed != "" {
		if _, more := pem.Decode([]byte(trimmed)); more != nil {
			return nil, errors.New("holds more than one certificate. --cert names exactly one member; a bundle of them is what --bundle is for")
		}
	}
	return pem.EncodeToMemory(block), nil
}

// subjectCommonName returns the certificate's own subject common name,
// or "" when it has none.
//
// Read off the certificate rather than off hostcert.Info.Subject,
// because that field is cert.Subject.String(): on a Comb's certificate
// it renders as "CN=brood,O=apiary", which is a printable description
// and not a name anything could dial or key a raft identity on. An
// empty return is not itself a refusal here - bundle mode's
// deriveNodeID is where "a certificate that does not say which name it
// is" is caught, and explicit mode has already been told.
func subjectCommonName(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", errors.New("does not contain a PEM CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parsing its certificate: %w", err)
	}
	return strings.TrimSpace(cert.Subject.CommonName), nil
}

// deriveNodeID is bundle mode's identity derivation, and it is the
// third thing this command refuses rather than guesses.
//
// A certificate states its identity in two places and they are allowed
// to differ in a specific, known way. internal/hostinstall issues
// CommonName: the SHORT hostname and puts the FQDN in the SANs
// alongside it, because the short name is what an operator types. A
// Colony's raft identity is the FQDN. So:
//
//   - no common name at all, or an empty one, is a refusal: there is
//     nothing to derive from.
//   - a common name that is not among the certificate's own DNS SANs
//     is a refusal. The CN is the field a certificate gets wrong
//     most often, and a CN the certificate does not corroborate is not
//     an identity.
//   - a common name that is already qualified is the node ID, since a
//     qualified CN is what this Colony uses as a raft identity.
//   - a bare short hostname becomes the ONE SAN that extends it into a
//     qualified name. Zero such SANs and two such SANs are both
//     refusals: a certificate that does not say which name the Colony
//     knows it by is not one this command will decide for it.
//
// The extension rule is deliberately the only place a name is
// constructed rather than read. It is a refusal-heavy rule, and it
// exists because the alternative - pinning a member under `brood` when
// the Colony knows it as `brood.lab3.home.arpa` - would write a
// trust anchor under a key nothing else in the system ever uses, and
// would do it silently, five times.
func deriveNodeID(cn string, info hostcert.Info) (string, error) {
	if cn == "" {
		return "", errors.New("has no common name, and its node_id is derived from it. Name the member explicitly with --node instead of guessing an identity a certificate does not state")
	}
	if !dnsSANContains(info.DNSNames, cn) {
		return "", fmt.Errorf("has common name %q, which is not among its own DNS subject alternative names (%s). A common name the certificate does not corroborate is not an identity", cn, info.SANs())
	}
	if strings.Contains(cn, ".") {
		return cn, nil
	}
	var qualified []string
	for _, name := range info.DNSNames {
		if strings.HasPrefix(name, cn+".") && strings.Contains(name[len(cn)+1:], ".") {
			qualified = append(qualified, name)
		}
	}
	switch len(qualified) {
	case 1:
		return qualified[0], nil
	case 0:
		return "", fmt.Errorf("has the short common name %q and no qualified subject alternative name, so the certificate does not say what this Colony calls the member. Name it explicitly with --node", cn)
	default:
		sort.Strings(qualified)
		return "", fmt.Errorf("has the short common name %q and %d qualified subject alternative names that extend it (%s), so which one is this Colony's node_id is a guess. Name it explicitly with --node", cn, len(qualified), strings.Join(qualified, ", "))
	}
}

func dnsSANContains(names []string, want string) bool {
	for _, n := range names {
		if strings.EqualFold(strings.TrimSpace(n), want) {
			return true
		}
	}
	return false
}

// repeatedFlag is a repeatable string flag. The standard library's
// flag package has no such type and the idiom is a slice behind a
// flag.Value; --node and --cert are paired by position afterwards, so
// each side has to keep its own order.
type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, ",") }

func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func unixTime(unix int64) string {
	if unix <= 0 {
		return "an unknown date"
	}
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04:05Z")
}
