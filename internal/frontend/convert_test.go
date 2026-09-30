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

func TestSortVMs_DefaultGroupsByHiveThenName(t *testing.T) {
	vms := []vmView{
		{ID: "stopped", Name: "a", NodeID: "apiarium", DesiredState: "stopped", Phase: "stopped"},
		{ID: "worker", Name: "worker", NodeID: "apiverse", DesiredState: "running", Phase: "ready"},
		{ID: "control", Name: "control", NodeID: "apiarium", DesiredState: "running", Phase: "ready"},
		{ID: "legacy", Name: "legacy", NodeID: "apiarium", Phase: "ready"},
	}
	sortVMs(vms, "", "asc")

	// The stopped VM sorts among its Hive-mates by name ("a" leads), not
	// into a tier of its own: state is a rendered column, not a sort key,
	// so a phase change must not move the row.
	if got, want := idsOf(vms), []string{"stopped", "control", "legacy", "worker"}; !equalStrings(got, want) {
		t.Errorf("default order = %v, want %v", got, want)
	}
}

// A Cell's state changing must not move its row. The list re-sorts on
// every three-second poll, so any ordering that depends on phase or
// desired state makes rows jump out from under the operator.
func TestSortVMs_DefaultOrderIsUnchangedByStateChanges(t *testing.T) {
	sort := func(vms []vmView) []string {
		sortVMs(vms, "", "asc")
		return idsOf(vms)
	}
	before := sort([]vmView{
		{ID: "a", Name: "a", NodeID: "apiarium", DesiredState: "running", Phase: "ready"},
		{ID: "b", Name: "b", NodeID: "apiarium", DesiredState: "stopped", Phase: "stopped"},
		{ID: "c", Name: "c", NodeID: "apiverse", DesiredState: "running", Phase: "ready"},
	})
	after := sort([]vmView{
		{ID: "a", Name: "a", NodeID: "apiarium", DesiredState: "running", Phase: "creating"},
		{ID: "b", Name: "b", NodeID: "apiarium", DesiredState: "running", Phase: "ready"},
		{ID: "c", Name: "c", NodeID: "apiverse", DesiredState: "stopped", Phase: "stopped"},
	})
	if !equalStrings(before, after) {
		t.Errorf("order changed with state alone: %v then %v", before, after)
	}
}

// Two Cells on one Hive can share a Name, and a VM created before names
// existed has none at all. ID is the final tiebreak, so those rows are
// still totally ordered rather than left in fetch order.
func TestSortVMs_DefaultOrderIsTotalOnRepeatedNameAndEmptyName(t *testing.T) {
	vms := []vmView{
		{ID: "vm-b", Name: "web", NodeID: "apiarium"},
		{ID: "vm-a", Name: "web", NodeID: "apiarium"},
		{ID: "vm-d", NodeID: "apiarium"},
		{ID: "vm-c", NodeID: "apiarium"},
	}
	sortVMs(vms, "", "asc")

	// Unnamed Cells sort first (empty name < any name), then by ID.
	if got, want := idsOf(vms), []string{"vm-c", "vm-d", "vm-a", "vm-b"}; !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// The default order is a consequence of the data, not of anything the
// operator picked, so ?dir= must not invert it.
func TestSortVMs_DefaultIgnoresDir(t *testing.T) {
	asc := []vmView{{ID: "a", Name: "a", NodeID: "apiarium"}, {ID: "b", Name: "b", NodeID: "apiverse"}}
	desc := []vmView{{ID: "a", Name: "a", NodeID: "apiarium"}, {ID: "b", Name: "b", NodeID: "apiverse"}}
	sortVMs(asc, "", "asc")
	sortVMs(desc, "", "desc")

	if !equalStrings(idsOf(asc), idsOf(desc)) {
		t.Errorf("dir=desc changed the default order: %v then %v", idsOf(asc), idsOf(desc))
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

func TestSortJails_GroupsByHiveThenName(t *testing.T) {
	jails := []jailView{
		{ID: "stopped", Name: "a", NodeID: "apiarium", DesiredState: "stopped", Phase: "stopped"},
		{ID: "worker", Name: "worker", NodeID: "apiverse", DesiredState: "running", Phase: "ready"},
		{ID: "control", Name: "control", NodeID: "apiarium", DesiredState: "running", Phase: "ready"},
	}
	sortJails(jails)
	got := []string{jails[0].ID, jails[1].ID, jails[2].ID}
	if want := []string{"stopped", "control", "worker"}; !equalStrings(got, want) {
		t.Errorf("jail order = %v, want %v", got, want)
	}
}

// The VM and jail lists are the same operator question asked of two Cell
// types; they must not disagree about how Cells are ordered.
func TestSortVMs_And_SortJails_AgreeOnOrder(t *testing.T) {
	vms := []vmView{
		{ID: "v2", Name: "web", NodeID: "apiverse"},
		{ID: "v1", Name: "db", NodeID: "apiarium"},
		{ID: "v3", Name: "api", NodeID: "apiarium"},
	}
	jails := []jailView{
		{ID: "v2", Name: "web", NodeID: "apiverse"},
		{ID: "v1", Name: "db", NodeID: "apiarium"},
		{ID: "v3", Name: "api", NodeID: "apiarium"},
	}
	sortVMs(vms, "", "asc")
	sortJails(jails)
	got := []string{jails[0].ID, jails[1].ID, jails[2].ID}

	if want := idsOf(vms); !equalStrings(got, want) {
		t.Errorf("jail order %v disagrees with VM order %v", got, want)
	}
	if want := []string{"v3", "v1", "v2"}; !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}
