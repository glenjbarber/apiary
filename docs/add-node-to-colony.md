# Add a node to an existing Apiary Colony

This runbook adds a fresh, validated host to an existing Colony through
Apiary's mutually authorized join flow. It deliberately avoids the retired
manual SSH Unix-socket forwarding procedure.

The safety rule is simple: the joining node must be reachable and waiting for
membership before an Admin approves it. Adding an unreachable voter can make a
small Colony lose leadership.

There are two ways to prepare the joining side of this flow:

- **Guided-action path**: for a host that already formed its own independent
  Comb (real Raft state already exists). Use the **"Convert this Comb to a
  joiner"** action on that Comb's own Machine Configuration page (ADR-0105).
  It performs the stop/reset/reconfigure/restart/confirm/submit sequence for
  you in one Admin-only action.
- **Manual path**: for a genuinely fresh host (or when hand-recovering a
  guided conversion that failed partway). You edit `raftd.json` yourself,
  reset local Raft state if needed, and start services by hand.

Pick whichever path matches your situation, then follow it top to bottom. Both
paths share the same prerequisites below, and rejoin into the same approval
and verification steps afterward.

## Prepare the new host

Follow [Create a new Apiary node](create-node.md) through host preparation:

1. Install FreeBSD packages, clone the source, and build Apiary.
2. Run `apiaryinstall` and apply safe host prerequisites.
3. Prepare the uplink bridge with console access available.
4. Run `make setup` and `make install`.
5. Create valid `managerd.json`, `frontend.json`, and `restshimd.json` files
   with local TLS enabled.

Do not start a normal standalone `raftd` cluster on a node intended to join.
A fresh raftd without `await_join` immediately creates an independent Raft
cluster.

## Establish trust before requesting the join

The joining managerd must be able to verify the target Colony member's
certificate before it can submit its request. If the target uses a
self-signed certificate, create a root-readable PEM trust bundle containing
that certificate and configure `peer_tls` and `peer_tls_ca` on the joining
host before continuing. Both the guided-action path and the manual path rely
on this being configured before the join request is submitted.

