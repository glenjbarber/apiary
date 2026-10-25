package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/glenjbarber/apiary/internal/capacity"
	"github.com/glenjbarber/apiary/internal/isostore"
)

// ADR-0153's disk expansion: growing a VM's OWN copy of a base image
// to a requested size, without ever touching the uploaded image.
//
// The ordering is the whole design and is not negotiable:
//
//  1. Resolve and validate the image, and measure its virtual size.
//  2. Admit a capacity claim against the storage domain, holding the
//     free-space reserve and the overcommit accounting.
//  3. Create the destination and copy/decompress the image into it.
//  4. Recheck the free capacity (a long copy may have consumed it).
//  5. Expand the destination to the target size.
//  6. Verify the destination's virtual size is EXACTLY the target.
//  7. Commit the claim, or release it and remove the destination.
//
// ANY failure between 3 and 6 removes the incomplete destination disk.
// A partially initialized VM must never be reported ready.

// expandDiskImage expands an already-copied VM disk to targetSizeMB.
//
// sourceVirtualBytes is the source image's measured virtual size, and
// sourceFormat its format ("raw" or "qcow2"). A sourceVirtualBytes of
// zero means UNKNOWN, which is refused: the expansion's arithmetic
// depends on knowing the source's size, and guessing it would either
// shrink the disk or expand it by the wrong amount.
func (r *Reconciler) expandDiskImage(ctx context.Context, path string, targetSizeMB uint64, sourceVirtualBytes int64, sourceFormat string) error {
	vmID := filepath.Base(filepath.Dir(path))

	if sourceVirtualBytes <= 0 {
		return fmt.Errorf("expanding the disk for VM %q: the source image's virtual size is unknown, so it cannot be expanded safely; re-upload the image so it can be measured", vmID)
	}
	targetBytes, err := isostore.MibToBytes(targetSizeMB)
	if err != nil {
		return fmt.Errorf("expanding the disk for VM %q: %w", vmID, err)
	}
	if targetBytes < sourceVirtualBytes {
		return fmt.Errorf("expanding the disk for VM %q: the requested size of %d MiB is smaller than the source image's virtual size of %d bytes; a disk is never shrunk", vmID, targetSizeMB, sourceVirtualBytes)
	}

	// The capacity admission. It is taken BEFORE the copy, so a
	// provision that cannot fit is refused before it spends time
	// copying a multi-gigabyte image.
	admission, domain, err := r.admitDiskCapacity(vmID, targetBytes, sourceVirtualBytes)
	if err != nil {
		return err
	}
	release := true
	defer func() {
		// admission is nil when this node has no capacity coordinator
		// configured (admitDiskCapacity's no-op case). There is no claim
		// to release in that case, so there is nothing to release - and
		// dereferencing the nil pointer would panic on the way out of
		// every expansion on a node that never opted into the
		// accounting.
		if release && admission != nil {
			admission.Release()
		}
	}()

	// Recheck the free capacity immediately before the expansion: a
	// long copy may have consumed headroom this node did not have when
	// the claim was taken.
	if err := r.recheckDiskCapacity(domain, vmID, targetBytes, sourceVirtualBytes); err != nil {
		return err
	}

	switch sourceFormat {
	case string(isostore.DiskFormatQcow2):
		if err := isostore.ExpandQcow2Disk(path, sourceVirtualBytes, targetBytes); err != nil {
			return fmt.Errorf("expanding the qcow2 disk for VM %q to %d MiB: %w", vmID, targetSizeMB, err)
		}
	case string(isostore.DiskFormatRaw), "":
		if err := isostore.ExpandRawDisk(path, sourceVirtualBytes, targetBytes); err != nil {
			return fmt.Errorf("expanding the raw disk for VM %q to %d MiB: %w", vmID, targetSizeMB, err)
		}
	default:
		return fmt.Errorf("expanding the disk for VM %q: the source image's format %q is not supported for expansion; expansion is implemented for raw and qcow2 images only, and an unknown format is never treated as raw", vmID, sourceFormat)
	}

	// The final verification: the disk's virtual size must be EXACTLY
	// what was asked for, before it is handed to bhyve.
	if err := isostore.VerifyVirtualSize(path, targetBytes); err != nil {
		return fmt.Errorf("expanding the disk for VM %q: the disk's size could not be verified after expansion: %w", vmID, err)
	}

	// Same no-op case as the deferred release above: with no capacity
	// coordinator there is no claim to commit.
	if admission != nil {
		admission.Commit(diskCommitmentKey(vmID))
	}
	release = false
	return nil
}

