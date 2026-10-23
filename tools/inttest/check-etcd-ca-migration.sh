#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# certs migrate-etcd-ca across three control planes.
#
# A cluster created before the etcd CA has etcd trust the cluster root for
# clients, so any kubelet's or admin's certificate is full etcd read/write.
# The cluster here is split from birth, so a forced rollback first takes it
# back to that state, with etcd's client trust carrying the root again, and
# the migration must leave every etcd refusing a root-signed certificate and
# accepting the apiserver's, with the cluster serving throughout. In every
# state etcd's peer port accepts only the etcd leaf.
. "$(dirname "$0")/harness-ha.sh"
trap finish EXIT

ha_bring_up
for n in $HA_NODES; do on "$n" "install -m 0755 /dist/stratum /usr/local/bin/stratum"; done

probe() { on cp1 "python3 - $(ip_of "$1") $ROOT/pki/$2.crt $ROOT/pki/$2.key ${3:-}" < "$(dirname "$0")/etcd-tls-probe.py"; }
check_etcd() {
  local n
  for n in $HA_NODES; do
    if [[ $2 == open ]]; then
      assert_contains "$n's etcd accepts admin's root-signed certificate $1" "HTTP/1.1 200" "$(probe "$n" admin)"
    else
      assert_contains "$n's etcd refuses admin's root-signed certificate $1" "rejected:" "$(probe "$n" admin)"
    fi
    assert_contains "$n's etcd accepts apiserver-etcd-client $1" "HTTP/1.1 200" "$(probe "$n" apiserver-etcd-client)"
    assert_contains "$n's etcd peer port accepts the etcd leaf $1" "HTTP/1.1 " "$(probe "$n" etcd 2380)"
    assert_contains "$n's etcd peer port refuses apiserver-etcd-client $1" "rejected:" "$(probe "$n" apiserver-etcd-client 2380)"
    assert_contains "$n's etcd peer port refuses admin's certificate $1" "rejected:" "$(probe "$n" admin 2380)"
    assert_posture "$n's etcd trust $1" "$n" etcd-trust "$3" "$([[ $3 == bad ]] && echo "from ca-bundle.crt")"
  done
  if on cp1 "$K get nodes" >/dev/null 2>&1; then pass "the API serves $1"; else fail "the API does not serve $1"; fi
  assert_eq "etcd has 3 started members $1" 3 "$(etcd_started cp1)"
}
migrate() {
  local out what="migrate-etcd-ca --cluster${*:+ $*}"
  if out=$(on cp1 "/usr/local/bin/stratum certs migrate-etcd-ca --cluster --yes $*" 2>&1); then
    pass "$what completed"
  else
    echo "$out" | tail -40
    for n in $HA_NODES; do node_log "$n"; done
    fail "$what failed"
    done_testing; exit 1
  fi
}

check_etcd "on a cluster split from birth" closed ok

pki_state() { on "$1" "cd $ROOT/pki && find . -path ./kubelet -prune -o -type f -print | sort | xargs sha256sum | sha256sum"; }
before=$(pki_state cp1)
if out=$(on cp1 "/usr/local/bin/stratum certs migrate-etcd-ca --cluster --rollback --yes" 2>&1); then
  fail "a rollback of a migrated cluster ran without --force"
else
  assert_contains "the rollback without --force names --force" "--force" "$out"
fi
assert_eq "the refused rollback changed nothing on cp1" "$before" "$(pki_state cp1)"

migrate --rollback --force
check_etcd "after the rollback" open bad

migrate
check_etcd "after the migration" closed ok

after=$(pki_state cp1)
migrate
assert_eq "a second migration changed nothing on cp1" "$after" "$(pki_state cp1)"
check_etcd "after a second migration" closed ok

done_testing
