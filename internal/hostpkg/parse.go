package hostpkg

import (
	"sort"
	"strings"
)

// Every function in this file is a pure parser: command output in,
// plain Go values out, no I/O and no clock. That separation is what
// makes the whole package testable on a non-FreeBSD host, and it is the
// same reasoning internal/hoststats states for its own parse* helpers.
//
// The shared rule across all of them: a line that cannot be understood
// is never dropped and never guessed at. It is returned to the caller
// as an unparsed line, and every caller downgrades the affected verdict
// to unknown and records the reason. A parser that silently skipped what
// it did not recognise would turn pkg's own output drift into a
// confident "up to date", which is the single failure mode this package
// exists to prevent.

// parseInstalledPackages reads `pkg query -e "%n|%v|%o" %n-%v`.
//
// The format string is chosen so each package is exactly one line of
// three `|`-separated fields. FreeBSD package names, versions and
// origins contain no `|`, so the separator is unambiguous; a line with
// any other field count is a line this build does not understand and
// comes back unparsed rather than being shifted into the wrong column.
func parseInstalledPackages(stdout string) (packages []InstalledPackage, unparsed []string) {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != 3 {
			unparsed = append(unparsed, line)
			continue
		}
		name := strings.TrimSpace(fields[0])
		version := strings.TrimSpace(fields[1])
		origin := strings.TrimSpace(fields[2])
		if name == "" || version == "" {
			unparsed = append(unparsed, line)
			continue
		}
		packages = append(packages, InstalledPackage{Name: name, Version: version, Origin: origin})
	}
	sort.Slice(packages, func(i, j int) bool { return packages[i].Name < packages[j].Name })
	return packages, unparsed
}

// parseBaseVersion classifies one `pkg version` line.
//
// pkg's exact phrasing has varied across releases - "FreeBSD
// 15.0-RELEASE-p4 [repo FreeBSD]: up to date" and "FreeBSD
// 14.2-RELEASE: 14.2-RELEASE-p2 [repo FreeBSD] up to date" have both
// been observed - so this matches on the two outcome words that carry
// the actual meaning and refuses everything else. An unrecognized line
// is unknown, not "up to date": a future pkg that rewords this must
// degrade the verdict, never silently improve it.
func parseBaseVersion(line string) UpdateStatus {
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "updates available"):
		// Checked before "up to date" purely so the order is explicit;
		// the two phrasings do not overlap.
		return UpdateStatusOutdated
	case strings.Contains(lower, "up to date"):
		return UpdateStatusCurrent
	default:
		return UpdateStatusUnknown
	}
}

// parseBaseVersionAll folds every non-empty line of `pkg version`
// together. It is deliberately all-or-nothing: pkg prints one line per
// thing it can comment on, so a single unrecognized line means this
// build cannot read pkg's verdict in full, and a partial "up to date"
// would be exactly the over-confident answer this package must not give.
func parseBaseVersionAll(stdout string) (status UpdateStatus, detail string, recognized bool) {
	var lines []string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return UpdateStatusUnknown, "", false
	}
	status = UpdateStatusCurrent
	for _, line := range lines {
		got := parseBaseVersion(line)
		if got == UpdateStatusUnknown {
			return UpdateStatusUnknown, line, true
		}
		if got == UpdateStatusOutdated {
			status = UpdateStatusOutdated
		}
	}
	return status, strings.Join(lines, "; "), true
}

// splitSpecName strips a package's own name off a `name-version` spec.
// It is used only where the name is already known from the inventory,
// so no guess about where the name ends and the version begins is ever
// made: if the spec does not literally start with "<name>-", this
// returns ok=false and the caller treats the line as unreadable.
//
// Doing it this way matters because FreeBSD package names may contain
// hyphens (py39-setuptools, bhyve-firmware) while versions may too
// (1.2.3-p4), so no separator-based split is safe in the general case.
func splitSpecName(name, spec string) (version string, ok bool) {
	prefix := name + "-"
	if !strings.HasPrefix(spec, prefix) {
		return "", false
	}
	return spec[len(prefix):], true
}

// nameResolver resolves a `name` or a `name-version` token to an
// installed package, without ever guessing where a name ends.
//
// FreeBSD package names may contain hyphens (bash-completion,
// py39-setuptools) and versions may too (15.0-RELEASE-p4), so no
// separator-based split is safe. Instead the token is matched against
// the installed names, longest first, so the most specific name wins
// (a "bash-completion-1.0" token resolves to bash-completion, never to
// bash with a version of "completion-1.0").
type nameResolver struct {
	// names is the installed name set, for the exact-match case.
	names map[string]string
	// sortedByLengthDesc is every installed name, longest first.
	sortedByLengthDesc []string
}

func newNameResolver(installed []InstalledPackage) nameResolver {
	r := nameResolver{names: make(map[string]string, len(installed))}
	for _, p := range installed {
		r.names[p.Name] = p.Version
		r.sortedByLengthDesc = append(r.sortedByLengthDesc, p.Name)
	}
	sort.Slice(r.sortedByLengthDesc, func(i, j int) bool {
		if len(r.sortedByLengthDesc[i]) != len(r.sortedByLengthDesc[j]) {
			return len(r.sortedByLengthDesc[i]) > len(r.sortedByLengthDesc[j])
		}
		return r.sortedByLengthDesc[i] < r.sortedByLengthDesc[j]
	})
	return r
}

