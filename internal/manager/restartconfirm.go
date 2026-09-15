package manager

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// PendingRestart records a granted restart lease this node's own
// RestartNodeService is about to act on, written before the real
// `service apiary_managerd restart` call and read back by cmd/managerd's
// own next-startup path to self-confirm (ADR-0103) - see that package's
// own doc comment for why confirmation cannot happen from inside the
// process that requested the restart. Exported (unlike most of this
// package's request/response internals) because cmd/managerd needs to
// name it directly in its own startup-confirmation logic.
type PendingRestart struct {
	Service string `json:"service"`
	NodeID  string `json:"node_id"`
	LeaseID uint64 `json:"lease_id"`
}

// RestartConfirmStore persists exactly one PendingRestart per service at
// a well-known path, mirroring internal/deadman's own single-purpose,
// atomic-write state-file convention.
type RestartConfirmStore struct {
	Dir string
}

// NewRestartConfirmStore returns a RestartConfirmStore rooted at dir.
func NewRestartConfirmStore(dir string) *RestartConfirmStore { return &RestartConfirmStore{Dir: dir} }

func (r *RestartConfirmStore) path(service string) string {
	return filepath.Join(r.Dir, "pending-restart-"+service+".json")
}

// Save writes p atomically (temp file + chmod 0600 + rename), overwriting
// any stale prior record for the same service - RestartNodeService calls
// this immediately BEFORE issuing the actual restart command, not after,
// so a crash immediately after issuing the command still leaves a
// discoverable, correctly-blocking trace.
func (r *RestartConfirmStore) Save(p PendingRestart) error {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return fmt.Errorf("restartconfirm: creating state directory: %w", err)
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("restartconfirm: encoding pending restart: %w", err)
	}
	path := r.path(p.Service)
	tmp, err := os.CreateTemp(r.Dir, ".pending-restart-*.tmp")
	if err != nil {
		return fmt.Errorf("restartconfirm: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once successfully renamed
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("restartconfirm: writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("restartconfirm: closing temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("restartconfirm: setting permissions: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("restartconfirm: finalizing write: %w", err)
	}
	return nil
}

// Load reads the pending record for service, if any. A missing file is
// not an error - it means either no restart is pending, or a prior one
// already confirmed and cleared itself (see Clear).
func (r *RestartConfirmStore) Load(service string) (PendingRestart, bool, error) {
	data, err := os.ReadFile(r.path(service))
	if err != nil {
		if os.IsNotExist(err) {
			return PendingRestart{}, false, nil
		}
		return PendingRestart{}, false, fmt.Errorf("restartconfirm: reading pending restart: %w", err)
	}
	var p PendingRestart
	if err := json.Unmarshal(data, &p); err != nil {
		return PendingRestart{}, false, fmt.Errorf("restartconfirm: parsing pending restart: %w", err)
	}
	return p, true, nil
}

// Clear removes the pending record for service - called only after a
// successful ConfirmRestartCompleted, never on failure (see
// cmd/managerd's own startup-confirm logic, ADR-0103).
func (r *RestartConfirmStore) Clear(service string) error {
	if err := os.Remove(r.path(service)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("restartconfirm: removing pending restart: %w", err)
	}
	return nil
}
