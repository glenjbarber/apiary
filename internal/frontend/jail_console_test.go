package frontend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestServer_JailConsolePage_FoundShowsTerminalWidget(t *testing.T) {
	client := &fakeClient{
		getJailResp: &rpcpb.GetJailResponse{Found: true, Jail: &rpcpb.JailDefinition{Id: "jail-1", Name: "web-1", NodeId: "apiarium"}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/jails/jail-1/console", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/static/xterm/xterm.js") {
		t.Errorf("jail console page missing the vendored xterm.js script, got: %s", body)
	}
	if !strings.Contains(body, "web-1") || !strings.Contains(body, "jail-1") {
		t.Errorf("jail console page missing jail name/id, got: %s", body)
	}
	if strings.Contains(body, `class="error"`) {
		t.Errorf("jail console page shows an error banner for a found jail, got: %s", body)
	}
}

func TestServer_JailConsolePage_NotFoundShowsError(t *testing.T) {
	client := &fakeClient{getJailResp: &rpcpb.GetJailResponse{Found: false}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/jails/missing/console", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (error rendered inline)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="error"`) || !strings.Contains(body, "not found") {
		t.Errorf("jail console page missing not-found error, got: %s", body)
	}
	if strings.Contains(body, "/static/xterm/xterm.js") {
		t.Errorf("jail console page should not render the terminal widget for a missing jail, got: %s", body)
	}
}

// TestServer_JailConsoleWS_ProxiesBytesBothWays proves the whole local
// path end to end: a real WebSocket client, through handleJailConsoleWS,
// openJailConsoleTunnel's local branch, and the fake ProxyJailConsole
// stream, which echoes data back - mirroring
// TestServer_ConsoleWS_ProxiesBytesToAndFromVNCEndpoint's own real-proof
// standard for the VM console.
func TestServer_JailConsoleWS_ProxiesBytesBothWays(t *testing.T) {
	client := &fakeClient{
		getJailResp: &rpcpb.GetJailResponse{Found: true, Jail: &rpcpb.JailDefinition{Id: "jail-1", NodeId: "apiarium"}},
	}
	s, err := NewServer(client, nil, nil, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/jails/jail-1/console/ws"
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("websocket.Dial() error: %v", err)
	}
	defer wsConn.Close()

	if err := wsConn.WriteMessage(websocket.BinaryMessage, []byte("echo hello-jail-console")); err != nil {
		t.Fatalf("WriteMessage() error: %v", err)
	}

	wsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, data, err := wsConn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage() error: %v", err)
	}
	if msgType != websocket.BinaryMessage || string(data) != "echo hello-jail-console" {
		t.Errorf("echoed message = (%d, %q), want (BinaryMessage, %q)", msgType, data, "echo hello-jail-console")
	}
}

func TestServer_JailConsoleWS_DialFailureReturnsBadGateway(t *testing.T) {
	client := &fakeClient{
		getJailResp:         &rpcpb.GetJailResponse{Found: true, Jail: &rpcpb.JailDefinition{Id: "jail-1", NodeId: "apiarium"}},
		proxyJailConsoleErr: errors.New("cannot open jail console"),
	}
	s, err := NewServer(client, nil, nil, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/jails/jail-1/console/ws", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}
