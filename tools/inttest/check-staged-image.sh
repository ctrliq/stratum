#!/usr/bin/env bash
# An image an operator stages in /etc/stratum/images.d runs on a node that
# cannot reach its registry. The image is named under registry.invalid, which
# never resolves, so any pull of it fails: a pod that starts proves kubelet
# used the staged copy, with IfNotPresent and with Never, rather than asking
# the registry to verify credentials. The archive is staged after the node
# first started, so the restart imports it fresh and renders kubelet.conf
# again before kubelet starts. An archive that names an image but carries none
# of its blobs fails to import, and must exempt nothing.
. "$(dirname "$0")/harness.sh"

start_cluster
trap finish EXIT

wait_for "node registration" 120 sh -c "$K get nodes --no-headers | grep -q ."
node=$($K get nodes -o jsonpath='{.items[0].metadata.name}')
wait_for "node Ready" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
wait_for "default ServiceAccount" 60 sh -c "$K get sa default"

CTR="$ROOT/bin/ctr --address /run/stratum/containerd/containerd.sock -n k8s.io"
DROPIN=/etc/stratum/images.d
STAGED=registry.invalid/inttest/staged:v1
BROKEN=registry.invalid/inttest/broken:v1
case $(uname -m) in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; *) ARCH=$(uname -m) ;; esac
pause=$(/tools/mirror-images.sh --list | grep '/pause:' | cut -d@ -f1)

# --- stage an image no registry serves -------------------------------------------
mkdir -p "$DROPIN"
$CTR images tag "$pause" "$STAGED" >/dev/null
$CTR images export --platform "linux/$ARCH" "$DROPIN/staged.tar" "$STAGED"
$CTR images rm "$STAGED" >/dev/null
if $CTR images ls -q | grep -qxF "$STAGED"; then fail "$STAGED is still in containerd before the restart"; fi

broken=$(mktemp -d)
printf '{"imageLayoutVersion":"1.0.0"}' >"$broken/oci-layout"
printf '{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%064d","size":1,"annotations":{"io.containerd.image.name":"%s"}}]}' 0 "$BROKEN" >"$broken/index.json"
tar -cf "$DROPIN/broken.tar" -C "$broken" oci-layout index.json

stop_cluster
start_cluster
wait_for "node Ready after the restart" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"

# --- what the restart did with the archives ----------------------------------------
assert_contains "the restart imported the staged archive" "imported operator images from $DROPIN/staged.tar" "$(cat /tmp/stratum.log)"
assert_contains "the archive that cannot import was skipped" "skipping $DROPIN/broken.tar" "$(cat /tmp/stratum.log)"
labels=$($CTR images ls | grep -F "$STAGED " || true)
assert_contains "the staged image is pinned" "io.cri-containerd.pinned=pinned" "$labels"
conf=$(cat "$ROOT/config/kubelet.conf")
assert_contains "kubelet.conf exempts the staged image's repository" '"registry.invalid/inttest/staged"' "$conf"
if [[ $conf == *registry.invalid/inttest/broken* ]]; then
  fail "kubelet.conf exempts the repository of an archive that failed to import"
else
  pass "an archive that failed to import exempts nothing"
fi

# --- pods run the staged image without its registry ----------------------------------
running() { [[ $($K get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null) == Running ]]; }
state() { $K get pod "$1" -o jsonpath='{.status.containerStatuses[0].state}' 2>&1; }
for policy in IfNotPresent Never; do
  pod="staged-${policy,,}"
  $K run "$pod" --image="$STAGED" --image-pull-policy="$policy" --restart=Never >/dev/null
  if wait_for "a $policy pod runs the staged image" 120 running "$pod"; then
    pass "a pod with imagePullPolicy $policy runs the staged image with no registry"
  else
    fail "the $policy pod did not start: $(state "$pod")"
  fi
done

done_testing
