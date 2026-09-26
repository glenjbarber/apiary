# ADR-0139: `rpc_addr` is a dial target as well as a bind address

## Status

Accepted

## Context

`managerd`'s `rpc_addr` is one string with three unrelated jobs, and for
years the shipped documentation only described the first of them. A
four-node FreeBSD Colony followed the documentation exactly, set
`"rpc_addr": "0.0.0.0:17700"` on every Comb as instructed, and then
failed in a way that pointed confidently at the wrong subsystem.

The literal evidence, from one node's `raftd` log:

```
cannot reach managerd at 0.0.0.0:17700 (connecting to 0.0.0.0:17700: dial tcp 0.0.0.0:17700: connect: connection refused)
```

Nobody diagnosing that reaches for "the address is serving two roles".
They reach for certificates, because `managerd`'s external API is
TLS-only and a handshake failure is the obvious suspect. It was not a
handshake failure. `raftd` never got as far as one: on FreeBSD, connecting
to `0.0.0.0` is connection-refused.

### The three roles, precisely

**1. `net.Listen` bind.** `managerd` binds this address. This is the role
the docs described and the only one most operators knew about. An
unspecified address is perfectly legal here — that is what it means to
bind a wildcard.

**2. Local dial target.** `cmd/raftd/confirm.go`, added by ADR-0125 §2,
reads `rpc_addr` straight out of `/usr/local/etc/apiary/managerd.json`
and dials it, **on this same node**, to call
`ConfirmRestartCompletedLocal` on this node's own `managerd` when a
pending `apiary_raftd` restart needs confirming. `resolveLocalManagerdEndpoint`
is explicit that it prefers "whatever `managerd.json` actually says" over
any hardcoded default — a hardcoded dial was rejected precisely because it
is wrong on any node whose managerd is not on the default address. So
`rpc_addr` is simultaneously the thing raftd *binds around* and the thing
it *connects to*. A wildcard binds and then refuses.

**3. The port half of the peer-advertised manager address.** Peers reach
this Comb's managerd for write forwarding and cluster-overview host stats.
The host half of that address is **not** taken from `rpc_addr`. It is
derived independently as `nodeID + peerHostnameSuffix`, with the port
taken from `peer_manager_port` — see `internal/frontend/cluster_overview.go`,
where the whole host is constructed as `nodeID + s.peerHostnameSuffix +
":" + s.peerManagerPort`. `rpc_addr` therefore contributes only its port,
which is why the wildcard's host half never propagated to peers and why
this bug presented as a purely local failure.

### Why the obvious fix — the node's numeric LAN address — does not work

The instinct after `0.0.0.0` is `10.90.0.94:17700`. It binds, it dials,
and it cannot work, because of what the certificate is.

`make setup`'s `setup-tls` target generates a self-signed certificate
with `subjectAltName=IP:127.0.0.1,DNS:$(hostname)`. On the production
Colony that means every serving certificate carries SANs
`IP:127.0.0.1` and `DNS:<node>.lab3.home.arpa`, and **no SAN for the
node's LAN address**.

`cmd/raftd/confirm.go` sets `tls.Config.ServerName` only when its
`endpoint.serverName` is non-empty, and nothing ever sets it, so it stays
empty. Go then verifies the presented certificate against whatever host
was dialed. Neither `10.90.0.94` nor `0.0.0.0` is in the certificate, so
neither verifies. Only the DNS name is.

## Decision

**`rpc_addr` is the node's own DNS hostname.** In production:
`"rpc_addr": "brood.lab3.home.arpa:17700"` on `brood`, and every Comb's
`frontend` and `restshimd` point `manager_addr` at the same per-node name.
The wildcard guidance is withdrawn from `docs/bootstrap.md`,
`docs/create-node.md` and `etc/apiary/managerd.json.sample`.

The hostname is the recommended value, not merely a legal one, because
it is the only host in the cluster that satisfies all three roles:

