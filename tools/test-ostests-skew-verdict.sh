#!/usr/bin/env bash
# Deterministic coverage for run-skew.sh's VERDICT, not its pin gate
# (test-ostests-skew.sh already owns the pins). This is the part the pin-gate
# test cannot reach: once a cluster exists and the swap is done, does the
# script actually fail a skewed kubelet the scheduler won't use, and actually
# pass one it will?
#
# Runs the real run-skew.sh end to end with fake provision.sh, a fake
# PROVISION_BACKEND, and a fake kubectl/curl/sleep standing in for the cluster
# and dl.k8s.io — same fixture-tree technique test-ostests-skew.sh uses, one
# step further: the whole script runs, not just --check-pins. A happy-path-only
# run would pass even if skew_failed's callers were deleted; the FAIL scenario
# is what actually exercises that path.
set -euo pipefail
cd "$(dirname "$0")/.."

WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT

K8S_VERSION=1.30.5
SKEW_VERSION=1.27.3
NODE=skew-worker-0
APISERVER=v$K8S_VERSION
AFTER=v$SKEW_VERSION
KUBELET_BLOB=fake-kubelet-binary
sha256() { if command -v sha256sum >/dev/null; then sha256sum | cut -d' ' -f1; else shasum -a 256 | cut -d' ' -f1; fi; }
DIGEST=$(printf '%s' "$KUBELET_BLOB" | sha256)

mkdir -p "$WORK/build" "$WORK/tools/ostests/backends" "$WORK/bin" "$WORK/dist"
cat >"$WORK/build/versions.mk" <<EOF
kubernetes_version = $K8S_VERSION
skew_kubelet_version = $SKEW_VERSION
skew_kubelet_sha256_linux_amd64 = $DIGEST
skew_kubelet_sha256_linux_arm64 = $DIGEST
EOF
: >"$WORK/dist/stratum"
cp tools/ostests/run-skew.sh "$WORK/tools/ostests/run-skew.sh"
ORIG_MANIFEST=$(cat "$WORK/build/versions.mk")

# Fake provision.sh: no cloud, no artifact copy — just the nodes file and a
# kubeconfig path, the only two things run-skew.sh reads from it.
cat >"$WORK/tools/ostests/provision.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
: "\${OUT:?}"
mkdir -p "\$OUT"
printf 'controller fake-cp1 10.0.0.2\nworker fake-worker-0 10.0.0.9\n' >"\$OUT/nodes"
: >"\$OUT/admin.conf"
echo "\$OUT/admin.conf"
EOF
chmod +x "$WORK/tools/ostests/provision.sh"

# Fake backend: the swap commands always "succeed" — what's under test here is
# the verdict logic that runs AFTER the swap, not the swap transport itself.
cat >"$WORK/tools/ostests/backends/fake.sh" <<EOF
backend_ssh() {
  local _handle=\$1; shift
  case "\$*" in
    "hostname -s") echo "$NODE" ;;
    *) return 0 ;;
  esac
}
backend_scp() { return 0; }
EOF

cat >"$WORK/tools/mirror-images.sh" <<'EOF'
#!/usr/bin/env bash
echo "registry.example/library/busybox:1.36@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
EOF
chmod +x "$WORK/tools/mirror-images.sh"

