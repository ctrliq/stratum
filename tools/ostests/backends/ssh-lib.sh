#!/usr/bin/env bash
# SSH plumbing shared by the cloud backends — see README.md.
#
# Every cloud backend has the same shape: `pulumi up` a program that emits two
# parallel lists — public IPs (the handle: SSH from the runner) and private IPs
# (what the cluster is built from) — then drive the nodes over SSH as an
# unprivileged user with passwordless sudo. Only backend_up differs per cloud:
# which config its program takes, and which state bucket its credentials can
# reach. So the other four contract functions live here once, and each cloud
# backend sources this file. local.sh does not: containers have no SSH.
#
# Before sourcing, a backend sets:
#   BACKEND_NAME   for messages ("gcp backend: ...")
#   PROG_DIR       the directory holding its Pulumi.yaml
#   SSH_USER       (optionally) a default other than stratum. GCP creates
#                  whichever user the key is uploaded for.
#
# Reads: $SSH_KEY                   default ~/.ssh/id_ed25519
#        $SSH_USER                  default stratum
#        $SSH_KNOWN_HOSTS           default $OUT/known_hosts
#        $PULUMI                    binary, for tests
#        $PULUMI_BACKEND_URL        checked by _require_state_backend

PULUMI=${PULUMI:-pulumi}
SSH_KEY=${SSH_KEY:-$HOME/.ssh/id_ed25519}
SSH_USER=${SSH_USER:-stratum}
: "${BACKEND_NAME:?ssh-lib.sh: BACKEND_NAME must be set before sourcing}"

# accept-new only means anything if the keys it accepts are actually written
# somewhere. Pointing UserKnownHostsFile at /dev/null makes this a security
# claim the code does not deliver: nothing persists, so a key that CHANGES
# between two connections to the same address inside one run is
# indistinguishable from a first sighting, which is precisely the case accept-new
# exists to catch. A file scoped to this cluster restores that.
#
# Under $OUT when provision.sh drives us — the per-CLUSTER_NAME directory the
# caller creates and where tunnel.pid lives — so no shared-/tmp
# predictable-path games apply. Nothing removes $OUT: a dev loop accumulates
# one directory per cluster name under _output/ostests/, deliberately (the
# kubeconfig and logs survive for inspection), and /_output/ is gitignored.
# mktemp fallback for a backend sourced outside provision.sh.
# Per-CLUSTER_NAME, strictly speaking, not per-run: a reused name in the local
# dev loop can meet a stale entry on a recycled address, and accept-new then
# REJECTS the node — failing closed, at worst a rerun after clearing $OUT.
# Never shared across clusters: a stable global file would make that collision
# routine instead of rare.
#
# Deliberate residual: the FIRST contact with each node is still
# trust-on-first-use. GCP publishes real host keys in guest attributes
# (gcloud compute instances get-guest-attributes --query-path=hostkeys/…), so a
# later hardening pass can seed this file and flip to StrictHostKeyChecking=yes.
SSH_KNOWN_HOSTS=${SSH_KNOWN_HOSTS:-${OUT:+$OUT/known_hosts}}
SSH_KNOWN_HOSTS=${SSH_KNOWN_HOSTS:-$(mktemp "${TMPDIR:-/tmp}/ske-known-hosts.XXXXXX")}

# BatchMode: a prompt on a CI runner hangs the job to the workflow timeout
# instead of failing. LogLevel=ERROR: callers capture 2>&1 and parse it, and
# OpenSSH 10.1+ warns on stderr whenever the node's sshd offers no post-quantum
# key exchange; real failures are errors and still print.
_ssh_opts=(
  -o BatchMode=yes
  -o LogLevel=ERROR
  -o StrictHostKeyChecking=accept-new
  -o UserKnownHostsFile="$SSH_KNOWN_HOSTS"
  -o ConnectTimeout=10
  -o ServerAliveInterval=15
  -i "$SSH_KEY"
)

# The preamble of every backend_up: the tool, the keypair, and the public half
# into $SSH_PUBLIC_KEY for the program's config.
_require_up_inputs() {
  command -v "$PULUMI" >/dev/null || { echo "$BACKEND_NAME backend: $PULUMI not installed" >&2; return 1; }
  [[ -r $SSH_KEY.pub ]] || { echo "$BACKEND_NAME backend: no public key at $SSH_KEY.pub" >&2; return 1; }
  # shellcheck disable=SC2034  # read by the sourcing backend's backend_up
  SSH_PUBLIC_KEY=$(cat "$SSH_KEY.pub") || return 1
}

# $1 is the URL scheme the job's cloud credentials can reach (gs://); $2 says
# why, for the message. In CI the state URL must be exactly
# that: the job's GCP WIF credentials reach only gs://, and a backend URL with
# any other scheme fails opaquely inside pulumi's first backend call. Fail fast
# with the real reason. Locally it is only a warning: file:// is legitimate for
# a cluster created and destroyed in one sitting, at the cost that state dies
# with the machine.
_require_state_backend() {
  local want=$1 why=$2
  case ${PULUMI_BACKEND_URL:-} in
    '') if [[ -n ${GITHUB_ACTIONS:-} ]]; then
          echo "$BACKEND_NAME backend: PULUMI_BACKEND_URL must be set to a $want bucket in CI (see backends/README.md)" >&2
          return 1
        fi
        echo "$BACKEND_NAME backend: PULUMI_BACKEND_URL unset — using whatever backend pulumi is logged into" >&2 ;;
    "$want"*) ;;
    *)  if [[ -n ${GITHUB_ACTIONS:-} ]]; then
          echo "$BACKEND_NAME backend: PULUMI_BACKEND_URL must be a $want bucket in CI — $why (see backends/README.md)" >&2
          return 1
        fi
        echo "$BACKEND_NAME backend: PULUMI_BACKEND_URL is not $want — fine for one local sitting; state dies with this machine (see backends/README.md)" >&2 ;;
  esac
}

