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
    # Trailing commas, at a line end only: an unanchored `,}` also rewrote the
    # glob `containerd{/,}**` to `containerd{/}**`, which cannot match the bare
    # module, so no case could see a fence naming it.
    src = re.sub(r",(\s*\n\s*[}\]])", r"\1", src)
    return json.loads(src)

config = load_json5(sys.argv[1])

rules = config["packageRules"]

# The update a case describes, beyond its branch and dependency. A field a case
# leaves as None never matches a rule that asks for it: Renovate's own matchers
# return false for an undefined input, and this keeps the older cases, which
# name no manager, out of the manager-scoped rules exactly as before.
MATCHERS = {
    "matchBaseBranches": "branch",
    "matchPackageNames": "dep",
    "matchDepNames":     "dep",
    "matchManagers":     "manager",
    "matchUpdateTypes":  "update",
    "matchFileNames":    "file",
    "matchDatasources":  "datasource",
    "matchCurrentVersion": "current",
}

def matches(rule, branch, dep, **ctx):
    ctx.update(branch=branch, dep=dep)
    unknown = [k for k in rule if k.startswith("match") and k not in MATCHERS]
    if unknown:
        raise SystemExit(f"test-fences: no model for {unknown} in {rule['description']!r}")
    # Renovate ANDs match* fields: a rule carrying both lists applies only to a
    # dependency that matches each. Or-ing them here would hide a rule that
    # matches nothing at all.
    return all(ctx.get(field) is not None and match_list(ctx[field], rule[key])
               for key, field in MATCHERS.items() if key in rule)

def match_list(value, patterns):
    """Renovate's matchRegexOrGlobList: some positive pattern (if any) matches
    and no negated one does."""
    if isinstance(patterns, str):                              # matchCurrentVersion
        patterns = [patterns]
    pos = [p for p in patterns if not p.startswith("!")]
    neg = [p[1:] for p in patterns if p.startswith("!")]
    return (not pos or any(glob(value, p) for p in pos)) and \
        not any(glob(value, p) for p in neg)

def glob(dep, pattern):
    """Renovate's minimatch, as far as this config uses it: ** and {a,b};
    a /regex/ is matched as one."""
    if len(pattern) > 1 and pattern.startswith("/") and pattern.endswith("/"):
        return bool(re.search(pattern[1:-1], dep))
    m = re.search(r"\{([^}]*)\}", pattern)
    if m:
        return any(glob(dep, pattern[:m.start()] + alt + pattern[m.end():])
                   for alt in m.group(1).split(","))
    return fnmatch(dep, pattern.replace("**", "*"))

def setting(key, default, branch, dep, **ctx):
    """Renovate applies the LAST matching rule's value for a key."""
    value = default
    for r in rules:
        if key in r and matches(r, branch, dep, **ctx):
            value = r[key]
    return value

def group(branch, dep):
    return setting("groupName", None, branch, dep)

def enabled(branch, dep, **ctx):
    """main's gomod default is off, so only the names the config allowlists can
    come out True there."""
    return setting("enabled", True, branch, dep, **ctx)

def allowed(branch, dep, version, **ctx):
    """Renovate applies the LAST matching rule's allowedVersions."""
    fence = None
    for r in rules:
        if matches(r, branch, dep, **ctx) and "allowedVersions" in r:
            fence = r["allowedVersions"]
    if fence is None:
        return True, "no fence"
    if fence.startswith("/") and fence.endswith("/"):
        return bool(re.search(fence[1:-1], version)), fence
    m = re.fullmatch(r"(<=?)\s*(\d+(?:\.\d+)*)", fence)
    if m:
        def ver(s):
            return tuple(int(x) for x in re.findall(r"\d+", s))
        ceiling = ver(m.group(2))
        head = ver(version)[:len(ceiling)]
        return (head <= ceiling if m.group(1) == "<=" else head < ceiling), fence
    raise SystemExit(f"test-fences: cannot interpret allowedVersions {fence!r}")

