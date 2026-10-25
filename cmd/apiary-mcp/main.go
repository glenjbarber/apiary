// Command apiary-mcp exposes a narrowly allowlisted, read-only MCP view of
// an Apiary ManagerService.
//
// Authorization is enforced by managerd, not here: every call carries the
// configured API key, and managerd's own role map decides whether that key
// may read. The key should be a Viewer key. This process only ever calls the
// five read RPCs listed in readMethods, so even an over-privileged key cannot
// reach a mutation through this connector.
package main

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const requestTimeout = 10 * time.Second

// readMethods is the complete set of ManagerService RPCs this connector may
// call. Tests assert that every tool stays inside it.
var readMethods = []string{"ListVMs", "ListJails", "GetVM", "GetJail", "ClusterHealth"}

type config struct {
	managerAddr string
	apiKey      string
	caFile      string
	serverName  string
	transport   string
	listen      string
	mcpToken    string
	client      rpcpb.ManagerServiceClient
}

func main() {
	transport := flag.String("transport", env("APIARY_MCP_TRANSPORT", "stdio"), "MCP transport: stdio or http")
	flag.Parse()

	cfg, err := loadConfig(*transport)
	if err != nil {
		log.Fatal(err)
	}
	conn, err := dialManager(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	cfg.client = rpcpb.NewManagerServiceClient(conn)

	server := newMCPServer(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.transport == "stdio" {
		if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("MCP server stopped: %v", err)
		}
		return
	}
	if err := serveHTTP(ctx, cfg, server); err != nil {
		log.Fatal(err)
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func loadConfig(transport string) (config, error) {
	cfg := config{
		managerAddr: strings.TrimSpace(os.Getenv("APIARY_MANAGER_ADDR")),
		apiKey:      os.Getenv("APIARY_API_KEY"),
		caFile:      strings.TrimSpace(os.Getenv("APIARY_MANAGER_CA")),
		serverName:  strings.TrimSpace(os.Getenv("APIARY_MANAGER_SERVER_NAME")),
		transport:   strings.ToLower(strings.TrimSpace(transport)),
		listen:      env("APIARY_MCP_LISTEN", "127.0.0.1:8787"),
		mcpToken:    os.Getenv("APIARY_MCP_TOKEN"),
	}
	if cfg.managerAddr == "" || cfg.apiKey == "" {
		return config{}, errors.New("APIARY_MANAGER_ADDR and APIARY_API_KEY are required")
	}
	if cfg.transport != "stdio" && cfg.transport != "http" {
		return config{}, fmt.Errorf("unsupported MCP transport %q (use stdio or http)", cfg.transport)
	}
	if cfg.transport == "http" {
		if len(cfg.mcpToken) < 32 {
			return config{}, errors.New("APIARY_MCP_TOKEN is required for HTTP mode and must be at least 32 characters")
		}
		host, _, err := net.SplitHostPort(cfg.listen)
		if err != nil {
			return config{}, fmt.Errorf("invalid APIARY_MCP_LISTEN: %w", err)
		}
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return config{}, errors.New("HTTP mode only binds to loopback; expose it through a TLS reverse proxy or private tunnel")
		}
	}
	return cfg, nil
}

func dialManager(cfg config) (*grpc.ClientConn, error) {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if cfg.caFile != "" {
		pem, err := os.ReadFile(cfg.caFile)
		if err != nil {
			return nil, fmt.Errorf("read APIARY_MANAGER_CA: %w", err)
		}
		if ok := roots.AppendCertsFromPEM(pem); !ok {
			return nil, errors.New("APIARY_MANAGER_CA contains no usable certificates")
		}
	}
	serverName := cfg.serverName
	if serverName == "" {
		serverName, _, _ = net.SplitHostPort(cfg.managerAddr)
	}
	return grpc.NewClient(cfg.managerAddr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: serverName})))
}

func authContext(ctx context.Context, key string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+key)
}

// unavailable turns a failed ManagerService call into a tool error that names
// only the gRPC status code. The backend's own message is not echoed: it can
// carry internal detail that has no business reaching an external AI client.
func unavailable(what string, err error) error {
	code := status.Code(err)
	if code == codes.PermissionDenied || code == codes.Unauthenticated {
		return fmt.Errorf("Apiary denied %s: the API key must have the Viewer role", what)
	}
	return fmt.Errorf("Apiary %s unavailable (%s)", what, code)
}

// audited logs one line per tool call: the tool, the outcome and the
// duration. Arguments and results are not logged, and neither is the key.
func audited[In, Out any](name string, fn func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error)) func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		start := time.Now()
		res, out, err := fn(ctx, req, in)
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		log.Printf("audit tool=%s outcome=%s duration=%s", name, outcome, time.Since(start).Round(time.Millisecond))
		return res, out, err
	}
}

