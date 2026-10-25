// Package capacity implements ADR-0153's node-wide capacity coordinator
// for VM disk provisioning.
//
// It exists because the honest answer to "will this disk fit?" is not a
// single free-space check. Two concurrent Apiary provisions can each
// observe the same free space and each spend it. And a THIN disk's
// logical size is not physical allocation: growing a sparse file's
// length reserves no blocks, so a "committed" size that is only ever
// checked against currently-free space forgets every disk this node
// already provisioned.
//
// So the coordinator keeps a LEDGER of two things per storage domain:
//
//   - ACTIVE commitments: the logical sizes of the thin disks that
//     already exist on this node. A commitment is released only when
//     the disk is deleted, or when an authoritative storage rescan
//     proves the disk is gone.
//
//   - IN-FLIGHT claims: the logical growth a provisioning operation has
//     requested but not yet completed. A claim is held through the
//     copy, the expansion and the final verification; on success it
//     BECOMES an active commitment, and on failure it is RELEASED.
//
// What this does and does not promise is stated plainly, because an
// operator reading a capacity error needs to know which guarantee they
// are getting:
//
//   - It DOES prevent two concurrent Apiary provisions from each
//     spending the same observed headroom.
//
//   - It DOES stop the coordinator from forgetting provisioned thin
//     disks, so a node's total logical commitment is visible rather
//     than implicit.
//
//   - It CANNOT guarantee that future physical writes by guests will
//     fit. A guest writing to its disk, a snapshot, compression, and
//     unrelated host consumers all change the actual allocation AFTER
//     admission.
//
//   - It CANNOT account for unrelated host consumers at all, without an
//     operating-system or storage-pool reservation interface this
//     project does not have.
//
// The 1 GiB free-space reserve is checked INDEPENDENTLY against
// currently available physical capacity. It is never counted as
// overcommit allowance: it is a minimum protected headroom, not a
// budget the coordinator may spend.
package capacity

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// DefaultOvercommitRatio is the default aggregate-logical-commitment
// limit, expressed as a multiple of the storage domain's capacity
// policy.
//
// A ratio of 2 means "the sum of the logical sizes of the thin disks
// provisioned on this domain may be up to twice the domain's capacity
// policy". That is the "configurable thin provisioning with
// overcommit accounting" ADR-0153 chose over strict physical
// reservation, and it is a DEFAULT rather than a hard rule: the owner
// must approve the overcommit setting's semantics, and the limit is
// configurable per node so an operator with a particular storage layout
// can tighten or loosen it.
//
// The semantics this default implies, stated explicitly because the
// ADR asks for them to be documented rather than assumed:
//
//   - The ratio limits AGGREGATE LOGICAL COMMITMENT relative to the
//     capacity policy. It does NOT promise physical space.
//
//   - In-flight claims count toward the aggregate, so a burst of
//     concurrent provisions cannot collectively exceed the ratio.
//
//   - The 1 GiB free-space reserve is checked separately against
//     observed free physical capacity and is never counted as
//     overcommit allowance.
const DefaultOvercommitRatio = 2.0

// MinFreeSpaceReserveBytes is the minimum free physical space kept on
// a storage domain, in bytes.
//
// It is the SAME 1 GiB reserve internal/isostore already enforces for
// uploads (isostore's minFreeSpaceReserveBytes), applied here to the
// filesystem or pool a VM's disk lives on. It is a minimum protected
// headroom, not an extra disk-size cap: a disk larger than the reserve
// is perfectly fine, as long as the reserve itself survives.
const MinFreeSpaceReserveBytes int64 = 1 << 30 // 1 GiB

// Domain is one storage domain: a filesystem or a ZFS pool, identified
// by a name that is stable across calls.
type Domain struct {
	// Name identifies the domain. Two claims against the same name
	// share one ledger.
	Name string

	// CapacityBytes is the domain's capacity policy: the total size the
	// policy considers available for VM disks. It is what the
	// overcommit ratio is measured against.
	//
	// It is a POLICY number, not an observed free space: observed free
	// space is checked separately, and changes as the node's other
	// consumers write.
	CapacityBytes int64
}

// Observation is what a caller reports about a domain's current
// physical state, immediately before it accepts a claim.
type Observation struct {
	// FreeBytes is the currently available physical capacity on the
	// domain.
	FreeBytes int64

	// ObservedAt is when the observation was taken.
	ObservedAt time.Time
}

// Claim is a held reservation of logical growth against a domain.
type Claim struct {
	// Domain is the domain the claim is held against.
	Domain Domain

	// GrowthBytes is the logical growth the claim reserves: the target
	// size MINUS the source virtual size.
	//
	// It is the GROWTH, not the whole target, because the source's own
	// size is already accounted for by whatever else committed it (or,
	// for a base image copied fresh, was never committed at all - the
	// copy is what creates the new commitment).
	GrowthBytes int64

	// VMID is the VM the claim is for, for the error messages.
	VMID string

	// ImageName is the source image, for the error messages.
	ImageName string

	// TargetBytes is the requested final size, for the error messages.
	TargetBytes int64

	// SourceBytes is the source image's virtual size, for the error
	// messages.
	SourceBytes int64
}

