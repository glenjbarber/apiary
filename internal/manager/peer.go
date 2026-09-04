package manager

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// PeerReporter implements internal/cluster's peerReporter interface,
// letting a node's Reconciler forward a raft write rejected by its own
// (non-leader) raftd to the leader node's own managerd instead - see
// ADR-0029. Dials fresh per call rather than caching a connection: the
// target address (whichever node is currently leader) can change
// between calls, and a fresh dial's cost is negligible next to an
// actual raft round-trip - simplicity over an optimization nothing
// here needs yet.
type PeerReporter struct {
	// APIKey is attached to every peer call the same way
	// cmd/frontend/cmd/restshimd attach their own (ADR-0023) - empty
	// means unauthenticated, matching managerd's own default. Once a
	// cluster has any API key created, every peer managerd also needs
	// this set (to a valid key) or these calls start failing
	// Unauthenticated instead of "not leader" - see cmd/managerd's
	// -peer-api-key flag.
	APIKey string

	// UseTLS dials every peer over TLS, trusting the system certificate
	// pool - the expected case once every node in a cluster has a real,
	// publicly-trusted certificate (ADR-0033), rather than a private CA
	// needing its own trust configuration. False (the default) preserves
	// the original plaintext-only behavior for a cluster that hasn't
	// adopted TLS everywhere yet.
	UseTLS bool

	// PeerHostnames maps a peer's bare IP (as it appears in a raft
	// leader_hint, e.g. "10.50.0.11") to the hostname its TLS
	// certificate actually names (e.g. "freebsd-apiary.apiary.work") -
	// raft's own leader_hint is always a plain address, never a
	// hostname, so without this a real cert (which Let's Encrypt never
	// issues for a bare IP) can never verify against the address
	// actually being dialed. This project runs on a small, fixed set of
	// known nodes, not an arbitrary/dynamic fleet, so a small static map
	// an operator fills in once is the right amount of machinery here -
	// see cmd/managerd's -peer-tls-hostname-map flag. An IP with no
	// entry fails TLS verification loudly (dialing the bare IP against
	// a hostname-only cert) rather than silently skipping verification.
	PeerHostnames map[string]string
}

func NewPeerReporter(apiKey string, useTLS bool, peerHostnames map[string]string) *PeerReporter {
	return &PeerReporter{APIKey: apiKey, UseTLS: useTLS, PeerHostnames: peerHostnames}
}

func (p *PeerReporter) dial(addr string) (*grpc.ClientConn, rpcpb.ManagerServiceClient, error) {
	var opts []grpc.DialOption
	if p.UseTLS {
		cfg := &tls.Config{}
		if host, _, err := net.SplitHostPort(addr); err == nil {
			if name, ok := p.PeerHostnames[host]; ok {
				cfg.ServerName = name
			}
		}
		opts = []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(cfg))}
	} else {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	if p.APIKey != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(apiKeyCredentials(p.APIKey)))
	}
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("dialing peer managerd at %s: %w", addr, err)
	}
	return conn, rpcpb.NewManagerServiceClient(conn), nil
}

// apiKeyCredentials mirrors cmd/frontend's own type of the same name -
// duplicated rather than shared, since cmd/frontend's copy is
// unexported in package main and this project's existing convention
// (stateFromRPC et al.) already accepts this kind of small duplication
// across independent binaries/packages over a shared-but-farther-away
// helper.
type apiKeyCredentials string

func (k apiKeyCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(k)}, nil
}

func (apiKeyCredentials) RequireTransportSecurity() bool { return false }

func vmPhaseToRPC(phase string) rpcpb.VMPhase {
	switch phase {
	case "creating":
		return rpcpb.VMPhase_VM_PHASE_CREATING
	case "ready":
		return rpcpb.VMPhase_VM_PHASE_READY
	case "deleting":
		return rpcpb.VMPhase_VM_PHASE_DELETING
	case "error":
		return rpcpb.VMPhase_VM_PHASE_ERROR
	default:
		return rpcpb.VMPhase_VM_PHASE_UNSPECIFIED
	}
}

func jailPhaseToRPC(phase string) rpcpb.JailPhase {
	switch phase {
	case "creating":
		return rpcpb.JailPhase_JAIL_PHASE_CREATING
	case "ready":
		return rpcpb.JailPhase_JAIL_PHASE_READY
	case "deleting":
		return rpcpb.JailPhase_JAIL_PHASE_DELETING
	case "error":
		return rpcpb.JailPhase_JAIL_PHASE_ERROR
	default:
		return rpcpb.JailPhase_JAIL_PHASE_UNSPECIFIED
	}
}

