#!/usr/bin/env bash
# Runs one integration check in a container that looks like a SKE node.
#
# Usage: tools/inttest-run.sh tools/inttest/check-single-node.sh
set -euo pipefail
cd "$(dirname "$0")/.."

SCRIPT=${1:?usage: $0 tools/inttest/check-<name>.sh}
[[ -f $SCRIPT ]] || { echo "no such check: $SCRIPT" >&2; exit 2; }
[[ -f dist/stratum ]] || { echo "no dist/stratum — run 'make package' first" >&2; exit 1; }

# A stratum node already running on THIS host — a live supervisor, or an
# etcd/apiserver orphaned by a failed uninstall — contends with the harness for
# /run/stratum, etcd :2379 and apiserver :6443. The contention surfaces deep
# inside the container as a misleading "containerd socket ... not up after 2m0s"
# timeout, when the real cause is the host cluster. Detect it here and fail in
# 0s with a clear message instead. Set INTTEST_SKIP_HOST_CHECK=1 to bypass.
preflight_host() {
  if [[ ${INTTEST_SKIP_HOST_CHECK:-0} == 1 ]]; then return 0; fi
  local hits=()
  if [[ -S /run/stratum/containerd/containerd.sock ]]; then
    hits+=("the containerd socket /run/stratum/containerd/containerd.sock")
  fi
  if command -v pgrep >/dev/null 2>&1 && pgrep -f 'stratum up([[:space:]]|$)' >/dev/null 2>&1; then
    hits+=("a running 'stratum up' process")
  fi
  if command -v ss >/dev/null 2>&1; then
    local found="" p
    for p in 2379 6443; do
      # grep without -q, output discarded: -q would close the pipe on the first
      # match, and under `set -o pipefail` the resulting SIGPIPE on ss/awk would
      # make the pipeline non-zero and the probe miss the listener. Consume the
      # whole stream instead and test grep's own exit status.
      if ss -ltn 2>/dev/null | awk '{print $4}' | grep -E "[:.]${p}\$" >/dev/null; then
        found="${found:+$found,}$p"
      fi
    done
    if [[ -n $found ]]; then hits+=("a listener on port $found"); fi
  fi
  if [[ ${#hits[@]} -gt 0 ]]; then
    {
      echo "inttest: a stratum node appears to be running on this host — stop it before running inttest."
      printf '  found: %s\n' "${hits[@]}"
      echo "  a host cluster contends with the harness for /run/stratum, :2379 and :6443;"
      echo "  otherwise this fails deep in the container as a misleading 'containerd socket not up' timeout."
      echo "  set INTTEST_SKIP_HOST_CHECK=1 to bypass."
    } >&2
    exit 1
  fi
}
preflight_host

NAME=$(basename "$SCRIPT" .sh)

# One reader, not two. inttest-matrix.sh owns the `# ske-inttest:` parse and
# this asks it — so a header CI refuses is one a local run refuses, and there
# is no second regex here to drift from it.
NETWORK=$(tools/inttest-matrix.sh --directive network "$SCRIPT")
IMAGE=ske-inttest-harness:local

# previous=<tag>: the check wants that published release at /dist/stratum-previous
# (dist/ is mounted there). CI downloads it (the amd64 asset, for its amd64
# runners); here PREVIOUS=<path> stages a binary already on this machine, and
# an absent one fails now, with the fetch command for the architecture the
# node container runs on, rather than inside the container.
ARCH=$(docker version --format '{{.Server.Arch}}')
PREVIOUS_TAG=$(tools/inttest-matrix.sh --directive previous "$SCRIPT")
if [[ -n $PREVIOUS_TAG ]]; then
  if [[ -n ${PREVIOUS:-} ]]; then
    [[ -f $PREVIOUS ]] || { echo "inttest: PREVIOUS=$PREVIOUS is not a file" >&2; exit 1; }
    cp "$PREVIOUS" dist/stratum-previous
    chmod +x dist/stratum-previous
  elif [[ ! -f dist/stratum-previous ]]; then
    {
      echo "inttest: $NAME needs the $PREVIOUS_TAG release at dist/stratum-previous and none is staged."
      echo "  gh release download $PREVIOUS_TAG -R ctrliq/stratum -p stratum-linux-$ARCH -O dist/stratum-previous && chmod +x dist/stratum-previous"
      echo "  or: PREVIOUS=<path-to-that-binary> $0 $SCRIPT"
    } >&2
    exit 1
  fi
fi

# Built, not apk-installed at run time: the offline check runs with
# --network none and could not fetch anything.
docker build -q -t "$IMAGE" \
  --build-arg "ALPINE_VERSION=$(sed -n 's/^alpine_version *= *//p' build/versions.mk)" \
  tools/inttest/ >/dev/null
TOPOLOGY=$(tools/inttest-matrix.sh --directive topology "$SCRIPT")

# topology=ha3: three node containers on one user-defined network, and the
# check run HERE, driving them through `docker exec` (tools/inttest/harness-ha.sh
# has the why). The subnet is fixed so the VIP can be a known address outside
# docker's dynamic range, where no container will ever be handed it.
if [[ $TOPOLOGY == ha3 ]]; then
  HA_PREFIX="ske-inttest-$NAME"
  HA_SUBNET=${INTTEST_HA_SUBNET:-172.30.99}
  HA_NODES="cp1 cp2 cp3"
  ha_cleanup() {
    local n
    for n in $HA_NODES; do
      docker rm -f "$HA_PREFIX-$n" >/dev/null 2>&1 || true
      docker volume rm -f "$HA_PREFIX-$n" >/dev/null 2>&1 || true
    done
    docker network rm "$HA_PREFIX" >/dev/null 2>&1 || true
  }
  ha_cleanup
  trap ha_cleanup EXIT
  # Three kubelets share one kernel here, and each holds inotify instances for
  # its own watches and its pods'. The common default of 128 (Docker Desktop,
  # GitHub's Ubuntu runners) is what kind documents as too low for a
  # multi-node cluster; the symptom is a kubelet exiting 1 in a loop on the
  # last node to start. Raise it, never lower it, and say so: this is a
  # kernel-wide setting on the docker host.
  inotify=$(docker run --rm "$IMAGE" cat /proc/sys/fs/inotify/max_user_instances)
  if (( inotify < 512 )); then
    echo "inttest: raising fs.inotify.max_user_instances $inotify -> 512 on the docker host (three kubelets share it)"
    docker run --rm --privileged "$IMAGE" sysctl -qw fs.inotify.max_user_instances=512 >/dev/null
  fi
  docker network create --subnet "$HA_SUBNET.0/24" --ip-range "$HA_SUBNET.128/25" "$HA_PREFIX" >/dev/null
  i=11
  for n in $HA_NODES; do
    docker volume create "$HA_PREFIX-$n" >/dev/null
    # The same node shape as the single-node run below, kept alive by a sleep so
    # the check can stop and start it like a machine.
    docker run -d --privileged --cgroupns=private \
      --name "$HA_PREFIX-$n" --hostname "$n" \
      --network "$HA_PREFIX" --ip "$HA_SUBNET.$i" --platform "linux/$ARCH" \
      -v "$PWD/dist:/dist:ro" -v "$PWD/tools/inttest:/inttest:ro" \
      -v "$HA_PREFIX-$n:/var/lib/stratum" \
      --tmpfs /run:exec,mode=755 \
      --entrypoint sleep "$IMAGE" infinity >/dev/null
    export "HA_IP_$n=$HA_SUBNET.$i"
    i=$((i + 1))
  done
  export HA_PREFIX HA_NODES HA_VIP="$HA_SUBNET.10"
  echo "=== $NAME (topology=ha3, network=$HA_PREFIX $HA_SUBNET.0/24, vip=$HA_VIP, arch=$ARCH)"
  bash "$SCRIPT"
  exit $?
fi

VOLUME="ske-inttest-$NAME"

# A fresh volume per run for /var/lib/stratum: cluster state must not leak between
# checks, and containerd's snapshotter needs a real filesystem rather than the
# container's own overlay.
docker volume rm -f "$VOLUME" >/dev/null 2>&1 || true
docker volume create "$VOLUME" >/dev/null
trap 'docker volume rm -f "$VOLUME" >/dev/null 2>&1 || true' EXIT

echo "=== $NAME (network=$NETWORK, arch=$ARCH)"
docker run --rm --privileged --cgroupns=private \
  --network "$NETWORK" --platform "linux/$ARCH" \
  -v "$PWD/dist:/dist:ro" -v "$PWD/tools/inttest:/inttest:ro" \
  -v "$PWD/build:/build:ro" -v "$PWD/tools:/tools:ro" \
  -v "$VOLUME:/var/lib/stratum" \
  --tmpfs /run:exec,mode=755 \
  "$IMAGE" "/inttest/$(basename "$SCRIPT")"
