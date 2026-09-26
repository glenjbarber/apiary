# Create a new Apiary node

This runbook creates one independent Apiary Comb on a fresh FreeBSD host.
Use it only for the first node in a new Colony or for a node that will remain
standalone. It does **not** add the node to an existing Colony.

If this host will join an existing Colony, follow
[Add a node to an existing Colony](add-node-to-colony.md) from the outset.
Do not start `apiary_raftd` during generic host preparation: a fresh raftd
without `await_join` immediately creates its own independent Raft cluster.

For the complete historical reference and troubleshooting notes, see
[bootstrap.md](bootstrap.md).

## Before you start

- Work as `root` throughout. The setup targets do not call `sudo` internally.
- On bare metal, enable VT-x/EPT in BIOS before relying on bhyve.
- On a laptop, prevent lid-close suspend before it becomes a cluster node.
- If you mix CPU generations, start new guests on the oldest host. Migration
  from an older CPU to a newer one is safe; the reverse may not be.
- Decide the host's stable `node_id`, routable address, ZFS pool, and uplink
  interface before writing configuration. Do not use placeholder text as a
  real `node_id`: it can be committed to persistent Raft state.
- Decide this host's **DNS name** before writing configuration. `managerd`'s
  `rpc_addr` is that name, not a wildcard and not a LAN address, and every
  Comb's `frontend` and `restshimd` use the same name for their
  `manager_addr` — see Step 6 and ADR-0139.

## 1. Get the source and build

```bash
pkg install -y git go
git clone https://github.com/glenjbarber/apiary.git ~/apiary
cd ~/apiary
make build
```

If the checkout belongs to another UNIX account, either build as that account
or mark the checkout safe for Git before building as root:

```bash
git config --global --add safe.directory /path/to/apiary
```

## 2. Check host prerequisites

Run the report first:

```bash
./apiaryinstall
```

Then apply the safe prerequisites. The pool must already exist.

```bash
./apiaryinstall -apply -zfs-pool <pool-name>
```

Repeat until every check other than networking reports `ok`.

## 3. Prepare host networking

Find the physical uplink:

```bash
ifconfig
```

Review the proposed bridge change:

```bash
./apiaryinstall -vlan-uplink <uplink-ifname> -bhyve-bridge bridge0
```

The next command can interrupt SSH when it changes the connected interface.
Use a console when possible.

```bash
./apiaryinstall -apply-network yes-modify-network \
  -vlan-uplink <uplink-ifname> -bhyve-bridge bridge0
```

If the uplink currently uses DHCP, this persists the safe bridge layout:
the physical NIC becomes an addressless member, `bridge0` inherits its MAC,
and `SYNCDHCP` moves the management lease to the bridge. The stock
`/etc/devd/dhclient.conf` remains enabled.

Verify the result:

```bash
./apiaryinstall -vlan-uplink <uplink-ifname> -bhyve-bridge bridge0
```

## 4. Install runtime support and generate local TLS

```bash
make setup
make install
```

`make setup` creates runtime directories, installs and enables Apiary rc.d
scripts, installs a PAM policy if one does not already exist, and creates a
local TLS certificate unless one is already present. `make install` copies the
daemon binaries and their commented JSON samples into the locations used by
the rc.d scripts.

Find the installed bhyve UEFI firmware before writing `managerd.json`:

```bash
pkg info -l edk2-bhyve
```

The usual path is `/usr/local/share/uefi-firmware/BHYVE_UEFI.fd`.

## 5. Configure and start the independent Raft node

Only continue with this section when this is the first or only node. A host
intended for an existing Colony must remain stopped until its `await_join`
configuration is complete and the join operation is ready to begin.

Create the real configuration from the installed sample:

```bash
sed -e '/^[[:space:]]*\/\//d' -e '/^[[:space:]]*$/d' \
  /usr/local/etc/apiary/raftd.json.sample \
  > /usr/local/etc/apiary/raftd.json
chmod 600 /usr/local/etc/apiary/raftd.json
```

