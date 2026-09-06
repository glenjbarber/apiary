package frontend

import (
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestFromRPCNodeConfig_RetainsUnavailableSelections(t *testing.T) {
	view := fromRPCNodeConfig(&rpcpb.GetNodeConfigResponse{
		Uplink:    "bridge999",
		NatUplink: "bridge999",
		AvailableInterfaces: []*rpcpb.NetworkInterface{
			{Name: "em0", Up: true, Addresses: []string{"10.50.0.9/24"}},
		},
	})
	if len(view.UplinkOptions) != 2 || view.UplinkOptions[1].Label != "bridge999 (saved, unavailable)" || !view.UplinkOptions[1].Selected {
		t.Errorf("UplinkOptions = %+v, want discovered em0 and selected unavailable bridge999", view.UplinkOptions)
	}
	if len(view.NATUplinkOptions) != 2 || view.NATUplinkOptions[1].Label != "bridge999 (saved, unavailable)" || !view.NATUplinkOptions[1].Selected {
		t.Errorf("NATUplinkOptions = %+v, want discovered em0 and selected unavailable bridge999", view.NATUplinkOptions)
	}
}

func idsOf(vms []vmView) []string {
	ids := make([]string, len(vms))
	for i, v := range vms {
		ids[i] = v.ID
	}
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSortVMs_ByIDAscendingIsCaseInsensitive(t *testing.T) {
	vms := []vmView{{ID: "web-2"}, {ID: "Api-1"}, {ID: "db-3"}}
	sortVMs(vms, "id", "asc")

	if got, want := idsOf(vms), []string{"Api-1", "db-3", "web-2"}; !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestSortVMs_ByIDDescending(t *testing.T) {
	vms := []vmView{{ID: "a"}, {ID: "c"}, {ID: "b"}}
	sortVMs(vms, "id", "desc")

	if got, want := idsOf(vms), []string{"c", "b", "a"}; !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestSortVMs_ByNode(t *testing.T) {
	vms := []vmView{
		{ID: "vm-1", NodeID: "node-b"},
		{ID: "vm-2", NodeID: "node-a"},
	}
	sortVMs(vms, "node", "asc")

	if got, want := idsOf(vms), []string{"vm-2", "vm-1"}; !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestSortVMs_ByState(t *testing.T) {
	vms := []vmView{
		{ID: "vm-1", Phase: "ready"},
		{ID: "vm-2", Phase: "creating"},
	}
	sortVMs(vms, "state", "asc")

	if got, want := idsOf(vms), []string{"vm-2", "vm-1"}; !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestSortVMs_TiesFallBackToID(t *testing.T) {
	vms := []vmView{
		{ID: "vm-b", Phase: "ready"},
		{ID: "vm-a", Phase: "ready"},
	}
	sortVMs(vms, "state", "asc")

	if got, want := idsOf(vms), []string{"vm-a", "vm-b"}; !equalStrings(got, want) {
		t.Errorf("order = %v, want %v (tie on state should fall back to ID)", got, want)
	}
}

func TestSortVMs_UnknownSortByFallsBackToID(t *testing.T) {
	vms := []vmView{{ID: "b"}, {ID: "a"}}
	sortVMs(vms, "bogus", "asc")

	if got, want := idsOf(vms), []string{"a", "b"}; !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestSortVMs_RunningGroupsByHiveThenName(t *testing.T) {
	vms := []vmView{
		{ID: "stopped", Name: "a", NodeID: "apiarium", DesiredState: "stopped", Phase: "stopped"},
		{ID: "worker", Name: "worker", NodeID: "apiverse", DesiredState: "running", Phase: "ready"},
		{ID: "control", Name: "control", NodeID: "apiarium", DesiredState: "running", Phase: "ready"},
		{ID: "legacy", Name: "legacy", NodeID: "apiarium", Phase: "ready"},
	}
	sortVMs(vms, "running", "asc")

	if got, want := idsOf(vms), []string{"control", "legacy", "worker", "stopped"}; !equalStrings(got, want) {
		t.Errorf("operational order = %v, want %v", got, want)
	}
}

func TestSortJails_RunningGroupsByHiveThenName(t *testing.T) {
	jails := []jailView{
		{ID: "stopped", Name: "a", NodeID: "apiarium", DesiredState: "stopped", Phase: "stopped"},
		{ID: "worker", Name: "worker", NodeID: "apiverse", DesiredState: "running", Phase: "ready"},
		{ID: "control", Name: "control", NodeID: "apiarium", DesiredState: "running", Phase: "ready"},
	}
	sortJails(jails)
	got := []string{jails[0].ID, jails[1].ID, jails[2].ID}
	if want := []string{"control", "worker", "stopped"}; !equalStrings(got, want) {
		t.Errorf("operational order = %v, want %v", got, want)
	}
}
