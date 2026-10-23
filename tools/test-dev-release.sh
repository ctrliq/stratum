#!/usr/bin/env bash
# Deterministic coverage for dev-release.sh's tag derivation and its two
# refusals. No network, no docker, no gh: everything here is pure logic, which
# is exactly the part that must not regress silently.
set -euo pipefail
cd "$(dirname "$0")/.."

# Git hooks export repository-local variables. Clear them before creating the
# fixture repo so ref creation and tag derivation use the fixture's own .git.
git_local_vars=$(git rev-parse --local-env-vars)
while IFS= read -r name; do
  unset "$name"
done <<< "$git_local_vars"

SCRIPT=tools/dev-release.sh
pass=0
fail() { echo "test-dev-release: FAIL  $*" >&2; exit 1; }
ok()   { pass=$((pass + 1)); }

TEST_TMP=$(mktemp -d)
trap 'rm -rf -- "$TEST_TMP"' EXIT

# --- tag derivation ----------------------------------------------------------
# A throwaway repo, so the assertions are about the derivation and not about
# whatever refs this checkout happens to have.
# The script resolves its own repo root with `cd $(dirname $0)/..`, so the copy
# under test has to live in the fixture repo — which also exercises that
# resolution rather than assuming it.
repo=$TEST_TMP/repo
mkdir -p "$repo/tools"
cp "$SCRIPT" "$repo/tools/"
UNDER_TEST=$repo/tools/$(basename "$SCRIPT")
git init -q "$repo"
git -C "$repo" -c user.email=t@t -c user.name=t commit -q --allow-empty -m init
sha=$(git -C "$repo" rev-parse --short=7 HEAD)
today=$(date -u +%Y%m%d)

derive() {  # derive <ref> [env assignments...]
  env "${@:2}" "$UNDER_TEST" --print-tag --ref "$1"
}

for br in main feat/dev-release Feature/MixedCase weird@ref!name wip@@fix trail@; do
  git -C "$repo" branch -f -- "$br" HEAD 2>/dev/null || continue
done

got=$(derive main)
[[ $got == "dev-main-$today-$sha" ]] || fail "plain ref: got '$got'"
ok

# Slashes are the common case and are not legal in a Docker tag.
got=$(derive feat/dev-release)
[[ $got == "dev-feat-dev-release-$today-$sha" ]] || fail "slashed ref: got '$got'"
ok

# Docker tags are case-sensitive but lower-case is the convention; more
# importantly the same ref must always produce the same tag.
got=$(derive Feature/MixedCase)
[[ $got == "dev-feature-mixedcase-$today-$sha" ]] || fail "mixed case: got '$got'"
ok

# Anything outside the Docker tag charset folds to '-'.
got=$(derive 'weird@ref!name')
[[ $got == "dev-weird-ref-name-$today-$sha" ]] || fail "odd characters: got '$got'"
ok

# ADJACENT bad characters must collapse to a single '-'. Without a case whose
# folded form contains a run, dropping the collapse from the sed is invisible:
# every other ref here has its invalid characters separated by valid ones.
got=$(derive 'wip@@fix')
[[ $got == "dev-wip-fix-$today-$sha" ]] || fail "adjacent bad characters: got '$got'"
ok

# A trailing bad character folds to '-' and must then be trimmed, or the slug
# ends in a dash and reads as a typo in every release title.
got=$(derive 'trail@')
[[ $got == "dev-trail-$today-$sha" ]] || fail "trailing bad character: got '$got'"
ok

# Every derived tag must be a legal Docker tag: [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}
for ref in main feat/dev-release Feature/MixedCase 'weird@ref!name' 'wip@@fix' 'trail@'; do
  t=$(derive "$ref")
  [[ $t =~ ^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$ ]] || fail "not a legal docker tag: '$t'"
done
ok

# --- refusal 1: the v* trigger ------------------------------------------------
# release.yml triggers on `tags: ['v*']`. A dev tag that matched would fire the
# real release pipeline and then fail at `make check-version`. The prefix makes
# this unreachable today; the guard exists so changing the prefix cannot make it
# reachable silently, and this asserts the guard actually fires.
if out=$(derive main DEV_PREFIX=v 2>&1); then
  fail "a v-prefixed tag was accepted: '$out'"
fi
grep -q "matches release.yml's v\* trigger" <<<"$out" \
  || fail "wrong error for a v-prefixed tag: '$out'"
ok

# The real prefix must never produce one, for any ref.
for ref in main feat/dev-release 'weird@ref!name'; do
  t=$(derive "$ref")
  [[ $t != v* ]] || fail "derived a v* tag from '$ref': '$t'"
