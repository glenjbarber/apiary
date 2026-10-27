# ADR-0175: The certificate renewal lifecycle

## Status

**Proposed.** This record decides when a certificate is renewed, what
happens when the renewal succeeds, and what an operator sees when it
does not.

Supersedes: None
Superseded by: None
Tracking issue: #26 (the Cloudflare/Let's Encrypt bootstrap task)
Implements: ADR-0133 (the issuers), ADR-0160 (the monitoring surface),
ADR-0172 (the walkthrough), ADR-0174 (the provisioned integration)

## Context

Most of the renewal machinery already exists. `internal/origincert`
has a `Renewer` that checks both local certificate stores, renews
what is due, and joins the failures; `cmd/managerd` runs it on a
timer; and `internal/tlscache` re-reads a serving certificate on the
next TLS handshake when the file changes.

Two things are wrong with how it is wired.

**The renewal RESTARTS the services, and it does not need to.** The
`Renewer` calls `RestartService` after a successful renewal. But
`internal/tlscache` exists precisely so that a replaced certificate is
picked up on the next handshake with no restart - and all three of
`managerd`, the frontend and the `restshimd` load their serving
certificate through it.

So the renewal's restart is not merely unnecessary: it is disruptive.
Restarting the frontend drops the operator's session and the web UI
for a few seconds, on a schedule, for a renewal that had already taken
effect. The MANUAL issuance path already knows this - it says "No
restart to schedule or ask for any more (ADR-0133's hot-reload)". The
renewal path was simply never brought in line.

**The renewal schedule is a fixed interval, not the certificate's own
expiry.** A public CA decides the lifetime - Let's Encrypt issues
90-day certificates - so a renewal schedule that assumes a fixed
lifetime is either too late (and the certificate expires) or wasteful
(it renews a 90-day certificate every day).

## Decision

**A renewal does NOT restart anything.** The certificate is written,
the services pick it up on their next TLS handshake through
`internal/tlscache`, and nothing else happens. The manual path's
behaviour and the renewal path's behaviour become the same, and both
are correct.

**The renewal trigger is the certificate's own expiry, not a fixed
lifetime.** A certificate enters the renewal window when it is within
the renewal window of its own expiry - which `origincert.RenewalWindow`
already expresses as 30 days - and is renewed then. The
certificate's expiry is read from the certificate itself, so a CA that
issues 7-day certificates and one that issues 90-day certificates are
both handled correctly, with no configuration.

**A renewal is IDEMPOTENT and CONCURRENT-SAFE.** Two ticks racing to
renew the same certificate must not leave two certificates or a
half-written pair. The write is already atomic; what is added is a
per-name guard so a second renewal for the same name is a no-op
rather than a second issuance.

**A failed renewal leaves the CURRENT certificate serving.** The
previous pair is retained until the replacement has been validated and
installed. A provider outage therefore leaves a working certificate in
place, and the renewal is retried on the next tick.

**An EXPIRED certificate is a degraded state with a named cause, never
a silent one.** A certificate that has expired and cannot be renewed
is reported as such - to the operator, on the Certificates page, and in
the health of the Comb. It is never presented as healthy, and there is
no plaintext fallback: a Comb that cannot present a valid certificate
fails the handshake, which is the correct outcome.

**The renewal outcome is visible.** A renewal that succeeded, and one
that failed and why, are both recorded. The Certificates page shows
what each Comb holds and when it was last renewed, so "is my
certificate renewing?" is answerable without reading a log.

**The renewal is not a leader-only act.** Each Comb renews its OWN
certificates, from its OWN store, with its OWN integration. There is
no coordinator, no ownership generation to fence, and no work to
reassign: the renewal is local to the machine that serves the
certificate, and a Comb that is offline simply does not renew until
it comes back.

## Consequences

- A renewal no longer drops the operator's web session, and no longer
  takes the frontend down for a few seconds on a schedule.
- A public CA that issues short-lived certificates is handled without
  configuration, because the trigger is the certificate's own expiry.
- A provider outage leaves the current certificate serving, and the
  failure is visible rather than silent.
- An expired certificate that cannot be renewed shows as degraded, and
  the Comb's TLS fails rather than falling back to plaintext.
- There is no coordinator to fence, because the renewal is local.

## Alternatives and consequences

- **Restart the services after a renewal.** Rejected: `internal/tlscache`
  exists to avoid exactly this, and the manual issuance path already
  stopped doing it. The restart was a leftover from before the
  hot-reload existed.
- **A fixed renewal interval.** Rejected: it either expires a
  short-lived certificate or churns a long-lived one.
- **A coordinator that renews every Comb's certificates.** Rejected: it
  would need its own ownership and fencing, and a certificate is
  renewed by the machine that serves it.
- **Renew at a fraction of the certificate's lifetime.** Rejected: the
  certificate's own expiry is the fact, and it is already recorded.
