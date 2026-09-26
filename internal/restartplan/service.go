package restartplan

// DefaultService is the rc.d service ADR-0125's workflow restarts, and
// the raft-replicated restart-lease key it reserves under. ADR-0125 §1
// is explicit that this is NOT a new string: "apiary_raftd" is already
// the rc.d service name, the internal/manager.services.go entry, and the
// key operators expect, so introducing a parallel constant would only
// create a second name for one thing.
//
// It is exported (and defaulted-to) from this package so that both sides
// of the workflow - the leader-side planner and cmd/raftd's own startup
// confirmation hook - name the same key without each keeping a private
// copy. The same value appears as internal/manager's own unexported
// restartGuardrailService for "apiary_managerd" and as
// cmd/managerd's duplicate of that constant; this one is the raftd
// counterpart, and TestDefaultServiceMatchesManagerServices asserts it
// against the real internal/manager entry rather than trusting this
// comment to stay true.
const DefaultService = "apiary_raftd"

// ManagerService is the sibling key ADR-0103's own managerd workflow
// already uses. It is named here only so a caller driving this package
// for managerd (whose lease/cooldown findings arrive as fact.Existing)
// can say so explicitly; nothing in this package defaults to it, and
// nothing in this package's ADR-0125 path uses it.
const ManagerService = "apiary_managerd"
