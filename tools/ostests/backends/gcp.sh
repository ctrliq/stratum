#!/usr/bin/env bash
# GCP backend for tools/ostests/provision.sh — see backends/README.md.
#
# Wraps the Pulumi program in tools/ostests/gcp/Pulumi.yaml. Everything
# provider-specific lives here so provision.sh stays a plain install driver;
# the SSH half of the contract is ssh-lib.sh, shared with the other clouds.
#
# GCP rather than a cheaper VPS provider for one reason: release-publish.yml
# refuses to publish a final without a conformance-results tarball for each of amd64 and
# arm64, and that evidence must be native — emulated runs do not count (policy;
# the gate itself only checks asset names). GCP covers both (E2/N2 and
# T2A/C4A); an x86-only provider would need a second backend for the arm64 leg.
#
# Reads: $CLUSTER_NAME              stack name; names every resource, labels the instances
#        $ARCH                      amd64 | arm64 -> machine type + image
#        $GCP_PROJECT $GCP_ZONE     project required; zone defaults us-central1-a
#        $SSH_KEY                   default ~/.ssh/id_ed25519
#        $SSH_USER                  default stratum
#        $PULUMI_BACKEND_URL        self-managed state — must be gs://, see README

BACKEND_NAME=gcp
PROG_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../gcp" && pwd)"
# shellcheck source=tools/ostests/backends/ssh-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/ssh-lib.sh"

# amd64 and arm64 need different machine types AND different images, and Pulumi
# YAML has no conditionals, so the pair is resolved here and passed as config.
#
# T2A rather than the newer C4A deliberately: T2A is what an earlier
# live validation used (3-CP + worker, Rocky 9 aarch64, every node reporting
# architecture=arm64). C4A is faster and more widely available and is a one-line
# swap — after this is green, not before.
_machine_type() {
  [[ ${ARCH:-amd64} == arm64 ]] && echo "${GCP_MACHINE_TYPE:-t2a-standard-4}" \
                                || echo "${GCP_MACHINE_TYPE:-e2-standard-4}"
}

# $OS_IMAGE wins when set. It is part of provision.sh's contract and what the
# weekly OS matrix varies, so ignoring it leaves the matrix inert: adding an
# OS row changes nothing, silently.
#
# The caller must supply an image matching $ARCH. The matrix indexes its per-OS
# entry by architecture for exactly that reason; an amd64 image on a T2A instance
# fails to boot, and it fails late.
_image() {
  [[ -n ${OS_IMAGE:-} ]] && { echo "$OS_IMAGE"; return; }
  [[ ${ARCH:-amd64} == arm64 ]] \
    && echo "rocky-linux-cloud/rocky-linux-9-optimized-gcp-arm64" \
    || echo "rocky-linux-cloud/rocky-linux-9"
}

backend_up() {
  _require_up_inputs || return 1
  : "${GCP_PROJECT:?gcp backend: GCP_PROJECT is required}"
  # gs:// is the only scheme that works in CI — WIF mints GCP credentials and
  # nothing else.
  _require_state_backend gs:// "the job's only credentials are GCP WIF" || return 1

  # Preflight the zone/machine-type pair when gcloud is available. T2A exists in
  # only a handful of zones, and a zone without it fails at `pulumi up` on an
  # invalid machine type — AFTER the amd64 leg has already passed in the same
  # zone, which reads as a flaky arm64 leg rather than a config error. Failing
  # here names the actual problem. Advisory only when gcloud is absent: pulumi
  # will surface the same failure, just less legibly.
  if command -v gcloud >/dev/null; then
    gcloud compute machine-types describe "$(_machine_type)" \
        --project "$GCP_PROJECT" --zone "${GCP_ZONE:-us-central1-a}" >/dev/null 2>&1 \
      || { echo "gcp backend: machine type $(_machine_type) is not available in ${GCP_ZONE:-us-central1-a}" >&2
           echo "             (T2A zones are limited — set GCP_ZONE to one that offers it)" >&2
           return 1; }
  fi

  echo "gcp: pulumi up ($CLUSTER_NAME, $ARCH, $(_machine_type))" >&2
  _pulumi_up \
    "gcp:project=$GCP_PROJECT" \
    "gcp:zone=${GCP_ZONE:-us-central1-a}" \
    "clusterName=$CLUSTER_NAME" \
    "machineType=$(_machine_type)" \
    "image=$(_image)" \
    "sshUser=$SSH_USER" \
    "sshPublicKey=$SSH_PUBLIC_KEY"
}
