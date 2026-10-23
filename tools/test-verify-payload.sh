#!/usr/bin/env bash
# Guards verify-payload's diagnostics: a damaged payload must fail with its own
# diagnostics, naming the archive and what is wrong with it — never with a
# Python traceback, and never with a message that misdescribes the damage.
#
# Two swallowed-failure shapes would produce that, and the cases below fail
# if either comes back:
#   * `unzip ... 2>/dev/null || true` discards BOTH the status and the stderr
#     of the extraction. The benign warning it would be there for ("extra
#     bytes at beginning", i.e. the ELF the ZIP is appended to) becomes
#     indistinguishable from a bad CRC or a missing central directory, so a
#     corrupt payload is reported as "no image archive in the payload" — or,
#     worse, half-extracted and handed to Python.
#   * `tarfile.open(tarpath)` with no handler raises tarfile.ReadError at the
#     operator when the tar is half-extracted.
#
# Hermetic: every fixture is built here with python3's stdlib (zipfile,
# tarfile), so this needs no payload build, no docker and no network — the
# fixtures ARE the damage, constructed deliberately rather than hoped for.
set -euo pipefail
cd "$(dirname "$0")/.."

SCRIPT=$PWD/tools/verify-payload.sh
pass=0
fail() { echo "test-verify-payload: FAIL  $*" >&2; exit 1; }
ok()   { pass=$((pass + 1)); }

command -v unzip >/dev/null || { echo "test-verify-payload: unzip not installed — skipping" >&2; exit 0; }

TEST_TMP=$(mktemp -d)
trap 'rm -rf -- "$TEST_TMP"' EXIT

# --- fixtures ----------------------------------------------------------------
# full.tar is a plausible `docker save` OCI layout (oci-layout + index.json +
# blob members). Blob content is deliberately NON-zero: a run of zero bytes at
# the end of a tar reads as the end-of-archive marker, so a zero-filled fixture
# truncates *silently* and would prove nothing.
#
# The truncation offsets are chosen for the two distinct tarfile failures:
#   head (300 bytes) cuts the first header  -> ReadError from tarfile.open()
#   mid  (60000)     cuts mid-member        -> ReadError from the member scan
# A guard on open() alone passes the first and still traces back on the second.
# One real pin, so a fixture can name an image the walk will actually follow.
# Captured whole and sliced in the shell rather than piped through `head`:
# under `set -o pipefail` a producer that outpaces the pipe buffer is killed
# with SIGPIPE and the assignment fails (exit 141). The list is short enough
# today that it would not, which is exactly what makes that a latent flake.
PIN_LIST=$(tools/mirror-images.sh --list)
PIN=${PIN_LIST%%$'\n'*}
[ -n "$PIN" ] || { echo "test-verify-payload: no pinned images to build a fixture from" >&2; exit 1; }
python3 - "$TEST_TMP" "$PIN" <<'PY'
import io, json, os, sys, tarfile, zipfile

tmp = sys.argv[1]
pin_ref = sys.argv[2]
pin_name, _, pin_digest = pin_ref.partition("@")
p = lambda *a: os.path.join(tmp, *a)

with tarfile.open(p("full.tar"), "w") as tf:
    def add(name, data):
        ti = tarfile.TarInfo(name); ti.size = len(data)
        tf.addfile(ti, io.BytesIO(data))
    add("oci-layout", b'{"imageLayoutVersion":"1.0.0"}')
    # An empty manifest list: structurally valid, so the control case below
    # reaches the pin comparison and reports every pinned image as MISSING.
    add("index.json", json.dumps({"manifests": []}).encode())
    for i in range(12):
        add("blobs/sha256/%064x" % i, bytes(range(256)) * 80)

# index.json present as a DIRECTORY entry: extractfile() returns None, and
# json.load(None) raises AttributeError, which no except clause catches.
with tarfile.open(p("indexdir.tar"), "w") as tf:
    ti = tarfile.TarInfo("index.json"); ti.type = tarfile.DIRTYPE
    tf.addfile(ti)

