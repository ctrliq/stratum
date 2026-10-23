#!/usr/bin/env bash
# Emits build-info.json: everything an outside party needs to rebuild this
# release and match hashes. EKS-D publishes source refs, patch lists and build
# flags per release; this is that, generated rather than hand-maintained.
#
# Usage: tools/build-info.sh [artifact-dir]   (default: dist)
set -euo pipefail

cd "$(dirname "$0")/.."
ARTIFACT_DIR=${1:-dist}
python3 - "$ARTIFACT_DIR" <<'PY'
import hashlib, json, os, pathlib, re, subprocess, sys

artifact_dir = pathlib.Path(sys.argv[1])
manifest = pathlib.Path("build/versions.mk").read_text()

def pins():
    out = {}
    for m in re.finditer(r'^([a-z_-]+?)_(version|revision)[ \t]*=[ \t]*(\S*)[ \t]*$', manifest, re.M):
        name, field, val = m.group(1), m.group(2), m.group(3)
        if val:
            out.setdefault(name, {})[field] = val
    return out

def flags(component):
    out = {}
    for m in re.finditer(rf'^{re.escape(component)}_build_(\S+)[ \t]*=[ \t]*(.*)$', manifest, re.M):
        val = m.group(2).strip()
        if val:
            out[m.group(1)] = val
    return out

def patches(component):
    d = pathlib.Path("build/components") / component / "patches"
    result = []
    for p in sorted(d.glob("*.patch")) if d.is_dir() else []:
        header = p.read_text(errors="replace").split("\n---\n")[0]
        def trailer(key):
            m = re.search(rf'^{key}:\s*(.+)$', header, re.M)
            return m.group(1).strip() if m else None
        result.append({
            "file": p.name,
            "upstream": trailer("Upstream-PR") or trailer("Upstream-Issue"),
            "carry_reason": trailer("Carry-Reason"),
            "justification": trailer("Justification"),
            "changed_lines": sum(1 for l in p.read_text(errors="replace").splitlines()
                                 if re.match(r'^[+-][^+-]', l)),
        })
    return result

def sh(*cmd):
    try:
        return subprocess.run(cmd, capture_output=True, text=True, check=True).stdout.strip()
    except Exception:
        return None

built = [d.name for d in sorted(pathlib.Path("build/components").iterdir())
         if (d / "Dockerfile").is_file()]
p = pins()

components = {}
for c in built:
    components[c] = {
        "version": p.get(c, {}).get("version"),
        "revision": p.get(c, {}).get("revision"),
        "build_image": None,
        "build_flags": flags(c),
        "patches": patches(c),
    }
    m = re.search(rf'^{c}_buildimage[ \t]*=[ \t]*(.*)$', manifest, re.M)
    if m:
        img = m.group(1).strip()
        # Resolve the make-variable indirection so the record is literal.
        img = img.replace("$(default_buildimage)",
                          f"docker.io/library/golang:{p['go']['version']}-alpine{p['alpine']['version']}")
        components[c]["build_image"] = img

artifacts = {}
if artifact_dir.is_dir():
    for f in sorted(artifact_dir.iterdir()):
        if f.is_file() and f.suffix not in (".sig", ".pem"):
            artifacts[f.name] = {
                "sha256": hashlib.sha256(f.read_bytes()).hexdigest(),
                "size": f.stat().st_size,
            }

info = {
    "version": sh("tools/version.sh", "--stamp"),
    "source": {
        "commit": sh("git", "rev-parse", "HEAD"),
        "tree_state": "clean" if not sh("git", "status", "--porcelain") else "dirty",
        "versions_mk_sha256": hashlib.sha256(
            pathlib.Path("build/versions.mk").read_bytes()).hexdigest(),
    },
    "toolchain": {
        "go": p.get("go", {}).get("version"),
        "alpine": p.get("alpine", {}).get("version"),
    },
    "target": {
        "os": os.environ.get("TARGET_OS", "linux"),
        "arch": os.environ.get("TARGET_ARCH") or (sh("uname", "-m") or "").replace("x86_64", "amd64").replace("aarch64", "arm64"),
    },
    "components": components,
    "images": [l.strip() for l in pathlib.Path("build/images.txt").read_text().splitlines()
               if l.strip() and not l.strip().startswith("#")],
    "artifacts": artifacts,
    "reproduce": "make components package && tools/build-info.sh — artifact sha256s must match this file",
}

out = pathlib.Path("build-info.json")
out.write_text(json.dumps(info, indent=2, sort_keys=False) + "\n")
print(f"wrote {out} ({len(artifacts)} artifact(s), {len(components)} component(s))")
PY