**None of this section applies when every Comb's certificate comes
from a public CA** (Let's Encrypt or similar) - leave `peer_tls_ca`
empty on both sides, the system trust store already has the public
CA, and skip straight to setting `peer_tls: true` on both `managerd.json`
and `frontend.json` (see docs/bootstrap.md's "Profile: every
certificate from a public CA" for the rest of that profile's fields).

For example, copy the existing Comb's self-signed certificate into a
root-readable trust bundle on the joining host:

```bash
install -d -m 700 /usr/local/etc/apiary/peer-ca
scp root@<existing-comb>:/usr/local/etc/apiary-tls/cert.pem \
  /usr/local/etc/apiary/peer-ca/<existing-comb>.pem
cat /usr/local/etc/apiary/peer-ca/<existing-comb>.pem \
  > /usr/local/etc/apiary/peer-ca.pem
chmod 600 /usr/local/etc/apiary/peer-ca.pem
```

Then set the corresponding fields in the joining host's `managerd.json`:

```json
{
  "peer_tls": true,
  "peer_tls_ca": "/usr/local/etc/apiary/peer-ca.pem"
}
```

If that certificate is issued by a CA already trusted by the joining host,
omit `peer_tls_ca`; the system trust store is used instead.

As of ADR-0115, `peer_tls_hostname_map` no longer needs to be set by hand
for the normal case: each managerd derives its own IP-to-hostname map
automatically from raft's own known cluster membership (every voter's
raft-bind hostname, resolved via DNS), refreshed periodically, and this
covers exactly the case a raft leader_hint reports as a bare IP. Leave
`peer_tls_hostname_map` unset unless DNS resolution for a peer's raft-bind
hostname is unreliable on this network (e.g. split-horizon DNS gaps, a
peer bootstrapped with a raft-bind hostname that doesn't actually resolve
everywhere) or a peer's certificate names a hostname other than its
raft-bind hostname - in either case, set it exactly as before
(`"<ip>=<hostname>"`); a manually-configured entry always takes precedence
over the automatically derived one for the same IP.

If Raft transport mutual TLS is enabled, the existing Colony also needs to
trust the joining node's Raft certificate before approval. Do not weaken TLS
verification to bypass this requirement.

## Guided-action path

Use this path when this host already formed its own independent Comb (real
Raft state already exists - this is not the case for a genuinely fresh
host).

### Prerequisites (on the joining Comb)

1. **Trust configuration is already in place** in this Comb's `managerd.json`
   (`peer_tls` and `peer_tls_ca` as described above). The action's final
   step submits the join request through this exact TLS-respecting path.

2. **`managerd` and `frontend` are already running** from this Comb's prior
   standalone bootstrap. The guided action is an RPC served by these
   services; you reach the Machine page through them. Only `apiary_raftd`
   gets stopped and restarted by the action itself.

### Execute the guided action

1. Open this Comb's Machine Configuration page in the frontend.
2. Click **"Convert this Comb to a joiner"** (ADR-0105).
3. Enter the target Colony member's managerd address.
4. Enter this Comb's new raft-bind address: the `host:port` other Combs will
   dial to reach this one. It must be a real, routable address, never
   `0.0.0.0`, `127.0.0.1` or `localhost`, for the same reason as `raft_bind`
   in the [Manual path](#manual-path) below.
5. Type the exact confirmation phrase shown on the page.
6. Submit the action.

The action performs the following internally (fails closed on any error,
leaving the Comb running exactly as it was):
- Stops local `apiary_raftd`
- Moves existing Raft state aside to a timestamped backup (via `raftd -reset`)
- Rewrites only `raft_bind` and `await_join` in `raftd.json`, leaving
  `node_id`, `socket`, `internal_token`, and Raft TLS settings untouched
- Restarts `raftd`
- Confirms it is listening at the new `raft_bind` address
- Submits the join request against the target Colony member you named,
  using this Comb's own real, current identity

On success, the action displays a short **first code** (ADR-0147 Part 2 -
the same value `apiaryctl join-introduce` would have printed on the manual
path below) and the join request is already submitted.

### After the guided action completes

1. Note the first code shown by the action.
2. Continue to [Approve on the existing Colony](#approve-on-the-existing-colony)
   below with that first code.

> **Recovery note**: If the action fails partway and you need to understand
> the current state by hand, see the [Manual path](#manual-path) below - it
> documents exactly what the guided action does internally.

## Manual path

Use this path for a genuinely fresh host, or when hand-recovering a guided
conversion (above) that failed partway.

### 1. Configure Raft to wait for approval

Create `/usr/local/etc/apiary/raftd.json` from its installed sample and set a
stable node identity and a real routable Raft address:

```json
{
  "data_dir": "/var/db/apiary/raftd",
  "socket": "/var/run/apiary/raftd.sock",
  "node_id": "<new-node-id>",
  "raft_bind": "<this-host-address>:17600",
  "await_join": true
}
```

Replace `<this-host-address>` with the joining host's real, reachable
address. Do not use `0.0.0.0`: Apiary also advertises this value to Raft
peers.

**Verify the existing Colony member's own `raft_bind` too, not just the
joining host's.** `raft_bind` defaults to `127.0.0.1:17600` when left
unset (`internal/raft`'s own `DefaultBindAddr`) - if the existing member's
own first, original bootstrap never set this explicitly, its own address
as recorded in the cluster's committed Raft configuration may still be
loopback-only, unreachable from any other host including the one you are
now trying to join. This was confirmed live: an existing single-node Comb
that had never set `raft_bind` sat permanently unable to win an election
the moment a second voter was added, since neither side could reach the
other by the recorded address. Check this on the existing member *before*
troubleshooting anything about the joining host.

**If a voter's recorded address is stale (as above, or after any other
address change), use `UpdateVoterAddress` (ADR-0106), not a repeated
join request.** An earlier version of this recovery - resubmitting
`RequestJoinColony`/`ApproveJoinRequest` against the same, already-voting
`node_id` to let `AddVoter`'s own "existing voter -> update its address"
behavior take effect - is no longer possible: both RPCs now reject a
`node_id` that already names a current voter outright, specifically to
prevent an accidental collision between a new Comb and an existing one
from being silently treated as an address correction. `UpdateVoterAddress`
is the dedicated replacement - it requires `node_id` to already be a
known voter (the opposite of `RequestJoinColony`'s new check), applies
the same reachability guardrail against the new address that
`ApproveJoinRequest` applies against a joiner's, and is Admin-only. It
is available from the "Correct a stale voter address" form in the
Machine Configuration page's raftd panel (Admin only), or can still be
called directly via the manager RPC.

**If Raft transport mutual TLS (`raft_tls_cert`/`raft_tls_key`/
`raft_tls_ca`, ADR-0078) is used anywhere in this Colony, every voter must
have all three set identically in kind - all set, or all left empty.** A
partial mismatch (one voter with Raft TLS configured, another without) is
a real, confirmed failure mode, not a hypothetical: the TLS-configured
side's `raftd.log` shows `tls: first record does not look like a TLS
handshake` repeating for every RPC attempt from the plaintext side; the
plaintext side's own `raftd.log` shows a bare `error=EOF` on
`failed to make requestVote RPC`, with pre-votes endlessly denied and no
leader ever elected. This looks identical to a network reachability
problem but is not one - `service apiary_raftd onestatus` and even
`ps`/`sockstat` will show `raftd` genuinely running and listening on both
sides throughout.

If this host previously bootstrapped its own independent Comb and you are
recovering by hand rather than using the guided-action path above (for
example, it failed partway), stop raftd and reset only its local Raft
state before entering await-join mode:

```bash
service apiary_raftd stop
/usr/local/libexec/apiary/raftd -reset yes-wipe-raft-state
```

Then start raftd and confirm that it is listening without self-bootstrapping:

```bash
service apiary_raftd start
tail -f /var/log/apiary/raftd.log
```

Keep this node reachable at its configured `raft_bind` address. Do not approve
the join until this is true.

### 2. Start the local services

Start managerd and the frontend after raftd is waiting:

```bash
service apiary_managerd start
service apiary_frontend start
service apiary_restshimd start
```

### 3. Submit the join request

There is deliberately no web form for this step (ADR-0147 Part 2): the
first code has to be generated by the joining Comb itself and read by the
target's Admin off that Comb's own screen, which is something a form on
the joiner cannot honestly do - it would satisfy the check without a
person ever looking at it. Run this on the joining node itself:

```bash
apiaryctl join-introduce --target <existing-comb-managerd-address>
```

Name any current member of the Colony, not only its leader. It prints the
first code, this Comb's certificate fingerprints (read from its own
certificate files), and a request id.

Continue to [Approve on the existing Colony](#approve-on-the-existing-colony)
below with that first code and the fingerprints printed above - the
existing Colony's Admin pastes both into the "Pending join requests"
panel's own first-code form, not into anything on the joining node.
Afterward, poll for the target's second PIN:

```bash
apiaryctl join-introduce --request-id <request-id> --first-code <first-code> \
  --target <existing-comb-managerd-address> --field second-pin
```

## Approve on the existing Colony

On any existing Colony member, log in as an Apiary Admin and open the Colony
overview. In **Pending join requests**, this is a two-stage handshake
(ADR-0147 Part 2), not a single approval:

1. Confirm the requested node ID and Raft address are the intended host.
2. **Stage 1 - verify the introduction.** In the request's own "Stage 1:
   verify the first code and fingerprints" form (open by default while the
   request is waiting on this step), paste the **first code** and every
   certificate fingerprint exactly as shown on the joining Comb's own
   screen, and submit. A mismatch on either value is a refusal and counts
   as an attempt.
3. Once verified, the target generates a **second PIN**, visible only on
   the joining Comb (poll for it with `apiaryctl join-introduce ... --field
   second-pin`, or read it off the guided action's own result if the
   joining Comb used that path). It is never shown on this page.
4. Confirm the joining node is already reachable at its Raft address.
5. Use **Preflight approval** (Check reachability). Resolve a blocked or
   unknown result before approving.
6. **Stage 2 - approve.** In the same request's Approve form, paste the
   second PIN read off the joining Comb's own screen, type the exact
   confirmation phrase, and submit.

The approval changes the real Raft membership. It is not merely a UI setting.

## Complete peer trust after the join

Each member needs peer-forwarding configuration so managerd and the frontend
can reach the other Combs. Configure the shared peer API key, peer managerd
port, and peer TLS settings on every participating node - including any
existing member that has never needed peer configuration before (a Comb
that was the very first, standalone-bootstrapped node in its Colony may
have none of this set at all, not just an incomplete version of it).

**`managerd.json` and `frontend.json` each have their own, separate
`peer_tls`/`peer_tls_ca` fields.** `managerd`'s own peer settings cover
managerd-to-managerd RPC forwarding (reconciliation writes/reads, ADR-0029).
`frontend`'s own, independent peer settings (also `peer_tls`/`peer_tls_ca`,
plus `peer_manager_port`) cover the Colony overview page's own direct
host-stats fetch from every other Comb's managerd.

As of ADR-0113's follow-up, leaving `peer_tls` unset on either file is no
longer the same as setting it to `false`: `managerd.json`'s `peer_tls`
defaults to on when that Comb's own `tls_cert`/`tls_key` are both set, and
`frontend.json`'s `peer_tls` defaults to whatever its own `manager_tls` is
set to. Joining two nodes that both use publicly trusted (or otherwise
properly configured) TLS certificates needs no manual `peer_tls` edits at
all any more - explicit `"peer_tls": true`/`false` in either file still
overrides its own default. The historical failure mode below can now only
happen from an explicit, mismatched override.

Confirmed live (before that default existed): fixing only `managerd.json`'s
peer TLS left the Colony overview still showing `rpc error: code =
Unavailable desc = connection error: desc = "error reading server preface:
EOF"` for the other Comb, because `frontend.json` on that same host still
had no peer TLS configured at all. That exact error text means a plaintext
client is dialing a TLS-only managerd. The Colony overview and the
per-host page now translate that exact error into a message naming both
files' `peer_tls`/`peer_tls_ca`/`tls_cert`/`tls_key` fields directly,
rather than showing the bare gRPC text - but checking both files' settings
on the host reporting the error, not just one, is still the fix.

For self-signed node certificates, update each member's peer CA bundle so it
contains every peer certificate it must trust, then configure the peer TLS CA
and hostname mapping. A real shared CA is preferable for a growing fleet. The
same trust bundle file can be reused for both `managerd.json`'s and
`frontend.json`'s `peer_tls_ca` field on a given host - it is the same
peer-trust requirement in both cases, just consumed by two independent
config surfaces.

Use the Machine Configuration page for the fields it exposes. Keep the peer
key and trust material root-readable only. Do not put secrets into Raft state
or command-line flags.

## Verify the new member

Confirm from more than one Colony frontend that:

- the new Comb appears as reachable
- its Raft membership is voter status as intended
- peer-forwarded host statistics work
- the health panel has current evidence rather than a stale or unknown result

If the request was sent to the wrong target or becomes obsolete, cancel it from
the joining node while pending, or have an Admin purge the request from the
existing Colony. Do not approve a request merely to clean it up.

## Stop conditions

Stop before approving when any of the following is true:

- the node ID is unexpected or still a placeholder
- the Raft address is unreachable, incorrect, or not the address being shown
  by the joining host
- the first codes or any certificate fingerprint differ
- the new host has already self-bootstrapped and has not been reset into
  `await_join` mode
- the existing Colony does not have a trustworthy view of its own quorum

These are not warnings to bypass. In a small Colony, an incorrect membership
change can immediately reduce or eliminate write availability.
