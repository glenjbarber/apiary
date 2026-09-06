package frontend

import (
	"context"
	"fmt"
	"io"
	"net/http"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// jailConsoleOwner mirrors consoleOwner (console.go) exactly, for jails
// instead of VMs.
func (s *Server) jailConsoleOwner(ctx context.Context, id string) (string, bool) {
	if s.peers == nil {
		return "", false
	}
	jailResp, err := s.client.GetJail(ctx, &rpcpb.GetJailRequest{Id: id})
	if err != nil || !jailResp.GetFound() || jailResp.GetJail().GetNodeId() == "" {
		return "", false
	}
	statusResp, err := s.client.Status(ctx, &rpcpb.StatusRequest{})
	if err != nil || statusResp.GetManagerNodeId() == "" || jailResp.GetJail().GetNodeId() == statusResp.GetManagerNodeId() {
		return "", false
	}
	return jailResp.GetJail().GetNodeId(), true
}

// resolveJailConsole reports whether id names a jail this frontend can
// plausibly reach a console for, without actually starting a jexec(8)
// session yet - handleJailConsolePage uses this so a jail that doesn't
// exist (or was deleted) shows a clear message instead of a terminal
// widget that just sits there failing to connect. Unlike VM console,
// there is no separate "is a console currently available" signal to
// check in advance (jexec either succeeds or fails when actually
// attempted) - handleJailConsoleWS surfaces that real failure itself.
func (s *Server) resolveJailConsole(ctx context.Context, id string) (name string, consoleErr string) {
	jailResp, err := s.client.GetJail(ctx, &rpcpb.GetJailRequest{Id: id})
	if err != nil {
		return "", err.Error()
	}
	if jailResp.GetError() != "" {
		return "", jailResp.GetError()
	}
	if !jailResp.GetFound() {
		return "", fmt.Sprintf("jail %q not found", id)
	}
	return jailResp.GetJail().GetName(), ""
}

// localJailConsoleTunnel wraps this frontend's own colocated managerd's
// ProxyJailConsole stream into an io.ReadWriteCloser - the local-case
// counterpart to manager.PeerReporter's own jailConsoleTunnel (used for
// a remote Hive instead). Close only ends this one stream
// (stream.CloseSend), never the shared, long-lived *grpc.ClientConn
// s.client is built on - that connection outlives any single console
// session.
type localJailConsoleTunnel struct {
	stream rpcpb.ManagerService_ProxyJailConsoleClient
	read   []byte
}

func (t *localJailConsoleTunnel) Read(p []byte) (int, error) {
	for len(t.read) == 0 {
		frame, err := t.stream.Recv()
		if err != nil {
			return 0, err
		}
		if frame.GetError() != "" {
			return 0, fmt.Errorf("jail console: %s", frame.GetError())
		}
		if len(frame.GetData()) == 0 {
			return 0, fmt.Errorf("jail console sent an invalid frame")
		}
		t.read = frame.GetData()
	}
	n := copy(p, t.read)
	t.read = t.read[n:]
	return n, nil
}

func (t *localJailConsoleTunnel) Write(p []byte) (int, error) {
	if err := t.stream.Send(&rpcpb.JailConsoleTunnelFrame{Payload: &rpcpb.JailConsoleTunnelFrame_Data{Data: p}}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (t *localJailConsoleTunnel) Close() error { return t.stream.CloseSend() }

// openJailConsoleTunnel opens a bidirectional byte tunnel to jail id's
// jexec(8) session, forwarding to the owning peer Hive's managerd when
// this frontend isn't colocated with it - mirroring
// consoleInfoForVM/OpenVMConsole's own local-vs-peer routing exactly.
func (s *Server) openJailConsoleTunnel(ctx context.Context, id string) (io.ReadWriteCloser, error) {
	if owner, remote := s.jailConsoleOwner(ctx, id); remote {
		return s.peers.OpenJailConsole(ctx, s.peerAddr(owner), id)
	}
	stream, err := s.client.ProxyJailConsole(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&rpcpb.JailConsoleTunnelFrame{Payload: &rpcpb.JailConsoleTunnelFrame_Open{Open: &rpcpb.JailConsoleTunnelOpen{Id: id}}}); err != nil {
		return nil, err
	}
	return &localJailConsoleTunnel{stream: stream}, nil
}

// handleJailConsolePage serves the xterm.js-based console page for one
// jail - see web/static/xterm's own vendoring note (MIT, mirrors
// noVNC's vendoring precedent) and ADR-0068 for the full jexec console
// design.
func (s *Server) handleJailConsolePage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name, consoleErr := s.resolveJailConsole(r.Context(), id)

	s.render(w, "jail_console_page", s.withAuthFields(r, pageData{
		JailConsoleID:     id,
		JailConsoleName:   name,
		JailConsoleWSPath: fmt.Sprintf("/jails/%s/console/ws", id),
		ConsoleError:      consoleErr,
		ActivePage:        "jails",
	}))
}

// handleJailConsoleWS upgrades to a WebSocket and proxies raw bytes
// bidirectionally between it and the jail's jexec(8) PTY - reuses
// proxyConsole (console.go), which has no VNC-specific logic of its
// own; it just pumps opaque bytes between any io.ReadWriteCloser and a
// WebSocket.
func (s *Server) handleJailConsoleWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	tunnel, err := s.openJailConsoleTunnel(r.Context(), id)
	if err != nil {
		http.Error(w, "opening jail console: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer tunnel.Close()

	wsConn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote its own error response - nothing more to
		// do here.
		return
	}
	defer wsConn.Close()

	proxyConsole(wsConn, tunnel)
}
