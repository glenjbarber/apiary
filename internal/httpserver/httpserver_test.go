package httpserver

import (
	"net"
	"net/http"
	"testing"
	"time"
)

func TestNew_SetsHeaderAndIdleTimeoutsButNotWholeRequestOnes(t *testing.T) {
	s := New("127.0.0.1:0", http.NotFoundHandler())
	if s.ReadHeaderTimeout != ReadHeaderTimeout || s.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want %v", s.ReadHeaderTimeout, ReadHeaderTimeout)
	}
	if s.IdleTimeout != IdleTimeout || s.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v, want %v", s.IdleTimeout, IdleTimeout)
	}
	if s.ReadTimeout != 0 || s.WriteTimeout != 0 {
		t.Errorf("ReadTimeout=%v WriteTimeout=%v, want both unset so uploads and streams are not cut off", s.ReadTimeout, s.WriteTimeout)
	}
}

// A client that connects and never sends headers must be dropped, not held.
func TestNew_DropsAConnectionThatNeverSendsHeaders(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := New("", http.NotFoundHandler())
	s.ReadHeaderTimeout = 200 * time.Millisecond // shortened for the test only
	go s.Serve(lis)
	defer s.Close()

	conn, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")); err != nil { // headers never finished
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		// The server may answer 408 before closing; either is a drop.
		return
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("connection was still open after the header timeout; a stalled client is being held")
	}
}
