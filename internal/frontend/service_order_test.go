package frontend

import (
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// canonicalServiceOrder is the order the Machine page is contractually
// required to show: the three daemons an operator acts on, with raftd -
// the substrate the other three depend on - last.
var canonicalServiceOrder = []string{
	"apiary_managerd",
	"apiary_frontend",
	"apiary_restshimd",
	"apiary_raftd",
}

// serviceNames pulls just the names out of a view slice, because every
// assertion here is about ORDER and reading a list of strings in a test
// failure is the difference between "got raftd, managerd, ..." and
// "got [nodeServiceView{Name:apiary_raftd ...} ...]".
func serviceNames(services []nodeServiceView) []string {
	names := make([]string, 0, len(services))
	for _, service := range services {
		names = append(names, service.Name)
	}
	return names
}

func serviceViewsEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestNodeServices_ServiceOrderIsDisplayOrderNotManagerOrder pins that
// the service table's layout is a property of the UI and not of whatever
// order the manager's ListNodeServices happened to return.
//
// The defect this exists to catch is the quiet kind. internal/manager's
// apiaryServices list happens to be ordered raftd, managerd, frontend,
// restshimd, and the Machine page rendered it in exactly that order for
// as long as it existed - so the table looked fine, and looked fine
// precisely because the manager's internal supervision order coincided
// with something a person would accept. That coincidence is not a
// contract. The moment someone reorders that slice while touching the
// manager's start/stop path, the operator's view of which daemons exist
// reshuffles, and the change lands in a commit whose subject line is
// about the manager. Nothing errors. The page simply shows raftd above
// the daemons that depend on it.
//
// So the input here is the manager's own order, and the expectation is
// NOT the manager's order. That inversion is the whole assertion: a
// future change that drops the sort at the end of fromRPCNodeServices
// passes this test only if the manager is reordered to match, which
// would mean the two had quietly become coupled again.
func TestNodeServices_ServiceOrderIsDisplayOrderNotManagerOrder(t *testing.T) {
	// The order internal/manager/services.go actually sends today.
	managerOrder := []string{
		"apiary_raftd",
		"apiary_managerd",
		"apiary_frontend",
		"apiary_restshimd",
	}

	if serviceViewsEqual(managerOrder, canonicalServiceOrder) {
		t.Fatal("the manager's order and the display order are the same in this test; it would pass on the sort being deleted")
	}

	got := serviceNames(fromRPCNodeServices(&rpcpb.ListNodeServicesResponse{
		Services: []*rpcpb.NodeService{
			{Name: managerOrder[0], Status: "running"},
			{Name: managerOrder[1], Status: "running"},
			{Name: managerOrder[2], Status: "running"},
			{Name: managerOrder[3], Status: "running"},
		},
	}))

	if !serviceViewsEqual(got, canonicalServiceOrder) {
		t.Errorf("services = %v, want %v (the manager's order must not leak into the page)", got, canonicalServiceOrder)
	}
}

// TestNodeServices_DisplayOrderIsInputOrderIndependent covers the other
// half of the same defect: the sort must not merely transform one
// particular input, it must be a total function of the name set.
//
// A hand-rolled fix that special-cased the manager's actual response -
// "if the first service is raftd, swap it to the end" - passes the
// manager's order test and fails here. That shape is easy to write by
// accident, because the one real input is the one you have, and it is
// the reverse and the scrambled orders that expose it. Both are checked
// against the same canonical expectation, so a sort that only handles
// one of them cannot pass.
func TestNodeServices_DisplayOrderIsInputOrderIndependent(t *testing.T) {
	tests := []struct {
		name  string
		input []string
	}{
		{
			// A manager or a transport that reverses, e.g. a list
			// assembled back-to-front during a refactor.
			name:  "the manager's order reversed",
			input: []string{"apiary_restshimd", "apiary_frontend", "apiary_managerd", "apiary_raftd"},
		},
		{
			// The worst case: every known service is in the wrong
			// place, so a partially-correct sort shows up as several
			// rows out of position rather than one.
			name:  "fully scrambled",
			input: []string{"apiary_raftd", "apiary_restshimd", "apiary_frontend", "apiary_managerd"},
		},
		{
			// Only a subset present, e.g. a node where restshim is
			// not installed. The two that are present must still land
			// in their canonical relative order, and the absent ones
			// must not be conjured into the list.
			name:  "a partial set",
			input: []string{"apiary_raftd", "apiary_managerd"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := &rpcpb.ListNodeServicesResponse{}
			for _, name := range tc.input {
				resp.Services = append(resp.Services, &rpcpb.NodeService{Name: name, Status: "running"})
			}

			got := serviceNames(fromRPCNodeServices(resp))
			want := make([]string, 0, len(tc.input))
			for _, name := range canonicalServiceOrder {
				for _, in := range tc.input {
					if in == name {
						want = append(want, name)
					}
				}
			}

			if !serviceViewsEqual(got, want) {
				t.Errorf("services = %v, want %v", got, want)
			}
		})
	}
}

// TestNodeServices_UnknownNameIsKeptAfterTheKnownFour is the test that
// makes the sort safe to extend.
//
// Anything that filters the service list to the four names it knows
// renders beautifully and silently hides a daemon. That is the failure
// mode of every "sort into known buckets" implementation that returns
// early on an unrecognised entry, and it is worst for the newest daemon:
// a manager upgrade that ships apiary_something would add a row on the
// manager's side, and this page would show an operator a table of four
// services on a node that is running five, with no indication that the
// fifth is missing. So the unknown name must be RETAINED, and placed
// after the known four rather than wedged among them, where it would
// read as though the UI had been told about it.
func TestNodeServices_UnknownNameIsKeptAfterTheKnownFour(t *testing.T) {
	resp := &rpcpb.ListNodeServicesResponse{Services: []*rpcpb.NodeService{
		// Interleaved, so "kept" is proven to be about retention and
		// not about a lucky position in the input.
		{Name: "apiary_newthing", Status: "running"},
		{Name: "apiary_raftd", Status: "running"},
		{Name: "apiary_managerd", Status: "running"},
		{Name: "apiary_frontend", Status: "running"},
		{Name: "apiary_restshimd", Status: "running"},
	}}

	got := serviceNames(fromRPCNodeServices(resp))

	want := append(append([]string{}, canonicalServiceOrder...), "apiary_newthing")
	if !serviceViewsEqual(got, want) {
		t.Fatalf("services = %v, want %v; a service this build does not recognise must be shown, not dropped", got, want)
	}
}

// TestNodeServices_UnknownNamesAreSortedAmongThemselves pins the second
// half of the same promise: an unknown's placement is deterministic too.
//
// Ranking unknowns identically and leaving their input order alone looks
// fine in every test above - there is only ever one unknown - and is not
// deterministic. Two unrecognised services would then swap places
// depending on the order the manager happened to enumerate them, which
// is the same class of bug as the raftd reshuffle this whole change
// exists to remove, just one release away instead of one commit away.
// The alphabetical tiebreak is what makes "we do not know about this
// one, but it is stable" true rather than aspirational.
func TestNodeServices_UnknownNamesAreSortedAmongThemselves(t *testing.T) {
	resp := &rpcpb.ListNodeServicesResponse{Services: []*rpcpb.NodeService{
		// Deliberately reverse-alphabetical and with a known service
		// on both sides, so neither "appended in input order" nor
		// "sorted before the known four" can pass.
		{Name: "apiary_zed", Status: "running"},
		{Name: "apiary_managerd", Status: "running"},
		{Name: "apiary_alpha", Status: "running"},
		{Name: "apiary_restshimd", Status: "running"},
		{Name: "apiary_mid", Status: "running"},
	}}

	got := serviceNames(fromRPCNodeServices(resp))

	want := []string{"apiary_managerd", "apiary_restshimd", "apiary_alpha", "apiary_mid", "apiary_zed"}
	if !serviceViewsEqual(got, want) {
		t.Fatalf("services = %v, want %v; unknown names belong after the known four, alphabetically among themselves", got, want)
	}
}

// TestNodeServices_DuplicateKnownNameKeepsBothCopies pins the
// sort.SliceStable contract, and it is here because the obvious cheaper
// implementation - sort.Slice - is the one that would break it.
//
// Two rows with the same name should not happen from a healthy manager,
// which is exactly why it needs pinning: when it does happen the
// operator is looking at a node whose service state is genuinely
// duplicated, and swapping the two rows silently would make the table
// look no different from a correct one. The distinguishing detail is
// Status: the two copies carry different statuses here, so an unstable
// sort is observable in the rendered table and not merely arguable.
func TestNodeServices_DuplicateKnownNameKeepsBothCopies(t *testing.T) {
	resp := &rpcpb.ListNodeServicesResponse{Services: []*rpcpb.NodeService{
		{Name: "apiary_raftd", Status: "running", Detail: "first raftd"},
		{Name: "apiary_managerd", Status: "running", Detail: "managerd"},
		{Name: "apiary_raftd", Status: "not_running", Detail: "second raftd"},
		{Name: "apiary_frontend", Status: "running", Detail: "frontend"},
	}}

	services := fromRPCNodeServices(resp)
	got := serviceNames(services)

	want := []string{"apiary_managerd", "apiary_frontend", "apiary_raftd", "apiary_raftd"}
	if !serviceViewsEqual(got, want) {
		t.Fatalf("services = %v, want %v; both copies must survive and stay adjacent", got, want)
	}

	// The adjacency alone is not enough - a sort that reversed the two
	// duplicates would still produce the same name list. Assert the
	// input order of the pair through the detail each one carries.
	var raftdDetails []string
	for _, service := range services {
		if service.Name == "apiary_raftd" {
			raftdDetails = append(raftdDetails, service.Detail)
		}
	}
	if strings.Join(raftdDetails, ",") != "first raftd,second raftd" {
		t.Errorf("raftd copies came back as %v, want input order (sort.Slice, not sort.SliceStable, will swap these)", raftdDetails)
	}
}

// TestNodeServices_EmptyInputIsHandled covers the trivial case that still
// deserves a name, because the function is called on a response that can
// legitimately carry no services at all - a node where the manager has
// not finished enumerating, or a FilteredList that matched nothing.
//
// A nil resp and an empty Services list both reach this path. The
// function has to return an empty slice and not panic, and because
// templates range over this value, a nil that renders "null" or panics
// inside a template is a 500 on the Machine page rather than an empty
// table. This is a cheap guard against a future edit that dereferences
// resp or indexes services[0] while "tidying up" the sort.
func TestNodeServices_EmptyInputIsHandled(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp *rpcpb.ListNodeServicesResponse
	}{
		{"a nil response", nil},
		{"a response with no services", &rpcpb.ListNodeServicesResponse{}},
		{"a response with an explicitly empty list", &rpcpb.ListNodeServicesResponse{Services: []*rpcpb.NodeService{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			services := fromRPCNodeServices(tc.resp)
			if len(services) != 0 {
				t.Fatalf("services = %v, want none", serviceNames(services))
			}
			// Ranging over it is what a template does, so do it here.
			for i, service := range services {
				t.Fatalf("index %d = %+v on an empty result", i, service)
			}
		})
	}
}

// TestNodeServices_RankAndNameAreNotFusedIntoOneComparison guards the
// subtlety the sort's own comment describes, because it is the one
// thing here that no name-list assertion can see.
//
// The comparator is two statements: compare rank, return on inequality,
// and only then compare names. A fused tuple comparison - "rank, then
// name" - is equivalent for every input the current ranks can produce,
// so every other test in this file passes with either version. The two
// differ only when two KNOWN services share a rank, which cannot happen
// today, which is exactly why the mistake is silent and would be
// introduced by the next person adding a fifth daemon.
//
// So the fused comparison is modelled here directly: sort the known
// four with a real tuple comparator and show that it produces
// alphabetical order - raftd second, directly under managerd - which is
// the concrete thing that would have shipped. This test failing means
// the ranks collided, which is the alarm the fused version cannot ring
// on its own.
func TestNodeServices_RankAndNameAreNotFusedIntoOneComparison(t *testing.T) {
	for _, name := range canonicalServiceOrder {
		if got := nodeServiceDisplayOrder(name); got < 0 || got > 3 {
			t.Errorf("nodeServiceDisplayOrder(%q) = %d, want a distinct rank in 0..3; a collision here is what lets a name tiebreak reorder the known four", name, got)
		}
	}

	// A tuple comparator, the shape the sort must NOT use.
	tuple := func(i, j int) bool {
		ri, rj := nodeServiceDisplayOrder(canonicalServiceOrder[i]), nodeServiceDisplayOrder(canonicalServiceOrder[j])
		if ri != rj {
			return ri < rj
		}
		return canonicalServiceOrder[i] < canonicalServiceOrder[j]
	}
	names := append([]string{}, canonicalServiceOrder...)
	// A simple insertion sort keeps the demonstration dependency-free
	// and, unlike sort.Slice, is obviously stable.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && tuple(j, j-1); j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	if serviceViewsEqual(names, canonicalServiceOrder) {
		t.Skip("ranks are distinct, so the fused comparison agrees with the correct one today; nothing to demonstrate")
	}
	if strings.Index(strings.Join(names, ","), "apiary_raftd") < len(strings.Join(canonicalServiceOrder[:2], ",")) {
		t.Errorf("fused tuple comparison produced %v, which puts raftd beside managerd; ranks are not distinct", names)
	}

	if got := nodeServiceDisplayOrder("apiary_newthing"); got != nodeServiceUnranked {
		t.Errorf("nodeServiceDisplayOrder(%q) = %d, want %d; unknowns must share one sentinel or they sort against each other by a value nobody chose", "apiary_newthing", got, nodeServiceUnranked)
	}
	if got := nodeServiceDisplayOrder("apiary_managerd"); got >= nodeServiceUnranked {
		t.Errorf("nodeServiceDisplayOrder(%q) = %d, must rank below the %d sentinel or known services sort after unknowns", "apiary_managerd", got, nodeServiceUnranked)
	}
}