func (p *PeerReporter) ReportVMPhase(ctx context.Context, addr, id, phase, phaseError string) error {
	conn, client, err := p.dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	resp, err := client.ReportVMPhase(ctx, &rpcpb.ReportVMPhaseRequest{
		Id: id, Phase: vmPhaseToRPC(phase), PhaseError: phaseError,
	})
	if err != nil {
		return err
	}
	if resp.GetError() != "" {
		return fmt.Errorf("%s", resp.GetError())
	}
	return nil
}

func (p *PeerReporter) ReportVMTeardownComplete(ctx context.Context, addr, id string) error {
	conn, client, err := p.dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	resp, err := client.ReportVMTeardownComplete(ctx, &rpcpb.ReportVMTeardownCompleteRequest{Id: id})
	if err != nil {
		return err
	}
	if resp.GetError() != "" {
		return fmt.Errorf("%s", resp.GetError())
	}
	return nil
}

func (p *PeerReporter) ReportJailPhase(ctx context.Context, addr, id, phase, phaseError string) error {
	conn, client, err := p.dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	resp, err := client.ReportJailPhase(ctx, &rpcpb.ReportJailPhaseRequest{
		Id: id, Phase: jailPhaseToRPC(phase), PhaseError: phaseError,
	})
	if err != nil {
		return err
	}
	if resp.GetError() != "" {
		return fmt.Errorf("%s", resp.GetError())
	}
	return nil
}

// ListVMs/GetVM/ListJails/GetJail/ListNetworks forward a leader-only
// read rejected by this node's own raftd to the leader node's own
// managerd, mirroring the Report* methods' write-forwarding pattern
// above but returning the peer's actual response instead of just an
// error - a read has no local side effect to skip, so the caller
// should get the real answer, not merely confirmation the forward
// succeeded (see ADR-0035).
func (p *PeerReporter) ListVMs(ctx context.Context, addr string) (*rpcpb.ListVMsResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.ListVMs(ctx, &rpcpb.ListVMsRequest{})
}

func (p *PeerReporter) GetVM(ctx context.Context, addr, id string) (*rpcpb.GetVMResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.GetVM(ctx, &rpcpb.GetVMRequest{Id: id})
}

func (p *PeerReporter) ListJails(ctx context.Context, addr string) (*rpcpb.ListJailsResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.ListJails(ctx, &rpcpb.ListJailsRequest{})
}

func (p *PeerReporter) GetJail(ctx context.Context, addr, id string) (*rpcpb.GetJailResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.GetJail(ctx, &rpcpb.GetJailRequest{Id: id})
}

func (p *PeerReporter) ListNetworks(ctx context.Context, addr string) (*rpcpb.ListNetworksResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.ListNetworks(ctx, &rpcpb.ListNetworksRequest{})
}

// SimulateNodeFailure forwards the entire original request to a peer -
// see ADR-0052's SimulateNodeFailure RPC and its own handler for why
// the whole request, not just one failed sub-call, is forwarded.
func (p *PeerReporter) SimulateNodeFailure(ctx context.Context, addr string, req *rpcpb.SimulateNodeFailureRequest) (*rpcpb.SimulateNodeFailureResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.SimulateNodeFailure(ctx, req)
}

// ListISOs forwards to a specific peer's own ListISOs RPC - like
// HostStats, not leader-only-read forwarding (ISOs always answer
// locally for whichever managerd receives the call); this is how
// internal/frontend's Images page builds its cluster-wide view
// (ADR-0040), addressing addr directly the same way HostStats already
// does.
func (p *PeerReporter) ListISOs(ctx context.Context, addr string) (*rpcpb.ListISOsResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.ListISOs(ctx, &rpcpb.ListISOsRequest{})
}

// HostStats forwards to a specific peer's own HostStats RPC - unlike
// ListVMs/GetVM/etc. above, this isn't leader-only-read forwarding
// (HostStats always answers locally, for whichever managerd receives
// the call); it's how internal/frontend reaches a node other than the
// one it's colocated with at all, addressing addr directly (a real DNS
// hostname this project's own nodes are each issued, not an IP derived
// from a raft leader_hint - see cmd/frontend's own -peer-* flags).
func (p *PeerReporter) HostStats(ctx context.Context, addr string) (*rpcpb.HostStatsResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.HostStats(ctx, &rpcpb.HostStatsRequest{})
}

