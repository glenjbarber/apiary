package raft

import (
	"strings"
	"testing"

	"github.com/hashicorp/raft"

	"github.com/glenjbarber/apiary/api/internalpb"
)

// createJailWithIPCmd builds a CreateJail command carrying an
// operator-requested address and, optionally, a caller-supplied
// ip_address that the FSM must ignore.
func createJailWithIPCmd(id, networkID, requested, callerIP string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_CreateJail{
			CreateJail: &internalpb.CreateJail{Jail: &internalpb.JailDefinition{
				Id:                 id,
				NetworkId:          networkID,
				RequestedIpAddress: requested,
				IpAddress:          callerIP,
			}},
		},
	}
}

// applyJailIPTest applies cmd at index and returns the result, failing
// the test on a raft-level error. A FSM-level rejection is returned
// via FSMApplyResult.Error for the caller to assert on.
func applyJailIPTest(t *testing.T, fsm *FSM, index uint64, cmd *internalpb.Command) *FSMApplyResult {
	t.Helper()
	return fsm.Apply(&raft.Log{Index: index, Data: mustMarshalCommand(t, cmd)}).(*FSMApplyResult)
}

func newJailIPTestFSM(t *testing.T) *FSM {
	t.Helper()
	fsm := NewFSM()
	res := applyJailIPTest(t, fsm, 1, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))
	if res.Error != "" {
		t.Fatalf("CreateNetwork error = %q", res.Error)
	}
	return fsm
}

func TestFSM_Apply_CreateJailRequestedIPAccepted(t *testing.T) {
	fsm := newJailIPTestFSM(t)

	res := applyJailIPTest(t, fsm, 2, createJailWithIPCmd("jail-1", "net-1", "10.60.0.50", ""))

	if res.Error != "" {
		t.Fatalf("Error = %q, want empty", res.Error)
	}
	if got := res.Jail.GetIpAddress(); got != "10.60.0.50" {
		t.Errorf("IpAddress = %q, want the requested 10.60.0.50", got)
	}
	if got := res.Jail.GetRequestedIpAddress(); got != "" {
		t.Errorf("RequestedIpAddress = %q, want cleared on the stored record", got)
	}
	stored, ok := fsm.Jail("jail-1")
	if !ok || stored.GetIpAddress() != "10.60.0.50" {
		t.Errorf("stored IpAddress = %q, want 10.60.0.50", stored.GetIpAddress())
	}
}

func TestFSM_Apply_CreateJailRequestedIPRejections(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		wantErr   string
	}{
		{"not an address", "not-an-ip", "must be an IPv4 address"},
		{"ipv6", "fd00::5", "must be an IPv4 address"},
		{"outside subnet", "10.61.0.5", "outside network"},
		{"network address", "10.60.0.0", "network address"},
		{"gateway", "10.60.0.1", "gateway"},
		{"broadcast", "10.60.0.255", "broadcast address"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsm := newJailIPTestFSM(t)

			res := applyJailIPTest(t, fsm, 2, createJailWithIPCmd("jail-1", "net-1", tc.requested, ""))

			if res.Error == "" {
				t.Fatalf("Error = empty, want a rejection for %q", tc.requested)
			}
			if !strings.Contains(res.Error, tc.wantErr) {
				t.Errorf("Error = %q, want it to mention %q", res.Error, tc.wantErr)
			}
			if _, ok := fsm.Jail("jail-1"); ok {
				t.Errorf("jail-1 was stored despite the rejected address")
			}
		})
	}
}

func TestFSM_Apply_CreateJailRequestedIPDuplicateOfVMRejected(t *testing.T) {
	fsm := newJailIPTestFSM(t)
	vmRes := applyJailIPTest(t, fsm, 2, createVMOnNetworkCmd("vm-1", "net-1"))
	vmIP := vmRes.VM.GetIpAddress()
	if vmIP != "10.60.0.2" {
		t.Fatalf("VM IpAddress = %q, want 10.60.0.2 from allocation", vmIP)
	}

	res := applyJailIPTest(t, fsm, 3, createJailWithIPCmd("jail-1", "net-1", vmIP, ""))

	if res.Error == "" || !strings.Contains(res.Error, "vm vm-1") {
		t.Fatalf("Error = %q, want a duplicate rejection naming vm vm-1", res.Error)
	}
}

