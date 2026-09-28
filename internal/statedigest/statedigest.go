// Package statedigest is ADR-0143's per-voter FSM state digest and the
// cross-voter verdicts computed from a set of them, in one place.
//
// It exists as a package because ADR-0143 has two halves that were
// written a long way apart and belong together:
//
//   - digest.go computes ONE voter's digest. raftd runs that, because
//     only raftd can see its own state machine. A digest computed
//     anywhere else is not a digest of anything.
//
//   - verdict.go decides what a SET of digests means. Nothing inside
//     raftd and nothing inside managerd can answer that, because a
//     single state machine cannot know whether the colony agrees with
//     it. A controller taking a step on one Comb at a time has to
//     answer it, so the verdicts cannot live inside a presentation
//     layer, which is where the first copy of them ended up.
//
// The split matters because the two halves fail in opposite ways.
// raftd can be wrong by computing a digest that does not describe its
// own state. A consumer can be wrong by reading one voter's digest as
// colony-wide agreement. Keeping the producer and the reader in one
// package means the reader's four verdicts are stated next to the
// thing they are verdicts about, and a change to one that quietly
// weakens the other is a change to one file rather than to two
// repositories' worth of prose.
//
// The three failure modes verdict.go refuses to commit, unchanged from
// ADR-0143 and restated here because they are the whole contract:
//
//   - A single observation is not agreement. One node always agrees
//     with itself, so "every observed digest matched" is true of a
//     one-node sample. Reporting that as agreement renders the absence
//     of evidence as health.
//   - Differing applied indexes may be a moving sample, not divergence.
//     That is Unsettled, and the correct response is to wait and read
//     again rather than to stop or to permit.
//   - Missing observations are not healthy. A Comb whose digest could
//     not be read is Unobserved, it is excluded from the comparison, and
//     it is never permission to continue.
//
// A fifth rule is about what a verdict may NOT say: that these state
// machines differ is something the evidence supports, and which of them
// is wrong is not. No Detail string in this package names a culprit,
// and a test asserts it stays that way.
package statedigest
