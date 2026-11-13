---
description: "The backup-evidence interface: one bounded read of the effective snapshot policy, the local artifacts, the remote copies and the maintenance diagnostics."
---

# Backup Evidence

This page documents the backup-evidence interface: Stratum's read-only view of
what it does for backups, served as one bounded read. You need it when you
integrate with Stratum from outside — Stratum PRO, a collector, a script — and
must read the engine's own evidence instead of re-deriving its configuration.
Taking and restoring snapshots is covered on
[Snapshot & Restore](snapshot-restore.md); the settings and file rules behind
the values reported here are in the
[Snapshot Reference](../appendices/snapshot-reference.md).

## One read, three surfaces

Every surface serves the same document from one bounded read, so the fields
cannot drift between them. The engine resolves the policy itself
(internal/backupevidence/policy.go); a consumer never re-derives which
config file won.

| Surface | How you call it | Where it is defined |
|---|---|---|
| CLI | `stratum snapshot list -o json`, as root on a node | pkg/cli/snapshot.go |
| Go | `cli.BackupEvidence(ctx, opts...)` in package `pkg/cli` | pkg/cli/evidence.go |
| MCP | the `backup_evidence` tool, read tier, no caller-supplied Kubernetes credential | internal/mcp/catalog.go |

```console
# /usr/local/bin/stratum snapshot list -o json
```

The output is one JSON object. This excerpt shows its shape; the
[fixtures](#fixtures) hold complete documents:

```json
{
  "policy": {
    "state": "configured",
    "intervalSeconds": 86400,
    "intervalFrom": "cluster-config",
    "retain": 3,
    "retainFrom": "snapshot-conf",
    "copy": "remote-required",
    "copyFrom": "snapshot-conf",
    "dir": "/var/backups/etcd",
    "dirFrom": "default",
    "warnings": []
  },
  "node": "cp-1",
  "nodeUID": "5e4f9a20-4af6-4d15-8c72-62b9c92223f3",
  "local": { "node": "cp-1", "artifacts": [], "complete": true, "warnings": [] },
  "remote": { "copies": [], "old": [], "complete": true, "warnings": [] },
  "backups": { "node": "cp-1", "entries": [], "complete": true, "warnings": [], "retention": [] },
  "maintenance": { "save": {}, "upload": {}, "prune": {}, "warnings": [], "attemptsKnown": true },
  "complete": true,
  "warnings": []
}
```

`-o` takes `json` and nothing else: any other value is refused, and the
command's default output stays the text table it has always printed
(pkg/cli/snapshot.go).

From Go, an external binary compiled against a pinned Stratum release calls:

```go
import "github.com/ctrliq/stratum/pkg/cli"

res := cli.BackupEvidence(ctx)
if !res.Complete {
    // A partial read: res.Warnings names what is unknown.
}
```

The options bound and scope the read:
`WithBackupEvidenceNode`, `WithBackupEvidenceMaxEntries`,
`WithBackupEvidenceMaxBytes` and `WithBackupEvidenceMaxTime`
(pkg/cli/evidence.go). Zero or negative caps mean the engine's built-in
budgets, never an unbounded read.

Over MCP, the tool takes the same caps as its input — `max_entries`,
`max_bytes` and `max_seconds`, each `0` for the built-in budget
(internal/mcp/stratumread.go) — and returns the document as its structured
content. The tool takes no caller-supplied Kubernetes credential: it reads this node's own
files and the configured S3 target, and resolves the node UID through the
Kubernetes API using the installed kubelet credentials. Those credentials
and the S3 credentials stay inside the engine, never in the result. See [MCP Server](mcp.md) for how to run
one.

## The document

The Go result is `BackupEvidenceResult` (pkg/cli/evidence.go); the JSON
keys below are the same on the CLI and MCP. Timestamps are RFC 3339 UTC
strings in JSON and `time.Time` values in Go; the interval is whole seconds
(`intervalSeconds`) in JSON and a `time.Duration` in Go.

The top level:

| Key | Go field | Holds |
|---|---|---|
| `policy` | `Policy` | the effective snapshot policy, every value with its provenance |
| `node` | `Node` | the node the local half is scoped to |
| `nodeUID` | `NodeUID` | immutable Kubernetes node UID; empty when unavailable |
| `local` | `Local` | the bounded local catalog |
| `remote` | `Remote` | the bounded remote catalog |
| `backups` | `Backups` | the deduplicated census |
| `maintenance` | `Maintenance` | the loop's durable maintenance state |
| `complete` | `Complete` | the whole read ran within its bounds |
| `warnings` | `Warnings` | every stage's safe diagnostics |

### Policy

| Key | Go field | Holds |
|---|---|---|
| `state` | `Policy.State` | what you can rely on in the fields below |
| `intervalSeconds` | `Policy.Interval` | how often the scheduled loop takes a snapshot |
| `intervalFrom` | `Policy.IntervalFrom` | where the interval came from |
| `retain` | `Policy.Retain` | how many of this node's snapshots pruning keeps |
| `retainFrom` | `Policy.RetainFrom` | where the retention came from |
| `copy` | `Policy.Copy` | `local-only` or `remote-required` |
| `copyFrom` | `Policy.CopyFrom` | where the copy requirement came from |
| `dir` | `Policy.Dir` | the local snapshot directory |
| `dirFrom` | `Policy.DirFrom` | where the directory came from |
| `warnings` | `Policy.Warnings` | the WARN conditions that shaped this policy |

Each value's *provenance* names where it came from: `cluster-config` (the
config file the controller was started with), `snapshot-conf` (the
operator-managed `snapshot.conf`, which wins outright when it exists),
`default` (the built-in default), or `unknown`. A zero or empty value beside
`unknown` provenance carries no information — the engine invents no default
for a value it cannot read (internal/backupevidence/policy.go).

