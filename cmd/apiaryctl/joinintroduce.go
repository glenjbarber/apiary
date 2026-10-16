// apiaryctl join-introduce is ADR-0147 Part 2's JOINER side.
//
// THE JOINER IS A HEADLESS SERVER. It has no browser and never will, so
// the ADR's own four-step "Requesting Comb's own Machine page" does not
// exist and this command is what is on that side of the handshake
// instead. It PRINTS; it never submits.
//
// That non-submission is the property the whole design protects, and it
// is why this is a command rather than a page. The target's operator has
// to READ the first code and the fingerprints off the joiner's own
// screen and TYPE them into the target's own page. A surface that
// submitted them on the operator's behalf would satisfy stage one by
// itself, which is exactly the attestation stage one demands from a
// human. So there is no code path in this file that calls
// VerifyJoinIntroduction, ApproveJoinRequest, or any other
// state-changing RPC on the target - only RequestJoinColony (which
// creates the request the operator is about to argue about) and
// GetJoinRequestStatus (which reads it).
//
// --field exists for the same reason. An operator is on an SSH session
// on the joiner and will copy these values into a browser on the
// target, and a decorated block of text does not paste. One bare value
// per line, no labels, no quotes, no trailing commentary, and a
// fingerprint mismatch is a refusal rather than a warning, so the value
// has to be EXACT.

package main

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/localraft"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
	"github.com/glenjbarber/apiary/internal/tlsdial"
)

// joinFirstCodeDigits and joinSecondPinDigits match
// internal/manager's own constants. The first code is the joiner's,
// six digits, because its strength comes from a human transcribing it
// and a longer string makes the transcription worse. It is generated
// HERE, on the joiner, before anything is dialed - that inversion is
// what makes the handshake work, because the target then checks a value
// the requester invented rather than one both sides read out of the
// same server.
const (
	joinFirstCodeDigits = 6
	joinCodeModulus     = 1000000
)

// generateJoinFirstCode is 6 digits from crypto/rand, zero-padded.
//
// crypto/rand rather than math/rand even though the value is not a
// secret in the strict sense: a predictable sequence is the kind of
// thing an operator glances at and reads as meaningful, and there is no
// cost to not being that.
func generateJoinFirstCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(joinCodeModulus))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", joinFirstCodeDigits, n.Int64()), nil
}

// localJoinIdentity is this Comb's OWN raft identity, as its own
// configuration states it, never as a caller claims it.
//
// The defaults come from raftd.json rather than from managerd.json,
// because node_id and raft_bind are raftd's own settings and the values
// AddVoter will later be handed are exactly these. An operator can
// override either on the command line, which is what a Comb being
// re-imaged with a corrected address needs - but the default is this
// Comb's own file, so the common case types nothing.
type joinIdentity struct {
	nodeID   string
	raftBind string
}

func localJoinIdentity(nodeID, raftBind string) (joinIdentity, error) {
	id := joinIdentity{nodeID: nodeID, raftBind: raftBind}
	cfg, err := (&raftdconfig.Manager{}).Load()
	if err != nil {
		return id, fmt.Errorf("reading this Comb's own raftd configuration: %w - pass --node-id and --raft-bind explicitly if this Comb has no raftd.json", err)
	}
	if id.nodeID == "" {
		id.nodeID = strings.TrimSpace(cfg.NodeID)
	}
	if id.raftBind == "" {
		id.raftBind = strings.TrimSpace(cfg.RaftBind)
	}
	if id.nodeID == "" {
		return id, fmt.Errorf("this Comb's own configuration names no node_id, and it is the identity AddVoter will be given. Pass --node-id.")
	}
	if id.raftBind == "" {
		return id, fmt.Errorf("this Comb's own configuration names no raft_bind, and it is the address the Colony's leader will dial. Pass --raft-bind.")
	}
	return id, nil
}

// localCertificate is one certificate this Comb actually holds, read
// from the certificate FILE.
//
// The config strings are only used to find the files, and that
// distinction is load-bearing: a "fingerprint" of the path
// /usr/local/etc/apiary/tls.crt is a fingerprint of nothing, and the
// target's operator would be asked to compare the joiner's screen
// against a path.
type localCertificate struct {
	label       string
	path        string
	fingerprint string
	pem         string
}

