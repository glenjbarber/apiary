package forcerestart

import (
	"fmt"
	"io"
	"os"

	"github.com/glenjbarber/apiary/internal/commonconfig"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
	"github.com/glenjbarber/apiary/internal/restartplan"
)

// RecordPaths locates the files writeForcedRecord needs: where the
// pending-restart record goes, and where the node id that has to go in
// it is configured. Every field defaults to the same path the daemons
// themselves use, so a zero RecordPaths is a correct production
// configuration rather than a broken one.
type RecordPaths struct {
	// Dir is where pending-restart-<service>.json is written. Defaults
	// to restartplan.DefaultStateDir, the same directory managerd's own
	// RestartConfirmStore uses.
	Dir string

	// RaftdJSON, ManagerdJSON and CommonJSON are the config files
	// consulted, in that order, for this Comb's raft node id. Defaults
	// to raftdconfig.DefaultPath, nodeconfig.DefaultPath and
	// commonconfig.DefaultPath.
	RaftdJSON    string
	ManagerdJSON string
	CommonJSON   string

	// Hostname is the last-resort node id, matching what raftd and
	// managerd themselves fall back to when no config file sets one.
	// Defaults to os.Hostname().
	Hostname string
}

func (p RecordPaths) withDefaults() RecordPaths {
	if p.Dir == "" {
		p.Dir = restartplan.DefaultStateDir
	}
	if p.RaftdJSON == "" {
		p.RaftdJSON = raftdconfig.DefaultPath
	}
	if p.ManagerdJSON == "" {
		p.ManagerdJSON = nodeconfig.DefaultPath
	}
	if p.CommonJSON == "" {
		p.CommonJSON = commonconfig.DefaultPath
	}
	return p
}

// writeForcedRecord leaves the same pending-restart record a leased
// restart leaves, with lease_id 0, before s is restarted.
//
// # WHY IT STILL WRITES A RECORD
//
// force-restart bypasses the ADR-0125/ADR-0103 guardrail on purpose:
// it takes no lease, runs no quorum preflight, and coordinates with
// nothing. That much is intended and stays that way. The gap this
// closes is different. The guardrail's 600-second cooldown - the
// window during which a second voter may not restart the same service -
// is fed by RestartRecord entries in the FSM, and the only thing that
// writes one is the confirm path. So a force-restart that wrote
// nothing would leave the guardrail believing no restart had happened,
// and an operator could force-restart raftd on one Comb and be granted
// a coordinated raftd restart on another seconds later - exactly the
// concurrent-restart window the cooldown exists to close.
//
// WHY lease_id IS ZERO
//
// applyRecordRestartCompleted writes the cooldown record
// unconditionally, and releases a lease only on an exact lease_id AND
// holder_node_id match. So lease_id 0 informs the cooldown and can
// never release a lease somebody else is holding, not even this node's
// own real one. Nothing in the confirm path checks for a zero lease id.
//
// # THE TWO CASES WHERE IT DELIBERATELY WRITES NOTHING
//
//  1. A pending record already exists. That is a real lease in flight,
//     and Save overwrites. Clobbering it with lease_id 0 would strand
//     that lease permanently: the daemon would confirm lease 0, the real
//     lease would never match, and leases have no TTL. It is left alone
//     and said so loudly.
//
//  2. The node id cannot be resolved. applyAcquireRestartLease only
//     counts a record toward the cooldown when its holder is a
//     currently-known voter, so a record with a wrong or empty node_id
//     is written and never blocks anything - a silent no-op that looks
//     like it worked. Better to record nothing and say why.
//
// Every failure here is a warning, never an error. This is bookkeeping
// on the emergency path, and the one thing it must not do is stop a
// restart an operator needs during an incident.
func writeForcedRecord(s Service, paths RecordPaths, warn io.Writer) {
	paths = paths.withDefaults()

	store := restartplan.NewPendingStore(paths.Dir)
	path := fmt.Sprintf("%s/pending-restart-%s.json", paths.Dir, s.RCName())

	if existing, found, err := store.Load(s.RCName()); err != nil {
		// An unreadable or corrupt record is not a free slot. Leave
		// whatever is there and say so, rather than overwriting a
		// record this code cannot interpret.
		fmt.Fprintf(warn, "  could not read the existing pending-restart record for %s (%v).\n", s.RCName(), err)
		fmt.Fprintf(warn, "  Leaving it untouched. If it is stale, clear it by hand once\n")
		fmt.Fprintf(warn, "  you have confirmed no lease is held.\n")
		fmt.Fprintf(warn, "  The guardrail cooldown will NOT learn about this force-restart.\n")
		return
	} else if found {
		fmt.Fprintf(warn, "  a pending-restart record already exists for %s:\n", s.RCName())
		fmt.Fprintf(warn, "    %s\n", path)
		fmt.Fprintf(warn, "    (service %s, node_id %s, lease_id %d)\n", existing.Service, existing.NodeID, existing.LeaseID)
		fmt.Fprintf(warn, "  Leaving it untouched - overwriting it would strand whatever\n")
		fmt.Fprintf(warn, "  lease wrote it, since that lease has no TTL. If that is a\n")
		fmt.Fprintf(warn, "  stale record from a restart that never came back, clear it by\n")
		fmt.Fprintf(warn, "  hand once you have confirmed no lease is held.\n")
		fmt.Fprintf(warn, "  The guardrail cooldown will NOT learn about this force-restart.\n")
		return
	}

	nodeID, err := resolveNodeID(s, paths)
	if err != nil {
		fmt.Fprintf(warn, "  could not determine a node_id: %v\n", err)
		fmt.Fprintf(warn, "  Writing nothing: the guardrail only counts a restart record\n")
		fmt.Fprintf(warn, "  toward its cooldown when the holder is a known voter, so a\n")
		fmt.Fprintf(warn, "  record with no node_id would never block anything.\n")
		fmt.Fprintf(warn, "  The guardrail cooldown will NOT learn about this force-restart.\n")
		return
	}

	// LeaseID is left at its zero value deliberately - see this
	// function's doc comment. It is written explicitly so that the
	// value on disk is a stated decision rather than an omission.
	if err := store.Save(restartplan.PendingRestart{Service: s.RCName(), NodeID: nodeID, LeaseID: 0}); err != nil {
		fmt.Fprintf(warn, "  could not write %s: %v\n", path, err)
		fmt.Fprintf(warn, "  Writing nothing.\n")
		fmt.Fprintf(warn, "  The guardrail cooldown will NOT learn about this force-restart.\n")
		return
	}
	fmt.Fprintf(warn, "  recorded %s restart by voter %s (lease_id 0, no lease taken)\n", s.RCName(), nodeID)
}

