#!/usr/bin/env bash
# Asserts tools/lint-revisions.sh catches a version bumped without its revision.
#
# Sibling of tools/test-version.sh. The cases are fixture manifests run through
# the real script — the drift it exists to catch is a patch bump still
# carrying the previous patch's revision (etcd 3.7.1 with 3.7.0's,
# cilium 1.20.2 with 1.20.1's) — so those are the fixtures.
#
# Needs network: the gate resolves real tags, and a test that stubbed that out
# would be testing a reimplementation of the thing it guards.
set -euo pipefail
cd "$(dirname "$0")/.."

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
fail=0

fixture() { cat > "$WORK/$1"; }

expect() {  # description, want_exit, fixture
  local what=$1 want=$2 file=$3 got=0
  tools/lint-revisions.sh "$WORK/$file" >"$WORK/out" 2>&1 || got=$?
  if [[ $got -eq $want ]]; then
    printf '  ok    %s\n' "$what"
  else
    printf '  FAIL  %s: want exit %s, got %s\n' "$what" "$want" "$got" >&2
    sed 's/^/        /' "$WORK/out" >&2
    fail=1
  fi
}

# A git in front of the real one that fails the first $SHIM_FAILS ls-remote
# calls. Every other call, including the ones after those, still goes to the
# real git and the real network, so the tag being resolved is the real one and
# only the flake is staged.
REAL_GIT=$(command -v git)
mkdir "$WORK/bin"
cat > "$WORK/bin/git" <<EOF
#!/usr/bin/env bash
if [[ \${1:-} == ls-remote ]]; then
  echo >> "$WORK/calls"
  if (( \$(wc -l < "$WORK/calls") <= SHIM_FAILS )); then
    echo "shim: ls-remote failing on purpose" >&2; exit 128
  fi
fi
exec "$REAL_GIT" "\$@"
EOF
chmod +x "$WORK/bin/git"

# The call counts are per run, so they hold only for single-pin fixtures.
flaky() {  # description, want_exit, failures, want_ls_remote_calls, fixture, [want_output]
  local what=$1 want=$2 fails=$3 calls=$4 file=$5 says=${6:-} got=0 made
  : > "$WORK/calls"
  PATH="$WORK/bin:$PATH" SHIM_FAILS=$fails LINT_REVISIONS_BACKOFF=0 \
    tools/lint-revisions.sh "$WORK/$file" >"$WORK/out" 2>&1 || got=$?
  made=$(wc -l < "$WORK/calls" | tr -d ' ')
  if [[ $got -eq $want && $made -eq $calls ]] && grep -qF -- "$says" "$WORK/out"; then
    printf '  ok    %s\n' "$what"
  else
    # shellcheck disable=SC2016  # inside "${...:+}" the single quotes are literal and $says expands
    printf '  FAIL  %s: want exit %s after %s ls-remote calls%s, got exit %s after %s\n' \
      "$what" "$want" "$calls" "${says:+ saying '$says'}" "$got" "$made" >&2
    sed 's/^/        /' "$WORK/out" >&2
    fail=1
  fi
}

fixture agree <<'EOF'
# renovate: datasource=github-tags depName=etcd-io/etcd
etcd_version = 3.7.0
etcd_revision = 4d71f7c83a2cef992d57abd3284edfaf66c91a4a
EOF

# The release-1.37 bug: Renovate moves the version, nothing moves the SHA.
fixture drift <<'EOF'
# renovate: datasource=github-tags depName=etcd-io/etcd
etcd_version = 3.7.1
etcd_revision = 4d71f7c83a2cef992d57abd3284edfaf66c91a4a
EOF

fixture drift-cilium <<'EOF'
# renovate: datasource=github-tags depName=cilium/cilium
cilium_version = 1.20.2
cilium_revision = 7d68cfb394f2960e10aa72e76d0d51e66c1b2ebc
EOF

# A lightweight tag has no peeled ref. coredns ships them, and the first cut of
# the gate exited silently on the first one it met instead of checking it.
fixture lightweight <<'EOF'
# renovate: datasource=github-tags depName=coredns/coredns
coredns_version = 1.14.7
coredns_revision = 427fc80ed9ca47f354585eb30a3f1332950856c4
EOF

