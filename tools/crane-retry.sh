#!/usr/bin/env bash
# crane_retry, sourced by mirror-images.sh and build-image-archive.sh: one retry
# policy for the crane calls on the release path that must succeed against a
# registry nobody here controls (the mirror-first fast path in
# build-image-archive.sh deliberately is not retried). It intentionally has no
# setup or side effects of its own.
#
# crane already retries a dropped connection or a 5xx itself (three tries, about
# 4s). This is the longer window on top of that: three more attempts with 5s and
# 10s backoff, so a registry hiccup does not fail a release gate. A permanent
# answer is never retried: a 429 spends a shared rate-limit budget three times
# to fail anyway, and auth / denied / not-found will not change in 15s. crane
# upper-cases registry error codes (DENIED, NAME_UNKNOWN), so the match is
# case-insensitive.
#
# Only stderr is captured, so stdout (a digest, a manifest) still flows to the
# caller. crane's own progress and warnings are passed through on success too.
# A caller's stdout redirect spans every attempt, so use it only with
# subcommands that print nothing until they succeed (copy, digest, manifest,
# pull).
crane_retry() {  # crane_retry <crane args...>
  local n=1 err rc
  while :; do
    rc=0; { err=$(crane "$@" 2>&1 >&3); } 3>&1 || rc=$?
    [ -n "$err" ] && echo "$err" >&2
    [ $rc -eq 0 ] && return 0
    case "$(printf '%s' "$err" | tr '[:upper:]' '[:lower:]')" in
      *toomanyrequests*|*"too many requests"*|*unauthorized*|*denied*|*forbidden*|*manifest_unknown*|*name_unknown*|*name_invalid*|*"not found"*)
        echo "crane $1: not retrying, this is not transient: ${*:2}" >&2; return 1 ;;
    esac
    [ $n -ge 3 ] && { echo "crane $1: failed after 3 attempts: ${*:2}" >&2; return 1; }
    echo "crane $1: attempt $n/3 failed, retrying in $((n * 5))s: ${*:2}" >&2
    sleep $((n * 5)); n=$((n + 1))
  done
}
