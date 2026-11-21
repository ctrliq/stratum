package main

import "testing"

// filepath-securejoin mixes licenses per package; pathrs-lite is MPL-2.0.
const (
	securejoinRoot = "github.com/cyphar/filepath-securejoin" // also the module path
	pathrsLite     = securejoinRoot + "/pathrs-lite"
)

// Uses the testdata/securejoin stub: the real module's paths, headers and COPYING.md.
func TestPathrsLiteGuardFixture(t *testing.T) {
	securejoin := module{path: securejoinRoot, dir: "securejoin", copyFrom: "testdata/securejoin"}
	bsdPackages := []string{
		securejoinRoot,
		securejoinRoot + "/internal/consts",
		pathrsLite + "/internal/gocompat",
	}

	t.Run("root package only passes", func(t *testing.T) {
		dir := fixture(t, map[string]string{"main.go": mainFile(securejoinRoot)}, securejoin)
		r := check(t, goArgs(dir, ".")...)
		wantExit(t, r, 0)
		for _, pkg := range bsdPackages[:2] {
			wantPass(t, r, pkg)
		}
		if got := r.lines(failLine); len(got) > 0 {
			t.Errorf("the BSD-3-Clause root package failed: %q", got)
		}
	})

	t.Run("stratum imports pathrs-lite", func(t *testing.T) {
		dir := fixture(t, map[string]string{"main.go": mainFile(securejoinRoot, pathrsLite)}, securejoin)
		r := check(t, goArgs(dir, ".")...)
		wantExit(t, r, 1)
		wantFail(t, r, failLine, pathrsLite, "MPL-2.0")
		wantFail(t, r, failLine, pathrsLite+"/internal/fd", "MPL-2.0")
		for _, pkg := range bsdPackages {
			wantPass(t, r, pkg)
		}
		wantOnly(t, r, failLine, pathrsLite, pathrsLite+"/internal/fd")
	})

	t.Run("a dependency imports pathrs-lite", func(t *testing.T) {
		helmlike := module{
			path:     "example.com/helmlike",
			dir:      "deps/helmlike",
			requires: []string{securejoinRoot},
			files: map[string]string{
				"LICENSE":     licenseText(t, "Apache-2.0"),
				"helmlike.go": src("", "helmlike", pathrsLite),
			},
		}
		dir := fixture(t, map[string]string{"main.go": mainFile(securejoinRoot, "example.com/helmlike")},
			securejoin, helmlike)
		r := check(t, goArgs(dir, ".")...)
		wantExit(t, r, 1)
		wantFail(t, r, failLine, pathrsLite, "MPL-2.0")
		wantPass(t, r, "example.com/helmlike")
		wantPass(t, r, securejoinRoot)
	})
}

// Pins securejoin v0.7.0, so go list resolves the real module with no updates.
const realSecurejoinGoSum = `github.com/cyphar/filepath-securejoin v0.7.0 h1:s0Y3ITPy6sQn5xt54DuYvTF8hu134ooYLUb58DX/HjE=
github.com/cyphar/filepath-securejoin v0.7.0/go.mod h1:ymLGms/u3BYaviIiuKFnUx8EkQEZeK6cInNoAPJA3o4=
golang.org/x/sys v0.26.0 h1:KHjCJyddX0LoSTb3J+vWpupP9p0oznkqVk/IfjymZbo=
golang.org/x/sys v0.26.0/go.mod h1:/VUhepiaJMQUp4+oa/7Zr1D23ma6VTLIYjOOTFZPUcA=
`

const realSecurejoinGoMod = `module example.com/app

go 1.22

require github.com/cyphar/filepath-securejoin v0.7.0

require golang.org/x/sys v0.26.0 // indirect
`

// Checks the published v0.7.0 module for both shipped architectures.
func TestPathrsLiteGuardRealModule(t *testing.T) {
	scratch := func(t *testing.T, imports ...string) string {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{
			"go.mod":  realSecurejoinGoMod,
			"go.sum":  realSecurejoinGoSum,
			"main.go": mainFile(imports...),
		})
		return dir
	}
	args := func(dir string) []string {
		return []string{"-C", dir, "-goos", "linux", "-goarch", "amd64,arm64", "."}
	}

	t.Run("root package only passes", func(t *testing.T) {
		r := check(t, args(scratch(t, securejoinRoot))...)
		wantExit(t, r, 0)
		wantPass(t, r, securejoinRoot)
		wantPass(t, r, securejoinRoot+"/internal/consts")
		if got := r.lines(failLine); len(got) > 0 {
			t.Errorf("the BSD-3-Clause root package failed: %q", got)
		}
	})

	t.Run("pathrs-lite fails", func(t *testing.T) {
		r := check(t, args(scratch(t, securejoinRoot, pathrsLite, pathrsLite+"/procfs"))...)
		wantExit(t, r, 1)
		wantFail(t, r, failLine, pathrsLite, "MPL-2.0")
		wantFail(t, r, failLine, pathrsLite+"/procfs", "MPL-2.0")
		wantPass(t, r, securejoinRoot)
		wantPass(t, r, securejoinRoot+"/internal/consts")
		wantPass(t, r, pathrsLite+"/internal/gocompat")
		wantPass(t, r, pathrsLite+"/internal/kernelversion")
	})
}
