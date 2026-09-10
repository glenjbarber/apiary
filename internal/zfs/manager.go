package zfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
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

// sendReadCloser wraps a running `zfs send`'s stdout pipe. Close waits
// for the subprocess to exit and surfaces a non-zero exit (with its
// captured stderr) as an error - the caller must always Close it,
// mirroring the same "the process isn't done until you've drained and
// closed it" contract an os.File from exec.Cmd.StdoutPipe requires.
type sendReadCloser struct {
	stdout io.ReadCloser
	cmd    *exec.Cmd
	stderr *bytes.Buffer
}

func (s *sendReadCloser) Read(p []byte) (int, error) { return s.stdout.Read(p) }

func (s *sendReadCloser) Close() error {
	s.stdout.Close()
	if err := s.cmd.Wait(); err != nil {
		msg := strings.TrimSpace(s.stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("zfs send: %s", msg)
	}
	return nil
}

// Send streams `zfs send <dataset@snapshot>`'s stdout live (ADR-0089) -
// unlike every other method in this file (which use runZFS's own
// fully-buffered stdout capture), a send stream can be arbitrarily
// large, so this starts the subprocess and hands back its stdout pipe
// directly rather than buffering it. The caller must Close() the
// returned ReadCloser once done reading, which waits for the
// subprocess and reports a non-zero exit as an error.
func (m *Manager) Send(ctx context.Context, snapshot string) (io.ReadCloser, error) {
	full, err := m.snapshotPath(snapshot)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "zfs", "send", full)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("zfs send %s: %w", full, err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("zfs send %s: %w", full, err)
	}
	return &sendReadCloser{stdout: stdout, cmd: cmd, stderr: &stderr}, nil
}

// Receive runs `zfs receive <dataset>` with its stdin fed from r,
// blocking until r is fully drained (ADR-0089) - the dataset name and
// its @apiary-template-suffixed snapshot both come from the incoming
// stream itself, exactly matching whatever the sending side's Send
// call named. destName is validated the same way every other dataset
// name in this file is.
func (m *Manager) Receive(ctx context.Context, destName string, r io.Reader) error {
	full, err := m.path(destName)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "zfs", "receive", full)
	cmd.Stdin = r
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("zfs receive %s: %s", full, msg)
	}
	return nil
}

// CreateSnapshot creates a new snapshot named "dataset@snapshot"
// (relative to Base) of an existing dataset - e.g. "vm-1@before-migration"
// to checkpoint a VM's own dataset (which holds its disk.img, see
// ADR-0090) before a risky change, so it can be rolled back with
// RollbackSnapshot instead of rebuilding the VM from scratch.
func (m *Manager) CreateSnapshot(ctx context.Context, name string) error {
	full, err := m.snapshotPath(name)
	if err != nil {
		return err
	}
	_, err = runZFS(ctx, "snapshot", full)
	return err
}

// RollbackSnapshot reverts Base/dataset to the state captured by the
// named snapshot ("dataset@snapshot", relative to Base), discarding
// every write since. Deliberately does not pass zfs rollback's own -r
// flag: this fails outright (rather than silently destroying them) if
// snapshots newer than the target exist, the same "never destroy data
// without being asked to" caution DestroyDataset's own doc comment
// states for datasets with children. The caller is responsible for
// making sure nothing is still using the dataset (see ADR-0090's own
// disclosed "stop the VM first" requirement) - this method has no way
// to check that itself.
func (m *Manager) RollbackSnapshot(ctx context.Context, name string) error {
	full, err := m.snapshotPath(name)
	if err != nil {
		return err
	}
	_, err = runZFS(ctx, "rollback", full)
	return err
}

// DestroySnapshot removes the named snapshot ("dataset@snapshot",
// relative to Base) outright.
func (m *Manager) DestroySnapshot(ctx context.Context, name string) error {
	full, err := m.snapshotPath(name)
	if err != nil {
		return err
	}
	_, err = runZFS(ctx, "destroy", full)
	return err
}

// ListSnapshots lists the names (just the part after "@") of every
// snapshot that exists directly on Base/datasetName - deliberately not
// recursive (no -r), so a VM's own dataset never reports some
// unrelated child dataset's snapshots as its own. Returns an empty
// list, not an error, if datasetName doesn't exist at all yet.
func (m *Manager) ListSnapshots(ctx context.Context, datasetName string) ([]string, error) {
	full, err := m.path(datasetName)
	if err != nil {
		return nil, err
	}
	out, err := runZFS(ctx, "list", "-H", "-o", "name", "-t", "snapshot", full)
	if err != nil {
		if strings.Contains(err.Error(), "dataset does not exist") {
			return nil, nil
		}
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	prefix := full + "@"
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		names = append(names, strings.TrimPrefix(line, prefix))
	}
	return names, nil
}

// ListTemplateNames lists every name under Base/templates that has an
// @apiary-template snapshot (ADR-0084's own fixed convention) - backs
// the peer-fetch "does this node have it" query (ADR-0089). Returns an
// empty list, not an error, if Base/templates doesn't exist at all yet
// (no templates have ever been created on this node).
func (m *Manager) ListTemplateNames(ctx context.Context) ([]string, error) {
	full, err := m.path("templates")
	if err != nil {
		return nil, err
	}
	out, err := runZFS(ctx, "list", "-H", "-o", "name", "-t", "snapshot", "-r", full)
	if err != nil {
		if strings.Contains(err.Error(), "dataset does not exist") {
			return nil, nil
		}
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	prefix := full + "/"
	const suffix = "@apiary-template"
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, suffix) {
			continue
		}
		names = append(names, strings.TrimSuffix(strings.TrimPrefix(line, prefix), suffix))
	}
	return names, nil
}
