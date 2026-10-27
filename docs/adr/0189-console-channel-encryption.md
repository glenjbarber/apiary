# ADR-0189: The console channel is encrypted, or it is refused

## Status

**Accepted.** This record decides what may carry a VM console.

Supersedes: None
Superseded by: None
Affected projects: Apiary
Extends: ADR-0020 (the noVNC console), ADR-0065 (the cross-node
console tunnel)
Tracking issue: #26

## Context

### What the README said, and why it was wrong

The README disclosed this as a limitation:

> No VNC credentials/encryption on the underlying console connection
> itself — it relies entirely on the login gate in front of it.

That sentence is true of exactly one hop of a three-hop chain, and
saying it about "the console connection" invites the reader to think
the whole channel is unencrypted. It is worth being precise, because
the difference is the whole point of this record.

### The chain, hop by hop

```
browser --[1] WSS--> frontend --[2] gRPC/TLS--> owner managerd --[3] TCP/loopback--> bhyve fbuf (RFB)
```

**Hop 3 is the one the README was about.** The RFB protocol itself
carries no credentials and no encryption — that is `fbuf`'s own
limitation, and Apiary cannot fix it. But hop 3 **never leaves
loopback**: bhyve's framebuffer argument is `tcp=127.0.0.1:PORT`
(`internal/bhyve/manager.go`'s `vncFramebufferArg`), and
`managerd` always reports `Host` as loopback
(`internal/manager/server.go`'s `GetVMConsole`). So hop 3 is not on a
network at all.

**Hops 1 and 2 were the ones that could carry the console in
cleartext**, because both are optional:

- Hop 1 is the browser's WebSocket to the frontend. The frontend
  serves TLS when `tls_cert`/`tls_key` are configured, and serves
  plain HTTP otherwise. It was serving plain HTTP by default.
- Hop 2 is the frontend's gRPC to the owning managerd. `peer_tls`
  is tri-state: absent and `true` both mean TLS, but an explicit
  `false` turns it off, and nothing in the console path objected.

And what those hops carry is **a raw RFB stream: the guest's screen
and its keyboard, live**. It is not a status read and not a summary.
It is interactive control of the guest.

### The guards that already existed, and the one that was missing

The codebase already had the right rule, stated in the code and
applied to the peer API key and the restart-guardrail token:

> a credential must not cross the network in cleartext

`internal/manager/peer.go` refuses to forward either of those on a
plaintext peer dial (`errInsecureCredentialForward`). The console
tunnel was missed **because it attaches no credential** — it carries
bytes, not a token — so it never went through the check that would
have caught it.

## Decision

### The console is refused over plain HTTP, and refused over a plaintext peer dial

**Hop 1.** `internal/frontend`'s console WebSocket is refused with
`403` when the frontend is not serving TLS. The console *page* says
the same thing, on the page the operator is already looking at,
rather than letting them click into a console that cannot work.

**Hop 2.** `internal/manager`'s `OpenVMConsole` refuses to open a
tunnel when `peer_tls` is off, with the same remedy the other two
refusals name.

Both are refusals rather than warnings, and that is the important
part. The alternative is a console that **looks** protected — there is
a login gate in front of it, the URL is under the operator's session —
and is not. An operator who sees the console come up believes the
traffic is private. A console that is refused is one that cannot
mislead anyone.

### The refusal names the remedy

Both refusals name the same fix the existing credential refusals
name: turn `peer_tls` on, with a trust anchor in place, or configure
`tls_cert`/`tls_key` and restart. An operator who has already been
told how to fix the other two should not have to work it out again.

### What is deliberately NOT changed

**The RFB hop stays as it is.** Apiary does not implement RFB
authentication or encryption, and cannot: `fbuf` has neither. What
this record changes is that hop 3 is the *only* hop the console
crosses, so it is a loopback connection between two processes on one
host rather than a leg of a network path.

**No new protocol is introduced.** A TLS wrapper around the RFB
stream, or an RFB-authenticating proxy, would be a new thing to get
wrong, and hop 3 does not need it.

## What this ADR does not cover

- **The frontend's TLS is still optional for the rest of the UI.** This
  record requires it for the console specifically. The web UI's other
  pages are as protected - or not - as the operator's configuration
  makes them, and that is a separate question.
- **`peer_tls` remains tri-state.** This record refuses the console
  when it is off. It does not remove the option, because the option
  exists for a plaintext single-node deployment that has no other
  member to talk to.
- **The serial console log** is a different path - it is a file the
  reconciler writes, not a live stream - and is untouched.

## Consequences

- A VM console cannot cross a network in cleartext, on either hop
  that could.
- A Comb whose frontend is not serving TLS says so, plainly, instead
  of offering a console that would be readable on the wire.
- The README's limitation bullet is corrected: the RFB hop is
  unauthenticated and unencrypted, and it is loopback-only; the hops
  that are on a network are encrypted, and the console is refused
  rather than opened when they are not.

## Validation

- `internal/frontend`: the console WebSocket is refused with `403`
  over plain HTTP, and the refusal says the console would be in
  cleartext; the console *page* carries the same explanation and does
  not offer the noVNC widget; and the console still works over TLS.
- `internal/manager`: a console tunnel over a plaintext peer dial is
  refused, with or without a peer API key configured; and the tunnel
  still relays bytes over TLS.
