package tlsmigrate

import (
	"fmt"
	"path/filepath"
	"testing"
)

type fakeRestarter struct {
	order  []string
	failOn string
	err    error
}

func (f *fakeRestarter) Restart(service string) error {
	f.order = append(f.order, service)
	if service == f.failOn {
		return f.err
	}
	return nil
}

func TestPlanRestartRunsOnlyTouchedServicesInSafeOrder(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc", "apiary")
	oldDir := filepath.Join(root, "etc", "apiary-tls")
	writeSampleConfigs(t, configDir, oldDir)

	plan, err := New(Paths{ConfigDir: configDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	r := &fakeRestarter{}
	restarted, err := plan.Restart(r)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	want := []string{"apiary_managerd", "apiary_frontend", "apiary_restshimd"}
	if len(restarted) != len(want) {
		t.Fatalf("restarted = %v, want %v", restarted, want)
	}
	for i := range want {
		if restarted[i] != want[i] {
			t.Errorf("restarted[%d] = %q, want %q - managerd must restart before frontend/restshimd dial its (possibly briefly stale) certificate again", i, restarted[i], want[i])
		}
	}
}

func TestPlanRestartStopsAtFirstFailure(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc", "apiary")
	oldDir := filepath.Join(root, "etc", "apiary-tls")
	writeSampleConfigs(t, configDir, oldDir)

	plan, err := New(Paths{ConfigDir: configDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	wantErr := fmt.Errorf("boom")
	r := &fakeRestarter{failOn: "apiary_frontend", err: wantErr}
	restarted, err := plan.Restart(r)
	if err != wantErr {
		t.Fatalf("Restart err = %v, want %v", err, wantErr)
	}
	if len(restarted) != 1 || restarted[0] != "apiary_managerd" {
		t.Errorf("restarted = %v, want only [apiary_managerd] before the failure", restarted)
	}
	if len(r.order) != 2 {
		t.Errorf("restarter was called %d times, want exactly 2 (managerd, then the failing frontend) - restshimd must not be touched after a failure", len(r.order))
	}
}

func TestNoOpPlanRestartsNothing(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc", "apiary")
	newDir := filepath.Join(configDir, "tls")
	writeSampleConfigs(t, configDir, newDir)

	plan, err := New(Paths{ConfigDir: configDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !plan.NoOp {
		t.Fatal("expected a no-op plan")
	}
	r := &fakeRestarter{}
	restarted, err := plan.Restart(r)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if len(restarted) != 0 {
		t.Errorf("restarted = %v, want none: a no-op migration touched no config file, so nothing needs restarting", restarted)
	}
}
