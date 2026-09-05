package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// fakeDNSAPI stands in for Cloudflare's DNS API, tracking every
// create/update/delete call and serving back whatever findRecord
// itself would need for EnsureCNAME/DeleteCNAME's own lookups.
func fakeDNSAPI(t *testing.T, existing map[string]string, createUpdateCalls, deleteCalls *int) {
	t.Helper()
	withFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			name := r.URL.Query().Get("name")
			if content, ok := existing[name]; ok {
				writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`[{"id":"rec-` + name + `","type":"CNAME","name":"` + name + `","content":"` + content + `","proxied":true}]`)})
				return
			}
			writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`[]`)})
		case http.MethodPost, http.MethodPut:
			*createUpdateCalls++
			writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`{}`)})
		case http.MethodDelete:
			*deleteCalls++
			writeJSON(w, apiResponse{Success: true, Result: json.RawMessage(`{}`)})
		}
	})
}

func TestReconcileExposures_NewExposureCallsEnsureCNAMEAndTracksSidecar(t *testing.T) {
	var createUpdate, deletes int
	fakeDNSAPI(t, nil, &createUpdate, &deletes)

	dir := t.TempDir()
	var pidfilePath string
	m := &Manager{RunDir: dir, execCommand: fakeExec(t, &pidfilePath)}

	err := m.ReconcileExposures(context.Background(), "tok", "zone-1", "tunnel-1.cfargotunnel.com", "tunnel-1", "creds.json",
		[]DesiredExposure{{VMID: "vm-1", Hostname: "web.example.com", Address: "10.0.0.1:8080"}})
	if err != nil {
		t.Fatalf("ReconcileExposures() error: %v", err)
	}
	if createUpdate != 1 {
		t.Errorf("createUpdate calls = %d, want 1", createUpdate)
	}
	sidecar, _ := m.loadSidecar()
	if rec, ok := sidecar["web.example.com"]; !ok || rec.VMID != "vm-1" || rec.Address != "10.0.0.1:8080" {
		t.Errorf("sidecar = %+v, want web.example.com tracked with vm-1/10.0.0.1:8080", sidecar)
	}
}

func TestReconcileExposures_UnchangedDesiredSetMakesNoAPICalls(t *testing.T) {
	var createUpdate, deletes int
	fakeDNSAPI(t, map[string]string{"web.example.com": "tunnel-1.cfargotunnel.com"}, &createUpdate, &deletes)

	dir := t.TempDir()
	m := &Manager{RunDir: dir, execCommand: fakeExec(t, new(string))}
	// Pre-seed the sidecar as if a previous tick already applied this
	// exact exposure.
	if err := m.saveSidecar(map[string]ExposureRecord{"web.example.com": {VMID: "vm-1", Address: "10.0.0.1:8080"}}); err != nil {
		t.Fatalf("saveSidecar() error: %v", err)
	}

	desired := []DesiredExposure{{VMID: "vm-1", Hostname: "web.example.com", Address: "10.0.0.1:8080"}}
	if err := m.ReconcileExposures(context.Background(), "tok", "zone-1", "tunnel-1.cfargotunnel.com", "tunnel-1", "creds.json", desired); err != nil {
		t.Fatalf("ReconcileExposures() error: %v", err)
	}
	if createUpdate != 0 || deletes != 0 {
		t.Fatalf("createUpdate/delete calls = %d/%d, want 0/0 - ADR-0063 finding 3: never call the DNS API when nothing changed", createUpdate, deletes)
	}
}

