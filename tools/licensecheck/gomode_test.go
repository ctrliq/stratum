package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestClassifiesEachLinkedPackage runs one build that links one package per
// case and checks each package's own verdict. Only the main module is
// first-party; every dependency is checked, even one replaced by a directory
// inside the main module.
func TestClassifiesEachLinkedPackage(t *testing.T) {
	mit, isc := licenseText(t, "MIT"), licenseText(t, "ISC")
	bsd2, bsd3 := licenseText(t, "BSD-2-Clause"), licenseText(t, "BSD-3-Clause")
	apache, mpl := licenseText(t, "Apache-2.0"), licenseText(t, "MPL-2.0")
	ccbysa := licenseText(t, "CC-BY-SA-4.0")

	deps := []module{
		// Allowed license files, under each recognised file name.
		{path: "example.com/mit", dir: "deps/mit", files: map[string]string{
			"LICENSE": mit, "mit.go": src("", "mit")}},
		{path: "example.com/bsd2", dir: "deps/bsd2", files: map[string]string{
			"LICENSE": bsd2, "bsd2.go": src("", "bsd2")}},
		{path: "example.com/bsd3", dir: "deps/bsd3", files: map[string]string{
			"LICENSE.txt": bsd3, "bsd3.go": src("", "bsd3")}},
		{path: "example.com/apache", dir: "deps/apache", files: map[string]string{
			"LICENSE.md": apache, "apache.go": src("", "apache")}},
		{path: "example.com/isc", dir: "deps/isc", files: map[string]string{
			"LICENCE": isc, "isc.go": src("", "isc")}},
		{path: "example.com/copying", dir: "deps/copying", files: map[string]string{
			"COPYING": bsd3, "copying.go": src("", "copying")}},

		// SPDX headers on every compiled file, in a module with no license
		// file: the headers alone decide each package.
		{path: "example.com/spdx", dir: "deps/spdx", files: map[string]string{
			"zerobsd/a.go": src(spdx("0BSD"), "zerobsd"),
			"zlib/a.go":    src(spdx("Zlib"), "zlib"),
			"bsl/a.go":     src(spdx("BSL-1.0"), "bsl"),
		}},

		// SPDX expressions. The module has no license file, so a checker that
		// ignored the headers would warn on every one of these instead.
		{path: "example.com/expr", dir: "deps/expr", files: map[string]string{
			"orpass/a.go":     src(spdx("MIT OR MPL-2.0"), "orpass"),
			"orfail/a.go":     src(spdx("MPL-2.0 OR CC-BY-SA-4.0"), "orfail"),
			"andpass/a.go":    src(spdx("MIT AND Apache-2.0"), "andpass"),
			"andfail/a.go":    src(spdx("Apache-2.0 AND MPL-2.0"), "andfail"),
			"parenpass/a.go":  src(spdx("(MIT OR MPL-2.0) AND Apache-2.0"), "parenpass"),
			"parenfail/a.go":  src(spdx("(MIT OR Apache-2.0) AND MPL-2.0"), "parenfail"),
			"precedence/a.go": src(spdx("MIT OR MPL-2.0 AND CC-BY-SA-4.0"), "precedence"),
			"withfail/a.go":   src(spdx("GPL-2.0-only WITH Classpath-exception-2.0"), "withfail"),
		}},

		// Known, non-allowlisted licenses.
		{path: "example.com/mplfile", dir: "deps/mplfile", files: map[string]string{
			"LICENSE": mpl, "mplfile.go": src("", "mplfile")}},
		{path: "example.com/twofiles", dir: "deps/twofiles", files: map[string]string{
			"LICENSE": mit, "LICENSE.MPL": mpl, "twofiles.go": src("", "twofiles")}},
		{path: "example.com/files", dir: "deps/files", files: map[string]string{
			"LICENSE":           mit,
			"mplheader/a.go":    src(spdx("MPL-2.0"), "mplheader"),
			"mplheader/b.go":    src(spdx("MPL-2.0"), "mplheader"),
			"mixedheaders/a.go": src(spdx("MIT"), "mixedheaders"),
			"mixedheaders/b.go": src(spdx("MPL-2.0"), "mixedheaders"),
		}},
		// A header on some files still applies to them: an MPL-2.0 file
		// beside an unheaded one fails, though the module's LICENSE is MIT.
		{path: "example.com/partialheader", dir: "deps/partialheader", files: map[string]string{
			"LICENSE":   mit,
			"headed.go": src(spdx("MPL-2.0"), "partialheader"),
			"bare.go":   src("", "partialheader")}},
		// A prebuilt object has no header either: its LICENSE decides, but
		// the Go file's MPL-2.0 header still fails the package.
		{path: "example.com/sysoheader", dir: "deps/sysoheader", files: map[string]string{
			"LICENSE":         mit,
			"headed.go":       src(spdx("MPL-2.0"), "sysoheader"),
			"blob_amd64.syso": "not an object file; go list reads only the name",
			"blob_arm64.syso": "not an object file; go list reads only the name"}},
		// An embedded file has no header either: the module's MPL-2.0
		// LICENSE applies though every Go file is headed MIT.
		{path: "example.com/embedded", dir: "deps/embedded", files: map[string]string{
			"LICENSE":     mpl,
			"embedded.go": src(spdx("MIT"), "embedded", "embed") + "\n//go:embed data.txt\nvar data string\n",
			"data.txt":    "shipped in the binary"}},
		// A header the check cannot read is an unknown license for its file,
		// though the module's LICENSE is MIT.
		{path: "example.com/badheader", dir: "deps/badheader", files: map[string]string{
			"LICENSE":   mit,
			"headed.go": src(spdx("MPL 2.0"), "badheader"),
			"bare.go":   src("", "badheader")}},
		// Only comments before the first code are a header: an SPDX tag
		// after "/* banner */ package p" is not one, so the LICENSE decides.
		{path: "example.com/bannercode", dir: "deps/bannercode", files: map[string]string{
			"LICENSE": mpl,
			"a.go":    "/* banner */ package bannercode\n\n" + spdx("MIT") + "\n"}},
		// Every tag in the header applies, not only the first.
		{path: "example.com/twotags", dir: "deps/twotags", files: map[string]string{
			"LICENSE": mit,
			"a.go":    src(spdx("MIT")+"\n"+spdx("MPL-2.0"), "twotags")}},
		// A header file that assembly can #include ships too, so its own
		// header counts.
		{path: "example.com/asmheader", dir: "deps/asmheader", files: map[string]string{
			"LICENSE": mit,
			"a.go":    src(spdx("MIT"), "asmheader"),
			"a.s":     spdx("MIT") + "\n\n#include \"inc.h\"\n",
			"inc.h":   spdx("MPL-2.0") + "\n\n#define X 1\n"}},
		// A malformed tag does not hide a valid one beside it.
		{path: "example.com/maskedtag", dir: "deps/maskedtag", files: map[string]string{
			"LICENSE": mit,
			"a.go":    src(spdx("MPL-2.0")+"\n"+spdx("not a license!"), "maskedtag")}},
		// A header after an opening line too long for a default scanner.
		{path: "example.com/longline", dir: "deps/longline", files: map[string]string{
			"LICENSE": mit,
			"a.go":    src("// "+strings.Repeat("x", 70<<10)+"\n"+spdx("MPL-2.0"), "longline")}},
		// One compiled file has no header, so the license file decides.
		{path: "example.com/partial", dir: "deps/partial", files: map[string]string{
			"LICENSE":   mpl,
			"headed.go": src(spdx("MIT"), "partial"),
			"bare.go":   src("", "partial"),
		}},
		// Files the build does not compile do not need a header.
		{path: "example.com/uncompiled", dir: "deps/uncompiled", files: map[string]string{
			"LICENSE":           mpl,
			"a.go":              src(spdx("MIT"), "uncompiled"),
			"a_test.go":         src("", "uncompiled"),
			"ignored.go":        src("//go:build ignore", "uncompiled"),
			"only_windows.go":   src("", "uncompiled"),
			"only_plan9_arm.go": src("", "uncompiled"),
		}},

		// The nearest license files win, walking up to the module root.
		{path: "example.com/nested", dir: "deps/nested", files: map[string]string{
			"LICENSE":         mpl,
			"nested.go":       src("", "nested"),
			"other/other.go":  src("", "other"),
			"sub/LICENSE":     mit,
			"sub/sub.go":      src("", "sub"),
			"sub/deeper/d.go": src("", "deeper"),
		}},

		// Unknown or missing licenses warn. deps/LICENSE, written below, sits
		// above every module root and must never be read.
		{path: "example.com/unknowntext", dir: "deps/unknowntext", files: map[string]string{
			"LICENSE": unknownText, "unknowntext.go": src("", "unknowntext")}},
		// An allowed text followed by terms no license matches, alone or
		// after a copyright notice.
		{path: "example.com/copyrightterms", dir: "deps/copyrightterms", files: map[string]string{
			"LICENSE":           mit + "\nCopyright (c) Example. Commercial use prohibited.\n",
			"copyrightterms.go": src("", "copyrightterms")}},
		{path: "example.com/copyrightclause", dir: "deps/copyrightclause", files: map[string]string{
			"LICENSE":            mit + "\nCopyright (c) Example — use is permitted for non-commercial purposes only\n",
			"copyrightclause.go": src("", "copyrightclause")}},
		{path: "example.com/addedterms", dir: "deps/addedterms", files: map[string]string{
			"LICENSE":       mit + "\nCommercial use prohibited.\n",
			"addedterms.go": src("", "addedterms")}},
		{path: "example.com/missing", dir: "deps/missing", files: map[string]string{
			"missing.go": src("", "missing")}},

		// The one exception (human decision "Exception: docs license") covers
		// go-digest's CC-BY-SA-4.0 LICENSE.docs, and only go-digest: the same
		// files under another path fail.
		{path: "github.com/opencontainers/go-digest", dir: "deps/go-digest", files: map[string]string{
			"LICENSE": apache, "LICENSE.docs": ccbysa, "digest.go": src("", "digest")}},
		{path: "example.com/digestlike", dir: "deps/digestlike", files: map[string]string{
			"LICENSE": apache, "LICENSE.docs": ccbysa, "digest.go": src("", "digestlike")}},

		// A directory beside the main module whose name extends the main
		// module's directory name is still outside it.
		{path: "example.com/apple", dir: "apple", files: map[string]string{
			"LICENSE": mpl, "apple.go": src("", "apple")}},

		// A replace directory inside the main module is still third-party.
		{path: "example.com/inside", dir: "app/third_party/inside", files: map[string]string{
			"LICENSE": mpl, "inside.go": src("", "inside")}},
	}

	pass := []string{
		"example.com/mit", "example.com/bsd2", "example.com/bsd3", "example.com/apache",
		"example.com/isc", "example.com/copying",
		"example.com/spdx/zerobsd", "example.com/spdx/zlib", "example.com/spdx/bsl",
		"example.com/expr/orpass", "example.com/expr/andpass", "example.com/expr/parenpass",
		"example.com/expr/precedence",
		"example.com/uncompiled",
		"example.com/nested/sub", "example.com/nested/sub/deeper",
		"github.com/opencontainers/go-digest",
	}
	fail := map[string][]string{
		"example.com/expr/orfail":        {"MPL-2.0", "CC-BY-SA-4.0"},
		"example.com/expr/andfail":       {"MPL-2.0"},
		"example.com/expr/parenfail":     {"MPL-2.0"},
		"example.com/expr/withfail":      {"GPL-2.0-only"},
		"example.com/mplfile":            {"MPL-2.0"},
		"example.com/twofiles":           {"MPL-2.0"},
		"example.com/files/mplheader":    {"MPL-2.0"},
		"example.com/files/mixedheaders": {"MPL-2.0"},
		"example.com/partial":            {"MPL-2.0"},
		"example.com/partialheader":      {"MPL-2.0"},
		"example.com/sysoheader":         {"MPL-2.0"},
		"example.com/embedded":           {"MPL-2.0"},
		"example.com/bannercode":         {"MPL-2.0"},
		"example.com/twotags":            {"MPL-2.0"},
		"example.com/maskedtag":          {"MPL-2.0"},
		"example.com/asmheader":          {"MPL-2.0"},
		"example.com/longline":           {"MPL-2.0"},
		"example.com/nested":             {"MPL-2.0"},
		"example.com/nested/other":       {"MPL-2.0"},
		"example.com/digestlike":         {"CC-BY-SA-4.0"},
		"example.com/apple":              {"MPL-2.0"},
		"example.com/inside":             {"MPL-2.0"},
	}
	warn := []string{"example.com/unknowntext", "example.com/addedterms", "example.com/copyrightterms", "example.com/copyrightclause", "example.com/badheader", "example.com/missing"}
	firstParty := []string{"example.com/app", "example.com/app/internal/lib"}

	imports := []string{"fmt", "example.com/app/internal/lib"}
	imports = append(imports, pass...)
	imports = append(imports, warn...)
	for pkg := range fail {
		imports = append(imports, pkg)
	}
	dir := fixture(t, map[string]string{
		"main.go":             mainFile(imports...),
		"LICENSE":             mpl,
		"internal/lib/lib.go": src(spdx("MPL-2.0"), "lib"),
	}, deps...)
	writeTree(t, filepath.Dir(dir), map[string]string{"deps/LICENSE": mpl})

	r := check(t, goArgs(dir, ".")...)
	wantExit(t, r, 1)

	for _, pkg := range pass {
		t.Run("pass "+pkg, func(t *testing.T) { wantPass(t, r, pkg) })
	}
	for pkg, licenses := range fail {
		t.Run("fail "+pkg, func(t *testing.T) { wantFail(t, r, failLine, pkg, licenses...) })
	}
	for _, pkg := range warn {
		t.Run("warn "+pkg, func(t *testing.T) { wantWarn(t, r, pkg) })
	}
	for _, pkg := range firstParty {
		t.Run("first-party "+pkg, func(t *testing.T) { wantPass(t, r, pkg) })
	}
	t.Run("nothing else reported", func(t *testing.T) {
		var failing []string
		for pkg := range fail {
			failing = append(failing, pkg)
		}
		wantOnly(t, r, failLine, failing...)
		wantOnly(t, r, warnLine, warn...)
	})
}

