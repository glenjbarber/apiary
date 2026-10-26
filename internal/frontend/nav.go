package frontend

// The Blend navigation model (the "Apiary: UI Redesign" handoff's section
// 6).
//
// WHY A MODEL IN GO RATHER THAN MARKUP IN THE TEMPLATE.
//
// The handoff requires that EVERY existing route be reachable from a rail
// section and a tab, in two clicks or fewer, and that the rail's
// "something is wrong in this section" dot reflect what the server
// actually observed. Both of those are facts, not presentation: the dot
// needs the health verdicts, and the completeness of the route table is
// something a test can check. A hand-written nav template cannot be
// checked for either.
//
// So the structure lives here, and the template renders it. One
// definition means the rail, the tab strip, and the palette's "Go to"
// list are all generated from the same table and cannot disagree about
// where a page lives.
//
// TWO LEVELS, NEVER THREE. The handoff's own rule: the rail is the first
// level, the tab strip under it is the second, and there is no third
// level of navigation. The Comb section's tabs are the one case where
// the tab set is not a fixed list - it is one tab per known Comb,
// gathered per request - so it is carried separately rather than being
// baked into the static table.

// railSectionView is one section of the icon rail.
type railSectionView struct {
	// ID is the section's stable slug, used to match a page to its
	// section. It is the same value ActivePage carries for the pages
	// that live in a section.
	ID string
	// Label is the text under the icon. The handoff is explicit that
	// labels are always visible and an icon never appears without one.
	Label string
	// Icon names the line icon to draw. It is a key into the icon set
	// in layout.html's "rail_icon" template, not arbitrary markup, so a
	// new section cannot inject anything into the page.
	Icon string
	// Href is the section's landing page.
	Href string
	// Tabs are the section's second-level entries. Every tab is a real
	// page that exists today - the handoff forbids rendering a tab
	// that leads nowhere.
	Tabs []railTabView
	// Pinned pins the section to the bottom of the rail (Settings).
	Pinned bool
	// Active marks the section containing the current page.
	Active bool
	// Alert marks the section as having something warning, critical,
	// or stale in it.
	Alert bool
}

// railTabView is one tab in a section's tab strip.
type railTabView struct {
	Label  string
	Href   string
	Active bool
	// Dynamic marks a tab whose href is generated per request rather
	// than being a fixed route (a per-Comb tab). It is not a different
	// kind of tab to the operator - it is only a different kind to
	// this file.
	Dynamic bool
}

// navSection is the static definition of one rail section and its tabs.
//
// It is a function rather than a package-level var because the tab set
// depends on the ROLE: an Admin-only page (API keys) must not appear in
// a Viewer's tab strip at all, not merely be disabled there. Building it
// per request keeps that rule in one place instead of spreading
// role checks across every template that renders a tab.
//
// The Comb section's tabs are NOT here - they are one per known Comb,
// so they are added per request by navCombsTabs.
type navSection struct {
	ID    string
	Label string
	Icon  string
	Href  string
	// Admin marks a tab only an Admin may see.
	Admin bool
	// Operator marks a tab only an Operator (or better) may see.
	Operator bool
	// Pinned pins the section to the bottom of the rail.
	Pinned bool
	Tabs   []navTab
}

// navTab is one static tab.
type navTab struct {
	Label string
	Href  string
	// Admin/Operator gate a single tab without gating its whole
	// section - the Machine section's own link, for instance, is
	// Operator-only while the section itself is always present.
	Admin    bool
	Operator bool
	// Pages names the ActivePage values this tab covers. A tab is
	// active for a page even when the page is a sub-page of the tab's
	// own target - "/vms/{id}" is still the "Virtual machines" tab.
	//
	// A tab with several Pages is active when ANY of them matches, so
	// a detail page still highlights its section's tab.
	Pages []string
}

// navSections is the whole rail, in rail order.
//
// The order is the handoff's section 5 order exactly: Overview, Cells,
// Combs, Network, Storage, Images, Colony, with Settings pinned to the
// bottom.
//
// THE ROUTE TABLE IS COMPLETE. Every route in this codebase appears
// exactly once below, and nav_test.go asserts that: a route added to
// the frontend without a place in this table fails the build rather
// than becoming a page nothing links to.
var navSections = []navSection{
	{
		ID: "overview", Label: "Overview", Icon: "overview", Href: "/",
		Tabs: []navTab{
			{Label: "Health", Href: "/", Pages: []string{"stats", "host"}},
		},
	},
	{
		ID: "cells", Label: "Cells", Icon: "cells", Href: "/vms",
		Tabs: []navTab{
			{Label: "Virtual machines", Href: "/vms", Pages: []string{"vms"}},
			{Label: "Jails", Href: "/jails", Pages: []string{"jails"}},
			{Label: "Create", Href: "/create", Operator: true, Pages: []string{"create"}},
		},
	},
	{
		ID: "combs", Label: "Combs", Icon: "combs", Href: "/maintenance",
		Tabs: []navTab{
			// The per-Comb tabs are added per request, one per known
			// Comb - see buildNav.
			{Label: "Maintenance", Href: "/maintenance", Pages: []string{"maintenance"}},
		},
	},
	{
		ID: "network", Label: "Network", Icon: "network", Href: "/networks",
		Tabs: []navTab{
			{Label: "Networks", Href: "/networks", Pages: []string{"networks"}},
			{Label: "Map", Href: "/topology", Pages: []string{"topology"}},
			{Label: "Path trace", Href: "/trace", Pages: []string{"trace"}},
		},
	},
	{
		ID: "storage", Label: "Storage", Icon: "storage", Href: "/replication",
		Tabs: []navTab{
			{Label: "Replication", Href: "/replication", Pages: []string{"replication"}},
			{Label: "Coverage map", Href: "/resilience-coverage", Pages: []string{"resilience-coverage"}},
		},
	},
	{
		ID: "images", Label: "Images", Icon: "images", Href: "/images",
		Tabs: []navTab{
			{Label: "ISO and disk images", Href: "/images", Pages: []string{"images"}},
		},
	},
	{
		ID: "colony", Label: "Colony", Icon: "colony", Href: "/colony-update",
		Tabs: []navTab{
			{Label: "Updates", Href: "/colony-update", Pages: []string{"colony-update"}},
			{Label: "Join", Href: "/machine#join-colony", Pages: []string{"join-colony"}},
			{Label: "Simulations", Href: "/simulate", Pages: []string{"simulate"}},
			{Label: "Recovery handbook", Href: "/recovery-handbook", Pages: []string{"recovery-handbook"}},
			{Label: "Assumptions", Href: "/assumptions", Pages: []string{"assumptions"}},
			{Label: "Assumption register", Href: "/assumption-register", Pages: []string{"assumption-register"}},
			{Label: "Invariants", Href: "/invariants", Pages: []string{"invariants"}},
			{Label: "Why not", Href: "/why-not", Pages: []string{"why-not"}},
			{Label: "Certificates", Href: "/certificates", Pages: []string{"certificates"}},
		},
	},
	{
		ID: "settings", Label: "Settings", Icon: "settings", Href: "/machine", Pinned: true,
		Tabs: []navTab{
			{Label: "Machine", Href: "/machine", Operator: true, Pages: []string{"machine"}},
			{Label: "Users", Href: "/users", Pages: []string{"users"}},
			{Label: "API keys", Href: "/apikeys", Admin: true, Pages: []string{"apikeys"}},
		},
	},
}
