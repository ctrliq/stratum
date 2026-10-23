"""Upstream compatibility data, fetched rather than remembered.

Every SKE fence, derived pin, and freshness metric traces back to one of these
functions, and each one names the upstream file it reads. That is the whole
point: the component support matrix exists in machine-readable form upstream,
so it should not also exist in someone's head.

Used by tools/gen-fences.sh (fences and derived pins),
tools/days-behind.sh (freshness), and tools/upstream-schedule.sh.
"""

import json
import os
import re
import sys
import urllib.error
import urllib.request

RAW = "https://raw.githubusercontent.com"
API = "https://api.github.com"

# Exit code for "could not determine, don't treat as a finding". CI can tell a
# rate limit apart from a real compatibility drift.
EXIT_INCONCLUSIVE = 3


class RateLimited(Exception):
    pass


_cache = {}


def fetch(url, timeout=30):
    """GET a URL, returning None on 404 so callers can probe for existence.

    Results are memoised: deriving fences for several branches re-reads the same
    containerd and Cilium documents, and the unauthenticated GitHub API allows
    only 60 requests an hour.
    """
    if url in _cache:
        return _cache[url]
    headers = {"User-Agent": "ske-upstream-sources"}
    token = os.environ.get("GITHUB_TOKEN")
    if token and url.startswith(API):
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            _cache[url] = r.read().decode()
            return _cache[url]
    except urllib.error.HTTPError as e:
        if e.code == 404:
            _cache[url] = None
            return None
        if e.code in (403, 429):
            raise RateLimited(
                f"GitHub API rate limit reached fetching {url}. "
                "Set GITHUB_TOKEN (CI passes github.token) and retry."
            ) from e
        raise


def main(fn):
    """Run a generator entry point, turning a rate limit into a clear exit 3."""
    try:
        fn()
    except RateLimited as e:
        print(f"upstream: INCONCLUSIVE - {e}", file=sys.stderr)
        sys.exit(EXIT_INCONCLUSIVE)


def die(msg):
    print(f"upstream: {msg}", file=sys.stderr)
    sys.exit(1)


# A Kubernetes minor upstream has not documented yet (containerd and Cilium
# both publish their rows weeks after a GA) inherits the previous minor's
# ceiling: nothing newer than what upstream recommends for N-1 gets onto the
# branch, and gen-fences.sh --check fails the day the real row appears. The
# inheritance is recorded here so the fence description can say so.
INHERITED = {}  # (component, minor) -> the minor whose ceiling was inherited


def _inherit(component, minor, ceiling, reason):
    major, n = minor.split(".")
    prev = f"{major}.{int(n) - 1}"
    if (component, prev) in INHERITED or int(n) <= 1:
        die(reason)  # one step back only: two undocumented minors is a real gap
    INHERITED[(component, minor)] = prev
    return ceiling(prev)


def ver(s):
    return tuple(int(x) for x in re.findall(r"\d+", s))


def supported_minors(path="build/branches.txt"):
    """The branch list — SKE's own source of truth, not an upstream one."""
    minors = [l.strip() for l in open(path) if l.strip() and not l.startswith("#")]
    if not minors:
        die(f"{path} lists no minors")
    return minors


def latest_patch(minor):
    """Current stable patch release of a Kubernetes minor. Source: dl.k8s.io."""
    v = fetch(f"https://dl.k8s.io/release/stable-{minor}.txt")
    if not v:
        die(f"no upstream stable release for Kubernetes {minor}")
    return v.strip().lstrip("v")


def kubeadm_constants(tag):
    """etcd, CoreDNS and pause versions kubeadm pins for a Kubernetes tag.

    Source: kubernetes/kubernetes cmd/kubeadm/app/constants/constants.go — the
    authoritative per-minor pin, and what upstream's own installer uses.
    """
    src = fetch(f"{RAW}/kubernetes/kubernetes/v{tag}/cmd/kubeadm/app/constants/constants.go")
    if src is None:
        die(f"cannot read kubeadm constants for v{tag}")
    out = {}
    for name, key in (("DefaultEtcdVersion", "etcd"),
                      ("CoreDNSVersion", "coredns"),
                      ("PauseVersion", "pause")):
        m = re.search(rf'{name}\s*=\s*"v?([0-9]+\.[0-9]+(?:\.[0-9]+)?)', src)
        if not m:
            die(f"{name} not found in kubeadm constants for v{tag}")
        out[key] = m.group(1)
    return out


