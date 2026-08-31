package zfs

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Manager creates, destroys, lists, and inspects ZFS datasets, all scoped
// under Base. Every operation validates its dataset name so it can never
// resolve to a path outside Base — the safety property that matters most
// here, since zfs destroy is irreversible.
type Manager struct {
	Base string
}

// New returns a Manager scoped to base (e.g. "zroot/apiary", or a test
// pool like "apiarytest").
func New(base string) *Manager {
	return &Manager{Base: base}
}

// path validates name and returns the full dataset path (Base/name).
func (m *Manager) path(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("zfs: dataset name must not be empty")
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("zfs: invalid dataset name %q", name)
		}
	}
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("zfs: invalid dataset name %q: must be relative to base", name)
	}
	return m.Base + "/" + name, nil
}

// CreateDataset creates a new dataset at Base/name.
func (m *Manager) CreateDataset(ctx context.Context, name string) error {
	full, err := m.path(name)
	if err != nil {
		return err
	}
	_, err = runZFS(ctx, "create", full)
	return err
}

// CreateZvol creates a new zvol (block device dataset) at Base/name,
// sized sizeMB - used as a HAST-replicated resource's local GEOM
// provider (see internal/cluster's HAST wiring), since neither of this
// project's real hosts has a spare raw disk/partition to dedicate.
// DatasetExists/DestroyDataset (unchanged) work identically against a
// zvol - zfs list/destroy don't distinguish dataset type - so this is
// the only zvol-specific method needed here.
func (m *Manager) CreateZvol(ctx context.Context, name string, sizeMB uint64) error {
	full, err := m.path(name)
	if err != nil {
		return err
	}
	_, err = runZFS(ctx, "create", "-V", strconv.FormatUint(sizeMB, 10)+"M", full)
	return err
}

// DestroyDataset destroys the dataset at Base/name. It fails if the
// dataset has children or snapshots, matching zfs destroy's own default
// behavior — there is no recursive option here; add one explicitly if a
// caller ever legitimately needs cascading delete.
func (m *Manager) DestroyDataset(ctx context.Context, name string) error {
	full, err := m.path(name)
	if err != nil {
		return err
	}
	_, err = runZFS(ctx, "destroy", full)
	return err
}

// DatasetExists reports whether Base/name currently exists.
func (m *Manager) DatasetExists(ctx context.Context, name string) (bool, error) {
	full, err := m.path(name)
	if err != nil {
		return false, err
	}
	_, err = runZFS(ctx, "list", "-H", "-o", "name", full)
	if err != nil {
		if strings.Contains(err.Error(), "dataset does not exist") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ListDatasets returns the names (relative to Base) of all descendant
// datasets of Base, not including Base itself.
func (m *Manager) ListDatasets(ctx context.Context) ([]string, error) {
	out, err := runZFS(ctx, "list", "-H", "-o", "name", "-r", m.Base)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}

	prefix := m.Base + "/"
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == m.Base {
			continue
		}
		names = append(names, strings.TrimPrefix(line, prefix))
	}
	return names, nil
}

// GetProperty returns the value of a ZFS property on Base/name.
func (m *Manager) GetProperty(ctx context.Context, name, prop string) (string, error) {
	full, err := m.path(name)
	if err != nil {
		return "", err
	}
	return runZFS(ctx, "get", "-H", "-o", "value", prop, full)
}

// SetProperty sets a ZFS property on Base/name.
func (m *Manager) SetProperty(ctx context.Context, name, prop, value string) error {
	full, err := m.path(name)
	if err != nil {
		return err
	}
	_, err = runZFS(ctx, "set", prop+"="+value, full)
	return err
}