# branch, dependency, candidate version, expected verdict
CASES = [
    ("release-1.36", "k8s.io/kubernetes",        "v1.36.5",  True),
    ("release-1.36", "k8s.io/kubernetes",        "v1.37.0",  False),
    ("release-1.36", "kubernetes/kubernetes",    "1.37.0",   False),
    ("release-1.36", "registry.k8s.io/kube-apiserver", "v1.36.9", True),
    ("release-1.36", "registry.k8s.io/kube-apiserver", "v1.37.1", False),
    ("release-1.35", "k8s.io/kubernetes",        "v1.35.9",  True),
    ("release-1.35", "k8s.io/kubernetes",        "v1.36.0",  False),
    # go is exact: lint-go-version rejects anything but the tag's .go-version.
    ("release-1.36", "go",                       "1.26.8",   False),
    ("release-1.36", "go",                       "1.26.9",   False),
    ("release-1.36", "go",                       "1.26.10",  True),
    ("release-1.36", "go",                       "1.27.0",   False),
    ("release-1.36", "containerd/containerd",    "2.3.5",    True),
    # containerd's 1.37 row is published upstream now (ceiling <=2.4); Cilium
    # still has no 1.37 row and inherits 1.36's ceiling.
    ("release-1.37", "containerd/containerd",    "2.4.5",    True),
    ("release-1.37", "containerd/containerd",    "2.5.0",    False),
    ("release-1.37", "cilium/cilium",            "1.21.0",   False),
    ("release-1.37", "k8s.io/kubernetes",        "v1.38.0",  False),
    ("main",         "containerd/containerd",    "2.3.6",    True),
    ("main",         "containerd/containerd",    "2.4.0",    False),
    ("release-1.35", "containerd/containerd",    "2.2.7",    True),
    ("release-1.35", "containerd/containerd",    "2.3.4",    False),
    ("release-1.36", "coredns/coredns",          "1.14.7",   True),
    ("release-1.35", "coredns/coredns",          "1.14.7",   False),
    # etcd is exact: kubeadm's pin and nothing else, binary and image alike.
    ("release-1.36", "etcd-io/etcd",             "3.6.8",    True),
    ("release-1.36", "etcd-io/etcd",             "3.6.9",    False),
    ("release-1.36", "etcd-io/etcd",             "3.7.1",    False),
    ("release-1.37", "etcd-io/etcd",             "3.7.2",    False),
    ("release-1.37", "go.etcd.io/etcd/client/v3", "v3.7.0",  True),
    ("release-1.37", "registry.k8s.io/etcd",     "3.7.0-0",  True),
    ("release-1.37", "registry.k8s.io/etcd",     "3.7.1-0",  False),
    ("release-1.36", "cilium/cilium",            "1.20.2",   True),
    ("release-1.36", "cilium/cilium",            "1.21.0",   False),
    # kube-vip publishes no matrix, so it is deliberately unfenced.
    ("release-1.36", "kube-vip/kube-vip",        "9.9.9",    True),
    # cosign v3 breaks sign-blob's output flags; capped on every branch,
    # including the release branches, where github-actions majors are otherwise allowed.
    ("main",         "sigstore/cosign",          "v2.6.6",   True),
    ("main",         "sigstore/cosign",          "v3.0.2",   False),
    ("release-1.35", "sigstore/cosign",          "v3.0.2",   False),
]

# main holds the Kubernetes group off pre-releases until 1.38.0 GA, and lets GA
# itself through. The libraries are already on v0.38.0-alpha.0, so they are
# fenced by their current version; the binary and images by ignoreUnstable.
# (branch, dependency, current version, candidate version, expected verdict)
CURRENT_CASES = [
    ("main",         "k8s.io/client-go",  "v0.38.0-alpha.0", "v0.38.0-alpha.2", False),
    ("main",         "k8s.io/client-go",  "v0.38.0-alpha.0", "v0.38.0-beta.0",  False),
    ("main",         "k8s.io/client-go",  "v0.38.0-alpha.0", "v0.39.0-alpha.0", False),
    ("main",         "k8s.io/client-go",  "v0.38.0-alpha.0", "v0.38.0",         True),
    ("main",         "k8s.io/client-go",  "v0.38.0-alpha.0", "v1.5.2",          False),
    ("main",         "k8s.io/klog/v2",    "v2.140.0",        "v2.150.0",        True),
    ("release-1.37", "k8s.io/client-go",  "v0.37.1",         "v0.37.2",         True),
]

