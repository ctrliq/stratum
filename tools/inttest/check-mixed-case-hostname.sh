#!/usr/bin/env bash
# A control plane on a hostname with capital letters registers its Node under
# the lowercased short hostname, and stratum labels that Node control-plane.
#
# The kubelet lowercases the name it registers, even one given by
# --hostname-override. A node whose kubelet certificate and flag kept the
# hostname's case was refused by the Node authorizer and never registered, and
# `stratum up` gave up labelling it after ten minutes.
. "$(dirname "$0")/harness.sh"

# The container has its own UTS namespace, so this renames only the node.
hostname HK-Inttest-CP1
start_cluster
trap finish EXIT

node=hk-inttest-cp1
wait_for "node $node registration" 180 sh -c "$K get node $node"
pass "node registered under the lowercased hostname ($node)"

cn=$(openssl x509 -noout -subject -nameopt RFC2253 -in "$ROOT/pki/kubelet.crt")
assert_contains "kubelet certificate is for the lowercased name" "CN=system:node:$node" "$cn"

wait_for "control-plane label on $node" 120 sh -c \
  "$K get node $node -o jsonpath='{.metadata.labels.stratum\.ciq\.com/control-plane}' | grep -qx true"
pass "stratum labelled $node control-plane"

# The admission webhook's EndpointSlice names this Node in its endpoint. The
# apiserver refuses an endpoint whose nodeName is not a valid node name, so a
# mixed-case one left the webhook with no endpoint and HelmChart creates failed.
wait_for "webhook EndpointSlice naming $node" 120 sh -c \
  "$K -n kube-system get endpointslices -l kubernetes.io/service-name=stratum-operator-webhook -o jsonpath='{.items[*].endpoints[*].nodeName}' | grep -qw $node"
pass "the webhook's EndpointSlice names $node"

done_testing
