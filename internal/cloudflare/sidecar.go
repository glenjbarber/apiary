package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// ExposureRecord is one hostname's last-successfully-applied exposure -
// persisted locally (never raft) so a removal can be detected across a
// managerd restart, mirroring internal/nodeconfig.Manager's own
// "physical, per-node, never raft" JSON file precedent. See ADR-0063
// finding 5 for why an in-memory-only record isn't enough: EnsureCNAME
// being idempotent covers additions fine even with no memory at all,
// but a removal needs to know what to delete, and the only sources are
// this local record or querying Cloudflare's own zone.
type ExposureRecord struct {
	VMID    string `json:"vm_id"`
	Address string `json:"address"`
}

// DesiredExposure is one VM's desired public exposure, as the
// reconciler already knows it (VMID for logging/tracking, Hostname,
// and Address in "ip:port" form).
type DesiredExposure struct {
	VMID     string
	Hostname string
	Address  string
}

func (m *Manager) sidecarPath() string { return m.runDir() + "/exposures.json" }

// loadSidecar reads the last-applied exposure set. A missing file is
// not an error - it returns an empty map, matching a fresh install or
// a sidecar that predates this node ever exposing anything.
func (m *Manager) loadSidecar() (map[string]ExposureRecord, error) {
	data, err := os.ReadFile(m.sidecarPath())
	if os.IsNotExist(err) {
		return map[string]ExposureRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	var records map[string]ExposureRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("decoding exposure sidecar: %w", err)
	}
	if records == nil {
		records = map[string]ExposureRecord{}
	}
	return records, nil
}

func (m *Manager) saveSidecar(records map[string]ExposureRecord) error {
	if err := os.MkdirAll(m.runDir(), 0o700); err != nil {
		return fmt.Errorf("creating cloudflared run dir: %w", err)
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.sidecarPath(), data, 0o600)
}

// ReconcileExposures is the single entry point internal/cluster's
// reconciler calls once per tick to converge both Cloudflare's DNS
// state and this node's own cloudflared process toward desired.
// tunnelTarget is the pre-provisioned tunnel's own
// "<tunnel-id>.cfargotunnel.com" routing target every exposed
// hostname's CNAME points at.
//
// DNS calls happen only on a real diff against the persisted sidecar
// (ADR-0063 finding 3 - Cloudflare's API is neither free nor unlimited,
// unlike the local CLI tools every "call unconditionally" precedent
// elsewhere in this codebase wraps) - additions/changes call
// EnsureCNAME, removals (present in the sidecar but no longer desired,
// or desired at a different address) call DeleteCNAME, mirroring
// ADR-0025's PlanReclaim "reassigned away from this node" detection
// applied to hostname exposure instead of physical resources.
func (m *Manager) ReconcileExposures(ctx context.Context, token, zoneID, tunnelTarget, tunnelID, credentialsFile string, desired []DesiredExposure) error {
	sidecar, err := m.loadSidecar()
	if err != nil {
		return fmt.Errorf("loading exposure sidecar: %w", err)
	}

	desiredByHostname := make(map[string]DesiredExposure, len(desired))
	for _, d := range desired {
		desiredByHostname[d.Hostname] = d
	}

	var firstErr error
	for hostname, rec := range sidecar {
		if d, ok := desiredByHostname[hostname]; ok && d.Address == rec.Address {
			continue
		}
		if err := DeleteCNAME(ctx, token, zoneID, hostname); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("removing stale exposure for %s: %w", hostname, err)
			}
			continue
		}
		delete(sidecar, hostname)
	}

	for hostname, d := range desiredByHostname {
		if rec, ok := sidecar[hostname]; ok && rec.Address == d.Address {
			continue
		}
		if err := EnsureCNAME(ctx, token, zoneID, hostname, tunnelTarget); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("applying exposure for %s: %w", hostname, err)
			}
			continue
		}
		sidecar[hostname] = ExposureRecord{VMID: d.VMID, Address: d.Address}
	}

	if err := m.saveSidecar(sidecar); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("saving exposure sidecar: %w", err)
	}

	ingresses := make([]Ingress, 0, len(desired))
	for _, d := range desired {
		ingresses = append(ingresses, Ingress{Hostname: d.Hostname, Address: d.Address})
	}
	if err := m.EnsureRunning(ctx, tunnelID, credentialsFile, ingresses); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("ensuring cloudflared is running: %w", err)
	}

	return firstErr
}
