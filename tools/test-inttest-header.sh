#!/usr/bin/env bash
# Proves the readers of the `# ske-inttest:` directive header agree.
#
# The header has two readers: tools/inttest-matrix.sh (all keys, and
# --directive for one key at a time) and .github/workflows/ci.yml (flaky,
# read with a sed of its own because the value must not reach the workflow
# through the matrix, which renames the job). tools/inttest-run.sh parses
# nothing: it asks inttest-matrix.sh --directive network, and this file
# asks --directive flaky.
#
# What this guards is that `network` stays a one-parser value — a
# reintroduced regex fails the refusal cases below. A second reader drifts
# silently: a truncating charset (`network=host2` -> the REAL network
# `host`), an empty value, a duplicate key, a key the generator rejects as
# unknown — every one of them runs a check on a different network from the
# one CI reported, and `network=NONE` would run check-offline.sh WITH a
# network, which is the single thing it exists to catch. `flaky` is still
# read separately by ci.yml, so that pair is checked for agreement too.
#
# Every reader here is EXTRACTED FROM ITS REAL FILE and evaluated. A test that
# reimplements the parser it guards stays green while the original drifts,
# which is the exact failure this test is about.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "test-inttest-header: FAIL  $*" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# The generator globs tools/inttest/check-*.sh relative to its own parent, so a
# throwaway tree exercises the MATRIX path on a fixture without a file landing
# in the repo. The --directive path is given the fixture directly.
mkdir -p "$tmp/tools/inttest"
cp tools/inttest-matrix.sh "$tmp/tools/"
fixture=$tmp/tools/inttest/check-probe.sh

# --- the readers, lifted from their use sites ---------------------------------

# tools/inttest-run.sh: the one line that resolves the network.
run_line=$(grep -m1 '^NETWORK=' tools/inttest-run.sh) \
  || fail "no NETWORK= line in tools/inttest-run.sh"

# ci.yml cannot carry markers and is not ours to edit here: editing a workflow
# is a separate permission from editing the rest of the tree. Anchor on the two
# lines themselves; if either stops matching, that IS the drift.
ci_hdr=$(grep -m1 -E "^[[:space:]]*hdr='" .github/workflows/ci.yml | sed 's/^[[:space:]]*//')
ci_flaky_line=$(grep -m1 -E '^[[:space:]]*flaky=\$\(sed' .github/workflows/ci.yml | sed 's/^[[:space:]]*//')
[[ -n $ci_hdr && -n $ci_flaky_line ]] \
  || fail "could not find the flaky reader in .github/workflows/ci.yml (hdr=/flaky= lines)"
# The pattern ci.yml holds a flaky value to before it grants a retry.
# shellcheck disable=SC2016  # the $flaky and \[\[ are literal text in ci.yml
ci_flaky_re=$(grep -m1 -E '^[[:space:]]*if \[\[ ! \$flaky =~ ' .github/workflows/ci.yml \
  | sed -E 's/^[[:space:]]*if \[\[ ! \$flaky =~ (.*) \]\]; then$/\1/')
[[ -n $ci_flaky_re ]] || fail "could not find the flaky pattern in .github/workflows/ci.yml"