# Valid JSON, wrong shape: `{}` reaches index["manifests"] and raises KeyError.
with tarfile.open(p("indexempty.tar"), "w") as tf:
    data = json.dumps({}).encode()
    ti = tarfile.TarInfo("index.json"); ti.size = len(data)
    tf.addfile(ti, io.BytesIO(data))

full = open(p("full.tar"), "rb").read()
open(p("trunc-head.tar"), "wb").write(full[:300])
open(p("trunc-mid.tar"), "wb").write(full[:60000])

import hashlib

def dash_tar(dest, name="ghcr.io/ctrliq/stratum/stratum-dash:v0.0.0", drop_layer=False, tamper_layer=False, config=None):
    """A minimal, valid single-image OCI archive like tools/ociimage writes."""
    def digest(b):
        return "sha256:" + hashlib.sha256(b).hexdigest()
    layer = b"dashboard layer " * 64
    if config is None:
        config = json.dumps({"architecture": "amd64", "os": "linux",
                             "rootfs": {"type": "layers", "diff_ids": [digest(layer)]}}).encode()
    manifest = json.dumps({"schemaVersion": 2,
                           "config": {"digest": digest(config), "size": len(config)},
                           "layers": [{"digest": digest(layer), "size": len(layer)}]}).encode()
    index = json.dumps({"manifests": [{"digest": digest(manifest), "size": len(manifest),
                                       "annotations": {"io.containerd.image.name": name}}]}).encode()
    stored_layer = layer[:-1] + b"X" if tamper_layer else layer
    with tarfile.open(dest, "w") as tf:
        def add(n, data):
            ti = tarfile.TarInfo(n); ti.size = len(data)
            tf.addfile(ti, io.BytesIO(data))
        add("oci-layout", b'{"imageLayoutVersion":"1.0.0"}')
        add("index.json", index)
        add("blobs/sha256/" + digest(config)[7:], config)
        add("blobs/sha256/" + digest(manifest)[7:], manifest)
        if not drop_layer:
            add("blobs/sha256/" + digest(layer)[7:], stored_layer)

dash_tar(p("dash-valid.tar"))
dash_tar(p("dash-wrongname.tar"), name="docker.io/library/nginx:latest")
dash_tar(p("dash-nolayer.tar"), drop_layer=True)
dash_tar(p("dash-tampered.tar"), tamper_layer=True)
dash_tar(p("dash-badconfig.tar"), config=b"not valid JSON")
open(p("dash-trunc.tar"), "wb").write(open(p("dash-valid.tar"), "rb").read()[:700])
DASH_VALID = open(p("dash-valid.tar"), "rb").read()

def zip_of(tar_bytes, dest, dash=DASH_VALID):
    with zipfile.ZipFile(dest, "w", zipfile.ZIP_STORED) as z:
        z.writestr("images/stratum-images.tar", tar_bytes)
        if dash is not None:
            z.writestr("images/stratum-dash.tar", dash)

# The ELF the ZIP is appended to. Only its length matters to unzip (it is the
# "extra bytes at beginning" the benign warning is about), but give it the real
# magic so the fixture looks like what it stands for.
ELF = b"\x7fELF" + bytes(4096)

def binary(dest, zip_src, cut=None, corrupt=False):
    z = bytearray(open(zip_src, "rb").read())
    if corrupt:  # flip bytes inside the stored member -> unzip reports a bad CRC
        mid = len(z) // 2
        for i in range(mid, mid + 64):
            z[i] ^= 0xFF
    raw = ELF + bytes(z)
    if cut:      # lop off the tail -> the central directory is gone entirely
        raw = raw[:int(len(raw) * cut)]
    open(dest, "wb").write(raw)

zip_of(full, p("full.zip"))
zip_of(open(p("trunc-mid.tar"), "rb").read(), p("trunctar.zip"))

