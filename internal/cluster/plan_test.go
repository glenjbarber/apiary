package cluster

import (
	"reflect"
	"testing"
)

func TestPlan(t *testing.T) {
	cases := []struct {
		name    string
		desired []VMPlacement
		local   string
		want    []VMPlacement
	}{
		{
			name:    "nothing desired",
			desired: nil,
			local:   "node-a",
			want:    nil,
		},
		{
			name:    "returns VM assigned to this node",
			desired: []VMPlacement{{ID: "vm-1", NodeID: "node-a", Vcpus: 2, MemoryMB: 1024}},
			local:   "node-a",
			want:    []VMPlacement{{ID: "vm-1", NodeID: "node-a", Vcpus: 2, MemoryMB: 1024}},
		},
		{
			name:    "ignores VMs assigned to other nodes",
			desired: []VMPlacement{{ID: "vm-1", NodeID: "node-b"}},
			local:   "node-a",
			want:    nil,
		},
		{
			name: "mixed: returns only VMs assigned locally, sorted by ID",
			desired: []VMPlacement{
				{ID: "vm-2", NodeID: "node-a"},
				{ID: "vm-3", NodeID: "node-b"},
				{ID: "vm-1", NodeID: "node-a"},
			},
			local: "node-a",
			want: []VMPlacement{
				{ID: "vm-1", NodeID: "node-a"},
				{ID: "vm-2", NodeID: "node-a"},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Plan(c.desired, c.local)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Plan() = %+v, want %+v", got, c.want)
			}
		})
	}
}