done
ok

# --- refusal 2: a nonexistent ref --------------------------------------------
if out=$("$UNDER_TEST" --print-tag --ref no-such-branch 2>&1); then
  fail "a nonexistent ref was accepted: '$out'"
fi
grep -q "no such ref" <<<"$out" || fail "wrong error for a bad ref: '$out'"
ok

# --- argument validation -----------------------------------------------------
if out=$("$UNDER_TEST" --arch riscv64 --print-tag 2>&1); then
  fail "an unsupported --arch was accepted: '$out'"
fi
grep -q "amd64 or arm64" <<<"$out" || fail "wrong error for a bad --arch: '$out'"
ok

if out=$("$UNDER_TEST" --nonsense 2>&1); then
  fail "an unknown argument was accepted: '$out'"
fi
ok

# --- refusal 3: local build for a foreign architecture ------------------------
# The one that produces a plausible-looking useless artifact if it is ever
# downgraded to a warning: components build with --platform and
# emulation is forbidden, so a foreign-arch local build is not a build of
# the thing anyone meant to test. A docker shim reports amd64; ask for arm64.
shim=$TEST_TMP/bin
mkdir -p "$shim"
cat > "$shim/docker" <<'SHIM'
#!/usr/bin/env bash
case "${1:-}" in
  version) echo amd64 ;;
  *) exit 0 ;;
esac
SHIM
chmod +x "$shim/docker"

# A `make` shim records any invocation. Asserting only "non-zero exit and the
# message appeared" is NOT enough: downgrading the die to a warning still exits
# non-zero, because the build it then wrongly starts fails a moment later. The
# marker is what distinguishes refusing from failing.
cat > "$shim/make" <<SHIM
#!/usr/bin/env bash
echo "\$@" >> "$TEST_TMP/make-was-called"
exit 1
SHIM
chmod +x "$shim/make"
rm -f "$TEST_TMP/make-was-called"

if out=$(PATH="$shim:$PATH" "$UNDER_TEST" --local --arch arm64 --ref main 2>&1); then
  fail "a cross-arch local build was accepted: '$out'"
fi
grep -q "cannot build arm64 locally" <<<"$out" \
  || fail "wrong error for a cross-arch local build: '$out'"
grep -q "no emulation" <<<"$out" || fail "the refusal should cite why: '$out'"
[[ ! -f $TEST_TMP/make-was-called ]] \
  || fail "refused but still started the build: $(cat "$TEST_TMP/make-was-called")"
ok

# The mirror image of the above: a matching arch must actually reach the build,
# or the refusal test above would pass for a script that refuses everything.
rm -f "$TEST_TMP/make-was-called"
PATH="$shim:$PATH" "$UNDER_TEST" --local --arch amd64 --ref main >/dev/null 2>&1 || true
grep -q "^components" "$TEST_TMP/make-was-called" 2>/dev/null \
  || fail "a same-arch local build did not reach 'make components'"
ok

# --- the derivation exists twice, so assert the copies agree --------------------
# dev-release.yml's meta job re-implements the slug inline, for a dispatch made
# from the GitHub UI with no tag input. Two copies of one rule drift silently, so
# compare the rules themselves rather than trusting a comment to keep them in step.
WF=.github/workflows/dev-release.yml
[[ -f $WF ]] || fail "$WF is missing"

extract_rule() {  # the sed program that folds a ref into a slug
  grep -o "sed 's#\[\^a-z0-9\._-\]#-#g;[^']*'" "$1" | head -1
}
script_rule=$(extract_rule "$SCRIPT")
wf_rule=$(extract_rule "$WF")
[[ -n $script_rule ]] || fail "could not find the slug rule in $SCRIPT"
[[ -n $wf_rule ]]     || fail "could not find the slug rule in $WF"
[[ $script_rule == "$wf_rule" ]] || fail "the slug rule has drifted:
  $SCRIPT: $script_rule
  $WF: $wf_rule"
ok

# Both must also strip the same ref prefixes, or a refs/heads/x dispatch from the
# UI would tag differently from the same dispatch made by the script.
for f in "$SCRIPT" "$WF"; do
  grep -q "s#\^refs/heads/##; s#\^refs/tags/##" "$f" \
    || fail "$f does not strip refs/heads and refs/tags the same way"
done
ok

# The workflow must carry the same v* refusal the script does.
grep -q "matches release.yml's v\* tag trigger" "$WF" \
  || fail "$WF is missing the v* tag refusal"
ok

echo "test-dev-release: ok ($pass checks)"
