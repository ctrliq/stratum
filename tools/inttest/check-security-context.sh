#!/usr/bin/env bash
# Pod and container securityContext fields take effect on a Stratum node, as
# listed in the docs' "Pod security contexts" table:
# https://kubernetes.io/docs/tasks/configure-pod-container/security-context/
#
# Each probe pod reports what the kernel says about itself -- its ids, its
# capability and no_new_privs bits, its seccomp mode, its uid map -- so a field
# the API server accepted but the runtime ignored fails here. Host-dependent
# features (AppArmor, user namespaces) are skipped with the reason when this
# host cannot provide them; SELinux is pinned to what it does today, so that
# turning it on has to update this check and the docs together.
. "$(dirname "$0")/harness.sh"

start_cluster
trap finish EXIT

# The name only once the Node exists: read earlier it can be empty, and the
# per-node lookups below would then ask about no node.
wait_for "node registration" 120 sh -c "$K get nodes --no-headers | grep -q ."
node=$($K get nodes -o jsonpath='{.items[0].metadata.name}')
wait_for "node Ready" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
pass "node Ready"
wait_for "default ServiceAccount" 60 sh -c "$K get sa default -n default"

# The payload's busybox, by its mirrored name: no registry exists here.
BB=$(/tools/mirror-images.sh --list | grep '/busybox:' | cut -d@ -f1)

skip() { echo "  skip  $*"; }

# probe NAME SPEC: runs a one-container pod whose spec (pod-level fields and a
# container named c) is SPEC and waits for it to finish. Its log is left in OUT
# and its phase in PHASE -- set here, so probe must not run in a subshell.
probe() {
  local name=$1 spec=$2
  $K delete pod "$name" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  $K apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: $name}
spec:
  restartPolicy: Never
$spec
EOF
  PHASE=
  for _ in $(seq 1 120); do
    PHASE=$($K get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [[ $PHASE == Succeeded || $PHASE == Failed ]] && break
    # A container that cannot be created never leaves Pending: stop waiting.
    if $K get pod "$name" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null |
      grep -qE 'CreateContainerError|CreateContainerConfigError|RunContainerError'; then
      PHASE=CreateFailed
      break
    fi
    sleep 1
  done
  OUT=$($K logs "$name" 2>/dev/null || true)
}

# why NAME: the pod's own explanation, for a probe that did not run.
why() {
  {
    $K get pod "$1" -o jsonpath='{.status.containerStatuses[0].state.waiting.message}{" "}{.status.message}' 2>/dev/null || true
    echo
    $K get events --field-selector "involvedObject.name=$1" -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null | tail -2
  } | sed '/^ *$/d' | paste -sd';' -
}

