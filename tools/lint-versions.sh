#!/usr/bin/env bash
# Fails if a pinned component's version literal appears outside the pin surface.
# The pin surface is build/versions.mk (binaries) + build/images.txt (images).
# Also fails on a workflow or composite action not pinned by commit SHA. Those
# pins live where they are used, since a uses: line cannot read versions.mk.
set -euo pipefail

cd "$(dirname "$0")/.."

MANIFEST=build/versions.mk
[[ -f $MANIFEST ]] || { echo "lint-versions: $MANIFEST missing" >&2; exit 1; }

# Exempt: the pin surface itself, prose (any markdown — documentation quotes
# versions by nature), generated output, and test fixtures.
is_allowed() {
  case "$1" in
    build/versions.mk|build/images.txt) return 0 ;;
    # Go module files: gomod versions are Renovate's to manage, grouped with
    # the binary pins (renovate.json5) — the lint's job is the repo's own text.
    go.mod|go.sum) return 0 ;;
    # A nested module's go.mod and go.sum likewise: tools/licensecheck is its
    # own module so its dependencies stay out of the root go.mod. Its versions
    # are Go module versions under the same gomod rules in renovate.json5, not
    # copies of a build/versions.mk pin.
    */go.mod|*/go.sum) return 0 ;;
    *.md|CHANGELOG*) return 0 ;;
    build-info.json|dist/*|staging/*|.stamps/*|*.spdx.json) return 0 ;;
    tools/testdata/*|tools/lint-versions.sh|.git/*) return 0 ;;
    # The apiboundary fixtures quote the released baseline they cover: the
    # released-consumer contract names the stratum release tag
    # (v$(kubernetes_version)-stratum.N) and the commit it was materialized at.
    # That is prose about a release, not a pin — the tag moves when
    # kubernetes_version moves, and tools/apiboundary's own tests (apidiff,
    # golden) keep the fixture honest, not this grep.
    tools/apiboundary/testdata/*) return 0 ;;
    # The one test that has to carry real pins, version and revision both: it
    # feeds fixture manifests to tools/lint-revisions.sh, which resolves the
    # tag upstream, so a made-up version or a made-up SHA would assert nothing.
    # Exempt from both checks rather than quarantined from one, and the
    # *_test.go comment below does not apply to it for the same reason.
    tools/test-revisions.sh) return 0 ;;
    # A nested checkout is not this repo's text. Git worktrees live under
    # .claude/worktrees/ when an agent is working in one, and recursing into a
    # second copy of build/versions.mk reports every pin as a duplicate of
    # itself — which fails `make lint` locally for a reason that has nothing to
    # do with the tree being linted. .opencode/ is the same kind of residue:
    # agent-session state that quotes repo text (and so the pins) into gitignored
    # logs the grep would otherwise walk. (Also excluded from the greps below.)
    .claude/*|.opencode/*) return 0 ;;
    # Generated bundles: the embedded manifests are generated from the pins
    # (tools/gen-cilium-manifest.sh, sentinel substitution) and
    # gen-cilium-manifest.sh asserts the cilium one matches its pin.
    internal/addons/manifests/*) return 0 ;;
    # The dashboard's npm dependency tree. Resolved by pnpm from
    # dash/package.json, not by us: the versions in it are npm packages that
    # happen to collide with our pins by coincidence — "containerd 2.3.5" here
    # is some unrelated package at 2.3.5. node_modules and vite's dist are not
    # ours to lint either. Renovate watches that tree on its own.
    # dash/.bundle/packages.json is the dash build's list of the npm packages
    # it bundled, for the license check: the same npm versions.
    dash/pnpm-lock.yaml|dash/node_modules/*|dash/dist/*|dash/.bundle/*) return 0 ;;
    # dash/package.json's `packageManager`. corepack reads a literal
    # from it and cannot expand $(pnpm_version), so the two are kept in step by
    # the assertion at the bottom of this script instead of by this grep.
    dash/package.json) return 0 ;;
    # Generated config whose rule descriptions cite the upstream release each
    # fence was derived from. The fences are ceilings, not pins, and
    # tools/gen-fences.sh --check is what keeps them honest.
    renovate.json5) return 0 ;;
    # A fixture table of candidate versions to accept and reject: its whole job
    # is to contain version literals that are deliberately not pins.
    tools/test-fences.sh) return 0 ;;
    # Go test files are test fixtures too (the exemption the header names): their
    # version literals are compare/upgrade test data — e.g. an upgrade preflight
    # needs a target version distinct from the cluster's, and a comparator needs
    # several distinct versions — not pins that track build/versions.mk. Any one
    # of them can collide with the pin the day kubernetes_version is bumped to a
    # number a fixture already used. Scoped to the version check ($2) only: a
    # pinned *revision* (a 40-char SHA) in a test is almost never a deliberate
    # fixture — it's an accidental copy of a real pin — so the revision loop must
    # keep enforcing on test files.
    *_test.go)
      [[ ${2:-} == version ]] && return 0
      return 1 ;;
    # The dashboard's tests are test fixtures for the same reason, in a
    # different language. A drift comparator needs two versions that
    # differ, and the node fixtures spell out a kubelet and the annotation
    # beside it; live.test.mjs carries a Kubernetes literal.
    # It passes this lint only while that literal happens not to be the
    # current pin — the collision the *_test.go comment above predicts. Scoped
    # to the version check for the same reason: a 40-char revision in a test is
    # an accidental copy of a real pin, not a fixture.
    dash/test/*.test.mjs|dash/test/harness.mjs)
      [[ ${2:-} == version ]] && return 0
      return 1 ;;
    *) return 1 ;;
  esac
}

# Every *_version value in the manifest becomes a forbidden literal elsewhere.
# Values shorter than 5 chars (e.g. alpine's "3.24") match far too much prose to
# police usefully, so they are reported as warnings, not failures.
fail=0 action_fail=0
while IFS='=' read -r key val; do
  key=${key// /}; val=${val// /}
  [[ -n $val ]] || continue
  name=${key%_version}

  while IFS= read -r hit; do
    file=${hit%%:*}
    is_allowed "$file" version && continue
    # An inttest's previous= header names the published release the check
    # downloads (v$(kubernetes_version)-stratum.N): a release tag, not a pin,
    # and substituting the pin would fetch a different release. Only that
    # header line is exempt; the same version anywhere else in the check is not.
    line=${hit#*:}; line=${line#*:}
    [[ $file == tools/inttest/check-*.sh && $line =~ ^\#\ ske-inttest:.*[[:space:]]previous= ]] && continue
    if [[ ${#val} -lt 5 ]]; then
      echo "lint-versions: WARN  $file pins $name literal '$val' (short version, verify by hand)"
    else
      echo "lint-versions: ERROR $file pins $name literal '$val' — use \$($key) from $MANIFEST"
      fail=1
    fi
  # site/ and .venv/ are docs's build output and its virtualenv, the
  # same kind of thing as dist/ and node_modules/. Both are gitignored, but this
  # grep walks the working tree rather than the index: after `make docs`, every
  # generated page carries mkdocs-material's own version in a <meta generator>
  # tag, which is a literal of a pin this manifest now owns.
  done < <(grep -rInE --exclude-dir=.git --exclude-dir=staging --exclude-dir=node_modules \
             --exclude-dir=dist --exclude-dir=.claude --exclude-dir=.opencode --exclude-dir=site --exclude-dir=.venv -- "(^|[^0-9.])$(printf '%s' "$val" | sed 's/[.]/\\./g')([^0-9.]|$)" . 2>/dev/null | sed 's|^\./||')
done < <(grep -E '^[a-z_-]+_version[[:space:]]*=' "$MANIFEST")

# Every pinned revision, likewise. Revisions are long enough to be unambiguous.
while IFS='=' read -r key val; do
  key=${key// /}; val=${val// /}
  [[ -n $val ]] || continue
  while IFS= read -r hit; do
    file=${hit%%:*}
    is_allowed "$file" revision && continue
    echo "lint-versions: ERROR $file pins $key literal — use \$($key) from $MANIFEST"
    fail=1
  done < <(grep -rIn --fixed-strings --exclude-dir=.git --exclude-dir=staging --exclude-dir=node_modules \
             --exclude-dir=dist --exclude-dir=.claude --exclude-dir=.opencode --exclude-dir=site --exclude-dir=.venv -- "$val" . 2>/dev/null | sed 's|^\./||')
done < <(grep -E '^[a-z_-]+_revision[[:space:]]*=' "$MANIFEST")

# The one pin that has to appear twice (see is_allowed): corepack's literal in
# dash/package.json must be the pnpm this manifest pins, or CI and a developer's
# tree resolve different pnpms from the same lockfile.
want="pnpm@$(sed -n 's/^pnpm_version *= *//p' "$MANIFEST" | tr -d ' ')"
got=$(sed -n 's/.*"packageManager"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' dash/package.json)
if [[ $got != "$want" ]]; then
  echo "lint-versions: ERROR dash/package.json packageManager is '${got:-unset}' — pnpm_version pins '$want'"
  fail=1
