package main

// The MODEL-AGNOSTIC part of this connector (issue #26, Glen's
// "make it model agnostic" requirement).
//
// What "model agnostic" means here, precisely: **the tool surface does
// not assume anything about the model sitting behind the MCP client.** A
// tool works the same for a small local model, a large hosted one, a
// deterministic rule engine driving the tools directly, or a human
// reading the JSON by hand. Nothing in the surface encodes a particular
// vendor's tool-call format, a particular prompt strategy, or a
// particular model's parameter habits.
//
// That is a property of FOUR things, and this file owns all four:
//
//  1. THE ANNOTATIONS. MCP's tool annotations are the protocol's own
//     machine-readable statement of what a tool does. Setting
//     ReadOnlyHint on every tool is what lets a client - or a host that
//     gates tool use - know these tools cannot mutate anything, without
//     inferring it from a prose description.
//
//  2. THE DESCRIPTIONS. A description is the only thing every model
//     reads, so it has to carry the semantics on its own: what the tool
//     returns, what its arguments mean, what it does NOT do, and what
//     the caller should do next. A description that only says "List Apiary
//     VMs and jails" leaves every model to guess the rest.
//
//  3. THE INPUT SCHEMAS. Argument names and descriptions are read by
//     the model and are the model's only guide to what a tool wants.
//     Each parameter says what it identifies and what happens if it is
//     wrong.
//
//  4. THE OUTPUT SHAPES. A model reads the RESULT, so the result's
//     shape has to be self-describing rather than positional. Every
//     output carries named fields, every enum-like field carries its
//     allowed values in its own documentation, and every collection
//     carries an explicit "was this truncated" flag - because a
//     truncated list that does not say so is indistinguishable from a
//     complete one, and a model that cannot tell will state a falsehood
//     to the user.
//
// What this file does NOT do is add a tool per model, a vendor-specific
// adapter, or a prompt-engineering layer. Those are exactly the things
// that make a surface model-specific. The tools stay three, and they get
// better at describing themselves.

import "github.com/modelcontextprotocol/go-sdk/mcp"

// serverInstructions is the server-level guidance MCP delivers to a
// client on connect.
//
// It is written for a MODEL, not for a human reading documentation, and
// that is the whole point: it says the things every model needs to know
// that are not specific to any one tool, so the tools themselves can
// stay short and factual.
//
// It is also deliberately model-neutral. It does not tell the model how
// to think, how to phrase things, or what the user wants - it states
// facts about the APIARY side that the model cannot infer.
const serverInstructions = `This server exposes a read-only view of an Apiary cluster.

Every tool here is READ-ONLY and is safe to call at any time: none of
them changes Apiary state, and none of them accepts a mutation.

TERMS
  "cell" is one Apiary resource: either a VM or a jail. Both are
  addressed by the same (type, id) pair.
  "node" is one Apiary Comb - one host in the cluster.

CONVENTIONS
  - A tool's result is a JSON object. Read the named fields; do not
    assume positions.
  - A collection field that can be truncated is accompanied by a
    boolean field saying whether it was. If that boolean is true, the
    list is NOT the whole picture and a caller that needs completeness
    must say so rather than presenting a partial list as complete.
  - "phase" is where a cell has actually GOT to. "desired_state" is
    where it has been ASKED to get to. A cell whose desired state has
    not been reached is not failing; it is still working towards it.
  - A tool error means the CALL could not be answered. It does not mean
    the thing asked about does not exist, and it does not mean it does.
    "not found" is reported as a successful call with an error message,
    never as a transport or authorization failure.
  - If a call is denied, the API key lacks the Viewer role. That is a
    configuration problem on the Apiary side and no retry will fix it.`

// readOnlyAnnotations are the annotations every tool in this connector
// carries.
//
// Every tool here is read-only, so ReadOnlyHint is true on all of them.
// OpenWorldHint is true because the tool reads a LIVE cluster whose
// state changes under the caller - a cell listed a moment ago may be
// gone now. DestructiveHint and IdempotentHint are left at their
// defaults because they are only meaningful for a tool that is not
// read-only, and setting them on a read-only tool would be a claim
// about behaviour that does not apply.
//
// The Title is a human-readable label for a client UI. It is here so a
// client that displays tool names as-is shows something a person can
// read, and it is deliberately plain rather than branded.
func readOnlyAnnotations(title string) *mcp.ToolAnnotations {
	openWorld := true
	return &mcp.ToolAnnotations{
		Title:         title,
		ReadOnlyHint:  true,
		OpenWorldHint: &openWorld,
	}
}

