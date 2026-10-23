# etcd backend fixtures

`localEtcdJoinState` reads etcd's bbolt backend directly when etcd is stopped:
the `members` bucket and the MVCC `key` bucket (`internal/cluster/ciskey.go`).
That is etcd's on-disk layout, not an API, so these databases were written by
the etcd image the cluster runs (`registry.k8s.io/etcd`, pinned in
`build/images.txt`), and `TestLocalEtcdJoinStateOnRealEtcdDatabases` reads
them. `TestRealEtcdDatabasesAreFromThePinnedEtcd` fails when that image moves
to a minor version the fixtures were not written by, so a minor etcd bump
regenerates them, and a layout change in the new etcd shows up as a failing
test instead of as a gate that silently misreads every cluster. A
patch bump keeps the same storage version and needs no new fixtures.

Each is defragmented and not compacted, so the revision history the reader
walks is all there. The values are dummy strings, not Secrets: nothing in them
is key material.

| File | Members | `/registry/secrets/kube-system/stratum-certs` |
| --- | --- | --- |
| `published.db` | 1 | put once: published |
| `deleted.db` | 1 | put, then deleted: a tombstone is its latest revision |
| `recreated.db` | 1 | put, deleted, put again: published |
| `members.db` | 2: one learner, after another was added and removed | never put |

Each also holds `/registry/secrets/kube-system/other`, a key that is not the
bundle.

To regenerate all four, with Docker running, from the repository root:

```bash
internal/cluster/testdata/etcd/generate.sh
```
