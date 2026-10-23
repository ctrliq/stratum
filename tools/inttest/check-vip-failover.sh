#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# Lose the node holding the VIP and assert the API stays reachable on the VIP.
#
# Reachable means TLS-verified: the new holder's apiserver must carry the VIP in
# its certificate. A joined control plane's can lack it, so the VIP "moves"
# and every client of it then fails the handshake.
. "$(dirname "$0")/harness-ha.sh"
trap finish EXIT

ha_bring_up

# Keep the holder the wait saw: reading it again can land in a kube-vip
# leadership handoff, when briefly no node holds the VIP.
holder=
has_holder() { holder=$(vip_holder); [[ -n $holder ]]; }
wait_for "a VIP holder" 120 has_holder || true
[[ -n $holder ]] || { fail "no node holds the VIP $HA_VIP"; done_testing; exit 1; }
pass "VIP held by $holder"

watcher=cp1; [[ $holder == cp1 ]] && watcher=cp2
docker stop -t 5 "$HA_PREFIX-$holder" >/dev/null
lost=$SECONDS
pass "stopped $holder"

moved() { local h; h=$(vip_holder); [[ -n $h && $h != "$holder" ]]; }
if wait_for "the VIP to move" 120 moved; then
  pass "VIP moved to $(vip_holder) in $((SECONDS - lost))s"
else
  fail "the VIP did not move off $holder"
fi
if wait_for "the API on the VIP" 120 api_via "$HA_VIP" "$watcher"; then
  pass "API answering on the VIP, TLS-verified, $((SECONDS - lost))s after losing $holder"
else
  fail "the API on the VIP did not come back"
fi
if on "$watcher" "$K --server https://$HA_VIP:6443 create configmap vip-failover-probe -n default --from-literal=at=\$(date +%s)" >/dev/null 2>&1; then
  pass "a write through the VIP succeeds after failover"
else
  fail "a write through the VIP failed after failover"
fi

done_testing
