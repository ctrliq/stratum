// Command licensecheck checks the licenses of the third-party code stratum
// ships: the Go packages linked into a binary, or the npm packages bundled
// into the dashboard.
//
// Usage:
//
//	licensecheck [-C dir] [-goos os] [-goarch arch,...] [-tags tags] package...
//	licensecheck -npm packages.json
//
// Go mode runs `go list -deps` in dir once per architecture, with cgo and
// workspaces off and GOOS and GOARCH set only on that subprocess, the way
// `make build` compiles the binary. VCS stamping is off (-buildvcs=false):
// the check reads packages and license files, never VCS metadata, so a stray
// VCS directory above dir, such as a junk .git in TMPDIR, cannot fail the
// load. It classifies every linked package that is
// neither standard library nor in the main module; a module replaced by a
// directory inside the repository is checked too. Classification is per
// package, not per module, because a module can mix licenses: a package's
// license is the SPDX-License-Identifier header of its compiled files when
// every one of them has one. Otherwise each valid header still applies to its
// own file, and the nearest LICENSE*, LICENCE* or COPYING* files, walking up
// from the package directory to its module root and classified offline with
// github.com/google/licenseclassifier/v2, apply to the rest.
//
// npm mode reads a JSON array of {name, version, license} objects, the list
// dash/bundled-packages.js writes, and checks each SPDX license expression.
//
// In an expression OR is allowed when any alternative is, and AND only when
// every term is; several license files count as AND. A known license outside
// the allowlist prints a FAIL line, and an unknown or missing license prints a
// WARN line without failing. Under GITHUB_ACTIONS=true each finding is an
// ::error:: or ::warning:: workflow command instead.
//
// The exit status is 0 when nothing fails, 1 when a package fails, and 2 when
// the check cannot run: a bad flag, unreadable input, or a package that does
// not load, which would otherwise drop out of the check and pass silently.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// Exit statuses.
const (
	exitPass      = 0
	exitFail      = 1
	exitToolError = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the whole command minus os.Exit: it returns the exit status.
// Findings go to stdout, and tool errors and the summary to stderr.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("licensecheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("C", ".", "run go list in `dir`")
	goos := fs.String("goos", "linux", "target `GOOS`")
	goarch := fs.String("goarch", "amd64,arm64", "comma-separated target `GOARCH` list, one go list each")
	tags := fs.String("tags", "", "comma-separated build `tags`")
	npmList := fs.String("npm", "", "check the npm packages listed in `file` instead of Go packages")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: licensecheck [-C dir] [-goos os] [-goarch arch,...] [-tags tags] package...")
		fmt.Fprintln(stderr, "       licensecheck -npm packages.json")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitPass
		}
		return exitToolError
	}

	rep := &reporter{out: stdout, actions: os.Getenv("GITHUB_ACTIONS") == "true"}
	var err error
	switch {
	case *npmList != "" && fs.NArg() > 0:
		err = errors.New("-npm takes no package arguments")
	case *npmList != "":
		err = checkNpm(*npmList, rep, stderr)
	case fs.NArg() == 0:
		err = errors.New("no packages to check")
	default:
		var arches []string
		for _, arch := range strings.Split(*goarch, ",") {
			if arch = strings.TrimSpace(arch); arch != "" {
				arches = append(arches, arch)
			}
		}
		if len(arches) == 0 {
			err = errors.New("-goarch names no architecture")
			break
		}
		err = checkGo(buildTarget{dir: *dir, goos: *goos, goarchs: arches, tags: *tags}, fs.Args(), rep, stderr)
	}
	if err != nil {
		fmt.Fprintln(stderr, "licensecheck:", err)
		return exitToolError
	}
	if rep.failed > 0 {
		return exitFail
	}
	return exitPass
}

// level is how a package fares against the license policy.
type level int

const (
	levelPass level = iota
	levelNote       // passes only by a recorded exception
	levelWarn       // unknown or missing license
	levelFail       // a known license outside the allowlist
)

// verdict is one package's level and the reason for it.
type verdict struct {
	level  level
	reason string
}

// reporter prints one line per finding and counts them.
type reporter struct {
	out     io.Writer
	actions bool // print GitHub Actions workflow commands
	failed  int
	warned  int
}

func (r *reporter) report(name string, v verdict) {
	switch v.level {
	case levelFail:
		r.failed++
		r.line("FAIL", "error", name, v.reason)
	case levelWarn:
		r.warned++
		r.line("WARN", "warning", name, v.reason)
	case levelNote:
		r.line("NOTE", "notice", name, v.reason)
	}
}

func (r *reporter) line(plain, command, name, reason string) {
	msg := name + ": " + reason
	if r.actions {
		fmt.Fprintf(r.out, "::%s title=licensecheck::%s\n", command, escapeData(msg))
		return
	}
	fmt.Fprintf(r.out, "%s %s\n", plain, msg)
}

func (r *reporter) summary(w io.Writer, checked string) {
	fmt.Fprintf(w, "licensecheck: %s: %d fail, %d warn\n", checked, r.failed, r.warned)
}

// escapeData escapes a workflow command's message as GitHub Actions expects.
func escapeData(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}
