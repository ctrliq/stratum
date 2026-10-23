#!/usr/bin/env bash
# Guards payload-asset.sh: the carved zip is exactly the bytes `make package`
# appended, and a binary with no payload is refused rather than carved empty.
set -euo pipefail
cd "$(dirname "$0")/.."

SCRIPT=$PWD/tools/payload-asset.sh
fail() { echo "test-payload-asset: FAIL  $*" >&2; exit 1; }

TEST_TMP=$(mktemp -d)
trap 'rm -rf -- "$TEST_TMP"' EXIT

python3 -I - "$TEST_TMP" <<'PY'
import os, sys, zipfile
d = sys.argv[1]
os.makedirs(f"{d}/stage/bin"); os.makedirs(f"{d}/stage/images")
with open(f"{d}/stage/bin/kubelet", "wb") as f: f.write(os.urandom(70000))
with open(f"{d}/stage/images/stratum-images.tar", "wb") as f: f.write(os.urandom(90000))
with zipfile.ZipFile(f"{d}/payload.zip", "w") as z:
    z.write(f"{d}/stage/bin/kubelet", "bin/kubelet", compress_type=zipfile.ZIP_DEFLATED)
    z.write(f"{d}/stage/images/stratum-images.tar", "images/stratum-images.tar", compress_type=zipfile.ZIP_STORED)
# An executable that itself contains a stray local-header signature, ahead of the real payload.
with open(f"{d}/elf", "wb") as f: f.write(b"\x7fELF" + os.urandom(5000) + b"PK\x03\x04" + os.urandom(3000))
PY
cat "$TEST_TMP/elf" "$TEST_TMP/payload.zip" > "$TEST_TMP/stratum"

"$SCRIPT" "$TEST_TMP/stratum" "$TEST_TMP/carved.zip" >/dev/null || fail "a packaged binary was not carved"
cmp -s "$TEST_TMP/payload.zip" "$TEST_TMP/carved.zip" || fail "the carved zip is not the appended payload, byte for byte"

if out=$("$SCRIPT" "$TEST_TMP/elf" "$TEST_TMP/none.zip" 2>&1); then
  fail "a binary with no payload was carved: $out"
fi
grep -q 'carries no payload' <<<"$out" || fail "the no-payload refusal does not say so: $out"
[ ! -e "$TEST_TMP/none.zip" ] || fail "a refused carve left a file behind"

if out=$("$SCRIPT" "$TEST_TMP/missing" "$TEST_TMP/missing.zip" 2>&1); then
  fail "a missing binary was carved: $out"
fi
grep -q "no such binary" <<<"$out" || fail "a missing binary is not named as missing (a traceback?): $out"

echo "test-payload-asset: ok"
