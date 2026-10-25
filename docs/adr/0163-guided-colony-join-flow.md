# ADR-0163: A guided Colony-join flow that shows both sides of the handshake

## Status

**Proposed.** This record designs the operator-facing guided join flow for
issue #79 and is the authority for what that flow is allowed to do. It is
**not yet implemented** in the tree this ADR lands beside: the flow described
below under "Decision" is implemented, and the "Accepted but not built" note
records what of it is not.

Supersedes: None
Superseded by: None
Tracking issue: #79
Amends ADR-0147 (Part 2's "The two pages" section, which this ADR replaces).

## Context

ADR-0147 Part 2 built the two-way, peer-authorized Colony join, and its
backend is complete: the joiner generates a first code and advertises its own
certificate fingerprints, the target's Admin pastes both in, the target
generates a second PIN that is released only to the requesting Comb, and the
requester pastes that back. The fingerprint comparison is fail-closed, the
second PIN is never rendered on the target's page, the join window is
Colony-wide and replicated, and every admin-side call forwards to the leader.

What an operator still does not have is a way to see where they are. Joining
two Combs to a single-node Colony took roughly two hours and dozens of manual
steps across three hosts: the operator ran commands on the joining Comb, the
leader and a follower, copied codes and fingerprints between them by hand,
raced several short timers, and read refusals that named the wrong Comb. The
web UI gave no feedback for some successful steps, and the order of
operations was not visible anywhere.

Issue #79 asks for a guided, step-by-step flow in the style of the VM and
jail creation flows: it states which Comb each action happens on, shows the
current step and what is recorded so far, and carries values between steps
instead of asking the operator to copy them.

### The tension this ADR has to resolve

ADR-0147 Part 2's "The two pages" section specified a four-step
"Requesting Comb's own Machine page" driven by a `GetLocalJoinIdentity` RPC,
showing the first code with a copy button, the fingerprints with a copy
button, the stage, and the second PIN when it exists. That RPC was never
implemented. The ADR's own status line, and the header comment of
`internal/manager/joinintroduction.go`, record why:

> THE JOINER SIDE IS apiaryctl, NOT A PAGE. The ADR's own sketch had a
> four-step "Requesting Comb's own Machine page" driving
> GetLocalJoinIdentity. That Comb is a headless FreeBSD server with no
> browser, so the page does not exist and this change does not build it.

Two objections were recorded against building the page, and both are real:

1. **A joining Comb is headless.** It is a FreeBSD server with no browser,
   so the page it was supposed to render on is not reachable by the operator
   who needs it.
2. **A page that submits would destroy the handshake.** The first code's
   entire purpose is to attest that a person read it off the requesting
   Comb's own screen. A page that generated the code and submitted it to
   the target would satisfy stage one by itself, which is exactly the
   attestation stage one exists to demand from a human.

So the flow cannot be "the joiner-side page ADR-0147 drew", and it cannot be
"a page that automates the handshake" either. What it has to be is decided
below.

### What the flow is not allowed to become

- Not a substitute for the human comparison. It never submits a first code,
  a fingerprint set, or a second PIN to the target on the operator's behalf.
- Not a holder of secrets. It never stores a first code, a fingerprint set,
  or a second PIN in replicated state, in a browser cookie, or in a URL that
  another operator could read.
- Not a second authorization path. It reads state that already exists and
  renders it; it never approves, verifies, or issues anything.

## Decision

The guided join flow is a **two-column, read-only join ledger** at
`/machine/join`, rendered on the **target Colony's** control host and
readable by that host's Admin.

Each column is one side of the handshake. The left column is the JOINING
Comb's steps; the right column is the TARGET Colony's steps. Every step
carries three things and nothing else: which Comb the action happens on, the
action itself in the imperative, and the value that action produces or
consumes.

A step's state is one of: **done** (the value the step produces or consumes
is recorded, and the ledger can say so from replicated state), **waiting**
(the step is the next one and nothing has happened yet), **blocked** (a
precondition is unmet — the join window is closed, the request was rejected,
the second PIN expired), or **unknown** (this page cannot tell, and the
ledger says so rather than guessing).

### The values the ledger shows, and the ones it does not

The ledger shows exactly the values that ADR-0147 already treats as
non-secret at the point they are displayed, and it shows them **from the
side that owns them**:

- The **first code** and the **fingerprints** appear on the JOINING column,
  because they are the joining Comb's own values and the joining Comb's
  operator is the one who reads them. On the TARGET side they appear only
  as the *advertised* values the target's operator compares a paste
  against — which is what the target's page already renders today, and
  which is not a secret at the target, since the request carried them.
- The **second PIN** never appears on the TARGET column, and never appears
  anywhere on the target's page. ADR-0147 is explicit that a target Admin
  who can read it can complete the handshake alone, which collapses the
  handshake back into one party. The ledger does not weaken that.

The ledger therefore never *moves* a value between the two columns. It
carries the **operator's place in the procedure**, not the operator's
secrets. That is what makes "carries values between steps" true in the sense
that matters here and false in the sense that would break the design.

### Where the ledger's state comes from

Every state the ledger renders comes from state that already exists and is
already replicated:

- The **join window's** live state, from `GetColonyJoinWindow`.
- The **pending request's** stage, status, attempt counters and second-PIN
  expiry, from `ListJoinRequests`.
- The **request's own** advertised fingerprints, from the same call.

The ledger adds **no new RPC, no new FSM command, and no new replicated
state.** This is deliberate: it is the same discipline ADR-0147 applied to
its own "the window is a toggle, not a capability" decision, and it is what
keeps the flow from becoming a second thing that can disagree with the join
flow it is meant to explain.

### The joiner-side column, and the headless objection

