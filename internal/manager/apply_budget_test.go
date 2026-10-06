package manager

import (
	"context"
	"testing"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"google.golang.org/grpc"
)

type applyBudgetClient struct {
	internalpb.RaftInternalClient
	observed uint32
}

func (c *applyBudgetClient) Apply(_ context.Context, req *internalpb.ApplyRequest, _ ...grpc.CallOption) (*internalpb.ApplyResponse, error) {
	c.observed = req.GetTimeoutMs()
	return &internalpb.ApplyResponse{Error: "captured budget"}, nil
}

func TestApplyCommandBudget_DefaultAndExplicitOverride(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested uint32
		want      uint32
	}{
		{"unset uses six seconds", 0, 6000},
		{"explicit longer budget is preserved", 12000, 12000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &applyBudgetClient{}
			server := &Server{raft: &RaftClient{client: client}}
			_, appErr, _ := server.applyCommand(context.Background(), &internalpb.Command{}, tc.requested)
			if appErr != "captured budget" {
				t.Fatalf("apply did not reach the client: %q", appErr)
			}
			if client.observed != tc.want {
				t.Fatalf("wire budget = %d ms, want %d ms", client.observed, tc.want)
			}
		})
	}
}
