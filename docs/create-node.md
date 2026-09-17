# Create a new Apiary node

This runbook creates one independent Apiary Comb on a fresh FreeBSD host.
Use it for a standalone installation or to prepare and validate a host before
you later add it to a Colony. It does **not** add the node to another Colony.

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
  "rpc_addr": "0.0.0.0:17700",
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
  "manager_addr": "127.0.0.1:17700",
  "http_addr": "0.0.0.0:8080",
  "manager_tls": true,
  "manager_tls_ca": "/usr/local/etc/apiary-tls/cert.pem",
  "tls_cert": "/usr/local/etc/apiary-tls/cert.pem",
  "tls_key": "/usr/local/etc/apiary-tls/key.pem"
}
```

Use a different `http_addr` port for restshimd, normally `8081`.

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