The reader resolves current files, rather than querying the running loop's
loaded configuration. Restart the controller after changing its cluster
config: until then, reported schedule, directory, disabled state and inline
settings may differ from those the controller uses. The loop reloads
`snapshot.conf` on each tick.

The states describe that file-based resolution (internal/backupevidence/policy.go):

| State | Meaning |
|---|---|
| `configured` | at least one operator-configured value is resolved |
| `disabled` | `spec.snapshots.disabled` is set; a controller started with this configuration omits the loop; other fields still report the configured policy |
| `unconfigured` | resolved values match the built-in defaults, reported with `default` provenance |
| `unknown` | the cluster config the controller was started with cannot be read, so the schedule and the directory are unknown |
| `unreadable` | `snapshot.conf` exists but cannot be read; the loop still takes snapshots but keeps every one and uploads nothing, and the retention and copy policy are unknown |

### Artifacts, copies and the census

`local` is the bounded walk of the snapshot directory:

| Key | Go field | Holds |
|---|---|---|
| `node` | `Local.Node` | the node whose store the walk read |
| `artifacts` | `Local.Artifacts` | the enumerated snapshots, sorted by ID |
| `complete` | `Local.Complete` | the walk covered the whole directory |
| `warnings` | `Local.Warnings` | the walk's WARN conditions |

`remote` is the bounded listing of the S3 target:

| Key | Go field | Holds |
|---|---|---|
| `copies` | `Remote.Copies` | the enumerated remote copies, sorted by ID |
| `old` | `Remote.Old` | reserved age diagnostic; empty because the engine configures count retention only |
| `complete` | `Remote.Complete` | the listing covered the whole store |
| `warnings` | `Remote.Warnings` | the listing's WARN conditions |

One artifact, local or remote, has seven fields
(internal/backupevidence/local.go):