// Ledger is the coordinator's per-domain state.
type Ledger struct {
	// active maps a disk key to its committed logical size.
	active map[string]int64

	// claims maps a disk key to the claim currently held against it.
	claims map[string]Claim
}

// Coordinator serializes capacity claims across the node's storage
// domains.
//
// It is safe for concurrent use: every mutation happens under one
// mutex, so two concurrent provisions cannot both observe the same
// free space and both commit against it.
type Coordinator struct {
	mu sync.Mutex

	// ledgers maps a domain name to its ledger.
	ledgers map[string]*Ledger

	// ratio is the aggregate-logical-commitment limit, as a multiple of
	// the domain's capacity policy. Zero means
	// DefaultOvercommitRatio.
	ratio float64

	// reserveBytes is the minimum free physical space to preserve. Zero
	// means MinFreeSpaceReserveBytes.
	reserveBytes int64

	// now is the clock, for tests.
	now func() time.Time
}

// New returns a Coordinator with the given overcommit ratio.
//
// A ratio of zero or less means the default. A ratio below 1 is
// refused: it would mean the aggregate logical commitment may not
// exceed the capacity policy at all, which is a strict reservation
// rather than the thin provisioning ADR-0153 chose, and adopting it by
// accident through a misconfiguration would be worse than refusing.
func New(ratio float64) (*Coordinator, error) {
	if ratio < 0 {
		return nil, fmt.Errorf("capacity: the overcommit ratio cannot be negative (got %v)", ratio)
	}
	if ratio == 0 {
		ratio = DefaultOvercommitRatio
	}
	if ratio < 1 {
		return nil, fmt.Errorf("capacity: an overcommit ratio of %v would be a strict physical reservation, not thin provisioning; use 1 or greater", ratio)
	}
	return &Coordinator{
		ledgers:     map[string]*Ledger{},
		ratio:       ratio,
		reserveBytes: MinFreeSpaceReserveBytes,
		now:         time.Now,
	}, nil
}

// SetReserveBytes overrides the free-space reserve. Zero restores the
// default. It exists so a test can exercise the boundary exactly.
func (c *Coordinator) SetReserveBytes(bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if bytes <= 0 {
		bytes = MinFreeSpaceReserveBytes
	}
	c.reserveBytes = bytes
}

// Ratio returns the coordinator's overcommit ratio.
func (c *Coordinator) Ratio() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ratio
}

// ReserveBytes returns the coordinator's free-space reserve.
func (c *Coordinator) ReserveBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reserveBytes
}

// ledgerFor returns the ledger for a domain, creating it if needed. The
// caller must hold c.mu.
func (c *Coordinator) ledgerFor(name string) *Ledger {
	ledger, ok := c.ledgers[name]
	if !ok {
		ledger = &Ledger{active: map[string]int64{}, claims: map[string]Claim{}}
		c.ledgers[name] = ledger
	}
	return ledger
}

// ClaimAdmission is the result of a successful admission.
type ClaimAdmission struct {
	// Claim is the held claim. It must be released or committed.
	Claim Claim

	// Release releases the claim without committing it.
	Release func()

	// Commit turns the claim into an active commitment. The disk key
	// is the identity the commitment is tracked under - the VM's
	// PHYSICAL identity (ADR-0152), because that is what names the
	// disk on disk.
	Commit func(diskKey string)
}