// resolveNodeID finds this Comb's raft node id for the service about to
// be restarted.
//
// The order is that service's own config, then the other daemon's, then
// ADR-0111's shared common.json, then the hostname. Only the first step
// is about correctness - see Service.loadOwnNodeID for why the record
// has to name the identity the daemon being restarted would report for
// itself. The rest are about not writing nothing: the guardrail's
// cooldown is fed by this record, and a Comb left with no record is a
// Comb the guardrail believes has not restarted, which is the exact gap
// the record exists to close. A record naming a slightly wrong voter is
// inert; no record at all is a hole.
//
// A config file that cannot be parsed is treated exactly like one that
// says nothing: resolution moves on. That matches what the shell
// implementation this replaces did (a sed that matched no key), and it
// is right here too - a broken raftd.json is something to complain about
// loudly, and refusing to record the restart as well would turn a single
// bad file into a silently uncooldowned Comb.
func resolveNodeID(s Service, paths RecordPaths) (string, error) {
	// This service's own config first, through its own loader. The
	// answer that matters: it is the identity the daemon being
	// restarted would report for itself.
	if path := s.ownConfigPath(paths); path != "" {
		if id, err := s.loadNodeIDFrom(path); err == nil && id != "" {
			return id, nil
		}
	}
	// Then the other raft daemon's config, so a Comb with a damaged
	// own-config is not left with no record at all. Getting here means
	// this daemon's own file named no id or could not be loaded.
	if path := s.otherConfigPath(paths); path != "" {
		if id, err := s.loadNodeIDFrom(path); err == nil && id != "" {
			return id, nil
		}
	}

	// The shared file, on its own. In practice the two calls above have
	// already consulted it through their own loaders, so reaching this
	// means both of them failed outright - which a damaged common.json
	// will do, because each loader consults it before reading the file
	// it was pointed at.
	commonCfg, err := (&commonconfig.Manager{Path: paths.CommonJSON}).Load()
	if err == nil && commonCfg.NodeID != "" {
		return commonCfg.NodeID, nil
	}

	// The hostname, which is what raftd and managerd themselves fall
	// back to when no config file sets one.
	if paths.Hostname != "" {
		return paths.Hostname, nil
	}
	// Last, and only now an error: os.Hostname has no fallback behind
	// it, so this is genuinely the end of the chain.
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("no node_id in %s, %s or %s, and os.Hostname failed: %w",
			paths.RaftdJSON, paths.ManagerdJSON, paths.CommonJSON, err)
	}
	if host == "" {
		return "", fmt.Errorf("no node_id in %s, %s or %s, and os.Hostname is empty",
			paths.RaftdJSON, paths.ManagerdJSON, paths.CommonJSON)
	}
	return host, nil
}
