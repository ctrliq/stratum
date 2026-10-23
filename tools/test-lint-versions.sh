#!/usr/bin/env bash
# Asserts tools/lint-versions.sh refuses a workflow action that is not pinned
# by commit SHA, and accepts a pinned one, a local ./ action, and a docker://
# image pinned by digest.
#
# Hermetic: each case runs a copy of the real script inside a throwaway tree
# (the script cds to its own parent), so the repo's own workflows are not what
# is being asserted on.
set -euo pipefail
cd "$(dirname "$0")/.."

WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT
fail=0

TREE=$WORK/tree
mkdir -p "$TREE/tools" "$TREE/build" "$TREE/dash" "$TREE/.github/workflows" "$TREE/.github/actions/prep"
cp tools/lint-versions.sh "$TREE/tools/"
printf 'pnpm_version = 10.0.0\n' > "$TREE/build/versions.mk"
printf '{ "packageManager": "pnpm@10.0.0" }\n' > "$TREE/dash/package.json"

SHA=0123456789abcdef0123456789abcdef01234567
DIGEST=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef

expect() {  # description, want_exit, file, contents, [want_output]
  local what=$1 want=$2 file=$3 body=$4 grep_for=${5:-} got=0
  rm -f "$TREE"/.github/workflows/* "$TREE"/.github/actions/prep/*
  printf '%s\n' "$body" > "$TREE/$file"
  "$TREE/tools/lint-versions.sh" >"$WORK/out" 2>&1 || got=$?
  if [[ $got -eq $want ]] && { [[ -z $grep_for ]] || grep -qF -- "$grep_for" "$WORK/out"; }; then
    printf '  ok    %s\n' "$what"
  else
    printf '  FAIL  %s: want exit %s, got %s\n' "$what" "$want" "$got" >&2
    sed 's/^/        /' "$WORK/out" >&2
    fail=1
  fi
}

WF=.github/workflows/ci.yml
expect "a tag-pinned action is refused, by file:line" 1 $WF "jobs:
  x:
    steps:
      - run: true
      - uses: some/action@v1" "$WF:5 uses 'some/action@v1'"
if grep -qF "belong in build/versions.mk" "$WORK/out"; then
  printf '  FAIL  %s\n' "an unpinned action is sent to build/versions.mk" >&2
  fail=1
else
  printf '  ok    %s\n' "an unpinned action is not sent to build/versions.mk"
fi
expect "an unpinned action is refused, and told to pin by SHA" 1 $WF "      - uses: some/action" "pin each action by commit SHA where it is used"
expect "a quoted tag in a .yaml workflow is refused" 1 .github/workflows/x.yaml '      - uses: "some/action@main"'
expect "a reusable workflow on a tag is refused" 1 $WF "    uses: org/repo/.github/workflows/w.yml@v2"
expect "a short SHA is refused" 1 $WF "      - uses: some/action@${SHA:0:12}"
expect "a docker:// image by tag is refused" 1 $WF "      - uses: docker://alpine:3.20"
expect "a composite action's tag is refused" 1 .github/actions/prep/action.yml "    - uses: some/action@v1"
expect "a SHA-pinned action is accepted" 0 $WF "      - uses: some/action@$SHA # v1.2.3"
expect "a SHA-pinned reusable workflow is accepted" 0 $WF "    uses: org/repo/.github/workflows/w.yml@$SHA"
expect "a local ./ action is accepted" 0 $WF "      - uses: ./.github/actions/prep"
expect "a docker:// image by digest is accepted" 0 $WF "      - uses: docker://alpine@$DIGEST"
expect "a SHA-pinned composite step is accepted" 0 .github/actions/prep/action.yml "    - uses: 'some/action@$SHA'"

mkdir -p "$TREE/tools/inttest"
CHECK=tools/inttest/check-x.sh
expect "a release tag in an inttest's previous= header is accepted" 0 $CHECK '#!/usr/bin/env bash
# ske-inttest: previous=v10.0.0-stratum.0-rc.1'
expect "the same version elsewhere in that check is refused" 1 $CHECK '#!/usr/bin/env bash
# ske-inttest: previous=v10.0.0-stratum.0-rc.1
want=10.0.0' "$CHECK pins pnpm literal '10.0.0'"
rm -f "$TREE/$CHECK"
expect "a previous= header outside tools/inttest is refused" 1 tools/x.sh '# ske-inttest: previous=v10.0.0-stratum.0-rc.1'
rm -f "$TREE/$CHECK" "$TREE/tools/x.sh"

if [[ $fail -ne 0 ]]; then
  echo "test-lint-versions: FAILED" >&2
  exit 1
fi
echo "test-lint-versions: ok"
