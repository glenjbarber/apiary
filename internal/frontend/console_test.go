package frontend

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestServer_ConsolePage_AvailableShowsNoVNCWidget(t *testing.T) {
	client := &fakeClient{
		getVMConsoleResp: &rpcpb.GetVMConsoleResponse{Available: true, Host: "apiarium", Port: 5901},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/console", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "novnc/core/rfb.js") {
		t.Errorf("console page missing the noVNC module import, got: %s", body)
	}
	if strings.Contains(body, `class="error"`) {
		t.Errorf("console page shows an error banner for an available console, got: %s", body)
	}
}

func TestServer_ConsolePage_UnavailableShowsErrorNotWidget(t *testing.T) {
	client := &fakeClient{
		getVMConsoleResp: &rpcpb.GetVMConsoleResponse{Available: false},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/console", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "no running console") {
		t.Errorf("console page missing unavailable-console message, got: %s", body)
	}
	if strings.Contains(body, "novnc/core/rfb.js") {
		t.Errorf("console page should not attempt to connect when unavailable, got: %s", body)
	}
}

func TestServer_ConsolePage_RPCErrorShownAsError(t *testing.T) {
	client := &fakeClient{getVMConsoleErr: errors.New("managerd unreachable")}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/console", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "managerd unreachable") {
		t.Errorf("console page missing the RPC error, got: %s", rec.Body.String())
	}
}

// fakeVNCServer starts a plain TCP listener that echoes back whatever it
// receives - a stand-in for a real VNC framebuffer, exercising
// handleConsoleWS's proxy without any real bhyve/VNC involved.
func fakeEchoTCPServer(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	t.Cleanup(func() { lis.Close() })

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						if _, werr := conn.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return lis.Addr().String()
}

func TestServer_ConsoleWS_ProxiesBytesToAndFromVNCEndpoint(t *testing.T) {
	echoAddr := fakeEchoTCPServer(t)
	host, portStr, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("SplitHostPort() error: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing port: %v", err)
	}

	client := &fakeClient{
		getVMConsoleResp: &rpcpb.GetVMConsoleResponse{Available: true, Host: host, Port: uint32(port)},
	}
	s, err := NewServer(client, nil, nil, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/vms/vm-1/console/ws"
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("websocket.Dial() error: %v", err)
	}
	defer wsConn.Close()

	if err := wsConn.WriteMessage(websocket.BinaryMessage, []byte("hello vnc")); err != nil {
		t.Fatalf("WriteMessage() error: %v", err)
	}

	wsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, data, err := wsConn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage() error: %v", err)
	}
	if msgType != websocket.BinaryMessage || string(data) != "hello vnc" {
		t.Errorf("echoed message = (%d, %q), want (BinaryMessage, %q)", msgType, data, "hello vnc")
	}
}

// TestServer_ConsoleWS_RejectsCrossOriginUpgrade is the regression test
// for a 2026-09-06 security-audit finding: wsUpgrader.CheckOrigin used
// to accept every origin unconditionally, the classic cross-site
// WebSocket hijacking setup - an attacker's page could open a console
// socket using a victim's own browser session. checkConsoleOrigin now
// rejects a mismatched Origin header while still allowing same-origin
// requests and requests with no Origin header at all (e.g. a
// non-browser client).
func TestServer_ConsoleWS_RejectsCrossOriginUpgrade(t *testing.T) {
	echoAddr := fakeEchoTCPServer(t)
	host, portStr, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("SplitHostPort() error: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing port: %v", err)
	}

	client := &fakeClient{
		getVMConsoleResp: &rpcpb.GetVMConsoleResponse{Available: true, Host: host, Port: uint32(port)},
	}
	s, err := NewServer(client, nil, nil, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/vms/vm-1/console/ws"

	header := http.Header{}
	header.Set("Origin", "https://evil.example.com")
	if _, resp, err := websocket.DefaultDialer.Dial(wsURL, header); err == nil {
		t.Fatal("expected the cross-origin WebSocket handshake to be rejected")
	} else if resp == nil || resp.StatusCode != http.StatusForbidden {
		status := "<nil response>"
		if resp != nil {
			status = resp.Status
		}
		t.Errorf("expected HTTP 403 rejecting the handshake, got %s (err: %v)", status, err)
	}

	// A same-origin request (Origin matching the server's own host)
	// should still be allowed.
	header.Set("Origin", httpSrv.URL)
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("same-origin websocket.Dial() error: %v", err)
	}
	wsConn.Close()
}

func TestServer_ConsoleWS_UnavailableConsoleRejectsUpgrade(t *testing.T) {
	client := &fakeClient{getVMConsoleResp: &rpcpb.GetVMConsoleResponse{Available: false}}
	s, err := NewServer(client, nil, nil, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/vms/vm-1/console/ws"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatalf("Dial() succeeded, want a rejected upgrade for an unavailable console")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Errorf("status = %d, want %d", status, http.StatusServiceUnavailable)
	}
}