type listCellsInput struct{}
type listCellsOutput struct {
	Cells     []cellSummary `json:"cells"`
	Truncated bool          `json:"truncated"`
}
type cellSummary struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	NodeID       string `json:"node_id,omitempty"`
	DesiredState string `json:"desired_state,omitempty"`
	Phase        string `json:"phase,omitempty"`
}

// getCellInput is get_cell's input.
//
// The jsonschema tags are the schema's DOCUMENTATION, and a model reads
// them to decide what to pass. They say what each argument identifies
// and what happens when it is wrong, which is more than a field name
// can. The SDK infers the schema's shape from the Go type, so these
// tags are the only place the guidance can live.
type getCellInput struct {
	// ID identifies the cell. A cell's ID is unique only WITHIN its
	// type, so Type is needed alongside it to identify one. Get it
	// from list_cells.
	ID string `json:"id" jsonschema:"The ID of the cell to read, as returned by list_cells."`
	// Type says which kind of cell the ID names. It must be exactly
	// "vm" or "jail"; a jail's ID with type "vm" finds nothing.
	Type string `json:"type" jsonschema:"Which kind of cell the ID names: exactly 'vm' or 'jail'."`
}

type healthInput struct{}
type healthOutput struct {
	LocalNodeID string             `json:"local_node_id,omitempty"`
	Unavailable bool               `json:"unavailable"`
	Nodes       []healthNodeResult `json:"nodes"`
	// Truncated says the node list was cut off. Without it, a
	// truncated list is indistinguishable from a complete one.
	Truncated bool `json:"truncated"`
}

// healthNodeResult is one Comb's health verdict.
//
// Unknown is the field that matters most here, and it is NOT the same
// as "the status string is literally unknown". api/rpc/manager.proto
// says of this status field:
//
//	status is one of "healthy", "degraded", "unknown", "stale", or
//	"contradictory" - or any other string if a newer managerd computes
//	a state this consumer has never heard of. Treat an unrecognized
//	status as unknown, never as healthy: that is the whole failure mode
//	Evidence-Aware Health exists to prevent, and the string contract
//	cannot enforce it on the consumer's behalf.
//
// The contract cannot be enforced on the CONSUMER's behalf, which means
// it has to be enforced HERE - this connector IS the consumer, and a
// model reading this JSON cannot be asked to know which strings are
// new. So an unrecognized status is reported as unknown, and the raw
// string is still carried in Status so a caller that knows about it can
// see it.
type healthNodeResult struct {
	NodeID string `json:"node_id"`
	// Status is the raw verdict string from managerd.
	Status string `json:"status"`
	// Dialed is whether the Comb answered at all.
	Dialed bool `json:"dialed"`
	// Unknown is true when the verdict is ABSENT rather than good:
	// either managerd said "unknown", or it said something this
	// connector does not recognise. It is never true for a verdict
	// this connector does recognise.
	Unknown bool `json:"unknown"`
}