func TestFSM_Apply_CreateJailRequestedIPDuplicateOfJailRejected(t *testing.T) {
	fsm := newJailIPTestFSM(t)
	if res := applyJailIPTest(t, fsm, 2, createJailWithIPCmd("jail-1", "net-1", "10.60.0.20", "")); res.Error != "" {
		t.Fatalf("first jail Error = %q", res.Error)
	}

	res := applyJailIPTest(t, fsm, 3, createJailWithIPCmd("jail-2", "net-1", "10.60.0.20", ""))

	if res.Error == "" || !strings.Contains(res.Error, "jail jail-1") {
		t.Fatalf("Error = %q, want a duplicate rejection naming jail jail-1", res.Error)
	}
}

// TestFSM_Apply_RequestedIPIsSkippedByAllocation confirms an
// operator-set address is visible to FSM allocation: a later VM or
// auto-allocated jail skips it rather than being handed the same host.
func TestFSM_Apply_RequestedIPIsSkippedByAllocation(t *testing.T) {
	fsm := newJailIPTestFSM(t)
	// Take .2 by request; allocation would otherwise hand that out first.
	if res := applyJailIPTest(t, fsm, 2, createJailWithIPCmd("jail-req", "net-1", "10.60.0.2", "")); res.Error != "" {
		t.Fatalf("requested jail Error = %q", res.Error)
	}

	vmRes := applyJailIPTest(t, fsm, 3, createVMOnNetworkCmd("vm-1", "net-1"))
	autoRes := applyJailIPTest(t, fsm, 4, createJailWithIPCmd("jail-auto", "net-1", "", ""))

	if got := vmRes.VM.GetIpAddress(); got != "10.60.0.3" {
		t.Errorf("VM IpAddress = %q, want 10.60.0.3 (skipping the requested .2)", got)
	}
	if got := autoRes.Jail.GetIpAddress(); got != "10.60.0.4" {
		t.Errorf("auto jail IpAddress = %q, want 10.60.0.4", got)
	}
}

func TestFSM_Apply_CreateJailAutoAllocatesWhenBlank(t *testing.T) {
	fsm := newJailIPTestFSM(t)

	res := applyJailIPTest(t, fsm, 2, createJailWithIPCmd("jail-1", "net-1", "", ""))

	if res.Error != "" {
		t.Fatalf("Error = %q, want empty", res.Error)
	}
	if got := res.Jail.GetIpAddress(); got != "10.60.0.2" {
		t.Errorf("IpAddress = %q, want the first free host 10.60.0.2", got)
	}
}

func TestFSM_Apply_CreateJailRequestedIPNeedsNetwork(t *testing.T) {
	fsm := newJailIPTestFSM(t)

	res := applyJailIPTest(t, fsm, 2, createJailWithIPCmd("jail-1", "", "10.60.0.50", ""))

	if res.Error == "" || !strings.Contains(res.Error, "requires network_id") {
		t.Fatalf("Error = %q, want a requires-network_id rejection", res.Error)
	}
}

func TestFSM_Apply_CreateJailRequestedIPOnUplinkBridgedRejected(t *testing.T) {
	fsm := NewFSM()
	netRes := applyJailIPTest(t, fsm, 1, &internalpb.Command{
		Op: &internalpb.Command_CreateNetwork{CreateNetwork: &internalpb.CreateNetwork{
			Network: &internalpb.NetworkDefinition{Id: "net-up", Subnet: "10.50.0.0/24", UplinkBridged: true},
		}},
	})
	if netRes.Error != "" {
		t.Fatalf("CreateNetwork error = %q", netRes.Error)
	}

	res := applyJailIPTest(t, fsm, 2, createJailWithIPCmd("jail-1", "net-up", "10.50.0.50", ""))

	if res.Error == "" || !strings.Contains(res.Error, "uplink_bridged") {
		t.Fatalf("Error = %q, want an uplink_bridged rejection", res.Error)
	}
}

// TestFSM_Apply_CreateJailCallerIPAddressIgnored confirms the FSM never
// takes ip_address from the caller: with no request the allocator's
// answer wins, and with no network the stored address stays empty.
func TestFSM_Apply_CreateJailCallerIPAddressIgnored(t *testing.T) {
	fsm := newJailIPTestFSM(t)

	withNet := applyJailIPTest(t, fsm, 2, createJailWithIPCmd("jail-1", "net-1", "", "10.60.0.99"))
	noNet := applyJailIPTest(t, fsm, 3, createJailWithIPCmd("jail-2", "", "", "10.60.0.98"))

	if withNet.Error != "" || noNet.Error != "" {
		t.Fatalf("Errors = %q, %q; want none", withNet.Error, noNet.Error)
	}
	if got := withNet.Jail.GetIpAddress(); got != "10.60.0.2" {
		t.Errorf("IpAddress = %q, want allocated 10.60.0.2 not the caller's value", got)
	}
	if got := noNet.Jail.GetIpAddress(); got != "" {
		t.Errorf("IpAddress = %q, want empty for a jail with no network", got)
	}
}

