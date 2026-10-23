#!/usr/bin/env bash
# Fails if a pinned *_revision is not the commit that its *_version's upstream
# tag actually points at.
#
# The pair has to agree or the pin means nothing: build/components/*/Dockerfile
# clones `--branch v$VERSION` and then asserts `git rev-parse HEAD` equals
# $REVISION, so a version bumped without its revision fails the *component
# build* — the slowest, most expensive job in the gauntlet, and one that a
# release-branch push does not run at all. Renovate rewrites the revision
# only through the github-tags datasource — under github-releases it cannot
# resolve the digest and drops the bump outright — so that datasource is
# part of what this gate checks. Deliberately no version literals in
# this file: lint-versions.sh polices those outside the pin surface, and
# prose that names a number the manifest later pins would trip it.
#
# The upstream repo is not configured here: it is read from the
# `# renovate: datasource=github-tags depName=<owner>/<repo>` annotation
# above the pin's *_version, which is the same line Renovate reads. The
# annotation is bound to that one component: a revision with no annotation of
# its own is an error rather than inheriting the pin above it, and an error
# rather than a skip — a pin that cannot be checked is a pin that is not
# checked.
#
# Needs network.
set -euo pipefail
cd "$(dirname "$0")/.."

MANIFEST=${1:-build/versions.mk}
[[ -f $MANIFEST ]] || { echo "lint-revisions: $MANIFEST missing" >&2; exit 1; }

fail=0 checked=0 datasource="" dep="" owner="" ods="" odep=""

# A non-zero exit is retried; a successful one never is. A missing tag is a
# success (exit 0, no refs), so it still fails on the first call, and a host
# that stays unreachable still fails, just three attempts later. Retrying a
# transient failure is what keeps a flake from teaching people to rerun red lint
# without reading it. git cannot tell a flake from a permanent refusal (a
# mistyped depName is an auth failure, exit 128), so that is retried too: it
# costs seconds, and it still fails. No terminal prompt, or that mistyped repo
# asks a developer for credentials once per attempt. git's own stderr is left
# visible, so a real outage says why.
#
# No token here: in CI, actions/checkout persists the job's token as an
# `http.https://github.com/.extraheader` that the repository's config pulls in,
# and this script runs inside the repository, so these requests already carry
# it. Adding a second Authorization header would not help.
ls_remote() {  # ls_remote <url> <ref>...
  local n=1 tries=3 out
  while ! out=$(GIT_TERMINAL_PROMPT=0 git ls-remote "$@"); do
    [[ $n -lt $tries ]] || return 1
    echo "lint-revisions: ls-remote $1 failed (attempt $n/$tries), retrying" >&2
    sleep $((n * ${LINT_REVISIONS_BACKOFF:-5})); n=$((n + 1))
  done
  printf '%s\n' "$out"
}

# shellcheck disable=SC2094  # the loop body only READS $MANIFEST (sed -n); nothing writes it
while IFS= read -r line; do
  case $line in
    '# renovate:'*)
      datasource=${line##*datasource=}; datasource=${datasource%% *}
      case $line in
        *depName=*) dep=${line##*depName=}; dep=${dep%% *} ;;
        *)          dep="" ;;
      esac
      continue ;;
  esac

  # An annotation belongs to the one pin beneath it. Bound to that pin's name
  # here and cleared, rather than left standing as the loop's running state:
  # standing, the next pin that carries no annotation of its own inherits this
  # repository, and a pin whose version and SHA happen to coincide with one of
  # that repository's tags then passes — the exact case the fail-closed
  # contract exists for.
  if [[ $line =~ ^([a-z_-]+)_version[[:space:]]*= ]]; then
    owner=${BASH_REMATCH[1]} ods=$datasource odep=$dep
    datasource="" dep=""
    continue
  fi

  # Every revision assignment, not just a well-formed one. Anchoring the SHA
  # into the pattern that *selects* the line would make
  # `foo_revision = not-a-sha` a line the loop skips in silence, and a gate
  # that fails closed cannot have a spelling that opts out of it.
  [[ $line =~ ^([a-z_-]+)_revision[[:space:]]*=[[:space:]]*(.*)$ ]] || continue
  name=${BASH_REMATCH[1]} rhs=${BASH_REMATCH[2]}

  # `[^#]*`: a trailing `#` comment is Make syntax on this line too, and the
  # revision side accepts one, so the pair must read the same.
  ver=$(sed -n "s/^${name}_version[[:space:]]*=[[:space:]]*\\([^#]*\\).*/\\1/p" "$MANIFEST" | tr -d ' ')
  if [[ -z $ver ]]; then
    echo "lint-revisions: ERROR ${name}_revision has no ${name}_version to check it against"
    fail=1; continue
  fi

  # The whole right-hand side, not its first word: truncating at the first
  # space would reduce `<sha> garbage` to a SHA that matches upstream and pass
  # it, while Make keeps the trailing token and hands the Dockerfile a value
  # its own `git rev-parse HEAD` comparison rejects. A trailing `#` comment is
  # Make syntax rather than part of the value, so that much is allowed.
  if [[ $rhs =~ ^([0-9a-f]{40})[[:space:]]*(#.*)?$ ]]; then
    rev=${BASH_REMATCH[1]}
  else
    echo "lint-revisions: ERROR ${name}_revision is '${rhs}', not a 40-character commit SHA"
    fail=1; continue
  fi

  if [[ $owner != "$name" || $ods != github-tags || -z $odep ]]; then
    echo "lint-revisions: ERROR ${name}_revision: no 'datasource=github-tags depName=' annotation above ${name}_version, so its revision cannot be verified, and Renovate cannot bump it"
    fail=1; continue
  fi

  # Both tag spellings, and both peeled and unpeeled: most of these are
  # annotated tags, where the unpeeled ref is the tag object and not the commit
  # the Dockerfile's `git rev-parse HEAD` would see.
  url="https://github.com/${odep}.git"
  if ! refs=$(ls_remote "$url" \
                "refs/tags/v${ver}^{}" "refs/tags/v${ver}" \
                "refs/tags/${ver}^{}"  "refs/tags/${ver}"); then
    echo "lint-revisions: ERROR ${name}: cannot reach $url"
    fail=1; continue
  fi

  # One awk, not a grep|head pipeline: a lightweight tag (coredns ships them)
  # has no peeled ref, grep finds nothing, and under `pipefail` that failure
  # takes the whole lint down silently instead of checking the next pin.
  want=$(printf '%s\n' "$refs" | awk '
    /\^\{\}$/  { peeled = $1 }
    !plain      { plain = $1 }
    END         { print (peeled ? peeled : plain) }')

  if [[ -z $want ]]; then
    echo "lint-revisions: ERROR ${name} $ver: $odep has no tag v${ver} or ${ver}"
    fail=1; continue
  fi
  if [[ $want != "$rev" ]]; then
    echo "lint-revisions: ERROR ${name}_version is $ver, whose tag is ${want}, but ${name}_revision pins ${rev}"
    fail=1; continue
  fi
  checked=$((checked + 1))
done < "$MANIFEST"

if [[ $fail -ne 0 ]]; then
  echo "lint-revisions: FAILED — bump the revision with the version, or the component build will" >&2
  exit 1
fi
echo "lint-revisions: ok — $checked revisions match their version's upstream tag"
