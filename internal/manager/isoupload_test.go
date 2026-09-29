package manager

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/isostore"
	"google.golang.org/grpc/peer"
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

// peerAddrString is a net.Addr whose String() is not host:port, standing in
// for the unix-socket and other non-TCP peer addresses a real deployment
// can present.
type peerAddrString string

func (a peerAddrString) Network() string { return "test" }
func (a peerAddrString) String() string  { return string(a) }

func TestISOUploadCaller_KeysOnHostAndDropsThePort(t *testing.T) {
	peerCtx := func(addr net.Addr) context.Context {
		return peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
	}
	tcp := func(ip string, port int) *net.TCPAddr {
		return &net.TCPAddr{IP: net.ParseIP(ip), Port: port}
	}

	for _, tc := range []struct {
		name string
		addr net.Addr
		want string
	}{
		{"first ephemeral port from a host", tcp("10.0.0.1", 50001), "10.0.0.1"},
		{"a second port from the same host", tcp("10.0.0.1", 50002), "10.0.0.1"},
		{"a different host on the same port", tcp("10.0.0.2", 50001), "10.0.0.2"},
		{"IPv6 with a port", tcp("2001:db8::1", 50001), "2001:db8::1"},
		{"an address with no port to split", peerAddrString("/tmp/manager.sock"), "/tmp/manager.sock"},
	} {
		if got := isoUploadCaller(peerCtx(tc.addr)); got != tc.want {
			t.Errorf("%s: isoUploadCaller() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// gateISOManager holds every Save at a gate until the test opens it, so
// several uploads can be genuinely in flight at the same moment.
type gateISOManager struct {
	fakeISOManager
	gate    chan struct{}
	entered atomic.Int64

	saveMu sync.Mutex
}

func (g *gateISOManager) Save(name string, r io.Reader, expectedSHA256 string) (*isostore.Info, error) {
	g.entered.Add(1)
	<-g.gate
	// The embedded fake records into shared fields, so serialize it.
	g.saveMu.Lock()
	defer g.saveMu.Unlock()
	return g.fakeISOManager.Save(name, r, expectedSHA256)
}

// waitForUploadsInFlight blocks until n uploads have reached Save, or
// fails the test. Without this the test races the uploads it started.
func waitForUploadsInFlight(t *testing.T, g *gateISOManager, n int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for g.entered.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d uploads reached Save within 10s", g.entered.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// peerUploadStream is a fakeUploadStream that reports a real peer address,
// unlike the plain fake which has none.
type peerUploadStream struct {
	fakeUploadStream
	addr net.Addr
}

func (p *peerUploadStream) Context() context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{Addr: p.addr})
}

func isoStreamFrom(host string, port int, name string) *peerUploadStream {
	return &peerUploadStream{
		fakeUploadStream: fakeUploadStream{reqs: []*rpcpb.UploadISORequest{
			metadataMsg(name, "deadbeef"),
			chunkMsg("data"),
		}},
		addr: &net.TCPAddr{IP: net.ParseIP(host), Port: port},
	}
}

// TestServer_UploadISO_RefusesBeyondPerCallerCapFromOneHost is the
// regression test for the per-caller cap being unreachable. Keying the
// limiter on the full peer address gave every upload from one machine its
// own bucket, because each gets a fresh ephemeral source port, so the cap
// could never fire no matter how many streams a single caller opened.
func TestServer_UploadISO_RefusesBeyondPerCallerCapFromOneHost(t *testing.T) {
	isos := &gateISOManager{gate: make(chan struct{})}
	s := NewServer(nil, "node-1", isos, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	var wg sync.WaitGroup
	for i := 0; i < maxConcurrentISOUploadsPerCaller; i++ {
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			if err := s.UploadISO(isoStreamFrom("10.0.0.1", port, "test.iso")); err != nil {
				t.Errorf("in-flight UploadISO() from 10.0.0.1:%d error: %v, want it admitted", port, err)
			}
		}(50001 + i)
	}
	waitForUploadsInFlight(t, isos, int64(maxConcurrentISOUploadsPerCaller))

	// A third stream from the same host, on yet another fresh ephemeral
	// port, must be refused. It is driven from a goroutine because a
	// refusal comes back at once, while a wrongly-admitted upload would
	// sit at the gate until this test opened it.
	third := make(chan error, 1)
	go func() { third <- s.UploadISO(isoStreamFrom("10.0.0.1", 50999, "test.iso")) }()

	var thirdErr error
	timedOut := false
	select {
	case thirdErr = <-third:
	case <-time.After(10 * time.Second):
		timedOut = true
	}
	close(isos.gate)
	wg.Wait()

	if timedOut {
		t.Fatalf("UploadISO() from 10.0.0.1 beyond its own cap of %d did not refuse; it waited for the gate like any admitted upload",
			maxConcurrentISOUploadsPerCaller)
	}
	if thirdErr == nil {
		t.Fatalf("UploadISO() from 10.0.0.1 beyond its own cap of %d = nil error, want refusal",
			maxConcurrentISOUploadsPerCaller)
	}
	if !strings.Contains(thirdErr.Error(), "too many concurrent ISO uploads") {
		t.Errorf("UploadISO() error = %q, want it to mention the concurrency limit", thirdErr)
	}
	if !strings.Contains(thirdErr.Error(), "10.0.0.1") {
		t.Errorf("UploadISO() error = %q, want it to name the calling host", thirdErr)
	}
}

// A different host is not caught by another host's cap.
func TestServer_UploadISO_AdmitsADifferentHostWhileOneHostIsCapped(t *testing.T) {
	isos := &gateISOManager{gate: make(chan struct{})}
	s := NewServer(nil, "node-1", isos, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	var wg sync.WaitGroup
	for i := 0; i < maxConcurrentISOUploadsPerCaller; i++ {
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			if err := s.UploadISO(isoStreamFrom("10.0.0.1", port, "test.iso")); err != nil {
				t.Errorf("in-flight UploadISO() from 10.0.0.1:%d error: %v, want it admitted", port, err)
			}
		}(50001 + i)
	}
	waitForUploadsInFlight(t, isos, int64(maxConcurrentISOUploadsPerCaller))

	other := make(chan error, 1)
	go func() { other <- s.UploadISO(isoStreamFrom("10.0.0.2", 50001, "other.iso")) }()
	waitForUploadsInFlight(t, isos, int64(maxConcurrentISOUploadsPerCaller)+1)
	close(isos.gate)
	wg.Wait()

	if err := <-other; err != nil {
		t.Errorf("UploadISO() from a different host = %v, want it unaffected by 10.0.0.1's cap", err)
	}
}
