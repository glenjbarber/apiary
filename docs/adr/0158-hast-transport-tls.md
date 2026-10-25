# ADR-0158: TLS-encrypting the HAST replication transport

## Status

Proposed. This design has not been approved by the owner and is not implemented.

Supersedes: None
Superseded by: None
Tracking issue: #97
Related: ADR-0078 (whose open question this closes), ADR-0130 (HAST versus ZFS replication transport)

## Context

Glen decided: HAST needs to be TLS-encrypted. This closes the open
question ADR-0078 and issue #97 both left to the owner. ADR-0078
recorded three options - disable HAST replication until it can be
secured, place it on a separately secured network, or adopt a protocol
with authenticated encryption - and recommended not claiming
cluster-wide encryption while HAST remained unprotected. The third
option is the one Glen chose.

### hastd has no encryption of its own

This was verified directly against the upstream FreeBSD `hastd` source
(`sbin/hastd/` in the FreeBSD base system), not from memory and not
from a document that might have drifted. A case-insensitive search for
`tls`, `ssl`, `encrypt` and `crypt` across `hastd.c`, `primary.c`,
`secondary.c`, `proto.c`, `proto_tcp.c`, `proto_uds.c`,
`proto_common.c`, `proto_socketpair.c` and `hast.conf.5` returns **zero**
occurrences. `hast.conf` has no stanza to enable it either: the grammar
is `listen`, `replication`, `checksum`, `compression`, `timeout`,
`exec`, `metaflush` and `pidfile`, plus per-resource and per-node
`name`, `local`, `remote` and `source`.

So the encryption must be a transport tunnel carrying hastd's own
protocol. There is no configuration-only answer.

### What the tunnel has to satisfy

Three properties of hastd's transport shape the design, and each was
read from the upstream source rather than assumed.

**1. The primary dials the secondary; a role flip inverts the
direction.** `primary_connect` (`sbin/hastd/primary.c`) calls
`proto_connect_wait` against the configured `remote` address. The
secondary does not dial back: `init_remote`
(`sbin/hastd/secondary.c`) operates on an already-established
`hr_remotein`. A tunnel must therefore work in whichever direction the
current role requires, without the operator changing anything by hand
when roles flip.

**2. The secondary authenticates the peer by source IP.** On accepting
a connection, `hastd.c` iterates the configured resources and calls
`proto_address_match(conn, res->hr_remoteaddr)`; if no resource matches,
it logs "Client %s isn't known." and closes the connection. For TCP,
`tcp_address_match` (`sbin/hastd/proto_tcp.c`) resolves the configured
address and compares **only the IP address** against the accepted
connection's peer address - the port is not compared. A tunnel that
terminates the TLS session on the secondary and opens a new local
connection to hastd would present the tunnel's own local address, not
the primary's, and every connection would be refused as "not known".

**3. hastd's own port is not configurable per-resource in a way that
helps.** `hastd` listens on `tcp4://0.0.0.0:8457` /
`tcp6://[::]:8457` by default (`HASTD_LISTEN_TCP4` / `HASTD_LISTEN_TCP6`
in `hast.h`), and the same address is used for the resource
connection.

Property 2 is the binding constraint. It rules out the obvious design
of a TLS-terminating proxy in front of hastd, and it is the reason the
choice of mechanism below is not simply "use stunnel".

## Decision

### A WireGuard peer link, with hastd's `remote` pointing at the tunnel

A WireGuard link between each pair of Combs that share a HAST
resource, carrying hastd's TCP protocol inside the encrypted tunnel,
with hastd's `remote` address rewritten to the tunnel endpoint.

The properties that make this the right mechanism, each of them a
consequence of the upstream behaviour above:

- **The peer address is preserved.** The tunnel is a layer-3 network.
  The primary still connects to the secondary's *tunnel* address, but
  because the tunnel interface carries the real address space, the
  source address the secondary sees is still the primary's. The
  secondary's `proto_address_match` check therefore still passes, and
  it still authenticates the *primary host*, not a local proxy.
  A TLS-terminating proxy cannot do this: it is a userspace process on
  the secondary, so the connection hastd accepts comes from the
  proxy's local address.
- **Direction is symmetric.** WireGuard peers are bidirectional and
  each side has a fixed tunnel address, so a role flip changes which
  side dials, but the addresses in `remote` do not change. The
  configuration is the same on both nodes in either role.
- **It is not a userspace proxy.** No extra daemon sits in the data
  path on the secondary's side in a way that would break the source
  address check, and there is no plaintext TCP listener for hastd's
  protocol exposed to the storage network at all.

