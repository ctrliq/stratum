#!/usr/bin/env bash
# Runs the CNCF conformance suite against a live SKE cluster and produces the
# evidence a release page has to carry.
#
# Hydrophone (kubernetes-sigs, SIG-Testing), which CNCF names as a standard
# conformance tool in cncf/k8s-conformance. It runs the upstream conformance
# image as one pod and writes the two files a submission PR carries, e2e.log
# and junit_01.xml, so there is no results tarball to unpick afterwards.
#
# Callers: tools/ostests/run-suite.sh (the weekly e2e, per branch x provider x arch)
# and release.yml's conformance-test job. Both provision a cluster first — this
# script does not, and hard-exits when KUBECONFIG is unset.
#
# --mode=certified-conformance is the full [Conformance] set. It takes hours.
# That is why it gates releases and not PRs, and why every final is preceded by
# an RC that has already been through it (RELEASE.md).
#
# Usage: tools/ostests/run-conformance.sh
#   $KUBECONFIG   admin credentials for a multi-node cluster (see provision.sh)
#   $ARCH         amd64 | arm64 — names the results file, nothing else
#   $PROVIDER     gcp | local — names the results file, nothing else;
#                 defaults to $PROVISION_BACKEND, then gcp
#   $TAG          the release tag being certified — names the results file
#   $OUT          output directory (default _output/conformance)
set -euo pipefail
cd "$(dirname "$0")/../.."

