package manager

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
)

// fakeJoinColonyPeerForwarder embeds the (nil) PeerForwarder interface
// so it only needs to implement the join-colony methods this file's
// tests actually exercise - mirrors fakeISOPeerForwarder's own
// established shape in iso_replication_test.go. Implements the
// *Unauthenticated variants (ADR-0096), since target_address forwarding
// now dials through those, never the authenticated originals.
type fakeJoinColonyPeerForwarder struct {
	PeerForwarder

	lastAddr string
	lastReq  *rpcpb.RequestJoinColonyRequest
	lastReqID,
	lastCancelReqID string
	lastCtx context.Context

	requestResp *rpcpb.RequestJoinColonyResponse
	statusResp  *rpcpb.GetJoinRequestStatusResponse
	cancelResp  *rpcpb.CancelJoinRequestResponse
}

func (f *fakeJoinColonyPeerForwarder) RequestJoinColonyUnauthenticated(ctx context.Context, addr string, req *rpcpb.RequestJoinColonyRequest) (*rpcpb.RequestJoinColonyResponse, error) {
	f.lastAddr, f.lastReq, f.lastCtx = addr, req, ctx
	if f.requestResp != nil {
		return f.requestResp, nil
	}
	return &rpcpb.RequestJoinColonyResponse{RequestId: "jreq-forwarded", Code: "123456"}, nil
}

func (f *fakeJoinColonyPeerForwarder) GetJoinRequestStatusUnauthenticated(ctx context.Context, addr, requestID string) (*rpcpb.GetJoinRequestStatusResponse, error) {
	f.lastAddr, f.lastReqID, f.lastCtx = addr, requestID, ctx
	if f.statusResp != nil {
		return f.statusResp, nil
	}
	return &rpcpb.GetJoinRequestStatusResponse{Request: &rpcpb.PendingJoinRequest{RequestId: requestID}}, nil
}

func (f *fakeJoinColonyPeerForwarder) CancelJoinRequestUnauthenticated(ctx context.Context, addr string, req *rpcpb.CancelJoinRequestRequest) (*rpcpb.CancelJoinRequestResponse, error) {
	f.lastAddr, f.lastCancelReqID, f.lastCtx = addr, req.GetRequestId(), ctx
	if f.cancelResp != nil {
		return f.cancelResp, nil
	}
	return &rpcpb.CancelJoinRequestResponse{Request: &rpcpb.PendingJoinRequest{RequestId: req.GetRequestId()}}, nil
}

// TestServer_RequestJoinColony_TargetAddressWithNilPeersErrors confirms
// ADR-0092's forwarding branch never silently falls through to the
// local raft-Apply path (which would recreate the exact ADR-0083
// confusion this fix closes) - a nil s.peers must produce a clear
// error instead. Uses a nil raft client, proving this path never
// touches raft at all.
func TestServer_RequestJoinColony_TargetAddressWithNilPeersErrors(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	resp, err := s.RequestJoinColony(context.Background(), &rpcpb.RequestJoinColonyRequest{
		NodeId: "node-2", RaftBindAddress: "10.0.0.2:17600", TargetAddress: "10.0.0.1:17700",
	})
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if !strings.Contains(resp.GetError(), "no peer forwarding is configured") {
		t.Errorf("Error = %q, want a clear no-peer-forwarding message", resp.GetError())
	}
}

