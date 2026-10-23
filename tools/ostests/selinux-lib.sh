#!/usr/bin/env bash
# Shared by check-selinux.sh and check-selinux-lifecycle.sh. Sourced after a
# backend, for backend_ssh.

# DENIALS matches an audit record that names a container, kubelet or Stratum
# type or path.
DENIALS='scontext=[^ ]*:(container_[a-z_]*|spc_t|kubelet_t)|tcontext=[^ ]*:(container_[a-z_]*|kubelet_[a-z_]*)|stratum'

# selinux_denials <handle>: the node's AVC, USER_AVC and SELINUX_ERR records
# since boot that match DENIALS, one per line; nothing when there are none.
selinux_denials() {
  backend_ssh "$1" "ausearch -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -E 'type=(AVC|USER_AVC|SELINUX_ERR)' | grep -E $(printf '%q' "$DENIALS") || true" </dev/null
}
