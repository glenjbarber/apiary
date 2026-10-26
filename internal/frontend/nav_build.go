package frontend

import "sort"

// Building the rail and the tab strip (handoff section 6).
//
// The rail is the FIRST level of navigation and the tab strip under it
// is the SECOND. There is no third level.
//
// THE COMBS SECTION'S TABS ARE PER-COMB, so they are added per request
// rather than being part of the static table. A Comb gets a tab because
// it exists, and its page already exists ("/host/{id}"), which is why
// the Comb-as-sidebar-item objection ADR-0059 recorded no longer
// applies.

// buildNav builds the rail for the current page.
//
// canOperate/canAdmin gate the tabs and the sections the session may
// see. A tab the session may not use is not rendered at all.
//
// combs are the known Combs, for the Combs section's per-Comb tabs.
// An empty list means the Combs section has only its static tabs.
//
// alerts names the sections that have something warning, critical, or
// stale in them - the rail's amber dot.
func (s *Server) buildNav(activePage, activeNodeID string, canOperate, canAdmin bool, combs []string, alerts map[string]bool) []railSectionView {
	out := make([]railSectionView, 0, len(navSections))
	for _, section := range navSections {
		view := railSectionView{
			ID:     section.ID,
			Label:  section.Label,
			Icon:   section.Icon,
			Href:   section.Href,
			Pinned: section.Pinned,
			Alert:  alerts[section.ID],
		}
		for _, tab := range section.Tabs {
			if tab.Admin && !canAdmin {
				continue
			}
			if tab.Operator && !canOperate {
				continue
			}
			view.Tabs = append(view.Tabs, railTabView{
				Label:  tab.Label,
				Href:   tab.Href,
				Active: navTabActive(tab, activePage),
			})
		}
		// A section whose landing page this session may not open
		// (the Settings section's Operator-only Machine page) must
		// point at the first tab it MAY open instead. The section
		// itself stays in the rail - it still has the Users tab,
		// which every role may reach.
		if !navHrefAllowed(section.Href, canOperate, canAdmin) {
			for _, tab := range section.Tabs {
				if tab.Admin && !canAdmin {
					continue
				}
				if tab.Operator && !canOperate {
					continue
				}
				view.Href = tab.Href
				break
			}
		}
		// A section is active when one of its tabs is. The Overview
		// section's tab covers "/" and the per-Comb pages, so a Comb's
		// own page lights up the Overview rail item too - and the
		// Combs section's per-Comb tab as well.
		for _, tab := range view.Tabs {
			if tab.Active {
				view.Active = true
			}
		}
		// A section whose own landing page is the current page is
		// active even when no tab matched - which is how a section
		// with a dynamic tab set (Combs) still lights up.
		if navSectionActive(section, activePage) {
			view.Active = true
		}
		out = append(out, view)
	}

	// The Combs section's per-Comb tabs.
	for i := range out {
		if out[i].ID != "combs" {
			continue
		}
		for _, nodeID := range combs {
			if nodeID == "" {
				continue
			}
			out[i].Tabs = append(out[i].Tabs, railTabView{
				Label: nodeID, Href: "/host/" + nodeID,
				// Only the Comb whose own page is open is active -
				// not every Comb tab at once.
				Active:  activePage == "host" && nodeID == activeNodeID,
				Dynamic: true,
			})
		}
		// The per-Comb tabs come BEFORE the static Maintenance tab, so
		// the Combs the operator is looking at are at the front of
		// the strip rather than after a page that is not about a
		// Comb.
		sort.SliceStable(out[i].Tabs, func(a, b int) bool {
			if out[i].Tabs[a].Dynamic != out[i].Tabs[b].Dynamic {
				return out[i].Tabs[a].Dynamic
			}
			return false
		})
	}
	return out
}

// navHrefAllowed reports whether a section's landing page is one this
// session may open.
//
// The Settings section is the case that matters: its landing page is
// the Machine page, which is Operator-only, but the section also
// carries the Users tab, which every role may reach. A Viewer must not
// be pointed at a page that would refuse them.
func navHrefAllowed(href string, canOperate, canAdmin bool) bool {
	for _, section := range navSections {
		if section.Href != href {
			continue
		}
		for _, tab := range section.Tabs {
			if tab.Href != href {
				continue
			}
			if tab.Admin && !canAdmin {
				return false
			}
			if tab.Operator && !canOperate {
				return false
			}
			return true
		}
	}
	return true
}

// navTabActive reports whether a static tab is the current page's tab.
//
// A tab covers several Pages, so a detail page still highlights its
// section's tab - "/vms/{id}" is still the "Virtual machines" tab.
func navTabActive(tab navTab, activePage string) bool {
	for _, page := range tab.Pages {
		if page == activePage {
			return true
		}
	}
	return false
}

// navSectionActive reports whether a section is the current page's
// section, by its own landing page.
func navSectionActive(section navSection, activePage string) bool {
	switch section.ID {
	case "overview":
		return activePage == "stats" || activePage == "host"
	case "combs":
		// A per-Comb page is the Combs section's page.
		return activePage == "host"
	}
	return false
}

// navAlertsFrom derives the rail's per-section amber dots.
//
// A dot sits on a section when something in that section is warning,
// critical, or stale - the handoff's section 5. A dot is NOT a count,
// and it is NOT shown for a section whose only issue is an unknown
// reading: "could not check" is not "something is wrong", and a dot on
// every section because the Colony is unreachable would train the
// operator to ignore it.
//
// The mapping is from a subject (a Comb, a cell, a certificate) to the
// section that owns it. It is a table rather than a rule because
// "which section owns this" is a fact about the information
// architecture, not a computation.
func navAlertsFrom(combs overviewCombView, attention []overviewAttentionItem) map[string]bool {
	alerts := map[string]bool{}

	// A Comb's own state belongs to the Combs section.
	for _, comb := range combs.Combs {
		switch comb.State {
		case statusWarn, statusCritical, statusContradictory, statusStale:
			alerts["combs"] = true
		}
		// A Comb over a resource threshold is that Comb's problem,
		// and the Cells section is where a cell's resources are
		// managed.
		for _, meter := range []overviewMeter{comb.CPU, comb.Memory, comb.ZFS} {
			if meter.Warn {
				alerts["combs"] = true
			}
		}
	}

	// The attention list names the subject of each item, so a subject
	// that names a known Comb marks the Combs section.
	known := map[string]bool{}
	for _, comb := range combs.Combs {
		known[comb.NodeID] = true
	}
	for _, item := range attention {
		switch item.State {
		case statusWarn, statusCritical, statusContradictory, statusStale:
		default:
			continue
		}
		if known[item.Subject] {
			alerts["combs"] = true
			continue
		}
		// A certificate expiring soon belongs to the Colony section,
		// which is where the Certificates page lives.
		if item.Href == "/certificates" {
			alerts["colony"] = true
		}
	}
	return alerts
}
