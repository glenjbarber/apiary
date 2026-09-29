package manager

import (
	"context"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestISOUploadLimiter_RefusesBeyondGlobalCap(t *testing.T) {
	l := newISOUploadLimiter()
	for i := 0; i < maxConcurrentISOUploads; i++ {
		caller := "10.0.0.1:" + string(rune('a'+i))
		if err := l.acquire(caller); err != nil {
			t.Fatalf("acquire() #%d error: %v, want it to succeed within the global cap", i, err)
		}
	}
	if err := l.acquire("10.0.0.99:1"); err == nil {
		t.Fatalf("acquire() = nil error, want refusal once the global cap (%d) is reached", maxConcurrentISOUploads)
	}
}

func TestISOUploadLimiter_RefusesBeyondPerCallerCap(t *testing.T) {
	l := newISOUploadLimiter()
	for i := 0; i < maxConcurrentISOUploadsPerCaller; i++ {
		if err := l.acquire("10.0.0.1:1"); err != nil {
			t.Fatalf("acquire() #%d error: %v, want it to succeed within the per-caller cap", i, err)
		}
	}
	if err := l.acquire("10.0.0.1:1"); err == nil {
		t.Fatalf("acquire() = nil error, want refusal once one caller's own cap (%d) is reached", maxConcurrentISOUploadsPerCaller)
	}
	// A different caller is unaffected by the first caller's own limit.
	if err := l.acquire("10.0.0.2:1"); err != nil {
		t.Errorf("acquire() for a different caller error: %v, want it unaffected by 10.0.0.1's cap", err)
	}
}

func TestISOUploadLimiter_ReleaseFreesASlot(t *testing.T) {
	l := newISOUploadLimiter()
	for i := 0; i < maxConcurrentISOUploadsPerCaller; i++ {
		if err := l.acquire("10.0.0.1:1"); err != nil {
			t.Fatalf("acquire() error: %v", err)
		}
	}
	l.release("10.0.0.1:1")
	if err := l.acquire("10.0.0.1:1"); err != nil {
		t.Errorf("acquire() after release error: %v, want the freed slot to be usable again", err)
	}
}

func TestISOUploadLimiter_ReleaseCleansUpEmptyCallerEntries(t *testing.T) {
	l := newISOUploadLimiter()
	if err := l.acquire("10.0.0.1:1"); err != nil {
		t.Fatalf("acquire() error: %v", err)
	}
	l.release("10.0.0.1:1")
	if _, ok := l.byPeer["10.0.0.1:1"]; ok {
		t.Errorf("byPeer retains an entry for a caller with zero active uploads, want it removed")
	}
}

func TestISOUploadCaller_FallsBackToUnknownWithNoPeerInfo(t *testing.T) {
	if got := isoUploadCaller(context.Background()); got != unknownISOUploadCaller {
		t.Errorf("isoUploadCaller() = %q, want %q for a context with no peer info", got, unknownISOUploadCaller)
	}
}

func TestServer_UploadISO_RefusesBeyondPerCallerCap(t *testing.T) {
	s := NewServer(nil, "node-1", &fakeISOManager{}, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	// Every fakeUploadStream in this test shares the same (context.Background)
	// peer identity - unknownISOUploadCaller - so this exercises the
	// per-caller cap exactly as multiple real uploads from one address would.
	for i := 0; i < maxConcurrentISOUploadsPerCaller; i++ {
		if err := s.isoUploads.acquire(unknownISOUploadCaller); err != nil {
			t.Fatalf("priming acquire() #%d error: %v", i, err)
		}
	}
	defer func() {
		for i := 0; i < maxConcurrentISOUploadsPerCaller; i++ {
			s.isoUploads.release(unknownISOUploadCaller)
		}
	}()

	stream := &fakeUploadStream{reqs: []*rpcpb.UploadISORequest{
		metadataMsg("test.iso", "deadbeef"),
		chunkMsg("data"),
	}}
	if err := s.UploadISO(stream); err == nil {
		t.Fatalf("UploadISO() = nil error, want refusal once the caller's concurrent-upload cap is already reached")
	} else if !strings.Contains(err.Error(), "too many concurrent ISO uploads") {
		t.Errorf("UploadISO() error = %q, want it to mention the concurrency limit", err)
	}
}

func TestServer_UploadISO_ReleasesSlotAfterCompletion(t *testing.T) {
	isos := &fakeISOManager{}
	s := NewServer(nil, "node-1", isos, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	stream := &fakeUploadStream{reqs: []*rpcpb.UploadISORequest{
		metadataMsg("test.iso", "deadbeef"),
		chunkMsg("data"),
	}}
	if err := s.UploadISO(stream); err != nil {
		t.Fatalf("UploadISO() error: %v", err)
	}

	s.isoUploads.mu.Lock()
	total, byPeer := s.isoUploads.total, len(s.isoUploads.byPeer)
	s.isoUploads.mu.Unlock()
	if total != 0 || byPeer != 0 {
		t.Errorf("isoUploads state after completion = total=%d byPeer entries=%d, want both 0", total, byPeer)
	}
}

func TestServer_UploadISO_ReleasesSlotEvenOnSaveError(t *testing.T) {
	isos := &fakeISOManager{saveErr: context.DeadlineExceeded}
	s := NewServer(nil, "node-1", isos, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	stream := &fakeUploadStream{reqs: []*rpcpb.UploadISORequest{
		metadataMsg("test.iso", "deadbeef"),
		chunkMsg("data"),
	}}
	if err := s.UploadISO(stream); err != nil {
		t.Fatalf("UploadISO() error: %v, want a response-level error instead", err)
	}

	s.isoUploads.mu.Lock()
	total := s.isoUploads.total
	s.isoUploads.mu.Unlock()
	if total != 0 {
		t.Errorf("isoUploads.total after a Save error = %d, want 0 (slot must still be released)", total)
	}
}
