#!/usr/bin/env bash
# certs rotate-ca --cluster --force recovers a control plane that lost the
# root CA's key.
#
# A trust bundle holding only the new root strands the node: every
# certificate on it is still signed by the old one, so from the trust-new
# restart nothing can verify anything — the kubelet, the controller-manager and
# the operator all fail TLS, the run halts, and the node stays down. The
# rotation therefore carries the old root's certificate, which is public and
# still on disk, so trust-new is old‖new like any rotation, and until sign-new
# the staged root signs in place of the lost key.
#
# Without the old root's certificate there is nothing to bridge trust with, so
# that case must be refused before anything changes. A forced rotation cannot
# be rolled back, and a halted one resumes with --cluster --force: this stages
# trust-new by hand, has the rollback refused, and resumes.
. "$(dirname "$0")/harness.sh"

start_cluster
trap finish EXIT
wait_for "node Ready" 300 sh -c "$K get nodes --no-headers | grep -qw Ready"

pki=$ROOT/pki
recovery_dump() {
  local kc="timeout 15 $K"
  echo "--- kube-root-ca.crt (managed fields) ---"
  $kc -n default get configmap kube-root-ca.crt -o yaml --show-managed-fields 2>&1 | head -60 || true
  for f in /var/log/pods/kube-system_kube-controller-manager*/*/*.log; do
    [[ -f $f ]] || continue
    echo "--- $f ---"; tail -80 "$f" || true
  done
  echo "--- $pki ---"; ls -la "$pki" || true
  echo "--- signing-key flags ---"
  grep -hE -- '--(cluster-signing|service-account)-' "$ROOT"/config/manifests/kube-*.yaml || true
}
pki_state() { (cd "$pki" && find . -path ./kubelet -prune -o -type f -print | sort | xargs sha256sum); }
before=$(sha256sum "$pki/ca.crt" | cut -d' ' -f1)

mv "$pki/ca.crt" "$pki/ca.crt.aside"
state=$(pki_state)
if out=$("$STRATUM" certs rotate-ca --cluster --force --yes --restart-workloads 2>&1); then
  fail "a forced rotation ran without the old root's certificate: $(tail -5 <<<"$out")"
else
  pass "a forced rotation without the old root's certificate was refused"
fi
for want in "refusing to rotate (nothing has been changed)" "ca.crt is missing" "copy ca.crt from another control plane"; do
  assert_contains "the refusal says: $want" "$want" "$out"
done
if [[ $(pki_state) == "$state" ]]; then pass "the refusal changed no PKI file"; else fail "the refusal changed the PKI"; fi
mv "$pki/ca.crt.aside" "$pki/ca.crt"

rm "$pki/ca.key"
if out=$("$STRATUM" certs rotate-ca --phase=trust-new --force --yes 2>&1); then
  pass "a forced trust-new ran without the old root key"
else
  echo "$out" | tail -20
  recovery_dump
  fail "the forced trust-new failed"
  done_testing; exit 1
fi
state=$(pki_state)
if out=$("$STRATUM" certs rotate-ca --rollback --yes 2>&1); then
  fail "a rotation started with --force was rolled back: $(tail -5 <<<"$out")"
else
  assert_contains "the rollback of a forced rotation is refused" "this rotation was started with --force" "$out"
fi
if [[ $(pki_state) == "$state" ]]; then pass "the refused rollback changed no PKI file"; else fail "the refused rollback changed the PKI"; fi

if out=$("$STRATUM" certs rotate-ca --cluster --force --yes --restart-workloads 2>&1); then
  pass "the forced recovery resumed from trust-new and completed"
  assert_contains "the run said it was resuming" "Resuming the forced rotation" "$out"
else
  echo "$out" | tail -40
  $K get pods -A 2>&1 | tail -20 || true
  recovery_dump
  fail "the forced recovery failed"
  done_testing; exit 1
fi

after=$(sha256sum "$pki/ca.crt" | cut -d' ' -f1)
if [[ $after != "$before" ]]; then pass "the node is on a new root"; else fail "the root did not change"; fi
if [[ -f $pki/ca.key ]]; then pass "the new root's key is in place"; else fail "no ca.key after the recovery"; fi
assert_eq "the trust bundle is the new root alone" "$after" "$(sha256sum "$pki/ca-bundle.crt" | cut -d' ' -f1)"
if grep -q -- "--cluster-signing-key-file=$pki/ca.key" "$ROOT/config/manifests/kube-controller-manager.yaml"; then
  pass "the controller-manager signs with ca.key again"
else
  fail "the controller-manager does not sign with ca.key after the recovery"
fi
wait_for "node Ready after the recovery" 300 sh -c "$K get nodes --no-headers | grep -qw Ready"
root_ca_is_new() { [[ $($K -n default get configmap kube-root-ca.crt -o jsonpath='{.data.ca\.crt}') == "$(cat "$pki/ca.crt")" ]]; }
if wait_for "kube-root-ca.crt to carry the new root alone" 300 root_ca_is_new; then
  pass "kube-root-ca.crt carries the new root alone"
else
  recovery_dump
  fail "kube-root-ca.crt does not carry the new root alone"
fi

done_testing