func TestReconcileExposures_RemovedExposureCallsDeleteCNAME(t *testing.T) {
	var createUpdate, deletes int
	fakeDNSAPI(t, map[string]string{"web.example.com": "tunnel-1.cfargotunnel.com"}, &createUpdate, &deletes)

	dir := t.TempDir()
	m := &Manager{RunDir: dir, execCommand: fakeExec(t, new(string))}
	if err := m.saveSidecar(map[string]ExposureRecord{"web.example.com": {VMID: "vm-1", Address: "10.0.0.1:8080"}}); err != nil {
		t.Fatalf("saveSidecar() error: %v", err)
	}

	// vm-1 no longer appears in desired (reassigned away, deleted, or
	// its hostname cleared) - must be torn down.
	if err := m.ReconcileExposures(context.Background(), "tok", "zone-1", "tunnel-1.cfargotunnel.com", "tunnel-1", "creds.json", nil); err != nil {
		t.Fatalf("ReconcileExposures() error: %v", err)
	}
	if deletes != 1 {
		t.Fatalf("delete calls = %d, want 1", deletes)
	}
	sidecar, _ := m.loadSidecar()
	if _, ok := sidecar["web.example.com"]; ok {
		t.Errorf("sidecar still tracks web.example.com after removal, want it gone")
	}
	// With nothing desired, cloudflared itself should be stopped, not left running.
	alive, err := m.processAlive()
	if err != nil || alive {
		t.Errorf("processAlive() = %v, %v, want false with zero desired exposures", alive, err)
	}
}

func TestReconcileExposures_AddressChangeUpdatesRecordAndSidecar(t *testing.T) {
	var createUpdate, deletes int
	fakeDNSAPI(t, map[string]string{"web.example.com": "tunnel-1.cfargotunnel.com"}, &createUpdate, &deletes)

	dir := t.TempDir()
	m := &Manager{RunDir: dir, execCommand: fakeExec(t, new(string))}
	if err := m.saveSidecar(map[string]ExposureRecord{"web.example.com": {VMID: "vm-1", Address: "10.0.0.1:8080"}}); err != nil {
		t.Fatalf("saveSidecar() error: %v", err)
	}

	// Same hostname, VM's own address changed (e.g. migrated to a
	// different network) - must re-apply (via EnsureCNAME, since the
	// CNAME target itself - the tunnel - hasn't changed, only what
	// cloudflared proxies to internally) and update the sidecar.
	desired := []DesiredExposure{{VMID: "vm-1", Hostname: "web.example.com", Address: "10.0.0.2:8080"}}
	if err := m.ReconcileExposures(context.Background(), "tok", "zone-1", "tunnel-1.cfargotunnel.com", "tunnel-1", "creds.json", desired); err != nil {
		t.Fatalf("ReconcileExposures() error: %v", err)
	}
	sidecar, _ := m.loadSidecar()
	if rec := sidecar["web.example.com"]; rec.Address != "10.0.0.2:8080" {
		t.Errorf("sidecar address = %q, want 10.0.0.2:8080 after the VM's address changed", rec.Address)
	}
}

func TestReconcileExposures_SurvivesRestartWithPersistedSidecar(t *testing.T) {
	// Regression test for ADR-0063 finding 5: a fresh Manager (simulating
	// a managerd restart, no in-memory state) must still detect and tear
	// down an exposure no longer desired, using only the persisted
	// sidecar - never silently leaking a stale CNAME.
	var createUpdate, deletes int
	fakeDNSAPI(t, map[string]string{"web.example.com": "tunnel-1.cfargotunnel.com"}, &createUpdate, &deletes)

	dir := t.TempDir()
	first := &Manager{RunDir: dir, execCommand: fakeExec(t, new(string))}
	if err := first.saveSidecar(map[string]ExposureRecord{"web.example.com": {VMID: "vm-1", Address: "10.0.0.1:8080"}}); err != nil {
		t.Fatalf("saveSidecar() error: %v", err)
	}

	fresh := &Manager{RunDir: dir, execCommand: fakeExec(t, new(string))} // a brand new Manager value - no shared memory with `first`
	if err := fresh.ReconcileExposures(context.Background(), "tok", "zone-1", "tunnel-1.cfargotunnel.com", "tunnel-1", "creds.json", nil); err != nil {
		t.Fatalf("ReconcileExposures() error: %v", err)
	}
	if deletes != 1 {
		t.Fatalf("delete calls = %d, want 1 - a fresh Manager must still tear down a no-longer-desired exposure via the persisted sidecar", deletes)
	}
}
