package manager

import (
	"testing"

	"github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// TestToInternalJail_RequestedIPPassedAndResolvedIPNot pins the
// address-field split (ADR-0117): a caller's requested_ip_address
// reaches the FSM, but ip_address never does - the FSM resolves it.
func TestToInternalJail_RequestedIPPassedAndResolvedIPNot(t *testing.T) {
	in := &rpcpb.JailDefinition{
		Id:                 "jail-1",
		NetworkId:          "net-1",
		IpAddress:          "10.60.0.99",
		RequestedIpAddress: "10.60.0.50",
	}

	got := toInternalJail(in)

	if got.GetRequestedIpAddress() != "10.60.0.50" {
		t.Errorf("RequestedIpAddress = %q, want 10.60.0.50 passed through", got.GetRequestedIpAddress())
	}
	if got.GetIpAddress() != "" {
		t.Errorf("IpAddress = %q, want empty: a caller must never set the resolved address", got.GetIpAddress())
	}
}

// TestFromInternalJail_ReportsResolvedIP confirms the resolved address
// is what the caller reads back, and the consumed request is not.
func TestFromInternalJail_ReportsResolvedIP(t *testing.T) {
	in := &internalpb.JailDefinition{Id: "jail-1", NetworkId: "net-1", IpAddress: "10.60.0.50"}

	got := fromInternalJail(in)

	if got.GetIpAddress() != "10.60.0.50" {
		t.Errorf("IpAddress = %q, want 10.60.0.50", got.GetIpAddress())
	}
	if got.GetRequestedIpAddress() != "" {
		t.Errorf("RequestedIpAddress = %q, want empty on read-back", got.GetRequestedIpAddress())
	}
}