# Select-or-create the run's stack, set every key=value given, `up`, then print
# the nodes. Arguments are "key=value"; the value may itself contain "=" (an
# ssh public key's base64 padding does), so only the FIRST one splits.
_pulumi_up() {
  (
    # backend_up is called from a command substitution whose status is tested,
    # which disables Bash's errexit handling inside this entire function. Check
    # every command explicitly so a failed config write cannot be hidden by a
    # later successful `pulumi up` against stale stack state.
    cd "$PROG_DIR" || exit 1
    # A stack per run. --create is idempotent, so a retried job re-uses its stack
    # rather than colliding with it.
    "$PULUMI" stack select --create "$CLUSTER_NAME" >&2 || exit 1
    local kv
    for kv in "$@"; do
      "$PULUMI" config set "${kv%%=*}" "${kv#*=}" >&2 || exit 1
    done
    "$PULUMI" up --yes --non-interactive >&2 || exit 1
  ) || return 1
  _pulumi_nodes
}

# Public IP is the handle (SSH), private IP is what the cluster is built from.
# Paired by index: every program emits both lists in the same order, under the
# same four output names.
_pulumi_nodes() {
  "$PULUMI" -C "$PROG_DIR" stack output --json | python3 -c '
import json, sys
o = json.load(sys.stdin)
for role, pub, priv in (("controller", "controllers", "controllersPrivate"),
                        ("worker", "workers", "workersPrivate")):
    a, b = o[pub], o[priv]
    if len(a) != len(b):
        sys.exit(f"{sys.argv[1]} backend: {pub}/{priv} length mismatch ({len(a)} vs {len(b)})")
    for h, ip in zip(a, b):
        print(role, h, ip)
' "$BACKEND_NAME"
}

backend_ready() {
  local handle=$1
  for _ in $(seq 1 60); do
    ssh "${_ssh_opts[@]}" "$SSH_USER@$handle" true 2>/dev/null && return 0
    sleep 5
  done
  echo "$BACKEND_NAME backend: SSH to $handle never came up (5m)" >&2
  return 1
}

backend_ssh() {
  local handle=$1; shift
  # sudo -n: the cloud image is passwordless-sudo by default; if one is not, fail
  # loudly here rather than hanging on a password prompt no one can answer.
  #
  # The %q expansion is client-side by design: it collapses the caller's command
  # into ONE shell-safe word here, so the remote shell hands `bash -c` a single
  # argument instead of resplitting on whatever spaces or quotes it contained.
  # shellcheck disable=SC2029
  ssh "${_ssh_opts[@]}" "$SSH_USER@$handle" "sudo -n bash -c $(printf '%q' "$*")"
}

backend_scp() {
  local src=$1 handle=$2 dst=$3
  # Two hops: scp cannot write to a root-owned path as the unprivileged user.
  scp "${_ssh_opts[@]}" "$src" "$SSH_USER@$handle:/tmp/.ske-upload" >&2 || return 1
  backend_ssh "$handle" "install -m 0755 /tmp/.ske-upload '$dst' && rm -f /tmp/.ske-upload"
}

# SSH already forwards stdin. Keep this explicit so the local backend can
# request docker exec -i only for callers that intentionally stream input.
backend_ssh_stdin() { backend_ssh "$@"; }

backend_tunnel() {
  local handle=$1 port=$2
  # -f backgrounds after authentication and (with ExitOnForwardFailure) after
  # the forward binds, so both a connect failure and a port-bind failure are our
  # exit status rather than a tunnel that silently is not there. -f detaches and
  # loses $!, hence finding the pid afterwards.
  # ExitOnForwardFailure: without it, ssh -f exits 0 even when the -L bind
  # fails (default is no), leaving a live ssh with no forward.
  # A dedicated control socket gives us an authoritative PID and, critically,
  # a way to terminate the detached master if PID discovery itself fails.
  local control check pid ctl_bytes
  control="${TMPDIR:-/tmp}/ske-$$-$port.ctl"
  ctl_bytes=$(printf '%s' "$control" | wc -c)
  if (( ctl_bytes + 17 > 103 )); then
    echo "$BACKEND_NAME backend: control socket path '$control' leaves no room for ssh's temporary suffix (sockaddr_un binds at most 103 bytes on macOS, 108 on Linux); set TMPDIR to something shorter" >&2
    return 1
  fi
  rm -f "$control"
  ssh "${_ssh_opts[@]}" -o ControlMaster=yes -o ControlPath="$control" \
      -o ExitOnForwardFailure=yes -f -N -L "$port:127.0.0.1:6443" \
      "$SSH_USER@$handle" || return 1
  if ! check=$(ssh "${_ssh_opts[@]}" -S "$control" -O check "$SSH_USER@$handle" 2>&1); then
    echo "$BACKEND_NAME backend: the forward started but its control socket could not be queried" >&2
    ssh "${_ssh_opts[@]}" -S "$control" -O exit "$SSH_USER@$handle" >/dev/null 2>&1 || true
    return 1
  fi
  pid=$(sed -n 's/.*pid=\([0-9][0-9]*\).*/\1/p' <<<"$check")
  if [[ -z $pid ]]; then
    echo "$BACKEND_NAME backend: the forward started but ssh did not report its pid" >&2
    ssh "${_ssh_opts[@]}" -S "$control" -O exit "$SSH_USER@$handle" >/dev/null 2>&1 || true
    return 1
  fi
  echo "$pid"
}