binary(p("bin-zip-truncated"), p("full.zip"), cut=0.7)
binary(p("bin-zip-badcrc"), p("full.zip"), corrupt=True)
binary(p("bin-truncated-tar"), p("trunctar.zip"))
binary(p("bin-valid"), p("full.zip"))

zip_of(full, p("nodash.zip"), dash=None)
binary(p("bin-no-dash"), p("nodash.zip"))
zip_of(full, p("baddash.zip"), dash=open(p("dash-tampered.tar"), "rb").read())
binary(p("bin-bad-dash"), p("baddash.zip"))

zip_of(open(p("indexdir.tar"), "rb").read(), p("indexdir.zip"))
zip_of(open(p("indexempty.tar"), "rb").read(), p("indexempty.zip"))
binary(p("bin-index-dir"), p("indexdir.zip"))
binary(p("bin-index-empty"), p("indexempty.zip"))

# A manifests LIST whose entry carries no digest: parses, passes the list
# check, then raises KeyError at m["digest"].
with tarfile.open(p("baddesc.tar"), "w") as tf:
    data = json.dumps({"manifests": [{}]}).encode()
    ti = tarfile.TarInfo("index.json"); ti.size = len(data)
    tf.addfile(ti, io.BytesIO(data))
zip_of(open(p("baddesc.tar"), "rb").read(), p("baddesc.zip"))
binary(p("bin-bad-descriptor"), p("baddesc.zip"))

# A descriptor whose digest is a non-empty string but not <algo>:<hex>:
# blob_member() splits on ":" and indexes [1], raising IndexError.
with tarfile.open(p("baddigest.tar"), "w") as tf:
    data = json.dumps({"manifests": [{"digest": "not-a-digest"}]}).encode()
    ti = tarfile.TarInfo("index.json"); ti.size = len(data)
    tf.addfile(ti, io.BytesIO(data))
zip_of(open(p("baddigest.tar"), "rb").read(), p("baddigest.zip"))
binary(p("bin-bad-digest"), p("baddigest.zip"))

# annotations present but not a mapping: `ann or {}` keeps the string and
# ann.get(...) raises AttributeError.
with tarfile.open(p("badann.tar"), "w") as tf:
    data = json.dumps({"manifests": [
        {"digest": "sha256:" + "0" * 64, "annotations": "not-a-mapping"}]}).encode()
    ti = tarfile.TarInfo("index.json"); ti.size = len(data)
    tf.addfile(ti, io.BytesIO(data))
zip_of(open(p("badann.tar"), "rb").read(), p("badann.zip"))
binary(p("bin-bad-annotations"), p("badann.zip"))

# A PINNED image whose top manifest blob parses but is a list, not an object.
# Naming a real pin is what gets the walk to follow it at all.
with tarfile.open(p("badtop.tar"), "w") as tf:
    def add(name, data):
        ti = tarfile.TarInfo(name); ti.size = len(data)
        tf.addfile(ti, io.BytesIO(data))
    add("index.json", json.dumps({"manifests": [{
        "digest": pin_digest,
        "annotations": {"io.containerd.image.name": pin_name},
    }]}).encode())
    add("blobs/sha256/" + pin_digest.split(":", 1)[1], json.dumps(["not", "an", "object"]).encode())
zip_of(open(p("badtop.tar"), "rb").read(), p("badtop.zip"))
binary(p("bin-bad-topmanifest"), p("badtop.zip"))
# Referenced child shapes must be diagnosed, including nested digest types.
child_digest = "sha256:" + "c" * 64
for label, child in {
    "scalar": 7, "list": [], "null": None,
    "config-list": {"config": [], "layers": []},
    "layers-object": {"config": {}, "layers": {}},
    "config-digest-list": {"config": {"digest": []}, "layers": []},
    "layer-digest-object": {"config": {}, "layers": [{"digest": {}}]},
}.items():
    with tarfile.open(p("child-" + label + ".tar"), "w") as tf:
        def add(name, obj):
            data = json.dumps(obj).encode()
            ti = tarfile.TarInfo(name); ti.size = len(data)
            tf.addfile(ti, io.BytesIO(data))
        add("index.json", {"manifests": [{"digest": pin_digest,
            "annotations": {"io.containerd.image.name": pin_name}}]})
        add("blobs/sha256/" + pin_digest.split(":", 1)[1],
            {"manifests": [{"digest": child_digest}]})
        add("blobs/sha256/" + "c" * 64, child)

