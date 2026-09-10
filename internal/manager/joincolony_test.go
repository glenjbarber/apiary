package manager

import (
	"context"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// fakeJoinColonyPeerForwarder embeds the (nil) PeerForwarder interface
// so it only needs to implement the join-colony methods this file's
// tests actually exercise - mirrors fakeISOPeerForwarder's own
// established shape in iso_replication_test.go.
type fakeJoinColonyPeerForwarder struct {
	PeerForwarder

	lastAddr string
	lastReq  *rpcpb.RequestJoinColonyRequest
	lastReqID,
	lastCancelReqID string

	requestResp *rpcpb.RequestJoinColonyResponse
	statusResp  *rpcpb.GetJoinRequestStatusResponse
	cancelResp  *rpcpb.CancelJoinRequestResponse
}

func (f *fakeJoinColonyPeerForwarder) RequestJoinColony(_ context.Context, addr string, req *rpcpb.RequestJoinColonyRequest) (*rpcpb.RequestJoinColonyResponse, error) {
	f.lastAddr, f.lastReq = addr, req
	if f.requestResp != nil {
		return f.requestResp, nil
	}
	return &rpcpb.RequestJoinColonyResponse{RequestId: "jreq-forwarded", Code: "123456"}, nil
}

func (f *fakeJoinColonyPeerForwarder) GetJoinRequestStatus(_ context.Context, addr, requestID string) (*rpcpb.GetJoinRequestStatusResponse, error) {
	f.lastAddr, f.lastReqID = addr, requestID
	if f.statusResp != nil {
		return f.statusResp, nil
	}
	return &rpcpb.GetJoinRequestStatusResponse{Request: &rpcpb.PendingJoinRequest{RequestId: requestID}}, nil
}

func (f *fakeJoinColonyPeerForwarder) CancelJoinRequest(_ context.Context, addr string, req *rpcpb.CancelJoinRequestRequest) (*rpcpb.CancelJoinRequestResponse, error) {
	f.lastAddr, f.lastCancelReqID = addr, req.GetRequestId()
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