# (branch, dependency, expected ignoreUnstable)
UNSTABLE_CASES = [
    ("main",         "kubernetes/kubernetes",                    True),
    ("main",         "registry.k8s.io/kube-apiserver",           True),
    ("main",         "registry.k8s.io/kube-controller-manager",  True),
    ("main",         "registry.k8s.io/kube-scheduler",           True),
    ("main",         "registry.k8s.io/pause",                    False),
    ("release-1.37", "kubernetes/kubernetes",                    True),
]

# branch, dependency, expected enabled
ENABLED_CASES = [
    ("main",         "etcd-io/etcd",         False),
    ("main",         "go.etcd.io/etcd/client/v3", False),
    ("main",         "registry.k8s.io/etcd", False),
    ("main",         "go",                   False),
    ("release-1.37", "k8s.io/api",           True),
    ("release-1.37", "k8s.io/kube-openapi",  False),
    ("release-1.36", "k8s.io/utils",         False),
]

# One update, every dimension Renovate matches on: branch, manager, dependency,
# update type, file. These reach the rules the name-only cases above cannot.
# (branch, manager, dependency, update type, file, expected enabled)
UPDATE_CASES = [
    # No majors on a release branch: React 19 and the containerd v2 Go
    # module were proposed onto every release branch.
    ("release-1.36", "npm",    "react",   "major", "dash/package.json", False),
    ("release-1.37", "npm",    "vite",    "major", "dash/package.json", False),
    ("release-1.35", "gomod",  "github.com/containerd/containerd", "major", "go.mod", False),
    # The fences set enabled: true, so the block has to come after them: this is
    # a fenced name, and with the block placed before the fences it comes out True.
    ("release-1.36", "gomod",  "k8s.io/api", "major", "go.mod", False),
    ("release-1.36", "custom.regex", "cilium/cilium", "major", "build/versions.mk", False),
    # ... but only majors, and only off main.
    ("release-1.36", "gomod",  "k8s.io/api", "patch", "go.mod", True),
    ("release-1.36", "npm",    "vite",    "minor", "dash/package.json", True),
    ("main",         "npm",    "react",   "major", "dash/package.json", True),
    # github-actions is exempt: action majors still reach release branches.
    ("release-1.36", "github-actions", "actions/checkout", "major",
     ".github/workflows/ci.yml", True),
    ("release-1.35", "github-actions", "actions/setup-go", "major",
     ".github/workflows/ci.yml", True),
    # The containerd fence names the binary, not the Go module: its enabled: true
    # would re-enable gomod updates to the module on release branches.
    ("release-1.35", "gomod",  "github.com/containerd/containerd", "patch", "go.mod", False),
    ("release-1.36", "gomod",  "github.com/containerd/containerd/v2", "minor", "go.mod", False),
    ("release-1.35", "custom.regex", "containerd/containerd", "patch", "build/versions.mk", True),
    ("release-1.35", "custom.regex", "opencontainers/runc", "patch", "build/versions.mk", True),
    # npm is scoped to dash/.
    ("main",         "npm",    "left-pad", "minor", "tools/package.json", False),
]

# (branch, manager, dependency, file, expected minimumReleaseAge)
AGE_CASES = [
    # pnpm 12 refuses a release younger than a day; Renovate must not propose one.
    ("main",         "npm",          "vite", "dash/package.json", "1 day"),
    ("release-1.36", "npm",          "react", "dash/package.json", "1 day"),
    # pnpm is one PR across both managers, so both sides wait alike.
    ("main",         "npm",          "pnpm", "dash/package.json", "1 day"),
    ("main",         "custom.regex", "pnpm", "build/versions.mk", "1 day"),
    ("main",         "custom.regex", "kubernetes/kubernetes", "build/versions.mk", None),
]

