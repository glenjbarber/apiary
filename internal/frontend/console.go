package frontend

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// consoleDialTimeout bounds how long handleConsoleWS waits to reach the
// VM's VNC framebuffer once it knows where to look - a hung TCP dial
// shouldn't hold an upgraded websocket open indefinitely.
const consoleDialTimeout = 5 * time.Second

// wsUpgrader upgrades the console's HTTP connection to a WebSocket.
// CheckOrigin is permissive: this endpoint is reached only through the
// console page this same server renders, on the same local-network
// deployment every other route in this UI already assumes (see
// ADR-0014's/ADR-0019's consequences on authentication) - no stricter
// than the rest of the UI's own trust model.
var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// resolveConsole calls managerd's GetVMConsole and folds every failure
// mode (transport error, application error, VM not yet available) into
// one message - handleConsolePage shows it as a banner, handleConsoleWS
// as a plain HTTP error, since a WebSocket upgrade has no good way to
// carry a human-readable message once it succeeds.
func (s *Server) resolveConsole(ctx context.Context, id string) (*rpcpb.GetVMConsoleResponse, string) {
	resp, err := s.client.GetVMConsole(ctx, &rpcpb.GetVMConsoleRequest{Id: id})
	if err != nil {
		return nil, err.Error()
	}
	if resp.GetError() != "" {
		return nil, resp.GetError()
	}
	if !resp.GetAvailable() {
		return nil, "this VM has no running console yet (not yet reconciled, or created before VNC support existed)"
	}
	return resp, ""
}

// handleConsolePage serves the noVNC-based console page for one VM. The
// availability check happens here too (not just in handleConsoleWS) so a
// VM with no console yet shows a clear message instead of a noVNC widget
// that just sits there failing to connect.
func (s *Server) handleConsolePage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var vmName string
	if resp, err := s.client.GetVM(r.Context(), &rpcpb.GetVMRequest{Id: id}); err == nil && resp.GetFound() {
		vmName = resp.GetVm().GetName()
	}

	_, consoleErr := s.resolveConsole(r.Context(), id)

	s.render(w, "console_page", pageData{
		ConsoleVMID:   id,
		ConsoleVMName: vmName,
		ConsoleWSPath: fmt.Sprintf("/vms/%s/console/ws", id),
		ConsoleError:  consoleErr,
		ActivePage:    "vms",
		AuthEnabled:   s.authUser != "",
	})
}

// handleConsoleWS upgrades to a WebSocket and proxies raw bytes
// bidirectionally between it and the VM's VNC TCP listener - noVNC (like
// every browser VNC client) speaks the RFB protocol over a WebSocket
// bytestream, and a browser can't open a raw TCP socket itself, so this
// proxy is what makes noVNC usable at all without a separate websockify
// process. The proxy has no understanding of that protocol; it forwards
// binary WebSocket messages to the TCP connection and vice versa.
func (s *Server) handleConsoleWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	consoleInfo, consoleErr := s.resolveConsole(r.Context(), id)
	if consoleErr != "" {
		http.Error(w, consoleErr, http.StatusServiceUnavailable)
		return
	}

	tcpConn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", consoleInfo.GetHost(), consoleInfo.GetPort()), consoleDialTimeout)
	if err != nil {
		http.Error(w, "dialing VM console: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer tcpConn.Close()

	wsConn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote its own error response - nothing more to
		// do here.
		return
	}
	defer wsConn.Close()

	proxyConsole(wsConn, tcpConn)
}

// proxyConsole pumps bytes between ws and tcp until either side closes
// or errors, then closes both ends so the other direction's blocking
// Read unblocks too, and waits for both goroutines to actually exit
// before returning - so the caller's own deferred Close calls never race
// a still-running copy.
func proxyConsole(ws *websocket.Conn, tcp net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := tcp.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		ws.Close()
		tcp.Close()
	}()

	go func() {
		defer wg.Done()
		for {
			msgType, data, err := ws.ReadMessage()
			if err != nil {
				break
			}
			if msgType != websocket.BinaryMessage {
				continue
			}
			if _, err := tcp.Write(data); err != nil {
				break
			}
		}
		ws.Close()
		tcp.Close()
	}()

	wg.Wait()
}
