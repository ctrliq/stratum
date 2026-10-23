#!/usr/bin/env bash
# The license gate: checks the third-party code stratum ships against the
# license allowlist with tools/licensecheck. `make licenses` and CI's
# `licenses` job run it. Two runs, and both always execute, so one failure
# never hides the other's findings:
#
#   1. Go: every package linked into ./cmd/stratum, for linux/amd64 and
#      linux/arm64 as `make build` compiles it, with the same TAGS. Without
#      noui that build embeds dash/dist, so go list needs it built first (make
#      licenses does); with TAGS=noui it is the build without a dashboard.
#   2. npm: the packages vite bundled into dash/dist, which the dash build
#      lists in dash/.bundle/packages.json. Skipped when TAGS contains noui,
#      since that binary embeds no dashboard; otherwise a missing list fails,
#      because nothing bundled would be checked.
#
# A known license outside the allowlist fails; an unknown or missing one prints
# a WARN line and passes. The policy and the exceptions live in LICENSING.md
# and tools/licensecheck.
#
# Overrides (relative paths are relative to the repository root):
#   LICENSECHECK_DIR  module directory for the Go run (default: the repo root)
#   LICENSECHECK_PKG  package to check (default: ./cmd/stratum)
#   LICENSECHECK_NPM  npm package list (default: dash/.bundle/packages.json)
#   TAGS              extra build tags, comma-separated, as the Makefile's TAGS
set -euo pipefail
cd "$(dirname "$0")/.."

dir=${LICENSECHECK_DIR:-.}
pkg=${LICENSECHECK_PKG:-./cmd/stratum}
npm_list=${LICENSECHECK_NPM:-dash/.bundle/packages.json}

# TAGS is comma-separated like `go build -tags`; whitespace is folded to commas
# so a space-separated value means the same. As with the Makefile's NEEDS_DASH,
# noui anywhere in it means the binary has no dashboard.
tags=${TAGS-}
tags=${tags//[[:space:]]/,}
# Squeeze empty elements, so ` noui ` reads as `noui`, not `,noui,`.
tags=$(printf '%s' "$tags" | tr -s ',')
tags=${tags#,}
tags=${tags%,}
case ",$tags," in
  *,noui,*) check_npm=0 ;;
  *) check_npm=1 ;;
esac

# Under TMPDIR explicitly: a bare `mktemp -d` on macOS ignores TMPDIR.
tmp=$(mktemp -d "${TMPDIR:-/tmp}/lint-licenses.XXXXXX")
trap 'rm -rf -- "$tmp"' EXIT

# Built once, not `go run` per check: tools/licensecheck is its own module, so
# its dependencies stay out of stratum's go.mod, and one build serves both runs.
bin=$tmp/licensecheck
# For the host, whatever GOOS and GOARCH the caller exported: the binary runs
# here, and the platforms it checks are its own -goos and -goarch flags.
(cd tools/licensecheck && GOOS="$(go env GOHOSTOS)" GOARCH="$(go env GOHOSTARCH)" go build -o "$bin" .) \
  || { echo "lint-licenses: building tools/licensecheck failed" >&2; exit 1; }

# Each run records its failure here instead of exiting, so the next one runs.
# licensecheck exits 1 on a FAIL finding and 2 when it cannot check at all
# (a package that does not load, unreadable input); both fail the gate.
failed=

echo "lint-licenses: Go packages linked into $pkg (linux/amd64,arm64, tags ${tags:-none})" >&2
"$bin" -C "$dir" -goos linux -goarch amd64,arm64 -tags "$tags" "$pkg" \
  || failed+="${failed:+, }Go (licensecheck exit $?)"

if [ "$check_npm" -eq 0 ]; then
  echo "lint-licenses: npm check skipped: TAGS=$tags builds no dashboard" >&2
elif [ ! -f "$npm_list" ]; then
  echo "lint-licenses: $npm_list is missing; build the dashboard (make dash) so its bundled npm packages can be checked" >&2
  failed+="${failed:+, }npm (no package list)"
else
  echo "lint-licenses: npm packages bundled into dash/dist ($npm_list)" >&2
  "$bin" -npm "$npm_list" || failed+="${failed:+, }npm (licensecheck exit $?)"
fi

if [ -n "$failed" ]; then
  echo "lint-licenses: FAILED: $failed" >&2
  exit 1
fi
echo "lint-licenses: ok" >&2
