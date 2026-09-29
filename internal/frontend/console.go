package frontend

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// consoleDialTimeout bounds how long handleConsoleWS waits to reach the
// VM's VNC framebuffer once it knows where to look - a hung TCP dial
// shouldn't hold an upgraded websocket open indefinitely.
const consoleDialTimeout = 5 * time.Second

// maxConsoleWSMessageBytes bounds a single inbound WebSocket message
// pumped from the browser toward the VM's VNC connection (2026-09-29
// security audit, "console WebSocket accepts unbounded messages" -
// gorilla/websocket's ReadMessage otherwise grows its buffer to hold
// whatever the client sends, with no ceiling). Ordinary RFB
// client-to-server traffic - key/pointer events, the occasional
// encoding-negotiation message - is a few bytes to a few hundred; a
// generous 1 MiB is nowhere near real traffic and exists purely as a
// backstop against a hostile or malfunctioning client.
const maxConsoleWSMessageBytes = 1 << 20 // 1 MiB

// consolePongWait/consolePingPeriod/consoleWriteWait implement the
// standard gorilla/websocket ping/pong keepalive (see the package's own
// chat example). The server requires some message - a pong, or real
// VNC traffic - at least every consolePongWait, resetting the read
// deadline each time one arrives; it sends an unsolicited ping every
// consolePingPeriod (comfortably under consolePongWait, leaving the
// client time to answer) so an otherwise-idle console still gets
// checked. Without this, a client that stops reading and writing
// entirely - a suspended browser tab, a network path that dropped with
// no RST - held its side of the proxy, and the TCP connection to the
// VM's VNC listener, open indefinitely (the same audit finding).
const (
	consolePongWait   = 60 * time.Second
	consolePingPeriod = (consolePongWait * 9) / 10
	consoleWriteWait  = 10 * time.Second
)

// wsUpgrader upgrades the console's HTTP connection to a WebSocket.
// CheckOrigin rejects a cross-origin WebSocket handshake rather than
// accepting every origin - a 2026-09-06 security-audit finding noted
// that accepting any origin here is the classic cross-site WebSocket
// hijacking setup, and that this endpoint's only real protection
// against it was the session cookie's SameSite=Lax attribute happening
// to block the cookie on a cross-site request - a defense living in a
// different file (server.go) than this one, which would silently stop
// applying if that cookie attribute ever changed. checkConsoleOrigin
// enforces the check directly here instead of relying on that
// incidental protection.
var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	CheckOrigin:     checkConsoleOrigin,
}

// checkConsoleOrigin allows a same-origin WebSocket handshake (no
// Origin header at all, e.g. a non-browser client or an older browser
// that omits it, is also allowed - matching gorilla/websocket's own
// default CheckOrigin behavior for that case) and rejects anything
// naming a different origin than the request's own Host.
func checkConsoleOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

// resolveConsole calls managerd's GetVMConsole and folds every failure
// mode (transport error, application error, VM not yet available) into
// one message - handleConsolePage shows it as a banner, handleConsoleWS
// as a plain HTTP error, since a WebSocket upgrade has no good way to
// carry a human-readable message once it succeeds.
func (s *Server) resolveConsole(ctx context.Context, id string) (*rpcpb.GetVMConsoleResponse, string) {
	resp, err := s.consoleInfoForVM(ctx, id)
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

func (s *Server) consoleOwner(ctx context.Context, id string) (string, bool) {
	if s.peers == nil {
		return "", false
	}
	vmResp, err := s.client.GetVM(ctx, &rpcpb.GetVMRequest{Id: id})
	if err != nil || !vmResp.GetFound() || vmResp.GetVm().GetNodeId() == "" {
		return "", false
	}
	statusResp, err := s.client.Status(ctx, &rpcpb.StatusRequest{})
	if err != nil || statusResp.GetManagerNodeId() == "" || vmResp.GetVm().GetNodeId() == statusResp.GetManagerNodeId() {
		return "", false
	}
	return vmResp.GetVm().GetNodeId(), true
}

func (s *Server) consoleInfoForVM(ctx context.Context, id string) (*rpcpb.GetVMConsoleResponse, error) {
	if owner, remote := s.consoleOwner(ctx, id); remote {
		return s.peers.GetVMConsole(ctx, s.peerAddr(owner), id)
	}
	return s.client.GetVMConsole(ctx, &rpcpb.GetVMConsoleRequest{Id: id})
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

	s.render(w, "console_page", s.withAuthFields(r, pageData{
		ConsoleVMID:   id,
		ConsoleVMName: vmName,
		ConsoleWSPath: fmt.Sprintf("/vms/%s/console/ws", id),
		ConsoleError:  consoleErr,
		ActivePage:    "vms",
	}))
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

	caller := consoleTunnelCaller(s, r)
	if err := s.consoleTunnels.acquire(caller); err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	defer s.consoleTunnels.release(caller)

	consoleInfo, consoleErr := s.resolveConsole(r.Context(), id)
	if consoleErr != "" {
		http.Error(w, consoleErr, http.StatusServiceUnavailable)
		return
	}

	var tcpConn io.ReadWriteCloser
	var err error
	if owner, remote := s.consoleOwner(r.Context(), id); remote {
		tcpConn, err = s.peers.OpenVMConsole(r.Context(), s.peerAddr(owner), id)
	} else {
		tcpConn, err = net.DialTimeout("tcp", fmt.Sprintf("%s:%d", consoleInfo.GetHost(), consoleInfo.GetPort()), consoleDialTimeout)
	}
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
// a still-running copy. It also enforces maxConsoleWSMessageBytes and
// drives the ping/pong keepalive described at those constants' own doc
// comments.
func proxyConsole(ws *websocket.Conn, tcp io.ReadWriteCloser) {
	ws.SetReadLimit(maxConsoleWSMessageBytes)
	ws.SetReadDeadline(time.Now().Add(consolePongWait))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(consolePongWait))
		return nil
	})

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

	// A separate goroutine, not folded into either pump above: WriteControl
	// is documented safe to call concurrently with the data-pump goroutines'
	// own ReadMessage/WriteMessage calls (unlike two goroutines both calling
	// WriteMessage, which is not), so this needs no extra synchronization
	// with them. It exits either when a ping fails to send (the connection
	// is already going down) or once pingDone is closed after both pumps
	// finish, whichever comes first - never leaked past proxyConsole's
	// return.
	pingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(consolePingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(consoleWriteWait)); err != nil {
					return
				}
			case <-pingDone:
				return
			}
		}
	}()

	wg.Wait()
	close(pingDone)
}