// TestServer_RequestJoinColony_TargetAddressForwardsToPeer confirms a
// target_address is dialed directly via s.peers, with target_address
// itself stripped from the forwarded copy (it's routing information for
// THIS hop only, not part of the request the target should record).
func TestServer_RequestJoinColony_TargetAddressForwardsToPeer(t *testing.T) {
	peers := &fakeJoinColonyPeerForwarder{requestResp: &rpcpb.RequestJoinColonyResponse{RequestId: "jreq-1", Code: "482913"}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, peers, "", nil, nil, nil, 0, nil)
	resp, err := s.RequestJoinColony(context.Background(), &rpcpb.RequestJoinColonyRequest{
		NodeId: "node-2", RaftBindAddress: "10.0.0.2:17600", TargetAddress: "10.0.0.1:17700",
	})
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if resp.GetRequestId() != "jreq-1" || resp.GetCode() != "482913" {
		t.Errorf("response = %+v, want the fake peer's own response passed through verbatim", resp)
	}
	if peers.lastAddr != "10.0.0.1:17700" {
		t.Errorf("dialed addr = %q, want the named target_address", peers.lastAddr)
	}
	if peers.lastReq.GetNodeId() != "node-2" || peers.lastReq.GetRaftBindAddress() != "10.0.0.2:17600" {
		t.Errorf("forwarded request = %+v, want node-2/10.0.0.2:17600 preserved", peers.lastReq)
	}
	if peers.lastReq.GetTargetAddress() != "" {
		t.Errorf("forwarded request still carries TargetAddress = %q, want it stripped", peers.lastReq.GetTargetAddress())
	}
}

// TestServer_RequestJoinColony_TargetAddressRejectedWhenNotAllowlisted
// is ADR-0097's own regression test: once -known-peer-addresses is
// configured, a target_address outside that list must be refused
// before ever dialing - the fake peer's lastAddr staying empty proves
// the dial never happened, not just that an error was also returned.
func TestServer_RequestJoinColony_TargetAddressRejectedWhenNotAllowlisted(t *testing.T) {
	peers := &fakeJoinColonyPeerForwarder{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, peers, "", nil, nil, nil, 0, nil)
	s.SetKnownPeerAddresses([]string{"10.0.0.9:17700", "10.0.0.14:17700"})

	resp, err := s.RequestJoinColony(context.Background(), &rpcpb.RequestJoinColonyRequest{
		NodeId: "node-2", RaftBindAddress: "10.0.0.2:17600", TargetAddress: "10.0.0.1:17700",
	})
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if !strings.Contains(resp.GetError(), "known-peer-addresses") {
		t.Errorf("Error = %q, want a clear allowlist-rejection message", resp.GetError())
	}
	if peers.lastAddr != "" {
		t.Errorf("lastAddr = %q, want empty - the peer must never be dialed for a disallowed target", peers.lastAddr)
	}
}

// TestServer_RequestJoinColony_TargetAddressAllowedWhenAllowlisted
// confirms the allowlist is a floor, not a lockout: a target_address
// that IS listed still forwards normally.
func TestServer_RequestJoinColony_TargetAddressAllowedWhenAllowlisted(t *testing.T) {
	peers := &fakeJoinColonyPeerForwarder{requestResp: &rpcpb.RequestJoinColonyResponse{RequestId: "jreq-1", Code: "482913"}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, peers, "", nil, nil, nil, 0, nil)
	s.SetKnownPeerAddresses([]string{"10.0.0.1:17700"})

	resp, err := s.RequestJoinColony(context.Background(), &rpcpb.RequestJoinColonyRequest{
		NodeId: "node-2", RaftBindAddress: "10.0.0.2:17600", TargetAddress: "10.0.0.1:17700",
	})
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if resp.GetError() != "" || resp.GetRequestId() != "jreq-1" {
		t.Errorf("response = %+v, want the forwarded call to succeed", resp)
	}
	if peers.lastAddr != "10.0.0.1:17700" {
		t.Errorf("lastAddr = %q, want the allowlisted target dialed", peers.lastAddr)
	}
}