cat >"$WORK/bin/sleep" <<'EOF'
#!/bin/sh
exit 0
EOF
cat >"$WORK/bin/curl" <<EOF
#!/usr/bin/env bash
out=
while [[ \$# -gt 0 ]]; do
  if [[ \$1 == -o ]]; then out=\$2; shift; fi
  shift
done
[[ -n \$out ]] || { echo "fake curl: no -o target" >&2; exit 1; }
printf '%s' '$KUBELET_BLOB' >"\$out"
EOF

# Fake kubectl: a node whose kubeletVersion is asked for twice — once BEFORE
# the swap (the live cluster's starting point, counter call 1) and once in the
# post-swap wait loop (counter call 2+, where the skewed version actually
# shows up) — plus a pod whose placement/phase/logs the test controls via
# FAKE_PLACED/FAKE_PHASE/FAKE_LOGS so one run proves PASS and another proves FAIL.
cat >"$WORK/bin/kubectl" <<EOF
#!/usr/bin/env bash
set -euo pipefail
COUNTER_FILE="\$FAKE_STATE/kubelet-calls"
case "\$1" in
  version) echo '{"serverVersion":{"gitVersion":"$APISERVER"}}' ;;
  get)
    case "\$2" in
      node)
        node=\$3
        out=
        for a in "\$@"; do :; done
        i=3
        args=("\$@"); n=\${#args[@]}
        while (( i < n )); do
          [[ \${args[\$i]} == -o ]] && out=\${args[\$((i+1))]}
          i=\$((i+1))
        done
        if [[ -z \$out ]]; then
          [[ \$node == "$NODE" ]] && exit 0 || exit 1
        fi
        case \$out in
          *kubeletVersion*)
            if [[ -n \${FAKE_NEVER_READY:-} ]]; then
              echo "$APISERVER"   # the swap never takes — same version forever
            else
              n=\$(( \$(cat "\$COUNTER_FILE" 2>/dev/null || echo 0) + 1 ))
              echo "\$n" >"\$COUNTER_FILE"
              if (( n == 1 )); then echo "$APISERVER"; else echo "$AFTER"; fi
            fi
            ;;
          *conditions*) echo True ;;
        esac
        ;;
      pod)
        args=("\$@"); n=\${#args[@]}; out=
        i=3
        while (( i < n )); do
          [[ \${args[\$i]} == -o ]] && out=\${args[\$((i+1))]}
          i=\$((i+1))
        done
        case \$out in
          *phase*) echo "\${FAKE_PHASE:-Succeeded}" ;;
          *nodeName*) echo "\${FAKE_PLACED-$NODE}" ;;
        esac
        ;;
    esac
    ;;
  delete) exit 0 ;;
  apply) cat >/dev/null ;;
  logs) echo "\${FAKE_LOGS:-skewed-kubelet-ran-this}" ;;
  describe) echo "fake kubectl describe" ;;
  *) echo "fake kubectl: unhandled '\$*'" >&2; exit 1 ;;
esac
EOF
chmod +x "$WORK/bin/sleep" "$WORK/bin/curl" "$WORK/bin/kubectl"

# The evidence file the weekly workflow uploads with if: always() — same path
# run-skew.sh writes on both the pass and the fail path, cwd-relative like
# everything else here, so it lands at $WORK/_output/skew/report.txt.
REPORT="$WORK/_output/skew/report.txt"

run() {  # FAKE_PHASE FAKE_PLACED FAKE_LOGS [FAKE_NEVER_READY] -> stdout+stderr, sets RC
  local phase=$1 placed=$2 logs=$3 never_ready=${4:-}
  local state; state=$(mktemp -d)
  local out_dir; out_dir=$(mktemp -d)
  rm -f "$REPORT"
  set +e
  OUTPUT=$(cd "$WORK" && \
    PATH="$WORK/bin:$PATH" \
    CLUSTER_NAME=verdict-test PROVISION_BACKEND=fake ARCH=amd64 \
    OUT="$out_dir" FAKE_STATE="$state" \
    FAKE_PHASE="$phase" FAKE_PLACED="$placed" FAKE_LOGS="$logs" \
    FAKE_NEVER_READY="$never_ready" \
    tools/ostests/run-skew.sh dist/stratum 2>&1)
  RC=$?
  set -e
  rm -rf "$state" "$out_dir"
}

# status, reason-substring ("" means the reason line must be empty)
check_report() {
  local want_status=$1 want_reason=$2
  [[ -r $REPORT ]] || { echo "  FAIL  no $REPORT — the evidence a failing weekly run needs to upload" >&2; fail=1; return; }
  grep -qx "status: $want_status" "$REPORT" \
    || { echo "  FAIL  report status wrong:" >&2; cat "$REPORT" >&2; fail=1; }
  grep -q "^apiserver_version: v$K8S_VERSION$" "$REPORT" \
    || { echo "  FAIL  report lost the apiserver version:" >&2; cat "$REPORT" >&2; fail=1; }
  grep -q "^kubelet_version: $AFTER$" "$REPORT" \
    || { echo "  FAIL  report lost the kubelet version:" >&2; cat "$REPORT" >&2; fail=1; }
  if [[ -z $want_reason ]]; then
    grep -qx "reason: " "$REPORT" || { echo "  FAIL  pass report carries a reason:" >&2; cat "$REPORT" >&2; fail=1; }
  else
    grep -q "reason:.*$want_reason" "$REPORT" \
      || { echo "  FAIL  fail report lost its reason:" >&2; cat "$REPORT" >&2; fail=1; }
  fi
}