### Alternatives considered and rejected

**stunnel.** A TLS-terminating TCP proxy in front of hastd. Rejected
because it breaks property 2: the secondary would accept the
connection from stunnel's local address, and `proto_address_match`
would refuse every connection as "Client ... isn't known." The
address check is in `hastd` itself and cannot be configured away.

**An in-repo Go TLS proxy.** Rejected for the same reason as stunnel,
and it additionally duplicates what WireGuard already does at the
kernel level. It would be the fallback only if WireGuard were
unavailable on the target hosts.

**Switching HAST replication to ZFS replication over managerd's
existing mTLS channel.** This is ADR-0130's alternative column. It is
a genuine design and it removes HAST entirely, but it is a much larger
change than a transport fix: it replaces hastd's role management,
split-brain detection and dirty-extent accounting with a raft-granted
lease and a monotonic generation, per ADR-0130's own description. It
is out of scope for closing the encryption gap, and it is recorded
here as the option to revisit if the tunnel approach proves
unworkable in practice.

### What is configured, and where

The reconciler already renders `hast.conf` from raft membership
(`internal/cluster/hast.go`'s `reconcileHASTRoles`, and
`internal/hast`'s `RenderConfig`). The tunnel changes only what goes
into `Node.Remote`:

- With encryption enabled, `remote` is the peer's **tunnel** address
  (`host:port` on the WireGuard interface) rather than the peer's LAN
  address.
- The same rendered config is deployed to both nodes, and the config
  is symmetric between them, so a role flip needs no reconfiguration.

`hast.conf`'s `listen` address is left alone: hastd continues to
listen on its own address, and the tunnel is what carries the traffic
to it. A HAST resource's `remote` is per-resource, so two Combs that
share a resource need a tunnel endpoint for each other.

### The encryption is opt-in per Colony, and defaults off until the tunnel is provisioned

Enabling the tunnel requires a WireGuard interface, a peer key per
Comb and a shared tunnel address plan. Those are host prerequisites
that cannot be provisioned by the reconciler alone, and a partially
provisioned tunnel would break replication silently - the kind of
failure ADR-0097's reachability check exists to prevent.

So the encrypted transport is behind an explicit setting, and the
installer refuses to enable it without the prerequisites in place.
This mirrors how `hastd_enable` itself is an opt-in host prerequisite
(`internal/install/checks.go`'s `hastd-enable` check).

### Failure behaviour

A tunnel that is down must degrade to "replication is not working",
never to "replication is working but in plaintext". The reconciler
never falls back to the LAN address: `remote` names exactly one
address, and if that address is unreachable the resource stays
degraded and `hastctl` reports it. There is no plaintext fallback
path at all, which is the whole point of the change.

### Migration

Enabling the tunnel is a coordinated change across every member of the
Colony, exactly like enabling Raft TLS (ADR-0078's own migration
note): a member with a tunneled `remote` and a member without cannot
replicate with each other, because each side dials a different address.

The installer therefore provisions the tunnel for all members before
any member's `hast.conf` is rewritten, and the reconciler refuses to
switch a resource's `remote` to the tunnel address until the peer
answers on the tunnel address. That last check is the pre-approval
reachability pattern ADR-0097 established, applied to the tunnel.

## Validation

Tests must pass without root, without a real `hastd`, and without
FreeBSD kernel features - this project has no CI host with WireGuard
or `hastd` available.

- The tunnel address selection is pure and testable: given raft
  membership and a tunnel address plan, the reconciler must render
  `remote` as the peer's tunnel address, not its LAN address.
- With encryption disabled, the rendered config is byte-for-byte the
  config rendered today. This is the regression guard: the existing
  `internal/hast` config tests must not change.
- With encryption enabled but the peer not yet provisioned, the
  reconciler must refuse rather than write a config that points at an
  address nothing is listening on.
- The reconciler must never write a config naming both a tunnel
  address and a LAN address for the same resource - there is no
  plaintext fallback.
- A role flip must not change the rendered `remote` addresses.
- The installer must refuse to enable encryption without the
  prerequisites, and must say which one is missing.

## Consequences

- HAST replication traffic is encrypted, and the secondary still
  authenticates the connecting host by its real address, because the
  tunnel preserves the source address rather than terminating it.
- A WireGuard interface per pair of Combs sharing a resource is a new
  host prerequisite, and enabling it is a coordinated migration.
- hastd's own protocol, role management and split-brain detection are
  untouched. Only the address it is told to dial changes.
- The plaintext fallback does not exist. A broken tunnel means broken
  replication, which is visible, rather than silent plaintext.