// TestServer_SetKnownPeerAddresses_EmptyClearsAllowlist confirms
// passing an empty/nil slice restores ADR-0092's original accept-any
// behavior, not an empty (deny-everything) allowlist - the difference
// between "unconfigured" and "configured but empty" matters here.
func TestServer_SetKnownPeerAddresses_EmptyClearsAllowlist(t *testing.T) {
	peers := &fakeJoinColonyPeerForwarder{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, peers, "", nil, nil, nil, 0, nil)
	s.SetKnownPeerAddresses([]string{"10.0.0.9:17700"})
	s.SetKnownPeerAddresses(nil)

	resp, err := s.RequestJoinColony(context.Background(), &rpcpb.RequestJoinColonyRequest{
		NodeId: "node-2", RaftBindAddress: "10.0.0.2:17600", TargetAddress: "10.0.0.1:17700",
	})
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Errorf("response = %+v, want an unconfigured allowlist to accept any target_address", resp)
	}
}

func TestServer_GetJoinRequestStatus_TargetAddressRejectedWhenNotAllowlisted(t *testing.T) {
	peers := &fakeJoinColonyPeerForwarder{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, peers, "", nil, nil, nil, 0, nil)
	s.SetKnownPeerAddresses([]string{"10.0.0.9:17700"})

	resp, err := s.GetJoinRequestStatus(context.Background(), &rpcpb.GetJoinRequestStatusRequest{
		RequestId: "jreq-1", TargetAddress: "10.0.0.1:17700",
	})
	if err != nil {
		t.Fatalf("GetJoinRequestStatus() error: %v", err)
	}
	if !strings.Contains(resp.GetError(), "known-peer-addresses") {
		t.Errorf("Error = %q, want a clear allowlist-rejection message", resp.GetError())
	}
	if peers.lastAddr != "" {
		t.Errorf("lastAddr = %q, want empty - the peer must never be dialed for a disallowed target", peers.lastAddr)
	}
}

func TestServer_CancelJoinRequest_TargetAddressRejectedWhenNotAllowlisted(t *testing.T) {
	peers := &fakeJoinColonyPeerForwarder{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, peers, "", nil, nil, nil, 0, nil)
	s.SetKnownPeerAddresses([]string{"10.0.0.9:17700"})

	resp, err := s.CancelJoinRequest(context.Background(), &rpcpb.CancelJoinRequestRequest{
		RequestId: "jreq-1", TargetAddress: "10.0.0.1:17700",
	})
	if err != nil {
		t.Fatalf("CancelJoinRequest() error: %v", err)
	}
	if !strings.Contains(resp.GetError(), "known-peer-addresses") {
		t.Errorf("Error = %q, want a clear allowlist-rejection message", resp.GetError())
	}
	if peers.lastAddr != "" {
		t.Errorf("lastAddr = %q, want empty - the peer must never be dialed for a disallowed target", peers.lastAddr)
	}
}

func TestServer_GetJoinRequestStatus_TargetAddressWithNilPeersErrors(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	resp, err := s.GetJoinRequestStatus(context.Background(), &rpcpb.GetJoinRequestStatusRequest{
		RequestId: "jreq-1", TargetAddress: "10.0.0.1:17700",
	})
	if err != nil {
		t.Fatalf("GetJoinRequestStatus() error: %v", err)
	}
	if !strings.Contains(resp.GetError(), "no peer forwarding is configured") {
		t.Errorf("Error = %q, want a clear no-peer-forwarding message", resp.GetError())
	}
}

func TestServer_GetJoinRequestStatus_TargetAddressForwardsToPeer(t *testing.T) {
	peers := &fakeJoinColonyPeerForwarder{statusResp: &rpcpb.GetJoinRequestStatusResponse{
		Request: &rpcpb.PendingJoinRequest{RequestId: "jreq-1", Status: rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING},
	}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, peers, "", nil, nil, nil, 0, nil)
	resp, err := s.GetJoinRequestStatus(context.Background(), &rpcpb.GetJoinRequestStatusRequest{
		RequestId: "jreq-1", TargetAddress: "10.0.0.1:17700",
	})
	if err != nil {
		t.Fatalf("GetJoinRequestStatus() error: %v", err)
	}
	if resp.GetRequest().GetRequestId() != "jreq-1" {
		t.Errorf("response = %+v, want the fake peer's own response passed through verbatim", resp)
	}
	if peers.lastAddr != "10.0.0.1:17700" || peers.lastReqID != "jreq-1" {
		t.Errorf("forwarded to addr=%q requestID=%q, want 10.0.0.1:17700/jreq-1", peers.lastAddr, peers.lastReqID)
	}
}