// The tool descriptions.
//
// A description is the ONLY thing every model reads, and it is read
// before anything else. So a description has to carry the semantics on
// its own: what the tool returns, what its arguments mean, what it does
// NOT do, and what the caller should do with a failure.
//
// They are written to be read by a model that has never seen Apiary, and
// to be equally useful to a model that has. Nothing in them assumes a
// particular prompt strategy, a particular vendor's tool-call format, or
// a particular model's habits.

// listCellsDescription describes list_cells.
const listCellsDescription = `List every Apiary VM and jail in the cluster, as identity and lifecycle summaries.

Returns a "cells" array. Each cell has:
  id            - the cell's ID, unique within its type
  name          - the operator's name for it, if it has one
  type          - "vm" or "jail"
  node_id       - the Comb it runs on
  desired_state - where it has been ASKED to get to
  phase         - where it has actually GOT to

Returns a "truncated" boolean alongside "cells". If it is true, the list
was cut off at 200 entries and is NOT the whole cluster - a caller that
needs completeness must say so rather than presenting a partial list as
complete.

A cell's phase and desired_state can differ: a cell whose phase has not
caught up with its desired state is still working towards it, not failing.

Reads the VM and jail inventories separately. If one of them cannot be
read, the whole call fails rather than returning a partial list that
looks complete.`

// getCellDescription describes get_cell.
const getCellDescription = `Read one Apiary VM or jail by its ID.

Arguments:
  id   - the cell's ID. Get it from list_cells.
  type - "vm" or "jail". A cell's ID is unique only WITHIN its type, so
         this is needed to identify one; passing a jail's ID with type
         "vm" finds nothing.

Returns the same fields list_cells returns for one cell.

If no cell of that type has that ID, the call FAILS with a "not found"
message. That is a different thing from the call being denied or the
backend being unavailable: "not found" means the answer is that the cell
does not exist.`

// getClusterHealthDescription describes get_cluster_health.
const getClusterHealthDescription = `Read the health of every Comb in the cluster.

Returns:
  local_node_id  - the Comb this connector is talking to
  unavailable    - true when Apiary could not produce a health verdict
  nodes          - one entry per Comb, each with:
      node_id  - the Comb's ID
      status   - the health verdict
      dialed   - whether the Comb answered at all
      unknown  - true when the verdict is UNKNOWN rather than a real
                 health answer

A node whose status is "unknown" has NOT been shown to be healthy. An
unreachable Comb and a Comb that was never reached look the same here,
and both mean the verdict is absent rather than good.

Detailed diagnostic text is deliberately omitted. This returns verdicts
and identifiers, not explanations.`

// The health verdict vocabulary.
//
// api/rpc/manager.proto names five statuses and says a NEWER managerd
// may return a string this consumer has never heard of, and that an
// unrecognized status must be treated as unknown and NEVER as healthy.
// It also says the string contract "cannot enforce it on the consumer's
// behalf" - which is precisely this connector's job, because this
// connector IS the consumer, and a model reading the JSON cannot be
// asked to know which strings are new.
//
// So the vocabulary is enumerated HERE, and anything outside it is
// unknown. The enumeration is a closed list on purpose: adding a status
// to the backend without adding it here is safe (the new status is
// reported as unknown rather than silently as healthy), whereas
// anything looser - a prefix match, a "does not look like an error"
// heuristic - would let a new status through as a real verdict.
//
// Note that "unknown" is NOT in this set, and that is deliberate.
// "unknown" is managerd's own name for the ABSENCE of a verdict - a
// Comb that was never reached, or whose health could not be
// established. It is a verdict string, but it is not a verdict of
// anything, and it must be reported as unknown.
//
// So the vocabulary here is the set of statuses that are a REAL verdict,
// and everything else - including "unknown" itself - is unknown.
var knownHealthStatuses = map[string]bool{
	"healthy":       true,
	"degraded":      true,
	"stale":         true,
	"contradictory": true,
}

// recognizedHealthStatus reports whether this connector knows the
// verdict string AS A REAL VERDICT.
//
// A status it does not know is NOT healthy and NOT degraded: it is
// unknown, because a verdict this connector cannot interpret is
// indistinguishable from no verdict at all. That is the failure mode
// Evidence-Aware Health exists to prevent, and this is the place it is
// prevented for an MCP client.
//
// managerd's own "unknown" is also not a real verdict - it is its name
// for the absence of one - so it is not in the known set either, and it
// is reported as unknown just like a status from a newer managerd.
//
// An EMPTY status is unknown for the same reason: a Comb that produced
// no verdict has produced no verdict, whatever it is called.
func recognizedHealthStatus(status string) bool {
	return knownHealthStatuses[status]
}
