#!/usr/bin/env bash
# etcd convergence helpers for provision.sh. This file is sourced by both the
# driver and its deterministic regression test; it intentionally has no setup or
# side effects of its own.

# New artifacts print the key-file path as the final field on the HA join
# line. Older release artifacts still print the inline key; accept both without
# ever evaluating CLI output as shell code.
parse_join_credentials() {
  local parsed
  TOKEN='' CAHASH='' CERTKEY_FILE='' CERTKEY=''
  parsed=$(join_flag_values "$1" --token --discovery-token-ca-cert-hash --certificate-key-file --certificate-key) || return 1
  { IFS= read -r TOKEN; IFS= read -r CAHASH; IFS= read -r CERTKEY_FILE; IFS= read -r CERTKEY || true; } <<<"$parsed"
  [[ -n $TOKEN && -n $CAHASH && ( -n $CERTKEY_FILE || -n $CERTKEY ) ]]
}

# A control-plane token can read the shared CA material, so workers join with
# their own token, parsed from a plain `token create`'s worker line.
parse_worker_credentials() {
  local parsed
  WORKER_TOKEN='' WORKER_CAHASH=''
  parsed=$(join_flag_values "$1" --token --discovery-token-ca-cert-hash) || return 1
  { IFS= read -r WORKER_TOKEN; IFS= read -r WORKER_CAHASH || true; } <<<"$parsed"
  [[ -n $WORKER_TOKEN && -n $WORKER_CAHASH ]]
}

# join_flag_values LINE FLAG... — each flag's value, one per line, "" if absent.
# shlex decodes quoting without executing substitutions or shell commands.
# Pass output on stdin, not argv, since it contains the bootstrap token.
join_flag_values() {
  local line=$1
  shift
  python3 -c '
import shlex, sys
try:
    args = shlex.split(sys.stdin.read())
    values = []
    for flag in sys.argv[1:]:
        indexes = [i for i, arg in enumerate(args) if arg == flag]
        if len(indexes) > 1 or (indexes and indexes[0]+1 == len(args)):
            raise ValueError()
        value = args[indexes[0]+1] if indexes else ""
        if "\n" in value or "\r" in value:
            raise ValueError()
        values.append(value)
    print("\n".join(values))
except ValueError:
    sys.exit(1)
' "$@" <<<"$line"
}

# Called by the driver's EXIT trap, including startup/readiness failures.
# Preserve the primary exit status and attempt BOTH removals if one fails.
cleanup_join_keys() {
  local status=$1
  if [[ -n ${join_key_path:-} ]]; then
    if ! backend_ssh "$h" "rm -f -- $(printf '%q' "$join_key_path")"; then
      log "WARNING: remove key file $join_key_path on $h manually"
      if (( status == 0 )); then status=1; fi
    fi
  fi
  if [[ -n ${CERTKEY_FILE:-} ]]; then
    if ! backend_ssh "$CP1_H" "rm -f -- $(printf '%q' "$CERTKEY_FILE")"; then
      log "WARNING: remove key file $CERTKEY_FILE on $CP1_H manually"
      if (( status == 0 )); then status=1; fi
    fi
  fi
  return "$status"
}

# Stream the secret between authenticated connections, without runner files,
# argv contents, or logs. mktemp creates a unique 0600 target on the joiner.
# The returned path belongs to the caller, which removes it after readiness.
copy_join_key() {
  local source=$1 source_path=$2 target=$3 dest
  dest=$(backend_ssh "$target" 'umask 077; mktemp /tmp/stratum-ha-certificate-key-XXXXXXXXXX') || return 1
  [[ $dest == /tmp/stratum-ha-certificate-key-* && $dest != *$'\n'* ]] || return 1
  join_key_path=$dest
  if ! backend_ssh "$source" "cat -- $(printf '%q' "$source_path")" |
    backend_ssh_stdin "$target" "cat > $(printf '%q' "$dest")"; then
    backend_ssh "$target" "rm -f -- $(printf '%q' "$dest")" || log "WARNING: remove key file $dest on $target manually"
    return 1
  fi
  printf '%s\n' "$dest"
}