# One Kubernetes release is one PR: binary pin, control-plane images, pause.
GROUP_CASES = [
    ("main", "kubernetes/kubernetes",                    "Kubernetes"),
    ("main", "registry.k8s.io/kube-apiserver",           "Kubernetes"),
    ("main", "registry.k8s.io/kube-controller-manager",  "Kubernetes"),
    ("main", "registry.k8s.io/kube-scheduler",           "Kubernetes"),
    ("main", "registry.k8s.io/pause",                    "Kubernetes"),
    ("main", "registry.k8s.io/etcd",                     "etcd"),
    # A Kubernetes patch that moves .go-version must bring Go in the same PR, or
    # lint-go-version fails both halves. main is not listed: Go is disabled there.
    ("release-1.37", "go",                               "Kubernetes"),
    ("release-1.36", "docker.io/library/golang",         "Kubernetes"),
    ("release-1.35", "go",                               "Kubernetes"),
]

fail = 0
for branch, manager, dep, update, file, want in UPDATE_CASES:
    got = enabled(branch, dep, manager=manager, update=update, file=file)
    if got != want:
        print(f"test-fences: FAIL {branch} {manager} {dep} {update}: enabled={got}, want {want}")
        fail = 1
    else:
        print(f"test-fences: ok   {branch:13} {manager:14} {dep:24} {update:5} enabled={got}")

for branch, manager, dep, file, want in AGE_CASES:
    got = setting("minimumReleaseAge", None, branch, dep, manager=manager, file=file)
    if got != want:
        print(f"test-fences: FAIL {branch} {manager} {dep}: minimumReleaseAge={got!r}, want {want!r}")
        fail = 1
    else:
        print(f"test-fences: ok   {branch:13} {manager:14} {dep:24} minimumReleaseAge={got}")

for branch, dep, want in GROUP_CASES:
    got = group(branch, dep)
    # The branch comes from groupSlug, so a matching name alone is not one PR.
    slug, want_slug = (setting("groupSlug", None, branch, d) for d in (dep, "kubernetes/kubernetes"))
    if want == "Kubernetes" and slug != want_slug:
        got = f"{got} (groupSlug {slug!r}, not {want_slug!r})"
    if got != want:
        print(f"test-fences: FAIL {branch} {dep}: group={got!r}, want {want!r}")
        fail = 1
    else:
        print(f"test-fences: ok   {branch:13} {dep:24} group={got}")

for branch, dep, want in ENABLED_CASES:
    got = enabled(branch, dep)
    if got != want:
        print(f"test-fences: FAIL {branch} {dep}: enabled={got}, want {want}")
        fail = 1
    else:
        print(f"test-fences: ok   {branch:13} {dep:24} enabled={got}")

for branch, dep, version, want in CASES:
    got, fence = allowed(branch, dep, version)
    verdict = "allow" if got else "reject"
    if got != want:
        print(f"test-fences: FAIL {branch} {dep} {version}: got {verdict}, "
              f"want {'allow' if want else 'reject'} (fence: {fence})")
        fail = 1
    else:
        print(f"test-fences: ok   {branch:13} {dep:24} {version:9} {verdict:6} [{fence}]")

for branch, dep, current, version, want in CURRENT_CASES:
    got, fence = allowed(branch, dep, version, current=current)
    verdict = "allow" if got else "reject"
    if got != want:
        print(f"test-fences: FAIL {branch} {dep}@{current} {version}: got {verdict}, "
              f"want {'allow' if want else 'reject'} (fence: {fence})")
        fail = 1
    else:
        print(f"test-fences: ok   {branch:13} {dep:24} {current} -> {version:15} {verdict:6} [{fence}]")

for branch, dep, want in UNSTABLE_CASES:
    got = setting("ignoreUnstable", None, branch, dep)
    if got != want:
        print(f"test-fences: FAIL {branch} {dep}: ignoreUnstable={got!r}, want {want!r}")
        fail = 1
    else:
        print(f"test-fences: ok   {branch:13} {dep:24} ignoreUnstable={got}")

if fail:
    print("test-fences: FAILED — the fences do not mean what they claim", file=sys.stderr)
    sys.exit(1)
total = len(CASES) + len(ENABLED_CASES) + len(GROUP_CASES) + len(UPDATE_CASES) + len(AGE_CASES) \
    + len(CURRENT_CASES) + len(UNSTABLE_CASES)
print(f"test-fences: ok — {total} fence cases behave as documented")
PY