Then edit the generated file with this separate command:

```bash
${EDITOR:-vi} /usr/local/etc/apiary/raftd.json
```

For an independent node, omit `join` and `await_join` entirely:

```json
{
  "data_dir": "/var/db/apiary/raftd",
  "socket": "/var/run/apiary/raftd.sock",
  "node_id": "<stable-node-id>",
  "raft_bind": "<this-host-address>:17600"
}
```

Replace `<this-host-address>` with this host's real, reachable address.
Do not use `0.0.0.0`: Apiary also advertises this value to Raft peers.

Start it through rc.d:

```bash
service apiary_raftd start
```

## 6. Configure and start managerd

```bash
sed -e '/^[[:space:]]*\/\//d' -e '/^[[:space:]]*$/d' \
  /usr/local/etc/apiary/managerd.json.sample \
  > /usr/local/etc/apiary/managerd.json
chmod 600 /usr/local/etc/apiary/managerd.json
```

Then edit the generated file with this separate command:

```bash
${EDITOR:-vi} /usr/local/etc/apiary/managerd.json
```

Set at least the host identity, local Raft socket, manager address, storage,
bridge, uplink, firmware, image directory, and local TLS paths:

```json
{
  "raftd_socket": "/var/run/apiary/raftd.sock",
  "rpc_addr": "brood.lab3.home.arpa:17700",
  "node_id": "<stable-node-id>",
  "zfs_base": "<pool-name>/apiary",
  "bhyve_bootrom": "/usr/local/share/uefi-firmware/BHYVE_UEFI.fd",
  "bhyve_bridge": "bridge0",
  "uplink": "<uplink-ifname>",
  "iso_dir": "/var/db/apiary/isos",
  "tls_cert": "/usr/local/etc/apiary-tls/cert.pem",
  "tls_key": "/usr/local/etc/apiary-tls/key.pem"
}
```

Replace `brood.lab3.home.arpa` with **this host's own DNS name**. Do not
use `0.0.0.0` and do not use the numeric LAN address.

`rpc_addr` does three jobs, not one (ADR-0139). It is what `managerd`
binds with `net.Listen`; it is also the address `cmd/raftd/confirm.go`
*dials*, on this host, to confirm a pending `raftd` restart to this
node's own `managerd`; and it contributes only the port half of the
address peers use to reach this Comb — their host half is built
independently as `node_id` plus `peer_hostname_suffix`. An unspecified
address is a legal bind and an illegal dial target, which is the entire
reason the wildcard is wrong: `raftd` would bind its own managerd fine
and then be refused every time it tried to connect, logging

```
cannot reach managerd at 0.0.0.0:17700 (connecting to 0.0.0.0:17700: dial tcp 0.0.0.0:17700: connect: connection refused)
```

which reads as a TLS fault and is not one.

The numeric LAN address fails for a different reason. Step 4's
certificate carries `subjectAltName=IP:127.0.0.1,DNS:$(hostname)` and
no SAN for the LAN address, and `cmd/raftd/confirm.go` leaves its TLS
`serverName` empty, so Go verifies that certificate against whatever host
was dialed. The DNS name is the one host it can be verified against.

A listener bound to the node's own name resolves to that name's LAN
address, so it serves LAN clients but deliberately does not serve
`127.0.0.1`. Step 7's `manager_addr` therefore repeats the hostname
rather than pointing at loopback.

```bash
service apiary_managerd start
```

## 7. Configure and start the web and REST services

```bash
sed -e '/^[[:space:]]*\/\//d' -e '/^[[:space:]]*$/d' \
  /usr/local/etc/apiary/frontend.json.sample \
  > /usr/local/etc/apiary/frontend.json
sed -e '/^[[:space:]]*\/\//d' -e '/^[[:space:]]*$/d' \
  /usr/local/etc/apiary/restshimd.json.sample \
  > /usr/local/etc/apiary/restshimd.json
```

