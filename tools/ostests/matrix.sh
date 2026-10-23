#!/usr/bin/env bash
# Emits the GitHub Actions matrices for the ostests jobs: which provider legs
# exist, on which branches, with which images and runners.
#
# One place, because the same truth feeds three jobs and a hardcoded list in
# workflow YAML is a second place to forget (tools/inttest-matrix.sh makes the
# same argument). GCP is the only cloud provider: OSTESTS_ENABLED gates the jobs
# themselves. Release branches get GCP weekly and at release time, which is the
# gate that matters (release.yml).
#
#   matrix.sh weekly      {"include":[{branch, provider, os, image, goarch, runner}, ...]}
#   matrix.sh release     {"include":[{provider, arch, runner}, ...]}
#   matrix.sh providers   ["gcp"]   — the nightly sweep job's dimension
#
# Reads: build/branches.txt (weekly)
set -euo pipefail
cd "$(dirname "$0")/../.."

mode=${1:?usage: $0 weekly|release|providers}

# shellcheck disable=SC2016  # the program is python, not shell
python3 -c '
import json, sys

mode = sys.argv[1]
providers = ["gcp"]

# Both architectures: release-publish.yml refuses to publish a final without a
# conformance-results tarball for each, and that evidence must be native (policy — the
# gate itself only checks asset names; emulated runs do not count). Native cuts
# both ways: the artifact under test is built on a runner of the same
# architecture as the nodes it lands on. An amd64 binary on an arm64 node dies
# as `exec format error`, after paying for the instances.
ARCHES = (("amd64", "ubuntu-latest"), ("arm64", "ubuntu-24.04-arm"))

# One OS row per provider for the weekly, carrying BOTH images: arch varies
# independently, and an amd64 image on an arm64 instance fails to boot, late.
# GCP stays on the Rocky 9 family it went live with. These duplicate the
# per-arch defaults in backends/gcp.sh on purpose: the release gate leaves
# OS_IMAGE unset and certifies the backend default, while the weekly passes its
# row explicitly so that adding a row HERE is what adds an OS leg.
OS = {
    "gcp":   ("rocky-9",  {"amd64": "rocky-linux-cloud/rocky-linux-9",
                           "arm64": "rocky-linux-cloud/rocky-linux-9-optimized-gcp-arm64"}),
}

if mode == "providers":
    print(json.dumps(providers, separators=(",", ":")))
elif mode == "release":
    legs = [{"provider": p, "arch": a, "runner": r} for p in providers for a, r in ARCHES]
    print(json.dumps({"include": legs}, separators=(",", ":")))
elif mode == "weekly":
    # Same source of truth as the fences and the reproducible job: branches.txt.
    with open("build/branches.txt") as fh:
        branches = ["main"] + ["release-" + l.strip() for l in fh
                               if l.strip() and not l.startswith("#")]
    legs = []
    for b in branches:
        for p in providers:
            key, images = OS[p]
            for a, r in ARCHES:
                legs.append({"branch": b, "provider": p, "os": key,
                             "image": images[a], "goarch": a, "runner": r})
    print(json.dumps({"include": legs}, separators=(",", ":")))
else:
    sys.exit(f"matrix: unknown mode {mode!r} (weekly|release|providers)")
' "$mode"