// readLocalCertificate loads one certificate by path and derives its
// fingerprint through hostcert.Fingerprint - the one place that format
// is defined. The target compares the value this prints against the
// value it received on the wire, so the two have to be the same string,
// and they are the same string because there is only one function.
func readLocalCertificate(label, path string) (*localCertificate, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%s is not set, so this Comb holds no certificate to advertise", label)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s is set to %q, which cannot be read: %w", label, path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s is set to %q, which does not contain a PEM certificate", label, path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s is set to %q, whose certificate does not parse: %w", label, path, err)
	}
	return &localCertificate{
		label:       label,
		path:        path,
		fingerprint: hostcert.Fingerprint(cert.Raw),
		pem:         string(pem.EncodeToMemory(block)),
	}, nil
}

// localCertificates reads this Comb's own managerd serving certificate
// and, when Raft transport TLS is configured, its raftd certificate.
//
// tls_cert is required. A joiner with no serving certificate advertises
// no fingerprints, and a request carrying no fingerprints cannot be
// approved at all - the target would have nothing to compare, which is
// the entire fail-closed point. Saying that here, before a request is
// recorded, is much better than the target refusing it later.
func localCertificates(configPath string) ([]*localCertificate, error) {
	cfg, err := (&nodeconfig.Manager{Path: configPath}).Load()
	if err != nil {
		return nil, fmt.Errorf("reading this Comb's own configuration: %w - run `/usr/local/libexec/apiary/apiaryctl install --apply` first", err)
	}
	serving, err := readLocalCertificate("tls_cert", cfg.TLSCert)
	if err != nil {
		return nil, err
	}
	certs := []*localCertificate{serving}

	raftCfg, rerr := (&raftdconfig.Manager{}).Load()
	if rerr != nil || strings.TrimSpace(raftCfg.RaftTLSCert) == "" {
		// Not an error. A Comb that has not turned on Raft transport TLS
		// genuinely holds one certificate and advertises one, and
		// refusing here would break the ordinary pre-ADR-0147 case.
		return certs, nil
	}
	raftCert, err := readLocalCertificate("raft_tls_cert", raftCfg.RaftTLSCert)
	if err != nil {
		return nil, err
	}
	return append(certs, raftCert), nil
}