func TestServer_CancelJoinRequest_TargetAddressWithNilPeersErrors(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	resp, err := s.CancelJoinRequest(context.Background(), &rpcpb.CancelJoinRequestRequest{
		RequestId: "jreq-1", TargetAddress: "10.0.0.1:17700",
	})
	if err != nil {
		t.Fatalf("CancelJoinRequest() error: %v", err)
	}
	if !strings.Contains(resp.GetError(), "no peer forwarding is configured") {
		t.Errorf("Error = %q, want a clear no-peer-forwarding message", resp.GetError())
	}
}

func TestServer_CancelJoinRequest_TargetAddressForwardsToPeer(t *testing.T) {
	peers := &fakeJoinColonyPeerForwarder{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, peers, "", nil, nil, nil, 0, nil)
	resp, err := s.CancelJoinRequest(context.Background(), &rpcpb.CancelJoinRequestRequest{
		RequestId: "jreq-1", TargetAddress: "10.0.0.1:17700",
	})
	if err != nil {
		t.Fatalf("CancelJoinRequest() error: %v", err)
	}
	if resp.GetRequest().GetRequestId() != "jreq-1" {
		t.Errorf("response = %+v, want the fake peer's own response passed through verbatim", resp)
	}
	if peers.lastAddr != "10.0.0.1:17700" || peers.lastCancelReqID != "jreq-1" {
		t.Errorf("forwarded to addr=%q requestID=%q, want 10.0.0.1:17700/jreq-1", peers.lastAddr, peers.lastCancelReqID)
	}
}

// assertBoundedForwardDeadline confirms ctx carries a deadline no later
// than defaultUnauthenticatedForwardTimeout from now, regardless of
// whatever (or however unbounded) a deadline the original caller's own
// context had - see defaultUnauthenticatedForwardTimeout's own doc
// comment (2026-09-12 audit finding).
func assertBoundedForwardDeadline(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("forwarding context has no deadline, want one bounded by defaultUnauthenticatedForwardTimeout")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > defaultUnauthenticatedForwardTimeout {
		t.Errorf("forwarding context deadline is %v from now, want (0, %v]", remaining, defaultUnauthenticatedForwardTimeout)
	}
}

// TestServer_RequestJoinColony_TargetAddressForwardIsBounded is the
// 2026-09-12 audit's own regression test: a caller-supplied
// target_address that never completes a TCP handshake (a black-holed
// IP, a firewalled port) must not hang this handler indefinitely, even
// though RequestJoinColony's own incoming context (ctx.Background()
// here, standing in for a caller who set no deadline at all) carries
// none itself.
func TestServer_RequestJoinColony_TargetAddressForwardIsBounded(t *testing.T) {
	peers := &fakeJoinColonyPeerForwarder{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, peers, "", nil, nil, nil, 0, nil)
	if _, err := s.RequestJoinColony(context.Background(), &rpcpb.RequestJoinColonyRequest{
		NodeId: "node-2", RaftBindAddress: "10.0.0.2:17600", TargetAddress: "10.0.0.1:17700",
	}); err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	assertBoundedForwardDeadline(t, peers.lastCtx)
}

func TestServer_GetJoinRequestStatus_TargetAddressForwardIsBounded(t *testing.T) {
	peers := &fakeJoinColonyPeerForwarder{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, peers, "", nil, nil, nil, 0, nil)
	if _, err := s.GetJoinRequestStatus(context.Background(), &rpcpb.GetJoinRequestStatusRequest{
		RequestId: "jreq-1", TargetAddress: "10.0.0.1:17700",
	}); err != nil {
		t.Fatalf("GetJoinRequestStatus() error: %v", err)
	}
	assertBoundedForwardDeadline(t, peers.lastCtx)
}