func TestAllowedLicensesExitZero(t *testing.T) {
	dir := fixture(t, map[string]string{
		"main.go": mainFile("fmt", "example.com/mit", "example.com/apache"),
	},
		module{path: "example.com/mit", dir: "deps/mit", files: map[string]string{
			"LICENSE": licenseText(t, "MIT"), "mit.go": src("", "mit")}},
		module{path: "example.com/apache", dir: "deps/apache", files: map[string]string{
			"LICENSE": licenseText(t, "Apache-2.0"), "apache.go": src("", "apache")}},
	)
	r := check(t, goArgs(dir, ".")...)
	wantExit(t, r, 0)
	if got := append(r.lines(failLine), r.lines(warnLine)...); len(got) > 0 {
		t.Errorf("allowed licenses produced findings: %q", got)
	}
}

// Policy: an unknown or missing license warns and never fails the check.
func TestUnknownAndMissingLicensesOnlyWarn(t *testing.T) {
	dir := fixture(t, map[string]string{
		"main.go": mainFile("example.com/mit", "example.com/unknowntext", "example.com/missing"),
	},
		module{path: "example.com/mit", dir: "deps/mit", files: map[string]string{
			"LICENSE": licenseText(t, "MIT"), "mit.go": src("", "mit")}},
		module{path: "example.com/unknowntext", dir: "deps/unknowntext", files: map[string]string{
			"LICENSE": unknownText, "unknowntext.go": src("", "unknowntext")}},
		module{path: "example.com/missing", dir: "deps/missing", files: map[string]string{
			"missing.go": src("", "missing")}},
	)
	r := check(t, goArgs(dir, ".")...)
	wantExit(t, r, 0)
	wantWarn(t, r, "example.com/unknowntext")
	wantWarn(t, r, "example.com/missing")
	wantPass(t, r, "example.com/mit")
	if got := r.lines(failLine); len(got) > 0 {
		t.Errorf("an unknown or missing license failed the check: %q", got)
	}
}

