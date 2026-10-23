#!/usr/bin/env bash
# Deterministic coverage for provision.sh's etcd compatibility and retry gate.
set -euo pipefail
cd "$(dirname "$0")/.."

REMOTE_BIN=/usr/local/bin/stratum
# shellcheck source=tools/ostests/provision-lib.sh
source tools/ostests/provision-lib.sh

fail() { echo "test-ostests-provision: FAIL  $*" >&2; exit 1; }
log() { echo "$*" >&2; }
sleep() { :; }

TEST_TMP=$(mktemp -d)
trap 'rm -rf -- "$TEST_TMP"' EXIT
CALLS_FILE=$TEST_TMP/calls
COMMAND_FILE=$TEST_TMP/command

# Exercise the actual remote classifier independently from the SSH state machine.
for root in plaintext tls partial missing-all; do
  mkdir -p "$TEST_TMP/$root/bin" "$TEST_TMP/$root/pki"
  : >"$TEST_TMP/$root/bin/etcdctl"
  chmod +x "$TEST_TMP/$root/bin/etcdctl"
done
: >"$TEST_TMP/tls/pki/etcd-ca-bundle.crt"
: >"$TEST_TMP/tls/pki/apiserver-etcd-client.crt"
: >"$TEST_TMP/tls/pki/apiserver-etcd-client.key"
: >"$TEST_TMP/partial/pki/etcd-ca-bundle.crt"
: >"$TEST_TMP/missing-all/pki/etcd-ca.crt"

contract=$(STRATUM_ROOT="$TEST_TMP/plaintext" bash -c "$ETCDCTL_REMOTE_PROBE")
[[ $contract == plaintext ]] || fail "legacy files classified as '$contract'"
contract=$(STRATUM_ROOT="$TEST_TMP/tls" bash -c "$ETCDCTL_REMOTE_PROBE")
[[ $contract == tls ]] || fail "complete TLS files classified as '$contract'"
partial_out=$(STRATUM_ROOT="$TEST_TMP/partial" bash -c "$ETCDCTL_REMOTE_PROBE" 2>&1) \
  && fail 'partial TLS files passed the remote probe'
[[ $partial_out == *'missing TLS credential:'* ]] \
  || fail 'partial TLS probe lost its diagnostic'
missing_all_out=$(STRATUM_ROOT="$TEST_TMP/missing-all" bash -c "$ETCDCTL_REMOTE_PROBE" 2>&1) \
  && fail 'current PKI with all TLS credentials missing downgraded to plaintext'
[[ $missing_all_out == *'dedicated etcd CA exists'* ]] \
  || fail 'missing-all TLS probe lost its diagnostic'

mode=''
reset_calls() { echo 0 >"$CALLS_FILE"; : >"$COMMAND_FILE"; }
call_count() { cat "$CALLS_FILE"; }
backend_ssh() {
  local _handle=$1 command=$2 calls
  calls=$(( $(cat "$CALLS_FILE") + 1 ))
  echo "$calls" >"$CALLS_FILE"
  case $mode:$calls in
    missing:1)
      echo 'missing TLS credential: /var/lib/stratum/pki/apiserver-etcd-client.key' >&2
      return 1
      ;;
    transport:1)
      return 255
      ;;
    transport:2|short:1|exact:1)
      echo tls
      ;;
    transport:3|exact:2)
      echo "$command" >"$COMMAND_FILE"
      printf '%s\n' 'https://cp1:2379 is healthy' 'https://cp2:2379 is healthy'
      ;;
    short:2)
      echo "$command" >"$COMMAND_FILE"
      echo 'https://cp1:2379 is healthy'
      ;;
    short:3)
      : # timeout diagnostic
      ;;
    legacy:1)
      echo plaintext
      ;;
    legacy:2)
      echo "$command" >"$COMMAND_FILE"
      printf '%s\n' 'http://cp1:2379 is healthy' 'http://cp2:2379 is healthy'
      ;;
    *)
      echo "unexpected backend call $calls in mode $mode" >&2
      return 1
      ;;
  esac
}

mode=missing; reset_calls
missing_out=$(wait_etcd_cluster cp1 cp2 10 2 2>&1) && fail 'missing TLS credential passed'
calls=$(call_count)
[[ $calls == 1 ]] || fail "missing credential made $calls calls, want 1"
[[ $missing_out == *'preflight failed'*apiserver-etcd-client.key* ]] \
  || fail 'missing credential diagnostic was lost'

mode=transport; reset_calls
wait_etcd_cluster cp1 cp2 10 2 || fail 'SSH 255 was not retried'
calls=$(call_count)
[[ $calls == 3 ]] || fail "transport retry made $calls calls, want 3"