Edit each generated file separately. Do not paste both editor commands at
once, because the second command can be consumed as input by the first editor.

```bash
${EDITOR:-vi} /usr/local/etc/apiary/frontend.json
```

```bash
${EDITOR:-vi} /usr/local/etc/apiary/restshimd.json
```

Both services must trust managerd's certificate:

```json
{
  "manager_addr": "brood.lab3.home.arpa:17700",
  "http_addr": "0.0.0.0:8080",
  "manager_tls": true,
  "manager_tls_ca": "/usr/local/etc/apiary-tls/cert.pem",
  "tls_cert": "/usr/local/etc/apiary-tls/cert.pem",
  "tls_key": "/usr/local/etc/apiary-tls/key.pem"
}
```

`manager_addr` repeats this node's own DNS name, matching Step 6's
`rpc_addr` exactly. Not `127.0.0.1` — a managerd listening on its own
name does not serve loopback (ADR-0139). Not a wildcard either:
`manager_tls_ca` establishes trust in the certificate, not which name
that certificate must match, so the dialed host still has to be a SAN.

`http_addr` here is `0.0.0.0:8080` for `frontend`, and that wildcard is
deliberate. The web UI is the operator's browser surface, reachable from
a LAN, and Step 8 opens it from another machine.

restshimd is configured separately, and differently:

```json
{
  "manager_addr": "brood.lab3.home.arpa:17700",
  "http_addr": "127.0.0.1:8081",
  "manager_tls": true,
  "manager_tls_ca": "/usr/local/etc/apiary-tls/cert.pem",
  "tls_cert": "/usr/local/etc/apiary-tls/cert.pem",
  "tls_key": "/usr/local/etc/apiary-tls/key.pem"
}
```

**restshimd serves on `127.0.0.1:8081`, not `0.0.0.0:8081`** — which
reverses what the shipped sample and this step used to say, and matches
`internal/restshimdconfig`'s own built-in default, so the documentation
had been contradicting the code. Same `manager_addr` and same
`manager_tls` requirements as frontend; the different exposure is
deliberate:

- Nothing inside the Colony ever calls restshimd. The web UI has its own
  conversion path, and raftd, managerd, and apiaryinstall never dial it.
  Its only intended clients are external tooling such as `curl`, CI, or a
  future provider.
- It is a full read/write control API — create, update, delete, migrate,
  and ISO upload across VMs, jails, and networks. Not a read-only status
  endpoint.
- It has no authentication of its own. It holds no static key; it
  forwards each caller's own `Authorization` header through to managerd
  unchanged (ADR-0024), so its entire security boundary is managerd's
  per-key auth (ADR-0023).

Remote callers use an SSH local forward rather than a LAN bind:

```bash
ssh -L 8081:127.0.0.1:8081 brood.lab3.home.arpa
```

Only consider LAN exposure for `8081` once managerd API keys are
actually configured. Until then the `Authorization` header is decorative
and the port is an unauthenticated control surface. `frontend` on `8080`
is the opposite case: that one is meant to be LAN-exposed, because it is
the UI a human uses.

```bash
service apiary_frontend start
service apiary_restshimd start
```

## 8. Verify and secure the node

Open `https://<this-host-address>:8080`. The overview should show one
reachable, healthy Comb.

Enable real PAM login before exposing the frontend to an untrusted network:

```bash
make setup-pam
```

Edit the configuration in a separate command:

```bash
${EDITOR:-vi} /usr/local/etc/apiary/managerd.json
```

Add this field to `managerd.json`:

```json
"pam_service": "apiary"
```

Then restart managerd followed by frontend. Frontend checks PAM status only
at startup:

```bash
service apiary_managerd restart
service apiary_frontend restart
```

The first successful PAM login becomes Apiary Admin. Log in immediately using
the intended administrator account.

To add this validated node to an existing Colony later, follow
[Add a node to an existing Colony](add-node-to-colony.md). Do not reuse the
old SSH Unix-socket tunnel method from the detailed historical reference.
