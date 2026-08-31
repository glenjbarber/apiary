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

func TestPlanReclaim(t *testing.T) {
	cases := []struct {
		name    string
		desired []VMPlacement
		local   string
		want    []string
	}{
		{
			name:    "nothing desired",
			desired: nil,
			local:   "node-a",
			want:    nil,
		},
		{
			name:    "VM assigned locally is not a reclaim candidate",
			desired: []VMPlacement{{ID: "vm-1", NodeID: "node-a"}},
			local:   "node-a",
			want:    nil,
		},
		{
			name:    "VM assigned elsewhere is a reclaim candidate",
			desired: []VMPlacement{{ID: "vm-1", NodeID: "node-b"}},
			local:   "node-a",
			want:    []string{"vm-1"},
		},
		{
			name: "mixed: only non-local VMs, sorted by ID",
			desired: []VMPlacement{
				{ID: "vm-3", NodeID: "node-b"},
				{ID: "vm-2", NodeID: "node-a"},
				{ID: "vm-1", NodeID: "node-c"},
			},
			local: "node-a",
			want:  []string{"vm-1", "vm-3"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PlanReclaim(c.desired, c.local)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("PlanReclaim() = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestPlanReplica(t *testing.T) {
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
			name:    "VM replicated to this node",
			desired: []VMPlacement{{ID: "vm-1", NodeID: "node-b", ReplicaNodeID: "node-a"}},
			local:   "node-a",
			want:    []VMPlacement{{ID: "vm-1", NodeID: "node-b", ReplicaNodeID: "node-a"}},
		},
		{
			name:    "VM replicated elsewhere is not a candidate",
			desired: []VMPlacement{{ID: "vm-1", NodeID: "node-b", ReplicaNodeID: "node-c"}},
			local:   "node-a",
			want:    nil,
		},
		{
			name:    "no replica set is not a candidate",
			desired: []VMPlacement{{ID: "vm-1", NodeID: "node-b"}},
			local:   "node-a",
			want:    nil,
		},
		{
			name:    "a deleting VM is not ensured as a replica - it's reclaimed instead",
			desired: []VMPlacement{{ID: "vm-1", NodeID: "node-b", ReplicaNodeID: "node-a", Deleting: true}},
			local:   "node-a",
			want:    nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PlanReplica(c.desired, c.local)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("PlanReplica() = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestPlanReplicaReclaim(t *testing.T) {
	cases := []struct {
		name    string
		desired []VMPlacement
		local   string
		want    []string
	}{
		{
			name:    "nothing desired",
			desired: nil,
			local:   "node-a",
			want:    nil,
		},
		{
			name:    "still replicated here is not a reclaim candidate",
			desired: []VMPlacement{{ID: "vm-1", NodeID: "node-b", ReplicaNodeID: "node-a"}},
			local:   "node-a",
			want:    nil,
		},
		{
			name:    "no longer replicated here is a reclaim candidate",
			desired: []VMPlacement{{ID: "vm-1", NodeID: "node-b", ReplicaNodeID: "node-c"}},
			local:   "node-a",
			want:    []string{"vm-1"},
		},
		{
			name:    "a deleting VM is a reclaim candidate even if still named",
			desired: []VMPlacement{{ID: "vm-1", NodeID: "node-b", ReplicaNodeID: "node-a", Deleting: true}},
			local:   "node-a",
			want:    []string{"vm-1"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PlanReplicaReclaim(c.desired, c.local)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("PlanReplicaReclaim() = %+v, want %+v", got, c.want)
			}
		})
	}
}
