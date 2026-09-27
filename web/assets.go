// Package web holds the HTMX frontend's templates and static assets,
// embedded into the built binary so cmd/frontend ships as a single
// self-contained file - no separate deploy step for HTML/JS files.
package web

import "embed"

// all: on templates (not static): embed.FS's default pattern silently
// excludes any file or directory named with a leading "_" (or "."),
// which is exactly the naming convention this project's own shared
// partials use (_gauge.html, _health_card.html) - see
// docs/web-ui-redesign.md Section 1. Without "all:" those files build
// and pass go vet cleanly but are simply absent from the embedded FS,
// so html/template.ParseFS silently parses zero of them and any
// {{template "gauge" ...}} call fails at render time with "no such
// template," not at build time.
//
//go:embed all:templates static
var FS embed.FS
