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

func TestShortHash_TruncatesALongDigestKeepingHeadAndTail(t *testing.T) {
	sha256 := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85"
	got := shortHash(sha256)
	if got != "e3b0c442...b7852b85" {
		t.Errorf("shortHash(%q) = %q, want a truncated head...tail form", sha256, got)
	}
}

func TestShortHash_ShortValueIsUnchanged(t *testing.T) {
	if got := shortHash("abc123"); got != "abc123" {
		t.Errorf("shortHash(short) = %q, want the value unchanged", got)
	}
}

func TestFromRPCISO_FormatsSizeAndKeepsFullHashAlongsideShortened(t *testing.T) {
	v := fromRPCISO(&rpcpb.ISOInfo{Name: "freebsd.iso", SizeBytes: 5242880, Sha256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85"})
	if v.Size != "5.00 MB" {
		t.Errorf("Size = %q, want a human-readable size", v.Size)
	}
	if v.SHA256 != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85" {
		t.Errorf("SHA256 = %q, want the full digest preserved for copying", v.SHA256)
	}
	if v.SHA256Short == v.SHA256 {
		t.Errorf("SHA256Short should be a shortened form of SHA256, got the same value")
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
