package manager

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// apiaryServices is the full status inventory for this Machine page. Only
// managerd and frontend are restartable in v1: raftd is consensus-critical,
// and restshimd is not currently a user-facing web dependency.
var apiaryServices = []struct {
	name        string
	restartable bool
}{
	{name: "apiary_raftd"},
	{name: "apiary_managerd", restartable: true},
	{name: "apiary_frontend", restartable: true},
	{name: "apiary_restshimd"},
}

type rcServiceController struct{}

func (rcServiceController) List(ctx context.Context) ([]*rpcpb.NodeService, error) {
	services := make([]*rpcpb.NodeService, 0, len(apiaryServices))
	for _, entry := range apiaryServices {
		status, detail := rcServiceStatus(ctx, entry.name)
		enabled, enabledDetail := rcServiceEnabled(ctx, entry.name)
		if enabledDetail != "" {
			if detail != "" {
				detail += "; "
			}
			detail += enabledDetail
		}
		services = append(services, &rpcpb.NodeService{
			Name: entry.name, Status: status, Enabled: enabled,
			Detail: detail, Restartable: entry.restartable,
		})
	}
	return services, nil
}

func (rcServiceController) Restart(ctx context.Context, name string) error {
	if !restartableService(name) {
		return fmt.Errorf("service %q cannot be restarted from Apiary", name)
	}
	out, err := exec.CommandContext(ctx, "service", name, "restart").CombinedOutput()
	if err != nil {
		return fmt.Errorf("service %s restart: %s", name, commandError(err, out))
	}
	return nil
}

func rcServiceStatus(ctx context.Context, name string) (string, string) {
	out, err := exec.CommandContext(ctx, "service", name, "onestatus").CombinedOutput()
	if err == nil {
		return "running", ""
	}
	text := strings.TrimSpace(string(out))
	if strings.Contains(strings.ToLower(text), "not running") ||
		strings.Contains(strings.ToLower(text), "not found") {
		return "stopped", ""
	}
	return "unknown", commandError(err, out)
}

func rcServiceEnabled(ctx context.Context, name string) (bool, string) {
	out, err := exec.CommandContext(ctx, "sysrc", "-n", name+"_enable").CombinedOutput()
	if err != nil {
		return false, "could not read rc.conf: " + commandError(err, out)
	}
	return strings.EqualFold(strings.TrimSpace(string(out)), "YES"), ""
}

func restartableService(name string) bool {
	for _, entry := range apiaryServices {
		if entry.name == name {
			return entry.restartable
		}
	}
	return false
}

func commandError(err error, out []byte) string {
	if text := strings.TrimSpace(string(out)); text != "" {
		return text
	}
	return err.Error()
}
