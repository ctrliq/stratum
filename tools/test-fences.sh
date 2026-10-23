#!/usr/bin/env bash
# Proves the committed fences actually reject out-of-fence bumps.
#
# gen-fences.sh checks the fences match upstream data; this checks they mean
# what we think. A fence is a regex or a range buried in JSON — the failure mode
# is one that still parses, still validates, and quietly allows Kubernetes 1.37
# onto release-1.36.
set -euo pipefail
cd "$(dirname "$0")/.."

python3 - renovate.json5 <<'PY'
import json, re, sys
from fnmatch import fnmatch

def load_json5(path):
    """Enough JSON5 for this config: comments, line-continued strings,
    unquoted keys, trailing commas. Avoids depending on a node package just to
    read our own config in a test."""
    src = open(path).read()
    src = re.sub(r"\\\n\s*", "", src)                        # string continuations
    src = re.sub(r"^\s*//.*$", "", src, flags=re.M)            # comment lines
    src = re.sub(r'([{,]\s*)(\$?[A-Za-z_][\w$]*)(\s*:)', r'\1"\2"\3', src)
    src = re.sub(r"^(\s*)(\$?[A-Za-z_][\w$]*)(\s*:)", r'\1"\2"\3', src, flags=re.M)
    src = re.sub(r",(\s*[}\]])", r"\1", src)                   # trailing commas
    return json.loads(src)

config = load_json5(sys.argv[1])

rules = config["packageRules"]

def matches(rule, branch, dep):
    branches = rule.get("matchBaseBranches")
    if branches and branch not in branches:
        return False
    names = rule.get("matchPackageNames", []) + rule.get("matchDepNames", [])
    if not names:
        return False
    return any(fnmatch(dep, p.replace("{/,}", "/").replace("**", "*")) for p in names)

def allowed(branch, dep, version):
    """Renovate applies the LAST matching rule's allowedVersions."""
    fence = None
    for r in rules:
        if matches(r, branch, dep) and "allowedVersions" in r:
            fence = r["allowedVersions"]
    if fence is None:
        return True, "no fence"
    if fence.startswith("/") and fence.endswith("/"):
        return bool(re.search(fence[1:-1], version)), fence
    m = re.fullmatch(r"<=\s*(\d+(?:\.\d+)*)", fence)
    if m:
        def ver(s):
            return tuple(int(x) for x in re.findall(r"\d+", s))
        ceiling = ver(m.group(1))
        return ver(version)[:len(ceiling)] <= ceiling, fence
    raise SystemExit(f"test-fences: cannot interpret allowedVersions {fence!r}")

# branch, dependency, candidate version, expected verdict
CASES = [
    ("release-1.36", "k8s.io/kubernetes",        "v1.36.5",  True),
    ("release-1.36", "k8s.io/kubernetes",        "v1.37.0",  False),
    ("release-1.36", "kubernetes/kubernetes",    "1.37.0",   False),
    ("release-1.35", "k8s.io/kubernetes",        "v1.35.9",  True),
    ("release-1.35", "k8s.io/kubernetes",        "v1.36.0",  False),
    ("release-1.36", "go",                       "1.26.6",   True),
    ("release-1.36", "go",                       "1.27.0",   False),
    ("release-1.36", "containerd/containerd",    "2.3.5",    True),
    # containerd's 1.37 row is published upstream now (ceiling <=2.4); Cilium
    # still has no 1.37 row and inherits 1.36's ceiling.
    ("release-1.37", "containerd/containerd",    "2.4.5",    True),
    ("release-1.37", "containerd/containerd",    "2.5.0",    False),
    ("release-1.37", "cilium/cilium",            "1.21.0",   False),
    ("release-1.37", "k8s.io/kubernetes",        "v1.38.0",  False),
    ("release-1.35", "containerd/containerd",    "2.2.7",    True),
    ("release-1.35", "containerd/containerd",    "2.3.4",    False),
    ("release-1.36", "coredns/coredns",          "1.14.7",   True),
    ("release-1.35", "coredns/coredns",          "1.14.7",   False),
    ("release-1.36", "etcd-io/etcd",             "3.6.9",    True),
    ("release-1.36", "etcd-io/etcd",             "3.7.1",    False),
    ("release-1.36", "cilium/cilium",            "1.20.2",   True),
    ("release-1.36", "cilium/cilium",            "1.21.0",   False),
    # kube-vip publishes no matrix, so it is deliberately unfenced.
    ("release-1.36", "kube-vip/kube-vip",        "9.9.9",    True),
]

fail = 0
for branch, dep, version, want in CASES:
    got, fence = allowed(branch, dep, version)
    verdict = "allow" if got else "reject"
    if got != want:
        print(f"test-fences: FAIL {branch} {dep} {version}: got {verdict}, "
              f"want {'allow' if want else 'reject'} (fence: {fence})")
        fail = 1
    else:
        print(f"test-fences: ok   {branch:13} {dep:24} {version:9} {verdict:6} [{fence}]")

if fail:
    print("test-fences: FAILED — the fences do not mean what they claim", file=sys.stderr)
    sys.exit(1)
print(f"test-fences: ok — {len(CASES)} fence cases behave as documented")
PY
