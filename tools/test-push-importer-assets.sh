#!/usr/bin/env bash
# Guards push-importer-assets.sh with a crane shim on PATH: each asset goes to
# its own ref as one layer holding exactly the asset and its .sig/.pem, and a
# missing signature is refused before anything is pushed.
set -euo pipefail
cd "$(dirname "$0")/.."

SCRIPT=$PWD/tools/push-importer-assets.sh
fail() { echo "test-push-importer-assets: FAIL  $*" >&2; exit 1; }

TEST_TMP=$(mktemp -d)
trap 'rm -rf -- "$TEST_TMP"' EXIT
mkdir -p "$TEST_TMP/bin" "$TEST_TMP/dist" "$TEST_TMP/pushed"
cat >"$TEST_TMP/bin/crane" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = append ] && [ "$2" = -f ] && [ "$3" = - ] && [ "$4" = -t ] || { echo "crane shim: unexpected $*" >&2; exit 1; }
ref=$5
cat >"$CRANE_SHIM_DIR/${ref//[\/:]/_}.tar"
echo "$ref" >>"$CRANE_SHIM_DIR/refs"
SH
chmod +x "$TEST_TMP/bin/crane"
export PATH="$TEST_TMP/bin:$PATH" CRANE_SHIM_DIR="$TEST_TMP/pushed"

for f in stratum-ldflags-linux-arm64.txt stratum-payload-linux-arm64.zip; do
  for s in "" .sig .pem; do printf '%s\n' "$f$s" >"$TEST_TMP/dist/$f$s"; done
done
echo stray >"$TEST_TMP/dist/stratum-linux-arm64"

"$SCRIPT" "$TEST_TMP/dist" reg.example/stratum:v9-rc.1 arm64 >/dev/null || fail "a complete dist was not pushed"
want=$'reg.example/stratum:v9-rc.1-ldflags-linux-arm64\nreg.example/stratum:v9-rc.1-payload-linux-arm64'
[ "$(cat "$TEST_TMP/pushed/refs")" = "$want" ] || fail "pushed refs: $(cat "$TEST_TMP/pushed/refs")"
for kind in ldflags:txt payload:zip; do
  name=${kind%%:*} file=stratum-${kind%%:*}-linux-arm64.${kind#*:}
  got=$(tar -tf "$TEST_TMP/pushed/reg.example_stratum_v9-rc.1-$name-linux-arm64.tar" | sort | paste -sd' ' -)
  [ "$got" = "$file $file.pem $file.sig" ] || fail "the $name layer holds '$got'"
  tar -xOf "$TEST_TMP/pushed/reg.example_stratum_v9-rc.1-$name-linux-arm64.tar" "$file.sig" | grep -qx "$file.sig" ||
    fail "the $name layer's signature is not the dist file's"
done

rm "$TEST_TMP/pushed/refs" "$TEST_TMP/dist/stratum-payload-linux-arm64.zip.sig"
if out=$("$SCRIPT" "$TEST_TMP/dist" reg.example/stratum:v9-rc.1 arm64 2>&1); then
  fail "a payload without its signature was pushed"
fi
grep -q 'stratum-payload-linux-arm64.zip.sig is missing' <<<"$out" || fail "the refusal does not name the missing file: $out"
[ ! -e "$TEST_TMP/pushed/refs" ] || fail "a set was pushed before the refusal: $(cat "$TEST_TMP/pushed/refs")"

echo "test-push-importer-assets: ok"
