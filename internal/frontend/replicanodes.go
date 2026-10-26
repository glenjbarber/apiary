package frontend

// The replica-node list, with the owner excluded SERVER-SIDE.
//
// WHY THIS IS ITS OWN FILE. The wizard's Placement pane offers a
// replica Comb, and the replica cannot be the owner - HAST replicates
// a disk between two DIFFERENT Combs. The server already refuses
// that pairing on submit (`validateVMCreateForm` and
// `validateJailCreateForm` both name it).
//
// But refusing on submit is not the same as not offering it. An
// operator who picks the owner Comb and then finds that Comb also in
// the replica list has been offered something the server will
// reject, and the only thing that tells them so is a refusal AFTER
// they have filled in everything else.
//
// The spec's own account of how this class of bug happens is the
// reason it is fixed here rather than in the template's JavaScript:
// "client JS mirroring server rules is exactly how the placement-pane
// gap happened - a rule existed in one place (`placementUnavailable`)
// and the template's own JS never learned about it."
//
// The rule therefore lives in ONE place, and the template asks it.
//
// The exclusion is applied when the list is rendered. The owner is
// known at that point because the wizard's Placement pane renders the
// owner first, and because a re-render of the Placement pane carries
// the owner's current answer.

import "strings"

// replicaNodeOptions is the list of Combs that may be a replica
// target, given the owner.
//
// The owner is excluded because replicating a disk onto the Comb that
// already holds it is not a thing HAST can do, and because offering it
// is offering an invalid pairing.
//
// An EMPTY owner excludes nothing: until the operator has chosen an
// owner, every Comb is a possible replica, and removing Combs from the
// list on the strength of an answer that does not exist yet would
// make the list look arbitrary.
func replicaNodeOptions(nodes []string, owner string) []string {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return append([]string(nil), nodes...)
	}
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node == owner {
			continue
		}
		out = append(out, node)
	}
	return out
}

// replicaOptionsView is the template-facing shape of the replica
// picker.
//
// It is a view rather than a bare slice because the template needs to
// know whether there was anything to offer at all - and because the
// exclusion is the whole point, the view carries the ALREADY-FILTERED
// list rather than the raw one. A template that filtered again would
// be a second implementation of the rule, which is what this work
// exists to remove.
type replicaOptionsView struct {
	// Nodes are the Combs that may be a replica target.
	Nodes []string
}
