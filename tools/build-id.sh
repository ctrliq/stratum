#!/usr/bin/env bash
# Prints the build identity stamped into stratum as version.BuildID:
#
#   <commit>[-dirty-<hash>]-in-<hash>
#
# The commit the binary was built from; a hash of anything in the tree that is
# not that commit -- changes to tracked files, and untracked files that are not
# ignored, since Go compiles an untracked .go file into its package just the
# same; and a hash of the build's other inputs that git does not see: the build
# tags (the first argument, e.g. `noui`) and the dashboard assets in dash/dist,
# ignored by git but embedded in the binary (not under `noui`, which leaves them out). All of it is the same on every
# architecture, so a cluster upgrade can pin every node to one build even
# where their bytes differ. The release stamp (tools/version.sh
# --stamp) cannot: every untagged commit before the next release shares it.
#
# What it cannot see is the per-architecture payload appended to the binary:
# that differs by architecture by design, and a cluster upgrade pins nodes of
# the orchestrator's own architecture to its exact bytes instead.
set -euo pipefail
cd "$(dirname "$0")/.."
id=$(git rev-parse HEAD 2>/dev/null) || { echo unknown; exit 0; }
# The tags as one sorted, de-duplicated, space-separated list: Go reads
# `a,b`, `a b` and `b,a,a` as one set of tags, so they are one build.
tags=${1:-}
read -ra tagv <<<"${tags//,/ }"
tags=$(printf '%s\n' "${tagv[@]:-}" | sed '/^$/d' | sort -u | paste -sd' ' -)
# go_inputs prints, one per line and relative to the repository, every file
# in it that the build compiles, assembles, links or embeds for these tags, as
# the Go toolchain itself resolves them. Files outside the repository come
# from the module cache and are pinned by go.sum; dash/dist is hashed on its
# own below. BUILD_ID_PKG names the main package; a test points it elsewhere.
go_inputs() {
  local root
  # The module root as go list spells it: package dirs come back in that form,
  # which on macOS is /var/... where pwd -P says /private/var/...
  root=$(go list -m -f '{{.Dir}}' 2>/dev/null) || return 1
  # shellcheck disable=SC2016 # a Go template, not shell
  go list -tags "${tags// /,}" -deps -f '{{if not .Standard}}{{$d := .Dir}}{{range .GoFiles}}{{$d}}/{{.}}
{{end}}{{range .CgoFiles}}{{$d}}/{{.}}
{{end}}{{range .CFiles}}{{$d}}/{{.}}
{{end}}{{range .CXXFiles}}{{$d}}/{{.}}
{{end}}{{range .HFiles}}{{$d}}/{{.}}
{{end}}{{range .SFiles}}{{$d}}/{{.}}
{{end}}{{range .SysoFiles}}{{$d}}/{{.}}
{{end}}{{range .EmbedFiles}}{{$d}}/{{.}}
{{end}}{{end}}' "${BUILD_ID_PKG:-./cmd/stratum}" 2>/dev/null |
    sed -n "s|^$root/||p" | grep -v '^dash/dist/' || true
}
changes() {
  git diff HEAD --
  # Each untracked file by name and content, in a stable order: the ones git
  # does not ignore, and every build input it does. The Go toolchain does not
  # read .gitignore, so an ignored .go, .s or .syso file in a package -- or an
  # embedded asset -- goes into the binary all the same. Which
  # files those are comes from go list, not a list of extensions. If go list
  # cannot run, the Go source and module files stand in. Over-counting is the
  # safe direction: a stray file makes a mismatch the rollout refuses, never a
  # match it should not.
  {
    git ls-files -z --others --exclude-standard
    if inputs=$(go_inputs) && [[ -n $inputs ]]; then
      grep -Fxv -f <(git ls-files) <<<"$inputs" | tr '\n' '\0' || true
    else
      git ls-files -z --others --ignored --exclude-standard -- \
        '*.go' 'go.mod' 'go.sum' 'go.work' 'go.work.sum' ':!:dash/node_modules'
    fi
  } | sort -zu | while IFS= read -r -d '' f; do
    printf 'untracked %s %s\n' "$f" "$(git hash-object -- "$f")"
  done
}
if [[ -n $(changes | head -c1) ]]; then
  id="$id-dirty-$(changes | git hash-object --stdin | cut -c1-12)"
fi
# The payload mode (EMBEDDED_BINS; empty means the default, all). The payload
# is appended after linking, so its bytes cannot be hashed here, but what goes
# into it is pinned by tracked files the commit already covers -- except
# whether there is one at all.
payload=${2:-all}
inputs() {
  printf 'tags %s\n' "$tags"
  printf 'payload %s\n' "$payload"
  # Under noui the dashboard is not embedded, so its files are not an input.
  if [[ -d dash/dist && " $tags " != *" noui "* ]]; then
    find dash/dist -type f -print0 | sort -z | while IFS= read -r -d '' f; do
      printf 'dist %s %s\n' "$f" "$(git hash-object --no-filters -- "$f")"
    done
  fi
}
echo "$id-in-$(inputs | git hash-object --stdin | cut -c1-12)"
