package zfs

import (
	"context"
	"fmt"
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

// snapshotPath validates a "dataset@snapshot" name relative to Base
// (e.g. "templates/freebsd-14@apiary-template") and returns the full
// path. The dataset half is validated by path() exactly like every
// other dataset name; the snapshot half must be non-empty and contain
// no "/".
func (m *Manager) snapshotPath(name string) (string, error) {
	dataset, snap, ok := strings.Cut(name, "@")
	if !ok || snap == "" {
		return "", fmt.Errorf("zfs: invalid snapshot name %q: must be \"dataset@snapshot\"", name)
	}
	if strings.Contains(snap, "/") {
		return "", fmt.Errorf("zfs: invalid snapshot name %q", name)
	}
	full, err := m.path(dataset)
	if err != nil {
		return "", err
	}
	return full + "@" + snap, nil
}

// SnapshotExists reports whether the snapshot named "dataset@snapshot"
// (relative to Base) currently exists.
func (m *Manager) SnapshotExists(ctx context.Context, name string) (bool, error) {
	full, err := m.snapshotPath(name)
	if err != nil {
		return false, err
	}
	_, err = runZFS(ctx, "list", "-H", "-o", "name", "-t", "snapshot", full)
	if err != nil {
		if strings.Contains(err.Error(), "dataset does not exist") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Clone creates a new dataset at Base/destName from the snapshot named
// "dataset@snapshot" (relative to Base) - e.g. cloning
// "templates/freebsd-14@apiary-template" into a fresh jail root. The
// clone is a normal, independent ZFS dataset from the caller's
// perspective (DestroyDataset works on it exactly like any other
// dataset this Manager created directly).
func (m *Manager) Clone(ctx context.Context, snapshot, destName string) error {
	fullSnap, err := m.snapshotPath(snapshot)
	if err != nil {
		return err
	}
	fullDest, err := m.path(destName)
	if err != nil {
		return err
	}
	_, err = runZFS(ctx, "clone", fullSnap, fullDest)
	return err
}