# A revision nothing can resolve must fail, not pass by default.
fixture unannotated <<'EOF'
etcd_version = 3.7.0
etcd_revision = 4d71f7c83a2cef992d57abd3284edfaf66c91a4a
EOF

# The second pin carries no annotation of its own. Its version and SHA are the
# first pin's, so an annotation left standing as loop state resolves it against
# etcd-io/etcd, finds a tag that matches, and passes it.
fixture inherited-annotation <<'EOF'
# renovate: datasource=github-tags depName=etcd-io/etcd
etcd_version = 3.7.0
etcd_revision = 4d71f7c83a2cef992d57abd3284edfaf66c91a4a
etcdutl_version = 3.7.0
etcdutl_revision = 4d71f7c83a2cef992d57abd3284edfaf66c91a4a
EOF

# A revision the pattern cannot read is a revision nothing checked.
fixture malformed <<'EOF'
# renovate: datasource=github-tags depName=etcd-io/etcd
etcd_version = 3.7.0
etcd_revision = not-a-sha
EOF

# The SHA is right and the value is not: Make keeps the trailing token, so
# what the Dockerfile compares against is not what upstream tagged.
fixture trailing-garbage <<'EOF'
# renovate: datasource=github-tags depName=etcd-io/etcd
etcd_version = 3.7.0
etcd_revision = 4d71f7c83a2cef992d57abd3284edfaf66c91a4a garbage
EOF

# A `#` comment, on the other hand, is Make syntax and not part of either
# value — on both lines of the pair, which have to read the same way.
fixture make-comment <<'EOF'
# renovate: datasource=github-tags depName=etcd-io/etcd
etcd_version = 3.7.0   # held back by the skew policy
etcd_revision = 4d71f7c83a2cef992d57abd3284edfaf66c91a4a  # v3.7.0
EOF

fixture wrong-datasource <<'EOF'
# renovate: datasource=docker depName=library/etcd
etcd_version = 3.7.0
etcd_revision = 4d71f7c83a2cef992d57abd3284edfaf66c91a4a
EOF

# This is the datasource the pins used before. The tag it names is right, but
# Renovate cannot resolve a digest through it and drops the bump, so a
# revisioned pin that drifts back to it must fail here rather than go quiet.
fixture github-releases <<'EOF'
# renovate: datasource=github-releases depName=etcd-io/etcd
etcd_version = 3.7.0
etcd_revision = 4d71f7c83a2cef992d57abd3284edfaf66c91a4a
EOF

fixture orphan-revision <<'EOF'
# renovate: datasource=github-tags depName=etcd-io/etcd
etcd_revision = 4d71f7c83a2cef992d57abd3284edfaf66c91a4a
EOF

fixture no-such-tag <<'EOF'
# renovate: datasource=github-tags depName=etcd-io/etcd
etcd_version = 3.7.9999
etcd_revision = 4d71f7c83a2cef992d57abd3284edfaf66c91a4a
EOF

echo "=== revision matches the version's tag"
expect "annotated tag, in step"            0 agree
expect "lightweight tag, in step"          0 lightweight
expect "trailing make comment on the pair" 0 make-comment

echo "=== drift"
expect "etcd version moved, revision did not"   1 drift
expect "cilium version moved, revision did not" 1 drift-cilium

echo "=== a revision that cannot be verified is not a pass"
expect "no renovate annotation"            1 unannotated
expect "annotation of the pin above it"    1 inherited-annotation
expect "revision is not a 40-char SHA"     1 malformed
expect "right SHA, trailing junk after it" 1 trailing-garbage
expect "annotation is not github-tags"     1 wrong-datasource
expect "annotation is github-releases"     1 github-releases
expect "revision with no version"          1 orphan-revision
expect "version has no upstream tag"       1 no-such-tag

echo "=== a flake is retried, and only a flake"
flaky "ls-remote fails once, then answers"    0 1  2 agree
flaky "ls-remote never answers"               1 99 3 agree "cannot reach"
flaky "no such tag is an answer, not a flake" 1 0  1 no-such-tag

[[ $fail -eq 0 ]] || { echo "test-revisions: FAILED" >&2; exit 1; }
echo "test-revisions: ok"
