# The Apiary Handbook

How to use Apiary: what it is, how to bring up a Colony, how to work
with it day to day, and what to do when something is wrong.

This is the document to read when you are handed an Apiary Comb and asked
to do something with it. It is not a design document - the design lives
in `docs/adr/` - and it is not a reference for the RPC surface, which
lives in `api/rpc/manager.proto`.

**If you are in a hurry**, jump to the section you need:

| You want to... | Read |
| --- | --- |
| Understand what Apiary is | [What Apiary is](#what-apiary-is) |
| Bring up the first node | [Bringing up your first Comb](#bringing-up-your-first-comb) |
| Add a node to an existing Colony | [Adding a node to a Colony](#adding-a-node-to-a-colony) |
| Create a VM or a jail | [Workloads](#workloads) |
| Set up certificates | [Certificates](#certificates) |
| Find out what is broken | [When something is wrong](#when-something-is-wrong) |
| Look up a command | [Command reference](#command-reference) |

## What Apiary is

An Apiary **Comb** is one FreeBSD host running an Apiary installation.
A **Colony** is one or more Combs that have agreed to be a single
cluster, with one of them holding leadership at any moment.

Each Comb runs four services:

| Service | What it does | Where you see it |
| --- | --- | --- |
| `apiary_raftd` | The consensus layer. Holds the Colony's replicated state. | Never directly. |
| `apiary_managerd` | The authority. Every state change goes through it. | Never directly. |
| `apiary_frontend` | The web UI. | <http://localhost:8080> |
| `apiary_restshimd` | A REST view of the same API, for external tools. | <http://localhost:8081> |

The web UI is where most work happens. `apiaryctl` is the command line
for the things that must happen on a headless host - which is, in
practice, every node that is not your desk.

Two words the rest of this document uses a lot:

- **Node** or **Comb**: one host.
- **Cell**: one thing Apiary runs - either a **VM** (a bhyve virtual
  machine) or a **jail** (a FreeBSD jail). Both are addressed by an
  ID, and a cell's ID is unique only *within its type*.

## Bringing up your first Comb

The full procedure is in [Create a new Apiary node](create-node.md).
This is the shape of it.

You need a FreeBSD host, root access, and the Apiary source to build
from. The build itself is ordinary:

```sh
git clone https://github.com/glenjbarber/apiary
cd apiary
make setup
make install
```

`make setup` writes the configuration files for the local host and
generates this Comb's serving certificate and identity. It is
**not** something to run on a node that will join an existing Colony -
see the next section for why that matters.

Once installed, the four services live under `/usr/local/etc/rc.d/`:

```sh
service apiary_raftd start
service apiary_managerd start
service apiary_frontend start
service apiary_restshimd start
```

Then open <http://localhost:8080>.

**The first login becomes the Admin.** Apiary creates no accounts on
its own: until one exists, the login page says so, and the first
successful login on this Comb is granted the Admin role. Every later
account is created by an Admin, with a role of their choosing.

If `pam_service` is configured, those accounts come from PAM instead,
and the same first-login rule applies to the first PAM user.

**The one thing to get right:** do not start `apiary_raftd` on a host
that is going to join an existing Colony. A fresh `raftd` with no
`await_join` immediately forms its own independent cluster, and that
cluster will then have to be discarded before the join can work.

## Adding a node to a Colony

The full procedure is in
[Add a node to an existing Colony](add-node-to-colony.md). The flow is
a **two-way, human-attested handshake**, and the human is not a
formality - it is the security property.

Here is why it is shaped that way. A join request has to travel from
the joining Comb to the target Colony, and something has to stop a
stranger from asking to join. The answer is that the target's operator
has to read a value off one screen and type it into another. That
transcription is the check, and no amount of automation replaces it.

### The flow

**1. Prepare the joining host.** Follow
[Create a new Apiary node](create-node.md) for the host preparation,
but **stop before starting `apiary_raftd`**.

**2. The joining Comb introduces itself and prints a first code.**

```sh
/usr/local/sbin/apiaryctl join-introduce --target leader.example.com:17700
```

This prints a 6-digit **first code**, this Comb's certificate
fingerprints, and a request ID. It submits the request to the target;
it does not approve anything.

**3. The target's Admin approves it.** On the target Colony's web UI,
the pending request shows the joining Comb's fingerprints. The Admin
compares them against what step 2 printed, and enters the second PIN
the joining Comb is showing.

The **join ledger** - *Machine* page, "Open the guided join flow" -
shows both Combs' steps side by side, so you always know which side
you are on and what the next value is. It never renders the second PIN
- that value is released only to the joining Comb.

**4. The joining Comb polls for approval and installs its credentials.**

```sh
/usr/local/sbin/apiaryctl join-introduce \
    --request-id jreq-XXXX --first-code 123456 \
    --target leader.example.com:17700 --install-credentials
```

This installs the Colony's peer credentials on the joining Comb and
restarts `managerd` and `frontend` so they pick them up.

**5. If the joining Comb was given a prepared certificate**, add the
reservation the preparing Comb printed:

```sh
/usr/local/sbin/apiaryctl join-introduce \
    --request-id jreq-XXXX --first-code 123456 \
    --target leader.example.com:17700 --install-credentials \
    --reservation-id 7Qk2...Vi
```

The same command then adopts the prepared pair as this Comb's own
serving certificate and reports the active fingerprint.

The reservation ID says **which** preparation is being collected. It
is not what authorizes the collection - the seal addressed to this
Comb's own certificate is - so presenting it is safe, and an id that
does not match the join is refused.

### Getting the values out of a script

`--field` prints one bare value and nothing else, so it can go
straight into another form:

```sh
/usr/local/sbin/apiaryctl join-introduce --target leader:17700 --field first-code
/usr/local/sbin/apiaryctl join-introduce --target leader:17700 --field fingerprints
/usr/local/sbin/apiaryctl join-introduce --target leader:17700 --field request-id
/usr/local/sbin/apiaryctl join-introduce --target leader:17700 --field second-pin
```

No labels, no quoting, no trailing explanation - because the output
goes into a form on another machine, and every decoration is a chance
to paste the wrong thing.

## Roles

Apiary has three roles, and they decide what an account can do.

| Role | Can do |
| --- | --- |
| `viewer` | Read. The monitoring API, the inventory, the health of the Colony. |
| `operator` | Read, and run workloads. Create and manage VMs and jails. |
| `admin` | Everything, plus the things that change the Colony itself: approving a join, managing API keys and accounts, pinning trust anchors. |

A role can be attached to an account per-Comb, so the same person can
be an Admin on one Comb and a Viewer on another.

An RPC that is not in the role map defaults to Admin, so a missing
entry is a fail-OPEN mistake rather than a safe one. The map is
checked by a test for exactly that reason.

## Workloads

### VMs

A VM is a bhyve virtual machine. Creating one needs a **base image**:
an ISO or a raw disk image.

- **Installer media** (an ISO) cannot be booted from as a base image.
  Upload it as an ISO and use it as the install media.
- **A compressed raw image** (`.raw.xz`, `.raw.gz`, `.raw.bz2`,
  `.raw.zst`) can be used as a base image, and Apiary decompresses it
  while creating the disk. A compressed image used as *installer
  media* is refused, because there is nothing to boot from it.

Upload images on the **Images** page, then create a VM on the **VMs**
page.

### Jails

A jail is a FreeBSD jail - a process-level container that shares the
host kernel. It needs a **base image** too, but a jail template
rather than a disk image.

### Networks

Networks are defined on the **Networks** page. A network needs a
subnet and a bridge; the bridge has to exist on the host first.

### The REST API

`apiary_restshimd` exposes the same read-only view as REST, on
<http://localhost:8081>, for external tooling - Terraform, CI, a
monitoring system.

```sh
curl -H "Authorization: Bearer $APIARY_API_KEY" http://localhost:8081/v1/vms
curl -H "Authorization: Bearer $APIARY_API_KEY" http://localhost:8081/v1/monitoring/health
```

| Endpoint | What it returns |
| --- | --- |
| `GET /v1/status` | This Comb's status. |
| `GET /v1/health` | The Colony's health verdict. |
| `GET /v1/monitoring/health` | The Colony's verdict plus one row per Comb, with measurements. |
| `GET /v1/monitoring/nodes` | The per-Comb rows on their own. |
| `GET /v1/vms` | The VM inventory. |
| `GET /v1/jails` | The jail inventory. |
| `GET /v1/networks` | The network inventory. |

The key is a normal Apiary API key with at least the `viewer` role,
sent as a bearer token. The REST shim forwards the caller's own key to
`managerd` - it has no identity of its own - so the key's role is
what decides what comes back.

**A measurement that was not taken is absent, not zero.** The
monitoring endpoints omit a measurement rather than rendering it as 0,
because a monitoring system that reads "memory available: 0" during an
outage will page an operator about a healthy machine.

### Where a workload runs

Every cell is owned by exactly one Comb, and the Comb that owns it
runs it. The **Cluster** view on the Machine page shows which.

## Certificates

Apiary uses TLS for every inter-host connection, and a Comb's own
serving certificate is what its peers pin.

### The everyday case

The certificate is issued automatically at install time and renewed
without a restart — the services re-read it on the next TLS handshake,
so a renewal never drops your session. The **Certificates** page shows
what each Comb holds and how long it has left. You do not normally
need to touch it.

A renewal is triggered by the certificate's **own expiry**, not by a
fixed schedule, so a short-lived certificate and a long-lived one are
both handled without configuration. If a renewal fails, the current
certificate keeps serving and the failure is reported — an outage at
the CA does not take your Comb offline.

### Public certificates from Cloudflare

If you want a certificate from a public CA - so the Comb's name
resolves publicly and the certificate is trusted by browsers - set up
the Cloudflare integration once with the guided walkthrough:

```sh
/usr/local/sbin/apiaryctl cert-setup setup
```

It asks for a Cloudflare API token (read without echo, written to a
0600 file), resolves the zone, **validates the token against that
zone**, asks for the naming suffix this integration may issue for, and
then obtains the certificate.

The suffix matters. It is what stops the saved integration from
issuing a certificate for a name you did not mean to give this Colony.
An empty suffix is refused rather than treated as a wildcard.

After the integration is saved, preparing a certificate for another
Comb reuses it - no credential re-entry:

```sh
/usr/local/sbin/apiaryctl cert-setup prepare --name host2.lab2.apiary.work
```

That prints a **reservation ID**, which the joining Comb presents when
it collects the certificate during the join:

```sh
/usr/local/sbin/apiaryctl join-introduce \
    --request-id jreq-XXXX --first-code 123456 \
    --target leader.example.com:17700 --install-credentials \
    --reservation-id 7Qk2...Vi
```

A Comb that was prepared for is also given the **Cloudflare
integration** — the token, the zone and the naming suffix — so it can
obtain and renew certificates of its own. A Comb that was *not*
prepared for gets no credential at all: a zone-scoped token can change
what every host in that zone resolves to, and handing one to every
Comb would make a single compromised Comb a way to redirect the whole
zone.

**The token is shared.** The preparing Comb and the joining Comb both
hold it, which is the cost of this design. Keep it as narrow as
Cloudflare allows — a zone-scoped token with only DNS edit — and
rotate it when a Comb leaves the Colony.

### Rotating a certificate

A Comb's pinned certificate is replaced through a **trust transition**,
not by overwriting a file. The transition stages the new certificate,
both are trusted briefly, and then the old one is retired. A failed
transition leaves the previous certificate trusted and serving.

## When something is wrong

### Start with the Cluster view

The **Machine** page's Cluster section shows every Comb's health
verdict. The vocabulary is five states, and one of them is the point:

| Verdict | Meaning |
| --- | --- |
| `healthy` | Observed, and nothing wrong. |
| `degraded` | Observed, and something is wrong. |
| `stale` | The observation is old. |
| `unknown` | There is no verdict. **Not** the same as healthy. |
| `contradictory` | The Combs disagree about this Comb. |

An unreachable Comb and a Comb that was never reached both read
`unknown`. That is deliberate: absence of a verdict is not a verdict,
and a monitoring system that cannot tell the difference will report
health it did not establish.

### The Offline Recovery Handbook

The **Recovery Handbook** page is the built-in diagnostic for a Colony
that is not working. It gathers the evidence - each member's state,
the assumptions register, the health signals - and produces a numbered
procedure with a stop condition on each step.

It is deliberately **offline**: it gathers what it can from this Comb
and prints the procedure, rather than acting on the Colony. A recovery
procedure that acts on a Colony which is failing is a recovery
procedure that can make it worse.

### Command-line triage

```sh
service apiary_managerd status     # is it running?
service apiary_raftd status
sockstat -4 -l | grep 17700        # is managerd listening?
```

### Restarting services

The Machine page's per-service control restarts a service and confirms
it came back. For the command line:

```sh
/usr/local/sbin/apiaryctl force-restart
```

It restarts `managerd` then `raftd`, confirming each by its own
listener port, and **refuses to run on the Colony's current leader** -
restarting `raftd` on the leader is the one restart the Colony cannot
absorb. It also refuses when it cannot determine the leadership,
because an unanswered question is not a permission.

### The trust store

If peers cannot verify each other, the Colony's peer trust store may
be empty - which happens on a Colony built before that store existed.
`apiaryctl pin-trusted-peers` backfills it:

```sh
/usr/local/sbin/apiaryctl pin-trusted-peers \
    --target leader.example.com:17700 --bundle /tmp/certs.json
```

Every input certificate is parsed, evaluated and matched **before** the
first pin is sent. A bundle whose fourth certificate is bad produces
zero pins, not three.

## Command reference

`apiaryctl` is installed at `/usr/local/sbin/apiaryctl` and is
**not on `PATH`**. Invoke it by its full path.

| Command | What it does | Root? |
| --- | --- | --- |
| `install` | Generate this Comb's configuration, certificate and identity. Reports only unless `--apply`. | Yes |
| `join-introduce` | Start or poll a two-way Colony join. Prints values; submits no approvals. | For `--install-credentials` |
| `join-authorize` | Authorize one Comb to join, by writing a single-use entry. | Yes |
| `force-restart` | Restart `managerd` then `raftd`, confirming each. Refuses on the leader. | - |
| `pin-trusted-peers` | Backfill the replicated peer trust store. | Yes |
| `peer-credential-backfill` | Install the Colony peer key on a Comb that predates automatic delivery. | Yes |
| `restart-guardrail-provision` | Generate the restart-guardrail token on a Colony that has none. | Yes |
| `migrate-tls-dir` | Move the serving certificate to the current default location. Reports only unless `--apply`. | Yes |
| `colony-ca init` | Create the Colony CA, on the first Comb of a Colony. | Yes |
| `colony-ca issue-raft` | Sign this Comb's raft certificate with the Colony CA. | Yes |
| `cert-setup setup` | Guided Cloudflare setup and certificate issuance. | Yes |
| `cert-setup prepare` | Prepare a certificate for a Comb that has not joined. | Yes |
| `cert-setup status` | Report the saved integration and the certificates held. | - |

Run `apiaryctl <command> --help` for the full flags of any one of them.

## Where to go next

| You need | Read |
| --- | --- |
| To bring up a node | [Create a new Apiary node](create-node.md) |
| To join a Colony | [Add a node to an existing Colony](add-node-to-colony.md) |
| To build a FreeBSD base image | [Minimal FreeBSD base](minimal-build.md) |
| To understand a design decision | `docs/adr/` |
| To see why the code is the way it is | The ADR index in `docs/adr/MIGRATION.md` |