The objection that a joining Comb is headless is real, and this ADR does
not pretend otherwise. The JOINING column is rendered from **the target's**
view of the request, so it is the target's operator reading it. It tells
that operator what the joining Comb's operator must do, and what the
joining Comb's operator will see when they do it.

The joining Comb's own operator still needs their own view. That view is
`apiaryctl join-introduce`, which prints the first code, the fingerprints
and the second PIN as bare values for pasting, and which prints nothing
else. This ADR does not change that command's contract. What it adds is
that the target's ledger names the exact command and the exact field, so
the joining Comb's operator is told what to run and the target's operator
is told what to expect.

That is the deliberate trade: **one page that guides the operator who is
already at a browser, and a CLI that serves the operator who is not.** It is
not the page ADR-0147 drew, because a page on a headless Comb is a page
nobody can read.

### The steps, in order

The target's operator, on the target's own page, works through this list.
The ledger marks each step done as the state proves it.

1. **Target: open the join window.** A Colony refuses join requests while
   its window is closed, and that is the normal state. Until the window is
   open, every later step is blocked and the ledger says so.
2. **Joiner: run `apiaryctl join-introduce --target HOST:PORT`.** The
   joiner generates its own first code and reads its own certificate
   fingerprints. This step is done when the target has a pending request
   from this Comb.
3. **Target: paste the first code and the fingerprints into the request's
   Stage 1 form.** This is the human comparison stage one exists for. The
   ledger marks it done when the request's stage advances past `introduced`.
4. **Joiner: read the second PIN** with
   `apiaryctl join-introduce --request-id ID --first-code CODE --field second-pin`.
   The ledger marks it done when the target's request shows a second PIN has
   been issued. The ledger never shows the PIN's value.
5. **Target: paste the second PIN into the Approve form and approve.** The
   ledger marks it done when the request's status is `approved`.
6. **Joiner: install the credentials.** The target returns a sealed
   credential bundle with the approval; the joiner decrypts and installs it
   with `apiaryctl join-introduce --request-id ID --first-code CODE --install-credentials`.
   This installs the joiner's own API key and the Colony's restart-guardrail
   token, and restarts the services that need them.
7. **Joiner: become a member.** The joining Comb is already listening with
   `-await-join` and joins automatically once it is a voter. If the joiner
   was an already-bootstrapped standalone Comb, it is converted first by
   the existing "Convert this Comb to a joiner" action.

### Timers

Every deadline in the flow is shown with its remaining time and the fact
that it is renewable:

- The **join window** shows its expiry and can be closed or reopened. It
  is Colony-wide and is opened on any member.
- The **second PIN** shows its own expiry and that it may be reissued, up
  to the two reissues ADR-0147 already permits. A spent PIN is not
  reusable, and the ledger says that rather than leaving the operator to
  discover it.
- The **request** itself shows the attempt counters, so an operator about
  to use the last of five wrong first codes knows it is the last.

### Leadership change does not break it

The ledger reads replicated state, and every admin-side mutation already
forwards to the leader (ADR-0147's "Leader forwarding"). A leader change
mid-flow therefore does not break the ledger: it re-reads the same
replicated state from whichever member is now the leader, and a step that
was done stays done because "done" is a property of replicated state and
not of a session on a particular node.

This is the reason the ledger is read-only. A flow that held a step's
progress in server-side session state would lose it on a leader change, and
a flow that held it in the browser would lose it on a page reload. The
ledger has no state to lose.

### The two things this flow deliberately does not do

- It does **not** create the authorization entry that ADR-0147 Part 3
  requires, and it does not list one. ADR-0147 is explicit that the panel
  served by a Comb a hostile Admin controls must never render the entry
  list or offer a way to create an entry; it may only say *that* a gate is
  unmet and what would satisfy it. The ledger obeys that.
- It does **not** perform the second-API-key gate, for the same reason.

## Consequences

- The guided join flow is one page, read-only, over existing RPCs, with no
  new replicated state and no new authorization surface. The attack surface
  it adds is a renderer, not a mechanism.
- The flow is honest about the two-host nature of the handshake. It cannot
  be completed from one screen, because the handshake cannot be, and a flow
  that pretended otherwise would be the "prototype wizard that marks itself
  complete without approving a join" that ADR-0147 removed.
- `apiaryctl join-introduce` keeps its contract, including its refusal to
  submit anything to the target. This ADR narrows what an operator has to
  remember, not what the system will do.
- The joining Comb still needs a terminal. That is a property of joining a
  headless server, not a gap in this flow, and the ledger says which
  command to run rather than pretending the browser is enough.
- Issue #75 — the first-API-key lockout — is **not** solved by this flow.
  The credential bundle delivered at step 6 already installs the joiner's
  own key; what #75 describes is the *first* key on a Colony that already
  exists, which is a different problem with a different trigger. This ADR
  records that it is out of scope here so it is not mistaken for solved.

## Alternatives considered

- **Restore ADR-0147's four-step joiner page, with `GetLocalJoinIdentity`.**
  Rejected: the page would render on a headless Comb, so the operator who
  needs it cannot read it, and a page that can also submit would satisfy
  stage one without a human. It would be a page that looks like the flow
  and is not.
- **A guided flow that automates the handshake end to end.** Rejected: it
  is exactly the collapse ADR-0147's second PIN exists to prevent. A
  target Admin who could complete the join alone is a target Admin who
  never proved anything about the machine they were admitting.
- **A CLI-driven guided flow instead of a page.** Rejected as the primary
  surface: the operators who struggle with this flow are working in a
  browser against a target they can see, and the target's own page is
  where the Stage 1 and Stage 2 forms already are. The CLI remains, for the
  joining side, and this ADR keeps it.