// runJoinIntroduce is the whole command. Two modes, one dial each:
//
//	--request-id set   POLL this Comb's existing request for the second
//	                   PIN. Read-only. Requires --first-code.
//	--request-id unset START a new request: generate the first code
//	                   locally, read this Comb's certificates, dial
//	                   --target, and print what to paste.
func runJoinIntroduce(args []string) int {
	fs := flag.NewFlagSet("apiaryctl join-introduce", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		target             string
		requestID          string
		firstCode          string
		field              string
		nodeID             string
		raftBind           string
		nodeConfigArg      string
		plaintext          bool
		caFile             string
		installCredentials bool
	)
	fs.StringVar(&target, "target", "", "host:port of ANY member of the Colony being joined, not only its leader (required to start a request)")
	fs.StringVar(&requestID, "request-id", "", "poll an existing request for its second PIN instead of starting a new one")
	fs.StringVar(&firstCode, "first-code", "", "the first code THIS Comb generated, required with --request-id")
	fs.StringVar(&field, "field", "", "print ONE bare value and nothing else, for pasting: first-code, fingerprints, request-id, target, second-pin")
	fs.StringVar(&nodeID, "node-id", "", "this Comb's raft identity; defaults to its own configured node_id")
	fs.StringVar(&raftBind, "raft-bind", "", "this Comb's raft bind address; defaults to its own configured raft bind")
	fs.StringVar(&nodeConfigArg, "node-config", "", "path to managerd.json; defaults to "+nodeconfig.DefaultPath)
	fs.BoolVar(&plaintext, "plaintext", false, "dial --target without TLS, for a Colony whose members do not serve TLS; the default dials with TLS, trusting the system CA pool")
	fs.StringVar(&caFile, "ca-file", "", "trust this CA file instead of the system pool when dialing --target with TLS; ignored with --plaintext")
	fs.BoolVar(&installCredentials, "install-credentials", false, "after approval, decrypt and install this Comb's per-node API key and Colony restart-guardrail token (root only; TLS required)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: /usr/local/libexec/apiary/apiaryctl join-introduce --target HOST:PORT [--field FIELD]
       /usr/local/libexec/apiary/apiaryctl join-introduce --request-id ID --first-code CODE [--target HOST:PORT] [--field FIELD]

Start or poll a two-way Colony join (ADR-0147 Part 2). This command PRINTS
and never submits anything to the target on your behalf: the target's Admin
has to read these values and type them into the target's own page, and
that transcription is the check.

  --target HOST:PORT   any member of the Colony, not only its leader
  --field FIELD        print ONE bare value, one per line, no labels:
                         first-code     the 6 digits this Comb generated
                         fingerprints   this Comb's certificate digests
                         request-id     needed to poll for the second PIN
                         second-pin     the target's 8 digits, when it has one
                         target         the --target value, to paste
  --request-id ID      poll that request instead of starting a new one
  --first-code CODE    the first code, required with --request-id
  --install-credentials after approval, install credentials on THIS Comb
  --plaintext          dial --target without TLS (only for a Colony that
                        does not serve TLS); default dials with TLS
  --ca-file FILE       trust this CA instead of the system pool; ignored
                        with --plaintext

The fingerprints are read from this Comb's own certificate FILES, not
from the config strings naming them. A fingerprint mismatch is a refusal,
so --field fingerprints prints digests and nothing else.
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	switch field {
	case "", "first-code", "fingerprints", "request-id", "target", "second-pin":
	default:
		fmt.Fprintf(os.Stderr, "apiaryctl join-introduce: --field must be one of first-code, fingerprints, request-id, target, second-pin\n")
		return 2
	}

	if requestID != "" {
		if installCredentials && field != "" {
			fmt.Fprintln(os.Stderr, "apiaryctl join-introduce: --install-credentials cannot be combined with --field")
			return 2
		}
		return pollJoinIntroduction(target, requestID, firstCode, field, plaintext, caFile, nodeConfigArg, installCredentials)
	}
	return startJoinIntroduction(target, field, nodeID, raftBind, nodeConfigArg, plaintext, caFile)
}

// buildJoinRequest assembles the request this Comb sends to the target.
// The command has already dialed that target directly, so it leaves
// target_address empty. Setting it would ask the receiving managerd to
// make a second unauthenticated dial to a caller-supplied address.
//
// This command dials the TARGET's managerd directly. It includes every
// value that only the joiner can know and leaves target_address empty,
// so the receiving managerd records this request locally or uses its
// Raft leader-forwarding path instead of making a caller-directed dial.
// A test pins this request shape.
//
// fingerprints is localCertificates' list, serving certificate first, and
// the serving certificate is the one tls_cert_fingerprint has always
// meant (ADR-0113). The caller guarantees it is non-empty, since
// localCertificates refuses a Comb with no tls_cert.
func buildJoinRequest(identity joinIdentity, firstCode string, fingerprints []string, lastLogIndex uint64) *rpcpb.RequestJoinColonyRequest {
	req := &rpcpb.RequestJoinColonyRequest{
		NodeId:                 identity.nodeID,
		RaftBindAddress:        identity.raftBind,
		IntroductionCode:       firstCode,
		AdvertisedFingerprints: fingerprints,
		JoinerLogStateObserved: true,
		JoinerLastLogIndex:     lastLogIndex,
	}
	if len(fingerprints) > 0 {
		req.TlsCertFingerprint = fingerprints[0]
	}
	return req
}

// startJoinIntroduction generates the first code, reads this Comb's own
// certificates, and records the request on the named member.
func startJoinIntroduction(target, field, nodeID, raftBind, nodeConfigArg string, plaintext bool, caFile string) int {
	if target == "" {
		fmt.Fprintln(os.Stderr, "apiaryctl join-introduce: --target is required. Name ANY member of the Colony you are joining - the request is replicated to all of them, and this command does not need the leader.")
		return 2
	}
	identity, err := localJoinIdentity(nodeID, raftBind)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apiaryctl join-introduce: %v\n", err)
		return 1
	}
	certs, err := localCertificates(nodeConfigArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apiaryctl join-introduce: %v\n", err)
		return 1
	}
	fingerprints := make([]string, 0, len(certs))
	for _, c := range certs {
		fingerprints = append(fingerprints, c.fingerprint)
	}
	// Generated BEFORE the dial, and never shown to the target. The
	// target's operator has to transcribe it off this Comb's own screen
	// into the target's own page; if it were generated after the dial,
	// or sent with the request, the check would be checking the
	// request against itself.
	firstCode, err := generateJoinFirstCode()
	if err != nil {
		fmt.Fprintf(os.Stderr, "apiaryctl join-introduce: generating the first code: %v\n", err)
		return 1
	}

	// THIS Comb's own raft log state, read from its own raftd, and sent
	// with the request.
	//
	// The joining Comb's own raft log state, read locally and sent with the
	// request. If it cannot be read, the command refuses rather than leaving
	// the target to measure its own unrelated log state.
	lctx, lcancel := context.WithTimeout(context.Background(), localraft.DefaultTimeout)
	local, err := localraft.Query(lctx, "")
	lcancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, `apiaryctl join-introduce: could not read this Comb's own raft log state from its local raftd: %v

The Colony needs to know that THIS Comb's log is empty before it will add it
as a voter, and only this Comb's own raftd can say so. Check that
apiary_raftd is running and that raftd.json names the right socket.
`, err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, closeFn, err := dialTargetManager(ctx, target, plaintext, caFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apiaryctl join-introduce: %v\n", err)
		return 1
	}
	defer closeFn()

	// The unauthenticated dial, deliberately: this Comb has no Colony
	// API key yet, by definition, and attaching a local credential to a
	// target an operator named would send it to whatever address the
	// operator typed.
	resp, err := client.RequestJoinColony(ctx, buildJoinRequest(identity, firstCode, fingerprints, local.LastLogIndex))
	if err != nil {
		fmt.Fprintf(os.Stderr, "apiaryctl join-introduce: %v\n", translateDialErr(target, plaintext, err))
		return 1
	}
	if resp.GetError() != "" {
		fmt.Fprintf(os.Stderr, "apiaryctl join-introduce: %s\n", resp.GetError())
		return 1
	}

	// --field mode, before anything else is printed, so a pasting
	// session gets exactly one value and no prose.
	if field != "" {
		return printField(field, map[string][]string{
			"first-code":   {firstCode},
			"fingerprints": fingerprints,
			"request-id":   {resp.GetRequestId()},
			"target":       {target},
			"second-pin":   nil,
		})
	}

	fmt.Printf("Join request recorded on %s.\n\n", target)
	fmt.Printf("  request id   %s\n", resp.GetRequestId())
	fmt.Printf("  first code   %s\n", firstCode)
	fmt.Printf("  node id      %s\n", identity.nodeID)
	fmt.Printf("  raft bind    %s\n\n", identity.raftBind)
	fmt.Println("Certificates this Comb holds, read from the certificate files themselves:")
	for _, c := range certs {
		fmt.Printf("  %-14s %s\n", c.label, c.fingerprint)
	}
	fmt.Printf("\nNow open the Colony being joined on ANY of its own members, find this\nrequest in the pending-requests panel, and paste the first code and every\nfingerprint above into its first-code form. It compares both against what\nthis request carried; a mismatch is a refusal.\n\n")
	fmt.Println("This command will not submit any of that for you. The transcription is")
	fmt.Println("the check.")
	fmt.Printf("\nWhen the target's Admin has verified it, poll for the second PIN:\n\n")
	fmt.Printf("  /usr/local/libexec/apiary/apiaryctl join-introduce --request-id %s --first-code %s --target %s --install-credentials\n", resp.GetRequestId(), firstCode, target)
	return 0
}

