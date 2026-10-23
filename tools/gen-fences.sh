#!/usr/bin/env bash
# Generates Renovate's per-branch version fences from upstream compatibility
# data, instead of from memory.
#
# The fences ARE the component support matrix. Every studied distribution
# maintains them by hand, even though the compatibility they encode is published
# upstream in machine-readable form. tools/upstream_sources.py names each source;
# this script turns them into config.
#
# Usage:
#   tools/gen-fences.sh              check committed fences against upstream (CI)
#   tools/gen-fences.sh --write      regenerate them in renovate.json5
#   tools/gen-fences.sh --print      show what would be generated
#   tools/gen-fences.sh --pins 1.35  the pin set upstream data implies for one
#                                   minor, for use when cutting its branch
set -euo pipefail
cd "$(dirname "$0")/.."

CONFIG=renovate.json5
MODE=${1:---check}

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

case $MODE in
  --check|--write|--print) ;;
  --pins) [[ $# -eq 2 ]] || { echo "usage: $0 --pins <minor>" >&2; exit 2; } ;;
  *) echo "usage: $0 [--check|--write|--print|--pins <minor>]" >&2; exit 2 ;;
esac

if [[ $MODE == --pins ]]; then
  MINOR=$2 exec python3 - <<'PY'
import os, sys
sys.path.insert(0, "tools")
from upstream_sources import *

def run():
  minor = os.environ["MINOR"]
  tag = latest_patch(minor)
  ka = kubeadm_constants(tag)
  ctr_line = containerd_ceiling(minor)
  cil_line = cilium_ceiling(minor)

  print(f"# Pin set implied by upstream data for Kubernetes {minor} (latest patch v{tag}).")
  print(f"# Fenced components take the derived line; unfenced ones take latest.")
  for name, value, source in [
      ("kubernetes", tag, "dl.k8s.io stable-%s" % minor),
      ("go", go_pin(tag, minor_only=False), f"k/k v{tag} .go-version"),
      # etcd takes kubeadm's EXACT pin, not the newest patch in its line: it is
      # the datastore, the component whose failures are least recoverable, so SKE
      # runs precisely what upstream tests.
      ("etcd", ka["etcd"], "kubeadm DefaultEtcdVersion (exact)"),
      # CoreDNS takes the newest patch in kubeadm's line: a stateless workload
      # where a patch bump is low-risk, and the fence is a minor ceiling anyway.
      ("coredns", latest_in_line("coredns/coredns", line_of(ka["coredns"])),
       f"newest in kubeadm's CoreDNSVersion line ({ka['coredns']})"),
      ("containerd", latest_in_line("containerd/containerd", ctr_line), "containerd k8s support matrix"),
      ("cilium", latest_in_line("cilium/cilium", cil_line), "Cilium k8s compatibility table"),
      ("pause", ka["pause"], "kubeadm PauseVersion"),
      ("runc", latest_release("opencontainers/runc"), "unfenced - latest"),
      ("kube-vip", latest_release("kube-vip/kube-vip"), "unfenced - latest"),
  ]:
      print(f"{name+'_version':22} = {value:<12}  # {source}")

main(run)
PY
fi

generated=$(python3 - <<'PY'
import json, sys
sys.path.insert(0, "tools")
from upstream_sources import *

def run():
    rules = []

    def rule(desc, source, minor, **kw):
        rules.append({"description": f"{desc} — source: {source}",
                      "enabled": True,
                      "matchBaseBranches": [f"release-{minor}"],
                      **kw})

    for minor in supported_minors():
        tag = latest_patch(minor)
        ka = kubeadm_constants(tag)
        n = minor.split(".")[1]

        rule(f"Kubernetes on release-{minor}", "the branch's own minor", minor,
             matchPackageNames=["k8s.io/**", "**/kubernetes", "registry.k8s.io/pause"],
             allowedVersions=rf"/^v?[01]\.{n}\./")
        rule(f"Go toolchain on release-{minor}", f"kubernetes/kubernetes v{tag} .go-version",
             minor,
             matchDepNames=["go", "docker.io/library/golang"],
             allowedVersions=f"<={go_pin(tag)}")
        rule(f"etcd on release-{minor}", f"kubeadm DefaultEtcdVersion at v{tag}", minor,
             matchPackageNames=["etcd-io/etcd", "go.etcd.io/etcd/**"],
             allowedVersions=f"<={ka['etcd'].rsplit('.', 1)[0]}")
        rule(f"CoreDNS on release-{minor}", f"kubeadm CoreDNSVersion at v{tag}", minor,
             matchPackageNames=["coredns/coredns", "docker.io/coredns/coredns"],
             allowedVersions=f"<={ka['coredns'].rsplit('.', 1)[0]}")
        # Ceilings first, sources second: an undocumented minor inherits N-1's
        # ceiling (upstream_sources._inherit), and the description must say so.
        ctr, cil = containerd_ceiling(minor), cilium_ceiling(minor)
        def inherited(component, source):
            src = INHERITED.get((component, minor))
            return f"{source} ({src} row inherited: no {minor} row published yet)" if src else source
        rule(f"containerd and runc on release-{minor}",
             inherited("containerd", "containerd RELEASES.md Kubernetes Support matrix"), minor,
             matchPackageNames=["containerd/containerd", "opencontainers/runc",
                                "github.com/containerd/containerd{/,}**"],
             allowedVersions=f"<={ctr}")
        rule(f"Cilium on release-{minor}",
             inherited("Cilium", "Cilium Documentation/network/kubernetes/compatibility.rst"), minor,
             matchPackageNames=["cilium/cilium", "quay.io/cilium/**"],
             allowedVersions=f"<={cil}")

    # kube-vip publishes no Kubernetes support matrix, so there is nothing to
    # derive. Saying so in the config beats inventing a ceiling.
    rules.append({
        "description": "kube-vip has no published Kubernetes support matrix, so it carries no "
                       "derived fence and tracks latest on every branch. Revisit if kube-vip "
                       "starts publishing one. — source: none available",
        "matchPackageNames": ["kube-vip/kube-vip", "ghcr.io/kube-vip/**"],
    })

    body = json.dumps(rules, indent=4, ensure_ascii=False)
    print("\n".join("    " + l for l in body[1:-1].strip("\n").split("\n")))

main(run)
PY
)

patterns=$(python3 - <<'PY'
import json, sys
sys.path.insert(0, "tools")
from upstream_sources import supported_minors
# Literal branch names, not a digit-class regex: a pattern like 1\.3[4-6]
# silently stops matching the moment the supported minors cross a decade.
print("  baseBranchPatterns: " +
      json.dumps(["main"] + [f"release-{m}" for m in supported_minors()]) + ",")
PY
)

replace_region() {  # file, begin marker, end marker, replacement text
  printf '%s\n' "$4" > "$tmpdir/region"
  python3 -c '
import sys
path, begin, end, region = sys.argv[1:5]
repl = open(region).read().rstrip("\n")
src = open(path).read()
b = src.index(begin); e = src.index(end)
b = src.index("\n", b) + 1
open(path, "w").write(src[:b] + repl + "\n" + src[src.rindex("\n", 0, e) + 1:])
' "$1" "$2" "$3" "$tmpdir/region"
}

write_into() {
  replace_region "$1" "BEGIN GENERATED FENCES" "END GENERATED FENCES" "$generated"
  replace_region "$1" "BEGIN GENERATED baseBranchPatterns" "END GENERATED baseBranchPatterns" "$patterns"
}

branches=$(grep -v '^#' build/branches.txt | grep . | tr '\n' ' ')

case $MODE in
  --print)
    printf '%s\n%s\n' "$patterns" "$generated" ;;
  --write)
    write_into "$CONFIG"
    echo "gen-fences: wrote fences for ${branches}into $CONFIG" ;;
  --check)
    # Generate into a copy, so a failing check never edits the tree.
    cp "$CONFIG" "$tmpdir/candidate"
    write_into "$tmpdir/candidate"
    if ! diff -u "$CONFIG" "$tmpdir/candidate" > "$tmpdir/diff"; then
      echo "gen-fences: FAILED — committed fences differ from upstream compatibility data" >&2
      sed 's/^/  /' "$tmpdir/diff" >&2
      echo "gen-fences: run 'tools/gen-fences.sh --write' and commit the result" >&2
      exit 1
    fi
    echo "gen-fences: ok — fences for ${branches}match upstream compatibility data" ;;
esac