fi

# Every action a workflow or composite action runs is pinned by commit SHA (a
# tag can be moved under us), is a local ./ path, or is a docker:// image by
# digest. Renovate (helpers:pinGitHubActionDigests) keeps the SHAs current.
while IFS= read -r hit; do
  file=${hit%%:*}; rest=${hit#*:}
  line=${rest%%:*}; ref=${rest#*:}
  ref=${ref#*uses:}; ref=${ref%%#*}
  ref=$(printf '%s' "$ref" | tr -d " \t\"'")
  case $ref in
    ./*) continue ;;
    docker://*) [[ $ref =~ @sha256:[0-9a-f]{64}$ ]] && continue ;;
    *) [[ $ref =~ @[0-9a-f]{40}$ ]] && continue ;;
  esac
  echo "lint-versions: ERROR $file:$line uses '$ref' — pin it to a 40-char commit SHA (a docker:// image to its sha256 digest)"
  action_fail=1
done < <(grep -rsnE --include='*.yml' --include='*.yaml' '^[[:space:]]*(-[[:space:]]+)?uses:' .github/workflows || true
         grep -rsnE --include='action.yml' --include='action.yaml' '^[[:space:]]*(-[[:space:]]+)?uses:' .github/actions || true)

if [[ $fail -ne 0 ]]; then
  echo "lint-versions: FAILED — all versions belong in $MANIFEST" >&2
fi
if [[ $action_fail -ne 0 ]]; then
  echo "lint-versions: FAILED — pin each action by commit SHA where it is used, with its version in a trailing comment" >&2
fi
if [[ $fail -ne 0 || $action_fail -ne 0 ]]; then
  exit 1
fi
echo "lint-versions: ok"