mode=short; reset_calls
wait_etcd_cluster cp1 cp2 5 2 >/dev/null 2>&1 && fail 'short healthy output passed'
calls=$(call_count)
[[ $calls == 3 ]] || fail "short health case made $calls calls, want 3"

mode=exact; reset_calls
wait_etcd_cluster cp1 cp2 5 2 || fail 'exact healthy count did not pass'
calls=$(call_count)
[[ $calls == 2 ]] || fail "exact health case made $calls calls, want 2"

mode=legacy; reset_calls
wait_etcd_cluster cp1 cp2 5 2 || fail 'legacy plaintext contract did not pass'
health_command=$(cat "$COMMAND_FILE")
[[ $health_command == *'ETCDCTL_ENDPOINTS=http://127.0.0.1:2379'* ]] \
  || fail 'legacy probe did not select plaintext localhost'
[[ $health_command != *'ETCDCTL_CACERT='* ]] \
  || fail 'legacy probe injected unavailable TLS credentials'

# Exercise the real parser against both current and release-branch output.
parse_join_credentials "stratum up --join https://cp:6443 --token test.token --discovery-token-ca-cert-hash sha256:aa --certificate-key-file '/tmp/key with spaces'" \
  || fail 'file handoff did not parse'
[[ $CERTKEY_FILE == '/tmp/key with spaces' && -z $CERTKEY && $TOKEN == test.token && $CAHASH == sha256:aa ]] \
  || fail 'file handoff parsed incorrectly'
parse_join_credentials 'stratum up --join https://cp:6443 --token test.token --discovery-token-ca-cert-hash sha256:aa --certificate-key legacy-key' \
  || fail 'legacy handoff did not parse'
[[ $CERTKEY == legacy-key && -z $CERTKEY_FILE ]] || fail 'legacy key or stale file path'
if parse_join_credentials 'stratum up --join https://cp --token test.token'; then
  fail 'incomplete handoff accepted'
fi
parse_worker_credentials "stratum up --worker --server https://cp:6443 --token worker.token --discovery-token-ca-cert-hash sha256:bb --pod-cidr '10.42.0.0/16'" \
  || fail 'worker join line did not parse'
[[ $WORKER_TOKEN == worker.token && $WORKER_CAHASH == sha256:bb ]] || fail 'worker join line parsed incorrectly'
if parse_worker_credentials 'stratum up --worker --server https://cp --token worker.token'; then
  fail 'worker line without a CA hash accepted'
fi
if parse_worker_credentials 'stratum up --worker --server https://cp --discovery-token-ca-cert-hash sha256:bb'; then
  fail 'worker line without a token accepted'
fi

# The backend mock implements the transport contract: secret bytes use stdin,
# never a command argument or the helper output. No actual remote host needed.
source_key="$TEST_TMP/source key 'quoted'"
printf '%s\n' 'secret-material' >"$source_key"
transfer_fail=no
backend_ssh() {
  local node=$1 command=$2
  printf '%s\n' "$command" >>"$COMMAND_FILE"
  case $node:$command in
    joiner:'umask 077; mktemp '* )
      (umask 077; : >"$TEST_TMP/received")
      echo /tmp/stratum-ha-certificate-key-fixture
      ;;
    cp1:'cat -- '* ) bash -c "$command" ;;
    joiner:'rm -f -- /tmp/stratum-ha-certificate-key-fixture' ) rm -f "$TEST_TMP/received" ;;
    *) return 1 ;;
  esac
}
backend_ssh_stdin() {
  [[ $1 == joiner && $2 == 'cat > /tmp/stratum-ha-certificate-key-fixture' ]] || return 1
  cat >"$TEST_TMP/received"
  [[ $transfer_fail == no ]]
}
copy_join_key cp1 "$source_key" joiner >"$TEST_TMP/key-path" || fail 'key transfer failed'
dest=$(cat "$TEST_TMP/key-path")
[[ $join_key_path == "$dest" ]] || fail 'target not registered for exit cleanup'
[[ $dest == /tmp/stratum-ha-certificate-key-fixture ]] || fail 'transfer output exposed more than path'
cmp "$source_key" "$TEST_TMP/received" || fail 'key bytes changed in transfer'
[[ $(LC_ALL=C ls -l "$TEST_TMP/received") == -rw-------* ]] || fail 'key copy is not 0600'
if grep -q 'secret-material' "$COMMAND_FILE"; then fail 'secret appeared in argv'; fi
transfer_fail=yes
if copy_join_key cp1 "$source_key" joiner; then fail 'failed transfer accepted'; fi
[[ ! -e $TEST_TMP/received ]] || fail 'failed transfer left partial key file'
transfer_fail=no
if copy_join_key cp1 "$TEST_TMP/missing-source" joiner 2>/dev/null; then
  fail 'source read failure was hidden by a successful destination write'
