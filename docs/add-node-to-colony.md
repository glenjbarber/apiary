# Add a node to an existing Apiary Colony

This runbook adds a fresh, validated host to an existing Colony through
Apiary's mutually authorized join flow. It deliberately avoids the retired
manual SSH Unix-socket forwarding procedure.

The safety rule is simple: the joining node must be reachable and waiting for
membership before an Admin approves it. Adding an unreachable voter can make a
small Colony lose leadership.

Keep `apiary_raftd` stopped throughout ordinary host preparation. Start it
only after the `await_join` configuration in Step 2 is complete and you are
ready to proceed with the Colony join.

## 1. Prepare the new host

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

**If this host already formed its own independent Comb** (real state
already exists - this is not the case for a genuinely fresh host), skip
Step 2 and Step 4 below entirely and use the guided **"Convert this Comb
to a joiner"** action on that Comb's own Machine Configuration page
instead (ADR-0105). It performs the entire stop/reset/reconfigure/
restart/confirm/submit sequence those two steps describe by hand, in one
Admin-only action with an exact typed confirmation phrase: it stops local
`apiary_raftd`, moves its existing state aside to a timestamped backup
(reusing `raftd -reset` itself, not a reimplementation of it), rewrites
only `raft_bind`/`await_join` in `raftd.json` while leaving `node_id`,
`socket`, `internal_token`, and Raft TLS settings untouched, restarts
`raftd`, confirms it is actually listening at the new address before
doing anything else, and only then submits the join request against the
target Colony member you name - using this Comb's own real, current
identity. It fails closed (refuses and leaves the Comb running exactly as
it already was) on an unreachable target, an invalid `raft_bind`, or no
existing state to convert. Continue reading this whole document anyway -
**Step 3 (peer TLS trust) must still be configured on this Comb's own
`managerd.json` before you use the guided action**, since its final step
(submitting the join request) dials the target through the exact same
TLS-respecting path Step 3 sets up, not a separate plaintext path. Steps
5-7 still apply afterward. The manual sequence below documents exactly
what the guided action does internally - useful if it ever fails partway
and you need to understand the current state by hand.

## 2. Configure Raft to wait for approval

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

If this host previously bootstrapped its own independent Comb and you are
not using the guided "Convert this Comb to a joiner" action above (e.g. it
failed partway and you are recovering by hand, or you are following this
manually for another reason), stop raftd and reset only its local Raft
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

## 3. Establish trust before requesting the join

The joining managerd must be able to verify the target Colony member's
certificate before it can submit its request. If the target uses a
self-signed certificate, create a root-readable PEM trust bundle containing
that certificate and configure `peer_tls`, `peer_tls_ca`, and the peer TLS
hostname mapping on the joining host before continuing.

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
  "peer_tls_ca": "/usr/local/etc/apiary/peer-ca.pem",
  "peer_tls_hostname_map": "<existing-comb-address>=<existing-comb-name>"
}
```

The hostname-map value must match the DNS name in the existing Comb's
certificate. If that certificate is issued by a CA already trusted by the
joining host, omit `peer_tls_ca`; the system trust store is used instead.

If Raft transport mutual TLS is enabled, the existing Colony also needs to
trust the joining node's Raft certificate before approval. Do not weaken TLS
verification to bypass this requirement.

## 4. Start the local services

**If you used the guided "Convert this Comb to a joiner" action in Step 1,
the join request below is already submitted** - that action is itself an
RPC served by `managerd`/`frontend`, so those two must already have been
running (on this Comb's own prior standalone bootstrap) to reach its
Machine page at all; only `apiary_raftd` gets stopped and restarted by the
action itself. Skip ahead to Step 5 with the confirmation code that action
displayed.

Otherwise, start managerd and the frontend after raftd is waiting:

```bash
service apiary_managerd start
service apiary_frontend start
service apiary_restshimd start
```

Open the joining node's Machine Configuration page. In **Join a Colony**,
provide:

- the new node's `node_id`
- its reachable `raft_bind` address
- the address of an existing Colony managerd

Submit the request. The joining node displays a short confirmation code.

## 5. Approve on the existing Colony

On any existing Colony member, log in as an Apiary Admin and open the Colony
overview. In **Pending join requests**:

1. Confirm the requested node ID and Raft address are the intended host.
2. Compare the confirmation code with the code visible on the joining node.
3. Confirm the joining node is already reachable at its Raft address.
4. Use **Check reachability**. Resolve a blocked or unknown result before
   approving.
5. Approve the request.

The approval changes the real Raft membership. It is not merely a UI setting.

## 6. Complete peer trust after the join

Each member needs peer-forwarding configuration so managerd and the frontend
can reach the other Combs. Configure the shared peer API key, peer managerd
port, and peer TLS settings on every participating node.

For self-signed node certificates, update each member's peer CA bundle so it
contains every peer certificate it must trust, then configure the peer TLS CA
and hostname mapping. A real shared CA is preferable for a growing fleet.

Use the Machine Configuration page for the fields it exposes. Keep the peer
key and trust material root-readable only. Do not put secrets into Raft state
or command-line flags.

## 7. Verify the new member

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
- the confirmation codes differ
- the new host has already self-bootstrapped and has not been reset into
  `await_join` mode
- the existing Colony does not have a trustworthy view of its own quorum

These are not warnings to bypass. In a small Colony, an incorrect membership
change can immediately reduce or eliminate write availability.
