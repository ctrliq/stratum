#!/usr/bin/env bash
# Guards tools/lint-licenses.sh, the license gate that `make licenses` and CI
# run on the code stratum ships. It has to fail when a linked Go package or a
# bundled npm package carries a known license outside the allowlist, warn
# without failing on an unknown or missing license, fail when the npm list is
# missing, and never let one failing run hide the next.
#
# Hermetic: every case runs the gate on a throwaway Go module and npm list
# (through LICENSECHECK_DIR, LICENSECHECK_PKG and LICENSECHECK_NPM), never on
# this repo's ./cmd/stratum or dash/.bundle/packages.json. The fixture app
# replaces each dependency with a sibling directory, so each stays a separate
# module the checker checks, as it checks every module but the main one.
# Directory replaces need neither the network nor the module cache.
set -euo pipefail
cd "$(dirname "$0")/.."

GATE=$PWD/tools/lint-licenses.sh
pass=0
fail() { echo "test-lint-licenses: FAIL  $*" >&2; exit 1; }
ok()   { pass=$((pass + 1)); }

[ -x "$GATE" ] || fail "the gate $GATE is missing or not executable"
command -v go >/dev/null || fail "go is not installed; this test needs it"

# Under TMPDIR explicitly: a bare `mktemp -d` on macOS ignores TMPDIR, which a
# sandboxed run needs it to honour.
TEST_TMP=$(mktemp -d "${TMPDIR:-/tmp}/test-lint-licenses.XXXXXX")
trap 'rm -rf -- "$TEST_TMP"' EXIT

# --- the fixture ---------------------------------------------------------------
# dep NAME HEADER: module example.com/NAME in $TEST_TMP/NAME, one package whose
# only file starts with HEADER (an SPDX line, or nothing).
dep() {
  mkdir -p "$TEST_TMP/$1"
  printf 'module example.com/%s\n\ngo 1.22\n' "$1" > "$TEST_TMP/$1/go.mod"
  { [ -z "$2" ] || printf '%s\n\n' "$2"; printf 'package %s\n' "$1"; } > "$TEST_TMP/$1/$1.go"
}
dep mit '// SPDX-License-Identifier: MIT'
dep mpl '// SPDX-License-Identifier: MPL-2.0'
dep unknown ''   # license-shaped prose that matches no known license
cat > "$TEST_TMP/unknown/LICENSE" <<'EOF'
Permission is granted to anyone who asks the author nicely,
and to nobody else, on alternate Tuesdays only.
EOF
dep nolicense '' # no SPDX header and no license file up to its module root

APP=$TEST_TMP/app
mkdir -p "$APP/cmd/app"
cat > "$APP/go.mod" <<'EOF'
module example.com/app

go 1.22

require (
	example.com/mit v0.0.0
	example.com/mpl v0.0.0
	example.com/nolicense v0.0.0
	example.com/unknown v0.0.0
)

replace (
	example.com/mit => ../mit
	example.com/mpl => ../mpl
	example.com/nolicense => ../nolicense
	example.com/unknown => ../unknown
)
EOF

# app_imports PKG...: the app's main package blank-imports exactly PKG...
app_imports() {
  {
    printf 'package main\n'
    for p in "$@"; do printf '\nimport _ "%s"\n' "$p"; done
    printf '\nfunc main() {}\n'
  } > "$APP/cmd/app/main.go"
}

# go_file NAME CONSTRAINT: an extra file of the main package that imports the
# MPL-2.0 module, under a //go:build CONSTRAINT (or none) — so the build only
# links it where the constraint and the file name allow.
go_file() {
  { [ -z "$2" ] || printf '//go:build %s\n\n' "$2"; printf 'package main\n\nimport _ "example.com/mpl"\n'; } \
    > "$APP/cmd/app/$1"
}

