# ADR-0113: TLS trust prompt on Colony join

## Status

Accepted

## Context

SHARED.md has carried an unscoped TODO for a while: "each Comb should
prompt to trust a new Comb's TLS cert when joining a Colony." It was
never designed - this ADR is that design, plus the implementation.

Before designing anything, the actual join-a-Colony flow
(`RequestJoinColony`/`ApproveJoinRequest`, ADR-0083, ADR-0092,
ADR-0097, ADR-0103, ADR-0106 - `internal/manager/joincolony.go`) was
read directly, along with the frontend side
(`internal/frontend/joincolony.go`, `web/templates/cluster_overview.html`,
`web/templates/machine.html`) and this project's separate, already-
existing peer-TLS trust mechanism (ADR-0093, `peer_tls_ca` on
`managerd.json`/`frontend.json`).

**What the join flow actually exchanges today (confirmed by reading the
code, not assumed):** a joining Comb calls `RequestJoinColony` with only
`node_id` and `raft_bind_address`. `ApproveJoinRequest` reads that
pending record and, after a leadership check and a plain-TCP
reachability dial (ADR-0097), calls `AddVoter` against local raftd.
Nothing in this exchange carries any TLS certificate material, a
fingerprint, or any other cryptographic identity for the joining Comb.
The Admin approving a request sees only `node_id`, `raft_bind_address`,
and a 6-digit visual-comparison code (`code`, explicitly documented as
"not a secret") - there was no code path at all for either side to
learn or display the other's TLS identity as part of this flow.

**What already exists, and is not being replaced:** `peer_tls_ca`
(ADR-0093) lets one managerd trust another's self-signed certificate for
managerd-to-managerd RPC forwarding, but it is configured entirely
out of band from the join flow - an operator manually copies a
`cert.pem` to the other host and sets `peer_tls_ca`/`peer_tls` on
`managerd.json`/`frontend.json` themselves (see SHARED.md's own several
first-hand incident write-ups of doing exactly this by hand across
`brood`/`drone`/`apiverse`/`apiarium`). `ApproveJoinRequest` never reads
or references this configuration, and completing a join never sets it
up automatically. In other words: the *raft membership* decision (who
gets to vote) and the *TLS trust* decision (whose certificate this
Comb will accept for peer RPC) are two entirely separate, previously
unconnected pieces of state, configured by two entirely separate
mechanisms, with no cross-reference between them at all.