// buildJoinStatusRequest assembles the poll for the second PIN.
//
// It deliberately leaves target_address UNSET. This command already dials
// the target's managerd directly, so the target should answer from its own
// replicated state. A request that names a target_address is instead
// forwarded onward by the receiving managerd, and that forward carries
// only the request_id: the first code is dropped. Since this command dials
// the target itself, the forward lands back on the same managerd, which
// then sees an empty first code, refuses to release the second PIN, and
// returns a response with no PIN and no error. The poll looked like
// "no PIN yet" for a request that was already in CODE_VERIFIED.
func buildJoinStatusRequest(requestID, firstCode string, certificatePEM ...string) *rpcpb.GetJoinRequestStatusRequest {
	req := &rpcpb.GetJoinRequestStatusRequest{
		RequestId:        requestID,
		IntroductionCode: firstCode,
	}
	if len(certificatePEM) > 0 {
		req.RecipientTlsCertificatePem = certificatePEM[0]
	}
	return req
}

// pollJoinIntroduction reads this Comb's own request back from the
// target and prints the second PIN when the target has released one.
//
// Read-only, and it sends the first code back on every poll. That is
// the point: the poll needs both values, so an attacker who can reach
// the port and guess a 64-bit request_id still cannot release the PIN
// without a six-digit code the requester generated.
//
// It never re-prints the first code, because it is not stored on the
// target after acceptance and the operator already has it - this
// command cannot recover it, and saying so is more useful than printing
// an empty field.
func pollJoinIntroduction(target, requestID, firstCode, field string, plaintext bool, caFile, nodeConfigPath string, installCredentials bool) int {
	if firstCode == "" {
		fmt.Fprintln(os.Stderr, "apiaryctl join-introduce: --first-code is required with --request-id. It is the 6-digit value `/usr/local/libexec/apiary/apiaryctl join-introduce` printed when the request was made; the target needs it to release the second PIN.")
		return 2
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "apiaryctl join-introduce: --target is required with --request-id, and it must be the same member the request was recorded on.")
		return 2
	}
	certificates, err := localCertificates(nodeConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apiaryctl join-introduce: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, closeFn, err := dialTargetManager(ctx, target, plaintext, caFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apiaryctl join-introduce: %v\n", err)
		return 1
	}
	defer closeFn()
	resp, err := client.GetJoinRequestStatus(ctx, buildJoinStatusRequest(requestID, firstCode, certificates[0].pem))
	if err != nil {
		fmt.Fprintf(os.Stderr, "apiaryctl join-introduce: %v\n", translateDialErr(target, plaintext, err))
		return 1
	}
	if field != "" {
		if resp.GetError() != "" {
			fmt.Fprintf(os.Stderr, "apiaryctl join-introduce: %s\n", resp.GetError())
			return 1
		}
		if resp.GetSecondPin() == "" {
			// No PIN yet is not a failure: the target's Admin may simply
			// not have verified the introduction. Print nothing and exit
			// zero, so a shell loop polling this does not treat "not
			// yet" as a failure worth alerting on.
			return 0
		}
		return printField("second-pin", map[string][]string{"second-pin": {resp.GetSecondPin()}})
	}
	if resp.GetError() != "" {
		fmt.Fprintf(os.Stderr, "apiaryctl join-introduce: %s\n", resp.GetError())
		return 1
	}
	req := resp.GetRequest()
	if installCredentials && req.GetStatus() == rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED {
		if err := installApprovedJoinCredentials(resp.GetSealedCredentials(), certificates[0], requestID, nodeConfigPath); err != nil {
			fmt.Fprintf(os.Stderr, "apiaryctl join-introduce: %v\n", err)
			return 1
		}
		fmt.Println("join credentials were installed locally. managerd and frontend were restarted.")
		return 0
	}
	fmt.Printf("request %s\n", requestID)
	fmt.Printf("  status       %s\n", req.GetStatus())
	fmt.Printf("  stage        %s\n", req.GetStage())
	if req.GetExpiresAtUnix() > 0 {
		fmt.Printf("  request ends %s\n", time.Unix(req.GetExpiresAtUnix(), 0).Local().Format("2006-01-02 15:04 MST"))
	}
	switch {
	case resp.GetSecondPin() != "":
		fmt.Printf("\n  second PIN   %s\n", resp.GetSecondPin())
		if resp.GetSecondPinExpiresAtUnix() > 0 {
			fmt.Printf("  expires      %s\n", time.Unix(resp.GetSecondPinExpiresAtUnix(), 0).Local().Format("2006-01-02 15:04 MST"))
		}
		fmt.Printf("\nTake that to the Colony being joined and paste it into the second-PIN\nform on the same pending request. It expires in five minutes from the moment\nthe target generated it.\n")
	case req.GetStage() == rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED:
		fmt.Printf("\n  no second PIN yet. The target's Admin has not verified the first code\n  and fingerprints yet. Keep polling; nothing has failed.\n")
	case req.GetStatus() == rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED:
		fmt.Printf("\n  this request has been approved. Run this command with --install-credentials as root to install the encrypted credentials on this Comb.\n")
	case req.GetStatus() == rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_FAILED:
		fmt.Printf("\n  this request is FAILED. It used all its attempts at a stage. The\n  target's Admin has to purge it and this Comb has to request again.\n")
	}
	return 0
}

