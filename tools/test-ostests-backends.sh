#!/usr/bin/env bash
# Deterministic coverage for the cloud backend behind tools/ostests/provision.sh
# (gcp): what backend_up tells Pulumi, what it refuses, and how it turns stack
# outputs into nodes. Offline: a stub pulumi and a stub gcloud on PATH.
#
# The contract's inputs-to-config mapping — ARCH, OS_IMAGE, the project and
# zone, into the config the program receives — is exactly where an arm64 leg
# silently becoming an amd64 one hides, and it is the part that can be pinned
# down offline.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "test-ostests-backends: FAIL  $*" >&2; exit 1; }

TEST_TMP=$(mktemp -d)
trap 'rm -rf -- "$TEST_TMP"' EXIT
export CALLS="$TEST_TMP/calls" FIX_OUTPUT="$TEST_TMP/output.json"
ssh-keygen -t ed25519 -N '' -q -f "$TEST_TMP/key"

# --- stubs --------------------------------------------------------------------

mkdir -p "$TEST_TMP/bin"
cat >"$TEST_TMP/bin/pulumi" <<'STUB'
#!/usr/bin/env bash
printf 'pulumi %s\n' "$*" >>"$CALLS"
case "$*" in
  "stack select --create "*|"config set "*|"up --yes --non-interactive") exit 0 ;;
  *" stack output --json") cat "$FIX_OUTPUT" ;;
  *) echo "stub pulumi: unexpected invocation: $*" >&2; exit 64 ;;
