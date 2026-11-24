// Command licensecheck checks the licenses of the third-party code stratum ships.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	exitPass      = 0
	exitFail      = 1
	exitToolError = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

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

type level int

const (
	levelPass level = iota
	levelNote       // passes only by a recorded exception
	levelWarn       // unknown or missing license
	levelFail       // a known license outside the allowlist
)

type verdict struct {
	level  level
	reason string
}

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