// effectiveDiskSizeMB resolves the size a VM's disk should be created
// at, in MiB.
//
// It is the MAXIMUM of three numbers, exactly as ADR-0148 specifies:
//
//   - the Colony-wide floor (replicated, and read from this node's own
//     FSM copy so every Comb computes the same answer);
//
//   - this Comb's own configured default (Reconciler.DiskSizeMB),
//     which an operator sets per node and which therefore cannot lower
//     the Colony floor, because a maximum cannot;
//
//   - the per-VM override (VMDefinition.disk_size_mb), which the FSM
//     has already refused if it is below the Colony floor.
//
// A VM seeded from a base image is the case ADR-0153 cares about: its
// effective size is the per-image default (or the override, or the
// floor - whichever is largest), and the expansion grows the VM's OWN
// copy of the image to that size.
//
// The Colony floor is read through a dedicated call, so a node whose
// raft is briefly unreachable does not silently fall back to a
// different size: the error is surfaced instead.
func (r *Reconciler) effectiveDiskSizeMB(ctx context.Context, vm VMPlacement) (uint64, error) {
	floorMB, err := r.colonyDiskFloorMB(ctx)
	if err != nil {
		return 0, err
	}
	effective := floorMB
	if r.DiskSizeMB > effective {
		effective = r.DiskSizeMB
	}
	if vm.DiskSizeMB > effective {
		effective = vm.DiskSizeMB
	}
	return effective, nil
}

// colonyDiskFloorMB reads ADR-0148's Colony-wide floor from this node's
// own replicated copy.
//
// A Colony that has never had a floor set has NO floor record at all,
// which is not the same as a floor of zero (ADR-0148's own "exactly one
// way to express no floor" rule). A node with no floor recorded falls
// back to its own configured default, which is the pre-ADR-0148
// behavior.
func (r *Reconciler) colonyDiskFloorMB(ctx context.Context) (uint64, error) {
	if r.Raft == nil {
		return 0, nil
	}
	resp, err := r.Raft.GetColonyDiskSizeLocal(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading the Colony disk-size floor: %w", err)
	}
	return resp.GetColonyDiskSize().GetFloorMb(), nil
}

// diskCommitmentKey is the key a disk's capacity commitment is tracked
// under.
//
// It is the VM's PHYSICAL identity (ADR-0152), because that is what
// names the disk on disk: a renamed VM keeps its commitment, and a
// second VM that took the old logical id never inherits it.
func diskCommitmentKey(vmID string) string {
	return vmID
}

// admitDiskCapacity takes a capacity claim for a disk growth.
//
// It is a no-op - returning a nil admission that does nothing - when
// this node has no capacity coordinator configured, because a node
// without one is a node whose operator has not opted into the
// accounting at all, and refusing every provision there would be a
// worse failure than proceeding.
func (r *Reconciler) admitDiskCapacity(vmID string, targetBytes, sourceVirtualBytes int64) (*capacity.ClaimAdmission, capacity.Domain, error) {
	if r.Capacity == nil {
		return nil, capacity.Domain{}, nil
	}
	domain := r.CapacityDomain
	observation, err := r.observeCapacity(domain)
	if err != nil {
		return nil, capacity.Domain{}, err
	}
	claim := capacity.Claim{
		Domain:      domain,
		GrowthBytes: targetBytes - sourceVirtualBytes,
		VMID:        vmID,
		TargetBytes: targetBytes,
		SourceBytes: sourceVirtualBytes,
	}
	admission, err := r.Capacity.Admit(domain, diskCommitmentKey(vmID), observation, claim)
	if err != nil {
		return nil, capacity.Domain{}, err
	}
	return admission, domain, nil
}

// recheckDiskCapacity re-reads the free capacity immediately before the
// expansion, and aborts if the reserve can no longer be maintained.
func (r *Reconciler) recheckDiskCapacity(domain capacity.Domain, vmID string, targetBytes, sourceVirtualBytes int64) error {
	if r.Capacity == nil {
		return nil
	}
	observation, err := r.observeCapacity(domain)
	if err != nil {
		return err
	}
	reserve := r.Capacity.ReserveBytes()
	if observation.FreeBytes < reserve {
		return fmt.Errorf("expanding the disk for VM %q: storage domain %q has only %d bytes free after the copy, which is below the %d byte minimum free-space reserve, so the expansion was abandoned and the incomplete disk removed. Check the target node's available storage", vmID, domain.Name, observation.FreeBytes, reserve)
	}
	if targetBytes-sourceVirtualBytes > observation.FreeBytes-reserve {
		return fmt.Errorf("expanding the disk for VM %q: storage domain %q has %d bytes free after the copy, which is not enough for the %d bytes of growth needed while preserving the %d byte minimum free-space reserve, so the expansion was abandoned and the incomplete disk removed. Check the target node's available storage", vmID, domain.Name, observation.FreeBytes, targetBytes-sourceVirtualBytes, reserve)
	}
	return nil
}

// observeCapacity reports the currently available physical capacity on a
// storage domain.
func (r *Reconciler) observeCapacity(domain capacity.Domain) (capacity.Observation, error) {
	if r.CapacityFreeSpace == nil {
		return capacity.Observation{}, fmt.Errorf("the disk capacity coordinator is configured but no free-space probe is available, so capacity cannot be checked")
	}
	free, err := r.CapacityFreeSpace(domain.Name)
	if err != nil {
		return capacity.Observation{}, fmt.Errorf("checking the free space on storage domain %q: %w", domain.Name, err)
	}
	return capacity.Observation{FreeBytes: free}, nil
}

// removeIncompleteDisk removes a partially initialized disk.
//
// It is the cleanup half of every failure path: a disk that was copied
// but not expanded, or expanded but not verified, must not survive,
// because the next reconcile tick would find it, treat it as "already
// created", and hand a broken disk to bhyve.
func removeIncompleteDisk(path string) {
	_ = os.Remove(path)
}
