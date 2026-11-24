package main

import (
	"path/filepath"
	"testing"
)

func writeNpmList(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"licenses.json": contents})
	return filepath.Join(dir, "licenses.json")
}

func TestNpmLicenses(t *testing.T) {
	list := writeNpmList(t, `[
  {"name": "left-pad", "version": "1.3.0", "license": "MIT"},
  {"name": "tslib", "version": "2.8.1", "license": "0BSD"},
  {"name": "@scope/dual", "version": "2.0.0", "license": "(MIT OR Apache-2.0)"},
  {"name": "either-one", "version": "1.0.0", "license": "MPL-2.0 OR ISC"},
  {"name": "both-allowed", "version": "1.0.0", "license": "BSD-3-Clause AND Zlib"},
  {"name": "mpl-thing", "version": "0.1.0", "license": "MPL-2.0"},
  {"name": "and-thing", "version": "1.0.0", "license": "MIT AND MPL-2.0"},
  {"name": "no-license", "version": "1.0.0", "license": ""},
  {"name": "gpl-with", "version": "1.0.0", "license": "GPL-2.0-only WITH Classpath-exception-2.0"},
  {"name": "apache-with", "version": "1.0.0", "license": "Apache-2.0 WITH LLVM-exception"},
  {"name": "apache-plus", "version": "1.0.0", "license": "Apache-2.0+"},
  {"name": "addition-ref", "version": "1.0.0", "license": "MIT WITH AdditionRef-extra-permission"},
  {"name": "license-as-exception", "version": "1.0.0", "license": "MIT WITH MPL-2.0"},
  {"name": "or-noassertion", "version": "1.0.0", "license": "MIT OR NOASSERTION"},
  {"name": "and-none", "version": "1.0.0", "license": "(MIT AND NONE)"},
  {"name": "no-assertion", "version": "1.0.0", "license": "NOASSERTION"},
  {"name": "none-declared", "version": "1.0.0", "license": "NONE"},
  {"name": "bare-bsd", "version": "1.0.0", "license": "BSD"},
  {"name": "spaced-apache", "version": "1.0.0", "license": "Apache 2.0"}
]`)
	r := check(t, "-npm", list)
	wantExit(t, r, 1)

	// A WITH exception only adds permissions; a trailing + includes the version.
	for _, pkg := range []string{"left-pad", "tslib", "@scope/dual", "either-one", "both-allowed", "apache-with", "apache-plus", "addition-ref"} {
		wantPass(t, r, pkg)
	}
	wantFail(t, r, failLine, "mpl-thing", "MPL-2.0")
	wantFail(t, r, failLine, "and-thing", "MPL-2.0")
	wantFail(t, r, failLine, "gpl-with", "GPL-2.0-only")
	// A well-formed id outside the allowlist fails, SPDX-defined or not.
	wantFail(t, r, failLine, "bare-bsd", "BSD")
	wantWarn(t, r, "no-license")
	wantWarn(t, r, "no-assertion")
	wantWarn(t, r, "none-declared")
	wantWarn(t, r, "spaced-apache")
	// Only an SPDX exception id or an AdditionRef may follow WITH.
	wantWarn(t, r, "license-as-exception")
	// NONE and NOASSERTION stand alone; combined they make it unreadable.
	wantWarn(t, r, "or-noassertion")
	wantWarn(t, r, "and-none")
	wantOnly(t, r, failLine, "mpl-thing", "and-thing", "gpl-with", "bare-bsd")
	wantOnly(t, r, warnLine, "no-license", "no-assertion", "none-declared", "spaced-apache", "license-as-exception", "or-noassertion", "and-none")
}

func TestNpmMissingLicenseOnlyWarns(t *testing.T) {
	list := writeNpmList(t, `[
  {"name": "left-pad", "version": "1.3.0", "license": "MIT"},
  {"name": "no-license", "version": "1.0.0", "license": ""},
  {"name": "absent-license", "version": "1.0.0"}
]`)
	r := check(t, "-npm", list)
	wantExit(t, r, 0)
	wantPass(t, r, "left-pad")
	wantWarn(t, r, "no-license")
	wantWarn(t, r, "absent-license")
	if got := r.lines(failLine); len(got) > 0 {
		t.Errorf("an unknown license failed the check: %q", got)
	}
}
