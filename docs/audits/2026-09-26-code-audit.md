# Code audit, 2026-09-26

Revision audited: `main` at `7198a5d` (branch `audit/code-audit`).
Method: static analysis (govulncheck, gosec, staticcheck, errcheck, gitleaks,
`go test -race`), plus manual reading of every analyzer hit that looked real,
of Codex's four scan findings, and of four recent changes from another agent.
Nothing was run against a live Colony. No exploit testing was done.

## Summary

| ID  | Severity | Area | Finding |
|-----|----------|------|---------|
| A1  | Medium | managerd | Operator API keys can purge any VM or jail definition (Codex #1, confirmed) |
| A2  | Medium | raft/join | Unauthenticated join requests grow Raft state without bound (Codex #2, confirmed) |
| A3  | Medium | login | Lockout trackers grow without bound and scan the whole map per failure (Codex #3, confirmed; worse than reported) |
| A4  | Info | dependency | grpc-go v1.84.0 GO-2026-6443 flagged by govulncheck; not exploitable here (needs xDS, which is not compiled in). Corrected after triage |
| A5  | Medium | auth | New `*Local` list RPCs have no auth role, and the change bypasses leader-only reads |
| A6  | Low | frontend | Frontend and restshimd use `http.ListenAndServe` with no timeouts |
| A7  | Low | join | Three unauthenticated join RPCs (`RequestJoinColony`, `GetJoinRequestStatus`, `CancelJoinRequest`) dial caller-chosen addresses when no allowlist is set (Codex #4, confirmed) |
| A8  | Low | auth | Four more read RPCs also lack a role entry; no test enforces role coverage |
| A9  | Low | bhyve | `kill <pid>` from a pidfile with no check that the pid is still ours |
| A10 | Low | raftd | `internal_token` is empty by default, so the internal RPC relies on filesystem permissions alone |
| A11 | Low | proto | `VMDefinition.hostname` is a dead field, and the UI and docs contradict each other about it |
| A12 | Low | tests/CI | `go test -race` fails in `internal/frontend`; CI does not run with `-race` |
| A13 | Info | docs | `add-node-to-colony.md` rewrite adds em dashes and omits the `raft_bind_host` field |
| A14 | Info | hardening | Several smaller hardening notes (see the end) |
| A15 | Medium | raft/join | FSM read the wall clock when applying join-request resolutions, so replays could diverge (found while fixing A2) |

Analyzer results: gitleaks found no real secrets in 463 commits (10 hits, all
false positives). govulncheck flagged 1 vulnerability, which triage showed is not exploitable here (A4). staticcheck
found only style issues and dead code. gosec reported 252 issues; the ones that
looked real are covered below, the rest are noise or accepted.

## Fix status

All fixes are on this branch, one commit per finding (or per closely related
group), each with tests. Nothing is merged or pushed.

| ID | Status | Commit | Notes |
|----|--------|--------|-------|
| A1 | Fixed | `cbc622d` | FSM refuses to purge a resource that is not DELETING. RPC roles left as they were: the peer credential the reconciler uses is Operator-tier and raising the role would break live teardown reporting. An Operator can still hasten teardown of a resource that is already deleting, which is no more than DeleteVM plus waiting. |
| A2 | Fixed | `eb4d921` | Field validation at RPC and FSM, 25 pending / 100 total caps, deterministic eviction of records expired for over an hour. |
| A3 | Fixed | `ad2ccbc` | Both trackers capped at 10000, sweep no longer per-request, username limit 256, login body limit 64 KB. |
| A4 | No change | (none) | Not exploitable in Apiary; see the corrected section. |
| A5 | Fixed, with one accepted tradeoff | `42e3c93` | Roles added and proto comments corrected. The frontend still reads a follower's local state, so a just-made change can briefly not appear; that is now documented and is a design decision for the owner, not changed here. |
| A6 | Fixed | `292a52d` | Header and idle timeouts. ReadTimeout and WriteTimeout deliberately not set. |
| A7 | Warning added; allowlist not set | `6b80eb9` | The startup warning landed. `known_peer_addresses` is not configured by this branch, so the exposure remains on every Comb until it is set. The warning prints only when managerd starts, so a managerd started before this change has not printed it, and setting the allowlist also needs a managerd restart. The behavior stays opt-in; requiring the allowlist would break single-node Colonies. |
| A8 | Fixed | `42e3c93` | Six roles assigned; `TestRequiredRole_CoversEveryRPC` enforces it. |
| A9 | Fixed | `63fb6d5` | Only signals a pid whose executable is `daemon`. |
| A10 | Warning added; token not generated | `6b80eb9` | The startup warning in raftd landed. Generating a token did not: it needs coordinated raftd.json and managerd.json changes and is left to the owner. `internal_token` is still empty wherever it was empty before. |
| A11 | Fixed | `e2ae418` | Field removed and reserved; the create page it contradicted was already replaced. |
| A12 | Fixed | `3c289ab`, `3ae9861` | Fake guarded by a mutex; CI now runs `go test -race ./...`. Full suite passed under -race before enabling. |
| A13 | Fixed | `d8eb396` | This doc only. About 850 added lines elsewhere still contain an em dash; a sweep across other authors' files was not attempted. |
| A14 | Partly fixed | `8012ba0`, `2a82600`, `778794c` | Interface-name validation in deadman, explicit TLS 1.2 minimum plus a guard test, broader rc.conf redaction. Not changed: no CSRF token (SameSite=Lax is the sole defense), and the unused `MigrationState.normalize` in the unwired migration package. |
| A15 | Fixed | `33b17d4` | Expiry check moved out of the FSM into the manager. |

## Findings

### A1. Operator keys can purge VM and jail definitions (Medium)

`internal/manager/auth.go:164-166` gives `ReportVMTeardownComplete` and
`ReportJailTeardownComplete` the Operator role. Both go straight to
`applyPurgeVM` / `applyPurgeJail` (`internal/raft/fsm.go`), which delete the
definition unconditionally: no existence check, no check that the resource is
marked deleting, no check that the caller is the owning node.
`ForcePurgeVM` and `ForcePurgeJail` are Admin-only and refuse unless the
resource is already in DELETING state, so this path bypasses both guards.

Impact: an Operator key can remove the definition of a live, running VM. The
guest keeps running but Apiary no longer knows about it, its IP allocation is
freed for reuse, and its disk is orphaned. Operators can already call
`DeleteVM`, so this is not new destructive power. What is bypassed is the
deliberate Admin gate and the lifecycle check.

Fix: make the FSM refuse to purge unless `desired_state == DELETING`, and
restrict these two RPCs to the internal or peer credential instead of a
role-based API key. Add a test that an Operator key is refused.

### A2. Unbounded join-request records (Medium)

`RequestJoinColony` is deliberately unauthenticated. Each call writes a Raft
record (`internal/manager/joincolony.go`, `applyCreatePendingJoinRequest` in
`internal/raft/fsm.go:611-619`). Nothing caps the number of pending requests,
limits the length of `node_id` or `raft_bind_address`, or rate-limits callers.
The 15 minute TTL only hides a request from the list; the only removal is a
manual admin purge. Every record is replicated to all voters and kept in every
snapshot.

Impact: any client that can reach a managerd port can grow the Raft log,
state, and snapshots on every Comb, and can fill disks.

Fix: validate field formats and lengths at the RPC and in the FSM, cap pending
requests (deterministically, in the FSM), and have the leader submit a purge
command for expired and resolved records.

### A3. Lockout trackers are unbounded (Medium)

`internal/manager/lockout.go:78-100` and its mirror
`internal/frontend/lockout.go` add a map entry per distinct username on every
failed login, and each failure then scans the entire map while holding the
mutex. `AuthenticatePassword` is unauthenticated by design. Usernames have no
length limit. On the frontend, `ParseForm` has no `MaxBytesReader` (Go's default
cap is 10 MB per form), so an unauthenticated web client can submit very large
usernames.

Impact: memory growth, CPU amplification under the lock, and slower logins for
real users. Keying lockout on username only also lets anyone lock out a known
account, which is a design tradeoff worth stating.

Fix: cap the map size and username length, evict the oldest entries, and add
`http.MaxBytesReader` to the login handler.

### A4. grpc-go GO-2026-6443 (Info, corrected)

govulncheck reports `google.golang.org/grpc@v1.84.0` as affected through
`cmd/managerd/main.go:516` (`grpc.Server.Serve` -> `http2Server.HandleStreams`).
The first version of this report rated that Medium and described a pre-auth
crash. That was wrong. Reading the advisory: the panic is in the xDS server
routing interceptor (`RouteAndProcess`), which indexes an empty authority when a
request lacks both `:authority` and Host headers. The transport symbol
govulncheck matched only accepts such a request. Apiary uses no xDS code, and
`go list -deps ./cmd/managerd` shows no xDS package is compiled in, so the
vulnerable interceptor is not present and the crash cannot occur.

No code change. Downgrading to v1.83.2 (a patched release) would silence the
scanner, but for a finding that cannot be triggered it is not worth moving off
the current release line; Dependabot would only re-propose 1.84.0. Re-run
govulncheck when a v1.84.x or v1.85 release containing the fix ships, and treat
its output for this ID as a known false positive until then.

### A5. `ListVMsLocal` and `ListJailsLocal` (Medium)

Commit `4e90280` added two RPCs and switched the frontend list pages to them.

- No entry in the `requiredRole` table, so both default to Admin, while
  `ListVMs` and `ListJails` are Viewer. Direct API users with a Viewer key get
  permission denied. The web UI only works because its own key is effectively
  Admin, which means managerd's Viewer and Operator tiers are not what protects
  UI users.
- They read the local FSM of whichever Comb the frontend is talking to, not the
  leader. That bypasses the project's leader-only-read design. A follower can
  lag, so a VM just created may be missing from the list after the redirect
  (read-your-writes). I reasoned this from the code and did not reproduce it.
- The premise does not hold: ADR-0106 already made every VM and jail visible
  from every Comb, and `ListVMs` already forwards to the leader.
- No ADR, and the only tests are stubs on fake clients.

Fix: add both to `requiredRole` as Viewer (or revert the frontend to `ListVMs`),
and document the consistency tradeoff if the local read is kept.

### A6. No HTTP server timeouts (Low)

`cmd/frontend/main.go:236,238` and `cmd/restshimd/main.go:129,131` call
`http.ListenAndServe` and `ListenAndServeTLS`, so there is no
`ReadHeaderTimeout` or `IdleTimeout`. Slow connections can be held open
indefinitely on the unauthenticated login page.

Fix: use an `http.Server` with `ReadHeaderTimeout` and `IdleTimeout`. Do not
set `WriteTimeout`, because the console and log streaming endpoints are
long-lived.

### A7. Join forwarding dials caller-chosen targets (Low)

With no `known_peer_addresses` configured, three unauthenticated RPCs dial any
caller-supplied `target_address` and return the error text:
`RequestJoinColony`, `GetJoinRequestStatus` and `CancelJoinRequest`
(`internal/manager/joincolony.go`, each guarded by `checkTargetAddressAllowed`,
which allows everything when the allowlist is unset). This is a limited
reachability oracle. It is timeout-bounded and does not send the peer API key.
The code documents it as an accepted residual risk (ADR-0096/0097).

Fix, in two separable parts:
- Warn at startup when the allowlist is empty: done (`6b80eb9`).
- Configure `known_peer_addresses` on every Comb: not done. Entries are matched
  as exact `host:port` strings, so they must be spelled the way callers write
  `target_address`, and the setting is read at managerd startup.

Until the second part is done, the exposure is unchanged.

### A8. More RPCs without a role, and no coverage test (Low)

A scripted comparison of the 92 RPCs in `manager.proto` against `requiredRole`
found six with no entry (default Admin): `ClusterHealth`, `HostPackages`,
`ListJailsLocal`, `ListNodeServices`, `ListVMsLocal`, `PushISOTo`. Failing
closed is the right default. But five of these (all but `PushISOTo`) are plainly
read RPCs, and nothing forces a new RPC to get a deliberate role.

Fix: add a test that every RPC in the service descriptor has an explicit entry,
and assign roles deliberately.

### A9. `kill` from a pidfile without identity check (Low)

`internal/bhyve/manager.go:668,691` read a pid from
`/var/run/apiary/bhyve/*.pid` and run `kill <pid>` as root with no check that
the pid still belongs to bhyve or the serial reader. If bhyve exited on its own
and the pid was reused before teardown, an unrelated process gets SIGTERM.
`/var/run` is cleared at boot, so the window is one uptime.

Fix: verify the process name and start time, or use a supervised handle.

### A10. Empty `internal_token` by default (Low)

`internal/raft/auth.go` `checkToken` returns success when the configured token
is empty, and neither the installer nor the Makefile generates one, and
nothing warns. raftd's socket is mode 0660 (`cmd/raftd/main.go:32`). The
directory is root-only per the project notes, so this is defense in depth.

Fix, in two separable parts:
- Warn at startup when the token is empty: done (`6b80eb9`, in raftd).
- Generate a token at setup: not done.

Until the second part is done, the internal RPC is protected by filesystem
permissions alone.

### A11. Dead `VMDefinition.hostname` field (Low)

Commit `74cb3ff` added field 20 in both protos and `name="hostname"` on the
create form, but `handleCreateVM` never copies it into the `VMDefinition`, and
nothing reads or displays it. The help text says it is "stored"; the newer
guided create page (`d060ac9`) says a VM has no persisted hostname. My earlier
test `TestServer_NewVMPage_HasHostnameDrivenIdentity` passes only because it
checks the attribute order `id=` then `name=`, and its comment is now false.

Fix: revert it and reserve field 20, or wire it up and validate it. Then make
both create pages agree.

### A12. Race detector failure (Low)

`go test -race` fails `TestHandleInvariantsPage_LeaderVsNonLeaderQuorumEvaluationDiffers`
in `internal/frontend`. The race is in the test fake
(`fakePeerHostStatsClient.HostStats`, `cluster_overview_test.go:60`), which
mutates a field without a lock while production code legitimately fans out
goroutines (`invariants.go:103-116`). It is not a production bug. `internal/manager`,
`raft`, `cluster`, `deadman`, `pf`, `migration` and `backup` pass under `-race`.

Fix: guard the fake with a mutex and run `go test -race` in CI.

### A13. Join doc rewrite (Info)

Commit `95bc87f` restructures `docs/add-node-to-colony.md` reasonably, but adds
2 em dashes (against the project rule), drops the trailing newline, and its
guided-action steps omit the `raft_bind_host` field the form requires (the form
takes `target_managerd_host`, `raft_bind_host`, and `confirm_phrase`). The commit
is titled "Multiple improvements:" and bundles unrelated work
(`internal/freebsdimg`, ADR-0127), which makes review and revert harder.
About 850 added lines across 58 recent commits contain an em dash.

### A15. FSM read the wall clock on join-request resolution (Medium)

Found while fixing A2. `applyResolvePendingJoinRequest` called
`time.Now()` to decide whether a request had expired. The FSM runs on every
replica and again on every log replay (restart, snapshot catch-up), so
replaying an approve, reject, or cancel after the request's 15 minute TTL
returned an "expired" error and left the request Pending, while nodes that
applied it live had it Approved. Replicas could disagree about the status.
Expiry is now enforced in `internal/manager` before submitting, and the FSM
no longer reads the clock (it was the only such read in the FSM apply path).

### A14. Hardening notes (Info)

- `deadman.ArmTapRevert` builds a root shell script with the bridge name
  unquoted. It is safe today because `nodeconfig` validates `bhyve_bridge`, but
  the package should validate or quote it itself.
- Five client `tls.Config` literals set no `MinVersion`. With `go 1.27` the
  default is TLS 1.2, so this is not exploitable; set it explicitly.
- `hostconfig.RedactRcConf` only redacts the legacy `-peer-api-key` pattern.
- Login is protected only by `SameSite=Lax` cookies; there is no CSRF token.
  No state-changing route is registered on GET.
- `MigrationState.normalize` is unused (staticcheck U1000).

## Reviewed and found sound

- XSS: only three `template.JS` / `template.HTML` conversions exist. The two
  `template.JS` values are `json.Marshal` output (HTML-escaped) placed in a
  script; `pageHeader` extra is raw HTML but its one data-driven caller escapes.
- Session tokens: 32 bytes from `crypto/rand`, new token per login, no
  `math/rand` in the codebase. Login cookie is `HttpOnly`, `SameSite=Lax`,
  `Secure` when TLS is on.
- Secret comparison uses `subtle.ConstantTimeCompare`.
- ISO and resource-name paths are validated (`validateName`, `validResourceID`)
  before reaching the filesystem. The `os.Open` sites gosec flagged are fed by
  validated names or operator config.
- `InsecureSkipVerify` in `managerlink/scheme.go:155` is used only to detect
  whether a peer speaks TLS, sends no data, and is documented as never feeding
  trust decisions.
- `service` and `sysrc` calls take names from an allowlist table.
- gosec's path-traversal hit (`hostconfig.go:166`) is a root-operator CLI mode,
  not network-reachable. Its 20 "hardcoded credential" hits were false positives.
- errcheck's 230 non-test hits are best-effort cleanup (closing connections,
  removing temp files, destroying a tap after a failure).

## Not covered

- No live testing: listeners, firewall, PAM, deployed configuration, exploits.
- About 50 of the 58 commits since `4ed8a08` were not read, including
  ADRs 0121 to 0136 and their designs.
- `internal/backup`, `internal/jailnet` and `internal/migration` have no
  non-test importers yet (work in progress), so they were not reviewed. `backup`
  and `migration` pass under `-race`; `jailnet` was not run under it.
- Codex's findings were re-verified against `7198a5d`; its report was pinned to
  `5dad79f`.