// A package that passes only because a header's license is excepted still
// gets its NOTE line when its other files fall back to an allowed license
// file.
func TestExceptedHeaderBesideLicenseFileNotes(t *testing.T) {
	dir := fixture(t, map[string]string{"main.go": mainFile("github.com/opencontainers/go-digest")},
		module{path: "github.com/opencontainers/go-digest", dir: "deps/go-digest", files: map[string]string{
			"LICENSE":   licenseText(t, "Apache-2.0"),
			"headed.go": src(spdx("CC-BY-SA-4.0"), "digest"),
			"bare.go":   src("", "digest")}},
	)
	r := check(t, goArgs(dir, ".")...)
	wantExit(t, r, 0)
	if got := r.naming(noteLine, "github.com/opencontainers/go-digest"); len(got) != 1 {
		t.Errorf("want one NOTE line naming go-digest, got %q; output:\n%s", got, r.out)
	}
}

// classifierText returns a license file from testdata/classifier, each an
// edge case for the license classifier. MIT-and-Apache-2.0-notice is
// go.yaml.in/yaml/v3 v3.0.5's LICENSE, verbatim: the MIT text, then the
// Apache-2.0 notice that covers the module's other files.
// MIT-and-altered-Apache-2.0-notice is that file with a non-commercial
// sentence inserted into the notice. MIT-and-AGPL-3.0-notice is
// testdata/licenses/MIT.txt followed by the notice from the AGPL-3.0's "How to
// Apply These Terms". BSD-3-Clause-no-nuclear is
// testdata/licenses/BSD-3-Clause.txt with a fourth condition inserted, which
// keeps the software out of nuclear facilities.
func classifierText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "classifier", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A license notice outside every license text in a file is a license of its
// own, as in go.yaml.in/yaml/v3's LICENSE: the MIT text, then an Apache-2.0
// notice for the rest of the module. The same file with an AGPL-3.0 notice
// must fail. The yaml file also says in prose which files each license
// covers, which no license matches, so it warns for a human to read.
func TestLicenseNoticeBesideLicenseText(t *testing.T) {
	dir := fixture(t, map[string]string{
		"main.go": mainFile("example.com/mitagpl", "example.com/mitapache"),
	},
		module{path: "example.com/mitagpl", dir: "deps/mitagpl", files: map[string]string{
			"LICENSE": classifierText(t, "MIT-and-AGPL-3.0-notice"), "mitagpl.go": src("", "mitagpl")}},
		module{path: "example.com/mitapache", dir: "deps/mitapache", files: map[string]string{
			"LICENSE": classifierText(t, "MIT-and-Apache-2.0-notice"), "mitapache.go": src("", "mitapache")}},
	)
	r := check(t, goArgs(dir, ".")...)
	wantExit(t, r, 1)
	wantFail(t, r, failLine, "example.com/mitagpl", "AGPL-3.0")
	wantWarn(t, r, "example.com/mitapache")
	wantOnly(t, r, failLine, "example.com/mitagpl")

	// The file is classified directly to show the notice is found beside
	// the MIT text, and that only the prose is unknown.
	t.Run("both licenses found", func(t *testing.T) {
		fc := &fileClassifier{cache: map[string]fileLicenses{}}
		got, err := fc.classify(filepath.Join("testdata", "classifier", "MIT-and-Apache-2.0-notice.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"Apache-2.0", "MIT"}; !slices.Equal(got.ids, want) {
			t.Errorf("classified as %q, want %q", got.ids, want)
		}
		if want := []string{"7 line(s) match no license"}; !slices.Equal(got.resembles, want) {
			t.Errorf("unknown text %q, want %q: a real notice matched too loosely to count", got.resembles, want)
		}
	})
}

// A text the classifier matches only loosely is not the license it resembles,
// but a different one: a BSD-3-Clause text with a clause added, or an
// Apache-2.0 notice that forbids commercial use. Such a text is unknown, so
// its package warns, naming what it resembles and how closely, even beside a
// license the file identifies.
func TestLooseMatchIsUnknownLicense(t *testing.T) {
	dir := fixture(t, map[string]string{
		"main.go": mainFile("example.com/nonuclear", "example.com/noncommercial"),
	},
		module{path: "example.com/nonuclear", dir: "deps/nonuclear", files: map[string]string{
			"LICENSE": classifierText(t, "BSD-3-Clause-no-nuclear"), "nonuclear.go": src("", "nonuclear")}},
		module{path: "example.com/noncommercial", dir: "deps/noncommercial", files: map[string]string{
			"LICENSE":          classifierText(t, "MIT-and-altered-Apache-2.0-notice"),
			"noncommercial.go": src("", "noncommercial")}},
	)
	r := check(t, goArgs(dir, ".")...)
	wantExit(t, r, 0)
	for pkg, resembled := range map[string]string{
		"example.com/nonuclear":     "BSD-3-Clause",
		"example.com/noncommercial": "Apache-2.0",
	} {
		t.Run(pkg, func(t *testing.T) {
			wantWarn(t, r, pkg)
			want := regexp.MustCompile(`resembles ` + regexp.QuoteMeta(resembled) + ` \(0\.\d\d\)`)
			for _, line := range r.naming(warnLine, pkg) {
				if !want.MatchString(line) {
					t.Errorf("the WARN line does not say the text resembles %s, and how closely: %q", resembled, line)
				}
			}
		})
	}
}

// The checker loads the build exactly as `make build` compiles it: one go
// list per -goarch, for -goos, with cgo off and -tags applied, whatever the
// caller's own environment says, and without changing that environment.
func TestLoadsEachTargetLikeTheShippedBuild(t *testing.T) {
	mpl := licenseText(t, "MPL-2.0")
	only := func(name string) module {
		return module{path: "example.com/" + name, dir: "deps/" + name, files: map[string]string{
			"LICENSE": mpl, name + ".go": src("", name)}}
	}
	dir := fixture(t, map[string]string{
		"main.go":   mainFile("example.com/mit"),
		"arm64.go":  src("//go:build arm64", "main", "example.com/armonly"),
		"darwin.go": src("//go:build darwin", "main", "example.com/darwinonly"),
		"cgo.go":    src("//go:build cgo", "main", "example.com/cgoonly"),
		"tagged.go": src("//go:build licensetag", "main", "example.com/tagonly"),
	},
		module{path: "example.com/mit", dir: "deps/mit", files: map[string]string{
			"LICENSE": licenseText(t, "MIT"), "mit.go": src("", "mit")}},
		only("armonly"), only("darwinonly"), only("cgoonly"), only("tagonly"),
	)
	// The caller's environment names a different target; the checker must
	// override it for go list and leave it untouched.
	callerEnv := map[string]string{"GOOS": "darwin", "GOARCH": "arm64", "CGO_ENABLED": "1"}
	for key, value := range callerEnv {
		t.Setenv(key, value)
	}
	wantEnvUnchanged := func(t *testing.T) {
		t.Helper()
		for key, value := range callerEnv {
			if got := os.Getenv(key); got != value {
				t.Errorf("the checker changed its own %s to %q, want %q", key, got, value)
			}
		}
	}

	t.Run("linux/amd64", func(t *testing.T) {
		r := check(t, "-C", dir, "-goos", "linux", "-goarch", "amd64", ".")
		wantExit(t, r, 0)
		wantPass(t, r, "example.com/mit")
		if got := r.lines(failLine); len(got) > 0 {
			t.Errorf("checked a package linux/amd64 without cgo does not link: %q", got)
		}
		wantEnvUnchanged(t)
	})

	t.Run("linux/amd64,arm64", func(t *testing.T) {
		r := check(t, "-C", dir, "-goos", "linux", "-goarch", "amd64,arm64", ".")
		wantExit(t, r, 1)
		wantFail(t, r, failLine, "example.com/armonly", "MPL-2.0")
		wantOnly(t, r, failLine, "example.com/armonly")
		wantEnvUnchanged(t)
	})

	t.Run("-tags", func(t *testing.T) {
		r := check(t, "-C", dir, "-goos", "linux", "-goarch", "amd64", "-tags", "licensetag", ".")
		wantExit(t, r, 1)
		wantFail(t, r, failLine, "example.com/tagonly", "MPL-2.0")
		wantOnly(t, r, failLine, "example.com/tagonly")
	})
}

// The shipped build uses no workspace, so a go.work, whether GOWORK names it
// or the go command finds it in a parent directory, must not change what is
// checked. In a workspace every module it uses is a main module, which the
// checker would take for first-party code and skip.
func TestIgnoresGoWorkspace(t *testing.T) {
	dir := fixture(t, map[string]string{"main.go": mainFile("example.com/mpl")},
		module{path: "example.com/mpl", dir: "deps/mpl", files: map[string]string{
			"LICENSE": licenseText(t, "MPL-2.0"), "mpl.go": src("", "mpl")}},
	)
	root := filepath.Dir(dir)
	writeTree(t, root, map[string]string{"go.work": "go 1.22\n\nuse (\n\t./app\n\t./deps/mpl\n)\n"})
	work := filepath.Join(root, "go.work")

	for name, gowork := range map[string]string{"found in a parent directory": "", "named by GOWORK": work} {
		t.Run(name, func(t *testing.T) {
			if gowork == "" {
				unsetenv(t, "GOWORK")
			} else {
				t.Setenv("GOWORK", gowork)
			}
			// The workspace is in effect for the caller, so a pass could only
			// be the checker's doing.
			cmd := exec.Command("go", "env", "GOWORK")
			cmd.Dir = dir
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if !samePath(t, strings.TrimSpace(string(out)), work) {
				t.Fatalf("go env GOWORK in %s is %q, want %s", dir, out, work)
			}

			r := check(t, goArgs(dir, ".")...)
			wantExit(t, r, 1)
			wantFail(t, r, failLine, "example.com/mpl", "MPL-2.0")
		})
	}
}

// samePath reports whether a and b name the same file once symbolic links
// are resolved, as on macOS, where the temporary directory is a link.
func samePath(t *testing.T, a, b string) bool {
	t.Helper()
	if a == "" || b == "" {
		return a == b
	}
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		t.Fatal(err)
	}
	return ra == rb
}