func TestServer_CancelJoinRequest_TargetAddressForwardIsBounded(t *testing.T) {
	peers := &fakeJoinColonyPeerForwarder{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, peers, "", nil, nil, nil, 0, nil)
	if _, err := s.CancelJoinRequest(context.Background(), &rpcpb.CancelJoinRequestRequest{
		RequestId: "jreq-1", TargetAddress: "10.0.0.1:17700",
	}); err != nil {
		t.Fatalf("CancelJoinRequest() error: %v", err)
	}
	assertBoundedForwardDeadline(t, peers.lastCtx)
}

// writeTestCertPEM generates a throwaway self-signed certificate and
// writes it to a temp PEM file, returning its path - a real on-disk
// tls_cert this file's own localTLSCertFingerprint can load, without
// depending on any fixture checked into the repo.
func writeTestCertPEM(t *testing.T) (path string, der []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-comb.example"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating test certificate: %v", err)
	}
	f, err := os.CreateTemp(t.TempDir(), "test-cert-*.pem")
	if err != nil {
		t.Fatalf("creating temp cert file: %v", err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		t.Fatalf("writing test cert PEM: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing test cert file: %v", err)
	}
	return f.Name(), certDER
}

// TestServer_RequestJoinColony_FillsTLSFingerprintFromLocalTLSCert is
// ADR-0113's own regression test for the forwarding side of the
// feature: when target_address is set and the caller left
// tls_cert_fingerprint empty, RequestJoinColony must compute it itself
// from this managerd's own locally-configured tls_cert (never from
// anything the caller supplied unauthenticated - there is no other
// input here) and attach it to the forwarded copy, so the eventual
// Admin reviewing the pending request sees real identity information,
// not a value any unauthenticated caller could have spoofed.
func TestServer_RequestJoinColony_FillsTLSFingerprintFromLocalTLSCert(t *testing.T) {
	certPath, certDER := writeTestCertPEM(t)
	wantSum := sha256.Sum256(certDER)
	wantPairs := make([]string, len(wantSum))
	for i, b := range wantSum {
		wantPairs[i] = fmt.Sprintf("%02X", b)
	}
	want := "SHA256:" + strings.Join(wantPairs, ":")

	peers := &fakeJoinColonyPeerForwarder{requestResp: &rpcpb.RequestJoinColonyResponse{RequestId: "jreq-1", Code: "482913"}}
	nodeConfig := &fakeNodeConfigStore{cfg: nodeconfig.Config{TLSCert: certPath}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, peers, "", nil, nodeConfig, nil, 0, nil)

	resp, err := s.RequestJoinColony(context.Background(), &rpcpb.RequestJoinColonyRequest{
		NodeId: "node-2", RaftBindAddress: "10.0.0.2:17600", TargetAddress: "10.0.0.1:17700",
	})
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("RequestJoinColony() returned error: %s", resp.GetError())
	}
	if peers.lastReq.GetTlsCertFingerprint() != want {
		t.Errorf("forwarded tls_cert_fingerprint = %q, want %q", peers.lastReq.GetTlsCertFingerprint(), want)
	}
}

