# Add a node to an existing Apiary Colony

This runbook adds a fresh, validated host to an existing Colony through
Apiary's mutually authorized join flow. It deliberately avoids the retired
manual SSH Unix-socket forwarding procedure.

The safety rule is simple: the joining node must be reachable and waiting for
membership before an Admin approves it. Adding an unreachable voter can make a
small Colony lose leadership.

## 1. Prepare the new host

Follow [Create a new Apiary node](create-node.md) through host preparation:

1. Install FreeBSD packages, clone the source, and build Apiary.
2. Run `apiaryinstall` and apply safe host prerequisites.
3. Prepare the uplink bridge with console access available.
4. Run `make setup` and `make install`.
5. Create valid `managerd.json`, `frontend.json`, and `restshimd.json` files
   with local TLS enabled.

Do not start a normal standalone `raftd` cluster on a node intended to join.
If it has already formed one, reset its local Raft state before continuing.

## 2. Configure Raft to wait for approval

Create `/usr/local/etc/apiary/raftd.json` from its installed sample and set a
stable node identity and a real routable Raft address:

```json
{
  "data_dir": "/var/db/apiary/raftd",
  "socket": "/var/run/apiary/raftd.sock",
  "node_id": "<new-node-id>",
  "raft_bind": "<new-node-address>:17600",
  "await_join": true
}
```

If this host previously bootstrapped its own independent Comb, stop raftd and
reset only its local Raft state before entering await-join mode:

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

Start managerd and the frontend after raftd is waiting:

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
