package frontend

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/manager"
)

func TestConsoleTunnelLimiter_RefusesBeyondGlobalCap(t *testing.T) {
	l := newConsoleTunnelLimiter()
	for i := 0; i < maxConcurrentConsoleTunnels; i++ {
		caller := "10.0.0." + strconv.Itoa(i)
		if err := l.acquire(caller); err != nil {
			t.Fatalf("acquire() #%d error: %v, want it to succeed within the global cap", i, err)
		}
	}
	if err := l.acquire("10.0.0.99"); err == nil {
		t.Fatalf("acquire() = nil error, want refusal once the global cap (%d) is reached", maxConcurrentConsoleTunnels)
	}
}

func TestConsoleTunnelLimiter_RefusesBeyondPerCallerCap(t *testing.T) {
	l := newConsoleTunnelLimiter()
	for i := 0; i < maxConcurrentConsoleTunnelsPerCaller; i++ {
		if err := l.acquire("10.0.0.1"); err != nil {
			t.Fatalf("acquire() #%d error: %v, want it to succeed within the per-caller cap", i, err)
		}
	}
	if err := l.acquire("10.0.0.1"); err == nil {
		t.Fatalf("acquire() = nil error, want refusal once one caller's own cap (%d) is reached", maxConcurrentConsoleTunnelsPerCaller)
	}
	if err := l.acquire("10.0.0.2"); err != nil {
		t.Errorf("acquire() for a different caller error: %v, want it unaffected by 10.0.0.1's cap", err)
	}
}

func TestConsoleTunnelLimiter_ReleaseFreesASlotAndCleansUpEmptyEntries(t *testing.T) {
	l := newConsoleTunnelLimiter()
	if err := l.acquire("10.0.0.1"); err != nil {
		t.Fatalf("acquire() error: %v", err)
	}
	l.release("10.0.0.1")
	if _, ok := l.byUser["10.0.0.1"]; ok {
		t.Errorf("byUser retains an entry for a caller with zero active tunnels, want it removed")
	}
	if err := l.acquire("10.0.0.1"); err != nil {
		t.Errorf("acquire() after release error: %v, want the freed slot to be usable again", err)
	}
}

func TestConsoleTunnelCaller_UsesSessionUsernameWhenPresent(t *testing.T) {
	s, err := NewServer(&fakeClient{}, fakeAuthenticator{user: "ops", pass: "secret"}, map[string]manager.Role{"ops": manager.RoleOperator}, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, err := s.sessions.Create("ops", manager.RoleOperator)
	if err != nil {
		t.Fatalf("sessions.Create() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/console/ws", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	req.RemoteAddr = "203.0.113.5:54321"

	if got := consoleTunnelCaller(s, req); got != "ops" {
		t.Errorf("consoleTunnelCaller() = %q, want the session username %q", got, "ops")
	}
}

func TestConsoleTunnelCaller_FallsBackToRemoteIPWithoutPortWhenNoSession(t *testing.T) {
	s, err := NewServer(&fakeClient{}, nil, nil, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/console/ws", nil)
	req.RemoteAddr = "203.0.113.5:54321"

	if got := consoleTunnelCaller(s, req); got != "203.0.113.5" {
		t.Errorf("consoleTunnelCaller() = %q, want the bare remote IP %q (port stripped)", got, "203.0.113.5")
	}
}

// TestServer_ConsoleWS_RefusesBeyondPerCallerCap is the regression test
// for the 2026-09-29 security audit's "console WebSocket accepts
// unbounded ... connections" finding: opening real WebSocket tunnels up
// to consoleTunnelLimiter's per-caller cap must succeed, one more from
// the same caller must be refused with 429, and closing one of the
// existing tunnels must free the slot for a new one.
func TestServer_ConsoleWS_RefusesBeyondPerCallerCap(t *testing.T) {
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

	var conns []*websocket.Conn
	for i := 0; i < maxConcurrentConsoleTunnelsPerCaller; i++ {
		c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("Dial() #%d error: %v, want it to succeed within the per-caller cap", i, err)
		}
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	if _, resp, err := websocket.DefaultDialer.Dial(wsURL, nil); err == nil {
		t.Fatal("Dial() succeeded, want refusal once the per-caller console-tunnel cap is reached")
	} else if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Errorf("status = %d, want %d", status, http.StatusTooManyRequests)
	}

	conns[0].Close()
	conns = conns[1:]
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.consoleTunnels.mu.Lock()
		total := s.consoleTunnels.total
		s.consoleTunnels.mu.Unlock()
		if total < maxConcurrentConsoleTunnelsPerCaller {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never released the closed connection's console-tunnel slot")
		}
		time.Sleep(10 * time.Millisecond)
	}

	newConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Errorf("Dial() after closing one connection error: %v, want the freed slot to be reusable", err)
	} else {
		conns = append(conns, newConn)
	}
}

// TestServer_ConsoleWS_EnforcesReadLimit is the regression test for the
// message-size half of the same audit finding: gorilla/websocket's
// ReadMessage otherwise grows its buffer to hold whatever a client
// sends, with no ceiling, until proxyConsole's SetReadLimit closes that
// gap. Exceeding the limit must close the connection with
// CloseMessageTooBig, not silently accept or hang.
func TestServer_ConsoleWS_EnforcesReadLimit(t *testing.T) {
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

	oversized := make([]byte, maxConsoleWSMessageBytes+1)
	if err := wsConn.WriteMessage(websocket.BinaryMessage, oversized); err != nil {
		t.Fatalf("WriteMessage() error: %v", err)
	}

	wsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := wsConn.ReadMessage(); err == nil {
		t.Fatal("ReadMessage() = nil error, want the server to close the connection after an over-limit message")
	} else if !websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
		t.Errorf("close error = %v, want CloseMessageTooBig (%d)", err, websocket.CloseMessageTooBig)
	}
}
