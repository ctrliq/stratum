#!/usr/bin/env bash
# Branch-aware gate for the Kubernetes pins. The policy is stated where the
# repo states build policy, build/versions.mk's header ("Branch-aware
# Kubernetes pin policy"); the rule in short:
#   - the kube-* images in build/images.txt equal v$(kubernetes_version), on
#     every branch — the image-tag check;
#   - a release branch (release/* or release-<minor>) pins its libraries
#     exactly: every go.mod k8s.io staging library carries the version
#     upstream tags for kubernetes_version (v0.<minor>.<patch>), the same
#     version as the binaries;
#   - on main and every other branch the go.mod k8s.io libraries may drift
#     one minor ahead into a prerelease, and the gate reports the drift as
#     a WARN, never a failure — that drift is how main tracks the next
#     Kubernetes minor before its binaries move;
#   - mise.toml's kubectl stays within upstream-supported skew of
#     kubernetes_version — one minor, older or newer — on every branch.
# The branch decides which library rule applies, and both halves of the
# policy are enforced in CI: pull requests run against every base branch
# and pushes run on main and the release branches. A pull request's
# checkout is detached, so there the run's environment names its target
# branch; a push's checkout carries the branch. A checkout with neither
# answer — a tarball, a detached local clone — gets main's rule.
#
# images.txt states "the kube-* tags must equal v$(kubernetes_version)"; this
# is what enforces it. Left unenforced, a bump that moves the binaries but
# not the images surfaces in CI as inttest failures that never name the skew.
set -euo pipefail

cd "$(dirname "$0")/.."

want="v$(sed -n 's/^kubernetes_version *= *//p' build/versions.mk | tr -d ' ')"
[[ $want != v ]] || { echo "lint-kube-images: no kubernetes_version in build/versions.mk" >&2; exit 1; }
kube=${want#v}   # the bare version, 1.<minor>.<patch>

# The branch is the one input the pin files cannot carry. CI's checkouts
# name it in the environment — GITHUB_BASE_REF on a pull request (its
# target branch; the checkout itself is detached, so git answers nothing
# there), GITHUB_REF_NAME on a push (the branch that run's checkout
# carries) — and every other run asks git. An empty answer (detached HEAD,
# no checkout) is not a release branch: it gets main's rule.
branch=${GITHUB_BASE_REF:-${GITHUB_REF_NAME:-}}
[[ -n $branch ]] || branch=$(git branch --show-current 2>/dev/null || true)
case $branch in
  release/*|release-[0-9]*) release=yes ;;
  *)                        release=no ;;
esac

kube_major=${kube%%.*}
kube_minor=${kube#*.}
kube_minor=${kube_minor%%.*}

fail=0

# The images: every branch, unchanged.
for image in kube-apiserver kube-controller-manager kube-scheduler; do
  got=$(sed -n "s|^registry\.k8s\.io/$image:\([^@]*\)@.*|\1|p" build/images.txt)
  if [[ $got != "$want" ]]; then
    echo "lint-kube-images: ERROR build/images.txt pins registry.k8s.io/$image:${got:-<none>}," \
      "but kubernetes_version in build/versions.mk is $want — move the image with the binary" >&2
    fail=1
  fi
done

# The libraries: upstream tags its staging repositories v0.<minor>.<patch>
# from the release's 1.<minor>.<patch>, so exact equality has one answer.
lib_want="v0${kube#1}"   # 1.<minor>.<patch> -> v0.<minor>.<patch>
# A go.mod may spell a requirement inside require (...) or on one
# `require module version` line. Compare every staging pin in either form.
lib_got=$(awk '{
  if ($1 == "require") { module=$2; version=$3 }
  else                 { module=$1; version=$2 }
  if (module ~ /^k8s\.io\// && version ~ /^v0\.[1-9][0-9]*\./) print version
}' go.mod 2>/dev/null | sort -u || true)
if [[ -z $lib_got ]]; then
  echo "lint-kube-images: ERROR go.mod carries no k8s.io staging library version to compare with kubernetes_version $kube" >&2
  fail=1
elif [[ $lib_got != "$lib_want" ]]; then
  if [[ $release == yes ]]; then
    echo "lint-kube-images: ERROR on $branch the go.mod k8s.io libraries are $lib_got, not kubernetes_version $kube's exact $lib_want —" \
      "a release ships one version of libraries and binaries" >&2
    fail=1
  elif [[ $lib_got =~ ^v0\.([0-9]+)\.[0-9]+-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]] \
    && [[ ${BASH_REMATCH[1]} == "$((kube_minor + 1))" ]]; then
    echo "lint-kube-images: WARN the go.mod k8s.io libraries are $lib_got against the $kube binaries — allowed off a release branch" \
      "to track the next prerelease; a release branch pins them exactly at $lib_want"
  else
    echo "lint-kube-images: ERROR on $branch the go.mod k8s.io libraries are $lib_got against the $kube binaries —" \
      "use the exact $lib_want or one shared prerelease version of the next minor" >&2
    fail=1
  fi
fi

# The dev kubectl: the one host tool pinned against the cluster version.
# Upstream supports one minor of skew either way; beyond it the combination
# is untested, on every branch.
kubectl_pin=$(sed -n 's/^[[:space:]]*kubectl[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' mise.toml 2>/dev/null || true)
if [[ -z $kubectl_pin ]]; then
  echo "lint-kube-images: ERROR mise.toml pins no kubectl to check against kubernetes_version $kube" >&2
  fail=1
elif [[ $kubectl_pin =~ ^v?([0-9]+)\.([0-9]+) ]]; then
  if [[ ${BASH_REMATCH[1]} != "$kube_major" ]] \
    || (( BASH_REMATCH[2] > kube_minor + 1 || BASH_REMATCH[2] < kube_minor - 1 )); then
    echo "lint-kube-images: ERROR mise.toml pins kubectl $kubectl_pin against kubernetes_version $kube —" \
      "beyond upstream-supported skew of one minor" >&2
    fail=1
  fi
else
  echo "lint-kube-images: ERROR mise.toml pins kubectl \"$kubectl_pin\", which carries no major.minor to check against $kube" >&2
  fail=1
fi

[[ $fail -eq 0 ]] || { echo "lint-kube-images: FAILED" >&2; exit 1; }
echo "lint-kube-images: ok — kube-{apiserver,controller-manager,scheduler} are $want"