// Admit checks the capacity policy and the free-space reserve, and
// holds a claim against the domain.
//
// It fails, WITHOUT holding anything, when:
//
//   - the aggregate logical commitment (active plus in-flight) would
//     exceed the ratio's limit against the domain's capacity policy;
//   - the observed free physical capacity cannot preserve the reserve.
//
// The error names the policy ratio, the active commitments, the
// in-flight claims and the observed free capacity, because an operator
// reading a capacity error needs all four to know what to change.
func (c *Coordinator) Admit(domain Domain, diskKey string, observation Observation, claim Claim) (*ClaimAdmission, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ledger := c.ledgerFor(domain.Name)

	// A second claim for the same disk replaces the first only after
	// the first is released: two live claims for one disk would
	// double-count it. This is refused rather than silently replaced,
	// because a double-count would under-report the aggregate.
	if _, held := ledger.claims[diskKey]; held {
		return nil, fmt.Errorf("capacity: a claim for %q on storage domain %q is already in flight", diskKey, domain.Name)
	}

	activeBytes := int64(0)
	for _, size := range ledger.active {
		activeBytes += size
	}
	inflightBytes := int64(0)
	for _, held := range ledger.claims {
		inflightBytes += held.GrowthBytes
	}

	// The overcommit limit: aggregate logical commitment relative to
	// the capacity policy.
	limit := float64(domain.CapacityBytes) * c.ratio
	committed := float64(activeBytes + inflightBytes + claim.GrowthBytes)
	if committed > limit {
		return nil, fmt.Errorf(
			"capacity: provisioning VM %q from image %q on storage domain %q would commit %d bytes of logical growth, taking the aggregate logical commitment to %d bytes, which exceeds this domain's capacity policy of %d bytes at an overcommit ratio of %v (active commitments %d bytes, in-flight claims %d bytes). Check the target node's available storage, raise the domain's capacity policy, or lower the requested disk size",
			claim.VMID, claim.ImageName, domain.Name, claim.GrowthBytes, activeBytes+inflightBytes+claim.GrowthBytes,
			domain.CapacityBytes, c.ratio, activeBytes, inflightBytes)
	}

	// The free-space reserve: checked INDEPENDENTLY against observed
	// physical capacity, and never counted as overcommit allowance.
	// The reserve is a floor on what must REMAIN free, so the growth
	// must fit in what is free MINUS the reserve.
	if observation.FreeBytes < c.reserveBytes {
		return nil, fmt.Errorf(
			"capacity: storage domain %q has only %d bytes free, which is below the %d byte minimum free-space reserve, so VM %q from image %q cannot be provisioned. Check the target node's available storage",
			domain.Name, observation.FreeBytes, c.reserveBytes, claim.VMID, claim.ImageName)
	}
	if claim.GrowthBytes > observation.FreeBytes-c.reserveBytes {
		return nil, fmt.Errorf(
			"capacity: provisioning VM %q from image %q on storage domain %q needs %d bytes of additional space, but only %d bytes are free beyond the %d byte minimum free-space reserve (observed free capacity %d bytes). Check the target node's available storage",
			claim.VMID, claim.ImageName, domain.Name, claim.GrowthBytes, observation.FreeBytes-c.reserveBytes, c.reserveBytes, observation.FreeBytes)
	}

	ledger.claims[diskKey] = claim

	return &ClaimAdmission{
		Claim: claim,
		Release: func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			delete(ledger.claims, diskKey)
		},
		Commit: func(committedKey string) {
			c.mu.Lock()
			defer c.mu.Unlock()
			delete(ledger.claims, diskKey)
			ledger.active[committedKey] = claim.GrowthBytes
		},
	}, nil
}

// Release removes an active commitment, for the case where the disk it
// was for has been deleted.
func (c *Coordinator) Release(domain Domain, diskKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ledger := c.ledgerFor(domain.Name)
	delete(ledger.active, diskKey)
	delete(ledger.claims, diskKey)
}

// Rescan replaces the active commitments for a domain with the set a
// storage rescan actually observed.
//
// It is how a commitment is removed when the disk it was for turns out
// not to exist: an authoritative storage rescan proving the disk is
// gone is the ONLY other way a commitment may be dropped. It is never
// dropped because a provision "probably" failed.
func (c *Coordinator) Rescan(domain Domain, observed map[string]int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ledger := c.ledgerFor(domain.Name)
	for key := range ledger.active {
		if _, present := observed[key]; !present {
			delete(ledger.active, key)
		}
	}
	for key, size := range observed {
		ledger.active[key] = size
	}
}

// Snapshot describes a domain's current ledger, for an operator-facing
// view.
type Snapshot struct {
	// Domain is the domain the snapshot describes.
	Domain Domain

	// ActiveBytes is the sum of the active commitments.
	ActiveBytes int64

	// InFlightBytes is the sum of the in-flight claims.
	InFlightBytes int64

	// LimitBytes is the aggregate limit the ratio implies.
	LimitBytes int64

	// Ratio is the coordinator's overcommit ratio.
	Ratio float64

	// ReserveBytes is the free-space reserve.
	ReserveBytes int64

	// ActiveKeys lists the active commitment keys, sorted.
	ActiveKeys []string
}

// Snapshot returns the current ledger state for a domain.
func (c *Coordinator) Snapshot(domain Domain) Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	ledger := c.ledgerFor(domain.Name)

	activeBytes := int64(0)
	keys := make([]string, 0, len(ledger.active))
	for key, size := range ledger.active {
		activeBytes += size
		keys = append(keys, key)
	}
	sort.Strings(keys)

	inflightBytes := int64(0)
	for _, held := range ledger.claims {
		inflightBytes += held.GrowthBytes
	}

	return Snapshot{
		Domain:       domain,
		ActiveBytes:  activeBytes,
		InFlightBytes: inflightBytes,
		LimitBytes:   int64(float64(domain.CapacityBytes) * c.ratio),
		Ratio:        c.ratio,
		ReserveBytes: c.reserveBytes,
		ActiveKeys:   keys,
	}
}