gen_network() {  # the MATRIX path, through the real script
  # Captured rather than piped straight into python: when the generator
  # refuses a header it prints nothing, and a bare pipe turns that into a
  # JSONDecodeError traceback on top of whatever the real failure was.
  local out
  out=$("$tmp/tools/inttest-matrix.sh" 2>/dev/null) || return 1
  printf '%s' "$out" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["include"][0]["network"], end="")'
}
# `|| exit 1` rather than `set -e` inside these: bash SUPPRESSES set -e in a
# function called from a tested context, and every refusal case below calls
# these from exactly that (`run_network && fail ...`). Relying on set -e here
# made the refusal cases silently pass — the helper swallowed the error and
# returned an empty value with status 0.
run_network() {  # the real line out of tools/inttest-run.sh; subshell so a
                 # refusal is catchable rather than fatal here
  # shellcheck disable=SC2034  # SCRIPT is read by the eval'd line
  ( SCRIPT=$fixture; NETWORK=
    eval "$run_line" || exit 1
    [[ -n $NETWORK ]] || exit 1
    printf '%s' "$NETWORK" )
}
ci_flaky() {     # the real two lines out of .github/workflows/ci.yml
  # shellcheck disable=SC2034  # CHECK_SCRIPT is read by the eval'd lines
  ( CHECK_SCRIPT=$fixture; flaky=
    eval "$ci_hdr" || exit 1
    eval "$ci_flaky_line" || exit 1
    printf '%s' "$flaky" )
}
gen_flaky() {    # the REAL parser, via --directive; not a copy of its split loop
  tools/inttest-matrix.sh --directive flaky "$fixture"
}

hdr() { printf '#!/usr/bin/env bash\n# ske-inttest: %b\n' "$1" > "$fixture"; }

# --- headers the generator ACCEPTS: both paths must resolve the same ---------

net() {
  hdr "network=$1"
  local g r
  g=$(gen_network) || fail "generator refused network=$1, which this case assumes it accepts"
  r=$(run_network) || fail "inttest-run.sh refused network=$1, which the generator accepts"
  [[ $g == "$r" ]] || fail "network=$1: matrix says '$g', inttest-run.sh says '$r'"
}
net none
net bridge
net host
net NONE       # uppercase: the old [a-z]* dropped it and silently ran bridge
net host2      # digit: the old [a-z]* truncated to the REAL network `host`
net no-ne      # dash
net none.1     # dot
net some_thing # underscore

# --- headers the generator REFUSES: inttest-run.sh must refuse them too ------
# This is what catches a reintroduced second parser. A regex resolves these to
# something; only delegation refuses them.

refused() {
  printf '#!/usr/bin/env bash\n# ske-inttest: %b\n' "$1" > "$fixture"
  "$tmp/tools/inttest-matrix.sh" >/dev/null 2>&1 \
    && fail "generator ACCEPTED '$1' — this case assumes it refuses"
  local r
  if r=$(run_network 2>/dev/null); then
    fail "inttest-run.sh resolved '$1' to '$r'; the generator refuses that header"
  fi
}
refused 'network=host!'               # outside SAFE_VALUE
refused 'network=host#nope'
refused 'network=host;id'
refused 'network=host/../x'
refused 'network='                    # present but empty
refused 'network=none network=host'   # duplicate key
refused 'xnetwork=host'               # unknown key that ENDS in a known one
refused 'netowrk=none'                # plain typo
refused 'network=none\n# ske-inttest: network=host'  # duplicate across two lines

# --- whitespace, defaults, multi-line headers --------------------------------

# A trailing comment after a TAB ends the value for the generator (it splits on
# any whitespace), so it must end there for the other readers too.
hdr 'network=none\t# offline'
[[ $(gen_network) == "$(run_network)" ]] \
  || fail "tab-separated comment: matrix '$(gen_network)', inttest-run.sh '$(run_network)'"

# No directive at all, and a header carrying only some OTHER key, both default.
printf '#!/usr/bin/env bash\necho hi\n' > "$fixture"
[[ $(gen_network) == bridge && $(run_network) == bridge ]] \
  || fail "no directive should default to bridge: '$(gen_network)' / '$(run_network)'"
hdr 'pending=vip_failover'
[[ $(gen_network) == bridge && $(run_network) == bridge ]] \
  || fail "pending-only should default to bridge: '$(gen_network)' / '$(run_network)'"

# Directives spread over two header lines: the generator merges them, so a
# reader that stops at the first `# ske-inttest:` line would miss the second.
printf '#!/usr/bin/env bash\n# ske-inttest: flaky=until-2026-12-03\n# ske-inttest: network=none\n' > "$fixture"
[[ $(gen_network) == none ]] || fail "matrix missed network on the second line: $(gen_network)"
[[ $(run_network) == none ]] || fail "inttest-run.sh missed network on the second line: $(run_network)"

