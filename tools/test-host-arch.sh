#!/usr/bin/env bash
# Guards HOST_ARCH surviving an unreachable docker daemon.
#
# HOST_ARCH is computed in two places, Makefile and build/components/Makefile:
#
#   HOST_ARCH := $(shell docker version --format '{{.Server.Arch}}' 2>/dev/null \
#                        || uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
#
# An unreachable daemon writes a blank line to STDOUT *and* exits non-zero, so
# `||` fires and the uname fallback runs too — both outputs land in the variable.
# $(shell) folds the newline to a space and HOST_ARCH becomes " arm64", which
# make then treats as TWO words: STAGING = `staging/linux- arm64`, the image tar
# and the zip get a space in their names, and the component build declares a
# target literally named `.stamps/linux-`. Nothing errors; the build just puts
# its output somewhere nobody looks.
#
# Hermetic: no real docker, no network, nothing built. A `docker` shim earlier on
# PATH plays each daemon state, and the makefiles are only PARSED — the variables
# are read back through a throwaway `probe` makefile passed as a second -f, which
# is where the bug lives.
set -euo pipefail
cd "$(dirname "$0")/.."

ROOT=$PWD
pass=0
fail() { echo "test-host-arch: FAIL  $*" >&2; exit 1; }
ok()   { pass=$((pass + 1)); }

TEST_TMP=$(mktemp -d)
trap 'rm -rf -- "$TEST_TMP"' EXIT

# What the fallback is supposed to produce. Same normalisation the makefiles do.
native=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
[ -n "$native" ] || fail "uname -m produced nothing; the fixture is broken"
# A DIFFERENT arch, for the mirror case below: the fallback must still USE the
# daemon's answer when there is one, not just always run uname.
if [ "$native" = amd64 ]; then other=arm64; else other=amd64; fi

# --- docker shims -------------------------------------------------------------
# Each shim is its own PATH dir, prepended, so `docker` resolves to it and never
# to a real one that may or may not be running on the machine running this test.
make_shim() {  # make_shim <name> <stdout-for-version> <exit-code>
  local dir=$TEST_TMP/$1
  mkdir -p "$dir"
  cat > "$dir/docker" <<SHIM
#!/usr/bin/env bash
# Only 'docker version' is played; anything else is a hard error, so a make
# parse that shells out to docker some other way shows up instead of hiding.
[ "\${1:-}" = version ] || { echo "shim: unexpected 'docker \$*'" >&2; exit 127; }
printf '%s' "$2"
exit $3
SHIM
  chmod +x "$dir/docker"
  echo "$dir"
}