| Key | Go field | Holds |
|---|---|---|
| `id` | `ID` | the stable identity: the file name, shared by the local artifact and the remote copy of the same snapshot |
| `location` | `Location` | `local` or `remote` |
| `locationID` | `LocationID` | opaque identity of the effective store; empty when unknown |
| `copyID` | `CopyID` | stable identity of this backup at that store; empty when unknown |
| `created` | `Created` | when the artifact came to be: the local file's modification time, the remote object's last-modified time |
| `size` | `Size` | bytes |
| `digest` | `Digest` | `sha256:...` from a `<name>.sha256` sidecar beside a local file, else `unknown`; a remote listing exposes no digest. The scheduled save itself does not write sidecars, so an engine-written snapshot reports `unknown` until a sidecar is placed beside it — by operator tooling or a future engine revision |

`backups` is the *census*: the deduplicated union that counts every stable ID
once, however many locations hold it (internal/backupevidence/remote.go):

| Key | Go field | Holds |
|---|---|---|
| `node` | `Backups.Node` | the node the local half is scoped to |
| `entries` | `Backups.Entries` | the distinct backups, sorted by ID |
| `complete` | `Backups.Complete` | both catalogs were complete, so the entry count is the exact total |
| `warnings` | `Backups.Warnings` | the union's WARN conditions |
| `retention` | `Backups.Retention` | reserved age diagnostics; empty under count retention |

Each entry carries `id`, and `local` and `remote` objects with the same
fields as an artifact, each present only where that location holds the
backup. The engine retains the newest N snapshots; it sets no maximum age.
`oldRemote`, `remote.old` and `backups.retention` stay false or empty rather
than inferring an age limit from the schedule. Actual prune failures appear
in maintenance diagnostics.

Location and copy IDs are `sha256:` followed by 64 lowercase hex digits.
The hash input is a compact JSON array of strings. A local location hashes
`["backup-local-location-v1", nodeUID, absoluteDirectory]`; a remote location
hashes `["backup-remote-location-v1", endpoint, region, bucket, prefix]`.
The endpoint and prefix lose one trailing slash, and an empty region becomes
`us-east-1`. A copy hashes `["backup-copy-v1", locationID, id]`. Access and
secret keys never participate, so credential rotation preserves identity.
The existing `id` remains the backup identity used by the census.

If the node UID cannot be read within the shared deadline, the catalogs
remain usable, `nodeUID` and local identity fields are empty, a safe warning
explains the gap, and top-level `complete` is false. Catalog and census
completeness still describe whether their counts are exact.

### Maintenance

`maintenance` is the scheduled loop's durable state, recorded so a restart
keeps it (internal/backupevidence/state.go). Every diagnostic is a typed
condition: no error text crosses, because a raw error can carry a credential,
an executable path or a storage address.

| Key | Go field | Holds |
|---|---|---|
| `save` | `Maintenance.Save` | the latest save attempt |
| `upload` | `Maintenance.Upload` | the latest upload attempt |
| `prune` | `Maintenance.Prune` | the latest prune attempt, independent of save and upload |
| `lastSaveSuccess` | `Maintenance.LastSaveSuccess` | when the save last succeeded, kept separate from the latest attempt so a streak of failures does not erase it |
| `lastUploadSuccess` | `Maintenance.LastUploadSuccess` | when the upload last succeeded |
| `warnings` | `Maintenance.Warnings` | the scheduling and retention warnings the loop's last tick ran with |
| `attemptsKnown` | `Maintenance.AttemptsKnown` | the record was read; false means the diagnostics are unknown, never that no attempt was made |

An attempt is `attemptedAt` and `outcome`. `attemptedAt` is absent when the
loop has not run the attempt yet, and that absence is exact, not an unknown.
`outcome` is `succeeded` or `failed`, absent when no attempt is recorded.
Prune is independent of save and upload: a save that
succeeded while pruning failed is a working backup with broken retention.

## Totals: exact or a floor

