package manager

import (
	"context"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
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
