package resetutil

import (
	"context"
	"errors"
	"testing"
)

type fakeJails struct {
	names    []string
	removed  []string
	failName string
}

func (f *fakeJails) ListJails(context.Context) ([]string, error) { return f.names, nil }
func (f *fakeJails) RemoveJail(_ context.Context, name string) error {
	if name == f.failName {
		return errors.New("boom")
	}
	f.removed = append(f.removed, name)
	return nil
}

type fakeVMs struct {
	names     []string
	destroyed []string
}

func (f *fakeVMs) ListVMs(context.Context) ([]string, error) { return f.names, nil }
func (f *fakeVMs) DestroyVM(_ context.Context, name string) error {
	f.destroyed = append(f.destroyed, name)
	return nil
}

type fakeDatasets struct {
	names     []string
	destroyed []string
}

func (f *fakeDatasets) ListDatasets(context.Context) ([]string, error) { return f.names, nil }
func (f *fakeDatasets) DestroyDataset(_ context.Context, name string) error {
	f.destroyed = append(f.destroyed, name)
	return nil
}

type fakeISOs struct {
	infos   []ISOInfo
	deleted []string
}

func (f *fakeISOs) List() ([]ISOInfo, error) { return f.infos, nil }
func (f *fakeISOs) Delete(name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}

func TestManagedResources_DestroysEverythingInScope(t *testing.T) {
	jails := &fakeJails{names: []string{"jail-1", "jail-2"}}
	vms := &fakeVMs{names: []string{"vm-1"}}
	datasets := &fakeDatasets{names: []string{"zroot/apiary/vm-1", "zroot/apiary/jail-1"}}
	isos := &fakeISOs{infos: []ISOInfo{{Name: "installer.iso"}}}

	res := ManagedResources(context.Background(), jails, vms, datasets, isos)

	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	if len(jails.removed) != 2 {
		t.Errorf("jails removed = %v, want 2 jails", jails.removed)
	}
	if len(vms.destroyed) != 1 {
		t.Errorf("vms destroyed = %v, want 1 vm", vms.destroyed)
	}
	if len(datasets.destroyed) != 2 {
		t.Errorf("datasets destroyed = %v, want 2 datasets", datasets.destroyed)
	}
	if len(isos.deleted) != 1 {
		t.Errorf("isos deleted = %v, want 1 iso", isos.deleted)
	}
}

func TestManagedResources_OneFailureDoesNotAbortTheRest(t *testing.T) {
	jails := &fakeJails{names: []string{"jail-1", "jail-2", "jail-3"}, failName: "jail-2"}
	vms := &fakeVMs{names: []string{"vm-1"}}
	datasets := &fakeDatasets{}
	isos := &fakeISOs{}

	res := ManagedResources(context.Background(), jails, vms, datasets, isos)

	if len(res.Errors) != 1 {
		t.Fatalf("Errors = %v, want exactly 1 error for jail-2", res.Errors)
	}
	if len(jails.removed) != 2 {
		t.Errorf("jails removed = %v, want jail-1 and jail-3 despite jail-2 failing", jails.removed)
	}
	if len(vms.destroyed) != 1 {
		t.Errorf("a failure in jail cleanup should not stop VM cleanup from running, got %v", vms.destroyed)
	}
}

func TestManagedResources_NilManagerIsSkipped(t *testing.T) {
	// A node without bhyve/jail support configured has nil managers for
	// those resource types - ManagedResources must not panic, and must
	// still process everything it was given.
	datasets := &fakeDatasets{names: []string{"zroot/apiary/leftover"}}

	res := ManagedResources(context.Background(), nil, nil, datasets, nil)

	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	if len(datasets.destroyed) != 1 {
		t.Errorf("datasets destroyed = %v, want 1 dataset even with jails/vms/isos nil", datasets.destroyed)
	}
}

func TestManagedResources_EmptyScopeDestroysNothing(t *testing.T) {
	res := ManagedResources(context.Background(), &fakeJails{}, &fakeVMs{}, &fakeDatasets{}, &fakeISOs{})

	if len(res.JailsRemoved) != 0 || len(res.VMsDestroyed) != 0 || len(res.DatasetsDestroyed) != 0 || len(res.ISOsDeleted) != 0 || len(res.Errors) != 0 {
		t.Errorf("Result = %+v, want a completely empty result for an empty scope", res)
	}
}
