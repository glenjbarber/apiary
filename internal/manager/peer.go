package manager

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
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
}

func NewPeerReporter(apiKey string) *PeerReporter {
	return &PeerReporter{APIKey: apiKey}
}

func (p *PeerReporter) dial(addr string) (*grpc.ClientConn, rpcpb.ManagerServiceClient, error) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
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