NPM_CLEAN=$TEST_TMP/npm-clean.json
NPM_MPL=$TEST_TMP/npm-mpl.json
NPM_ABSENT=$TEST_TMP/absent/packages.json
printf '[{"name": "left-pad", "version": "1.3.0", "license": "MIT"}]\n' > "$NPM_CLEAN"
printf '[{"name": "left-pad", "version": "1.3.0", "license": "MIT"},
 {"name": "mpl-thing", "version": "0.1.0", "license": "MPL-2.0"}]\n' > "$NPM_MPL"

# Captured rather than piped: the gate exits non-zero precisely in the cases
# whose output we want to read, and set -e would stop the test at the capture.
# Every input is set on each run, so neither a developer's environment nor a
# `make lint TAGS=noui` (make exports command-line variables to recipes) can
# change what a case means; a case's own NAME=value arguments come later and
# win. GITHUB_ACTIONS is unset so findings print as plain FAIL and WARN lines,
# not workflow commands, when this runs in CI.
run() {
  out=$(env -u GITHUB_ACTIONS GOWORK=off TAGS= \
    LICENSECHECK_DIR="$APP" LICENSECHECK_PKG=./cmd/app LICENSECHECK_NPM="$NPM_CLEAN" \
    "$@" "$GATE" 2>&1) && rc=0 || rc=$?
}

# finding KIND NAME [TEXT]: some output line is a KIND (FAIL or WARN) finding
# for NAME that mentions TEXT. The checker prints "KIND path: reason" for a Go
# package and "KIND name@version: reason" for an npm one; matching through the
# ":" or "@" keeps example.com/mpl from matching a longer name.
finding() {
  local line
  while IFS= read -r line; do
    case $line in *"$1 $2"[:@]*"${3-}"*) return 0 ;; esac
  done <<<"$out"
  return 1
}

# --- a clean build and npm list pass --------------------------------------------
# Also the proof that the gate honours the three overrides: run on the repo's
# ./cmd/stratum or dash/.bundle/packages.json instead, it would fail or check
# something else.
app_imports example.com/mit
run
[ "$rc" -eq 0 ] || fail "a clean fixture failed (rc=$rc): $out"
if finding FAIL example.com/mit || finding WARN example.com/mit || finding FAIL left-pad; then
  fail "an MIT package was reported: $out"
fi
ok

# --- a stray VCS root above the fixture cannot fail the load -----------------------
# The check reads packages and licenses, never VCS metadata: its go list runs
# with -buildvcs=false, so a junk .git directory between the fixture and the
# filesystem root (a polluted TMPDIR, a container layer) must not fail the load.
# An empty .git is enough: the go command takes it for the VCS root and its git
# calls then fail.
mkdir "$TEST_TMP/.git"
app_imports example.com/mit
run
rmdir "$TEST_TMP/.git"
[ "$rc" -eq 0 ] || fail "a stray .git above the fixture failed the load (rc=$rc): $out"
ok

# --- the Go run builds with the caller's tags ------------------------------------
# The release build compiles the !noui files, so an import behind !noui is
# checked; only TAGS=noui leaves it out, as that build does.
go_file ui.go '!noui'
run
[ "$rc" -ne 0 ] || fail "an MPL-2.0 import behind !noui passed: the default run must check the !noui build (rc=$rc): $out"
finding FAIL example.com/mpl MPL-2.0 || fail "no FAIL line names example.com/mpl behind !noui: $out"
run TAGS=noui
[ "$rc" -eq 0 ] || fail "TAGS=noui checked an import its build leaves out (rc=$rc): $out"
rm "$APP/cmd/app/ui.go"
ok

# --- an MPL-2.0 Go dependency fails -------------------------------------------------
app_imports example.com/mit example.com/mpl
run
[ "$rc" -ne 0 ] || fail "an MPL-2.0 Go dependency passed: $out"
finding FAIL example.com/mpl MPL-2.0 || fail "no FAIL line names example.com/mpl and MPL-2.0: $out"
ok