fail=0

run Succeeded "$NODE" skewed-kubelet-ran-this
if [[ $RC -eq 0 ]]; then
  echo "  ok    a healthy skewed kubelet passes"
else
  echo "  FAIL  healthy run exited $RC:" >&2; echo "$OUTPUT" >&2; fail=1
fi
[[ $OUTPUT == *"run-skew: ok"* ]] || { echo "  FAIL  healthy run printed no verdict line" >&2; fail=1; }
check_report pass ""

run Pending "" skewed-kubelet-ran-this
if [[ $RC -ne 0 ]]; then
  echo "  ok    a probe never placed on the skewed node fails the script"
else
  echo "  FAIL  unplaced-probe run exited 0 — a failing skewed kubelet did not fail the script" >&2; fail=1
fi
[[ $OUTPUT == *"the scheduler would not use a kubelet"* ]] \
  || { echo "  FAIL  unplaced-probe run lost its diagnostic:" >&2; echo "$OUTPUT" >&2; fail=1; }
check_report fail "the scheduler would not use a kubelet"

run Failed "$NODE" skewed-kubelet-ran-this
if [[ $RC -ne 0 ]]; then
  echo "  ok    a probe that lands but does not Succeed fails the script"
else
  echo "  FAIL  Failed-phase run exited 0" >&2; fail=1
fi
[[ $OUTPUT == *"could not run a pod"* ]] \
  || { echo "  FAIL  Failed-phase run lost its diagnostic:" >&2; echo "$OUTPUT" >&2; fail=1; }
check_report fail "could not run a pod"

run Succeeded "$NODE" wrong-output
if [[ $RC -ne 0 ]]; then
  echo "  ok    a probe that ran something else fails the script"
else
  echo "  FAIL  wrong-output run exited 0" >&2; fail=1
fi
[[ $OUTPUT == *"the container did not actually run"* ]] \
  || { echo "  FAIL  wrong-output run lost its diagnostic:" >&2; echo "$OUTPUT" >&2; fail=1; }
check_report fail "the container did not actually run"

# A kubelet that never comes back Ready dies BEFORE skew_failed is ever
# reached, straight out of the Ready-wait die(). No per-path write_report call
# covers this one — only the generic EXIT trap does, and this is what proves
# it actually fires.
run Succeeded "$NODE" skewed-kubelet-ran-this 1
if [[ $RC -ne 0 ]]; then
  echo "  ok    a kubelet that never comes back Ready fails the script"
else
  echo "  FAIL  never-ready run exited 0" >&2; fail=1
fi
[[ $OUTPUT == *"the swapped binary never came up"* ]] \
  || { echo "  FAIL  never-ready run lost its diagnostic:" >&2; echo "$OUTPUT" >&2; fail=1; }
[[ -r $REPORT ]] \
  || { echo "  FAIL  no $REPORT for the never-ready failure — the generic EXIT trap must fire even here" >&2; fail=1; }
grep -qx "status: fail" "$REPORT" \
  || { echo "  FAIL  never-ready report status wrong:" >&2; cat "$REPORT" >&2; fail=1; }
grep -qx "apiserver_version: " "$REPORT" \
  || { echo "  FAIL  never-ready report should have no apiserver version (never reached that check):" >&2; cat "$REPORT" >&2; fail=1; }
grep -qx "kubelet_version: $APISERVER" "$REPORT" \
  || { echo "  FAIL  never-ready report lost the stuck kubelet version:" >&2; cat "$REPORT" >&2; fail=1; }
grep -q "reason:.*before recording a verdict" "$REPORT" \
  || { echo "  FAIL  never-ready report lost its generic reason:" >&2; cat "$REPORT" >&2; fail=1; }

