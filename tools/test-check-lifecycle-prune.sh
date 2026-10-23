#!/usr/bin/env bash
# Exercise the lifecycle check's pruning sequence without booting a node.
# The boundary model follows the operator's desired-generation gate: deleting
# a bundled file alone cannot authorize pruning. Only a config-driven restart
# records the removal. The real gate is covered by the operator's Go tests.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "test-check-lifecycle-prune: FAIL  $*" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
ROOT=$tmp/node
mkdir -p "$ROOT/manifests/local-path"
printf 'staged local-path bundle\n' >"$ROOT/manifests/local-path/local-path.yaml"
touch "$ROOT/storage-class"
cat >"$tmp/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ ! -f $ROOT/api-unavailable ]] || exit 1
case "$*" in
  "get storageclass local-path") [[ -f $ROOT/storage-class ]] ;;
  "get storageclass local-path --ignore-not-found -o name")
    if [[ -f $ROOT/storage-class ]]; then echo storageclass.storage.k8s.io/local-path; fi ;;
  *) exit 2 ;;
esac
SH
chmod +x "$tmp/kubectl"
export ROOT
export K="$tmp/kubectl"
running=1
desired=local-path
staged=0

stop_cluster() { running=0; }
start_cluster() {
  [[ $running == 0 ]] || fail "restarted without stopping the old supervisor"
  [[ $# == 2 && $1 == --config && -s $2 ]] || fail "restart must read the changed config"
  grep -qE '^[[:space:]]+default: none$' "$2" || fail "storage remains enabled in config"
  # stratum up stages the config's set and publishes it atomically. The
  # deployer can now prune; its record still owns the removed stack.
  rm -f "$ROOT/manifests/local-path/local-path.yaml"
  desired=none
  staged=$((staged + 1))
  running=1
}
wait_for() {
  local what=$1
  shift 2
  # A matching removal prunes. A hand-deleted bundle with unchanged desired
  # intent does nothing, exactly as the failed CI job's digest diagnostic says.
  if [[ $running == 1 && $desired == none && ! -f $ROOT/manifests/local-path/local-path.yaml ]]; then
    rm -f "$ROOT/storage-class"
  fi
  "$@" || fail "$what (desired=$desired)"
}
pass() { :; }
done_testing() {
  [[ $staged == 1 && ! -f $ROOT/storage-class ]] || fail "no recorded storage removal was tested"
}

# Run the actual section, not a copy that could stay green while CI drifts.
section=$(sed -n '/^# --- manifest deployer prune\./,/^done_testing$/p' tools/inttest/check-lifecycle.sh)
[[ $section == *done_testing* ]] || fail "cannot find the lifecycle pruning section"
eval "$section"
# An unavailable API must not be mistaken for successful pruning.
touch "$ROOT/api-unavailable"
if local_path_pruned; then fail "an API error was reported as successful pruning"; fi
echo "test-check-lifecycle-prune: ok"
