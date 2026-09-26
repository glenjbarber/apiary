// Package jailnet is the VNET jail networking reconciler: the piece
// ADR-0117's Stage 1 created but did not yet keep alive.
//
// Stage 1 gave a VNET jail a dedicated epair(4) pair and an address at
// creation time and recorded the pair's two names in a node-local JSON
// file so a later tick could find them again. That is a one-shot
// provisioning step, not reconciliation, and the gap matters more than
// any other gap in the design:
//
//   - An epair(4) pair is not persistent. It is not in rc.conf, and
//     nothing on FreeBSD recreates it at boot. So after any reboot, or
//     any `ifconfig -a` teardown, or any failure to load if_epair, the
//     recorded names refer to interfaces that no longer exist, and the
//     next tick hands jail(8) an interface it cannot find.
//   - A bridge(4) that was destroyed and recreated (a network deleted
//     and re-created under the same name, an operator mistake, a
//     failed network artifact reconcile) comes back with no members, so
//     a jail that is still running has an epair that belongs to nothing.
//   - An address or a default route inside a running jail is not
//     durable either: it lives in the jail's own routing table, and a
//     jail restarted by anything other than this reconciler - an
//     operator, a package, an at(1) job - comes back with whatever
//     rc.conf inside it happens to say.
//
// So this package exists to make all of that converge: every pass it
// observes what is really there, compares it against what the
// intended state is, and repairs only the difference. A pass that finds
// nothing to do issues no commands at all, which is what makes calling
// it on every reconcile tick affordable rather than a source of
// constant "File exists" failures.
//
// # Unknown is never false
//
// The rule this package is built around, and the one it is most easily
// tempted to break: a question that could not be answered is VerdictUnknown,
// never VerdictInSync and never a definite "the interface is gone".
// Every observation here returns either a definite answer or an error,
// and every Verdict distinguishes the two. The two places this is
// hardest to keep straight are spelled out at their definitions:
// VerdictUnknown and FindingRestartRequired.
//
// # Placement
//
// This package deliberately does not live in internal/cluster, next to
// ensureJail, because everything in it is pure orchestration around two
// narrow interfaces (a bridge driver and a jail driver) and an
// injectable command runner, and none of that is cluster-specific. That
// makes all of it testable on a development host with no FreeBSD, no
// root, and no jail: the same reasoning internal/deadman's atRunner and
// internal/cluster's own per-dependency manager interfaces already use.
package jailnet