**The actual gap this confirms:** an Admin approving a join request had
no way to see the joining Comb's TLS identity as part of approving that
join - not because it was silently trusted, but because it was never
surfaced in this flow at all. `ApproveJoinRequest` also required no
confirmation beyond a single button click, unlike this project's other
guarded, hard-to-reverse actions (`ConvertStandaloneToJoiner`'s
`confirm_phrase`, raftd's `-reset`, `apiaryinstall -apply-network`).
Approving a join is exactly this kind of action: it is the moment this
Comb starts trusting a new Comb's identity as a fellow Colony member
going forward, and (per ADR-0097's own documented incident history) is
expensive to reverse.

## Decision

1. **`RequestJoinColony`/`PendingJoinRequest` gain a `tls_cert_fingerprint`
   field** (`api/rpc/manager.proto`, mirrored in
   `api/internalpb/state.proto`). When a joining Comb's frontend submits
   the join form (`target_address` set), `RequestJoinColony` computes
   this automatically from the joining Comb's own locally-configured
   `tls_cert` (`nodeConfig.Load().TLSCert`) - a SHA-256 digest of the
   DER-encoded certificate, formatted `SHA256:AA:BB:...` (uppercase
   colon-separated hex, the same shape common TLS tooling already uses).
   A direct RPC caller may also set the field explicitly. Reading/parsing
   the local certificate is best-effort: any failure (no `tls_cert`
   configured, unreadable file, unparsable PEM) leaves the field empty
   rather than blocking the join - TLS remains opt-in throughout this
   codebase (ADR-0087/ADR-0093), and a Comb with TLS not yet configured
   is a legitimate, already-common deployment state, not an error.

2. **The landing page's "Pending join requests" panel renders this
   fingerprint** (`web/templates/cluster_overview.html`) - a real value
   in `<code>`, or an explicit "no TLS certificate presented" fallback
   when empty. This value is not a secret and needs no additional
   RPC/auth tier; it rides along on the same `ListJoinRequests` response
   the panel already fetches.

3. **`ApproveJoinRequest` gains a mandatory `confirm_phrase` field**
   (`"yes-trust-new-comb"`), checked as the very first statement in the
   handler - before the leadership check, before reading the pending
   request, before anything else - matching this codebase's own
   established exact-match confirmation-phrase convention
   (`ConvertStandaloneToJoiner`'s `confirm_phrase`, raftd's `-reset`,
   `apiaryinstall -apply-network`). A wrong or missing phrase returns an
   error with no action taken at all - no leader forward, no raft read,
   no `AddVoter` call. The landing page's Approve form always renders
   the confirm-phrase input directly beside the fingerprint (or its
   absence), so an Admin cannot reach that field without the identity
   information already being on the page in front of them.

   The phrase is required unconditionally, regardless of whether the
   pending request carries a fingerprint - approving a join always
   trusts a new Comb's identity going forward, whether or not that
   identity currently includes a TLS certificate, and the UI always
   shows whichever is true right next to the field.

4. **A landing-page error banner** (`JoinRequestError`, reading the
   existing but previously-unrendered `?join_request_error=` redirect
   parameter) now actually displays a confirm-phrase mismatch (or any
   other Approve/Reject/Purge failure) on the page, so a wrong phrase
   visibly re-renders the form with an error rather than silently doing
   nothing.

## Explicitly out of scope

- **Not renegotiating or replacing the existing internal-token/TLS
  bootstrap mechanism.** `peer_tls_ca` (ADR-0093) configuration is
  untouched; approving a join does not automatically wire up
  `peer_tls_ca` on either side. The fingerprint shown here is for the
  Admin's own manual visual comparison (e.g. against a value the
  joining Comb's own operator reads off its Machine page or its local
  `openssl x509 -fingerprint` output) - it is not yet cross-checked
  against anything programmatically, and nothing yet validates a
  future `peer_tls_ca` setup against the fingerprint recorded here.
  Wiring the two together (so approving a join could, say, offer to
  write `peer_tls_ca` automatically) is a natural follow-up but a
  materially larger change with its own design questions, and was not
  requested.
- **Not a general certificate-management UI.** There is no cert list,
  no revocation, no rotation handling - just a fingerprint on one
  existing panel and one guarded-action confirmation phrase.
- **Not changed for the first node in a brand new Colony.** A
  standalone Comb's own initial bootstrap never calls
  `RequestJoinColony`/`ApproveJoinRequest` at all; this only affects
  joining a Colony that already has at least one member.

## Consequences

- An Admin approving a join now sees the joining Comb's TLS identity
  (or its explicit absence) and must type an exact confirmation phrase
  before the join actually takes effect - the same deliberate friction
  this project already applies to its other hard-to-reverse guarded
  actions.
- `ApproveJoinRequest` is now a breaking change for any existing direct
  RPC caller that was not already supplying `confirm_phrase` - every
  in-repo call site (tests, the frontend handler) was updated
  accordingly. Any external caller/script would need `confirm_phrase:
  "yes-trust-new-comb"` added to keep working.
- The fingerprint is informational, not authenticating: it is
  self-reported by the joining Comb over an already-unauthenticated RPC
  path (`RequestJoinColony`'s forwarding, ADR-0096), the same trust
  model ADR-0083 already discloses for `code`. It raises the bar from
  "no identity information at all" to "a value the Admin can manually
  cross-check," not to cryptographic proof.
- `docs/adr/0112-common-config.md` and `docs/adr/0111-frontend-restart-routing.md`
  were both merged to `main` concurrently with this work (after this
  worktree's own branch point), which is why this feature is numbered
  0113 rather than 0112 as originally planned - confirmed by checking
  `docs/adr/` on `main` directly rather than assuming the number was
  still free.