// CreateVM/UpdateVM/DeleteVM/CreateJail/UpdateJail/DeleteJail/
// CreateNetwork/DeleteNetwork/CreateAPIKey/RevokeAPIKey forward an
// external write RPC rejected by this node's own raftd (not the
// leader) to the leader node's own managerd, passing the caller's
// original request through unchanged and returning the leader's actual
// response - the same "return the real answer, not just success/
// failure" posture ListVMs/GetVM/etc. already established for reads
// above, since a Create/Update/Delete response carries the resulting
// record the caller needs (e.g. the web UI redirecting on a newly
// created VM's id).
func (p *PeerReporter) CreateVM(ctx context.Context, addr string, req *rpcpb.CreateVMRequest) (*rpcpb.CreateVMResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.CreateVM(ctx, req)
}

func (p *PeerReporter) UpdateVM(ctx context.Context, addr string, req *rpcpb.UpdateVMRequest) (*rpcpb.UpdateVMResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.UpdateVM(ctx, req)
}

func (p *PeerReporter) DeleteVM(ctx context.Context, addr string, req *rpcpb.DeleteVMRequest) (*rpcpb.DeleteVMResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.DeleteVM(ctx, req)
}

func (p *PeerReporter) CreateJail(ctx context.Context, addr string, req *rpcpb.CreateJailRequest) (*rpcpb.CreateJailResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.CreateJail(ctx, req)
}

func (p *PeerReporter) UpdateJail(ctx context.Context, addr string, req *rpcpb.UpdateJailRequest) (*rpcpb.UpdateJailResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.UpdateJail(ctx, req)
}

func (p *PeerReporter) DeleteJail(ctx context.Context, addr string, req *rpcpb.DeleteJailRequest) (*rpcpb.DeleteJailResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.DeleteJail(ctx, req)
}

func (p *PeerReporter) CreateNetwork(ctx context.Context, addr string, req *rpcpb.CreateNetworkRequest) (*rpcpb.CreateNetworkResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.CreateNetwork(ctx, req)
}

func (p *PeerReporter) DeleteNetwork(ctx context.Context, addr string, req *rpcpb.DeleteNetworkRequest) (*rpcpb.DeleteNetworkResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.DeleteNetwork(ctx, req)
}

// CreateAPIKey/RevokeAPIKey forward the caller's original request as-is
// - notably, a forwarded CreateAPIKey means the LEADER generates and
// hashes its own fresh raw key; this node's own locally-generated
// raw/hashed pair (built before the local Apply was attempted, see
// Server.CreateAPIKey) is simply discarded on the forwarding path, so
// no key material ever needs to cross nodes.
func (p *PeerReporter) CreateAPIKey(ctx context.Context, addr string, req *rpcpb.CreateAPIKeyRequest) (*rpcpb.CreateAPIKeyResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.CreateAPIKey(ctx, req)
}

func (p *PeerReporter) RevokeAPIKey(ctx context.Context, addr string, req *rpcpb.RevokeAPIKeyRequest) (*rpcpb.RevokeAPIKeyResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.RevokeAPIKey(ctx, req)
}

// ListAPIKeys forwards a leader-only read rejected by this node's own
// raftd to the leader node's own managerd - mirrors ListVMs/GetVM/etc.
// above (ADR-0035). Missing from that original set (ADR-0037's
// follow-up closes the gap): ListAPIKeys is a read, but was never
// included in ADR-0035's original list of forwarded reads.
func (p *PeerReporter) ListAPIKeys(ctx context.Context, addr string) (*rpcpb.ListAPIKeysResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.ListAPIKeys(ctx, &rpcpb.ListAPIKeysRequest{})
}

// ForcePurgeVM/MigrateVM/ForcePurgeJail/MigrateJail forward the whole
// original request to the leader node's own managerd - unlike
// Create/Update/Delete, these RPCs read local FSM state up front (via
// RaftClient.GetVM/GetJail, not the exported, now-forwarding GetVM/
// GetJail RPC handlers) before ever attempting an Apply, so this node's
// own local read has to be the thing that triggers the forward, not
// just a rejected Apply (ADR-0037's follow-up, closing the gap that
// ADR itself named as left open).
func (p *PeerReporter) ForcePurgeVM(ctx context.Context, addr string, req *rpcpb.ForcePurgeVMRequest) (*rpcpb.ForcePurgeVMResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.ForcePurgeVM(ctx, req)
}