# A complete pinned image tree with its last layer body cut after the intact
# header. The verifier must diagnose corruption, not just other missing pins.
with tarfile.open(p("layer-full.tar"), "w") as tf:
    def add(name, data):
        ti = tarfile.TarInfo(name); ti.size = len(data)
        tf.addfile(ti, io.BytesIO(data))
    add("index.json", json.dumps({"manifests": [{"digest": pin_digest,
        "annotations": {"io.containerd.image.name": pin_name}}]}).encode())
    add("blobs/sha256/" + pin_digest.split(":", 1)[1], json.dumps({
        "config": {"digest": "sha256:" + "a" * 64},
        "layers": [{"digest": "sha256:" + "b" * 64}]}).encode())
    add("blobs/sha256/" + "a" * 64, b"{}")
    layer_start = tf.offset + 512
    add("blobs/sha256/" + "b" * 64, b"x" * 8192)
raw = open(p("layer-full.tar"), "rb").read()
open(p("layer-cut.tar"), "wb").write(raw[:layer_start + 100])

# A complete pinned image tree whose referenced LAYER blob is a directory
# entry rather than a regular file. Its name is under blobs/sha256/, so a
# name-only inventory counts it present and the image reads as complete,
# while `ctr images import` cannot load it.
with tarfile.open(p("blobdir.tar"), "w") as tf:
    def add(name, data):
        ti = tarfile.TarInfo(name); ti.size = len(data)
        tf.addfile(ti, io.BytesIO(data))
    add("index.json", json.dumps({"manifests": [{"digest": pin_digest,
        "annotations": {"io.containerd.image.name": pin_name}}]}).encode())
    add("blobs/sha256/" + pin_digest.split(":", 1)[1], json.dumps({
        "config": {"digest": "sha256:" + "a" * 64},
        "layers": [{"digest": "sha256:" + "b" * 64}]}).encode())
    add("blobs/sha256/" + "a" * 64, b"{}")
    d = tarfile.TarInfo("blobs/sha256/" + "b" * 64); d.type = tarfile.DIRTYPE
    tf.addfile(d)

# JSON scalar/container confusion and unsupported descriptor formats.
def shape_archive(label, name=pin_name, top=None, digest=pin_digest):
    with tarfile.open(p("shape-" + label + ".tar"), "w") as tf:
        def add(name, obj):
            data = json.dumps(obj).encode()
            ti = tarfile.TarInfo(name); ti.size = len(data)
            tf.addfile(ti, io.BytesIO(data))
        add("index.json", {"manifests": [{"digest": digest,
            "annotations": {"io.containerd.image.name": name}}]})
        if top is not None:
            add("blobs/sha256/" + pin_digest.split(":", 1)[1], top)
        # A complete-looking child at the SHA-256 path must not satisfy sha512.
        add("blobs/sha256/" + "c" * 64,
            {"config": {"digest": "sha256:" + "a" * 64}, "layers": []})
        add("blobs/sha256/" + "a" * 64, {})
for label, value in {"list": [], "object": {}, "null": None, "number": 1, "bool": True}.items():
    shape_archive("name-" + label, name=value)
    shape_archive("media-" + label, top={"mediaType": value, "config": {}})
for label, digest in {"sha512": "sha512:" + "c" * 64,
                      "short": "sha256:" + "c" * 32,
                      "long": "sha256:" + "c" * 128,
                      "uppercase": "sha256:" + "C" * 64}.items():
    shape_archive("digest-" + label, digest=digest)
    shape_archive("child-digest-" + label, top={"manifests": [{"digest": digest}]})

PY

