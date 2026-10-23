//go:build noui

package dash

import (
	"io/fs"
	"testing/fstest"
)

// Embedded is false: the handler serves its placeholder page instead.
const Embedded = false

// FS returns an empty filesystem.
func FS() fs.FS { return fstest.MapFS{} }
