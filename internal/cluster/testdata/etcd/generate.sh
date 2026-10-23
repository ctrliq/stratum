#!/usr/bin/env bash
# Regenerates the etcd backend fixtures in this directory with the etcd image
# pinned in build/images.txt. See README.md.
#
# Usage: internal/cluster/testdata/etcd/generate.sh
set -euo pipefail
cd "$(dirname "$0")"

IMAGE=$(grep -E '^registry\.k8s\.io/etcd:' ../../../../build/images.txt)
BUNDLE=/registry/secrets/kube-system/stratum-certs
OTHER=/registry/secrets/kube-system/other
CTR=

cleanup() { [[ -z $CTR ]] || docker rm -f "$CTR" >/dev/null; }
trap cleanup EXIT

ctl() { docker exec "$CTR" etcdctl "$@"; }

fail() {
	echo "$1:" >&2
	docker logs "$CTR" >&2
	exit 1
}

healthy() {
	local out end=$((SECONDS + 30))
	while ((SECONDS < end)); do
		out=$(ctl --dial-timeout=1s --command-timeout=2s endpoint health 2>&1) && return
		[[ $(docker inspect -f '{{.State.Running}}' "$CTR") == true ]] || fail "etcd exited during startup"
		sleep 1
	done
	fail "etcd was not healthy after 30s; the last health check said: $out"
}

fixture() {
	local name=$1
	shift
	CTR=$(docker create "$IMAGE" etcd --name cp1 --data-dir /data \
		--listen-client-urls http://127.0.0.1:2379 --advertise-client-urls http://127.0.0.1:2379 \
		--listen-peer-urls http://127.0.0.1:2380 --initial-advertise-peer-urls http://127.0.0.1:2380 \
		--initial-cluster cp1=http://127.0.0.1:2380)
	docker start "$CTR" >/dev/null
	healthy
	ctl put "$OTHER" dummy-other >/dev/null
	"$@"
	ctl defrag >/dev/null
	docker stop "$CTR" >/dev/null
	docker cp "$CTR:/data/member/snap/db" "$name"
	cleanup
	CTR=
	echo "wrote $name"
}

publish() { ctl put "$BUNDLE" "$1" >/dev/null; }
unpublish() { ctl del "$BUNDLE" >/dev/null; }

published() { publish dummy-1; }
deleted() { publish dummy-1 && unpublish; }
recreated() { publish dummy-1 && unpublish && publish dummy-2; }

learners() {
	local id
	id=$(ctl member add cp2 --learner --peer-urls=http://127.0.0.1:2381 |
		sed -n 's/^Member \([0-9a-f]*\) added.*/\1/p')
	[[ -n $id ]] || { echo "member add cp2 printed no member ID" >&2; exit 1; }
	ctl member remove "$id" >/dev/null
	ctl member add cp3 --learner --peer-urls=http://127.0.0.1:2382 >/dev/null
}

fixture published.db published
fixture deleted.db deleted
fixture recreated.db recreated
fixture members.db learners