# MemberAdd changes quorum before the joining member starts. A kubelet-started
# marker only proves that the process which will launch the etcd static pod is
# alive; it does not prove the member is participating. Before adding another
# controller, require every advertised etcd endpoint to report healthy.
#
# `stratum etcdctl` configures itself now, and treats anything already in the
# environment as authoritative — so the explicit sets below still win and this
# harness behaves as before. Keep them. This job checks out its harness from the
# workflow ref but builds the artifact from the branch under test, so the
# plaintext leg runs release-branch binaries from before the dedicated etcd CA,
# where etcd listens on plaintext localhost and none of the three TLS files
# exists. Those binaries inject nothing at all, and relying on injection here
# would silently stop testing what the artifact actually does.
#
# The remote probe distinguishes those two complete contracts. A partial TLS set
# is structural damage, never a reason to downgrade to plaintext.
# shellcheck disable=SC2016 # This program must expand on the remote node.
ETCDCTL_REMOTE_PROBE='
root=${STRATUM_ROOT:-/var/lib/stratum}
etcdctl=$root/bin/etcdctl
pki=$root/pki
ca=$pki/etcd-ca-bundle.crt
cert=$pki/apiserver-etcd-client.crt
key=$pki/apiserver-etcd-client.key
dedicated_ca=$pki/etcd-ca.crt

if [[ ! -x $etcdctl ]]; then
  echo "missing executable: $etcdctl" >&2
  exit 1
fi

present=0
for file in "$ca" "$cert" "$key"; do
  [[ -f $file ]] && present=$((present + 1))
done

case $present in
  3)
    echo tls
    ;;
  0) if [[ ! -f $dedicated_ca ]]; then
    echo plaintext
    else
      echo "dedicated etcd CA exists but its TLS credentials are missing" >&2
      exit 1
    fi
    ;;
  *)
    for file in "$ca" "$cert" "$key"; do
      [[ -f $file ]] || echo "missing TLS credential: $file" >&2
    done
    exit 1
    ;;
esac
'

ETCDCTL_TLS_ENV='ETCDCTL_API=3 ETCDCTL_ENDPOINTS=https://127.0.0.1:2379 ETCDCTL_CACERT=/var/lib/stratum/pki/etcd-ca-bundle.crt ETCDCTL_CERT=/var/lib/stratum/pki/apiserver-etcd-client.crt ETCDCTL_KEY=/var/lib/stratum/pki/apiserver-etcd-client.key'
ETCDCTL_PLAINTEXT_ENV='ETCDCTL_API=3 ETCDCTL_ENDPOINTS=http://127.0.0.1:2379'

wait_etcd_cluster() {  # handle, description, timeout-seconds, expected-members
  local h=$1 what=$2 timeout=$3 want=$4 waited=0 out='' rc remote_env=''

  while (( waited < timeout )); do
    # Resolve the artifact's etcd contract once. SSH transport failures are
    # transient and retry; a remote structural verdict fails immediately with
    # its diagnostic.
    if [[ -z $remote_env ]]; then
      rc=0
      out=$(backend_ssh "$h" "$ETCDCTL_REMOTE_PROBE" 2>&1) || rc=$?
      if (( rc == 255 )); then
        sleep 5; waited=$(( waited + 5 ))
        continue
      fi
      if (( rc != 0 )); then
        log "$what: etcd gate preflight failed — $out"
        return 1
      fi
      case $out in
        tls)       remote_env=$ETCDCTL_TLS_ENV ;;
        plaintext) remote_env=$ETCDCTL_PLAINTEXT_ENV ;;
        *)
          log "$what: etcd gate preflight returned an invalid contract: $out"
          return 1
          ;;
      esac
    fi

    # `endpoint health --cluster` enumerates endpoints from MemberList, and an
    # unstarted member has no ClientURLs, so it is silently skipped. Count the
    # healthy lines rather than trusting a successful exit status.
    if out=$(backend_ssh "$h" "env $remote_env $REMOTE_BIN etcdctl endpoint health --cluster" 2>&1) &&
       (( $(grep -c ' is healthy' <<<"$out") == want )); then
      return 0
    fi
    sleep 5; waited=$(( waited + 5 ))
  done

  log "$what: etcd cluster did not report $want healthy members after ${timeout}s"
  log "$what: last health output:"
  printf '%s\n' "$out" >&2
  if [[ -n $remote_env ]]; then
    backend_ssh "$h" "env $remote_env $REMOTE_BIN etcdctl endpoint status --cluster --write-out=table" >&2 || true
  fi
  return 1
}
