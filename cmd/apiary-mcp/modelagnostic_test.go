package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The model-agnostic properties (issue #26, Glen's "make it model
// agnostic" requirement).
//
// These tests are about PROPERTIES a client can rely on regardless of
// what model is behind it, not about the shape of any particular
// implementation. A model that has never seen Apiary, a model that has,
// a rule engine driving the tools directly and a person reading the JSON
// all get the same guarantees from them.

// TestEveryToolIsAnnotatedReadOnly checks the protocol's own
// machine-readable statement that these tools cannot mutate anything.
//
// This is the annotation a client - or a host that gates tool use - reads
// to decide what it is allowed to call, and it is the thing that makes
// the surface model-independent: a client does not have to infer
// read-only-ness from a prose description, and a model does not have to
// be told.
func TestEveryToolIsAnnotatedReadOnly(t *testing.T) {
	s := connect(t, &fakeManager{})
	list, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(list.Tools) == 0 {
		t.Fatal("no tools registered")
	}
	for _, tool := range list.Tools {
		if tool.Annotations == nil {
			t.Errorf("tool %q has no annotations; every tool must declare that it is read-only", tool.Name)
			continue
		}
		if !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %q is not annotated read-only; every tool here must be", tool.Name)
		}
		if tool.Annotations.Title == "" {
			t.Errorf("tool %q has no title; a client that displays tool names verbatim must show something a person can read", tool.Name)
		}
	}
}

// TestEveryToolDescriptionCarriesItsOwnSemantics checks that a
// description is self-sufficient.
//
// A description is the only thing every model reads. One that says only
// "List Apiary VMs and jails" leaves every model to guess what the
// result fields mean, what the tool does NOT do, and what to do about a
// failure - and different models guess differently, which is exactly the
// model-dependence this work removes.
func TestEveryToolDescriptionCarriesItsOwnSemantics(t *testing.T) {
	s := connect(t, &fakeManager{})
	list, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range list.Tools {
		desc := tool.Description
		if strings.TrimSpace(desc) == "" {
			t.Errorf("tool %q has an empty description", tool.Name)
			continue
		}
		// Each description must name what the tool returns, what its
		// arguments mean, and what a failure means. These are the
		// three things a model cannot infer.
		for _, needed := range []string{"Returns", "not"} {
			if !strings.Contains(desc, needed) {
				t.Errorf("tool %q's description does not mention %q; it must carry its own semantics rather than relying on the model to infer them", tool.Name, needed)
			}
		}
	}
}

// TestTheServerSendsInstructionsToTheClient checks the server-level
// guidance arrives over the protocol, which is how a client learns the
// conventions the tools depend on without every tool having to repeat
// them.
func TestTheServerSendsInstructionsToTheClient(t *testing.T) {
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := newMCPServer(config{client: &fakeManager{}, apiKey: "test-key"}).Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	initial := session.InitializeResult()
	if initial == nil {
		t.Fatal("the session has no initialize result")
	}
	instructions := initial.Instructions
	if strings.TrimSpace(instructions) == "" {
		t.Fatal("the server sent no instructions; the conventions every tool depends on have to reach the client somehow")
	}
	// The instructions must state the conventions a model cannot infer
	// from a tool call alone.
	for _, needed := range []string{"READ-ONLY", "truncated", "not found"} {
		if !strings.Contains(instructions, needed) {
			t.Errorf("the server instructions do not mention %q; they must carry the conventions a model cannot infer from a single tool call", needed)
		}
	}
}

