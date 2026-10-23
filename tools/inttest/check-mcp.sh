#!/usr/bin/env bash
# The MCP server, run against a REAL Stratum node.

. "$(dirname "$0")/harness.sh"

start_cluster
trap finish EXIT

# start_cluster gates on kubelet having started, not on the node being
# Ready — the CNI arrives later, via the manifest deployer. node_list
# reports readiness faithfully, so asserting on it before the CNI lands
# tests the harness's patience rather than the tool.
wait_for "node Ready (CNI deployed)" 180 sh -c \
  "$K get nodes --no-headers 2>/dev/null | grep -qw Ready"
pass "node is Ready"

# The driver speaks JSON-RPC over stdio to `stratum mcp` and prints one line
# per result.
mcp_call() {  # $1 = flags (may be empty), rest = tool:jsonargs pairs
  local flags="$1"; shift
  STRATUM="$STRATUM" MCP_FLAGS="$flags" python3 - "$@" <<'PY'
import json, os, subprocess, sys, threading, queue

flags = [f for f in os.environ.get("MCP_FLAGS", "").split() if f]
proc = subprocess.Popen([os.environ["STRATUM"], "mcp"] + flags,
                        stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                        stderr=subprocess.PIPE, text=True, bufsize=1)

lines = queue.Queue()
threading.Thread(target=lambda: [lines.put(l) for l in proc.stdout], daemon=True).start()

# Drain stderr too. The server logs there by design, and an unread pipe
# fills at 64 KiB and blocks the write -- which stalls stdout as well, so
# the reader below times out on a server that is working fine. Kept for
# the failure paths, which print it.
errlines = []
threading.Thread(target=lambda: [errlines.append(l) for l in proc.stderr], daemon=True).start()

def stderr_tail(n=15):
    return "".join(errlines[-n:]) or "(nothing on stderr)\n"

def send(obj):
    proc.stdin.write(json.dumps(obj) + "\n")
    proc.stdin.flush()

def recv(what):
    try:
        raw = lines.get(timeout=120)
    except queue.Empty:
        print(f"TIMEOUT {what}", file=sys.stderr)
        print("--- server stderr ---", file=sys.stderr)
        print(stderr_tail(), file=sys.stderr, end="")
        sys.exit(1)
    # Every line on stdout must be a JSON-RPC message. Anything else is a
    # subsystem printing past the guard, which corrupts the stream.
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        print(f"NON-JSON ON STDOUT: {raw!r}", file=sys.stderr)
        print("--- server stderr ---", file=sys.stderr)
        print(stderr_tail(), file=sys.stderr, end="")
        sys.exit(2)

send({"jsonrpc": "2.0", "id": 1, "method": "initialize",
      "params": {"protocolVersion": "2025-06-18", "capabilities": {},
                 "clientInfo": {"name": "inttest", "version": "0"}}})
recv("initialize")
send({"jsonrpc": "2.0", "method": "notifications/initialized"})

send({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
tools = sorted(t["name"] for t in recv("tools/list")["result"]["tools"])
print("TOOLS " + " ".join(tools))

for i, spec in enumerate(sys.argv[1:], start=3):
    name, _, raw = spec.partition(":")
    args = json.loads(raw) if raw else {}
    send({"jsonrpc": "2.0", "id": i, "method": "tools/call",
          "params": {"name": name, "arguments": args}})
    reply = recv(name)
    result = reply.get("result", {})
    if "error" in reply or result.get("isError"):
        detail = reply.get("error", {}).get("message") or json.dumps(result.get("content", ""))
        print(f"ERR {name} {detail[:300]}")
    else:
        print(f"OK {name} {json.dumps(result.get('structuredContent'))}")

proc.stdin.close()
proc.wait(timeout=30)
PY
}

# ---------------------------------------------------------------- read tier

out=$(mcp_call "" \
  'cluster_status' \
  'certs_check' \
  'node_list' \
  'namespace_list' \
  'uninstall_preview' \
  'version_get')

tools=$(printf '%s\n' "$out" | sed -n 's/^TOOLS //p')
assert_contains "cluster_status is advertised" "cluster_status" "$tools"
assert_contains "certs_check is advertised" "certs_check" "$tools"
pass "read tier advertises $(printf '%s' "$tools" | wc -w | tr -d ' ') tools"

# The whole point of this check: these answer with real data here, where
# off a node they can only answer "not a Stratum node".
for tool in cluster_status certs_check node_list namespace_list uninstall_preview version_get; do
  line=$(printf '%s\n' "$out" | grep "^OK $tool " || true)
  if [[ -z $line ]]; then
    fail "$tool did not succeed on a real node: $(printf '%s\n' "$out" | grep "^ERR $tool " || echo '(no reply)')"
    continue
  fi
  pass "$tool answered"
done

# cluster_status must see the supervisor, not merely fail politely. This is
# the control-socket path, which has no other test anywhere.
status=$(printf '%s\n' "$out" | sed -n 's/^OK cluster_status //p')
supervisor=$(printf '%s' "$status" | python3 -c 'import json,sys; print(json.load(sys.stdin)["supervisor"])')
assert_eq "supervisor reachable over the control socket" "running" "$supervisor"

procs=$(printf '%s' "$status" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(" ".join(p["name"] for p in d.get("procs") or []))')
assert_contains "the supervised process table includes kubelet" "kubelet" "$procs"
pass "supervised processes: $procs"

# certs_check must find the PKI this cluster generated at boot, and nothing
# should be expired on a cluster minutes old.
certs=$(printf '%s\n' "$out" | sed -n 's/^OK certs_check //p')
printf '%s' "$certs" | python3 -c '
import json,sys
d = json.load(sys.stdin)
items = d.get("items") or []
assert items, "certs_check returned no certificates on a running node"
expired = d["expired"]
assert expired == 0, "%d certificates already expired on a fresh cluster" % expired
names = [i["name"] for i in items]
assert any("apiserver" in n for n in names), "no apiserver certificate among %s" % names
print("  ok    certs_check found %d certificates, none expired" % len(items))
'

# node_list is the generic half reading the same cluster through the API.
printf '%s\n' "$out" | sed -n 's/^OK node_list //p' | python3 -c '
import json,sys
d = json.load(sys.stdin)
items = d.get("items") or []
assert len(items) == 1, "expected this one node, got %d" % len(items)
n = items[0]
assert n["ready"] == "True", "node not Ready: %s" % n
print("  ok    node_list reports %s Ready, kubelet %s" % (n["name"], n.get("kubeletVersion")))
'

# uninstall_preview must see the real install, and must NOT have removed it.
printf '%s\n' "$out" | sed -n 's/^OK uninstall_preview //p' | python3 -c '
import json,sys
d = json.load(sys.stdin)
assert d["present"] > 0, "uninstall_preview found none of its targets on an installed node"
print("  ok    uninstall_preview found %d/%d targets present" % (d["present"], len(d["targets"])))
'
assert_eq "uninstall_preview changed nothing" "ok" "$($K get --raw /healthz)"

# ------------------------------------------------------------- the tier gate
for tool in snapshot_save token_create resource_apply uninstall upgrade_apply rollback; do
  if printf '%s' "$tools" | tr ' ' '\n' | grep -qx "$tool"; then
    fail "$tool is advertised at the read tier"
  fi
done
pass "no write or destructive tool is advertised at the read tier"

write_tools=$(mcp_call "--allow-write" | sed -n 's/^TOOLS //p')
assert_contains "--allow-write adds snapshot_save" "snapshot_save" "$write_tools"
assert_contains "--allow-write adds token_create" "token_create" "$write_tools"
for tool in uninstall upgrade_apply rollback snapshot_restore; do
  if printf '%s' "$write_tools" | tr ' ' '\n' | grep -qx "$tool"; then
    fail "$tool is advertised with only --allow-write"
  fi
done
pass "--allow-write does not reach the destructive tier"

destructive_tools=$(mcp_call "--allow-destructive" | sed -n 's/^TOOLS //p')
assert_contains "--allow-destructive adds uninstall" "uninstall" "$destructive_tools"
assert_contains "--allow-destructive adds upgrade_apply" "upgrade_apply" "$destructive_tools"

# --------------------------------------------------- stdout stays uncorrupted
save=$(mcp_call "--allow-write" 'snapshot_save:{"skip_s3":true}')
if printf '%s\n' "$save" | grep -q '^OK snapshot_save '; then
  pass "snapshot_save completed with the JSON-RPC stream intact"
  printf '%s\n' "$save" | sed -n 's/^OK snapshot_save //p' | python3 -c '
import json,sys
d = json.load(sys.stdin)
assert d["path"], "snapshot_save reported no path"
print("  ok    snapshot wrote %s" % d["path"])
'
else
  # A snapshot can legitimately fail in a constrained container; what must NOT
  # happen is stream corruption, and the driver would have exited 2 for that.
  echo "  note  snapshot_save did not complete here: $(printf '%s\n' "$save" | grep '^ERR ' | head -1)"
  pass "snapshot_save left the JSON-RPC stream intact"
fi

done_testing
