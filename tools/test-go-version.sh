#!/usr/bin/env bash
# Asserts tools/lint-go-version.sh catches a go_version that has left the
# pinned Kubernetes tag's .go-version.
#
# Sibling of tools/test-revisions.sh, but hermetic: the gate's only upstream
# read is one small file over HTTP, so a local python3 stub answers it
# (STRATUM_K8S_RAW_BASE) with each status the gate has to tell apart, through
# the gate's own curl call. The versions are made up, which keeps this file
# off lint-versions.sh's radar. Each kubernetes_version is a route in the stub.
set -euo pipefail
cd "$(dirname "$0")/.."

WORK=$(mktemp -d)
stub=""
trap '[[ -z $stub ]] || kill "$stub" 2>/dev/null; rm -rf "$WORK"' EXIT
fail=0

python3 - "$WORK/port" <<'EOF' &
import http.server, os, sys
ROUTES = {  # tag: (status, body)
    "v9.1.2": (200, b"8.3.4\n"),
    "v9.1.3": (200, b"8.3.4\r\n"),  # CRLF: still just the version
    "v9.2.0": (200, b""),
    "v9.2.1": (200, b"8.3 .4\n"),  # must not collapse to 8.3.4
    "v9.3.1": (301, b""),
    "v9.4.3": (403, b""),
    "v9.4.29": (429, b""),
    "v9.5.3": (503, b""),
}
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        status, body = ROUTES.get(self.path.split("/")[1], (404, b""))
        self.send_response(status)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *args):
        pass
srv = http.server.HTTPServer(("127.0.0.1", 0), H)
# Written then renamed, so the shell never reads a half-written port.
open(sys.argv[1] + ".tmp", "w").write(str(srv.server_port))
os.rename(sys.argv[1] + ".tmp", sys.argv[1])
srv.serve_forever()
EOF
stub=$!
disown  # killed on exit; no "Terminated" job notice in the output
for _ in $(seq 50); do [[ -s $WORK/port ]] && break; sleep 0.1; done
[[ -s $WORK/port ]] || { echo "test-go-version: stub server did not start" >&2; exit 1; }
STRATUM_K8S_RAW_BASE="http://127.0.0.1:$(cat "$WORK/port")"
export STRATUM_K8S_RAW_BASE

pins() { printf 'go_version = %s\nkubernetes_version = %s\n' "$2" "$3" > "$WORK/$1"; }

expect() {  # description, want_exit, fixture, [text the output must contain]
  local what=$1 want=$2 file=$3 needle=${4:-} got=0
  # Any exit 1 or 3 would do otherwise, including the gate not running at all.
  [[ -n $needle || $want -ne 1 ]] || needle="lint-go-version: ERROR"
  [[ -n $needle || $want -ne 3 ]] || needle="lint-go-version: INCONCLUSIVE"
  tools/lint-go-version.sh "$WORK/$file" >"$WORK/out" 2>&1 || got=$?
  if [[ $got -ne $want ]]; then
    printf '  FAIL  %s: want exit %s, got %s\n' "$what" "$want" "$got" >&2
    sed 's/^/        /' "$WORK/out" >&2
    fail=1
  elif [[ -n $needle ]] && ! grep -qF -- "$needle" "$WORK/out"; then
    printf '  FAIL  %s: output does not mention %s\n' "$what" "$needle" >&2
    sed 's/^/        /' "$WORK/out" >&2
    fail=1
  else
    printf '  ok    %s\n' "$what"
  fi
}

pins agree     8.3.4 9.1.2
pins crlf      8.3.4 9.1.3
# The drift bug: Renovate moved Go on its own, one minor past upstream.
pins drift     8.4.1 9.1.2
# A patch behind is drift too: the rule is equality, not "at least".
pins behind    8.3.3 9.1.2
# CI's `pins` steps would hand setup-go "8.3.4 # note", so this must not pass.
pins comment   "8.3.4 # note" 9.1.2
pins k8s-note  8.3.4 "9.1.2 # held back"
pins no-tag    8.3.4 9.9.9
pins empty     8.3.4 9.2.0
pins spaced    8.3.4 9.2.1
pins moved     8.3.4 9.3.1
pins denied    8.3.4 9.4.3
pins limited   8.3.4 9.4.29
pins down      8.3.4 9.5.3
printf 'kubernetes_version = 9.1.2\n' > "$WORK/no-go"
printf 'go_version = 8.3.4\n'         > "$WORK/no-k8s"

expect "matching pins pass"                   0 agree
expect "a CRLF .go-version still matches"     0 crlf
expect "go ahead of upstream fails"           1 drift   "'8.4.1'"
expect "the failure names upstream's Go too"  1 drift   "'8.3.4'"
expect "go behind upstream fails"             1 behind
expect "a trailing comment fails"             1 comment   "not a plain version"
expect "a comment on the k8s pin fails"       1 k8s-note  "not a plain version"
expect "a missing go_version fails"           1 no-go   "has no go_version"
expect "a missing kubernetes_version fails"   1 no-k8s  "has no kubernetes_version"
expect "a tag with no .go-version fails"      1 no-tag  "HTTP 404"
expect "an empty .go-version fails"           1 empty   "is empty"
expect "a malformed .go-version fails"       1 spaced  "not a plain version"
expect "a redirect fails, not warns"          1 moved   "HTTP 301"
expect "a denial fails, not warns"            1 denied  "HTTP 403"
expect "a rate limit is inconclusive"         3 limited
expect "a 5xx is inconclusive"                3 down

# Nothing listening at all: curl itself fails.
STRATUM_K8S_RAW_BASE=http://127.0.0.1:1 expect "unreachable upstream is inconclusive" 3 agree

if [[ $fail -ne 0 ]]; then
  echo "test-go-version: FAILED" >&2
  exit 1
fi
echo "test-go-version: ok"