// TestAnUnrecognizedHealthStatusIsNeverReportedAsKnown is the
// correctness half of the model-agnostic work.
//
// api/rpc/manager.proto says a NEWER managerd may return a status
// string this consumer has never heard of, and that it must be treated
// as unknown and NEVER as healthy - and that the string contract
// "cannot enforce it on the consumer's behalf". This connector IS the
// consumer, and a model behind it cannot be asked to know which strings
// are new. So the connector enforces it.
func TestAnUnrecognizedHealthStatusIsNeverReportedAsKnown(t *testing.T) {
	fake := &fakeManager{
		health: &rpcpb.ClusterHealthResponse{
			LocalNodeId: "node-a",
			Nodes: []*rpcpb.ClusterNodeHealth{
				{NodeId: "node-a", Status: "healthy", Dialed: true},
				{NodeId: "node-b", Status: "degraded", Dialed: true},
				{NodeId: "node-c", Status: "unknown", Dialed: false},
				// A status from a NEWER managerd that this connector has
				// never heard of.
				{NodeId: "node-d", Status: "quiescing", Dialed: true},
			},
		},
	}
	var out healthOutput
	res := call(t, connect(t, fake), "get_cluster_health", map[string]any{})
	if err := json.Unmarshal([]byte(text(t, res)), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(out.Nodes) != 4 {
		t.Fatalf("nodes = %d, want 4", len(out.Nodes))
	}
	byID := map[string]healthNodeResult{}
	for _, n := range out.Nodes {
		byID[n.NodeID] = n
	}
	for _, id := range []string{"node-a", "node-b"} {
		if byID[id].Unknown {
			t.Errorf("node %q is a known status but was reported as unknown", id)
		}
	}
	if !byID["node-c"].Unknown {
		t.Errorf("node-c's status is literally \"unknown\" but was not reported as unknown")
	}
	if !byID["node-d"].Unknown {
		t.Errorf("node-d's status %q is unrecognized and must be reported as unknown, never as a known verdict", byID["node-d"].Status)
	}
	// The raw string is still carried, so a caller that DOES know about
	// it can see it.
	if byID["node-d"].Status != "quiescing" {
		t.Errorf("node-d's raw status = %q, want the original string preserved", byID["node-d"].Status)
	}
}

// TestAnEmptyHealthStatusIsUnknown checks the degenerate case: no
// verdict at all is not a verdict.
func TestAnEmptyHealthStatusIsUnknown(t *testing.T) {
	fake := &fakeManager{
		health: &rpcpb.ClusterHealthResponse{
			Nodes: []*rpcpb.ClusterNodeHealth{{NodeId: "node-a", Status: ""}},
		},
	}
	var out healthOutput
	res := call(t, connect(t, fake), "get_cluster_health", map[string]any{})
	if err := json.Unmarshal([]byte(text(t, res)), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Nodes) != 1 || !out.Nodes[0].Unknown {
		t.Errorf("an empty status must be reported as unknown, got %+v", out.Nodes)
	}
}

// TestEveryKnownHealthStatusIsRecognized guards the other direction of
// the vocabulary: a status that IS a real verdict must be recognized, or
// the connector would report real verdicts as unknown and be useless.
//
// Note that "unknown" is deliberately NOT one of them. It is managerd's
// own name for the ABSENCE of a verdict, so it is reported as unknown -
// which is what the separate test above asserts.
func TestEveryKnownHealthStatusIsRecognized(t *testing.T) {
	for _, status := range []string{"healthy", "degraded", "stale", "contradictory"} {
		if !recognizedHealthStatus(status) {
			t.Errorf("status %q is a real verdict and must be recognized", status)
		}
	}
}

// TestAnAbsentVerdictIsUnknownWhateverItIsCalled checks that the
// absence of a verdict is reported as unknown under every spelling -
// managerd's own "unknown", an empty string, and a status from a newer
// managerd this connector has never heard of.
func TestAnAbsentVerdictIsUnknownWhateverItIsCalled(t *testing.T) {
	for _, status := range []string{"unknown", "", "quiescing", "Healthy", "HEALTHY"} {
		if recognizedHealthStatus(status) {
			t.Errorf("status %q is not a real verdict and must be reported as unknown", status)
		}
	}
}

// TestEveryTruncatedCollectionSaysSo checks the property that makes a
// partial result distinguishable from a complete one.
//
// A model reading a truncated list that does not say it was truncated
// will state a falsehood to the user - "there are 200 cells" when there
// are 300. The flag is what stops that.
func TestEveryTruncatedCollectionSaysSo(t *testing.T) {
	// The cell list.
	fake := &fakeManager{}
	for i := 0; i < 150; i++ {
		fake.vms = append(fake.vms, &rpcpb.VMDefinition{Id: "vm-" + strings.Repeat("x", 1) + string(rune('a'+i%26))})
	}
	for i := 0; i < 100; i++ {
		fake.jails = append(fake.jails, &rpcpb.JailDefinition{Id: "jail-" + string(rune('a'+i%26))})
	}
	var cells listCellsOutput
	res := call(t, connect(t, fake), "list_cells", map[string]any{})
	if err := json.Unmarshal([]byte(text(t, res)), &cells); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !cells.Truncated {
		t.Errorf("a truncated cell list must say it was truncated")
	}

	// The node list.
	health := &rpcpb.ClusterHealthResponse{}
	for i := 0; i < 150; i++ {
		health.Nodes = append(health.Nodes, &rpcpb.ClusterNodeHealth{NodeId: "node-" + string(rune('a'+i%26)), Status: "healthy"})
	}
	var out healthOutput
	res = call(t, connect(t, &fakeManager{health: health}), "get_cluster_health", map[string]any{})
	if err := json.Unmarshal([]byte(text(t, res)), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Truncated {
		t.Errorf("a truncated node list must say it was truncated")
	}
}

// TestTheToolSurfaceIsStillTheSameThreeTools checks that making the
// surface model-agnostic did not grow it.
//
// A model-agnostic surface is not one with a tool per model or a
// vendor-specific adapter; it is the same small set of tools that
// describe themselves properly. Growing the set would make the surface
// MORE model-dependent, not less.
func TestTheToolSurfaceIsStillTheSameThreeTools(t *testing.T) {
	s := connect(t, &fakeManager{})
	list, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if want := []string{"get_cell", "get_cluster_health", "list_cells"}; !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
}

// TestTheInputSchemaDocumentsItsArguments checks that the argument
// documentation reaches the client over the protocol. A model reads the
// schema, not the Go source, so guidance that lives only in a comment
// is guidance no model ever sees.
func TestTheInputSchemaDocumentsItsArguments(t *testing.T) {
	s := connect(t, &fakeManager{})
	list, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range list.Tools {
		if tool.Name != "get_cell" {
			continue
		}
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok {
			t.Fatalf("get_cell's input schema is %T, want a JSON object", tool.InputSchema)
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("get_cell's input schema has no properties map: %v", schema)
		}
		for _, field := range []string{"id", "type"} {
			prop, ok := props[field].(map[string]any)
			if !ok {
				t.Errorf("get_cell's schema has no %q property: %v", field, props)
				continue
			}
			desc, _ := prop["description"].(string)
			if strings.TrimSpace(desc) == "" {
				t.Errorf("get_cell's %q parameter has no description; a model reads the schema, not the source", field)
			}
		}
	}
}

// TestEveryToolHasAnOutputSchema checks that the result's shape is
// published. A model reading a result benefits from knowing the shape
// in advance, and a client that validates results needs it.
func TestEveryToolHasAnOutputSchema(t *testing.T) {
	s := connect(t, &fakeManager{})
	list, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range list.Tools {
		if tool.OutputSchema == nil {
			t.Errorf("tool %q publishes no output schema", tool.Name)
		}
	}
}
