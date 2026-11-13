---
description: "Prepared release notes for the tagged release that first exposes the public backup-evidence interface — no tag and no release exist yet."
---

# Backup Evidence Release Notes

These are the prepared release notes for the tagged release that first
exposes Stratum's public backup-evidence interface. No tag and no release
exist: cutting both is a human action, and this page only records what the
release will carry, verified against the code as it stands on this branch.
When the release is cut, give these notes their version here and publish
them with it.

The interface itself, field by field, is documented on
[Backup Evidence](../cluster-admin/backup-evidence.md); this page says what
changes for a consumer moving onto the release.

## The interface

The release exposes the engine's backup evidence — its read-only view of
what it does for backups: the effective snapshot policy with every value's
provenance, the local snapshot artifacts, the remote copies on the S3
target, and the maintenance diagnostics — as one bounded read, served
identically from three surfaces:

- From Go, `BackupEvidence(ctx, opts...)` in package `pkg/cli`
  (pkg/cli/evidence.go:348), with the `WithBackupEvidenceNode`,
  `WithBackupEvidenceMaxEntries`, `WithBackupEvidenceMaxBytes` and
  `WithBackupEvidenceMaxTime` options (pkg/cli/evidence.go:299-322) and the
  `Backup*` value types the result is built from (pkg/cli/evidence.go:25-281).
  This is the sanctioned surface for an external pinned binary: it imports
  no engine internals and re-derives none of the engine's configuration
  resolution — the engine resolves its own policy
  (internal/backupevidence/policy.go:144).
- From the CLI, `stratum snapshot list -o json`
  (pkg/cli/snapshot.go:113-121). The flag takes `json` and nothing else;
  any other value is refused, and the command's default output stays the
  text table it has always printed (pkg/cli/snapshot.go:136-138).
- Over MCP, the `backup_evidence` tool, registered at the read tier with no
  credential (internal/mcp/catalog.go:221-230) and served by the same read the
  CLI prints (internal/mcp/stratumread.go:952-963), so the surfaces cannot
  drift. A server started with no flags serves it, raising that set from
  19 to 20 tools; see [MCP Server](../cluster-admin/mcp.md).

A read that passes no cap runs on the engine's built-in bounds — 1000
backups, 1 TiB of artifact bytes, 20 seconds — never unbounded
(internal/backupevidence/reader.go:82-90, internal/backupevidence/local.go:29-32).
A stage the bounds stop is a floor with the true total unknown, never zero
and never an exact count; the census counts every stable backup ID once
across both locations, which two separate store listings cannot reconstruct
by summing or comparing totals (internal/backupevidence/remote.go:148-169).

## The publisher

The release also starts a node-owned backup-evidence publisher beside the
scheduled snapshot loop, as one of the controller's post-start tasks, so it
runs wherever snapshots run and needs no operator
(internal/cluster/run.go:947-955). An operator-disabled node starts and
records it exactly as an operator-enabled one does — proved on a live node
(tools/inttest/check-backup-evidence-operator-off.sh:2-5), and a node
upgraded from a release that has no publisher activates one on its own with
no operator involvement (tools/inttest/check-upgrade-from-release.sh:118-124).

The publisher samples the engine's evidence through the same read the
surfaces serve, on the engine's built-in bounds (pkg/cli/up.go:650-666), on a
one-minute cadence with the first sample at once
(internal/cluster/run.go:1583-1586, internal/cluster/run.go:1638-1642), and
records a bounded summary — node, counts, whether the total is exact, safe
warnings — durably at `/var/lib/stratum/config/backup-evidence.yaml`, written
atomically at mode 0600 with a schema version a later revision can refuse
instead of misreading (internal/cluster/run.go:1595-1626,
internal/cluster/run.go:1666-1695). It shares the snapshot loop's gate: with
snapshots disabled in the cluster config there is no backup evidence to
publish, and the publisher does not start (internal/cluster/run.go:947-955).
The record is the node's own proof of what the publisher last saw, readable
without calling any interface; whole reads come from the three surfaces
above.

## Compatibility

The release changes nothing a consumer of an earlier one relies on:

- The pkg/cli surface is additive. The release adds the `BackupEvidence`
  call, its option and result types, and the `Backup*` value types; the
  exported-surface golden list gained entries only and changed no existing
  identifier (tools/apiboundary/testdata/pkgcli.api:41). No internal type
  crosses the surface.
- The native `stratum snapshot list` text table stays byte-for-byte what it
  was; `-o json` is the only addition, and the JSON rows are additive, not a
  replacement (pkg/cli/snapshot_evidence_cli_test.go:262-265).
- The MCP server gains one tool at the read tier and no flags; every
  existing tool and contract is unchanged.
- The read is read-only on the node: it takes no locks and writes nothing,
  and its result carries no credentials, executable paths or raw storage
  errors — only typed conditions and what is unknown about them
  (pkg/cli/evidence.go:331-347).
- The maintenance diagnostics are durable: the scheduled loop's save,
  upload and prune attempts and last-success stamps survive a process
  restart (internal/backupevidence/state.go:54-72,
  internal/backupevidence/state.go:122-144).

A consumer pinned to a release before this one finds no backup-evidence
surface in `pkg/cli`: the interface first ships here. An external binary
could not work around that by importing the engine's internals even if it
wanted to: Go refuses an import of another module's `internal/` packages, and
the supported surface is what `pkg/cli` exports, pinned by the
exported-surface golden test (tools/apiboundary/testdata/pkgcli.api:41).

## Using the release

A consumer moving onto the release reads evidence from one of the three
surfaces and treats every count by its `complete` field: true is the exact
total, an empty one included; false is a floor with the total unknown. The
complete JSON document for each situation — a working node, built-in
defaults, a disabled loop, an unreadable config, a partial listing — is
pinned field for field in the fixtures under
[Backup Evidence](../cluster-admin/backup-evidence.md#fixtures), and the
wire format is pinned by the tests
(pkg/cli/snapshot_evidence_cli_test.go:55-57) while every shipped fixture is
itself decoded against that contract with its sorted orders and structural
invariants held (pkg/cli/snapshot_evidence_fixtures_test.go:33), so a
fixture that drifts from the release fails the suite rather than a consumer.
