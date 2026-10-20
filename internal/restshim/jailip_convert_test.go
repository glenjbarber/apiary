package restshim

import "testing"

// TestJailRequestedIPAddress_JSONAndRPC confirms the REST request field
// reaches the RPC request as requested_ip_address, and that the
// read-back ip_address is only ever the resolved address.
func TestJailRequestedIPAddress_JSONAndRPC(t *testing.T) {
	j := jail{ID: "jail-1", NetworkID: "net-1", RequestedIPAddress: "10.60.0.50", IPAddress: "10.60.0.99"}

	rpcJail := toRPCJail(j)

	if rpcJail.GetRequestedIpAddress() != "10.60.0.50" {
		t.Errorf("RequestedIpAddress = %q, want 10.60.0.50", rpcJail.GetRequestedIpAddress())
	}
	if rpcJail.GetIpAddress() != "" {
		t.Errorf("IpAddress = %q, want empty: REST input must not set the resolved address", rpcJail.GetIpAddress())
	}

	back := fromRPCJail(rpcJail)
	if back.RequestedIPAddress != "" {
		t.Errorf("read-back RequestedIPAddress = %q, want empty", back.RequestedIPAddress)
	}
}
