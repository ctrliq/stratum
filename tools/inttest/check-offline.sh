#!/usr/bin/env bash
# ske-inttest: network=none
#
# The artifact installs with no network. This is the airgap claim as a test:
# the harness runs this case with --network none, so any registry contact fails.
. "$(dirname "$0")/harness.sh"

setup_cgroups
"$STRATUM" stage --root "$ROOT" >/dev/null
pass "payload staged with no network"

export PATH="$ROOT/bin:$PATH"
containerd --root "$ROOT/containerd" --state /run/containerd \
           --address /run/containerd/containerd.sock >/tmp/containerd.log 2>&1 &
wait_for "containerd socket" 60 test -S /run/containerd/containerd.sock
pass "containerd started offline"

for tar in "$ROOT/images/"*.tar; do
  ctr --address /run/containerd/containerd.sock -n k8s.io images import "$tar" >/dev/null
done
ctr --address /run/containerd/containerd.sock -n k8s.io images ls -q | grep -v '^sha256:' >/tmp/imported || true
missing=$(/tools/mirror-images.sh --list | cut -d@ -f1 | grep -cvxF -f /tmp/imported || true)
assert_eq "every pinned image imported from the payload" 0 "$missing"
dash=$(grep -c '/stratum-dash:' /tmp/imported || true)
assert_eq "the dashboard image imported from the payload" 1 "$dash"

# Running one proves the layers are complete, not merely present.
ctr --address /run/containerd/containerd.sock -n k8s.io run --detach \
    "$(/tools/mirror-images.sh --list | grep '/pause:' | cut -d@ -f1)" probe /pause
wait_for "sandbox running offline" 30 sh -c \
  "ctr --address /run/containerd/containerd.sock -n k8s.io tasks ls | grep -q RUNNING"
pass "ran a container from the payload with no network"

done_testing
trap finish EXIT