# run <args...> -> stdout+stderr in $out, exit status in $rc. verify-payload is
# expected to FAIL on every fixture here; what is asserted is HOW.
out=""; rc=0
run() {
  rc=0
  out=$("$SCRIPT" "$@" 2>&1) || rc=$?
}

no_traceback() {  # the defect this whole file exists for
  grep -q "Traceback (most recent call last)" <<<"$out" \
    && fail "$1: a Python traceback reached the operator:
$out"
  return 0
}

# --- 1. --tar, truncated in the first header ---------------------------------
# tarfile.open() itself raises here.
run --tar "$TEST_TMP/trunc-head.tar"
[[ $rc -ne 0 ]] || fail "a tar truncated in its first header was accepted"
no_traceback "truncated header"
grep -q "truncated or corrupt" <<<"$out" \
  || fail "no diagnostic naming the damage for a truncated tar: '$out'"
grep -q "trunc-head.tar" <<<"$out" \
  || fail "the diagnostic does not name the archive: '$out'"
ok

# --- 2. --tar, truncated mid-member ------------------------------------------
# tarfile.open() SUCCEEDS on this one — the header it reads is intact — and the
# ReadError surfaces later, during the member scan. Guarding only the open call
# leaves this case tracing back, so it is asserted separately.
run --tar "$TEST_TMP/trunc-mid.tar"
[[ $rc -ne 0 ]] || fail "a tar truncated mid-member was accepted"
no_traceback "truncated mid-member"
grep -q "truncated or corrupt" <<<"$out" \
  || fail "no diagnostic naming the damage for a mid-member truncation: '$out'"
ok

# --- 3. packaged binary whose ZIP has lost its central directory -------------
# unzip exits 9 and extracts nothing. With the status discarded the only symptom
# was "no image archive in the payload", which describes a payload built without
# images — a different bug, sending the operator somewhere else entirely.
run "$TEST_TMP/bin-zip-truncated"
[[ $rc -ne 0 ]] || fail "a binary with a truncated ZIP was accepted"
no_traceback "truncated zip"
grep -q "is damaged (unzip exit" <<<"$out" \
  || fail "a truncated ZIP was not reported as a damaged ZIP: '$out'"
grep -q "no image archive in the payload" <<<"$out" \
  && fail "a truncated ZIP was misreported as a payload with no images: '$out'"
ok

# --- 4. packaged binary with a bad CRC ---------------------------------------
# The sharpest case: unzip exits 2 but still WRITES the member, so the emptiness
# check at the extraction site cannot catch it — the corrupt tar sails on into
# Python. The extraction failure has to be judged where it happens.
run "$TEST_TMP/bin-zip-badcrc"
[[ $rc -ne 0 ]] || fail "a binary with a bad-CRC ZIP member was accepted"
no_traceback "bad crc"
grep -q "is damaged (unzip exit" <<<"$out" \
  || fail "a bad CRC was not reported as a damaged ZIP: '$out'"
ok

# --- 5. the benign ELF-prefix warning must stay benign -----------------------
# The counterweight to 3 and 4: every packaged binary produces "extra bytes at
# beginning", because the ZIP is appended to an ELF. Treating unzip's non-zero
# status as failure would reject EVERY payload. This fixture's ZIP is intact and
# its tar is truncated, so a run that reaches the tar diagnostic proves the
# extraction was judged sound and the warning was not mistaken for damage.
run "$TEST_TMP/bin-truncated-tar"
[[ $rc -ne 0 ]] || fail "a binary carrying a truncated tar was accepted"
no_traceback "truncated tar in a sound zip"
grep -q "is damaged (unzip exit" <<<"$out" \
  && fail "the benign ELF-prefix warning was reported as a damaged ZIP: '$out'"
grep -q "truncated or corrupt" <<<"$out" \
  || fail "a truncated tar inside a sound ZIP was not diagnosed: '$out'"
ok