# --- both shipped architectures are checked ---------------------------------------
# make build ships linux/amd64 and linux/arm64; a dependency only one of them
# links is still shipped. The _linux_<arch>.go name is the only constraint.
app_imports example.com/mit
for arch in amd64 arm64; do
  go_file "only_linux_$arch.go" ''
  run
  [ "$rc" -ne 0 ] || fail "an MPL-2.0 dependency linked only on linux/$arch passed: $out"
  finding FAIL example.com/mpl MPL-2.0 || fail "no FAIL line names example.com/mpl, linked only on linux/$arch: $out"
  rm "$APP/cmd/app/only_linux_$arch.go"
done
ok

# --- TAGS reach the Go run --------------------------------------------------------
# The same file passes without the tag, so the failure is the tag's doing.
go_file tagged.go licfixture
run
[ "$rc" -eq 0 ] || fail "an import behind an unset build tag was checked (rc=$rc): $out"
run TAGS=licfixture
[ "$rc" -ne 0 ] || fail "TAGS=licfixture did not reach the Go run: the MPL-2.0 import behind it passed: $out"
finding FAIL example.com/mpl MPL-2.0 || fail "no FAIL line names example.com/mpl under TAGS=licfixture: $out"
rm "$APP/cmd/app/tagged.go"
ok

# --- an unknown or missing license warns and passes --------------------------------
app_imports example.com/mit example.com/unknown example.com/nolicense
run
[ "$rc" -eq 0 ] || fail "an unknown or missing license failed the gate (rc=$rc): $out"
finding WARN example.com/unknown || fail "no WARN line names example.com/unknown (unknown license text): $out"
finding WARN example.com/nolicense || fail "no WARN line names example.com/nolicense (no license at all): $out"
if finding FAIL example.com/unknown || finding FAIL example.com/nolicense; then
  fail "an unknown or missing license printed a FAIL line: $out"
fi
ok

# --- a package that does not load fails ---------------------------------------------
# The checker exits 2 when it cannot load the build; a gate that passed on
# anything but exit 1 would let a dropped package through silently.
app_imports example.com/mit example.com/mit/nosuchpkg
run
[ "$rc" -ne 0 ] || fail "the gate passed a build whose packages do not load: $out"
ok

# --- an MPL-2.0 npm entry fails -----------------------------------------------------
app_imports example.com/mit
run LICENSECHECK_NPM="$NPM_MPL"
[ "$rc" -ne 0 ] || fail "an MPL-2.0 npm package passed: $out"
finding FAIL mpl-thing MPL-2.0 || fail "no FAIL line names mpl-thing and MPL-2.0: $out"
if finding FAIL left-pad; then fail "the MIT npm package failed: $out"; fi
ok

# --- a failing run does not stop the next one ---------------------------------------
# Stopping at the Go failure would hide the npm finding until the Go one is
# fixed, and CI would report half the problem.
app_imports example.com/mit example.com/mpl
run LICENSECHECK_NPM="$NPM_MPL"
[ "$rc" -ne 0 ] || fail "two MPL-2.0 findings passed: $out"
finding FAIL example.com/mpl MPL-2.0 || fail "no FAIL line names example.com/mpl: $out"
finding FAIL mpl-thing MPL-2.0 || fail "the npm run did not execute after the Go run failed: $out"
ok

# --- a missing npm list fails ---------------------------------------------------------
# The dash build writes the list. Without it no bundled package is checked, so
# a pass would mean nothing; the failure has to say which file is missing.
app_imports example.com/mit
run LICENSECHECK_NPM="$NPM_ABSENT"
[ "$rc" -ne 0 ] || fail "a missing npm list passed: $out"
case $out in *absent/packages.json*) ;; *) fail "the missing-list failure does not name the file: $out" ;; esac
ok

# --- TAGS containing noui skips the npm run -------------------------------------------
# A TAGS=noui build embeds no dashboard, so there is no npm list to check. As
# with the Makefile's NEEDS_DASH, noui counts anywhere in a comma-separated
# TAGS.
for tags in noui extra,noui; do
  run TAGS="$tags" LICENSECHECK_NPM="$NPM_ABSENT"
  [ "$rc" -eq 0 ] || fail "TAGS=$tags did not skip the npm run (rc=$rc): $out"
done
ok

echo "test-lint-licenses: ok ($pass cases)"
