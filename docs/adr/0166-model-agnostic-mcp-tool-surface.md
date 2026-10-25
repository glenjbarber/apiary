# ADR-0166: A model-agnostic MCP tool surface

## Status

**Proposed.** This record designs the model-agnostic property of the
Apiary MCP server for issue #26, and amends ADR-0162's boundary
decision with respect to what "model agnostic" means for the tool
surface. It is **not yet implemented** in the tree this ADR lands beside;
the implementation is in the same change as this record.

Supersedes: None
Superseded by: None
Tracking issue: #26
Amends ADR-0162 (the MCP server's tool surface and its boundary).

## Context

ADR-0162 recorded the Apiary MCP server's boundary: a read-only server
over an explicit allowlist of five read RPCs, three tools, authorization
enforced by managerd's own role map, bounded output summaries, and no
guarded writes until a per-operation authorization and approval policy
exists. It also recorded that the boundary for basic read-only
external-client access "remains unresolved", and that deployment and
client interoperability are not verified.

Glen's requirement for issue #26 is that the MCP integration be **model
agnostic**. That word needs a precise meaning before it can be
implemented, because several plausible readings are different projects:

- "It must not require a particular model PROVIDER." True of almost any
  MCP server, and almost nothing to build.
- "It must work with a small local model as well as a large hosted one."
  Real, but it is a property of how much the tool surface DEMANDS of a
  model, not of which vendor serves it.
- "It must not contain anything that encodes a particular model's habits
  or a particular vendor's tool-call format." This is the reading this
  ADR takes, because it is the one that is testable.

The thing that makes a tool surface model-specific, in practice, is
almost never the choice of backend. It is the surface's own assumptions:
descriptions that only make sense to a model that already knows the
domain, parameters named the way one model family prefers, output shapes
that require the reader to already know what an unlabelled position
means, and results whose completeness cannot be told from the result
itself.

### What the surface already does, and what it does not

`cmd/apiary-mcp` registers three tools - `list_cells`, `get_cell` and
`get_cluster_health` - over `ListVMs`, `ListJails`, `GetVM`, `GetJail`
and `ClusterHealth`, supports stdio and Streamable HTTP, and enforces
authorization through managerd. That much is right, and none of it is
model-specific.

What it does not do, as it stood:

- **It publishes no annotations.** MCP's tool annotations are the
  protocol's own machine-readable statement of what a tool does. A
  client - or a host that gates tool use - has no way to learn that
  these tools are read-only except by reading prose.
- **Its descriptions are labels, not contracts.** "List Apiary VMs and
  jails. Returns only identity and lifecycle summary fields." says what
  the tool touches and nothing about what its result means, what a
  truncated list means, or what a failure means. Every model fills those
  gaps differently, or wrongly.
- **Its server sends no instructions.** The conventions the tools share
  - that a cell is a VM or a jail, that phase and desired_state differ,
  that a tool error is not a "not found" - were implicit in the tool
  bodies and are now nowhere a model can read.
- **Its argument documentation was a field name.** `jsonschema:"Apiary
  cell ID"` does not say that a cell's ID is unique only within its type,
  which is the fact a model needs to call the tool correctly.
- **Its results could not be told from a partial result.** `list_cells`
  carried a `truncated` flag but `get_cluster_health` did not, and a
  truncated list that does not say so is indistinguishable from a
  complete one.
- **It could report a verdict it did not understand as a real one.**

## Decision

The MCP tool surface is **model agnostic** in the sense that it makes no
assumption about the model behind the client. Concretely, four things
carry that property, and this ADR owns all four.

### 1. The annotations

Every tool declares `ReadOnlyHint: true` and `OpenWorldHint: true`, and
carries a plain human-readable `Title`.

`ReadOnlyHint` is the machine-readable statement that these tools cannot
mutate anything. It is what lets a host gate tool use without parsing
prose, and it is the part of the surface that is genuinely
model-independent rather than model-friendly.

`OpenWorldHint` is true because the tools read a LIVE cluster whose
state changes under the caller: a cell listed a moment ago may be gone
now, and a Comb's health may have changed. A client that wants to cache
these results needs to know that it may not.

`DestructiveHint` and `IdempotentHint` are left at their defaults,
because they are only meaningful for a tool that is not read-only, and
setting them on a read-only tool would be a claim about behaviour that
does not apply.

### 2. The descriptions are contracts

Each description says, in the tool's own words: what the tool returns
and what every result field means; what its arguments identify and what
happens when they are wrong; what the tool does NOT do; and what a
failure means as distinct from a negative answer.

The last of those is the one that most affects behaviour. A tool error
means the call could not be answered - it does not mean the thing asked
about does not exist, and it does not mean it does. "Not found" is
reported as a failed call with a "not found" message, and the
descriptions say so, because a model that conflates "the call failed"
with "the thing is not there" will report a backend outage as "no such
VM".

### 3. The server sends instructions

MCP delivers server-level instructions to the client on connect, and
they are read by the model as part of the tool context. They carry the
conventions every tool depends on, so the tool descriptions themselves
stay short and factual and do not repeat themselves.

They state the vocabulary ("cell" means a VM or a jail; "node" means a
Comb), the result conventions (read named fields; a truncated
collection says so; phase and desired_state differ; a tool error is not
a "not found"), and the authorization failure's meaning.

They are deliberately model-neutral: they do not tell the model how to
think, how to phrase things, or what the user wants. They state facts
about the Apiary side that the model cannot infer.

### 4. The results are self-describing

- Every collection that can be truncated carries a boolean saying
  whether it was. Both `list_cells` and `get_cluster_health` now do;
  `get_cluster_health` did not.
- Every field that can hold one of a known set of values says so, in
  the description of the tool that returns it.
- A verdict this connector does not recognize is reported as unknown,
  never as a known verdict.

That last one is a CORRECTNESS fix as much as a model-agnostic one, and
it is the clearest illustration of why the two are the same thing.
`api/rpc/manager.proto` says of the health status field:

> status is one of "healthy", "degraded", "unknown", "stale", or
> "contradictory" - or any other string if a newer managerd computes a
> state this consumer has never heard of. Treat an unrecognized status
> as unknown, never as healthy: that is the whole failure mode
> Evidence-Aware Health exists to prevent, and the string contract
> cannot enforce it on the consumer's behalf.

The connector is that consumer. The previous implementation reported
`unknown` only for the literal string `"unknown"`, so a status from a
newer managerd - say `"quiescing"` - was reported to the model as a
recognized verdict. The model behind it has no way to know which strings
are new; it cannot be asked. So the connector enumerates the vocabulary
it knows and reports everything else as unknown, while still carrying the
raw string so a caller that does know about it can see it.

Note that managerd's own `"unknown"` is not in the known set either. It
is managerd's name for the ABSENCE of a verdict, not a verdict of
anything, so it is reported as unknown as well.

### What this does not do

- It does not add a tool per model, a vendor adapter, or a
  prompt-engineering layer. Those are precisely what would make a
  surface model-specific. The tool set stays three.
- It does not add a write tool. ADR-0162's guarded-write phase still
  requires a separate per-operation authorization, approval and audit
  design, and nothing here changes that.
- It does not change the allowlist, the transport, the authorization
  model, or the deployment story. Those are ADR-0162's, and they are
  untouched.
- It does not claim deployment or client interoperability has been
  verified. ADR-0162's record of that remains true.

## Consequences

- The tool surface describes itself. A model that has never seen Apiary
  gets the same information a model that has, because the information is
  in the tool descriptions and the server instructions rather than in a
  conversation.
- A host can gate these tools without reading prose: the protocol
  carries the read-only statement.
- A model can no longer mistake a truncated list for a complete one, or
  a call failure for a negative answer, or an unrecognized health
  verdict for a real one. Each of those was a way for the surface to
  tell a model something untrue.
- The tool set is unchanged in size, which is the point: a model-agnostic
  surface is a small set of tools that describe themselves properly, not
  a large set of specialized ones.
- The vocabulary of health verdicts is now enumerated in code, so a
  backend that adds a status without updating the connector degrades
  safely - the new status is reported as unknown rather than silently
  as a real verdict. Updating the connector is then a deliberate act.

## Alternatives considered

- **One tool per model family.** Rejected: this is the opposite of
  model agnostic. It makes the surface's shape a function of which
  vendors it is trying to serve, and it multiplies the surface every
  time a new model family appears.
- **A prompt-engineering layer, or a system prompt shipped to the
  client.** Rejected: a prompt is the most model-specific artifact
  there is, and it is invisible to a client that does not ask for it.
  The same information in the tool descriptions and the server
  instructions reaches a model through every client, including one that
  renders it as plain text.
- **An unstructured "ask Apiary anything" tool.** Rejected: it hides the
  surface's semantics inside a parameter, so the model has to infer the
  vocabulary from prose at call time, and it defeats the bounded-output
  and explicit-allowlist guarantees ADR-0162 established.
- **Enumerate the health vocabulary in the backend instead.** Rejected as
  the primary fix: the backend may be NEWER than the connector, and the
  contract explicitly says it cannot enforce this on the consumer's
  behalf. The connector still enumerates; the backend's own string set
  remains authoritative for what it returns.