func (p *PeerReporter) MigrateVM(ctx context.Context, addr string, req *rpcpb.MigrateVMRequest) (*rpcpb.MigrateVMResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.MigrateVM(ctx, req)
}

func (p *PeerReporter) SetVMFirewallPaused(ctx context.Context, addr string, req *rpcpb.SetVMFirewallPausedRequest) (*rpcpb.SetVMFirewallPausedResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.SetVMFirewallPaused(ctx, req)
}

func (p *PeerReporter) ForcePurgeJail(ctx context.Context, addr string, req *rpcpb.ForcePurgeJailRequest) (*rpcpb.ForcePurgeJailResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.ForcePurgeJail(ctx, req)
}

func (p *PeerReporter) MigrateJail(ctx context.Context, addr string, req *rpcpb.MigrateJailRequest) (*rpcpb.MigrateJailResponse, error) {
	conn, client, err := p.dial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return client.MigrateJail(ctx, req)
}

func (p *PeerReporter) ReportJailTeardownComplete(ctx context.Context, addr, id string) error {
	conn, client, err := p.dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	resp, err := client.ReportJailTeardownComplete(ctx, &rpcpb.ReportJailTeardownCompleteRequest{Id: id})
	if err != nil {
		return err
	}
	if resp.GetError() != "" {
		return fmt.Errorf("%s", resp.GetError())
	}
	return nil
}

// UploadISO streams r (a local file this node already has) into addr's
// own UploadISO RPC as a client, mirroring internal/frontend's own
// uploadISOStream exactly (metadata message first, then chunks, then
// CloseAndRecv) - the same client-streaming shape a browser upload
// already uses, just with this managerd acting as the client instead
// (the PushISOTo handler is the caller).
func (p *PeerReporter) UploadISO(ctx context.Context, addr, name, expectedSHA256 string, r io.Reader) error {
	conn, client, err := p.dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := client.UploadISO(ctx)
	if err != nil {
		return fmt.Errorf("opening upload stream to %s: %w", addr, err)
	}
	if err := stream.Send(&rpcpb.UploadISORequest{
		Data: &rpcpb.UploadISORequest_Metadata{
			Metadata: &rpcpb.ISOUploadMetadata{Name: name, ExpectedSha256: expectedSHA256},
		},
	}); err != nil {
		return fmt.Errorf("sending upload metadata to %s: %w", addr, err)
	}

	buf := make([]byte, 256*1024)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if serr := stream.Send(&rpcpb.UploadISORequest{Data: &rpcpb.UploadISORequest_Chunk{Chunk: chunk}}); serr != nil {
				return fmt.Errorf("sending upload data to %s: %w", addr, serr)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("reading local file for upload to %s: %w", addr, rerr)
		}
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("closing upload stream to %s: %w", addr, err)
	}
	if resp.GetError() != "" {
		return fmt.Errorf("%s rejected the upload: %s", addr, resp.GetError())
	}
	return nil
}

// RequestISOPush calls addr's own PushISOTo RPC - used by
// internal/cluster's Reconciler (ADR-0041) to ask a peer node that's
// already confirmed (via ListISONames) to have a named image push it
// to this node, rather than this node pulling bytes itself.
func (p *PeerReporter) RequestISOPush(ctx context.Context, addr, name, targetNodeID string) error {
	conn, client, err := p.dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	resp, err := client.PushISOTo(ctx, &rpcpb.PushISOToRequest{Name: name, TargetNodeId: targetNodeID})
	if err != nil {
		return err
	}
	if resp.GetError() != "" {
		return fmt.Errorf("%s", resp.GetError())
	}
	return nil
}

// ListISONames is ListISOs narrowed to just the names present at addr -
// used by internal/cluster's Reconciler (ADR-0041) to find which known
// peer, if any, already has an image this node's own isostore lacks.
// A distinct method (rather than reusing ListISOs's own rpcpb-typed
// signature) because internal/cluster's peerReporter interface
// deliberately stays decoupled from rpcpb wire types, the same
// reasoning peer.go's phase-as-plain-string methods already establish.
func (p *PeerReporter) ListISONames(ctx context.Context, addr string) ([]string, error) {
	resp, err := p.ListISOs(ctx, addr)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(resp.GetIsos()))
	for _, iso := range resp.GetIsos() {
		names = append(names, iso.GetName())
	}
	return names, nil
}