| Candidate | Binds (role 1) | Local dial (role 2) | Verifies (role 2's TLS) |
|---|---|---|---|
| `0.0.0.0:17700` | yes | **no** — refused | n/a, never connects |
| `10.90.0.94:17700` | yes | yes | **no** — not a SAN |
| `127.0.0.1:17700` | yes | yes | yes |
| `<node>.lab3.home.arpa:17700` | yes | yes | yes — the DNS SAN |

`127.0.0.1:17700` is legal and verifiable but is not the answer for a
multi-node Colony: a loopback bind makes this Comb unreachable from every
other Comb, so peer forwarding and the cluster overview's host stats
have nothing to reach.

### The rule this implies

**An unspecified address is a legal bind and an illegal dial target.**
There is no value of `rpc_addr` that means "listen everywhere, connect
locally", because the same string has to serve both. A hostname listener
resolves to the node's LAN address, so it serves LAN clients and
deliberately does not serve `127.0.0.1` — which is why `manager_addr`
in `frontend.json`/`restshimd.json` is the same hostname rather than
loopback. That is a consequence to write down, not a second bug.

### Rejected alternatives

**`0.0.0.0:17700`, as previously shipped guidance.** Rejected because it
is exactly the reported failure. It was never wrong as a *bind*; it was
wrong as a *dial target*, and the documentation never disclosed that it
was serving both. Reinstating it as a documented option would preserve
the trap.

**A bare numeric LAN address, `10.90.0.94:17700`.** Rejected because it
cannot verify against the certificates this project actually issues. A
SAN-less `InsecureSkipVerify`, or a `peer_tls_hostname_map`-style
side-channel to paper over it, would be weakening TLS to accommodate a
documentation choice — the same trade `managerd.json.sample` already
warns against for `peer_tls_hostname_map` ("a missing mapping fails
verification rather than weakening TLS").

**Silently substituting a default when a configured value looks wrong.**
Rejected outright. `resolveLocalManagerdEndpoint` already declines to
guess: it falls back to a default only when the config file is absent or
unparseable, and says so in the log. The alternative — sniffing for
`0.0.0.0` and quietly dialing `127.0.0.1` instead — would have hidden
this class of outage entirely, leaving an operator with a bind address
that does not match reality and no error anywhere. A loud refusal is
better than a quiet reinterpretation. This is the same reasoning
ADR-0093's `LoadPeerCAPool` and ADR-0125's probe semantics already apply:
a wrong guess is a connection failure either way, so report it.

### restshimd's serving address, decided in the same pass

`restshimd` serves on `127.0.0.1:8081`. This is already
`internal/restshimdconfig`'s built-in default; the documentation and the
shipped sample contradicted the code. The exposure decision rests on
three facts:

- Nothing inside the Colony calls it. The frontend has its own
  conversion path; `raftd`, `managerd` and `apiaryinstall` never dial
  it. Its only intended clients are external tooling — `curl`, CI, a
  future provider.
- It is a full read/write control API: create, update, delete, migrate
  and ISO upload, across VMs, jails and networks.
- It has no authentication of its own. By design (ADR-0024) it holds no
  static key and forwards each caller's `Authorization` header through to
  `managerd` unchanged, so its entire security boundary is managerd's
  per-key auth (ADR-0023).

Remote callers use an SSH local forward, `ssh -L 8081:127.0.0.1:8081
<comb>`. LAN exposure becomes worth considering only once real managerd
API keys exist; until then the `Authorization` header is decorative.

`frontend` on `8080` is the deliberate opposite: it is the operator's
browser UI and is meant to be LAN-exposed. Its `0.0.0.0` is correct and
is now documented as such, because unlike `managerd` nothing ever dials
it and there is no second role for the string to break.

## Consequences

- `managerd`, `frontend` and `restshimd` configuration all name the same
  per-node hostname. A node whose DNS name changes needs `rpc_addr`,
  both `manager_addr` fields, and its certificate regenerated together.
- Every Comb's certificate must carry that node's own DNS SAN. A
  certificate without it produces a verification failure that reads like
  a trust-anchor problem, which is precisely the misdiagnosis this ADR
  exists to prevent.
- `raftd` restart confirmations now land on a reachable address, so a
  stale `apiary_raftd` restart lease is no longer a plausible consequence
  of following the documentation.
- The web UI's `UpdateManagerdBindAddress` (ADR-0104) now accepts a
  hostname, so the one value this ADR recommends can be set through the
  Machine Configuration page. The interface-inventory clause still applies
  in full to a numeric non-loopback address; it is skipped for a name,
  because the inventory holds only numeric addresses and resolving the
  name at validation time would make a legitimate save depend on DNS
  being up at that instant. ADR-0104 carries a dated amendment
  recording the original conflict and this resolution.
- `Makefile`'s `setup-quick` defaults changed with this decision:
  `NODE_RPC_ADDR` to `127.0.0.1:17700` (matching `cmd/managerd`'s own
  startup default, so a single-node bring-up writes the value managerd
  would have used anyway) and `NODE_REST_ADDR` to `127.0.0.1:8081`
  (matching `internal/restshimdconfig`'s code default). `NODE_HTTP_ADDR`
  is deliberately left at `0.0.0.0:8080`. A multi-Comb colony must
  override `NODE_RPC_ADDR` with the node's own name, because that is the
  only host its certificate carries a DNS SAN for.
- The distinction is now enforced, not just documented.
  `internal/addrpolicy` exposes `ValidateDialTarget` and
  `ValidateBindAddress` as two separately named functions rather than one
  predicate with a boolean — a boolean would let a future call site flip
  the distinction by accident, which is the mistake being fixed. The dial
  guard is applied to `restshimdconfig`'s and `frontendconfig`'s
  `manager_addr` and to `raftd`'s confirm hook; the bind guard accepts
  the wildcard. `raftd` does not refuse to start on a wildcard `rpc_addr`
  — it falls back to `127.0.0.1:17700` and logs why, because managerd is
  on that host and loopback is where a listener bound to every interface
  is reachable from.

## References

- ADR-0125 §2 — the `cmd/raftd/confirm.go` local-managerd dial that made
  `rpc_addr` a dial target.
- ADR-0104 — `UpdateManagerdBindAddress` / `validateLocalBindAddress`;
  amended by this ADR.
- ADR-0100 — the JSON config files all three roles live in.
- ADR-0024 — restshim's per-request `Authorization` forwarding, and the
  `127.0.0.1:8081` default.
- ADR-0023 — managerd's per-key auth, which is restshimd's whole
  security boundary.
- ADR-0087 / `make setup`'s `setup-tls` — where the certificate and its
  SANs come from.
