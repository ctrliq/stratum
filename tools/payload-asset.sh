#!/usr/bin/env bash
# Writes the ZIP payload appended to a packaged stratum binary out as its own
# file, byte for byte, so an importer of pkg/cli can append the same payload to
# its binary.
#
# Usage: tools/payload-asset.sh <packaged-binary> <out.zip>
set -euo pipefail

[ $# -eq 2 ] || { echo "usage: $0 <packaged-binary> <out.zip>" >&2; exit 2; }
[ -f "$1" ] || { echo "payload-asset: no such binary: $1 (run 'make package' first)" >&2; exit 1; }
python3 -I - "$1" "$2" <<'PY'
import os, sys, zipfile

src, out = sys.argv[1], sys.argv[2]
try:
    zf = zipfile.ZipFile(src)
except zipfile.BadZipFile:
    sys.exit(f"payload-asset: {src} carries no payload (not packaged, an EMBEDDED_BINS=none build, or a darwin build)")
infos = zf.infolist()
if not infos:
    sys.exit(f"payload-asset: {src}'s payload is empty")
# header_offset is absolute in the file once zipfile has accounted for the
# executable in front, and `make package` writes the archive with no prefix of
# its own, so the first local header is where the payload starts.
start = min(i.header_offset for i in infos)
size = os.path.getsize(src)
with open(src, "rb") as f:
    f.seek(start)
    if f.read(4) != b"PK\x03\x04":
        sys.exit(f"payload-asset: {src}: no local file header at the payload start {start}")
    f.seek(start)
    tmp = out + ".tmp"
    with open(tmp, "wb") as w:
        while chunk := f.read(1 << 20):
            w.write(chunk)
os.replace(tmp, out)
with zipfile.ZipFile(out) as carved:
    if sorted(carved.namelist()) != sorted(zf.namelist()):
        sys.exit(f"payload-asset: {out} does not list the same members as {src}'s payload")
    bad = carved.testzip()
    if bad:
        sys.exit(f"payload-asset: {out}: member {bad} fails its CRC")
print(f"{out}: {len(infos)} members, {size - start} bytes from offset {start}")
PY