func newMCPServer(cfg config) *mcp.Server {
	// The server-level instructions are the model-agnostic part: they
	// carry the conventions every tool depends on, so the tool
	// descriptions themselves can stay short and factual. See
	// modelagnostic.go for what "model agnostic" means here.
	s := mcp.NewServer(&mcp.Implementation{Name: "apiary-mcp", Version: "0.1.0"}, &mcp.ServerOptions{
		Instructions: serverInstructions,
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_cells",
		Title:       "List cells",
		Description: listCellsDescription,
		Annotations: readOnlyAnnotations("List cells"),
	}, audited("list_cells", func(ctx context.Context, _ *mcp.CallToolRequest, _ listCellsInput) (*mcp.CallToolResult, listCellsOutput, error) {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		vmResp, err := cfg.client.ListVMs(authContext(ctx, cfg.apiKey), &rpcpb.ListVMsRequest{})
		if err != nil {
			return nil, listCellsOutput{}, unavailable("VM inventory", err)
		}
		if vmResp.GetError() != "" {
			return nil, listCellsOutput{}, errors.New("Apiary VM inventory unavailable")
		}
		jailResp, err := cfg.client.ListJails(authContext(ctx, cfg.apiKey), &rpcpb.ListJailsRequest{})
		if err != nil {
			return nil, listCellsOutput{}, unavailable("jail inventory", err)
		}
		if jailResp.GetError() != "" {
			return nil, listCellsOutput{}, errors.New("Apiary jail inventory unavailable")
		}
		out := listCellsOutput{}
		for _, vm := range vmResp.GetVms() {
			if vm != nil {
				out.Cells = append(out.Cells, cellSummary{ID: vm.GetId(), Name: vm.GetName(), Type: "vm", NodeID: vm.GetNodeId(), DesiredState: vm.GetDesiredState().String(), Phase: vm.GetPhase().String()})
			}
		}
		for _, jail := range jailResp.GetJails() {
			if jail != nil {
				out.Cells = append(out.Cells, cellSummary{ID: jail.GetId(), Name: jail.GetName(), Type: "jail", NodeID: jail.GetNodeId(), DesiredState: jail.GetDesiredState().String(), Phase: jail.GetPhase().String()})
			}
		}
		if len(out.Cells) > 200 {
			out.Cells = out.Cells[:200]
			out.Truncated = true
		}
		return nil, out, nil
	}))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_cell",
		Title:       "Get cell",
		Description: getCellDescription,
		Annotations: readOnlyAnnotations("Get cell"),
	}, audited("get_cell", func(ctx context.Context, _ *mcp.CallToolRequest, in getCellInput) (*mcp.CallToolResult, cellSummary, error) {
		if in.ID == "" || len(in.ID) > 256 || (in.Type != "vm" && in.Type != "jail") {
			return nil, cellSummary{}, errors.New("type must be vm or jail and id must be between 1 and 256 characters")
		}
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		if in.Type == "vm" {
			resp, err := cfg.client.GetVM(authContext(ctx, cfg.apiKey), &rpcpb.GetVMRequest{Id: in.ID})
			if err != nil {
				return nil, cellSummary{}, unavailable("VM lookup", err)
			}
			if resp.GetError() != "" {
				return nil, cellSummary{}, errors.New("Apiary VM lookup unavailable")
			}
			vm := resp.GetVm()
			if !resp.GetFound() || vm == nil {
				return nil, cellSummary{}, errors.New("VM not found")
			}
			return nil, cellSummary{ID: vm.GetId(), Name: vm.GetName(), Type: "vm", NodeID: vm.GetNodeId(), DesiredState: vm.GetDesiredState().String(), Phase: vm.GetPhase().String()}, nil
		}
		resp, err := cfg.client.GetJail(authContext(ctx, cfg.apiKey), &rpcpb.GetJailRequest{Id: in.ID})
		if err != nil {
			return nil, cellSummary{}, unavailable("jail lookup", err)
		}
		if resp.GetError() != "" {
			return nil, cellSummary{}, errors.New("Apiary jail lookup unavailable")
		}
		jail := resp.GetJail()
		if !resp.GetFound() || jail == nil {
			return nil, cellSummary{}, errors.New("jail not found")
		}
		return nil, cellSummary{ID: jail.GetId(), Name: jail.GetName(), Type: "jail", NodeID: jail.GetNodeId(), DesiredState: jail.GetDesiredState().String(), Phase: jail.GetPhase().String()}, nil
	}))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_cluster_health",
		Title:       "Get cluster health",
		Description: getClusterHealthDescription,
		Annotations: readOnlyAnnotations("Get cluster health"),
	}, audited("get_cluster_health", func(ctx context.Context, _ *mcp.CallToolRequest, _ healthInput) (*mcp.CallToolResult, healthOutput, error) {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		resp, err := cfg.client.ClusterHealth(authContext(ctx, cfg.apiKey), &rpcpb.ClusterHealthRequest{})
		if err != nil {
			return nil, healthOutput{}, unavailable("cluster health", err)
		}
		out := healthOutput{LocalNodeID: resp.GetLocalNodeId(), Unavailable: resp.GetError() != "", Nodes: make([]healthNodeResult, 0, len(resp.GetNodes()))}
		for _, node := range resp.GetNodes() {
			if node != nil {
				out.Nodes = append(out.Nodes, healthNodeResult{NodeID: node.GetNodeId(), Status: node.GetStatus(), Dialed: node.GetDialed(), Unknown: !recognizedHealthStatus(node.GetStatus())})
			}
		}
		if len(out.Nodes) > 100 {
			out.Nodes = out.Nodes[:100]
			out.Truncated = true
		}
		return nil, out, nil
	}))
	return s
}

func serveHTTP(ctx context.Context, cfg config, server *mcp.Server) error {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	mux := http.NewServeMux()
	mux.Handle("/mcp", bearerGuard(cfg.mcpToken, handler))
	httpServer := &http.Server{Addr: cfg.listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	log.Printf("Apiary MCP listening on loopback at %s/mcp", cfg.listen)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func bearerGuard(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got := strings.TrimPrefix(header, "Bearer ")
		if len(got) != len(token) || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
