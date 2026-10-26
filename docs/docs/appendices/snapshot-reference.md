---
description: "Snapshot files, retention, settings, restore rules, the controller backup and rejoin behaviour, in full."
---

# Snapshot Reference

This page lists the exact files, settings, rules and messages behind
`stratum snapshot`. Use it when
[Snapshot & Restore](../cluster-admin/snapshot-restore.md) does not answer your
question, or you need the behaviour of an edge case. Each section stands on its
own.

## Snapshot files

File names carry a UTC timestamp in the form `YYYYMMDD-HHMMSS`.

| File | Written by | Directory | Uploaded to S3 |
|---|---|---|---|
| `etcd-snapshot-<host>-<stamp>.db` | `snapshot save` and the schedule | the snapshot directory | yes, when S3 is configured |
| `<file>` | `snapshot save --name <file>` | the snapshot directory | yes, when S3 is configured |
| `pre-upgrade-<stamp>.db` | `stratum upgrade` on a control plane, before it changes anything | always `/var/backups/etcd` | no, local only |

- `<host>` is the node's short hostname. Control planes that share one S3
  bucket never overwrite or prune each other's snapshots.
- `snapshot save --name <file>` joins `<file>` onto the snapshot directory as
  given. A path such as `../<file>` writes outside the directory, so use a
  plain file name.
- `snapshot list` shows local files only when their names end in `.db`. It
  lists S3 objects whatever their names, but only those directly under the
  S3 prefix. Objects under a deeper prefix, such as another cluster's
  `<prefix>/other/`, are not listed.
- Pre-upgrade snapshots stay in `/var/backups/etcd` even when
  `spec.snapshots.dir` is set. `snapshot list` does not show them then.

## Retention

After every `snapshot save`, scheduled snapshot and `snapshot prune`, Stratum
keeps the newest `retain` snapshots for this host. Newest means the latest
file modification time locally and the latest upload time in S3, so a clock
that steps backwards cannot make a fresh snapshot look old. Stratum deletes
the older snapshots whose names match:

| Where | Pruned when the name is exactly |
|---|---|
| Local, in the snapshot directory | `etcd-snapshot-<host>-<stamp>.db` |
| S3 | `etcd-snapshot-<host>-<stamp>.db` |

- A snapshot saved with `--name` is pruned only when its name has exactly
  that form; `etcd-snapshot-<host>-before-maint.db` is kept.
- Anything else, including `pre-upgrade-*.db`, stays until you delete it.
- On S3, retention counts and deletes only objects directly under the prefix.
  Snapshots under a deeper prefix are neither counted nor deleted, so another
  target whose prefix sits inside this one keeps its own snapshots.
- `snapshot prune --retain <n>` keeps the newest `<n>` for that run, in place
  of the configured count.
- A prune tries every older snapshot even when one cannot be deleted, and
  prints a `WARN:` line naming each one that failed. The command still exits 0.
- If the S3 upload fails, the save still prunes the snapshot directory, so an
  S3 outage cannot fill the disk. It leaves the bucket alone and exits with
  the upload error.

## Where retention and the upload target come from

The scheduled snapshot loop, `stratum snapshot save`, `list`, `prune`,
`restore s3://…` and `schedule`, and the MCP snapshot tools all read the
retention count and S3 target from one source. The MCP `snapshot_save` tool
reads only the S3 target there: it ignores the configured retention and prunes
only when you pass `retain`.

| When | Retention and S3 target come from |
|---|---|
| `/etc/stratum/snapshot.conf` exists | its `RETAIN` and `S3_*` values. `spec.snapshots.retain` and `spec.snapshots.s3` are ignored, even where the file leaves a value unset. |
| `snapshot.conf` does not exist | `spec.snapshots.retain` and `spec.snapshots.s3` in `config.yaml` |
| `snapshot.conf` does not exist, and `config.yaml` cannot be read or parsed, or the file passed to `stratum up --config` has been deleted | the built-in defaults: retain 5 and no S3 target. The command prints a warning to stderr that names the file and the error. |

