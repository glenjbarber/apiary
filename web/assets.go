// Package web holds the HTMX frontend's templates and static assets,
// embedded into the built binary so cmd/frontend ships as a single
// self-contained file - no separate deploy step for HTML/JS files.
package web

import "embed"

//go:embed templates static
var FS embed.FS
