#!/usr/bin/env bash
# A payload image stays usable after kubelet pulls the same image under its
# upstream name. The pull leaves kubelet a credential record for the shared
# image ID that names only the upstream image; without the payload registry on
# kubelet's verification allowlist, a later pod using the payload name sends
# kubelet to that registry for an image already on the node, and an airgapped
# or credential-less node never starts the pod.
. "$(dirname "$0")/harness.sh"

start_cluster
trap finish EXIT

wait_for "node registration" 120 sh -c "$K get nodes --no-headers | grep -q ."
node=$($K get nodes -o jsonpath='{.items[0].metadata.name}')
wait_for "node Ready" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
wait_for "default ServiceAccount" 60 sh -c "$K get sa default"

read -r upstream payload < <(/tools/mirror-images.sh --pairs | grep '/pause:')
upstream=${upstream%@*}
payload=${payload%@*}

running() { [[ $($K get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null) == Running ]]; }
start_pod() { $K run "$1" --image="$2" --image-pull-policy=IfNotPresent --restart=Never >/dev/null; }

start_pod upstream-pull "$upstream"
if wait_for "kubelet pulls $upstream" 180 running upstream-pull; then
  pass "kubelet pulled the upstream image that shares the payload image's digest"
else
  fail "the pod using $upstream did not start: $($K get pod upstream-pull -o jsonpath='{.status.containerStatuses[0].state}')"
fi

start_pod payload-after "$payload"
if wait_for "a pod using $payload starts" 120 running payload-after; then
  pass "the payload image is still used from the node after the upstream pull"
else
  fail "the pod using $payload did not start after the upstream pull: $($K get pod payload-after -o jsonpath='{.status.containerStatuses[0].state}')"
fi
assert_contains "kubelet used the payload image already on the node" "already present on machine" \
  "$($K get events --field-selector involvedObject.name=payload-after,reason=Pulled -o jsonpath='{.items[*].message}')"

done_testing
