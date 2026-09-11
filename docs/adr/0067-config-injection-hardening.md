# ADR-0067: Config injection hardening

## Status

Accepted

## Context

A 2026-09-06 security audit (requested by the user, run against an
isolated clean clone of `main` so it did not collide with concurrent
work) found that caller-supplied identifiers and node-config values
were validated at the FSM boundary only for non-emptiness and
uniqueness - never for the character set the several generated-config
renderers downstream actually needed safe. Four of the renderers
(`internal/dhcpd.RenderConfig`, `internal/hast.RenderConfig`,
`internal/pf.Manager.ApplyNAT`, `internal/cloudflare.RenderConfig`)
build their target daemon's config text with plain `fmt.Fprintf`/string
interpolation and no escaping of their own, on the implicit assumption
that a VM/jail/network ID or a node-config value is a short, plain
token.

The most severe consequence: a newline in a `VMDefinition.id` (used as
a dnsmasq lease hostname), `NetworkDefinition.bridge_name`/
`external_gateway`, or `nodeconfig.DNSServer` injected an arbitrary
additional dnsmasq directive into `dnsmasq.conf` - including
`dhcp-script=`, which dnsmasq executes **as root** on every lease
event. `CreateVM`/`CreateNetwork` require only Operator role. This was
proven directly against the real `internal/dhcpd.RenderConfig`, not
merely asserted.

A related, separate finding from the same audit: `/etc/rc.conf` is
world-readable on both live Hives and contains the Admin-role
`-peer-api-key` in plaintext, and managerd's gRPC port is LAN-reachable
- meaning any local unprivileged user on a Hive already had a path from
"can read a config file" to "root," via `CreateNetwork`. That key
rotation and `rc.conf` permissions are an operational follow-up, not a
code change, and are tracked separately from this ADR.

The same audit also found and fixed, in the web UI: an open redirect
in the post-login `?next=` flow (`/\evil.com` bypassed the existing
guard - confirmed live in a real browser, since browsers normalize a
leading backslash to a forward slash in special-scheme URLs) and a
permissive WebSocket `CheckOrigin` on the VM console endpoint (the
classic cross-site WebSocket hijacking setup, though not currently
exploitable given the session cookie's independent `SameSite=Lax`
setting) and a session cookie missing the `Secure` attribute even when
TLS is configured.

## Decision

**Validate at the boundary that already owns the value, not only in
the renderer that eventually consumes it** - each fix lands at the
`internal/raft.FSM` `applyCreate*`/`applySet*` function (or
`internal/nodeconfig.Manager.Save`, for the one value that is
deliberately never raft-replicated) that already validates other
fields of the same command (e.g. `applyCreateNetwork` already validated
`subnet` via `net.ParseCIDR`):

- `VMDefinition.id`/`JailDefinition.id`/`NetworkDefinition.id`: a new
  `validResourceID` (alphanumerics, `-`, `_`, max 64 chars) at each
  `applyCreate*`, mirroring `internal/jail.qualifiedName`'s existing
  allowlist - the one place in this codebase that already got this
  right.
- `NetworkDefinition.bridge_name`, `nodeconfig.Uplink`/`NATUplink`: a
  new `validInterfaceName` (alphanumerics and `-`, max 15 chars - the
  FreeBSD kernel's own null-padded-16-byte-buffer limit, the same one
  ADR-0022 discovered the hard way for Apiary-generated names).
- `NetworkDefinition.external_gateway`, `nodeconfig.DNSServer`:
  `net.ParseIP`.
- `SetVMCloudflareExposure.hostname`: a new `validHostname`
  (alphanumerics, `-`, `.`, max 255 chars) - interpolation safety, not
  real DNS validity; a hostname that fails real DNS resolution just
  fails later, harmlessly, when cloudflared can't route it.

**Also add defense in depth at each renderer itself** - `RenderConfig`
(dhcpd, hast, cloudflare) and `ApplyNAT` (pf) now independently reject
any interpolated value containing `\n`/`\r`, regardless of whether a
caller already validated anything upstream. A renderer has no way to
know whether the FSM validation above actually ran (a future command
type could add a new field without remembering to validate it there),
so each one checks for itself rather than trusting its caller
implicitly. `internal/cloudflare.RenderConfig`'s signature changed from
`string` to `(string, error)` to make this possible; its one caller
(`EnsureRunning`) already returned `error`.

**Web UI fixes**, all independent of the above:

- `isSafeRedirectPath` now parses with `net/url` and explicitly rejects
  a leading `/\`, instead of pattern-matching for `//`/`://` only.
- The session cookie gained a `Secure` flag driven by a new
  `Server.tlsEnabled` field, threaded through `NewServer`'s existing
  parameter-list-growth convention (`cmd/frontend` computes it from
  `-tls-cert`/`-tls-key` before constructing the server, so the
  cookie's Secure-ness is never briefly wrong).
- The console WebSocket's `CheckOrigin` now rejects a mismatched
  `Origin` header via a new `checkConsoleOrigin`, rather than
  accepting every origin and relying entirely on the session cookie's
  own `SameSite=Lax` (a defense living in a different file, which
  would silently stop applying if that cookie attribute ever changed).

## Scope and verification

No protobuf, RPC surface, or wire-format changes. Existing production
VM/jail/network IDs and node-config values on both live Hives were
checked against the new rules *before* writing them (plain alphanumeric
IDs, real interface names, real IPs throughout) - none would be
rejected by this change, so no existing raft log record risks silently
dropping out on the next full log replay.

Every finding except the `rc.conf`/peer-key follow-up has a permanent
regression test proving the exact exploit is now rejected:
`TestFSM_Apply_CreateVMInvalidIDRejected`,
`TestFSM_Apply_CreateJailInvalidIDRejected`,
`TestFSM_Apply_CreateNetworkInvalidBridgeNameOrGatewayRejected`,
`TestFSM_Apply_SetVMCloudflareExposure_InvalidHostnameRejected`,
`TestManager_SaveRejectsUnsafeValues` (nodeconfig),
`TestApplyNAT_RejectsNewlineInUplinkOrSubnet` (pf),
`TestRenderConfig_RejectsInvalid`'s two new cases (hast),
`TestRenderConfig_RejectsNewlineInjection` (dhcpd, using the exact four
vectors proven exploitable during the audit),
`TestRenderConfig_RejectsNewlineInHostname` (cloudflare),
`TestServer_Login_RejectsOpenRedirectNextURL`'s two new cases,
`TestServer_Login_SessionCookieSecureFlagMatchesTLSEnabled`, and
`TestServer_ConsoleWS_RejectsCrossOriginUpgrade` (which also proves the
same-origin case still works, via a real `httptest.Server` and a real
WebSocket dial). `go build`/`go vet`/`go test ./...` and the FreeBSD
cross-compile for `managerd`/`raftd`/`restshimd` all pass;
`cmd/frontend` requires a native FreeBSD build as usual (ADR-0030's cgo
constraint).

## Deferred

Rotating the disclosed `-peer-api-key` on both Hives is a live production
change, not a code fix, and is tracked as a separate operational
follow-up rather than bundled into this commit.

`apiaryinstall` (ADR-0082) briefly enforced `chmod 600 /etc/rc.conf` as a
follow-up to this finding, but that has been removed (see ADR-0082's
2026-09-11 correction): 644 is FreeBSD's own stock default, and changing
it was never actually a decision of this ADR, just unlabeled operational
language. The plaintext-secret concern itself is still real and still
unresolved - key rotation (above) is its actual fix.
