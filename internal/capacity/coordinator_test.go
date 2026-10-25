package capacity

import (
	"strings"
	"testing"
)

// ADR-0153's capacity coordinator. What matters is not that a number
// is stored, it is what the coordinator REFUSES: two concurrent
// provisions each spending the same observed headroom, and a
// provision that would take the aggregate logical commitment past the
// policy's limit.

// testDomain is a small domain, so the arithmetic in a test is easy to
// check by hand.
func testDomain() Domain {
	// The capacity policy is deliberately much larger than the 1 GiB
	// free-space reserve, so a test's "plenty of room" observation is
	// above the reserve and the reserve check is not what the test is
	// exercising.
	return Domain{Name: "pool", CapacityBytes: 4096 * 1024 * 1024}
}

func testObservation(free int64) Observation {
	return Observation{FreeBytes: free}
}

func TestAdmitHoldsAClaimAndTracksItAsInFlight(t *testing.T) {
	c, err := New(2)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	domain := testDomain()

	admission, err := c.Admit(domain, "vm-1", testObservation(8*1024*1024*1024), Claim{
		Domain: domain, GrowthBytes: 10 * 1024 * 1024, VMID: "vm-1",
	})
	if err != nil {
		t.Fatalf("Admit() error: %v", err)
	}

	snapshot := c.Snapshot(domain)
	if snapshot.InFlightBytes != 10*1024*1024 {
		t.Errorf("InFlightBytes = %d, want %d", snapshot.InFlightBytes, 10*1024*1024)
	}
	if snapshot.ActiveBytes != 0 {
		t.Errorf("ActiveBytes = %d, want 0: a claim is in flight, not active", snapshot.ActiveBytes)
	}

	// Committing turns it into an active commitment, and the in-flight
	// claim disappears - a completing provision must not make its
	// commitment disappear.
	admission.Commit("vm-1")
	snapshot = c.Snapshot(domain)
	if snapshot.InFlightBytes != 0 {
		t.Errorf("InFlightBytes = %d, want 0 after commit", snapshot.InFlightBytes)
	}
	if snapshot.ActiveBytes != 10*1024*1024 {
		t.Errorf("ActiveBytes = %d, want %d after commit", snapshot.ActiveBytes, 10*1024*1024)
	}
}

func TestAdmitRefusesWhenTheAggregateWouldExceedThePolicy(t *testing.T) {
	c, err := New(2)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	domain := testDomain()

	// The limit is 2 x 4096 MiB = 8192 MiB of aggregate logical
	// commitment. A 8000 MiB claim fits; a second 1000 MiB claim does
	// not.
	if _, err := c.Admit(domain, "vm-a", testObservation(16*1024*1024*1024), Claim{
		Domain: domain, GrowthBytes: 8000 * 1024 * 1024, VMID: "vm-a",
	}); err != nil {
		t.Fatalf("the first Admit() error: %v", err)
	}

	_, err = c.Admit(domain, "vm-b", testObservation(16*1024*1024*1024), Claim{
		Domain: domain, GrowthBytes: 1000 * 1024 * 1024, VMID: "vm-b",
	})
	if err == nil {
		t.Fatal("a third claim past the aggregate limit was accepted, want a refusal")
	}
	// The error must name everything an operator needs to act.
	for _, want := range []string{"capacity policy", "overcommit ratio", "active commitments", "in-flight claims"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err.Error(), want)
		}
	}
}

func TestAdmitRefusesWhenTheFreeSpaceReserveCannotBeMaintained(t *testing.T) {
	c, err := New(2)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	domain := testDomain()

	// The observed free space is already below the 1 GiB reserve.
	_, err = c.Admit(domain, "vm-1", testObservation(MinFreeSpaceReserveBytes-1), Claim{
		Domain: domain, GrowthBytes: 1024, VMID: "vm-1",
	})
	if err == nil {
		t.Fatal("a claim against a domain already below the reserve was accepted, want a refusal")
	}
	if !strings.Contains(err.Error(), "minimum free-space reserve") {
		t.Errorf("refusal %q does not name the reserve", err.Error())
	}
}

func TestAdmitRefusesWhenTheGrowthWouldEatTheReserve(t *testing.T) {
	c, err := New(2)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	domain := testDomain()

	// 1.5 GiB free, a 1 GiB reserve, and a 1 GiB growth: the reserve
	// cannot survive.
	_, err = c.Admit(domain, "vm-1", testObservation(1536*1024*1024), Claim{
		Domain: domain, GrowthBytes: 1024 * 1024 * 1024, VMID: "vm-1",
	})
	if err == nil {
		t.Fatal("a claim that would eat the reserve was accepted, want a refusal")
	}
	if !strings.Contains(err.Error(), "reserve") {
		t.Errorf("refusal %q does not name the reserve", err.Error())
	}
}

func TestAdmitRefusesASecondClaimForTheSameDisk(t *testing.T) {
	c, err := New(2)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	domain := testDomain()

	claim := Claim{Domain: domain, GrowthBytes: 1024, VMID: "vm-1"}
	if _, err := c.Admit(domain, "vm-1", testObservation(8*1024*1024*1024), claim); err != nil {
		t.Fatalf("the first Admit() error: %v", err)
	}
	if _, err := c.Admit(domain, "vm-1", testObservation(8*1024*1024*1024), claim); err == nil {
		t.Fatal("a second live claim for the same disk was accepted, want a refusal")
	}
}

func TestReleaseRemovesAnActiveCommitment(t *testing.T) {
	c, err := New(2)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	domain := testDomain()

	admission, err := c.Admit(domain, "vm-1", testObservation(8*1024*1024*1024), Claim{
		Domain: domain, GrowthBytes: 10 * 1024 * 1024, VMID: "vm-1",
	})
	if err != nil {
		t.Fatalf("Admit() error: %v", err)
	}
	admission.Commit("vm-1")

	c.Release(domain, "vm-1")
	if got := c.Snapshot(domain).ActiveBytes; got != 0 {
		t.Errorf("ActiveBytes = %d, want 0 after the disk was released", got)
	}
}

func TestRescanDropsCommitmentsForDisksThatNoLongerExist(t *testing.T) {
	c, err := New(2)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	domain := testDomain()

	admission, err := c.Admit(domain, "vm-1", testObservation(8*1024*1024*1024), Claim{
		Domain: domain, GrowthBytes: 10 * 1024 * 1024, VMID: "vm-1",
	})
	if err != nil {
		t.Fatalf("Admit() error: %v", err)
	}
	admission.Commit("vm-1")

	// An authoritative rescan that sees NO disks drops the commitment.
	c.Rescan(domain, map[string]int64{})
	if got := c.Snapshot(domain).ActiveBytes; got != 0 {
		t.Errorf("ActiveBytes = %d, want 0: a rescan that proves the disk is gone must drop its commitment", got)
	}
}

func TestNewRefusesAStrictReservationRatio(t *testing.T) {
	if _, err := New(0.5); err == nil {
		t.Fatal("New(0.5) was accepted, want a refusal: a ratio below 1 is a strict reservation, not thin provisioning")
	}
	if _, err := New(-1); err == nil {
		t.Fatal("New(-1) was accepted, want a refusal")
	}
	c, err := New(0)
	if err != nil {
		t.Fatalf("New(0) error: %v", err)
	}
	if c.Ratio() != DefaultOvercommitRatio {
		t.Errorf("Ratio() = %v, want the default %v", c.Ratio(), DefaultOvercommitRatio)
	}
}