// TestServer_RequestJoinColony_NoLocalTLSCertMeansEmptyFingerprint
// confirms TLS remains opt-in (ADR-0087/ADR-0093): a joining Comb with
// no tls_cert configured forwards with an empty tls_cert_fingerprint,
// not an error and not a fabricated value - the eventual Admin then
// sees "no TLS certificate presented" instead.
func TestServer_RequestJoinColony_NoLocalTLSCertMeansEmptyFingerprint(t *testing.T) {
	peers := &fakeJoinColonyPeerForwarder{requestResp: &rpcpb.RequestJoinColonyResponse{RequestId: "jreq-1", Code: "482913"}}
	nodeConfig := &fakeNodeConfigStore{cfg: nodeconfig.Config{}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, peers, "", nil, nodeConfig, nil, 0, nil)

	if _, err := s.RequestJoinColony(context.Background(), &rpcpb.RequestJoinColonyRequest{
		NodeId: "node-2", RaftBindAddress: "10.0.0.2:17600", TargetAddress: "10.0.0.1:17700",
	}); err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if got := peers.lastReq.GetTlsCertFingerprint(); got != "" {
		t.Errorf("forwarded tls_cert_fingerprint = %q, want empty with no tls_cert configured", got)
	}
}

// TestServer_ApproveJoinRequest_WrongConfirmPhraseRejectedNoAction is
// the RPC-level guard's own regression test (ADR-0113), mirroring the
// established convention for ConvertStandaloneToJoiner's own
// confirm_phrase check: a wrong phrase must be rejected before this
// function does anything else at all, including its own leadership
// check. Passing a nil *RaftClient proves this directly - if the
// confirm_phrase check were not first, s.raft.Status(ctx) below it
// would be reached and panic on the nil receiver; this test failing to
// panic (and instead returning a clean error with no Request in the
// response) is the actual proof no action was taken.
func TestServer_ApproveJoinRequest_WrongConfirmPhraseRejectedNoAction(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	resp, err := s.ApproveJoinRequest(context.Background(), &rpcpb.ApproveJoinRequestRequest{
		RequestId: "jreq-1", ConfirmPhrase: "not-the-right-phrase",
	})
	if err != nil {
		t.Fatalf("ApproveJoinRequest() error: %v", err)
	}
	if !strings.Contains(resp.GetError(), "does not match the required confirmation phrase") {
		t.Errorf("Error = %q, want a clear confirm_phrase mismatch message", resp.GetError())
	}
	if resp.GetRequest() != nil {
		t.Errorf("Request = %+v, want nil - nothing should have been approved", resp.GetRequest())
	}
}

// TestServer_ApproveJoinRequest_MissingConfirmPhraseRejectedNoAction
// covers the "field left blank entirely" variant - an empty
// confirm_phrase must fail exactly like a wrong one.
func TestServer_ApproveJoinRequest_MissingConfirmPhraseRejectedNoAction(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	resp, err := s.ApproveJoinRequest(context.Background(), &rpcpb.ApproveJoinRequestRequest{RequestId: "jreq-1"})
	if err != nil {
		t.Fatalf("ApproveJoinRequest() error: %v", err)
	}
	if !strings.Contains(resp.GetError(), "does not match the required confirmation phrase") {
		t.Errorf("Error = %q, want a clear confirm_phrase mismatch message", resp.GetError())
	}
	if resp.GetRequest() != nil {
		t.Errorf("Request = %+v, want nil - nothing should have been approved", resp.GetRequest())
	}
}

// TestTLSCertFingerprint_FormatAndDeterminism is a plain unit test of
// the pure formatting helper, independent of any file I/O or Server
// state.
func TestTLSCertFingerprint_FormatAndDeterminism(t *testing.T) {
	der := []byte("not a real certificate, just some bytes to hash")
	sum := sha256.Sum256(der)
	wantPairs := make([]string, len(sum))
	for i, b := range sum {
		wantPairs[i] = fmt.Sprintf("%02X", b)
	}
	want := "SHA256:" + strings.Join(wantPairs, ":")

	got := tlsCertFingerprint(der)
	if got != want {
		t.Errorf("tlsCertFingerprint() = %q, want %q", got, want)
	}
	if got2 := tlsCertFingerprint(der); got2 != got {
		t.Errorf("tlsCertFingerprint() not deterministic: %q vs %q", got, got2)
	}
}
