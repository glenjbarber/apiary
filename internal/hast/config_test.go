package hast

import "testing"

func TestRenderConfig(t *testing.T) {
	resources := []Resource{
		{
			Name: "test",
			Nodes: []Node{
				{Name: "node-a", Local: "/dev/da1", Remote: "10.0.0.2"},
				{Name: "node-b", Local: "/dev/da1", Remote: "10.0.0.1"},
			},
		},
	}

	got, err := RenderConfig(resources)
	if err != nil {
		t.Fatalf("RenderConfig() error: %v", err)
	}

	want := `resource test {
  on node-a {
    local /dev/da1
    remote 10.0.0.2
  }
  on node-b {
    local /dev/da1
    remote 10.0.0.1
  }
}
`
	if got != want {
		t.Errorf("RenderConfig() =\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderConfig_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name      string
		resources []Resource
	}{
		{"empty resource name", []Resource{{Name: "", Nodes: []Node{{Name: "a", Local: "/dev/da1", Remote: "x"}, {Name: "b", Local: "/dev/da1", Remote: "y"}}}}},
		{"wrong node count", []Resource{{Name: "test", Nodes: []Node{{Name: "a", Local: "/dev/da1", Remote: "x"}}}}},
		{"missing node field", []Resource{{Name: "test", Nodes: []Node{{Name: "a", Local: "", Remote: "x"}, {Name: "b", Local: "/dev/da1", Remote: "y"}}}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := RenderConfig(c.resources); err == nil {
				t.Errorf("RenderConfig() = nil error, want rejection")
			}
		})
	}
}

func TestParseStatus_Primary(t *testing.T) {
	out := `test:
  role: primary
  provname: test
  localpath: /dev/md10
  extentsize: 2097152 (2.0MB)
  keepdirty: 64
  remoteaddr: 10.50.0.12
  replication: memsync
  status: degraded
  workerpid: 2363
  dirty: 0 (0B)
  statistics:
    reads: 0
    writes: 0
    deletes: 0
    flushes: 0
    activemap updates: 0
    local errors: read: 0, write: 0, delete: 0, flush: 0
    queues: local: 0, send: 0, recv: 0, done: 0, idle: 256`

	s, err := parseStatus(out)
	if err != nil {
		t.Fatalf("parseStatus() error: %v", err)
	}
	if s.Role != "primary" {
		t.Errorf("Role = %q, want %q", s.Role, "primary")
	}
	if s.LocalPath != "/dev/md10" {
		t.Errorf("LocalPath = %q, want %q", s.LocalPath, "/dev/md10")
	}
	if s.RemoteAddr != "10.50.0.12" {
		t.Errorf("RemoteAddr = %q, want %q", s.RemoteAddr, "10.50.0.12")
	}
	if s.ResourceStatus != "degraded" {
		t.Errorf("ResourceStatus = %q, want %q", s.ResourceStatus, "degraded")
	}
}

func TestParseStatus_Secondary(t *testing.T) {
	// A secondary's hastctl list output has no top-level "status:" line.
	out := `test:
  role: secondary
  provname: test
  localpath: /dev/md10
  extentsize: 0 (0B)
  keepdirty: 0
  remoteaddr: 10.50.0.11
  replication: memsync
  dirty: 0 (0B)`

	s, err := parseStatus(out)
	if err != nil {
		t.Fatalf("parseStatus() error: %v", err)
	}
	if s.Role != "secondary" {
		t.Errorf("Role = %q, want %q", s.Role, "secondary")
	}
	if s.ResourceStatus != "" {
		t.Errorf("ResourceStatus = %q, want empty for a secondary", s.ResourceStatus)
	}
}
