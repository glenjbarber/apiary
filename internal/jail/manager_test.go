package jail

import (
	"reflect"
	"testing"
)

// TestCreateArgs_IP4Inherit confirms the default (no VNET) path is
// byte-for-byte unchanged from before ADR-0117 - existing callers that
// never set Config.VNET must see zero behavior change.
func TestCreateArgs_IP4Inherit(t *testing.T) {
	got := createArgs("apiary-web-1", Config{Path: "/apiary-jails/web-1", Hostname: "web-1.apiary.test"})
	want := []string{
		"name=apiary-web-1",
		"path=/apiary-jails/web-1",
		"host.hostname=web-1.apiary.test",
		"ip4=inherit",
		"persist",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("createArgs() = %v, want %v", got, want)
	}
}

// TestCreateArgs_VNET confirms a VNET jail (ADR-0117) gets the vnet;
// stanza with its assigned epair interface instead of ip4=inherit.
func TestCreateArgs_VNET(t *testing.T) {
	got := createArgs("apiary-web-1", Config{
		Path:          "/apiary-jails/web-1",
		Hostname:      "web-1.apiary.test",
		VNET:          true,
		VNETInterface: "epair0b",
	})
	want := []string{
		"name=apiary-web-1",
		"path=/apiary-jails/web-1",
		"host.hostname=web-1.apiary.test",
		"vnet",
		"vnet.interface=epair0b",
		"persist",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("createArgs() = %v, want %v", got, want)
	}
	for _, arg := range got {
		if arg == "ip4=inherit" {
			t.Errorf("createArgs() includes ip4=inherit alongside vnet, want only one networking mode")
		}
	}
}

// TestCreateJail_VNETRequiresInterface confirms CreateJail itself
// rejects VNET without an interface before ever shelling out - there is
// nothing for jail(8) to attach in that case.
func TestCreateJail_VNETRequiresInterface(t *testing.T) {
	m := New("apiary-")
	err := m.CreateJail(nil, "web-1", Config{Path: "/tmp", Hostname: "web-1", VNET: true})
	if err == nil {
		t.Fatalf("CreateJail() error = nil, want a VNETInterface-required rejection")
	}
}