fi
[[ ! -e $TEST_TMP/received ]] || fail 'source read failure left a key file'

# Shell quoting is decoded, never evaluated. Include apostrophes and literal
# substitutions in the same representation emitted by the Go CLI.
quoted_path="'/tmp/key '\"'\"'quote'\"'\"' \$(printf INJECTED); tail'"
parse_join_credentials "stratum up --join https://cp --token test.token --discovery-token-ca-cert-hash sha256:aa --certificate-key-file $quoted_path" \
  || fail 'quoted path did not parse'
[[ $CERTKEY_FILE == "/tmp/key 'quote' \$(printf INJECTED); tail" ]] || fail 'quoted path was evaluated or changed'
if parse_join_credentials "stratum up --certificate-key-file 'unterminated"; then fail 'invalid quoting accepted'; fi

# Run the actual exit-cleanup function in subprocesses, as the driver does.
# A failing removal must not prevent the other attempt or replace the join error.
for primary in 0 7; do
  for cleanup_fails in no yes; do
    : >"$COMMAND_FILE"
    status=0
    (
      CP1_H=cp1 h=joiner CERTKEY_FILE='/tmp/source key' join_key_path='/tmp/target key'
      # shellcheck disable=SC2317,SC2329  # called by cleanup_join_keys, from the EXIT trap
      backend_ssh() {
        printf '%s\n' "$1:$2" >>"$COMMAND_FILE"
        [[ $cleanup_fails == no ]]
      }
      # shellcheck disable=SC2154  # exit_status IS assigned, first thing in the same trap string
      trap 'exit_status=$?; trap - EXIT; cleanup_join_keys "$exit_status" || exit_status=$?; exit "$exit_status"' EXIT
      exit "$primary"
    ) 2>"$TEST_TMP/cleanup-error" || status=$?
    expected=$primary
    if [[ $primary == 0 && $cleanup_fails == yes ]]; then expected=1; fi
    [[ $status == "$expected" ]] || fail "cleanup changed exit status $primary to $status"
    [[ $(wc -l <"$COMMAND_FILE" | tr -d ' ') == 2 ]] || fail 'cleanup did not attempt both nodes'
    grep -q '^joiner:rm -f -- ' "$COMMAND_FILE" || fail 'joiner key not cleaned'
    grep -q '^cp1:rm -f -- ' "$COMMAND_FILE" || fail 'source key not cleaned'
    if [[ $cleanup_fails == yes ]]; then
      grep -q 'manually' "$TEST_TMP/cleanup-error" || fail 'cleanup failure not reported'
    fi
  done
done

# provision.sh's stdout is the kubeconfig path and nothing else: a node command
# that writes to stdout (dnf did, and broke every conformance run) must not leak,
# and the path itself must still arrive. One controller, no workers, all success.
tree=$TEST_TMP/tree
mkdir -p "$tree/tools/ostests/backends" "$tree/bin"
cp tools/ostests/provision.sh tools/ostests/provision-lib.sh "$tree/tools/ostests/"
: >"$tree/artifact"
printf '#!/bin/sh\nexit 0\n' >"$tree/bin/kubectl"
chmod +x "$tree/bin/kubectl"
cat >"$tree/tools/ostests/backends/noisy.sh" <<'EOF'
backend_up()     { echo 'controller cp1 10.0.0.1'; }
backend_ready()  { echo 'noise from backend_ready'; }
backend_scp()    { echo 'noise from backend_scp'; }
backend_tunnel() { :; }
backend_ssh() {
  case $2 in
    *dnf*|*setsid*) echo 'Google Compute Engine  29 kB/s | 8.5 kB  00:00' ;;
    *'token create'*) echo 'stratum up --join https://10.0.0.1:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:00 --certificate-key-file /run/k' ;;
    *'get nodes --no-headers'*) echo 1 ;;
    *admin.conf*) echo 'server: https://10.0.0.1:6443' ;;
  esac
}
EOF
out=$(PATH="$tree/bin:$PATH" PROVISION_BACKEND=noisy CLUSTER_NAME=t ARCH=amd64 \
  "$tree/tools/ostests/provision.sh" "$tree/artifact" 2>"$TEST_TMP/provision-err") \
  || fail "provision.sh failed against the stub backend: $(tail -3 "$TEST_TMP/provision-err")"
[[ $out == "$tree/_output/ostests/t/admin.conf" ]] \
  || fail "provision.sh stdout is not just the kubeconfig path: $out"
grep -q 'server: https://127.0.0.1:16443' "$out" || fail 'kubeconfig server not rewritten'

echo 'test-ostests-provision: ok'