# A bad pin dies before ANY of the preflight/provisioning runs, let alone a
# cluster — proving the trap is installed at the very top of the file, not
# merely before provisioning.
run_with_manifest() {  # manifest-content, then run()'s usual args
  local content=$1; shift
  printf '%s\n' "$content" >"$WORK/build/versions.mk"
  run "$@"
  printf '%s\n' "$ORIG_MANIFEST" >"$WORK/build/versions.mk"
}
BAD_MANIFEST="kubernetes_version = $K8S_VERSION
skew_kubelet_version = $SKEW_VERSION
skew_kubelet_sha256_linux_amd64 = not-a-digest
skew_kubelet_sha256_linux_arm64 = $DIGEST"

run_with_manifest "$BAD_MANIFEST" Succeeded "$NODE" skewed-kubelet-ran-this
if [[ $RC -ne 0 ]]; then
  echo "  ok    a bad pin, before anything is provisioned, still fails the script"
else
  echo "  FAIL  bad-pin run exited 0" >&2; fail=1
fi
[[ $OUTPUT == *"not a sha256"* ]] \
  || { echo "  FAIL  bad-pin run lost its diagnostic:" >&2; echo "$OUTPUT" >&2; fail=1; }
[[ -r $REPORT ]] \
  || { echo "  FAIL  no $REPORT for a pin failure before provisioning — the trap must be installed before anything else" >&2; fail=1; }
grep -qx "status: fail" "$REPORT" \
  || { echo "  FAIL  bad-pin report status wrong:" >&2; cat "$REPORT" >&2; fail=1; }
grep -qx "node: " "$REPORT" \
  || { echo "  FAIL  bad-pin report should have no node (nothing was ever provisioned):" >&2; cat "$REPORT" >&2; fail=1; }
grep -qx "apiserver_version: " "$REPORT" \
  || { echo "  FAIL  bad-pin report should have no apiserver version:" >&2; cat "$REPORT" >&2; fail=1; }
grep -qx "kubelet_version: " "$REPORT" \
  || { echo "  FAIL  bad-pin report should have no kubelet version:" >&2; cat "$REPORT" >&2; fail=1; }
grep -q "reason:.*before recording a verdict" "$REPORT" \
  || { echo "  FAIL  bad-pin report lost its generic reason:" >&2; cat "$REPORT" >&2; fail=1; }


# A passing run whose report can't be written must fail loudly, not print
# "run-skew: ok" and exit 0. The
# trigger is deterministic rather than a chmod — a read-only bit can be
# bypassed by root or a runner's own umask, but a regular file can never be
# opened for writing where a directory already sits, in any context. REPORT
# is made a directory here, before run-skew.sh runs at all, which is
# equivalent to doing it "just before" the pass path's write: nothing else in
# a healthy run touches $REPORT until that one line.
rm -rf "$REPORT"
mkdir -p "$REPORT"
state=$(mktemp -d)
out_dir=$(mktemp -d)
set +e
OUTPUT=$(cd "$WORK" && \
  PATH="$WORK/bin:$PATH" \
  CLUSTER_NAME=verdict-test PROVISION_BACKEND=fake ARCH=amd64 \
  OUT="$out_dir" FAKE_STATE="$state" \
  FAKE_PHASE=Succeeded FAKE_PLACED="$NODE" FAKE_LOGS=skewed-kubelet-ran-this \
  tools/ostests/run-skew.sh dist/stratum 2>&1)
RC=$?
set -e
rm -rf "$state" "$out_dir" "$REPORT"

if [[ $RC -ne 0 ]]; then
  echo "  ok    a pass whose report can't be written still fails the script"
else
  echo "  FAIL  unwritable-report run exited 0" >&2; fail=1
fi
[[ $OUTPUT == *"could not write"* ]] \
  || { echo "  FAIL  unwritable-report run lost its warning:" >&2; echo "$OUTPUT" >&2; fail=1; }
[[ $OUTPUT != *"run-skew: ok"* ]] \
  || { echo "  FAIL  unwritable-report run still printed run-skew: ok" >&2; fail=1; }

[[ $fail -eq 0 ]] || { echo "test-ostests-skew-verdict: FAILED" >&2; exit 1; }
echo "test-ostests-skew-verdict: ok"