// A package that fails to load is a tool error, never skipped: a dropped
// package would pass silently. It exits 2 even when other packages fail.
func TestPackageLoadErrorExits2(t *testing.T) {
	mpl := licenseText(t, "MPL-2.0")

	t.Run("import no module provides", func(t *testing.T) {
		dir := fixture(t, map[string]string{
			"main.go": mainFile("example.com/mit", "example.com/nowhere/pkg"),
		},
			module{path: "example.com/mit", dir: "deps/mit", files: map[string]string{
				"LICENSE": licenseText(t, "MIT"), "mit.go": src("", "mit")}},
		)
		r := check(t, goArgs(dir, ".")...)
		wantExit(t, r, 2)
		if !mentions(r.out, "example.com/nowhere/pkg") {
			t.Errorf("the output does not name the package that failed to load:\n%s", r.out)
		}
	})

	t.Run("dependency imports a missing package", func(t *testing.T) {
		dir := fixture(t, map[string]string{
			"main.go": mainFile("example.com/broken", "example.com/mplfile"),
		},
			module{path: "example.com/broken", dir: "deps/broken", files: map[string]string{
				"LICENSE":   licenseText(t, "MIT"),
				"broken.go": src("", "broken", "example.com/broken/gone"),
			}},
			module{path: "example.com/mplfile", dir: "deps/mplfile", files: map[string]string{
				"LICENSE": mpl, "mplfile.go": src("", "mplfile")}},
		)
		r := check(t, goArgs(dir, ".")...)
		wantExit(t, r, 2)
		if !mentions(r.out, "example.com/broken/gone") {
			t.Errorf("the output does not name the package that failed to load:\n%s", r.out)
		}
	})
}