// printField emits one bare value, one per line, and nothing else.
//
// No labels, no quotes, no trailing explanation, exit 0 - because the
// output goes straight into a terminal buffer and then into a browser
// form on another machine, and every decoration is a chance for the
// operator to paste the wrong thing. A fingerprint mismatch is a
// refusal, so the value has to be byte-exact.
func printField(field string, values map[string][]string) int {
	for _, v := range values[field] {
		fmt.Println(v)
	}
	return 0
}

// dialTargetManager opens the UNAUTHENTICATED connection this joiner
// uses.
//
// No API key, deliberately, and this is the same reasoning internal/
// manager's PeerReporter.RequestJoinColonyUnauthenticated gives: this
// Comb has no Colony credential yet, by definition, and attaching one
// to a target address the operator typed would send a credential to
// whatever that address happens to be.
//
// TLS by default (trusting the system CA pool, like
// tlsdial.ManagerTLSConfig already does for the frontend and restshimd),
// because pam_service being set makes managerd serve TLS and a plaintext
// dial against it fails during the handshake rather than at the RPC
// layer - see translateDialErr. --plaintext is opt-in, for a Colony whose
// members genuinely serve no TLS. serverName is left empty: --target is
// the address actually dialed, and the default grpc-go behavior of
// verifying the certificate against it is exactly right here, unlike
// managerd's own loopback-only case that tlsdial.ManagerDialOption's doc
// comment describes.
//
// Not routed through internal/manager for the reason
// listPendingJoinRequests states: apiaryctl is a small root shell tool
// and importing the manager package would drag raft, bbolt and the
// cluster packages into a binary an operator runs during an incident.
func dialTargetManager(ctx context.Context, target string, plaintext bool, caFile string) (rpcpb.ManagerServiceClient, func(), error) {
	dialOpt, err := tlsdial.ManagerDialOption(!plaintext, caFile, "")
	if err != nil {
		return nil, nil, fmt.Errorf("configuring TLS for %s: %w", target, err)
	}
	conn, err := grpc.NewClient(target, dialOpt)
	if err != nil {
		return nil, nil, fmt.Errorf("dialing %s: %w", target, err)
	}
	return rpcpb.NewManagerServiceClient(conn), func() { conn.Close() }, nil
}

// translateDialErrPrefaceHint is the substring grpc-go's client surfaces
// when a plaintext client speaks to a TLS-only server: the server's TLS
// handshake bytes do not look like an HTTP/2 preface to a client that
// never offered TLS, so it reads as a bare EOF instead of a clear
// rejection. Matched as a substring, not parsed, because this is
// grpc-go's own wrapped error text, not a typed error this package
// controls.
const translateDialErrPrefaceHint = "error reading server preface"

// translateDialErr rewrites that specific EOF into the sentence an
// operator actually needs: this Comb dialed in plaintext and the target
// is TLS-only. Every other error is returned unchanged - this is a
// translation for one known failure shape, not a catch-all.
func translateDialErr(target string, plaintext bool, err error) error {
	if err == nil || !plaintext {
		return err
	}
	if !strings.Contains(err.Error(), translateDialErrPrefaceHint) {
		return err
	}
	return fmt.Errorf("reaching %s: the connection closed during the handshake - this looks like a plaintext client dialing a TLS-only server. Drop --plaintext and retry (the default dials with TLS): %w", target, err)
}
