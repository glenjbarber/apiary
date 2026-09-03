# ADR-0046: VM table polling corrupted by an out-of-band swap sharing a table-row response

## Status

Accepted

## Context

Caught live by the user while checking the VM table in a real browser
(Safari) after the ADR-0045 milestone: a few seconds after `/vms` loads
correctly, the table's IP/MAC/Replica columns visually collapsed into
one run of text (`4e:50:cb:24:ab:afnone`) - every time, on every page,
tied exactly to the 3-second `hx-trigger="every 3s"` poll
(ADR-0016/ADR-0018) that refreshes the table body.

## Root cause

`vm_rows_fragment` (the response `/vms/rows` actually served) prepended
an out-of-band error banner ahead of the row markup:

```html
<div id="vm-error" class="banner-error" hx-swap-oob="true">{{.Error}}</div>{{template "vm_rows" .}}
```

htmx's own response parser (`makeFragment`) sniffs the *first* tag in
the response text via an unanchored regex to decide whether the content
needs wrapping in a synthetic `<table>` before parsing (so bare `<tr>`/
`<td>` fragments - the normal shape of an htmx table-row response -
parse correctly). Since the response here starts with `<div>`, not
`<tr>`, that sniff picks `"div"` and skips the table-wrapping entirely,
falling back to parsing the whole response as a plain document body.

Per the HTML5 parsing spec, a `<tr>`/`<td>` start tag encountered with no
table ancestor is a parse error and the tag itself is dropped - but its
*text content* is not. That's exactly the observed symptom: the row's
structure vanished while its text (the badge, the MAC, "none") rendered
as one undifferentiated run.

This is not a one-time discrepancy - the `<div>` is present in *every*
response from this endpoint, empty or not, so the bug fired on every
single 3-second poll, all the time, for any browser.

## A dead end worth recording: `useTemplateFragments`

htmx ships a `useTemplateFragments` config flag specifically meant for
this class of problem: when set, `makeFragment` parses the response by
stuffing it into a `<template>` element instead of tag-sniffing +
table-wrapping, and `<template>` content isn't subject to real-table
foster-parenting rules.

Enabling it was tried first and **did not fix the bug** - confirmed live
in this project's actual browser (Safari/WebKit): htmx's own
implementation of that branch parses via
`new DOMParser().parseFromString("<body><template>"+response+"</template></body>", "text/html")`
and pulls out `.querySelector("template").content`. That specific
construction - a `<template>` nested inside a synthetic `<body>`, parsed
as a whole document via `DOMParser` - still dropped the `<tr>`/`<td>`
tags in live testing here, even though a plain, already-connected
`document.createElement('template'); tpl.innerHTML = response` correctly
preserves them (verified directly, side by side, against the exact same
response text). This looks like a genuine WebKit quirk in how `<template>`
parsing is applied when reached via a synthetic full-document parse
rather than live DOM assignment - not something this project can fix,
and not something to rely on going forward.

## Fix

Never mix non-table content into a response that gets swapped into a
`<tbody>`. `vm_rows_fragment` is removed entirely; `/vms/rows` (and the
create/delete action handlers that re-render it) now always return rows
only (`vm_rows` alone - guaranteed to start with `<tr` or be empty),
regardless of whether an error occurred.

The error banner is delivered instead via an `HX-Trigger` response
header (`internal/frontend`'s new `renderVMRows` helper), which htmx
turns into a `vmError` CustomEvent dispatched on (and bubbling from)
whichever element issued the request - the polling `<tbody>` or a row's
Delete button. `vms.html` adds a small always-present listener at the
document level (`document.body.addEventListener("vmError", ...)`) that
writes the event's payload into the persistent `#vm-error` div - fully
decoupled from the row response's own body, so there is no longer any
content-type ambiguity for htmx's parser to get wrong.

## Verification

Two new tests in `internal/frontend/server_test.go`:
`TestServer_ListVMs_FetchErrorSetsHXTriggerNotBody`/
`TestServer_DeleteVM_ErrorSetsHXTriggerNotBody` (confirm the error
reaches an `HX-Trigger` header, never the body) and
`TestServer_ListVMs_RowsFragmentNeverLeadsWithNonRowContent` (the general
regression guard: whatever this endpoint renders, in any of the
no-VMs/error/has-VMs cases, must start with `<tr` or be empty - nothing
else is safe to lead a `<tbody>`-targeted response with).

Live-verified against a minimal, faithful standalone reproduction (the
real vendored `htmx.min.js`, the real `vm_rows.html` markup, and the
real CSS from `layout.html`, served from a throwaway Go binary and
driven through an actual browser): confirmed the original bug
reproduced exactly as described (columns collapsing after the first
poll), confirmed `useTemplateFragments` did not fix it, and confirmed
the final fix keeps the table's row structure fully intact across
repeated polls while a simulated error still correctly reaches the
banner via the `HX-Trigger` path - with no code path left that emits
non-`<tr>` content into this response.
