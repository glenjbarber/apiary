# ADR-0167: The frontend's API key is resolved live, not captured at startup

## Status

**Proposed.** This record decides how a frontend learns the API key it
presents to its own managerd, and it closes the last of issue #75's lockout.
It amends ADR-0023's consequence that a frontend "must be restarted" once a
key exists.

Supersedes: None
Superseded by: None
Tracking issue: #75
Amends ADR-0023 (the first-key lockout and the restart it required).

## Context

### What already works

The API Keys page does the file half of this already. When an operator
creates the first key, `internal/frontend`'s `provisionSelfAfterFirstKey`
installs the new key into this Comb's own two credential slots -
managerd's `peer_api_key` and the frontend's own `manager_api_key` -
through the `UpdateNodeConfig` and `UpdateFrontendConfig` RPCs, the
moment the key exists. It writes them with the brand-new key as the
credential for those two calls alone, because by then the live
connection carries none.

The other two populations are covered too: a Comb that JOINS later gets
its own key in the sealed credential bundle the join flow delivers, and
an EXISTING member gets one from the root-run
`apiaryctl peer-credential-backfill`.

### What does not work, and it is the part that matters

Writing the file is only half of it.

`cmd/frontend` reads `manager_api_key` **once, at startup**, and attaches
it to its gRPC connection as a per-RPC credential. The credential was a
`string` captured at that moment and could not change while the process
ran.

So the sequence an operator actually experiences was:

1. The page creates the first key.
2. The FSM flips `auth_enabled` on - permanently, colony-wide, by ADR-0023's design.
3. The page writes the key into `frontend.json`.
4. **Every subsequent call from that frontend fails `Unauthenticated`.**

Step 3 does not fix step 4. The file now holds a key the running
frontend is not using. The operator has to restart the frontend by hand
- and on every other Comb, whose key was never written at all.

That is the lockout, and it is why #75 says "every frontend that has no
`manager_api_key` ... then fails with Unauthenticated, **including the
page that created the key**".

The same shape appeared in the backfill command, which restarted
`apiary_frontend` on every member of a multi-Comb Colony for the same
reason.

## Decision

**The frontend's API key is resolved at call time from
`frontend.json`, rather than captured at startup.**

`cmd/frontend` attaches a credential that re-reads the file and presents
whatever it currently holds. gRPC calls `PerRPCCredentials.GetRequestMetadata`
once per RPC, so a credential that reads live state picks up a key
written moments earlier with no restart at all.

### Why the file, and not a notification

- **The key is already in the file.** The API Keys page wrote it there.
  Re-reading it reads the authoritative value rather than
  reimplementing it.

- **A frontend can also be given a key by other means** - an operator
  editing `frontend.json` directly, or the join flow installing one. A
  file-backed credential picks those up too, with no second mechanism to
  keep in step.

- **There is no signal to notify on.** The page that writes the file is
  a different process from the frontend that needs to hear about it, and
  a poll of a file that already holds the answer needs no protocol
  between them.

### Why a short cache rather than a read per RPC

The key changes about once in a Comb's lifetime, and a file read on
every request is a real cost on a frontend serving a whole Colony's UI.
The read is cached for a second.

A second is chosen because the window in which it matters is exactly the
window an operator is staring at a broken page: the key was just
created, and the frontend is failing every call. A second of staleness
there is invisible; a minute of it is a page that looks broken for a
minute after it was fixed.

### What the credential presents when there is no key

No credential at all - an empty metadata map, not an empty
`authorization` header and not an error.

That is deliberate, and it is the same distinction managerd's own
interceptor makes: an unauthenticated call is ALLOWED while auth is not
yet enabled and REFUSED once it is. Returning an error from the
credential instead would fail every call locally, with a message about
the frontend's own configuration rather than about authentication - and
would break a frontend that was working perfectly well before the first
key existed.

### A read failure keeps the working key

If the file cannot be read - a permission change, a filesystem blip -
the last known key stays in effect. Stripping a working credential
because of a transient read error would lock the operator out of a
frontend that was working a moment ago, which is the opposite of what
this change is for.

### The consequences elsewhere

- **`apiaryctl peer-credential-backfill` no longer restarts the
  frontend.** managerd still needs its restart - it reads `peer_api_key`
  once, at startup - but the frontend does not, so the backfill takes
  one service down instead of two on every member of the Colony.

- **The API Keys page's warning names the file paths**, not just the
  field names, because the issue asks it to say "which files to change"
  and a warning that names the fields but not the paths leaves the
  operator guessing where the files are.

## Consequences

- The lockout is closed. The Comb that creates the first key is
  provisioned and usable immediately, with no restart.

- The backfill is lighter on a multi-Comb Colony: it no longer takes
  each member's UI down.

- A frontend reads a small file at most once a second. That is not
  free, and it is bounded.

- The key is now read from a file that an operator can edit, which
  means a hand-edited key takes effect without a restart too. That is
  the same property the API Keys page already relies on when it writes
  the file, and it is a feature: the file is the source of truth.

- The frontend's credential is no longer fixed at startup, so
  "restart the frontend" stops being the way an operator applies a key
  change. Nothing else depended on that.

## Alternatives considered

- **Have the API Keys page restart the frontend.** Rejected: it is a
  different process on a different Comb in the general case, the
  frontend may be several Combs away, and restarting a UI to apply a
  credential is a blunt instrument where a re-read is exact.

- **Push the key to the frontend over an RPC.** Rejected: it adds a
  surface for delivering a credential to a process, and the process
  already has the credential in a file it can read.

- **Read the file on every RPC, with no cache.** Rejected: a file read
  per request is a needless cost for a value that changes about once in
  a Comb's lifetime.

- **Leave it alone, and tell operators to restart the frontend.** That
  is the status quo, and it is the bug.