`config.yaml` has no credential fields. Wherever the target comes from, the
node reads S3 credentials as [Credentials](#credentials) lists.

### Which config.yaml

- `config.yaml` means the file the controller was started with:
  `/etc/stratum/config.yaml`, or the path you passed to `stratum up --config`.
- The controller records that path in its node state each time it starts. The
  CLI and MCP tools read the recorded path.
- Until the controller has restarted on this release, the CLI and MCP tools
  read `/etc/stratum/config.yaml`, even if the controller uses another file.
- If the node state that holds the recorded path cannot be read or parsed, they
  read `/etc/stratum/config.yaml` and print a warning to stderr. The retention
  they use may then differ from the controller's.
- The scheduled loop always uses the controller's own file.

### snapshot.conf

- Only `stratum snapshot schedule` and the MCP `snapshot_schedule` tool create
  `snapshot.conf`, with mode `0600`.
- Both start from the settings already in effect. On a node without the file,
  they keep the S3 target from `config.yaml`.
- The MCP tool changes only retention.
- With the CLI, `--s3-bucket` replaces the whole S3 target: a flag you leave
  out clears that setting.
- Once the file exists, change it by running `stratum snapshot schedule` again
  or by editing it. The running controller's snapshot loop reads it on its next
  tick.
- A control-plane node joined by an earlier release has a default
  `snapshot.conf` (retain 5, no S3), which hides its `config.yaml` values.
  Delete `/etc/stratum/snapshot.conf` on such a node if it should use
  `config.yaml`.

| Key | Set by | Meaning |
|---|---|---|
| `RETAIN` | `--retain <n>`; `0` leaves it unchanged | snapshots kept per node |
| `S3_BUCKET` | `--s3-bucket` | the bucket; setting it turns on the upload |
| `S3_ENDPOINT` | `--s3-endpoint` | a non-AWS endpoint, such as MinIO or Ceph |
| `S3_REGION` | `--s3-region` | the region; `us-east-1` when unset |
| `S3_PREFIX` | `--s3-prefix` | key prefix within the bucket |
| `S3_PATH_STYLE` | `--s3-path-style` | path-style addressing, which most non-AWS endpoints need |
| `S3_ACCESS_KEY`, `S3_SECRET_KEY` | never written from your environment; kept if already present | S3 credentials on disk |

### spec.snapshots in config.yaml

| Key | Default | Meaning |
|---|---|---|
| `intervalHours` | `12` | hours between scheduled snapshots; a negative value is an error |
| `disabled` | `false` | `true` stops the scheduled snapshot |
| `dir` | `/var/backups/etcd` | the local snapshot directory; see [The snapshot directory](#the-snapshot-directory) |
| `retain` | `5` | snapshots kept per control plane, when no `snapshot.conf` exists |
| `s3.bucket` | none | the upload bucket, when no `snapshot.conf` exists |
| `s3.prefix` | none | key prefix within the bucket |
| `s3.region` | none | the region |
| `s3.endpoint` | none | a non-AWS endpoint; omit it for AWS |
| `s3.pathStyle` | `false` | path-style addressing, which MinIO, Ceph and most non-AWS endpoints need |

`intervalHours`, `disabled` and `dir` reach the scheduled loop when the node
restarts. `snapshot save`, `list` and `prune` read the directory on each
invocation.

## The snapshot directory

- `spec.snapshots.dir` sets it. The default is `/var/backups/etcd`.
- The scheduled loop, `snapshot save`, `list`, `prune` and `restore`, and the
  MCP `snapshot_list`, `snapshot_save`, `snapshot_prune`, `snapshot_restore`
  and `rollback` tools all use it. They read it from the same `config.yaml` as
  [above](#which-configyaml), and use `/var/backups/etcd` if that file cannot be
  read.
- `dir` must be an absolute path. A relative value, such as `backups` or
  `./backups`, is ignored, and every reader, the scheduled loop included, uses
  `/var/backups/etcd`. The `stratum snapshot` commands and the MCP tools print
  `warning: spec.snapshots.dir "<dir>" is not absolute; using /var/backups/etcd`
  to stderr.
- A `dir` with a `..` element, such as `/srv/snaps/../x`, is ignored the same
  way, with
  `warning: spec.snapshots.dir "<dir>" contains a ".." element; using /var/backups/etcd`.
- A `.` element, repeated slashes and a trailing slash are cleaned without a
  warning, so `/srv/./snaps`, `//srv//snaps` and `/srv/snaps/` all mean
  `/srv/snaps`.
- Earlier releases resolved a relative value against the working directory of
  the process that read it. If you set one, replace it with the absolute path
  of the directory your snapshots are in, or `snapshot list` and retention no
  longer see them.
- Stratum creates a missing snapshot directory with mode `0700`. It does not
  change the mode of an existing one.
- `snapshot save`, `list`, `prune` and the scheduled loop follow symlinks in
  the path. `snapshot restore s3://…` does not; see
  [Downloading an s3:// snapshot](#downloading-an-s3-snapshot).

## Credentials

`stratum snapshot schedule` never writes credentials from your environment to
disk. On every scheduled snapshot, the running node resolves each key
separately. The first one set wins:

| Order | Access key | Secret key |
|---|---|---|
| 1 | `STRATUM_S3_ACCESS_KEY` in the environment | `STRATUM_S3_SECRET_KEY` in the environment |
| 2 | `S3_ACCESS_KEY` in `snapshot.conf` | `S3_SECRET_KEY` in `snapshot.conf` |
| 3 | `AWS_ACCESS_KEY_ID` in the environment | `AWS_SECRET_ACCESS_KEY` in the environment |

- If `snapshot.conf` already holds a key, the matching `AWS_*` variable is
  ignored.
- Keys already in `snapshot.conf` keep uploads working after a reboot without
  `stratum.env`. To move them off disk, set `STRATUM_S3_*` in `stratum.env`,
  remove `S3_ACCESS_KEY` and `S3_SECRET_KEY` from `snapshot.conf`, and check
  that the next upload succeeds.
- `stratum snapshot schedule` prints a note when it finds credentials in
  `snapshot.conf`.
- After a reboot, systemd starts the node with none of your shell's
  environment. Credentials that come only from the shell stop the upload; local
  snapshots keep working. Put them in `/etc/stratum/stratum.env`, owned by root
  with mode `0600`. See
  [Environment for the unit](../installation/configuration.md#environment-for-the-unit).

## Snapshot security

- A snapshot holds the whole datastore, Secrets included. Without a profile
  that encrypts Secrets at rest, it holds all of them in clear text.
- Under a profile that encrypts Secrets at rest, Secrets written after the
  profile was turned on are encrypted. Secrets that existed before stay in
  clear text until they are rewritten through the API server. Later snapshots
  hold the rewritten data; earlier snapshots keep their original contents.
- The at-rest key, a file the profile keeps under `/var/lib/stratum/pki/`, is
  not in the snapshot. A reader with the snapshot and the key can read the
  encrypted Secrets. A restore without the key cannot.
- An S3 credential used for backups needs write, list and delete on the
  bucket. One used for `snapshot restore s3://…` also needs read.

## Restore

### The snapshot argument

`stratum snapshot restore <NAME|FILE|s3://bucket/key> --yes`:

| Argument | Resolved as |
|---|---|
| a name with no `/`, as `snapshot list` prints it | that file in the snapshot directory |
| `./<name>` | that file in the current directory |
| an absolute path | used as given |
| any other relative path | prefixed with the current directory and not otherwise changed, so `..` after a symlink still follows the symlink |
| `.`, `..` or another directory | refused before anything is stopped |
| `s3://<bucket>/<key>` | downloaded from the configured S3 target, then restored |

Without `--yes`, restore exits with
`stratum snapshot restore is disruptive (resets etcd on this seed node); re-run with --yes`.

The MCP `snapshot_restore` tool resolves a bare name in the snapshot directory.
It refuses any other relative path, so pass it an absolute path for a file
elsewhere.

### Matching an s3:// URL

An `s3://` snapshot always comes from the configured S3 target, because that
target's credentials are the only ones the node has. Without a target, restore
exits with
``s3:// restore needs an S3 target (configure it with `stratum snapshot schedule --s3-bucket …`)``.

| URL | Result, with prefix `snaps` |
|---|---|
| `s3://<bucket>/<name>` | read as `<name>` under the target's prefix |
| `s3://<bucket>/snaps/<name>` | accepted: the key is exactly `<prefix>/<name>` |
| `s3://<bucket>/snaps/./<name>`, `s3://<bucket>/snaps//<name>`, `s3://<bucket>/other/../<name>` | refused: the key is compared as written |
| another bucket, another prefix, or no object | refused |

- When the configured prefix starts with `/` or ends in more than one `/`, the
  key is that prefix as written. With prefix `/snaps` only
  `s3://<bucket>//snaps/<name>` matches, and with `snaps//` only
  `s3://<bucket>/snaps//<name>`. `s3://<bucket>/snaps/<name>` is refused for
  both. Pass the bare key, or set the prefix as `snaps`.
- A refusal names the bucket or key the configured target would use. It comes
  before restore creates the snapshot directory or downloads anything, so
  nothing on the node changes.

### Downloading an s3:// snapshot

- The snapshot is downloaded into the snapshot directory under its key's base
  name. It replaces any local file of that name, and is restored from there.
- Restore opens the snapshot directory one path component at a time from `/`,
  without following symlinks, and creates each missing component with mode
  `0700`. It refuses the directory, and downloads nothing, when:
    - any component is a symlink, such as `link` in `/var/backups/link/etcd`.
      The error names the component. This also refuses `/var/backups` linked to
      another disk; set `spec.snapshots.dir` to the real path instead.
    - any component is not a directory.
    - the directory is writable by group or others, or is owned by another user.
- Otherwise it leaves the directory's mode as it is. It writes through the
  directory it opened and checked, so renaming the directory or replacing it
  with a symlink during the download does not move where the file lands.
- The file is written with mode `0600` to a temporary name, `.<name>.<digits>`,
  and renamed into place when the download completes. A failed download leaves
  an existing local file of that name untouched.
- A download killed partway, by `SIGKILL` or a power loss, leaves the temporary
  file behind, holding a partial copy of the snapshot. The next
  `snapshot restore s3://…` of the same name removes such regular files last
  written more than 15 minutes ago. It leaves younger ones, which may belong to
  a running download, and every other file.
- If it cannot remove one, it prints
  `warning: removing stale download temp files in <dir>: <error>` and carries
  on. Temporary files for other names stay until those names are restored; you
  can delete them by hand.

### What restore does to the datastore

Restore rebuilds this node's datastore from the snapshot and resets etcd to a
new single-member cluster.

| Situation | Result |
|---|---|
| any restore | the current datastore is moved to `/var/lib/stratum/etcd.pre-restore-<UTC stamp>` first |
| the restore fails before etcd runs on the restored data | the original datastore goes back in place, and etcd and the API server restart on it. What the failed attempt wrote is kept at `/var/lib/stratum/etcd.failed-restore-<UTC stamp>`. The error says what was rolled back. |
| the original datastore cannot be put back | the control plane stays stopped. The error says where the original datastore and the failed restore are. |
| the node has lost its datastore entirely | restore works. If it fails there is nothing to roll back to, and the control plane stays stopped. |
| the node was staged by an older release and has no `etcdutl` | refused before anything is stopped; run `stratum stage` |

`stratum snapshot prune-restores` lists the pre-restore and failed-restore
copies with their sizes, and `--yes` removes them. Nothing removes them
automatically.

### What the restore restarts

A restore takes etcd's resourceVersions back to the snapshot's. A component
watching the cluster from before the restore is never told its view is stale.
Left running, each one shows its own symptom:

| Component | Symptom without a restart |
|---|---|
| kubelet | keeps the pods it started after the snapshot and never starts the restored ones |
| Cilium | can drop the backend of the `kubernetes` Service, so pods that reach the API through `10.96.0.1` fail with `no route to host` |
| CoreDNS | can answer headless-Service names from post-snapshot endpoints |
| local-path provisioner | can leave a restored PersistentVolumeClaim unprovisioned |
| dashboard | keeps the cluster CA it read at start |

The restore checks this node's own API server, `https://127.0.0.1:6443`, not
the [virtual IP (VIP)](glossary.md) in `admin.conf`, which can be down until the
other control planes rejoin. The check probes for up to 2 minutes. A failed
restore's rollback is checked the same way. Each `kubectl` call the restore
makes is killed after 30 seconds without an answer and named as the call that
failed. Once the API server is healthy, the restore restarts these in order and
prints one line for each:

| Order | Component | How |
|---|---|---|
| 1 | kubelet on this node | through the supervisor |
| 2 | `kube-controller-manager`, `kube-scheduler` and, with a VIP, `kube-vip` | from the manifests on disk, not re-rendered, so an upgrade rollback keeps the older images it restored. Each is skipped on a node without its manifest. |
| 3 | the stratum operator | through the supervisor. It then re-registers its webhook endpoint and reacquires its leader lease. Skipped on a node that is not a control plane. |
| 4 | `ds/cilium` and `deploy/cilium-operator` in `kube-system` | `rollout restart` through this node's API server |
| 4 | `deploy/coredns` in `kube-system` | the same |
| 4 | `deploy/local-path-provisioner` in `local-path-storage` | the same, when the cluster runs it |
| 4 | `deploy/stratum-dashboard` in `stratum-dashboard` | the same, when the cluster runs it |

- Each step 4 target is skipped if the cluster does not run it, and always
  restarted otherwise, even if another restart fails.
- The restore waits up to 60 seconds in total for the rollouts, all at once.
  A rollout that does not finish in that time is a `WARN` line, not a failure.
  Expect it on a cluster with workers, until their kubelets restart.
- The restore does not restart `cilium-envoy`, containerd, your own workloads,
  or controllers and operators you installed.
- If a component cannot be restarted, the restore tries the rest, then fails
  with
  `etcd restored, but not every component holding a pre-restore watch or start-up state was restarted`
  and the names of the ones that failed. The restored data stays in place. Do
  not run the restore again.
- An upgrade rollback restores etcd the same way. There, a component that
  could not be restarted is a `WARN` line rather than an error, so a retried
  rollback does not restore etcd twice. Its list of what is left, which the
  MCP rollback result also carries, leaves out rejoining and repeating the
  restarts, because a rollback restores a single-member cluster.

## Controller backup

### What to back up

Copy these off each controller with the snapshots. Copy them again after a
certificate or CA rotation, a change to the cluster config, a change under
`/var/lib/stratum/manifests/`, or enabling a profile.

| Path | What it holds | Needed |
|---|---|---|
| `/var/lib/stratum/pki/` | The cluster CA, front-proxy CA and etcd CA (certificates and private keys), the service-account signing key, any secret the node's profile shares between control planes, such as an at-rest encryption key, and `netconfig`, the cluster's pod and service CIDRs | Yes. **Secret.** |
| `/var/lib/stratum/config/node.yaml` | The saved node state: role, hostname, node IP, API server URL, VIP, etcd member name and cluster | Yes |
| `/etc/stratum/` | `config.yaml`; `snapshot.conf`, the S3 target, which can hold S3 keys; `stratum.env`, which can hold credentials; `registries.yaml` | Yes, if present. Secret if it holds credentials. |
| The file passed to `stratum up --config` | The cluster config, when it is not `/etc/stratum/config.yaml`. The unit's `ExecStart` shows the path. | Yes, if used |
| `/var/lib/stratum/manifests/` | What the operator applies: manifests and stacks you added, and `.applied.json`, its record of what each stack applied. The bundled stacks in it are rewritten on start. | Yes, if you added manifests or stacks |
| `/var/lib/stratum/kubeconfig/` | `admin.conf` and the component kubeconfigs | Optional: `stratum up` rebuilds them from `pki/` on every start. Secret. |
| `/etc/systemd/system/stratum.service` | The unit, with the `stratum up` flags the node runs with | Optional: `stratum up` writes it again |

`/var/lib/stratum/config/manifests/` holds the control-plane static pods, which
`stratum up` generates. You do not need to back it up.

### The etcdHasPeers record

- `stratum up` refuses to start a controller whose saved node state records an
  etcd cluster with more than one member when its etcd data is gone. It prints
  `refusing to start an empty member, which could overwrite a cluster whose other members still hold quorum`.
  `--confirm-etcd-quorum-lost` lifts the refusal for one start.
- The first controller sets `etcdHasPeers: true` in its `node.yaml` within
  about a minute of seeing a second etcd member, while it runs a release that
  has this check. Its `etcdState` stays `new` and its `etcdInitialCluster`
  keeps one member.
- A backup of the first controller taken before the record is set looks like a
  single-controller cluster, so `stratum up` cannot protect a replacement
  restored from it.
- Under a profile that encrypts Secrets at rest, a missing key file under
  `/var/lib/stratum/pki/` makes startup refuse with
  `this node has saved node state ... but no etcd data`.

## Rejoin

`stratum snapshot rejoin --server https://<seed-node-ip>:6443 --yes [--force]`.

### Checks on --server

- `--server` is required, and must be the seed control plane's own address.
  Rejoin talks to the seed's etcd directly, and etcd does not listen on the VIP,
  so a VIP is refused.
- Rejoin first refuses a `--server` that is this node's own address, with or
  without `--force`, and says that this node is the seed. On a dual-stack node,
  both halves of the node's address count as its own.
- With `--force`, it also refuses a `--server` that answers as this node's own
  etcd member: this node reached by a hostname, loopback or another interface.
  It reads the `--server` etcd's member list, takes the member that answered,
  and refuses if one of its peer URLs is on one of this node's addresses. It
  never reads this node's own etcd for this check.
- It also refuses when the answering member is not in the list, the list cannot
  be read, or the member's peer URLs are missing or are not IP addresses.
- A `--server` that is a different member of the same cluster is not refused.
  That is the repair of a diverged member against its healthy majority.
- A mistyped address of another member of the old cluster is not caught. Rejoin
  wipes this node's data and adds it back to that cluster, and does not touch
  the seed.
- Nothing changes when a check refuses.

### Learners and the wait

- Each rejoining node is added as an etcd *learner*, a member with no vote, and
  promoted to a voter once it has caught up with the seed. The seed's quorum
  does not depend on it until then.
- Rejoin waits while the member makes progress: first its data directory
  changing on disk, then, once etcd answers, its applied index advancing.
- It gives up after 5 minutes with no progress, and in any case after 30
  minutes.

| Ending | State left |
|---|---|
| no progress for 5 minutes, and the seed confirms a learner | this node's etcd is stopped, so a re-run replaces the learner |
| no progress for 5 minutes, and the seed does not confirm a learner | etcd is left running, because a promote whose reply was lost may have made it a voter |
| 30 minutes with progress | etcd is left running, so a re-run resumes the wait |
| Ctrl-C | the member keeps catching up; a re-run while its etcd runs resumes the wait |

### What a re-run does

Rejoin reads the seed's membership and this node's etcd before it changes
anything:

| What rejoin finds | What it does |
| --- | --- |
| this node is not a member | removes the stale local data and adds a learner. If the data directory holds member data, the quorum check runs first, unless `--force` is passed. |
| this node's etcd is serving as its member | keeps everything and resumes: waits, promotes a learner, starts the API server |
| a learner whose etcd is running but not answering | keeps everything and resumes the wait |
| a learner whose etcd is not running, or answers as another cluster | removes the learner and the local data, then adds a new learner, with the same check first |
| a voter that is not serving, and the other voters have quorum without it | removes and re-adds it, with the same check first |
| a voter that is not serving, and the cluster needs its vote | keeps its data and membership, starts etcd, and waits |

- If the seed's membership cannot be read because the cluster has lost quorum,
  rejoin falls back to a read that needs none, and then only keeps everything
  and resumes. It never removes a member or wipes a data directory on a
  membership it could not confirm.
- The pod log of a member's etcd is under `/var/log/pods/kube-system_etcd-*`.

### The quorum check

Before a first rejoin wipes anything, it reads the member list from this node's
etcd, with a 5-second timeout, and sorts the voting members:

| Group | Members | Counted |
|---|---|---|
| Answers in this cluster | this node, and every peer that reports the same cluster ID | yes |
| Answers from another cluster | for example a restored seed on an old member's address | no |
| Cannot be reached | a timeout, a TLS failure or no connection | yes, as possibly in the cluster |

Learners are not counted. Rejoin goes ahead without `--force` only when the
first and third groups together fall short of a majority. With three voters,
both other voters must answer from another cluster. With five, one of the other
four can be unreachable if the remaining three answer from another cluster.

Otherwise it refuses with `not wiping this node's etcd: <reason> ... Nothing was changed`:

| Reason | Example |
|---|---|
| the cluster still has quorum | `2 of its 3 voting members answer, 2 are needed` |
| rejoin cannot rule out quorum | `1 of its 3 voting members answer in this cluster and 1 could not be reached (they may hold a majority), 2 are needed` |
| rejoin cannot tell: this node's etcd is not running, its membership cannot be read, or it reports no voting members | `cannot read this node's etcd membership (context deadline exceeded)` |

- A survivor whose etcd restarted after quorum was lost, for example after a
  reboot, never becomes ready. Rejoin cannot tell, and needs `--force`.
- Most rejoins need `--force`. A diverged member of a healthy cluster still
  sees quorum. A member whose data directory is gone has no etcd running. After
  a seed restore, the other control planes still form a cluster: with two of
  three left, it has quorum.
- After a rejoin that wiped this node's etcd and then failed, a re-run needs
  `--force` only if the data directory holds member data again.
- The MCP `snapshot_rejoin` tool cannot pass `--force`.

## MCP snapshot tools

| Tool | Acts like |
|---|---|
| `snapshot_save` | `stratum snapshot save`, but it prunes only when you pass `retain` (0, the default, keeps all). `name` must be a bare file name ending in `.db`. `skip_s3` skips the upload. |
| `snapshot_list` | `stratum snapshot list` |
| `snapshot_prune` | `stratum snapshot prune`; `keep` must be at least 1 |
| `snapshot_schedule` | `stratum snapshot schedule`, for retention only; it reports the S3 target but cannot change it |
| `snapshot_restore` | `stratum snapshot restore`; a bare name or an absolute path only |
| `snapshot_restore_copies` | `stratum snapshot prune-restores` without `--yes`: lists the copies |
| `snapshot_prune_restores` | `stratum snapshot prune-restores --yes`, for the copies you name |
| `snapshot_rejoin` | `stratum snapshot rejoin`, without `--force` |

See [MCP](../cluster-admin/mcp.md) for how to run the server and which tools
need `--allow-write` or `--allow-destructive`.
