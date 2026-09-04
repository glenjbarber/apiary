# ADR-0059: Hierarchical sidebar application shell

## Status

Accepted

## Context

Apiary's horizontal navigation grew from a few primary pages into a mixture of
Colony resources, networking tools, operational evidence, recovery tools,
media, and account administration. The resulting header consumed increasing
width, forced secondary pages into a dropdown, and previously caused both
stacking-order and horizontal-overflow defects.

The established product vocabulary now provides a useful hierarchy:
`Apiary > Colony > Hive > Comb > Cell`. The interface should make that model
visible without pretending that Comb is a new persisted API resource or
renaming existing VM and jail endpoints.

## Decision

Replace the horizontal navigation and Manage dropdown with a shared responsive
application shell:

- a compact header contains the Apiary mark, signed-in user and role, serving
  Hive, color-theme control, and settings menu;
- a persistent desktop sidebar groups existing routes under Colony, Network,
  Status, and Media;
- the Colony group displays Hives, Combs, and Cells as a hierarchy while the
  existing VM and jail pages remain the concrete Cell-type destinations;
- Machine, Users, API keys, and Log out move to the permission-aware settings
  menu;
- page content receives its own bounded, minimum-width-zero grid region so
  wide tables scroll locally instead of widening the entire application; and
- the sidebar becomes an accessible, dismissible drawer below 900 pixels.

Every full page uses the same main-content landmark and footer. Active links
carry `aria-current="page"`, a keyboard-visible skip link bypasses navigation,
and the mobile drawer can be closed by selecting a destination or pressing
Escape. The unauthenticated login remains a focused standalone surface, but
uses the same mark, palette, and responsive styling.

The theme control selects light or dark CSS variables and stores only that
preference in browser local storage. With no saved preference, Apiary follows
the operating-system color scheme. No preference enters raft or server-side
state.

## Consequences

- Primary navigation no longer competes with account information for a single
  horizontal row or depends on an overlapping dropdown.
- The product hierarchy becomes visible while protocol names, protobufs,
  routes, and persisted resource types remain unchanged.
- Settings remain permission-aware using the same template fields and
  server-side role checks as before.
- Wide operational pages retain more usable width because the content column
  can shrink independently and the sidebar can collapse on small screens.
- The small amount of plain JavaScript is limited to theme persistence and
  mobile-sidebar interaction. HTMX behavior and the server-rendered page model
  remain unchanged.
