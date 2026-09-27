package manager

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// apiaryServices is the full status inventory for this Machine page.
// restshimd is restartable (ADR-0102): it's stateless and
// non-consensus, and UpdateRestshimdConfig needs to be able to trigger
// a restart after a successful config save.
//
// apiary_raftd became restartable in ADR-0125, which is what finally
// made it safe to expose: restarting it is now the single most
// consequential restart in this inventory, so it is also the one most
// heavily guarded. RestartNodeService routes it through the same
// cluster-wide Raft lease as apiary_managerd, and additionally through
// the quorum-safety evaluation that decides whether the remaining
// voters would still form a majority (internal/manager/raftdquorum.go).
// It stays out of the auto-restart-on-config-write path for the same
// reason it always did: UpdateRaftdConfig writing a new value is not an
// operator asking to bounce consensus, and the config's own save flow
// must never be able to trigger that implicitly.
//
// apiary_managerd is NOT restartable, and that is a correction rather
// than a limitation. Every restart this platform performs is orchestrated
// by a goroutine running INSIDE the calling managerd, which sleeps 250ms
// and then synchronously runs `service <name> restart`. When the target
// is managerd itself, the stop half of that command kills the very
// process the goroutine lives in, so the start half never runs: the
// service stops and does not come back. This was reproduced on two
// Combs, not inferred - managerd stopped, never returned, and a manual
// `service apiary_managerd start` was needed each time.
//
// There is no ordering trick that fixes it, because the failure is
// structural rather than a race: any in-process orchestration of your own
// death has the same problem. So the honest response is to refuse, and to
// point the operator at the path that does work - `make force-restart`,
// which restarts managerd from rc.d in a process that is not managerd,
// and is documented in ADR-0141. Failing safe is the right call here
// even though the guarded RPC path is strictly better than bare
// `service`: a guardrail that stops a daemon and cannot start it again is
// worse than one that declines to act, because the operator is now
// debugging a downed control plane instead of reading a refusal.
//
// See managerdSelfRestartRefusal for the operator-facing message.
var apiaryServices = []struct {
	name        string
	restartable bool
}{
	{name: "apiary_raftd", restartable: true},
	{name: managerdServiceName, restartable: false},
	{name: "apiary_frontend", restartable: true},
	{name: "apiary_restshimd", restartable: true},
}

// managerdSelfRestartRefusal is the operator-facing explanation returned
// whenever anything asks managerd to restart itself. It names the
// working alternative rather than only the failure, because "cannot be
// restarted from Apiary" on its own would leave the operator with a dead
// daemon and no next step.
const managerdSelfRestartRefusal = "apiary_managerd cannot be restarted through the API: the restart is " +
	"orchestrated from inside the managerd being restarted, so stopping it kills the process that was " +
	"about to start it, and the daemon does not come back. Restart it on the node instead, with " +
	"`make force-restart` (restarts managerd and then raftd, deliberately bypassing the restart " +
	"lease and quorum preflight) or `service apiary_managerd restart` if you only need managerd."

// managerdSelfRestartRefused reports whether name is managerd itself, the
// one service in the inventory that must never be restarted from inside
// managerd. Checked ahead of the generic restartableService test so the
// operator gets this specific explanation instead of the generic
// "cannot be restarted from Apiary".
func managerdSelfRestartRefused(name string) bool {
	return name == managerdServiceName
}

// managerdServiceName is the rc.d service name for the manager daemon.
// Named as a constant so the refusal predicate, the inventory entry and
// the tests that assert the two agree cannot drift apart by typo.
const managerdServiceName = "apiary_managerd"

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