Every catalog and the census carry `complete`. True means the list is the
store's exact total, an empty one included. False means the read stopped short
— a cap, a cancellation, a refused or hung store, an unreadable entry — and
the rows are a *floor* while the true total is unknown. Never read a partial
count as the store's total, and never label it exact: a stage the bounds stop
is unknown, never zero.

The same rule composes: the census is exact only when both catalogs were
complete, and the document's top-level `complete` is true only when the census
was exact and the maintenance record was read.

## Bounds and cancellation

One read carries one overall budget, shared by every stage: the local walk
draws from it first and the remote listing gets what remains. A caller that
passes no cap gets the built-ins, never an unbounded read
(internal/backupevidence/reader.go):

| Budget | Built-in | Go option | MCP input |
|---|---|---|---|
| entries — every backup the read may describe | 1000 | `WithBackupEvidenceMaxEntries` | `max_entries` |
| bytes — the artifact bytes both walks account for | 1 TiB | `WithBackupEvidenceMaxBytes` | `max_bytes` |
| time — the deadline over every read the call makes | 20 s | `WithBackupEvidenceMaxTime` | `max_seconds` |

The caller's context propagates into the local walk and the S3 listing: a
cancelled request ends the underlying I/O, and a stage whose context has
already ended launches no read. A hung or denied store ends when the bound ends
it, and the read still returns, with the affected stage a floor and a warning
naming the condition.

## Compatibility

The interface is additive to the released pkg/cli surface: it adds the
`BackupEvidence` call, its option and result types, and the `Backup*` value
types (pkg/cli/evidence.go), and changes no existing identifier — the
exported-surface golden list gained entries only
(tools/apiboundary/testdata/pkgcli.api). The native `stratum snapshot list`
text table stays byte-for-byte what it was; `-o json` is the only addition,
and the JSON rows are additive, not a replacement
(pkg/cli/snapshot_evidence_cli_test.go). The MCP server gains one tool at
the read tier; every existing tool and contract is unchanged. The read changes
nothing on the node: it takes no locks, writes nothing, and returns no
credentials, executable paths or raw storage errors — only typed conditions
and what is unknown about them.

## Fixtures

Complete documents in the shape `snapshot list -o json` prints, one per
situation, live under `docs/docs/appendices/backup-evidence-fixtures/` in the
repository. The wire format is pinned field for field by the tests
(pkg/cli/snapshot_evidence_cli_test.go), and every shipped fixture is
itself pinned: the suite decodes each one against that contract and holds
its sorted orders and structural invariants
(pkg/cli/snapshot_evidence_fixtures_test.go), so a fixture that drifts
from the binary fails the suite.

- [configured-remote-required.json](../appendices/backup-evidence-fixtures/configured-remote-required.json) — a working node: every stage complete, the census counting a snapshot held in both locations once, an older remote copy retained without an invented age limit, and maintenance with typed attempts
- [unconfigured-defaults.json](../appendices/backup-evidence-fixtures/unconfigured-defaults.json) — built-in defaults, `local-only` copies, an exact zero remote store, and a loop that has not run an attempt yet
- [disabled.json](../appendices/backup-evidence-fixtures/disabled.json) — a disabled loop reporting the configured policy beside exact empty stores
- [unreadable-snapshot-conf.json](../appendices/backup-evidence-fixtures/unreadable-snapshot-conf.json) — an unreadable `snapshot.conf`: unknown retention and copy, a save that still succeeds, an unknown remote catalog — the copies it may already have uploaded make it never an exact zero — and the tick's warning
- [unknown-cluster-config.json](../appendices/backup-evidence-fixtures/unknown-cluster-config.json) — an unreadable cluster config: unknown schedule and directory, a local catalog that is not a total, and `snapshot.conf` still supplying retention and copies
- [partial-listing-unknown-totals.json](../appendices/backup-evidence-fixtures/partial-listing-unknown-totals.json) — a read whose remote listing stopped at its bound and whose attempt record could not be read: floors, unknown totals and the warnings that state them