// resolve maps a bare package name to that package, and a
// name-version spec to the package and the version it names.
func (r nameResolver) resolve(token string) (name, version string, ok bool) {
	version, ok = r.names[token]
	if ok {
		return token, version, true
	}
	for _, candidate := range r.sortedByLengthDesc {
		if version, ok := splitSpecName(candidate, token); ok {
			return candidate, version, true
		}
	}
	return "", "", false
}

// isBareName reports whether token is exactly an installed package
// name, with no version appended. This is what distinguishes pkg's
// two-line layout (leading field is the bare name) from a single-line
// "name-version < name-version" entry (leading field already carries a
// version).
func (r nameResolver) isBareName(token string) bool {
	_, ok := r.names[token]
	return ok
}

// currentOutdatedName reads a "<" line's package name and the version
// it claims is installed, requiring the token after the marker to be a
// genuine name-version spec. A bare name there is not a layout this
// build understands, and accepting it would let a changed pkg format be
// read as a confident "up to date".
func currentOutdatedName(r nameResolver, fields []string, idxLess int) (name, version string, ok bool) {
	if idxLess < 1 || idxLess+1 >= len(fields) {
		return "", "", false
	}
	// fields[0] is the package name in pkg's own layout; a single-line
	// variant prints name-version here instead. Both resolve through the
	// installed list.
	resolvedName, _, ok := r.resolve(fields[0])
	if !ok {
		return "", "", false
	}
	currentVersion, ok := splitSpecName(resolvedName, fields[idxLess+1])
	if !ok {
		return "", "", false
	}
	return resolvedName, currentVersion, true
}

// parseOutdated reads `pkg outdated` against the installed inventory.
//
// pkg's table is a two-line-per-package layout - a "<" line naming the
// currently installed name-version and a ">" line naming the candidate
// name-version - and the column layout has changed across pkg versions,
// including a single-line "name-version < name-version" form. Rather
// than depend on column positions, this pairs the "<" line with the
// next ">" line and then resolves BOTH tokens through the installed
// inventory's own names, which is unambiguous (see nameResolver). A
// token that matches no installed package is not attributed to some
// other package by guesswork; it is reported unparsed and every verdict
// downgrades to unknown.
func parseOutdated(stdout string, installed []InstalledPackage) (outdated map[string]string, unparsed []string) {
	outdated = make(map[string]string)
	resolver := newNameResolver(installed)
	var pendingCurrent string // the name whose "<" line we have seen
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		idxLess, idxMore := indexOf(fields, "<"), indexOf(fields, ">")
		switch {
		case idxLess >= 0 && idxMore >= 0:
			// Both markers on one line is a layout this build does not
			// handle. Refusing is the safe direction: guessing a layout
			// is how a missing update becomes a reported "up to date".
			unparsed = append(unparsed, trimmed)
		case idxLess >= 0:
			name, _, ok := currentOutdatedName(resolver, fields, idxLess)
			if !ok {
				unparsed = append(unparsed, trimmed)
				continue
			}
			if resolver.isBareName(fields[idxLess-1]) {
				// pkg's own two-line layout: the bare package name, then
				// the currently installed spec. The candidate arrives on
				// the following ">" line.
				pendingCurrent = name
				continue
			}
			// Single-line layout: "name-version < name-version". The
			// leading token already carried a version, so the token
			// after the marker is the candidate, not the current spec.
			candidate, ok := splitSpecName(name, fields[idxLess+1])
			if !ok {
				unparsed = append(unparsed, trimmed)
				continue
			}
			outdated[name] = candidate
		case idxMore >= 0:
			if pendingCurrent == "" || idxMore+1 >= len(fields) {
				unparsed = append(unparsed, trimmed)
				continue
			}
			name := pendingCurrent
			pendingCurrent = ""
			specName, version, ok := resolver.resolve(fields[idxMore+1])
			if !ok || specName != name {
				unparsed = append(unparsed, trimmed)
				continue
			}
			outdated[name] = version
		default:
			// A header line, or output this build does not understand.
			unparsed = append(unparsed, trimmed)
		}
	}
	if pendingCurrent != "" {
		// A "<" line with no ">" partner. The package is not claimed to
		// be current either, so this is recorded as its own unreadable
		// fact rather than losing the knowledge that something was
		// printed for it.
		unparsed = append(unparsed, "<line for "+pendingCurrent+" had no candidate line>")
	}
	return outdated, unparsed
}

// indexOf returns the position of want in fields, or -1. fields is
// always short (a handful of whitespace-separated fields), so a linear
// scan is the clearest form rather than a map allocation per line.
func indexOf(fields []string, want string) int {
	for i, f := range fields {
		if f == want {
			return i
		}
	}
	return -1
}