esac
STUB
# gcp.sh preflights the machine type with a real gcloud when one is on PATH,
# and this developer machine has one. Shadow it.
cat >"$TEST_TMP/bin/gcloud" <<'STUB'
#!/usr/bin/env bash
printf 'gcloud %s\n' "$*" >>"$CALLS"; exit 0
STUB
chmod +x "$TEST_TMP"/bin/*
cat >"$FIX_OUTPUT" <<'JSON'
{"controllers": ["203.0.113.1", "203.0.113.2", "203.0.113.3"],
 "controllersPrivate": ["10.0.0.1", "10.0.0.2", "10.0.0.3"],
 "workers": ["203.0.113.4", "203.0.113.5"],
 "workersPrivate": ["10.0.0.4", "10.0.0.5"],
 "clusterName": "ske-t"}
JSON

# --- driver -------------------------------------------------------------------

# Runs one backend function under the stubs, with the given VAR=value settings.
# /bin/bash on purpose where that is 3.2: the backends are sourced by
# provision.sh, whose local dev loop runs on stock macOS bash.
rc=0 out="" err=""
run() {  # backend function [VAR=value ...]
  local backend=$1 fn=$2; shift 2
  : >"$CALLS"; rc=0
  # shellcheck disable=SC2016  # the inner program expands in the inner shell
  out=$(env -i PATH="$TEST_TMP/bin:/usr/bin:/bin" HOME="$TEST_TMP" \
          CALLS="$CALLS" FIX_OUTPUT="$FIX_OUTPUT" \
          SSH_KEY="$TEST_TMP/key" OUT="$TEST_TMP/out" CLUSTER_NAME=ske-t \
          PULUMI_BACKEND_URL="${BACKEND_URL-unset}" "$@" \
          bash -c 'set -eo pipefail
                   [[ $PULUMI_BACKEND_URL == unset ]] && unset PULUMI_BACKEND_URL
                   mkdir -p "$OUT"; source "tools/ostests/backends/$1.sh"; shift; "$@"' \
          _ "$backend" "$fn" 2>"$TEST_TMP/err") || rc=$?
  err=$(cat "$TEST_TMP/err")
}
called()     { grep -qxF -- "$1" "$CALLS"; }
call_index() { grep -nxF -- "$1" "$CALLS" | cut -d: -f1; }

# The five node lines the backend must print, in this order.
want_nodes=$'controller 203.0.113.1 10.0.0.1\ncontroller 203.0.113.2 10.0.0.2\ncontroller 203.0.113.3 10.0.0.3\nworker 203.0.113.4 10.0.0.4\nworker 203.0.113.5 10.0.0.5'

# Every config set must land before `up`, or the run is against stale stack
# config with nothing said.
config_before_up() {
  local up; up=$(call_index "pulumi up --yes --non-interactive") || fail "$1: pulumi up was never called"
  local last; last=$(grep -n '^pulumi config set ' "$CALLS" | tail -1 | cut -d: -f1)
  (( last < up )) || fail "$1: a config set came after up"
}

# --- backend_up: the arch mapping and the state-backend rule -------------------

BACKEND_URL=gs://state run gcp backend_up GCP_PROJECT=proj GCP_ZONE=us-central1-f ARCH=arm64
[[ $rc -eq 0 ]] || fail "gcp arm64 backend_up exited $rc:"$'\n'"$err"
[[ $out == "$want_nodes" ]] || fail "gcp nodes:"$'\n'"$out"
called "pulumi stack select --create ske-t" || fail "gcp: stack not selected per run"
called "pulumi config set gcp:project proj" || fail "gcp: project not set"
called "pulumi config set gcp:zone us-central1-f" || fail "gcp: zone not set"
called "pulumi config set machineType t2a-standard-4" || fail "gcp arm64: wrong machine type"
called "pulumi config set image rocky-linux-cloud/rocky-linux-9-optimized-gcp-arm64" || fail "gcp arm64: wrong image"
called "pulumi config set sshUser stratum" || fail "gcp: ssh user"
called "pulumi config set sshPublicKey $(cat "$TEST_TMP/key.pub")" || fail "gcp: public key not passed"
called "gcloud compute machine-types describe t2a-standard-4 --project proj --zone us-central1-f" || fail "gcp: no machine-type preflight"
config_before_up gcp

BACKEND_URL=gs://state run gcp backend_up GCP_PROJECT=proj ARCH=amd64 OS_IMAGE=rocky-linux-cloud/rocky-linux-10
called "pulumi config set machineType e2-standard-4" || fail "gcp amd64: wrong machine type"
called "pulumi config set image rocky-linux-cloud/rocky-linux-10" || fail "gcp: OS_IMAGE was ignored — the OS matrix would be inert"

# --- the state-backend rule: in CI, the one scheme the credentials reach ---------

BACKEND_URL=s3://wrong run gcp backend_up GITHUB_ACTIONS=true GCP_PROJECT=p
[[ $rc -ne 0 ]] || fail "gcp: CI accepted state at s3://wrong"
! called "pulumi up --yes --non-interactive" || fail "gcp: up ran against s3://wrong in CI"
[[ $err == *"PULUMI_BACKEND_URL must"* ]] || fail "gcp: the wrong-scheme failure did not name the variable:"$'\n'"$err"
BACKEND_URL="unset" run gcp backend_up GITHUB_ACTIONS=true GCP_PROJECT=p
[[ $rc -ne 0 && $err == *"must be set to a gs:// bucket in CI"* ]] || fail "gcp: CI ran with no state URL:"$'\n'"$err"
# Locally the same is only a warning: file:// state is legitimate for one sitting.
BACKEND_URL=file:///tmp/x run gcp backend_up GCP_PROJECT=p
[[ $rc -eq 0 && $err == *"fine for one local sitting"* ]] || fail "gcp: a local non-gs state URL was refused:"$'\n'"$err"

# --- the node parser refuses mismatched output lists ----------------------------

echo '{"controllers":["a","b"],"controllersPrivate":["x"],"workers":[],"workersPrivate":[]}' >"$FIX_OUTPUT"
BACKEND_URL=gs://state run gcp backend_up GCP_PROJECT=p
[[ $rc -ne 0 && $err == *"length mismatch"* ]] || fail "a public/private length mismatch was not refused:"$'\n'"$err"

# --- only gcp and local remain ---------------------------------------------------

# The dropped clouds must fail in provision.sh before any infrastructure is
# touched, by name, rather than half-provisioning with a backend nobody runs.
touch "$TEST_TMP/artifact"
for backend in aws azure oci; do
  rc=0
  err=$(env -i PATH="$TEST_TMP/bin:/usr/bin:/bin" HOME="$TEST_TMP" CALLS="$CALLS" \
          CLUSTER_NAME=ske-t OUT="$TEST_TMP/out" PROVISION_BACKEND=$backend \
          bash tools/ostests/provision.sh "$TEST_TMP/artifact" 2>&1 >/dev/null) || rc=$?
  [[ $rc -ne 0 && $err == *"no such backend '$backend'"* ]] \
    || fail "provision.sh did not refuse the dropped $backend backend (rc=$rc):"$'\n'"$err"
done

# --- backend_tunnel's control socket ----------------------------------

cat >"$TEST_TMP/bin/ssh" <<'STUB'
#!/usr/bin/env bash
printf 'ssh %s\n' "$*" >>"$CALLS"
exit 1
STUB
chmod +x "$TEST_TMP/bin/ssh"

tunnel() {
  : >"$CALLS"; rc=0
  # shellcheck disable=SC2016  # the inner program expands in the inner shell
  out=$(env -i PATH="$TEST_TMP/bin:/usr/bin:/bin" HOME="$TEST_TMP" CALLS="$CALLS" \
          SSH_KEY="$TEST_TMP/key" OUT="$TEST_TMP/out" CLUSTER_NAME=ske-t "$@" \
          bash -c 'set -eo pipefail
                   mkdir -p "$OUT"
                   source tools/ostests/backends/gcp.sh
                   backend_tunnel host-1 6443' 2>"$TEST_TMP/err") || rc=$?
  err=$(cat "$TEST_TMP/err")
}

tunnel OUT="$TEST_TMP/out/ske-99999999999-7-gcp-rocky-9-optimized-gcp-arm64-arm64"
ctl=$(sed -n 's/.*-o ControlPath=\([^ ]*\).*/\1/p' "$CALLS" | head -1)
[[ -n $ctl ]] || fail "backend_tunnel: no ControlPath reached ssh: $err"
case $ctl in
  *ske-99999999999*) fail "backend_tunnel: ControlPath still derives from \$OUT: $ctl" ;;
