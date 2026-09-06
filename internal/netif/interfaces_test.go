package netif

import "testing"

func TestListIncludesLoopback(t *testing.T) {
	interfaces, err := List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	for _, iface := range interfaces {
		if iface.Name == "lo0" || iface.Name == "lo" {
			return
		}
	}
	t.Fatalf("List() = %+v, want a loopback interface", interfaces)
}
