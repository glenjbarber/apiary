// ADR-0156 Stage 1: the data-directory sidecar, and the startup guard
// that reads it.
//
// WHAT THIS IS FOR: a small JSON file beside the raft data, recording
// which Colony this node's data directory was last part of. It exists so
// that a node which self-bootstraps into an EMPTY data directory - the
// one way to silently form a second, unrelated Colony - can be caught.
//
// THE FAILURE IT PREVENTS: ADR-0038's Tier 1 reset wipes a node's raft
// state. Before ADR-0156, the next normal start would bootstrap a fresh
// single-node cluster in that now-empty directory, and NOTHING in the
// system could tell that this new cluster was not the Colony the node
// used to be part of. The node would believe it was a member of its
// Colony, its leader would believe it was a member of its Colony, and
// the two would be in different clusters - the split-leader condition
// ADR-0156 records as verified to exist.
//
// WHY A SIDECAR RATHER THAN A FLAG IN THE DATA DIRECTORY: the raft data
// itself is wiped by a reset, so anything stored only there is lost
// exactly when it is needed. The sidecar lives OUTSIDE the wiped
// content but INSIDE the data directory, so a reset that moves the
// directory aside (raftd's -reset renames it, it does not delete it)
// carries the sidecar with it - which is what lets an operator see
// which Colony the moved-aside data belonged to.
//
// WHY IT IS NOT THE AUTHORITY: the replicated identity is. The sidecar
// is a HINT that this directory used to hold state for some Colony. The
// guard REFUSES a self-bootstrap when the hint is present; it does not
// decide which Colony this node is in. That is decided, always, by the
// replicated state a join brings in.

package raft

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// sidecarFileName is the file the sidecar is written to, inside the
// node's data directory.
const sidecarFileName = "colony-identity.json"

// ColonySidecar is the on-disk record of which Colony a node's data
// directory was last part of.
//
// It is written when this node's identity is known, and read at startup
// by the guard that refuses to self-bootstrap over it.
type ColonySidecar struct {
	// ColonyID is the Colony this node's data belonged to.
	ColonyID string `json:"colony_id"`

	// NodeID is the node's own identity at the time the sidecar was
	// written, for the operator's benefit when reading a moved-aside
	// data directory.
	NodeID string `json:"node_id"`

	// WrittenAtUnix is when this sidecar was last written.
	WrittenAtUnix int64 `json:"written_at_unix"`
}

// sidecarPath returns the sidecar's path for a data directory.
func sidecarPath(dataDir string) string {
	return filepath.Join(dataDir, sidecarFileName)
}

// ReadColonySidecar reads the sidecar from a data directory.
//
// A MISSING sidecar is NOT an error. It is the normal case for a node
// that has never had a Colony identity - including every node built
// before ADR-0156 - and it means "this directory has no record of
// having belonged to a Colony", which is exactly what the guard wants to
// know.
//
// A CORRUPT sidecar IS an error, and deliberately so. A sidecar that
// cannot be parsed might be a sidecar belonging to some other Colony, or
// a truncated write, or a file that is not a sidecar at all. Ignoring
// it and self-bootstrapping anyway would discard the only evidence the
// guard has, in favour of a guess - and the whole point of the guard is
// that it refuses to guess.
func ReadColonySidecar(dataDir string) (*ColonySidecar, error) {
	data, err := os.ReadFile(sidecarPath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the Colony sidecar in %s: %w", dataDir, err)
	}

	var sidecar ColonySidecar
	if err := json.Unmarshal(data, &sidecar); err != nil {
		return nil, fmt.Errorf("the Colony sidecar in %s exists but could not be parsed: %w", dataDir, err)
	}
	return &sidecar, nil
}

// WriteColonySidecar records this node's Colony identity in its data
// directory.
//
// A failure here is reported rather than swallowed, but it is also not
// fatal to the node: the sidecar is a guard's HINT, not the authority
// on identity, and a node that cannot write a sidecar is still a
// perfectly good member of its Colony. The refusal to self-bootstrap
// is a safety net for the common case, and a node whose data directory
// cannot hold the net still has the replicated identity, which is the
// real answer.
func WriteColonySidecar(dataDir, colonyID, nodeID string) error {
	if colonyID == "" {
		return errors.New("refusing to write a Colony sidecar with an empty colony_id")
	}

	sidecar := ColonySidecar{
		ColonyID:      colonyID,
		NodeID:        nodeID,
		WrittenAtUnix: time.Now().Unix(),
	}
	data, err := json.MarshalIndent(sidecar, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling the Colony sidecar: %w", err)
	}
	data = append(data, '\n')

	path := sidecarPath(dataDir)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("creating %s for the Colony sidecar: %w", dataDir, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing the Colony sidecar to %s: %w", path, err)
	}
	return nil
}

// ErrColonyIdentityRequired is returned by the startup guard when a node
// refuses to self-bootstrap over a data directory that already belonged
// to a Colony.
var ErrColonyIdentityRequired = errors.New("raft: this data directory already belonged to a Colony")

// GuardSelfBootstrap refuses a self-bootstrap when the data directory
// holds a sidecar naming a Colony.
//
// WHY THE GUARD EXISTS: a node with an EMPTY data directory and no sidecar
// is a node forming a new Colony, and that is legitimate - it is how
// every Colony starts. A node with an EMPTY data directory and a
// sidecar is a node whose data was wiped or moved aside, and
// self-bootstrapping there would form a second Colony that nothing
// could detect. The sidecar is what tells those two cases apart.
//
// WHY IT REFUSES RATHER THAN ASKS: raftd is a daemon with no operator
// present at startup. A refusal that names the Colony it belonged to
// turns a silent, undetectable split into a startup failure that says
// exactly what happened. The operator's options are then explicit: join
// the Colony it belonged to, or declare that it is forming a NEW Colony
// (which is what a deliberate -reset is for, since -reset moves the
// whole directory aside, sidecar included).
func GuardSelfBootstrap(dataDir string) error {
	sidecar, err := ReadColonySidecar(dataDir)
	if err != nil {
		// A corrupt sidecar is reported rather than ignored: ignoring
		// it would discard the only evidence the guard has, in favour
		// of a guess.
		return fmt.Errorf("refusing to self-bootstrap: %w", err)
	}
	if sidecar == nil || sidecar.ColonyID == "" {
		// No sidecar, or a sidecar naming no Colony: this directory
		// has no record of having belonged to a Colony.
		return nil
	}

	return fmt.Errorf("%w: %s records that it belonged to Colony %s (node %s), so bootstrapping a new single-node cluster here would form a second, unrelated Colony that nothing in the system could detect. Join that Colony instead, or declare a new Colony deliberately by moving the whole data directory aside (raftd -reset), which carries the sidecar with it",
		ErrColonyIdentityRequired, dataDir, sidecar.ColonyID, sidecar.NodeID)
}