# --- 6. a sound payload still gets through to the real check -----------------
# Without this, 3-5 would all pass for a script that refuses everything. The
# fixture carries an intact ZIP and an intact-but-empty OCI archive, so the run
# must reach the pin comparison and report the pinned images as MISSING.
run "$TEST_TMP/bin-valid"
[[ $rc -ne 0 ]] || fail "an archive carrying none of the pinned images was accepted"
no_traceback "sound payload"
grep -q "is damaged (unzip exit" <<<"$out" \
  && fail "a sound ELF-prefixed ZIP was rejected as damaged: '$out'"
grep -q "truncated or corrupt" <<<"$out" \
  && fail "a sound OCI archive was rejected as corrupt: '$out'"
grep -q "MISSING" <<<"$out" \
  || fail "a sound payload did not reach the pinned-image comparison: '$out'"
ok

# --- 7. index.json present, but not a regular file ---------------------------
# extractfile() returns None for a directory entry, and json.load(None) raises
# AttributeError, which is not in the except tuple.
run "$TEST_TMP/bin-index-dir"
[[ $rc -ne 0 ]] || fail "an archive whose index.json is a directory was accepted"
no_traceback "index.json as a directory"
ok

# --- 8. index.json is valid JSON of the wrong shape --------------------------
# `{}` parses, then index["manifests"] raises KeyError.
run "$TEST_TMP/bin-index-empty"
[[ $rc -ne 0 ]] || fail "an archive whose index.json has no manifests was accepted"
no_traceback "index.json without manifests"
grep -q "no manifests list" <<<"$out" \
  || fail "a shapeless index.json was not diagnosed: '$out'"
ok

# --- 9. a manifests entry that is not a descriptor ---------------------------
# `{"manifests":[{}]}` clears the list check above and then raises KeyError at
# m["digest"]. One level deeper than case 8, same failure mode.
run "$TEST_TMP/bin-bad-descriptor"
[[ $rc -ne 0 ]] || fail "an archive with a digest-less descriptor was accepted"
no_traceback "descriptor without a digest"
grep -q "malformed descriptor" <<<"$out" \
  || fail "a digest-less descriptor was not diagnosed: '$out'"
ok

# --- 10. a descriptor whose digest is not <algorithm>:<hex> ------------------
# Clears the descriptor guard (it IS a non-empty string), then blob_member()
# does digest.split(":", 1)[1] and raises IndexError.
run "$TEST_TMP/bin-bad-digest"
[[ $rc -ne 0 ]] || fail "an archive with a malformed digest was accepted"
no_traceback "digest that is not algorithm:hex"
grep -q "not a supported sha256:" <<<"$out" \
  || fail "a malformed digest was not diagnosed: '$out'"
ok

# --- 11. annotations that are not a mapping ----------------------------------
# `ann or {}` keeps a truthy non-dict, and ann.get(...) then raises
# AttributeError rather than producing a diagnostic.
run "$TEST_TMP/bin-bad-annotations"
[[ $rc -ne 0 ]] || fail "an archive with non-mapping annotations was accepted"
no_traceback "annotations that are not a mapping"
ok

# --- 12. a pinned image whose top manifest blob is not an object -------------
# Parses as JSON, so it is not caught by the index guards; the walk then scans
# it and raises AttributeError/TypeError. Naming a real pin is what makes the
# walk follow it instead of stopping at the MISSING comparison.
run "$TEST_TMP/bin-bad-topmanifest"
[[ $rc -ne 0 ]] || fail "an archive with a non-object top manifest was accepted"
no_traceback "top manifest that is not an object"
grep -q "not an object\|not an image manifest" <<<"$out" \
  || fail "a malformed top manifest was not diagnosed: '$out'"
ok

for fixture in "$TEST_TMP"/child-*.tar "$TEST_TMP"/shape-*.tar "$TEST_TMP/layer-cut.tar"; do
  run --tar "$fixture"
  [[ $rc -ne 0 ]] || fail "damaged fixture accepted: $fixture"
  no_traceback "$fixture"
  grep -q "truncated or corrupt" <<<"$out" || fail "missing corruption diagnosis: $out"
  ok