# --- flaky: ci.yml against the generator's splitter --------------------------
# Includes the whitespace case that was fixed by hand in ca7247d.

# $( ) strips the trailing newline from --directive, so the two are compared
# as the same kind of value. Only headers the generator ACCEPTS are compared:
# one it refuses aborts the matrix job, so ci.yml's reader never runs on it.
flak() {
  hdr "flaky=$1"
  local g c
  g=$(gen_flaky) || fail "generator refused flaky=$1, which this case assumes it accepts"
  c=$(ci_flaky)  || fail "ci.yml's reader errored on flaky=$1"
  [[ $g == "$c" ]] || fail "flaky=$1: generator says '$g', ci.yml says '$c'"
}
flak 'until-2026-12-03'
flak 'until-2026-12-03\t# flakes on arm64'
flak 'until-2026-12-03 # flakes on arm64'
flak 'until-2026-12-03\t'

# --- flaky: what ci.yml grants a retry to ------------------------------------
# An expiry and nothing else. The value is public, so it must not name a ticket
# in a tracker an outside reader cannot open; and the anchored pattern is also
# the sanitiser, since the value goes into a workflow command.
[[ until-2026-12-03 =~ $ci_flaky_re ]] \
  || fail "ci.yml refuses flaky=until-2026-12-03, the documented form"
for v in ticket-158-until-2026-12-03 yes until-2026-12-3 'until-2026-12-03;x' ''; do
  [[ $v =~ $ci_flaky_re ]] && fail "ci.yml accepts flaky=$v"
done

# --- previous: resolved by --directive, never emitted ------------------------
# ci.yml and inttest-run.sh ask --directive for it. It must not reach the
# matrix: a value there lands in the job name, and the job falls out of the
# branch rule's required-check list when the tag moves.

hdr 'previous=v0.0.1-stratum.0-rc.1'
[[ $(tools/inttest-matrix.sh --directive previous "$fixture") == v0.0.1-stratum.0-rc.1 ]] \
  || fail "--directive previous did not return the header's tag"
"$tmp/tools/inttest-matrix.sh" | grep -q previous \
  && fail "the matrix emitted the previous directive (it would rename the job)"
printf '#!/usr/bin/env bash\necho hi\n' > "$fixture"
[[ -z $(tools/inttest-matrix.sh --directive previous "$fixture") ]] \
  || fail "--directive previous is not empty for a check without the directive"

# --- the 20-line header window -----------------------------------------------
# The generator REFUSES a directive below it rather than ignoring it, so the
# others must not honour one either: a check that believes it is quarantined,
# or offline, and is not, is the whole failure mode.

pad_then() { { echo '#!/usr/bin/env bash'
               for i in $(seq 2 20); do echo "# pad $i"; done
               echo "# ske-inttest: $1"; } > "$fixture"; }

pad_then 'network=none'
"$tmp/tools/inttest-matrix.sh" >/dev/null 2>&1 \
  && fail "generator accepted a directive below the 20-line header"
run_network >/dev/null 2>&1 \
  && fail "inttest-run.sh honoured a directive below the header that the generator refuses"

pad_then 'flaky=until-2026-12-03'
[[ -z $(ci_flaky) ]] || fail "ci.yml honoured a flaky directive below the header: $(ci_flaky)"

# --- and the checks actually in the tree -------------------------------------

for f in tools/inttest/check-*.sh; do
  cp "$f" "$fixture"
  g=$(gen_network) || fail "$f: the generator refuses its own header"
  r=$(run_network) || fail "$f: inttest-run.sh refuses its own header"
  [[ $g == "$r" ]] || fail "$f: matrix says '$g', inttest-run.sh says '$r'"
done

echo "test-inttest-header: ok"
