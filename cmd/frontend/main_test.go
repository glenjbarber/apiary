package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// fakeStatusClient embeds the (nil) ManagerServiceClient interface so
// it satisfies the full interface without stubbing every method by
// hand - only Status, the one checkPAMConfigured actually calls, is
// overridden.
type fakeStatusClient struct {
	rpcpb.ManagerServiceClient

	// errsThenSucceeds, if set, is returned in order on the first N
	// calls (N = len(errsThenSucceeds)) before every call after that
	// succeeds - lets a test model managerd coming up after a few
	// failed attempts.
	errsThenSucceeds []error
	calls            int

	resp *rpcpb.StatusResponse
}

func (f *fakeStatusClient) Status(context.Context, *rpcpb.StatusRequest, ...grpc.CallOption) (*rpcpb.StatusResponse, error) {
	i := f.calls
	f.calls++
	if i < len(f.errsThenSucceeds) {
		return nil, f.errsThenSucceeds[i]
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &rpcpb.StatusResponse{}, nil
}

func TestCheckPAMConfigured_SucceedsImmediately(t *testing.T) {
	client := &fakeStatusClient{resp: &rpcpb.StatusResponse{PamConfigured: true}}

	configured, err := checkPAMConfigured(client, 5, time.Millisecond)
	if err != nil {
		t.Fatalf("checkPAMConfigured() error: %v", err)
	}
	if !configured {
		t.Error("configured = false, want true")
	}
	if client.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retries needed)", client.calls)
	}
}

// TestCheckPAMConfigured_RetriesThroughTransientErrors guards the exact
// live race this function exists to close: managerd briefly
// unreachable at frontend's own startup (e.g. still connecting to
// raftd's socket), then reachable moments later.
func TestCheckPAMConfigured_RetriesThroughTransientErrors(t *testing.T) {
	client := &fakeStatusClient{
		errsThenSucceeds: []error{errors.New("connection refused"), errors.New("connection refused")},
		resp:             &rpcpb.StatusResponse{PamConfigured: true},
	}

	configured, err := checkPAMConfigured(client, 5, time.Millisecond)
	if err != nil {
		t.Fatalf("checkPAMConfigured() error: %v", err)
	}
	if !configured {
		t.Error("configured = false, want true")
	}
	if client.calls != 3 {
		t.Errorf("calls = %d, want 3 (2 failures then a success)", client.calls)
	}
}

func TestCheckPAMConfigured_FallsBackAfterExhaustingAttempts(t *testing.T) {
	wantErr := errors.New("connection refused")
	client := &fakeStatusClient{errsThenSucceeds: []error{wantErr, wantErr, wantErr}}

	configured, err := checkPAMConfigured(client, 3, time.Millisecond)
	if err == nil {
		t.Fatal("checkPAMConfigured() error = nil, want the last attempt's error surfaced")
	}
	if configured {
		t.Error("configured = true, want false when every attempt failed")
	}
	if client.calls != 3 {
		t.Errorf("calls = %d, want exactly 3 (attempts), not more", client.calls)
	}
}

func TestCheckPAMConfigured_NotConfiguredIsNotAnError(t *testing.T) {
	client := &fakeStatusClient{resp: &rpcpb.StatusResponse{PamConfigured: false}}

	configured, err := checkPAMConfigured(client, 5, time.Millisecond)
	if err != nil {
		t.Fatalf("checkPAMConfigured() error: %v", err)
	}
	if configured {
		t.Error("configured = true, want false")
	}
}
