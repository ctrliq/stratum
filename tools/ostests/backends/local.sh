#!/usr/bin/env bash
# Local backend for tools/ostests/provision.sh — see backends/README.md.
#
# Containers on a user-defined docker network, not VMs. Two reasons, both
# checked rather than assumed:
#
#   - Lima cannot do it on this host. Its own template says the vzNAT address
#     "is accessible from the host, but not from other guests", and socket_vmnet
#     (the option that does bridge guests together) is not installed. VMs that
#     cannot reach each other cannot form an etcd quorum.
#   - The pieces already exist: check-ha-three-controller.sh names the
#     missing piece as "a multi-container harness with a shared docker network",
#     and tools/inttest-run.sh already proves the packaged binary runs in the
#     Alpine harness image with --privileged --cgroupns=private.
#
# What this is for: iterating on provision.sh's *logic* — ordering, join
# sequencing, readiness gating, failure paths — without a 30-minute cloud cycle.
# It is not a substitute for a live cloud run. Containers share the host kernel and skip
# a real boot, so only the cloud backend can tell you whether the thing installs
# on a real image.
#
# Reads: $CLUSTER_NAME              names the network, containers and volumes
#        $CONTROLLERS $WORKERS      node counts (defaulted low, see below)
#        $LOCAL_API_PORT            host port for controller-0's apiserver (16443)

_net()  { echo "ske-${CLUSTER_NAME}"; }
_name() { echo "ske-${CLUSTER_NAME}-$1-$2"; }   # role, index
LOCAL_API_PORT=${LOCAL_API_PORT:-16443}

_image() {
  local repo image=ske-inttest-harness:local
  repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
  docker build -q -t "$image" \
    --build-arg "ALPINE_VERSION=$(sed -n 's/^alpine_version *= *//p' "$repo/build/versions.mk")" \
    "$repo/tools/inttest/" >/dev/null || return 1
  echo "$image"
}

# A controller runs etcd, the apiserver, controller-manager, scheduler, kubelet
# and containerd. Two of them plus a worker will not fit in a 4 GiB VM, and the
# failure mode is an OOM-killed apiserver that reads as a Stratum bug. Warn with
# the exact fix rather than letting someone debug that.
_check_capacity() {
  local want=$(( ${CONTROLLERS:-1} + ${WORKERS:-1} )) mem_gib
  mem_gib=$(docker info --format '{{.MemTotal}}' 2>/dev/null | awk '{printf "%d", $1/1073741824}')
  [[ -n $mem_gib && $mem_gib -gt 0 ]] || return 0
  local need=$(( want * 2 ))
  if (( mem_gib < need )); then
    echo "local backend: $want nodes want ~${need}GiB; the docker VM has ${mem_gib}GiB." >&2
    echo "               colima stop && colima start --cpus 8 --memory 24" >&2
    echo "               (continuing — expect OOM kills if this is too small)" >&2
  fi
}

