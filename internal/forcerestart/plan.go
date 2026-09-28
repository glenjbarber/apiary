package forcerestart

import (
	"fmt"
	"strings"

	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

// Service is one daemon in a force-restart plan: the daemon's short
// name, and the fixed TCP port its own listener is confirmed on.
//
// The port is not a tuning knob and is never discovered at runtime.
// Apiary's daemons each listen on a port that is part of their
// contract with every other daemon and with the web UI's forms - see
// internal/frontend/fixedport.go and ADR-0108, which is where these two
// numbers are also written down. A daemon with no port here is refused,
// not guessed at: guessing would mean restarting a daemon and then
// deciding what success looks like, and the two mistakes are the same
// mistake.
type Service struct {
	Name string
	Port int
}

// RCName is the FreeBSD rc.d service name - what `service` is given and
// what the pending-restart record's service field carries. The
// apiary_ prefix is uniform, so it is derived rather than repeated,
// which is also why a plan entry cannot disagree with its own record.
func (s Service) RCName() string { return "apiary_" + s.Name }

// String renders a plan entry the way the operator sees it in the
// restart plan, and the way the timeout diagnostic refers back to it.
func (s Service) String() string {
	return fmt.Sprintf("%s:%d", s.RCName(), s.Port)
}

// loadNodeIDFrom reads a raft node id out of one config file, using the
// loader the named daemon uses for that file. It returns "" and no
// error when the file names no id, and a non-nil error when the file
// could not be read or parsed.
//
// The loader is chosen by the service whose identity is being resolved,
// not by the file being read: raftd's id comes out of raftd's config
// through raftd's own loader. That is what makes the answer the one
// that daemon would give for itself at startup, which is the only
// answer the record can usefully carry.
//
// Note that each loader applies ADR-0111's common.json as its own
// fallback, and does so before reading the file it was pointed at. So a
// per-daemon file that names no id resolves through the shared file
// without this function having to do anything, and a damaged shared
// file makes every loader fail. That is also why this differs from the
// shell implementation it replaces, which read the three files itself
// in the order raftd, managerd, common: there, a raftd.json naming no
// id fell straight through to managerd.json and skipped the shared file
// entirely, so a raftd restart could be recorded under managerd's
// identity. Here it is recorded under the identity raftd itself would
// report. That difference is the point of moving the resolution into
// Go rather than carrying the sed across.
func (s Service) loadNodeIDFrom(path string) (string, error) {
	switch s.Name {
	case "raftd":
		cfg, err := (&raftdconfig.Manager{Path: path}).Load()
		if err != nil {
			return "", err
		}
		return cfg.NodeID, nil
	case "managerd":
		cfg, err := (&nodeconfig.Manager{Path: path}).Load()
		if err != nil {
			return "", err
		}
		return cfg.NodeID, nil
	default:
		// A daemon with no loader here is not an error: the fallback
		// chain in resolveNodeID still resolves an id for it, and the
		// preflight has already refused any daemon with no known port,
		// so this is reached only by a caller that built its own plan.
		return "", nil
	}
}

// ownConfigPath is the config file this daemon reads its own node id
// from: raftd.json for raftd, managerd.json for managerd, and nothing
// for a daemon with no loader here.
func (s Service) ownConfigPath(paths RecordPaths) string {
	switch s.Name {
	case "raftd":
		return paths.RaftdJSON
	case "managerd":
		return paths.ManagerdJSON
	}
	return ""
}

// otherConfigPath is the other raft daemon's config file - the fallback
// for a Comb whose own config is damaged. It is "" when there is no
// other file to fall back to.
func (s Service) otherConfigPath(paths RecordPaths) string {
	switch s.Name {
	case "raftd":
		return paths.ManagerdJSON
	case "managerd":
		return paths.RaftdJSON
	}
	return ""
}

// DefaultPlan is the set of daemons force-restart restarts, in the
// order it restarts them.
//
// THE ORDER IS PART OF THE PLAN, NOT AN INCIDENTAL DETAIL. managerd
// goes first because raftd's startup confirm path talks to managerd:
// raftd comes back, reads its pending-restart record, and calls
// managerd to resolve it. If raftd is restarted first it comes back
// into a node whose managerd is not running yet, the confirm fails,
// and the record is left pending - and because restart leases have no
// TTL, a lease stranded that way stays stranded until somebody clears
// it by hand. managerd tolerates raftd being down: it is a manager of
// the cluster, not a dependant on a specific member of it. The
// asymmetry decides the order.
//
// `update` restarts frontend and restshimd, which are disjoint from
// this list, so the two cannot race each other over the same daemon.
var DefaultPlan = []Service{
	{Name: "managerd", Port: 17700},
	{Name: "raftd", Port: 17600},
}

// renderPlan formats the whole plan on one line, in order, for the
// announcement printed before anything is restarted.
func renderPlan(plan []Service) string {
	parts := make([]string, 0, len(plan))
	for _, s := range plan {
		parts = append(parts, s.String())
	}
	return strings.Join(parts, " ")
}

// ErrNoKnownPort reports a plan entry with no fixed listener port. It
// is a refusal that happens before the first restart, so the caller
// can report it with the same certainty as "nothing was touched".
type ErrNoKnownPort struct {
	Service string
}

func (e *ErrNoKnownPort) Error() string {
	return fmt.Sprintf("no known listener port for %s, and force-restart will not guess one", e.Service)
}
