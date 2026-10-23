//go:build !noui

package dash

import (
	"embed"
	"io/fs"
)

// dist is the Vite output: `pnpm run build` in dash/. Building without it
// fails on purpose — the alternative is a binary that serves a blank page.
//
//go:embed all:dist
var dist embed.FS

// Embedded reports whether the SPA is compiled into this binary.
const Embedded = true

// FS returns the built SPA rooted at dist/, so "index.html" resolves.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return dist
	}
	return sub
}