ARCH=${ARCH:-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')}
PROVIDER=${PROVIDER:-${PROVISION_BACKEND:-gcp}}
# It lands in a file name under $OUT, so a value with a slash in it would write
# the evidence somewhere else. Not a fixed set: `local` is a legitimate dev value.
[[ $PROVIDER =~ ^[a-z0-9]+$ ]] || { echo "run-conformance: PROVIDER must be a plain lowercase name, got '$PROVIDER'" >&2; exit 1; }
TAG=${TAG:-$(tools/version.sh --stamp)}
OUT=${OUT:-_output/conformance}
MODE=${MODE:-certified-conformance}
# Only certified-conformance exists today; refuse rather than run something else
# under a mode name that suggests otherwise. Up here, with the other input
# checks, so a bad value is refused before anything touches the cluster.
[[ $MODE == certified-conformance ]] || { echo "run-conformance: unsupported MODE '$MODE'" >&2; exit 1; }
TIMEOUT=${TIMEOUT:-14400}      # 4h: the certified suite genuinely takes hours

[[ -n ${KUBECONFIG:-} ]] || {
  echo "run-conformance: KUBECONFIG is unset — this needs a real multi-node cluster." >&2
  echo "                 tools/ostests/provision.sh is what produces one." >&2
  exit 1; }
[[ -r $KUBECONFIG ]] || { echo "run-conformance: cannot read \$KUBECONFIG ($KUBECONFIG)" >&2; exit 1; }
command -v hydrophone >/dev/null || {
  echo "run-conformance: hydrophone not installed (https://github.com/kubernetes-sigs/hydrophone/releases)" >&2
  exit 1; }
command -v timeout >/dev/null || { echo "run-conformance: timeout (coreutils) not installed" >&2; exit 1; }

mkdir -p "$OUT"
# The provider is in the name because one release now carries evidence from
# several clouds, and conformance-upload merges every leg's artifact into one
# directory before attaching — two legs producing the same file name would
# clobber each other there. release-publish.yml's guard looks for exactly this
# name per (provider, arch), so the two must move together. (Named
# sonobuoy-conformance-results-* before the move to Hydrophone; the guard still accepts that
# for tags cut from branches that have not taken the change.)
RESULTS="$OUT/conformance-results-$PROVIDER-$ARCH-$TAG.tar.gz"
# Hydrophone writes e2e.log and junit_01.xml to the same names every run, so a
# directory per leg keeps them from meeting a previous run's files.
RAW="$OUT/hydrophone-$PROVIDER-$ARCH"

# Leftovers from an interrupted run make the next one report someone else's
# results, which is the one failure mode that is worse than no results.
rm -rf "$RAW"
hydrophone --cleanup >/dev/null 2>&1 || true

# The e2e image comes from upstream and is pulled by the cluster under test. It
# is deliberately NOT in the SKE mirror: the conformance image must be the one
# upstream published, or the run certifies nothing. Pinned to the apiserver's
# version rather than left to hydrophone's own default so the log says which
# one was meant; SKE's gitVersion carries a -stratum suffix, which the tag
# does not.
server=$(kubectl version -o json \
         | python3 -c 'import json,sys,re; print(re.match(r"v\d+\.\d+\.\d+", json.load(sys.stdin)["serverVersion"]["gitVersion"]).group(0))') \
  || { echo "run-conformance: could not read the apiserver version" >&2; exit 1; }
image=registry.k8s.io/conformance:$server

echo "==> hydrophone --conformance ($MODE, $ARCH, $TAG, $image)"
# hydrophone has no overall deadline of its own, so timeout is the bound. Its
# exit status is the conformance container's — non-zero when any test fails
# (hydrophone cmd/root.go: os.Exit(exitCode)) — and, without `|| rc=$?`, set -e
# would end this script before the evidence is packaged.
rc=0
timeout "$TIMEOUT" hydrophone --conformance --conformance-image "$image" \
  --output-dir "$RAW" || rc=$?

hydrophone --cleanup >/dev/null 2>&1 || true

# The verdict is the exit status AND the junit: a run that exits 0 without
# producing junit_01.xml, whose junit disagrees, whose junit executed nothing
# (an empty or all-skipped report), or that left no e2e.log beside it is not a
# pass: a cncf/k8s-conformance submission needs both files, and the publish
# guard only checks that the tarball exists. Print the
# summary either way — a failing run's list of failed tests is the whole point
# of running it on an RC.
echo "==> results"
if [[ -r $RAW/junit_01.xml ]]; then
  { python3 - "$RAW/junit_01.xml" <<'PY' || true; } | tee "$OUT/summary.txt"
import sys, xml.etree.ElementTree as ET
suites = ET.parse(sys.argv[1]).getroot().iter("testsuite")
cases = [c for s in suites for c in s.iter("testcase")]
bad = [c for c in cases if c.find("failure") is not None or c.find("error") is not None]
skipped = [c for c in cases if c.find("skipped") is not None]
print(f"executed {len(cases) - len(skipped)}, failed {len(bad)}, skipped {len(skipped)}")
for c in bad:
    print("FAILED:", c.get("name"))
PY
  failed=$(sed -n '1s/.*failed \([0-9]*\),.*/\1/p' "$OUT/summary.txt")
  executed=$(sed -n '1s/^executed \([0-9]*\),.*/\1/p' "$OUT/summary.txt")
else
  echo "no junit_01.xml was produced (hydrophone exit $rc)" | tee "$OUT/summary.txt"
  failed=unknown
fi
failed=${failed:-unknown}
executed=${executed:-0}
if [[ -s $RAW/e2e.log ]]; then have_log=1; else have_log=0; echo "no e2e.log was produced" | tee -a "$OUT/summary.txt"; fi
# hydrophone makes $RAW itself, but not when the deadline kills it or it dies
# before reaching the cluster; the evidence is still packaged then, and set -e
# must not end the script on the cp below before it says FAILED.
mkdir -p "$RAW"
cp "$OUT/summary.txt" "$RAW/summary.txt"
tar -czf "$RESULTS" -C "$RAW" e2e.log junit_01.xml summary.txt 2>/dev/null \
  || tar -czf "$RESULTS" -C "$RAW" .

echo
echo "results: $RESULTS"
if [[ $rc == 0 && $failed == 0 && $executed -gt 0 && $have_log == 1 ]]; then
  echo "run-conformance: ok — 0 failed tests ($MODE, $PROVIDER, $ARCH)"
  exit 0
fi
echo "run-conformance: FAILED — hydrophone exit $rc, ${failed} failed, ${executed} executed, e2e.log $([[ $have_log == 1 ]] && echo present || echo missing); see $OUT/summary.txt" >&2
exit 1
