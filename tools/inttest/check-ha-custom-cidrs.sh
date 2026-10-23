#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# A joined control plane keeps the cluster's custom pod and service CIDRs across
# a restart.
#
# Every other HA check runs on the default CIDRs, so this is the only cover
# for custom ranges. A joined control plane has the cluster's CIDRs in netconfig
# and, as node_up writes it, a config that names none (only nodeIP and the VIP).
# A restart must not fill defaults in over netconfig: doing so makes the node
# render --service-cluster-ip-range and --cluster-cidr from them while the rest
# of the cluster keeps the custom ranges.
. "$(dirname "$0")/harness-ha.sh"
trap finish EXIT

# Not the defaults (10.245.0.0/16, 10.96.0.0/12), and clear of docker's subnet.
HA_POD_CIDR=10.42.0.0/16
HA_SERVICE_CIDR=10.43.0.0/16

ha_bring_up

apiserver_manifest=$ROOT/config/manifests/kube-apiserver.yaml
cm_manifest=$ROOT/config/manifests/kube-controller-manager.yaml

# assert_cidrs NODE WHEN — netconfig and the rendered control-plane flags.
assert_cidrs() {
  assert_eq "$2: $1 netconfig" "$HA_POD_CIDR $HA_SERVICE_CIDR" "$(on "$1" "tr '\n' ' ' < $ROOT/pki/netconfig | sed 's/ $//'")"
  assert_contains "$2: $1 apiserver service range" "--service-cluster-ip-range=$HA_SERVICE_CIDR" "$(on "$1" "cat $apiserver_manifest")"
  assert_contains "$2: $1 controller-manager cluster CIDR" "--cluster-cidr=$HA_POD_CIDR" "$(on "$1" "cat $cm_manifest")"
}

for n in $HA_NODES; do assert_cidrs "$n" "first boot"; done

# Restart a JOINED control plane the way a rebooted machine does: plain `up`,
# from its own state, with a config that names no pod or service CIDR.
victim=cp2
survivor=cp1
docker stop -t 5 "$HA_PREFIX-$victim" >/dev/null
docker start "$HA_PREFIX-$victim" >/dev/null
on "$victim" ": > /tmp/stratum.log"
HA_POD_CIDR='' node_up "$victim"
# Not a bare node_log: it ends in `|| true`, and the assertions below would then
# read the first boot's files and pass although the restart never got that far.
wait_for "$victim to restart" 600 node_started "$victim" || { node_log "$victim"; exit 1; }
assert_contains "$victim restarted from a config that names no CIDR" "nodeIP: $(ip_of "$victim")" "$(on "$victim" "cat /etc/stratum/config.yaml")"
if on "$victim" "grep -q 'podCIDR' /etc/stratum/config.yaml"; then fail "$victim's config still names a CIDR; the check is not testing the bug"; fi

assert_cidrs "$victim" "after restart"
if wait_for "3 nodes Ready again" 600 ready_at_least "$survivor" 3; then
  pass "3 control planes Ready after the restart"
else
  fail "not every node returned to Ready"
fi

done_testing
