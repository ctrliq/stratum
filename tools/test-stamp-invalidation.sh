#!/usr/bin/env bash
# Guards that a component's build stamp carries its pinned version+revision,
# so a version bump invalidates it even when a `git checkout` leaves make with
# misleading mtimes. A bare .stamps/<os>-<arch>/<component> name does not: an
# mtime prerequisite on versions.mk silently fails across checkouts,
# `make components` skips the rebuild, and `make package` ships the
# previously built binary under the new version string.
#
# Hermetic and docker-free: it redirects STAMPS to a temp dir and reads the plan
# with `make -n`, so it asserts the invalidation BEHAVIOUR from a clean tree —
# which is exactly where the bug hides, since a clean CI checkout has no
# stale stamp to expose it. The plan is captured into a variable before grepping
# (not `make -n | grep -q`): grep -q closes the pipe on its first match and,
# under `set -o pipefail`, make's resulting SIGPIPE would flip the result.
set -euo pipefail
cd "$(dirname "$0")/.."

arch=$(go env GOARCH 2>/dev/null || echo amd64)
comp=etcd
ver=$(sed -n "s/^${comp}_version *= *//p" build/versions.mk)
rev=$(sed -n "s/^${comp}_revision *= *//p" build/versions.mk)
if [ -z "$ver" ] || [ -z "$rev" ]; then
	echo "test-stamp-invalidation: could not read ${comp}_version/${comp}_revision from build/versions.mk" >&2
	exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
plan() { make -C build/components -n "$comp" TARGET_ARCH="$arch" STAMPS="$tmp" 2>/dev/null; }

# 1. From an empty stamp dir the build targets a stamp named for the current pin.
p=$(plan)
grep -q "touch .*/${comp}-${ver}-${rev}\$" <<<"$p" || {
	echo "FAIL: the build plan does not target the version-stamped ${comp}-${ver}-${rev}" >&2
	echo "$p" >&2
	exit 1
}

# 2. That stamp then satisfies the build — no needless rebuild when nothing moved.
: >"$tmp/${comp}-${ver}-${rev}"
p=$(plan)
if grep -q "building ${comp}" <<<"$p"; then
	echo "FAIL: the current-pin stamp did not satisfy the build (would rebuild every time)" >&2
	exit 1
fi

# 3. THE GUARD: neither a bare-name stamp nor a stamp from a different
#    pin may satisfy the build. Fails if either one does.
rm -f "$tmp/${comp}-${ver}-${rev}"
: >"$tmp/${comp}"           # the non-versioned stamp name
: >"$tmp/${comp}-0.0.0-old" # a stale different-pin stamp
p=$(plan)
grep -q "building ${comp}" <<<"$p" || {
	echo "FAIL: a stale/old-pin stamp satisfied the build — regression: a pin change would ship the previously built binary" >&2
	exit 1
}

echo "ok: ${comp} build stamp is invalidated by a pin change"
