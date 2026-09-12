// Package frontend is the metiche board: a Go service that consumes the
// coordination backend's event stream and re-broadcasts rendered HTML.
//
// The static assets and the fixture recordings are embedded so the binary runs
// from anywhere with no arguments and nothing beside it.
package frontend

import (
	"embed"
	"io/fs"
)

//go:embed static
var staticFS embed.FS

//go:embed fixtures
var fixturesFS embed.FS

// Static is the /static/ tree: the vendored htmx build, its SSE extension, the
// stylesheet and the thirty lines of page script. Nothing is fetched from a CDN
// — a board that stops working when a CDN does is not an operations board.
func Static() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

// Fixtures is the recorded event streams used when no backend is configured.
func Fixtures() fs.FS {
	sub, err := fs.Sub(fixturesFS, "fixtures")
	if err != nil {
		panic(err)
	}
	return sub
}
