#!/usr/bin/env bash
# Driver for the nightly/weekly ostests suite: stand up a real multi-node
# cluster, run conformance against it, hand back the evidence.
#
# Usage: tools/ostests/run-suite.sh [artifact]
#   $CLUSTER_NAME   unique per run; names every resource, labels the instances
#   $ARCH $OS_IMAGE passed through to provision.sh
#   $TAG            release tag being certified (run-conformance.sh names files by it)
#
# This does NOT destroy the cluster. pulumi destroy belongs in the caller's
# always() step, so a failing suite leaves the cluster up long enough to look at
# — the same reason provision.sh does not destroy either.
set -euo pipefail
cd "$(dirname "$0")/../.."

ARTIFACT=${1:-dist/stratum}
: "${CLUSTER_NAME:?CLUSTER_NAME is required — it names every resource and labels the instances}"

[[ -r $ARTIFACT ]] || { echo "run-suite: no artifact at '$ARTIFACT' — run 'make package' first" >&2; exit 1; }

# The apiserver forward is a process on THIS machine, not infrastructure, so it
# is ours to clean up even though the cluster is not. Leaking it would hold the
# port against the next run on a self-hosted runner.
#
# Installed BEFORE provisioning, deriving the path from the same convention
# provision.sh writes to: provision.sh can die AFTER creating the forward
# (the kubeconfig rewrite, the runner-side verify), and under set -e a
# trap installed after the call would never exist for exactly those failures —
# the ones that leak.
tunnel_pid_file="${OUT:-_output/ostests/$CLUSTER_NAME}/tunnel.pid"
cleanup() {
  [[ -r $tunnel_pid_file ]] || return 0
  kill "$(cat "$tunnel_pid_file")" 2>/dev/null || true
  rm -f "$tunnel_pid_file"
}
trap cleanup EXIT

echo "==> provisioning ($CLUSTER_NAME)"
# provision.sh prints the kubeconfig path on stdout and everything else on stderr,
# which is what makes this capture safe.
KUBECONFIG=$(tools/ostests/provision.sh "$ARTIFACT")
export KUBECONFIG
echo "==> kubeconfig: $KUBECONFIG"

echo "==> SELinux"
tools/ostests/check-selinux.sh

echo "==> conformance"
# OUT is pinned: $OUT in this environment carries provision.sh's meaning (the
# per-cluster scratch dir), and run-conformance.sh would inherit it as its
# RESULTS dir — silently moving the evidence away from the path the workflows
# upload (_output/conformance/), whose if-no-files-found:ignore would say so
# only as a warning. Run run-conformance.sh directly to steer its output.
OUT=_output/conformance tools/ostests/run-conformance.sh

echo "==> SELinux lifecycle"
tools/ostests/check-selinux-lifecycle.sh "$ARTIFACT"