func updateJailCmd(j *internalpb.JailDefinition) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_UpdateJail{UpdateJail: &internalpb.UpdateJail{Jail: j}},
	}
}

// TestFSM_Apply_UpdateJailPreservesIPAddress covers the firewall-rules
// round trip: a jail read back and written whole, without its address
// in the record, must keep the address it was given at creation.
func TestFSM_Apply_UpdateJailPreservesIPAddress(t *testing.T) {
	fsm := newJailIPTestFSM(t)
	if res := applyJailIPTest(t, fsm, 2, createJailWithIPCmd("jail-1", "net-1", "10.60.0.50", "")); res.Error != "" {
		t.Fatalf("create Error = %q", res.Error)
	}

	res := applyJailIPTest(t, fsm, 3, updateJailCmd(&internalpb.JailDefinition{Id: "jail-1", NetworkId: "net-1", Name: "renamed"}))

	if res.Error != "" {
		t.Fatalf("UpdateJail Error = %q, want empty", res.Error)
	}
	stored, _ := fsm.Jail("jail-1")
	if stored.GetIpAddress() != "10.60.0.50" {
		t.Errorf("IpAddress after UpdateJail = %q, want 10.60.0.50 preserved", stored.GetIpAddress())
	}
	if stored.GetName() != "renamed" {
		t.Errorf("Name = %q, want the update applied", stored.GetName())
	}
}

func TestFSM_Apply_UpdateJailRefusesChangedIPAddress(t *testing.T) {
	fsm := newJailIPTestFSM(t)
	if res := applyJailIPTest(t, fsm, 2, createJailWithIPCmd("jail-1", "net-1", "10.60.0.50", "")); res.Error != "" {
		t.Fatalf("create Error = %q", res.Error)
	}

	res := applyJailIPTest(t, fsm, 3, updateJailCmd(&internalpb.JailDefinition{Id: "jail-1", NetworkId: "net-1", RequestedIpAddress: "10.60.0.51"}))

	if res.Error == "" || !strings.Contains(res.Error, "cannot be changed after creation") {
		t.Fatalf("Error = %q, want a not-editable rejection", res.Error)
	}
	stored, _ := fsm.Jail("jail-1")
	if stored.GetIpAddress() != "10.60.0.50" {
		t.Errorf("IpAddress = %q, want unchanged 10.60.0.50", stored.GetIpAddress())
	}
}

func TestFSM_Apply_UpdateJailAcceptsUnchangedRequestedIP(t *testing.T) {
	fsm := newJailIPTestFSM(t)
	if res := applyJailIPTest(t, fsm, 2, createJailWithIPCmd("jail-1", "net-1", "10.60.0.50", "")); res.Error != "" {
		t.Fatalf("create Error = %q", res.Error)
	}

	res := applyJailIPTest(t, fsm, 3, updateJailCmd(&internalpb.JailDefinition{Id: "jail-1", NetworkId: "net-1", RequestedIpAddress: "10.60.0.50"}))

	if res.Error != "" {
		t.Fatalf("Error = %q, want empty for an unchanged address", res.Error)
	}
}

func TestFSM_Apply_UpdateJailRefusesNetworkChangeWhileAddressHeld(t *testing.T) {
	fsm := newJailIPTestFSM(t)
	if res := applyJailIPTest(t, fsm, 2, createJailWithIPCmd("jail-1", "net-1", "", "")); res.Error != "" {
		t.Fatalf("create Error = %q", res.Error)
	}
	if res := applyJailIPTest(t, fsm, 3, createNetworkCmd("net-2", "other", "10.70.0.0/24")); res.Error != "" {
		t.Fatalf("second network Error = %q", res.Error)
	}

	res := applyJailIPTest(t, fsm, 4, updateJailCmd(&internalpb.JailDefinition{Id: "jail-1", NetworkId: "net-2"}))

	if res.Error == "" || !strings.Contains(res.Error, "network_id cannot be changed") {
		t.Fatalf("Error = %q, want a network_id-change rejection", res.Error)
	}
}