def go_pin(tag, minor_only=True):
    """Go toolchain upstream pins for a Kubernetes tag. Source: .go-version."""
    v = fetch(f"{RAW}/kubernetes/kubernetes/v{tag}/.go-version")
    if v is None:
        die(f"no .go-version for Kubernetes v{tag}")
    v = v.strip()
    return ".".join(v.split(".")[:2]) if minor_only else v


_containerd_matrix = None


def containerd_ceiling(minor):
    """Highest containerd line containerd recommends for a Kubernetes minor.

    Source: containerd RELEASES.md "Kubernetes Support" matrix. Rows read like
    "2.3.0+, 2.2.0+", i.e. the minimum patch of each still-recommended line; the
    highest line present is the ceiling for that Kubernetes minor.
    """
    global _containerd_matrix
    if _containerd_matrix is None:
        src = fetch(f"{RAW}/containerd/containerd/main/RELEASES.md")
        if src is None:
            die("cannot read containerd RELEASES.md")
        _containerd_matrix = {}
        for row in re.finditer(r"^\|\s*(\d+\.\d+)\s*\|([^|]+)\|", src, re.M):
            lines = re.findall(r"(\d+\.\d+)\.\d+\+", row.group(2))
            if lines:
                _containerd_matrix[row.group(1)] = max(lines, key=ver)
    if minor not in _containerd_matrix:
        return _inherit("containerd", minor, containerd_ceiling,
                        f"containerd publishes no recommendation for Kubernetes {minor}")
    return _containerd_matrix[minor]


def cilium_minors(limit=8):
    """Recent Cilium minor versions, newest first. Source: GitHub tags."""
    tags = json.loads(fetch(f"{API}/repos/cilium/cilium/tags?per_page=100"))
    seen = []
    for t in tags:
        m = re.fullmatch(r"v(\d+\.\d+)\.\d+", t["name"])
        if m and m.group(1) not in seen:
            seen.append(m.group(1))
    return sorted(seen, key=ver, reverse=True)[:limit]


def cilium_ceiling(minor):
    """Highest Cilium minor whose compatibility table lists a Kubernetes minor.

    Source: cilium Documentation/network/kubernetes/compatibility.rst on the
    Cilium release branch. Cilium states the Kubernetes versions it e2e-tests,
    and freezes that list for a release branch's maintenance period.
    """
    for cminor in cilium_minors():
        doc = fetch(f"{RAW}/cilium/cilium/v{cminor}/Documentation/network/kubernetes/compatibility.rst")
        if doc and re.search(rf"(?<![\d.]){re.escape(minor)}(?![\d.])", doc):
            return cminor
    return _inherit("Cilium", minor, cilium_ceiling,
                    f"no recent Cilium release documents support for Kubernetes {minor}")


def latest_release(repo):
    """Newest plain-semver tag of a repo, without the leading v.

    Deliberately not /releases/latest: that endpoint reflects whatever the
    project marked as latest, and some projects mark something odd. Kubernetes'
    apiserver-network-proxy, for instance, marks an old klog-suffixed tag as its
    latest release while its real newest release is many minors ahead.
    """
    tags = json.loads(fetch(f"{API}/repos/{repo}/tags?per_page=100"))
    cands = [t["name"].lstrip("v") for t in tags
             if re.fullmatch(r"v?\d+\.\d+\.\d+", t["name"])]
    if not cands:
        die(f"no plain-semver tag found for {repo}")
    return max(cands, key=ver)


def line_of(version):
    """The major.minor line a version belongs to."""
    return ".".join(version.split(".")[:2])


def latest_in_line(repo, line):
    """Newest release of repo within a given major.minor line, e.g. 2.2."""
    tags = json.loads(fetch(f"{API}/repos/{repo}/releases?per_page=100"))
    cands = [t["tag_name"].lstrip("v") for t in tags
             if re.fullmatch(rf"v?{re.escape(line)}\.\d+", t["tag_name"])]
    if not cands:
        die(f"no {repo} release found in the {line} line")
    return max(cands, key=ver)


def tag_commit(repo, tag):
    """Resolve a tag to its commit SHA, dereferencing annotated tags."""
    o = json.loads(fetch(f"{API}/repos/{repo}/git/ref/tags/{tag}"))["object"]
    if o["type"] == "tag":
        o = json.loads(fetch(o["url"]))["object"]
    return o["sha"]


def tag_release_date(repo, tag):
    """Publication date (YYYY-MM-DD) of a release tag, for freshness metrics."""
    r = fetch(f"{API}/repos/{repo}/releases/tags/{tag}")
    if r is None:
        return None
    return json.loads(r)["published_at"][:10]
