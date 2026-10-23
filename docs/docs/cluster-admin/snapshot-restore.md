---
description: "Scheduled etcd snapshots, restore, and rejoining a member."
---

# Snapshot & Restore

This page saves and schedules etcd snapshots, restores a cluster from one,
replaces a lost controller, and rejoins a control plane to a restored cluster.
You need root on the control planes, and on the workers after a restore. File names, settings, restore
rules and rejoin behaviour in full are in the
[Snapshot Reference](../appendices/snapshot-reference.md).

## Taking snapshots

```bash
sudo /usr/local/bin/stratum snapshot save                 # now
sudo /usr/local/bin/stratum snapshot schedule             # the recurring timer
sudo /usr/local/bin/stratum snapshot list
```

A snapshot is a consistent copy of etcd's data, taken through etcd rather than
copied from disk. The supervisor on each control plane takes one every 12
hours. When an S3 target is configured, Stratum uploads each snapshot as well
as keeping it locally.

### Where snapshots go

Stratum writes snapshots to the *snapshot directory*, `/var/backups/etcd`
unless `spec.snapshots.dir` sets another absolute path. A snapshot is named
`etcd-snapshot-<host>-<stamp>.db`, or the name you pass with
`snapshot save --name <file>`.

After each snapshot, Stratum keeps the newest `retain` snapshots for this host,
5 by default, and deletes older ones. Locally and in S3 it prunes only names
of the form `etcd-snapshot-<host>-<stamp>.db`. Snapshots with any other
name, including most `--name` snapshots and the `pre-upgrade-*.db` snapshot
an upgrade takes, stay until you delete them. See
[Snapshot files](../appendices/snapshot-reference.md#snapshot-files) and
[Retention](../appendices/snapshot-reference.md#retention).

### Schedule settings

Set the interval and the directory under `spec.snapshots` in
`/etc/stratum/config.yaml`, then restart the node:

```yaml
spec:
  snapshots:
    intervalHours: 12       # the default
    disabled: false         # true stops the scheduled snapshot
    dir: /var/backups/etcd  # the default; the local snapshot directory, an absolute path
```

Set retention and the S3 target with `stratum snapshot schedule`. It writes
`/etc/stratum/snapshot.conf`, and the running snapshot loop reads the file on
its next tick:

```bash
sudo /usr/local/bin/stratum snapshot schedule --retain 10 \
  --s3-bucket etcd-backups --s3-endpoint https://minio.example.com:9000 --s3-path-style
```

Once `snapshot.conf` exists, its values win over `spec.snapshots.retain` and
`spec.snapshots.s3` in `config.yaml`. Every key, and how the two sources
combine, is in
[Where retention and the upload target come from](../appendices/snapshot-reference.md#where-retention-and-the-upload-target-come-from).

### Credentials for remote storage

`stratum snapshot schedule` never writes credentials from your environment to disk. Put them in
`/etc/stratum/stratum.env`, owned by root with mode `0600`, so uploads keep
working after a reboot:

```bash
# /etc/stratum/stratum.env
STRATUM_S3_ACCESS_KEY=<access-key>
STRATUM_S3_SECRET_KEY=<secret-key>
```

The unit loads this file each time systemd starts it. See
[Environment for the unit](../installation/configuration.md#environment-for-the-unit)
for how to create it, and
[Credentials](../appendices/snapshot-reference.md#credentials) for the order in
which the node reads keys.

!!! warning "A snapshot exposes every Secret"
    A snapshot is a copy of the whole datastore. Anyone who can read it can
    read every Secret that is not encrypted at rest, which without a profile
    that encrypts them is all of them.

    - Keep the snapshot directory root-only (`0700`). Stratum sets this mode on
      a directory it creates, but does not change an existing one.
    - For S3, use a private bucket with server-side encryption and an
      `https://` `--s3-endpoint`. Scope the credential to that bucket.
    - Back up the at-rest key that an encrypting profile keeps under
      `/var/lib/stratum/pki/` separately. It is not in the snapshot, and a
      restore cannot read encrypted Secrets without it.

    See [Snapshot security](../appendices/snapshot-reference.md#snapshot-security).

!!! warning "A snapshot on the same disk does not survive losing the machine"
    Configure remote storage if the cluster matters. A snapshot still does not
    hold the cluster CA or the node state, and a replacement controller without
    them mints a CA that nothing trusts. Back those files up as well; see
    [Losing the controller machine](#losing-the-controller-machine).

## Restoring

Restore takes the cluster back to a snapshot, and discards everything written
since. Run it on one control plane, the *seed*. To bring back one failed
member of a healthy cluster, use [Rejoining a member](#rejoining-a-member)
instead.

1. List the snapshots:

    ```bash
    sudo /usr/local/bin/stratum snapshot list
    ```

2. Restore one by the name `snapshot list` prints:

    ```bash
    sudo /usr/local/bin/stratum snapshot restore etcd-snapshot-<host>-<stamp>.db --yes
    ```

    You can also pass a path to a file, or `s3://<bucket>/<key>` from the
    configured S3 target. A name with no `/` is always looked up in the
    snapshot directory, so pass `./<name>` for a file in the current directory.
    See [The snapshot argument](../appendices/snapshot-reference.md#the-snapshot-argument).

3. Read the output. Restore moves the current datastore to
   `/var/lib/stratum/etcd.pre-restore-<UTC stamp>`, restarts the control plane
   and the components that watch the cluster, and prints one line for each. It
   ends with a `Still to do:` list. Work through it as
   [After a restore](#after-a-restore) describes.

If the restore fails before etcd runs on the restored data, Stratum puts the
original datastore back and restarts on it. The error says what it rolled back.
See [What restore does to the datastore](../appendices/snapshot-reference.md#what-restore-does-to-the-datastore).

### What the restore restarts

The restore restarts kubelet, `kube-controller-manager`, `kube-scheduler`,
`kube-vip` and the stratum operator on the seed. It then restarts Cilium,
CoreDNS, the local-path provisioner and the dashboard in the cluster, through
the seed's own API server at `https://127.0.0.1:6443`. These hold a view of the
cluster from before the restore, and left running they serve stale data. The
full list, with the symptom of each, is in
[What the restore restarts](../appendices/snapshot-reference.md#what-the-restore-restarts).

A rollout that does not finish within 60 seconds prints a `WARN` line. The
restore still succeeds. Check one with:

```bash
sudo /usr/local/bin/stratum kubectl --server https://127.0.0.1:6443 -n kube-system rollout status ds/cilium
```

If a component cannot be restarted, the restore fails with
`etcd restored, but not every component holding a pre-restore watch or start-up state was restarted`
and names it. The restored data stays in place. Do not run the restore again.
Restart the named components by hand: kill kubelet, the operator or a static
pod's process, and the supervisor or kubelet restarts it. Restart the
in-cluster workloads on the seed, one command per namespace:

```bash
sudo /usr/local/bin/stratum kubectl --server https://127.0.0.1:6443 -n kube-system rollout restart ds/cilium deploy/cilium-operator deploy/coredns
sudo /usr/local/bin/stratum kubectl --server https://127.0.0.1:6443 -n local-path-storage rollout restart deploy/local-path-provisioner
sudo /usr/local/bin/stratum kubectl --server https://127.0.0.1:6443 -n stratum-dashboard rollout restart deploy/stratum-dashboard
```

Keep `--server` as shown. Until the other control planes rejoin, the
[virtual IP (VIP)](../appendices/glossary.md) in `admin.conf` can be down or
lead to one that still holds the old data. Then
continue with [After a restore](#after-a-restore).

### After a restore

The restore does not touch the other nodes. It prints this list when it
succeeds, and when it fails only because some components could not be
restarted:

1. Rejoin every other control plane, one at a time, with
   [`stratum snapshot rejoin --force`](#rejoining-a-member) pointed at the seed.
2. Restart kubelet on every other node: workers, and each control plane once it
   has rejoined. Stopping the process makes the supervisor restart it:

    ```bash
    sudo pkill -x kubelet
    ```

3. Check that every Pod is `Running` with a current IP:

    ```bash
    sudo /usr/local/bin/stratum kubectl get pods -A -o wide
    ```

4. If the cluster has other control planes, repeat the restarts once every one
   has rejoined. Until then, components that reach the API server through the
   VIP may have reloaded stale data. On each control plane, run:

    ```bash
    sudo pkill -x kubelet
    sudo pkill -TERM -f '^([^[:space:]]*/)?[k]ube-controller-manager([[:space:]]|$)'
    sudo pkill -TERM -f '^([^[:space:]]*/)?[k]ube-scheduler([[:space:]]|$)'
    sudo pkill -TERM -f '^([^[:space:]]*/)?[k]ube-vip([[:space:]]|$)'
    sudo pkill -TERM -f '^[^[:space:]]+ operator( [^[:space:]]+)* --kubeconfig (/var/lib/stratum/kubeconfig/super-admin\.conf|/var/lib/stratum/kubeconfig/admin\.conf)( [^[:space:]]+)* --manifests-dir /var/lib/stratum/manifests( [^[:space:]]+)* --helm-cache /var/lib/stratum/helm( [^[:space:]]+)* --leader-elect([[:space:]]|$)'
    ```

    Kubelet recreates the controller containers, and the supervisor restarts
    kubelet and the operator. The operator's pattern matches its subcommand and
    flags, because its executable can carry a release artifact's name, such as
    `stratum-linux-amd64`.

    Then restart the in-cluster workloads once:

    ```bash
    sudo /usr/local/bin/stratum kubectl -n kube-system rollout restart ds/cilium deploy/cilium-operator deploy/coredns
    sudo /usr/local/bin/stratum kubectl -n local-path-storage rollout restart deploy/local-path-provisioner
    sudo /usr/local/bin/stratum kubectl -n stratum-dashboard rollout restart deploy/stratum-dashboard
    ```

5. Restart the controllers and operators you installed yourself, such as
   cert-manager, an ingress or GitOps controller, or a database operator. The
   restore does not restart them, and they hold a pre-restore view too:

    ```bash
    sudo /usr/local/bin/stratum kubectl -n <namespace> rollout restart deploy/<name>
    sudo /usr/local/bin/stratum kubectl -n <namespace> rollout status deploy/<name>
    ```

    Use `ds/<name>` or `sts/<name>` for a DaemonSet or StatefulSet. Find
    candidates with `sudo /usr/local/bin/stratum kubectl get deploy,ds,sts -A`. Until the
    other control planes have rejoined, add `--server https://127.0.0.1:6443`.

An upgrade rollback that restores etcd prints the same list, without the items
about other control planes.

### Cleaning up after a restore

!!! warning "Restore copies can fill the disk etcd needs"
    Each pre-restore and failed-restore copy is a full copy of etcd's data, on
    the same disk as the live datastore. Nothing removes them automatically,
    and the pre-restore copy may be the only copy of the data from before the
    restore. Remove them once the cluster is verified.

```bash
sudo /usr/local/bin/stratum snapshot prune-restores          # list the copies and their sizes
sudo /usr/local/bin/stratum snapshot prune-restores --yes    # remove them
```

## Losing the controller machine

Use this procedure when a controller is gone with its disk and no other control
plane is left. The replacement becomes the cluster's only control plane, and
you add more afterwards with a normal join. A snapshot holds only etcd's data,
so the procedure also restores the controller's PKI and node state from a
backup.

!!! warning "A restore can overwrite a cluster that still has quorum"
    Use this only when every other control plane is destroyed, or confirmed
    powered off and kept off. If one may still be running, the cluster may have
    quorum and data newer than any snapshot, and a restore discards it. Treat
    the lost machine as a failed member of a running cluster instead, or
    contact support.

### What to back up

Back up these files from each controller, together with the snapshots. Back them
up again after a certificate or CA rotation, a change to the cluster config or
to `/var/lib/stratum/manifests/`, or enabling a profile:

- `/var/lib/stratum/pki/`
- `/var/lib/stratum/config/node.yaml`
- `/etc/stratum/`
- the file passed to `stratum up --config`, if it is not `/etc/stratum/config.yaml`
- `/var/lib/stratum/manifests/`, if you added manifests or stacks
- `/var/lib/stratum/kubeconfig/` and `/etc/systemd/system/stratum.service` (optional)

What each one holds is in
[What to back up](../appendices/snapshot-reference.md#what-to-back-up).

!!! danger "The PKI directory is the cluster's root of trust"
    Anyone holding `/var/lib/stratum/pki/` can sign credentials the cluster
    accepts as any user, and can decrypt Secrets encrypted at rest. Encrypt
    the backup before it leaves the host, store it apart from the snapshots,
    and limit access to the people who would run a recovery.

As root, create the archive under `umask 077`, encrypt it, then remove the
unencrypted copy. Keep it outside `/var/lib/stratum` and `/etc/stratum`, and
add your `--config` file to the list if it lives elsewhere. Each step runs only
if the one before it succeeded. Copy a backup off the host only from a run that
printed `backup complete`:

```bash
sudo -i
umask 077
backup=/root/stratum-node-$(hostname)-$(date -u +%Y%m%dT%H%M%SZ).tgz
tar -czpf "$backup" \
  /var/lib/stratum/pki \
  /var/lib/stratum/config/node.yaml \
  /var/lib/stratum/manifests \
  /var/lib/stratum/kubeconfig \
  /etc/stratum \
  /etc/systemd/system/stratum.service \
  && gpg --symmetric --cipher-algo AES256 --output "$backup.gpg" "$backup" \
  && shred -u "$backup" \
  && echo "backup complete: $backup.gpg"
exit
```

Copy only the `.gpg` file off the host. Keep the passphrase with your other
recovery secrets, not next to the archive.

#### Back up again after adding control planes

After you add control planes, or upgrade a cluster that has them, check the
first controller:

```bash
sudo grep etcdHasPeers /var/lib/stratum/config/node.yaml
```

When it shows `etcdHasPeers: true`, take a new backup of that controller and
discard the older ones. No output means the record is not set; check again
in a minute. A backup taken before the record is set lets a replacement start
without the quorum check that protects a running cluster. See
[The etcdHasPeers record](../appendices/snapshot-reference.md#the-etcdhaspeers-record).

### Replacing the controller

The restored files must be in place before the replacement's first start,
because that start otherwise creates a new CA. The commands call Stratum as
`/usr/local/bin/stratum`: on a stock Rocky Linux host, `sudo` leaves
`/usr/local/bin` off the `PATH`, and `sudo stratum` fails with
`command not found`.

1. If the cluster had other control planes, confirm that every one is
    destroyed, or powered off through its console or cloud provider and kept
    off. Not answering on the network does not count. Take the list of control
    planes from your inventory, cloud console, or a node list saved while the
    cluster ran, not from the backed-up `node.yaml`, which can name only some
    of them.

    If any may still be running, or you cannot say for certain whether the
    cluster had other control planes, stop here.

2. Bring up the replacement machine with the same hostname and IP address as
   the old controller. The certificates and node state name that address, and
   the workers connect to it.
3. Install the same Stratum release the old controller ran, at
   `/usr/local/bin/stratum`. Do not start it.
4. Copy the encrypted backup to `/root` on the replacement, then decrypt and
    unpack it at `/`:

    ```bash
    sudo -i
    umask 077
    enc=/root/stratum-node-<host>-<timestamp>.tgz.gpg
    plain=/root/stratum-node-restore.tgz
    rm -f "$plain" \
      && gpg --output "$plain" --decrypt "$enc" \
      && tar -xzpf "$plain" -C / \
      && shred -u "$plain" \
      && echo "files restored"
    exit
    ```

    If the output does not end with `files restored`, stop, and do not start
    Stratum until it does. If the old controller used a `--config` file outside
    `/etc/stratum`, check that it is back at the same path: the restored unit
    names that path, and `stratum up` refuses to start when the file is
    missing. Only the default `/etc/stratum/config.yaml` may be absent; the
    node then starts with the defaults, which can replace the cluster's CIDRs
    and VIP.

5. Start the node with the restored unit:

    ```bash
    sudo systemctl enable --now stratum
    sudo journalctl -u stratum -n 20 --no-pager
    ```

    The output names the address it uses, for example
    `node IP 10.60.1.10 (saved node state)`.

    - If the cluster had one controller, or the backup predates its growth, the
      control plane comes up on an empty datastore. Go on to step 6.
    - If the node state records more than one member, startup refuses with
      `refusing to start an empty member, which could overwrite a cluster whose
      other members still hold quorum`. Step 1 confirmed no other control plane
      runs, so stop the unit and start the node once by hand with the flags from
      `systemctl cat stratum` and `--confirm-etcd-quorum-lost`:

        ```bash
        sudo systemctl stop stratum
        sudo /usr/local/bin/stratum up --config /etc/stratum/config.yaml --confirm-etcd-quorum-lost
        ```

        This runs in the foreground, so do step 6 in a second terminal. On the
        first controller, the API comes up on a new, empty datastore. On a
        control plane that joined later, the API stays down until the restore,
        and `kubectl` fails with errors such as `no route to host`. Once step 7
        passes, stop it with Ctrl-C and run `sudo systemctl start stratum`. The
        unit starts without the flag from then on.

    Under a profile that encrypts Secrets at rest, startup refuses with
    `this node has saved node state ... but no etcd data` if the key file under
    `/var/lib/stratum/pki/` is missing. Restore the original key from the
    backup. A new key cannot read the restored Secrets.

6. Copy the snapshot onto the node and restore it. The commands create
    `/var/backups/etcd` root-only, and restore only if the copy succeeded:

    ```bash
    sudo install -d -m 0700 /var/backups/etcd \
      && sudo install -m 0600 <snapshot>.db /var/backups/etcd/ \
      && sudo /usr/local/bin/stratum snapshot restore /var/backups/etcd/<snapshot>.db --yes
    ```

    If `/var/backups/etcd` already exists with looser permissions, run
    `sudo chmod 700 /var/backups/etcd` first. If the restored `snapshot.conf`
    names an S3 target you can reach, you can pass `s3://<bucket>/<key>`
    instead.

    From the list the restore prints, skip rejoining the other control planes
    and repeating the restarts: none is left to rejoin. The other items are
    step 7.

7. Restart kubelet on every worker, and the controllers you installed yourself
    as in step 5 of [After a restore](#after-a-restore):

    ```bash
    sudo pkill -x kubelet
    ```

    Then check that the replacement and every worker are `Ready`, and every
    Pod is `Running` with a current IP:

    ```bash
    sudo /usr/local/bin/stratum kubectl get nodes
    sudo /usr/local/bin/stratum kubectl get pods -A -o wide
    ```

    Control planes from the old cluster stay listed as `NotReady`, because
    their Node objects are in the snapshot. They are no longer etcd members.
    Delete each one:

    ```bash
    sudo /usr/local/bin/stratum kubectl delete node <node-name>
    ```

8. To run more control planes again, join freshly installed machines as in
    [Growing to three controllers](high-availability.md#growing-to-three-controllers).
    Mint the token on the replacement and run the join command it prints, as
    `sudo /usr/local/bin/stratum up --join ...`:

    ```bash
    sudo /usr/local/bin/stratum token create --control-plane
    ```

    On a cluster that runs a profile, the printed command already carries
    `--profile <name>`. Give each new machine the same `spec.vip` in its `/etc/stratum/config.yaml`
    as the replacement, so its certificate covers the VIP. Do not start Stratum
    again on a machine from the old cluster with its old disk.

9. Once the cluster is verified, remove the copies the restore kept, as in
   [Cleaning up after a restore](#cleaning-up-after-a-restore).

If the replacement started before the files were restored, it has already
created a new CA. Restoring files over a running node is not supported, so
uninstall and start again from step 3.

!!! warning "Uninstall deletes `/var/lib/stratum` and `/etc/stratum`"
    It also deletes the unit. Before you run it, check that the encrypted
    backup and the snapshot are outside those paths, for example in `/root` or
    `/var/backups/etcd`, or off the host.

```bash
sudo /usr/local/bin/stratum uninstall --yes
```

## Rejoining a member

Rejoin wipes one control plane's etcd and adds it back as a member of the
cluster, which it then resynchronises from. Use it on each other control plane
after a restore, and to repair a controller whose etcd has diverged or lost its
data while the cluster stays healthy.

!!! warning "Restore and rejoin are not interchangeable"
    Restore takes the cluster back to the snapshot, and everything written since
    is gone. Rejoin keeps the cluster where it is and brings one member back
    into line. Restoring a single failed member of a healthy cluster throws away
    work the other members already have.

1. Choose the seed: the node that ran `snapshot restore`, or for a repair, a
   healthy member of the same cluster. Use its own address, not the VIP: etcd
   does not listen on the VIP, so rejoin refuses it.
2. For a repair, check on the seed that every endpoint apart from the failed
    member is healthy, and that the member list is the cluster you expect:

    ```bash
    sudo /usr/local/bin/stratum etcdctl endpoint health --cluster
    sudo /usr/local/bin/stratum etcdctl member list -w table
    ```

3. On the node you are rejoining, never on the seed, run:

    ```bash
    sudo /usr/local/bin/stratum snapshot rejoin --server https://<seed-node-ip>:6443 --yes
    ```

    This form goes ahead only when rejoin can confirm that this node's own etcd
    cluster has lost quorum. Most rejoins refuse with
    `not wiping this node's etcd: <reason> ... Nothing was changed`, because
    this node's data may be newer than the seed's.

4. If it refuses, and this node's data can be discarded, run it again with
    `--force`:

    ```bash
    sudo /usr/local/bin/stratum snapshot rejoin --server https://<seed-node-ip>:6443 --yes --force
    ```

    This node's data can be discarded when you restored the seed on purpose,
    when every other member of this node's old cluster is destroyed or kept
    off, or when you are repairing a failed member against a healthy majority.

5. After a restore, repeat on each remaining control plane, one at a time.

Each node joins as a non-voting member and becomes a voter once it has caught
up, so the seed keeps serving while it catches up. Rejoin waits while the
member makes progress, and gives up after 5 minutes without progress or 30
minutes in all. Re-running rejoin is safe after a timeout, a failure or Ctrl-C:
it resumes rather than starting over. The checks, the wait and what a re-run
does are in [Rejoin](../appendices/snapshot-reference.md#rejoin).

!!! warning "A voting member the cluster is waiting on"
    If rejoin reports that this node is a voting member the cluster needs for
    quorum, do not remove the member or wipe its data. The cluster cannot
    commit anything, its own removal included, until the member is up. etcd
    keeps retrying, and re-running rejoin waits again without wiping anything.
    Read the etcd pod's log under `/var/log/pods/kube-system_etcd-*`. Only if
    the member cannot start at all, run `snapshot restore` on the seed again
    and rejoin the other control planes, which discards what was written since
    the last restore.

The MCP `snapshot_rejoin` tool cannot pass `--force`. Run the CLI on the node.