done
run --tar "$TEST_TMP/layer-full.tar"
no_traceback "intact layer control"
grep -q "truncated or corrupt\|INCOMPLETE" <<<"$out" && fail "intact image tree rejected as damaged: $out"
grep -q "MISSING" <<<"$out" || fail "intact image tree did not reach pin comparison: $out"
ok

# --- a referenced blob that is not a regular file ----------------------------
# The inventory is built from TarInfo, not names: a directory entry parked at a
# layer digest must not satisfy the completeness check just because something
# exists at that path.
run --tar "$TEST_TMP/blobdir.tar"
[[ $rc -ne 0 ]] || fail "an image whose layer blob is a directory was accepted"
no_traceback "layer blob present as a directory"
grep -q "INCOMPLETE\|missing blobs" <<<"$out" \
  || fail "a non-regular layer blob was not reported as missing: '$out'"
ok

# --- the dashboard image ------------------------------------------------------
# It is built, not pinned, so it is checked on its own: by name and by the
# digests inside it.
run --dash-tar "$TEST_TMP/dash-valid.tar" ghcr.io/ctrliq/stratum/stratum-dash:v0.0.0
[[ $rc -eq 0 ]] || fail "a valid dashboard image was rejected: '$out'"
ok
run --dash-tar "$TEST_TMP/dash-valid.tar"
[[ $rc -eq 0 ]] || fail "a valid dashboard image under the registry was rejected without an exact ref: '$out'"
ok
run --dash-tar "$TEST_TMP/dash-valid.tar" ghcr.io/ctrliq/stratum/stratum-dash:v9.9.9
[[ $rc -ne 0 ]] || fail "a dashboard image with another tag was accepted"
grep -q "is named" <<<"$out" || fail "a wrong dashboard tag was not diagnosed: '$out'"
ok
run --dash-tar "$TEST_TMP/dash-wrongname.tar"
[[ $rc -ne 0 ]] || fail "a dashboard archive naming another image was accepted"
grep -q "is named" <<<"$out" || fail "a foreign image name was not diagnosed: '$out'"
ok
run --dash-tar "$TEST_TMP/dash-nolayer.tar"
[[ $rc -ne 0 ]] || fail "a dashboard image without its layer was accepted"
grep -q "is missing" <<<"$out" || fail "a missing dashboard layer was not diagnosed: '$out'"
ok
run --dash-tar "$TEST_TMP/dash-tampered.tar"
[[ $rc -ne 0 ]] || fail "a dashboard image whose layer does not match its digest was accepted"
grep -q "does not hash" <<<"$out" || fail "a tampered dashboard layer was not diagnosed: '$out'"
ok
run --dash-tar "$TEST_TMP/dash-badconfig.tar"
[[ $rc -ne 0 ]] || fail "a dashboard image whose config is not JSON was accepted"
no_traceback "dashboard config that is not JSON"
grep -q "malformed JSON\|not an image config" <<<"$out" || fail "a non-JSON dashboard config was not diagnosed: '$out'"
ok
run --dash-tar "$TEST_TMP/dash-trunc.tar"
[[ $rc -ne 0 ]] || fail "a truncated dashboard archive was accepted"
no_traceback "truncated dashboard archive"
grep -q "truncated or corrupt\|is missing\|malformed\|exactly one image" <<<"$out" \
  || fail "a truncated dashboard archive was not diagnosed: '$out'"
ok
run "$TEST_TMP/bin-no-dash"
[[ $rc -ne 0 ]] || fail "a payload without the dashboard image was accepted"
grep -q "no dashboard image archive" <<<"$out" || fail "a payload missing the dashboard image was not diagnosed: '$out'"
ok
run "$TEST_TMP/bin-bad-dash"
[[ $rc -ne 0 ]] || fail "a payload with a damaged dashboard image was accepted"
grep -q "does not hash" <<<"$out" || fail "a payload's damaged dashboard image was not diagnosed: '$out'"
ok

echo "test-verify-payload: ok ($pass checks)"
