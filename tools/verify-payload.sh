#!/usr/bin/env bash
# Asserts that the images embedded in a packaged stratum binary are exactly the
# set pinned in build/images.txt, matched by digest — under the MIRRORED names
# (tools/mirror-images.sh), because those are the names a running cluster asks
# for and therefore the names the payload has to register — AND that each of
# those images is actually COMPLETE in the archive: the target-platform manifest,
# its config, and every layer blob physically present.
#
# This is the invariant the airgap claim rests on: the artifact installs with no
# registry access, so whatever it carries IS the image set — there is no pull to
# correct a mismatch later. It is a release gate.
#
# The completeness half exists because `docker save` from the containerd image
# store, given a multi-arch image, can write a correct top-level index (matching
# the pinned manifest-list digest) while silently omitting some of the pulled
# platform's config/layer blobs. Such a tar passes a name+digest-only check but
# fails at runtime with `ctr: content digest sha256:…: not found`, which stalls
# kubelet's WaitFor forever. Checking only the outer index is structurally unable
# to catch that; this walks each manifest tree down to the blobs `ctr images
# import` actually needs.
#
# Usage:
#   tools/verify-payload.sh dist/stratum        # verify a packaged binary
#   tools/verify-payload.sh --tar staging/.../images/stratum-images.tar
#                                               # verify a raw staged archive
#   tools/verify-payload.sh --dash-tar staging/.../images/stratum-dash.tar [<ref>]
#                                               # verify the built dashboard image
# A packaged binary must also carry images/stratum-dash.tar; set DASH_REF to
# require its exact name (the default accepts any tag under the registry).
# The --tar form runs right after `make images`, before there is a packaged
# binary to unzip, so a broken bundle fails the build at its source.
set -euo pipefail

cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# verify_dash <archive> <want>: the dashboard image the build makes rather than
# mirrors. It has no pin, so it is checked by name and by its own digests:
# exactly one image, named <want> (or, for a "<registry>/stratum-dash:" prefix,
# any tag under it), with its manifest, config and layer all present and hashing
# to the digests that reference them.
verify_dash() {
  python3 - "$1" "$2" <<'DASH'
import hashlib, json, re, sys, tarfile

tarpath, want = sys.argv[1], sys.argv[2]

def die(detail):
    print("verify-payload: %s" % detail, file=sys.stderr)
    print("verify-payload: FAILED - the dashboard image archive is not usable: %s" % tarpath, file=sys.stderr)
    sys.exit(1)

try:
    tf = tarfile.open(tarpath)
    members = {(m.name[2:] if m.name.startswith("./") else m.name): m for m in tf.getmembers() if m.isfile()}
except (tarfile.TarError, OSError, EOFError) as e:
    die("the dashboard image archive is truncated or corrupt: %s" % e)

def read(name):
    if name not in members:
        return None
    try:
        return tf.extractfile(members[name]).read()
    except (tarfile.TarError, OSError, EOFError) as e:
        die("the dashboard image archive is truncated or corrupt: %s" % e)

def blob(desc, where):
    d = desc.get("digest") if isinstance(desc, dict) else None
    if not isinstance(d, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", d):
        die("%s has no sha256 digest" % where)
    b = read("blobs/sha256/" + d[7:])
    if b is None:
        die("%s blob %s is missing" % (where, d))
    if "sha256:" + hashlib.sha256(b).hexdigest() != d:
        die("%s blob %s does not hash to its digest" % (where, d))
    return b

try:
    index = json.loads(read("index.json") or b"null")
    entries = index.get("manifests") if isinstance(index, dict) else None
    if not isinstance(entries, list) or len(entries) != 1:
        die("the dashboard image archive must hold exactly one image")
    ann = entries[0].get("annotations") if isinstance(entries[0], dict) else None
    name = ann.get("io.containerd.image.name") if isinstance(ann, dict) else None
    if not isinstance(name, str) or not (name == want or (want.endswith(":") and name.startswith(want))):
        die("the dashboard image is named %r, want %r" % (name, want))
    manifest = json.loads(blob(entries[0], "the dashboard manifest"))
    if not isinstance(manifest, dict) or not isinstance(manifest.get("layers"), list):
        die("the dashboard manifest is not an image manifest")
    if not isinstance(json.loads(blob(manifest.get("config"), "the dashboard config")), dict):
        die("the dashboard config is not an image config")
    for layer in manifest["layers"]:
        blob(layer, "a dashboard layer")
except ValueError as e:
    die("the dashboard image archive has malformed JSON: %s" % e)
print("verify-payload: ok - dashboard image %s present and complete" % name)
DASH
}

DASH_PREFIX="$(sed -n 's/^image_registry *= *//p' build/versions.mk | tr -d ' ')/stratum-dash:"

if [[ ${1:-} == --dash-tar ]]; then
  tar=${2:-}
  [[ -f $tar ]] || { echo "verify-payload: no such archive: $tar" >&2; exit 1; }
  verify_dash "$tar" "${3:-$DASH_PREFIX}"
  exit 0
fi

if [[ ${1:-} == --tar ]]; then
  tar=${2:-}
  [[ -f $tar ]] || { echo "verify-payload: no such archive: $tar" >&2; exit 1; }
else
  BIN=${1:-dist/stratum}
  [[ -f $BIN ]] || { echo "verify-payload: no such artifact: $BIN" >&2; exit 1; }
  # The appended ZIP is readable straight out of the executable. unzip exits 1
  # with a warning about the "extra bytes at beginning" — that is the ELF, and
  # it is the whole point. That ONE warning is benign; a bad CRC (exit 2) or a
  # missing central directory (exit 9) is a damaged payload. Discarding the
  # status AND stderr made them the same event, so a corrupt archive got no
  # diagnostic here and reappeared later as a tarfile traceback — or, for a
  # member unzip writes before reporting its bad CRC, as a confident verdict
  # about garbage. Capture both and judge them where the failure happens.
  # Without this, a missing unzip surfaces as status 127 and gets reported below
  # as a damaged ZIP — the one wrong answer the rest of this block exists to
  # avoid giving.
  command -v unzip >/dev/null \
    || { echo "verify-payload: unzip is not installed" >&2; exit 1; }
  unzip_rc=0
  unzip -o -q "$BIN" 'images/*' -d "$work" 2>"$work/unzip.log" || unzip_rc=$?
  # Everything unzip said except that warning (two lines: it and its
  # "attempting to process anyway" continuation).
  unzip_said=$(grep -vE 'extra bytes at beginning|attempting to process anyway|^[[:space:]]*$' \
    "$work/unzip.log" || true)
  if [[ $unzip_rc -eq 11 ]]; then
    : # "no matching files": no images/ member at all — the emptiness check below
      # is the accurate diagnosis for that, not "damaged".
  elif [[ $unzip_rc -gt 1 || -n $unzip_said ]]; then
    echo "verify-payload: the ZIP appended to $BIN is damaged (unzip exit $unzip_rc)" >&2
    [[ -z $unzip_said ]] || printf '%s\n' "$unzip_said" >&2
    echo "verify-payload: FAILED - the payload's embedded archive could not be extracted" >&2
    exit 1
  fi
  # `|| true`: find exits non-zero when images/ was never created, and under
  # `set -o pipefail` that failed the assignment and killed the script with no
  # message at all — swallowing the very check on the next line.
  tar=$(find "$work/images" -name '*-images.tar' 2>/dev/null | head -1 || true)
  [[ -n $tar ]] || { echo "verify-payload: no image archive in the payload" >&2; exit 1; }
  [[ -f $work/images/stratum-dash.tar ]] \
    || { echo "verify-payload: no dashboard image archive (images/stratum-dash.tar) in the payload" >&2; exit 1; }
  verify_dash "$work/images/stratum-dash.tar" "${DASH_REF:-$DASH_PREFIX}"
fi

tools/mirror-images.sh --list > "$work/pinned.txt"
python3 - "$tar" "$work/pinned.txt" <<'PY'
import json, re, sys, tarfile

tarpath, pinfile = sys.argv[1], sys.argv[2]

# The mirrored pin list is "<name>:<tag>@<digest>". Both halves matter: the
# digest is the pin, and the name is what `ctr images import` uses to register
# the image. An archive with the right digests but no names imports silently and
# usefully does nothing, so verify the pairs, not just the digests. The name
# side is also how a payload built before the mirror existed gets caught: same
# digests, upstream names, and every manifest would ImagePullBackOff.
pinned = {}
for line in open(pinfile):
    line = line.strip()
    if not line or line.startswith("#"):
        continue
    name, _, digest = line.partition("@")
    pinned[(name, digest)] = line

def die(detail):
    """Every "the archive itself is unusable" exit goes through here. A damaged
    payload has to read as a verdict about the payload — naming the archive and
    the damage — and not as a Python traceback, which reads like verify-payload
    is the thing that is broken."""
    print("verify-payload: %s" % detail, file=sys.stderr)
    print("verify-payload: FAILED - the image archive is truncated or corrupt: %s"
          % tarpath, file=sys.stderr)
    sys.exit(1)


# A half-written or partially extracted archive fails here, and this is the
# first place that can say so. tarfile.open() reads only the first header, so a
# cut further in survives it and raises during the member scan instead: both
# need catching, or one kind of truncation still traces back.
try:
    tf = tarfile.open(tarpath)
except (tarfile.TarError, OSError, EOFError) as e:
    die("cannot open the image archive: %s" % e)

# Member names in a `docker save` OCI archive are "blobs/sha256/<hex>" (some
# tars prefix "./"); index at "index.json". Build the set of present blob
# digests once, and a name->member map for reading the small manifest blobs.
members = {}
present = set()  # "sha256:<hex>" for every blob physically in the tar
try:
    # getnames() walks headers and seeks over the bodies. That is sufficient:
    # a member cut short makes the seek run off the end and tarfile raises
    # here, which is caught below — see the layer-cut case in
    # tools/test-verify-payload.sh, which is diagnosed with or without a body
    # read. Draining every member instead would read every blob in the payload
    # on every run, and no case in that suite distinguishes the two.
    entries = tf.getmembers()
except (tarfile.TarError, OSError, EOFError) as e:
    die("cannot list the image archive's members: %s" % e)
for entry in entries:
    n = entry.name[2:] if entry.name.startswith("./") else entry.name
    members[n] = entry.name
    # Only a REGULAR member counts as a blob. A directory, symlink or device
    # entry sitting at a referenced config/layer path would otherwise satisfy
    # manifest_complete() and the image would be reported complete, while
    # `ctr images import` cannot load any of it. The name is still recorded in
    # `members`, so read_json() can say what it found rather than "absent".
    if n.startswith("blobs/sha256/") and entry.isfile():
        present.add("sha256:" + n[len("blobs/sha256/"):])

def read_json(member):
    # Reading a member is a second chance to hit the truncation: the header was
    # intact enough to list, the bytes behind it were not.
    try:
        f = tf.extractfile(members[member])
        # None for a directory, symlink or device entry: json.load(None) would
        # raise AttributeError, which is not an archive diagnostic.
        if f is None:
            die("%s in the image archive is not a regular file" % member)
        return json.load(f)
    except (tarfile.TarError, OSError, ValueError) as e:
        die("cannot read %s from the image archive: %s" % (member, e))

def blob_member(digest):
    return "blobs/sha256/" + digest.split(":", 1)[1]

def load_blob_json(digest):
    m = blob_member(digest)
    if m not in members:
        return None
    return read_json(m)

# No index.json at all: whatever this is, `ctr images import` cannot read it
# either, and saying so beats a KeyError.
if "index.json" not in members:
    die("the image archive has no index.json - it is not a `docker save` archive")

index = read_json("index.json")

# Valid JSON is not a valid index: `{}` or a list would reach index["manifests"]
# below and raise KeyError or TypeError, which reads as a crash rather than the
# archive diagnostic this script promises.
if not isinstance(index, dict) or not isinstance(index.get("manifests"), list):
    die("the image archive's index.json has no manifests list - it is not a "
        "`docker save` archive")

# The archive's top-level index lists one entry per image, each tagged with the
# image name and pinned by its top manifest digest (a manifest LIST for a
# multi-arch image). Match those (name, digest) pairs against the pinned set...
def descriptor_digest(d, where):
    # Every descriptor in an OCI index carries a digest. One that does not is
    # not a descriptor, and dereferencing it is a KeyError/TypeError rather
    # than the archive diagnostic this script promises.
    if not isinstance(d, dict) or not isinstance(d.get("digest"), str):
        die("the image archive has a malformed descriptor in %s - it is not a "
            "`docker save` archive" % where)
    # Blob lookup supports the canonical SHA-256 layout only. A different
    # algorithm must never resolve to a same-named file under blobs/sha256.
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", d["digest"]):
        die("the image archive has a descriptor in %s whose digest is not a "
            "supported sha256:<64 lowercase hex digits> reference: %r" % (where, d["digest"]))
    return d["digest"]

present_pairs = {}
for m in index["manifests"]:
    ann = m.get("annotations") if isinstance(m, dict) else None
    # A truthy non-mapping (a string, say) survives `ann or {}` and then blows
    # up on .get(). Anything that is not a mapping is simply no annotations.
    if not isinstance(ann, dict):
        ann = {}
    digest = descriptor_digest(m, "index.json")
    name = ann.get("io.containerd.image.name", "<unnamed>")
    if not isinstance(name, str):
        die("the image archive has a non-string image-name annotation")
    present_pairs[(name, digest)] = m

missing = [ref for key, ref in sorted(pinned.items()) if key not in present_pairs]
extra = [f"{n}@{d}" for (n, d) in sorted(present_pairs) if (n, d) not in pinned]

# ...then walk each pinned image's manifest tree down to the blobs a real
# `ctr images import` must resolve. For a manifest LIST, at least one platform
# child must be fully present (child manifest + config + every layer): the
# bundle is single-platform by design, so sibling children are expected absent,
# but the ONE platform that is shipped must be complete. For a plain manifest,
# check its own config + layers.
incomplete = []  # (image_ref, reason)

def manifest_complete(mfst, where):
    """Return the blobs an image manifest references but the tar does not carry.
    The config descriptor is mandatory — `ctr images import` must load it — so a
    manifest with no config (or one whose config/layer blob is absent) is
    incomplete, not vacuously complete."""
    # The blob parsed as JSON; that does not make it a manifest. A scalar, a
    # list, or an object whose config/layers are the wrong type would reach the
    # scan below and raise AttributeError or TypeError — a traceback in place
    # of the archive diagnostic this script exists to give.
    if not isinstance(mfst, dict):
        die("the image archive's %s is not an image manifest" % where)
    cfg = mfst.get("config", {})
    if not isinstance(cfg, dict):
        die("the image archive's %s has a malformed config descriptor" % where)
    layers = mfst.get("layers", [])
    if not isinstance(layers, list) or not all(isinstance(l, dict) for l in layers):
        die("the image archive's %s has a malformed layers list" % where)
    gaps = []
    cfg_digest = descriptor_digest(cfg, where + " config") if cfg else None
    if not cfg_digest:
        gaps.append("<no config descriptor>")
    elif cfg_digest not in present:
        gaps.append(cfg_digest)
    for layer in layers:
        digest = descriptor_digest(layer, where + " layers")
        if digest not in present:
            gaps.append(digest)
    return gaps

for (name, digest), ref in sorted(pinned.items()):
    if (name, digest) not in present_pairs:
        continue  # already reported as MISSING above
    top = load_blob_json(digest)
    if top is None:
        incomplete.append((ref, f"top manifest blob {digest} absent"))
        continue
    if not isinstance(top, dict):
        die("the image archive's top manifest for %s is not an object" % ref)
    mt = top.get("mediaType", "")
    if not isinstance(mt, str):
        die("the image archive's top manifest for %s has a non-string mediaType" % ref)
    is_list = "list" in mt or "index" in mt or "manifests" in top
    if not is_list:
        gaps = manifest_complete(top, "top manifest for %s" % ref)
        if gaps:
            incomplete.append((ref, "missing blobs: " + ", ".join(gaps)))
        continue
    # Manifest list: find a platform child that is fully present.
    children = top.get("manifests", [])
    if not isinstance(children, list):
        die("the image archive's manifest list for %s has a malformed "
            "manifests field" % ref)
    complete_child = None
    child_gap = None
    for c in children:
        child_digest = descriptor_digest(c, "a manifest list")
        if blob_member(child_digest) not in members:
            continue  # sibling platform not shipped — expected
        cm = load_blob_json(child_digest)
        gaps = manifest_complete(cm, "child manifest for %s" % ref)
        if not gaps:
            complete_child = c
            break
        # A child whose manifest IS present but whose content is not is the
        # exact etcd failure — remember it for a precise message.
        plat = c.get("platform", {})
        if not isinstance(plat, dict):
            plat = {}
        child_gap = "%s/%s child %s missing blobs: %s" % (
            plat.get("os", "?"), plat.get("architecture", "?"),
            descriptor_digest(c, "a manifest list"), ", ".join(gaps))
    if complete_child is None:
        incomplete.append((ref, child_gap or "no platform child manifest present in the archive"))

for ref in missing:
    print("verify-payload: MISSING  %s" % ref)
for ref in extra:
    print("verify-payload: UNPINNED %s" % ref)
for ref, reason in incomplete:
    print("verify-payload: INCOMPLETE %s — %s" % (ref, reason))

if missing or extra or incomplete:
    print("verify-payload: FAILED - the payload is not the complete pinned image set",
          file=sys.stderr)
    sys.exit(1)
print("verify-payload: ok - %d images present, complete, mirrored names and digests match the pinned set" % len(pinned))
PY