backend_up() {
  command -v docker >/dev/null || { echo "local backend: docker not installed" >&2; return 1; }
  docker info >/dev/null 2>&1 || { echo "local backend: docker daemon not reachable (colima start?)" >&2; return 1; }

  # Deliberately smaller than the cloud default of 3+2: this backend is for
  # iterating on the driver, and a 1+1 cluster exercises every code path except
  # the second and third controller join. Raise it once colima has the memory.
  CONTROLLERS=${CONTROLLERS:-1}
  WORKERS=${WORKERS:-1}
  _check_capacity

  # Only controller-0 publishes, on a fixed port, so two local clusters cannot
  # coexist and one leaked container blocks every later run. Docker reports that
  # as "Bind for 127.0.0.1:16443 failed: port is already allocated" from inside a
  # `docker run`, which says nothing about what to do. Name the holder instead.
  local holder
  holder=$(docker ps -aq --filter "publish=$LOCAL_API_PORT" | head -1)
  if [[ -n $holder ]]; then
    local holder_name; holder_name=$(docker inspect -f '{{.Name}}' "$holder" | sed 's|^/||')
    if [[ $holder_name != "$(_name controller 0)" ]]; then
      echo "local backend: port $LOCAL_API_PORT is held by container '$holder_name'" >&2
      echo "               docker rm -f $holder_name    # or set LOCAL_API_PORT to something else" >&2
      return 1
    fi
  fi

  local img net; net=$(_net)
  img=$(_image) || { echo "local backend: harness image build failed" >&2; return 1; }

  # A user-defined network, not the default bridge: it gives container-to-container
  # DNS and a stable subnet, which is the whole reason this backend can form a
  # cluster at all.
  docker network inspect "$net" >/dev/null 2>&1 || docker network create "$net" >/dev/null

  local role idx name vol pub
  for role in controller worker; do
    local count; [[ $role == controller ]] && count=$CONTROLLERS || count=$WORKERS
    for (( idx=0; idx<count; idx++ )); do
      name=$(_name "$role" "$idx"); vol="$name-data"
      docker rm -f "$name" >/dev/null 2>&1 || true
      docker volume rm -f "$vol" >/dev/null 2>&1 || true
      docker volume create "$vol" >/dev/null

      # Only controller-0 publishes. Container IPs are not routable from macOS,
      # so this published port is how the runner reaches the apiserver — the
      # local equivalent of the gcp backend's SSH tunnel. 127.0.0.1 is in the
      # apiserver cert SANs (internal/pki/pki.go:239), so TLS still verifies.
      pub=()
      [[ $role == controller && $idx == 0 ]] && pub=(-p "127.0.0.1:$LOCAL_API_PORT:6443")

      # A dedicated volume for /var/lib/stratum: containerd's snapshotter needs a
      # real filesystem rather than the container's overlay (tools/inttest-run.sh).
      # ${pub[@]+...}: expanding an empty array unguarded is an "unbound
      # variable" under set -u before bash 4.4, and macOS /bin/bash is 3.2 —
      # the host this backend exists for.
      docker run -d --name "$name" --hostname "$name" \
        --privileged --cgroupns=private --network "$net" \
        ${pub[@]+"${pub[@]}"} \
        -v "$vol:/var/lib/stratum" \
        --tmpfs /run:exec,mode=755 \
        "$img" sleep infinity >/dev/null || return 1

      # Captured first, not inlined: a failed inspect inside an echo argument
      # still lets echo succeed, so set -e never fires and the caller gets a
      # two-field line whose missing IP surfaces ten minutes later as an opaque
      # join timeout. provision.sh validates too — catch it at the source.
      local ip
      ip=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$net\").IPAddress}}" "$name") || return 1
      [[ -n $ip ]] || { echo "local backend: no IP for $name on $net" >&2; return 1; }
      echo "$role" "$name" "$ip"
    done
  done
}

backend_ready() {
  local name=$1
  for _ in $(seq 1 30); do
    docker exec "$name" true 2>/dev/null && return 0
    sleep 1
  done
  echo "local backend: container $name never became ready" >&2
  return 1
}

backend_ssh() {
  local name=$1; shift
  docker exec "$name" bash -c "$*"
}

# Explicit stdin transport for secrets; never put their contents in argv.
backend_ssh_stdin() {
  local name=$1; shift
  docker exec -i "$name" bash -c "$*"
}

backend_scp() {
  local src=$1 name=$2 dst=$3
  docker cp "$src" "$name:$dst" >&2 || return 1
  docker exec "$name" chmod 0755 "$dst"
}

# Nothing to forward: controller-0 published 6443 at create time. Print no PID,
# which provision.sh reads as "this backend has no tunnel process to kill".
backend_tunnel() {
  local name=$1 port=$2
  [[ $port == "$LOCAL_API_PORT" ]] || {
    echo "local backend: asked to tunnel :$port but $name published :$LOCAL_API_PORT" >&2
    return 1
  }
  return 0
}