# The reproduction: a blank line on stdout AND a non-zero exit.
unreachable=$(make_shim unreachable '
' 1)
# The other silent-empty shape: the daemon answers, but with no arch. `||` never
# fires here, so an unfixed line leaves HOST_ARCH completely empty — `staging/linux-/`.
empty_ok=$(make_shim empty-ok '' 0)
# A healthy daemon reporting an arch that is NOT this host's.
healthy=$(make_shim healthy "$other
" 0)
# Nothing on stdout, exit 127: indistinguishable from no docker installed at
# all, which is the fallback's original reason for existing.
notfound=$(make_shim notfound '' 127)
# A daemon that answers with two words. Squashing whitespace out of the reply
# would turn this into the single word `amd64arm64`, which passes both guards
# and builds for a platform nobody asked for; it has to fail loudly instead.
twoword=$(make_shim twoword 'amd64 arm64
' 0)

# --- reading a variable back out of a makefile --------------------------------
# A second -f adds a target without editing the file under test. `probe` is the
# explicit goal, so neither makefile's default goal runs and nothing is built.
cat > "$TEST_TMP/probe.mk" <<'PROBE'
probe: ; @echo "$($(VAR))"
PROBE

probe() {  # probe <dir> <shim-dir> <var>
  local dir=$1 shim=$2 var=$3
  ( cd "$ROOT/$dir" \
    && PATH="$shim:$PATH" \
       make -f Makefile -f "$TEST_TMP/probe.mk" VAR="$var" probe 2>"$TEST_TMP/stderr" )
}

words() { echo "$#"; }

# --- the assertions -----------------------------------------------------------
# Applied identically to both makefiles, because the defective line was in both
# and a fix to only one lets them disagree about the target arch.
check_arch() {  # check_arch <label> <dir> <shim> <expected-arch>
  local label=$1 dir=$2 shim=$3 want=$4 got staging n

  got=$(probe "$dir" "$shim" HOST_ARCH) \
    || fail "$label: make failed to parse $dir/Makefile: $(cat "$TEST_TMP/stderr")"

  # Exactly one word: the whole failure mode is a value that SPLITS.
  # shellcheck disable=SC2086  # unquoted on purpose: counting the split words is the test
  n=$(words $got)
  [ "$n" -eq 1 ] || fail "$label: HOST_ARCH is not a single word: '$got' ($n words)"
  [ "$got" = "$want" ] || fail "$label: HOST_ARCH is '$got', want '$want'"

  # A malformed target name is the symptom that actually reaches disk, so assert
  # on a derived path too rather than trusting HOST_ARCH alone.
  staging=$(probe "$dir" "$shim" STAGING)
  # shellcheck disable=SC2086  # as above
  n=$(words $staging)
  [ "$n" -eq 1 ] || fail "$label: STAGING is not a single word: '$staging' ($n words)"
  case $staging in
    *"linux-$want"*) : ;;
    *) fail "$label: STAGING '$staging' does not carry the arch '$want'" ;;
  esac

  # And make must not have complained while parsing. The duplicate `.stamps/linux-`
  # rules the split produced show up here as "overriding commands for target".
  ! grep -q 'overriding commands for target' "$TEST_TMP/stderr" \
    || fail "$label: make warned while parsing: $(cat "$TEST_TMP/stderr")"
}

for dir in . build/components; do
  # THE GUARD. Fails on an unfixed makefile, passes on a guarded one.
  check_arch "$dir / unreachable daemon" "$dir" "$unreachable" "$native"
  ok
  # The daemon answered, but with nothing useful. Must also fall back.
  check_arch "$dir / daemon reports no arch" "$dir" "$empty_ok" "$native"
  ok
  # No docker at all — the case the fallback was always for.
  check_arch "$dir / docker not installed" "$dir" "$notfound" "$native"
  ok
  # The mirror: a working daemon's answer must still WIN, or "always use uname"
  # would pass every check above while quietly breaking cross-engine builds.
  check_arch "$dir / healthy daemon" "$dir" "$healthy" "$other"
  ok
done

# --- the two makefiles must agree ---------------------------------------------
# Not implied by the checks above: this compares them directly, under every
# daemon state, which is the thing a fix to only one copy breaks.
for shim in "$unreachable" "$empty_ok" "$notfound" "$healthy"; do
  a=$(probe . "$shim" HOST_ARCH)
  b=$(probe build/components "$shim" HOST_ARCH)
  [ "$a" = "$b" ] \
    || fail "the makefiles disagree about HOST_ARCH: root '$a' vs components '$b'"
done
ok

# --- the defective form must not come back ------------------------------------
# Cheap and file-local, so a reintroduction in EITHER copy fails here with
# the cause attached rather than as a mystery path with a space in it.
for f in Makefile build/components/Makefile; do
  ! grep -q "docker version --format '{{.Server.Arch}}' 2>/dev/null || uname" "$f" \
    || fail "$f still uses the '|| uname' form: docker writes a blank line
  to stdout AND exits non-zero when the daemon is unreachable, so both outputs
  land in HOST_ARCH. Probe the daemon first (see tools/dev-release.sh)."
done
ok

# --- a malformed TARGET_ARCH must be refused, not silently used ---------------
# The backstop for every source of a bad arch, not just the daemon: an override
# that splits has to stop the build rather than write to `staging/linux- x`.
for dir in . build/components; do
  if out=$( cd "$ROOT/$dir" \
            && PATH="$notfound:$PATH" \
               make -f Makefile -f "$TEST_TMP/probe.mk" \
                    VAR=STAGING TARGET_ARCH="$native junk" probe 2>&1 ); then
    fail "$dir: a two-word TARGET_ARCH was accepted: '$out'"
  fi
  grep -q 'TARGET_ARCH' <<<"$out" \
    || fail "$dir: the refusal does not name TARGET_ARCH: '$out'"
done
ok

# --- a padded TARGET_ARCH must be refused too ---------------------------------
# $(words) counts fields, so ' arm64 ' reads as one word and slips past the
# word-count guard — while the padding still reaches STAGING and yields a
# directory whose name ends in a space. Same defect class, different shape.
for dir in . build/components; do
  if out=$( cd "$ROOT/$dir" \
            && PATH="$notfound:$PATH" \
               make -f Makefile -f "$TEST_TMP/probe.mk" \
                    VAR=STAGING TARGET_ARCH=" $native " probe 2>&1 ); then
    fail "$dir: a whitespace-padded TARGET_ARCH was accepted: '$out'"
  fi
  grep -q 'TARGET_ARCH' <<<"$out" \
    || fail "$dir: the padding refusal does not name TARGET_ARCH: '$out'"
done
ok

# --- a daemon answering with two words must fail, not be squashed ------------
# The reply is passed through as-is and stripped only to decide whether it was
# blank: `tr -d` on the value itself would produce `amd64arm64`, one make word,
# silently accepted.
for dir in . build/components; do
  if out=$( cd "$ROOT/$dir" \
            && PATH="$twoword:$PATH" \
               make -f Makefile -f "$TEST_TMP/probe.mk" VAR=STAGING probe 2>&1 ); then
    fail "$dir: a two-word daemon answer was accepted: '$out'"
  fi
  grep -q 'amd64arm64' <<<"$out" \
    && fail "$dir: the daemon's answer was squashed into one word: '$out'"
  grep -q 'TARGET_ARCH' <<<"$out" \
    || fail "$dir: the refusal does not name TARGET_ARCH: '$out'"
done
ok

# Unsupported words and shell/path syntax must be rejected from either source.
for bad in 'amd64;id' 'arm64|id' '../amd64' '%' 'riscv64'; do
  malformed=$(make_shim malformed "$bad" 0)
  for dir in . build/components; do
    if out=$( cd "$ROOT/$dir" && PATH="$notfound:$PATH" \
        make -f Makefile -f "$TEST_TMP/probe.mk" VAR=STAGING TARGET_ARCH="$bad" probe 2>&1 ); then
      fail "$dir: unsafe TARGET_ARCH was accepted: '$out'"
    fi
    grep -q 'TARGET_ARCH must be amd64 or arm64' <<<"$out" \
      || fail "$dir: expected architecture allowlist refusal: '$out'"
    if out=$( cd "$ROOT/$dir" && PATH="$malformed:$PATH" \
        make -f Makefile -f "$TEST_TMP/probe.mk" VAR=STAGING probe 2>&1 ); then
      fail "$dir: unsafe daemon architecture was accepted: '$out'"
    fi
    grep -q 'TARGET_ARCH must be amd64 or arm64' <<<"$out" \
      || fail "$dir: expected daemon architecture refusal: '$out'"
    ok
  done
done

echo "test-host-arch: ok ($pass checks)"
