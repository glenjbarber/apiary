package cluster

import (
	"reflect"
	"testing"
)

func TestPlan(t *testing.T) {
	cases := []struct {
		name     string
		desired  []VMPlacement
		existing []string
		local    string
		want     []string
	}{
		{
			name:     "nothing desired",
			desired:  nil,
			existing: nil,
			local:    "node-a",
			want:     nil,
		},
		{
			name:     "creates missing VM assigned to this node",
			desired:  []VMPlacement{{ID: "vm-1", NodeID: "node-a"}},
			existing: nil,
			local:    "node-a",
			want:     []string{"vm-1"},
		},
		{
			name:     "ignores VMs assigned to other nodes",
			desired:  []VMPlacement{{ID: "vm-1", NodeID: "node-b"}},
			existing: nil,
			local:    "node-a",
			want:     nil,
		},
		{
			name:     "skips VMs that already have a local dataset",
			desired:  []VMPlacement{{ID: "vm-1", NodeID: "node-a"}},
			existing: []string{"vm-1"},
			local:    "node-a",
			want:     nil,
		},
		{
			name: "mixed: creates only what's missing and assigned locally",
			desired: []VMPlacement{
				{ID: "vm-1", NodeID: "node-a"},
				{ID: "vm-2", NodeID: "node-a"},
				{ID: "vm-3", NodeID: "node-b"},
			},
			existing: []string{"vm-1"},
			local:    "node-a",
			want:     []string{"vm-2"},
		},
		{
			name: "does not report anything to destroy for orphaned datasets",
			desired: []VMPlacement{
				{ID: "vm-1", NodeID: "node-a"},
			},
			existing: []string{"vm-1", "stale-vm"},
			local:    "node-a",
			want:     nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Plan(c.desired, c.existing, c.local)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Plan() = %v, want %v", got, c.want)
			}
		})
	}
}