esac
(( ${#ctl} + 17 <= 103 )) || fail "backend_tunnel: ControlPath $ctl leaves no room for the suffix"

grep -q 'ctl_bytes + 17 > 103' tools/ostests/backends/ssh-lib.sh \
  || fail "backend_tunnel: the cap is not the portable 103 (macOS binds 103 bytes, Linux 108)"

utf8=$(locale -a 2>/dev/null | grep -ixE 'c\.utf-?8|en_us\.utf-?8' | head -1 || true)
if [[ -n $utf8 ]]; then
  mb=$(printf '\346\227\245%.0s' {1..26})
  mb_bytes=$(printf '%s' "$mb" | wc -c)
  case $mb in
    $'\346\227\245'*) ;;
    *) fail "backend_tunnel: the multibyte fixture did not expand to UTF-8 octets, so this case would pass on length alone" ;;
  esac
  (( mb_bytes == 78 )) || fail "backend_tunnel: the multibyte fixture is $mb_bytes bytes, expected 78 for 26 three-byte glyphs"
  tunnel TMPDIR="/tmp/$mb" LC_ALL="$utf8"
  [[ $rc -ne 0 ]] || fail "backend_tunnel: accepted a multibyte TMPDIR that overruns in bytes"
  case $err in
    *"leaves no room for ssh's temporary suffix"*) ;;
    *) fail "backend_tunnel: wrong refusal for a multibyte TMPDIR: $err" ;;
  esac
else
  echo "test-ostests-backends: no UTF-8 locale installed — multibyte TMPDIR case not exercised" >&2
fi

tunnel TMPDIR="$TEST_TMP/$(printf 'd%.0s' {1..100})"
[[ $rc -ne 0 ]] || fail "backend_tunnel: accepted a TMPDIR with no room for the suffix"
case $err in
  *"leaves no room for ssh's temporary suffix"*) ;;
  *) fail "backend_tunnel: wrong refusal for an over-long TMPDIR: $err" ;;
esac

# --- the files themselves --------------------------------------------------------

if command -v shellcheck >/dev/null; then
  shellcheck -S style tools/ostests/backends/ssh-lib.sh tools/ostests/backends/gcp.sh "$0" \
    || fail "shellcheck -S style is not clean"
else
  echo "test-ostests-backends: shellcheck not installed — skipping (brew install shellcheck)" >&2
fi

echo 'test-ostests-backends: ok'