capbit() {  # hex CapEff value, bit -> 0|1
  echo $(((16#$1 >> $2) & 1))
}

# --- runAsUser, runAsGroup, supplementalGroups, fsGroup ----------------------
probe sc-ids "  securityContext: {runAsUser: 1000, runAsGroup: 3000, supplementalGroups: [4000], fsGroup: 2000}
  volumes: [{name: data, emptyDir: {}}]
  containers:
  - name: c
    image: $BB
    command: [sh, -c, 'id; touch /data/f && stat -c \"file-gid=%g\" /data/f; stat -c \"dir-mode=%A\" /data']
    volumeMounts: [{name: data, mountPath: /data}]"
out=$OUT
assert_eq "runAsUser/runAsGroup pod ran" Succeeded "$PHASE"
assert_contains "runAsUser sets the uid" "uid=1000" "$out"
assert_contains "runAsGroup sets the gid" "gid=3000" "$out"
assert_contains "supplementalGroups adds 4000" "4000" "$(grep -o 'groups=.*' <<<"$out")"
assert_contains "fsGroup adds 2000 to the process" "2000" "$(grep -o 'groups=.*' <<<"$out")"
assert_contains "fsGroup owns new files in the volume" "file-gid=2000" "$out"
assert_contains "fsGroup sets the setgid bit on the volume" "s" "$(sed -n 's/^dir-mode=......\(.\).*/\1/p' <<<"$out")"

# --- runAsNonRoot -------------------------------------------------------------
# The payload's busybox runs as root, so the kubelet must refuse to start it.
probe sc-nonroot-refused "  securityContext: {runAsNonRoot: true}
  containers: [{name: c, image: $BB, command: [id]}]"
if [[ $PHASE == CreateFailed ]]; then
  assert_contains "runAsNonRoot refuses an image that runs as root" "image will run as root" "$(why sc-nonroot-refused)"
else
  fail "runAsNonRoot did not stop a root container (phase $PHASE): $OUT"
fi
probe sc-nonroot-ok "  securityContext: {runAsNonRoot: true, runAsUser: 1000}
  containers: [{name: c, image: $BB, command: [id, -u]}]"
assert_eq "runAsNonRoot admits a non-root user" 1000 "$OUT"

# --- fsGroupChangePolicy ------------------------------------------------------
probe sc-fsgroup-policy "  securityContext: {runAsUser: 1000, fsGroup: 2000, fsGroupChangePolicy: OnRootMismatch}
  volumes: [{name: data, emptyDir: {}}]
  containers:
  - name: c
    image: $BB
    command: [sh, -c, 'stat -c \"root-gid=%g\" /data']
    volumeMounts: [{name: data, mountPath: /data}]"
# Accepted, and the volume root still takes the fsGroup. Whether OnRootMismatch
# skips the recursive change is not shown: that needs a volume whose root
# already matches, and the volumes that start out populated here (local-path
# claims) are hostPath, which fsGroup does not touch at all.
assert_eq "fsGroupChangePolicy OnRootMismatch is accepted" Succeeded "$PHASE"
assert_contains "…and the volume root still takes the fsGroup" "root-gid=2000" "$OUT"

# --- supplementalGroupsPolicy -------------------------------------------------
# Merge adds the groups /etc/group in the image gives the user; Strict must not.
# Root in the payload's busybox belongs to groups beyond 0, which is the
# difference measured.
features=$($K get node "$node" -o jsonpath='{.status.features.supplementalGroupsPolicy}' 2>/dev/null || true)
# Not host-dependent: the kubelet and containerd Stratum ships either support
# the field or the docs are wrong, so a node that does not report it fails.
if [[ $features != true ]]; then
  fail "supplementalGroupsPolicy: the node does not report support (status.features.supplementalGroupsPolicy='$features')"
else
  probe sc-sgp-merge "  securityContext: {runAsUser: 0, runAsGroup: 0, supplementalGroupsPolicy: Merge}
  containers: [{name: c, image: $BB, command: [id, -G]}]"
  merge=$OUT
  probe sc-sgp-strict "  securityContext: {runAsUser: 0, runAsGroup: 0, supplementalGroupsPolicy: Strict}
  containers: [{name: c, image: $BB, command: [id, -G]}]"
  strict=$OUT
  # The payload's busybox gives root more groups than 0, so Merge must show
  # them; equal output means the field was ignored, not that it cannot be told
  # apart here.
  if [[ $merge == 0 ]]; then
    fail "supplementalGroupsPolicy Merge did not add the image's groups for root (got '$merge'); the fixture changed or Merge is ignored"
  fi
  assert_eq "supplementalGroupsPolicy Strict drops the image's groups (Merge gave: $merge)" 0 "$strict"
fi

# --- capabilities, allowPrivilegeEscalation, readOnlyRootFilesystem ----------
probe sc-caps "  containers:
  - name: c
    image: $BB
    command: [sh, -c, 'grep -E \"^(CapEff|NoNewPrivs):\" /proc/self/status; touch /probe 2>/dev/null && echo rootfs-writable || echo rootfs-readonly']
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities: {drop: [ALL], add: [NET_ADMIN]}"
out=$OUT
assert_eq "capabilities pod ran" Succeeded "$PHASE"
capeff=$(sed -n 's/^CapEff:[[:space:]]*//p' <<<"$out")
assert_eq "drop ALL + add NET_ADMIN leaves exactly CAP_NET_ADMIN" 0000000000001000 "$capeff"
assert_contains "allowPrivilegeEscalation: false sets no_new_privs" "1" "$(sed -n 's/^NoNewPrivs:[[:space:]]*//p' <<<"$out")"
assert_contains "readOnlyRootFilesystem makes / read-only" "rootfs-readonly" "$out"

# --- privileged ---------------------------------------------------------------
probe sc-priv "  containers:
  - name: c
    image: $BB
    command: [sh, -c, 'grep \"^CapEff:\" /proc/self/status']
    securityContext: {privileged: true}"
out=$OUT
assert_eq "privileged pod ran" Succeeded "$PHASE"
capeff=$(sed -n 's/^CapEff:[[:space:]]*//p' <<<"$out")
if [[ $capeff =~ ^[0-9a-f]+$ ]] && [[ $(capbit "$capeff" 21) == 1 ]]; then
  pass "privileged grants CAP_SYS_ADMIN ($capeff)"
else
  fail "privileged pod lacks CAP_SYS_ADMIN: CapEff=$capeff"
fi

# --- seccomp: RuntimeDefault, Unconfined, Localhost ---------------------------
probe sc-seccomp-default "  securityContext: {seccompProfile: {type: RuntimeDefault}}
  containers: [{name: c, image: $BB, command: [sh, -c, 'grep \"^Seccomp:\" /proc/self/status']}]"
out=$OUT
assert_contains "seccompProfile RuntimeDefault filters syscalls (Seccomp: 2)" "2" "$(sed -n 's/^Seccomp:[[:space:]]*//p' <<<"$out")"

probe sc-seccomp-unconfined "  securityContext: {seccompProfile: {type: Unconfined}}
  containers: [{name: c, image: $BB, command: [sh, -c, 'grep \"^Seccomp:\" /proc/self/status']}]"
out=$OUT
assert_eq "seccompProfile Unconfined runs unfiltered" 0 "$(sed -n 's/^Seccomp:[[:space:]]*//p' <<<"$out")"

# Localhost profiles live under the kubelet's root dir, which on Stratum is
# $ROOT/kubelet -- not the /var/lib/kubelet the upstream page uses.
mkdir -p "$ROOT/kubelet/seccomp/stratum-test"
cat >"$ROOT/kubelet/seccomp/stratum-test/deny-mkdir.json" <<'EOF'
{"defaultAction": "SCMP_ACT_ALLOW",
 "syscalls": [{"names": ["mkdir", "mkdirat"], "action": "SCMP_ACT_ERRNO"}]}
EOF
probe sc-seccomp-localhost "  securityContext: {seccompProfile: {type: Localhost, localhostProfile: stratum-test/deny-mkdir.json}}
  containers: [{name: c, image: $BB, command: [sh, -c, 'mkdir /tmp/probe 2>/dev/null && echo mkdir-allowed || echo mkdir-denied']}]"
out=$OUT
if [[ $PHASE == Succeeded ]]; then
  assert_contains "a Localhost profile under $ROOT/kubelet/seccomp is enforced" "mkdir-denied" "$out"
else
  fail "Localhost seccomp profile pod did not run ($PHASE): $(why sc-seccomp-localhost)"
fi

# --- AppArmor (host-dependent) -----------------------------------------------
if [[ $(cat /sys/module/apparmor/parameters/enabled 2>/dev/null) != Y ]]; then
  skip "appArmorProfile: this kernel has no AppArmor (e.g. Rocky/RHEL, which use SELinux)"
else
  probe sc-apparmor "  securityContext: {appArmorProfile: {type: RuntimeDefault}}
  containers: [{name: c, image: $BB, command: [sh, -c, 'cat /proc/self/attr/apparmor/current 2>/dev/null || cat /proc/self/attr/current']}]"
out=$OUT
  reason=$(why sc-apparmor)
  if [[ $PHASE == Succeeded ]]; then
    assert_contains "appArmorProfile RuntimeDefault confines the container" "cri-containerd.apparmor.d" "$out"
  elif [[ $reason == *"AppArmor is not enabled on the host"* ]]; then
    # The kernel has AppArmor, but from inside this container the kubelet
    # cannot see securityfs. Only this condition is the host's; any other
    # failure is the field not working.
    skip "appArmorProfile: the kernel has AppArmor, but the kubelet cannot use it from this container: $reason"
  else
    fail "appArmorProfile RuntimeDefault pod did not run ($PHASE): $reason"
  fi
fi

# --- user namespaces (host-dependent) -----------------------------------------
probe sc-userns "  hostUsers: false
  containers: [{name: c, image: $BB, command: [sh, -c, 'cat /proc/self/uid_map']}]"
out=$OUT
if [[ $PHASE == Succeeded ]]; then
  # Compared with the map of the namespace this script and the kubelet run in,
  # not with the identity map: under rootless or userns-remapped Docker that
  # namespace is already remapped, and an ordinary pod would match it.
  base=$(tr -s ' ' </proc/self/uid_map | sed 's/^ //' | head -1)
  got=$(tr -s ' ' <<<"$out" | sed 's/^ //' | head -1)
  if [[ -n $got && $got != "$base" ]]; then
    pass "hostUsers: false runs the pod in its own user namespace ($got, host side $base)"
    userns_ok=yes
  else
    fail "hostUsers: false pod shares the kubelet's user namespace: pod '$got', kubelet '$base'"
  fi
else
  # Skipped only where the host cannot provide pod user namespaces: a kernel
  # before 6.3 (no idmapped tmpfs), or the runtime saying it is unsupported.
  # Any other failure is the field not working.
  reason=$(why sc-userns)
  kver=$(uname -r | grep -oE '^[0-9]+\.[0-9]+')
  if [[ $(printf '%s\n' "$kver" 6.3 | sort -V | head -1) != 6.3 ]] ||
    grep -qiE 'not supported|unsupported|idmap' <<<"$reason"; then
    skip "hostUsers: false: this host cannot provide user namespaces (kernel $(uname -r)): $reason"
  else
    fail "hostUsers: false pod did not run on kernel $(uname -r) ($PHASE): $reason"
  fi
fi

# --- procMount ----------------------------------------------------------------
# The runtime masks sensitive /proc paths by mounting over them; /proc/kcore is
# one. Unmasked leaves them as they are, and is allowed only with hostUsers:
# false, so both pods run in a user namespace and differ only in procMount.
if [[ ${userns_ok:-} == yes ]]; then
  masks() {  # pod name, procMount -> the pod's /proc/kcore mount count
    probe "$1" "  hostUsers: false
  containers:
  - name: c
    image: $BB
    command: [sh, -c, 'grep -c \" /proc/kcore \" /proc/self/mountinfo || true']
    securityContext: {procMount: $2}"
  }
  masks sc-procmount-default Default
  default_masks=$OUT default_phase=$PHASE
  masks sc-procmount-unmasked Unmasked
  if [[ $default_phase != Succeeded || $PHASE != Succeeded ]]; then
    fail "procMount pods did not run (Default: $default_phase, Unmasked: $PHASE): $(why sc-procmount-unmasked)"
  else
    assert_eq "procMount Default masks /proc/kcore" 1 "$default_masks"
    assert_eq "procMount Unmasked leaves /proc/kcore unmasked" 0 "$OUT"
  fi
else
  skip "procMount: needs hostUsers: false, which this host cannot provide"
fi

# --- SELinux: accepted, not applied ---------------------------------
# containerd runs with enable_selinux = false, so seLinuxOptions are accepted
# and ignored. Pinned here so that changing it changes this check and the docs.
probe sc-selinux "  securityContext: {seLinuxOptions: {level: 's0:c123,c456'}}
  containers: [{name: c, image: $BB, command: [sh, -c, 'cat /proc/self/attr/current 2>/dev/null; echo']}]"
out=$OUT
if [[ $PHASE != Succeeded ]]; then
  fail "seLinuxOptions pod did not run ($PHASE): $(why sc-selinux)"
elif [[ $out == *c123,c456* ]]; then
  fail "seLinuxOptions now label the container ($out): SELinux support changed -- update this check and the hardening docs"
else
  pass "seLinuxOptions accepted but not applied, as documented"
fi

# seLinuxChangePolicy chooses how volumes are relabelled for the pod's SELinux
# context. Both values must be accepted and the pod must run; with labelling
# off there is nothing to relabel, which is what the docs say.
for policy in MountOption Recursive; do
  probe "sc-selinux-change-$(tr '[:upper:]' '[:lower:]' <<<"$policy")" "  securityContext: {seLinuxChangePolicy: $policy, seLinuxOptions: {level: 's0:c123,c456'}}
  volumes: [{name: data, emptyDir: {}}]
  containers:
  - name: c
    image: $BB
    command: [sh, -c, 'touch /data/f && echo wrote']
    volumeMounts: [{name: data, mountPath: /data}]"
  if [[ $PHASE == Succeeded && $OUT == wrote ]]; then
    pass "seLinuxChangePolicy $policy is accepted and the pod runs"
  else
    fail "seLinuxChangePolicy $policy pod did not run ($PHASE): $OUT $(why "sc-selinux-change-$(tr '[:upper:]' '[:lower:]' <<<"$policy")")"
  fi
done

done_testing
