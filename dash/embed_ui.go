//go:build !noui

package dash

import (
	"embed"
	"io/fs"
)

// Building without dist fails on purpose: the alternative is a blank-page binary.
//
//go:embed all:dist
var dist embed.FS

const Embedded = true

func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return dist
	}
	return sub
}
