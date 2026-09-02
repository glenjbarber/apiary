// Package resetutil implements the destructive enumerate-and-destroy
// logic behind managerd's -reset-managed/-factory-reset one-shot modes
// (see docs/adr/0038-tiered-reset-cli.md). It's kept separate from
// cmd/managerd/main.go so the actual destructive behavior is unit
// testable against fake managers, without needing real ZFS/jail/bhyve
// tooling - the same reasoning internal/cluster's own local manager
// interfaces already follow.
package resetutil

import (
	"context"
	"fmt"
)

// DatasetManager is the subset of *zfs.Manager needed to enumerate and
// destroy every dataset under a node's configured base - defined
// locally so tests can supply a fake, mirroring internal/cluster's own
// datasetManager interface (which is unexported and can't be reused
// directly across the package boundary).
type DatasetManager interface {
	ListDatasets(ctx context.Context) ([]string, error)
	DestroyDataset(ctx context.Context, name string) error
}

// JailManager is the subset of *jail.Manager needed here, for the same
// reason as DatasetManager.
type JailManager interface {
	ListJails(ctx context.Context) ([]string, error)
	RemoveJail(ctx context.Context, name string) error
}

// VMManager is the subset of *bhyve.Manager needed here, for the same
// reason as DatasetManager.
type VMManager interface {
	ListVMs(ctx context.Context) ([]string, error)
	DestroyVM(ctx context.Context, name string) error
}

// ISOManager is the subset of *isostore.Manager needed here, for the
// same reason as DatasetManager.
type ISOManager interface {
	List() ([]ISOInfo, error)
	Delete(name string) error
}

// ISOInfo mirrors isostore.Info's Name field only - the one thing this
// package needs from it, avoiding an isostore import purely for a
// struct shape.
type ISOInfo struct {
	Name string
}

// Result reports what ManagedResources actually did, so a caller (the
// one-shot CLI mode) can log a clear summary - matching this project's
// established "best-effort per item, report what happened" convention
// (e.g. internal/hoststats's per-subsystem gathering).
type Result struct {
	JailsRemoved      []string
	VMsDestroyed      []string
	DatasetsDestroyed []string
	ISOsDeleted       []string
	Errors            []error
}

// ManagedResources enumerates and destroys everything each already-
// scoped manager can see - jails first (plain jail(8) removal, doesn't
// touch the underlying dataset), then bhyve VMs, then every remaining
// ZFS dataset under the base, then every stored ISO. This ordering
// mirrors internal/cluster/reconciler.go's own teardownVM/purgeJail
// sequencing (bhyve/jail teardown before the ZFS dataset it depends
// on). Any nil manager is skipped entirely (matching the reconciler's
// own nil-able Bhyve/ISOs fields - a node without bhyve support, for
// instance, simply has nothing to destroy there). Best-effort per item:
// one failure is recorded in Result.Errors and enumeration continues,
// rather than aborting the whole run - the same "one bad item doesn't
// blank out the rest" posture internal/hoststats already established.
//
// This is exactly Tier 2 (docs/adr/0038-tiered-reset-cli.md) - every
// resource here is, by construction, inside Apiary's own configured
// scope (the ZFS base dataset, the jail/bhyve name prefix, the ISO
// directory), so nothing outside that scope is ever reachable through
// this function no matter what it's given.
func ManagedResources(ctx context.Context, jails JailManager, vms VMManager, datasets DatasetManager, isos ISOManager) Result {
	var res Result

	if jails != nil {
		names, err := jails.ListJails(ctx)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("listing jails: %w", err))
		}
		for _, name := range names {
			if err := jails.RemoveJail(ctx, name); err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("removing jail %q: %w", name, err))
				continue
			}
			res.JailsRemoved = append(res.JailsRemoved, name)
		}
	}

	if vms != nil {
		names, err := vms.ListVMs(ctx)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("listing VMs: %w", err))
		}
		for _, name := range names {
			if err := vms.DestroyVM(ctx, name); err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("destroying VM %q: %w", name, err))
				continue
			}
			res.VMsDestroyed = append(res.VMsDestroyed, name)
		}
	}

	if datasets != nil {
		names, err := datasets.ListDatasets(ctx)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("listing datasets: %w", err))
		}
		for _, name := range names {
			if err := datasets.DestroyDataset(ctx, name); err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("destroying dataset %q: %w", name, err))
				continue
			}
			res.DatasetsDestroyed = append(res.DatasetsDestroyed, name)
		}
	}

	if isos != nil {
		infos, err := isos.List()
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("listing ISOs: %w", err))
		}
		for _, info := range infos {
			if err := isos.Delete(info.Name); err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("deleting ISO %q: %w", info.Name, err))
				continue
			}
			res.ISOsDeleted = append(res.ISOsDeleted, info.Name)
		}
	}

	return res
}
