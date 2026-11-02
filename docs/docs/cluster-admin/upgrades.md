---
description: "Rolling upgrades with the skew rules enforced, and rollback."
---

# Upgrades & Rollback

This page upgrades one node, or a whole cluster, to a newer Kubernetes release,
and rolls a node back to its previous binaries. Use it when a new `stratum`
release is available. You need the new `stratum` binary and root access to the
nodes. Flags, refusals, timings and the objects an upgrade creates are listed in
[Upgrade Reference](../appendices/upgrade-reference.md).

## Before you upgrade

- Get the new `stratum` binary for each architecture in the cluster. This
  binary is the *upgrade binary*. Its embedded
  [payload](../appendices/glossary.md) carries everything the
  upgrade installs: control-plane images, host binaries and add-on images.
- Upgrade one minor version at a time. A cluster upgrade refuses a target that
  skips a minor version, downgrades any node, or changes the major version. It
  also refuses when the control plane would lose etcd quorum. See
  [Preflight refusals](../appendices/upgrade-reference.md#preflight-refusals).
- A control plane takes an etcd snapshot before it changes anything, as
  `/var/backups/etcd/pre-upgrade-<stamp>.db`. Keep it until the upgrade is
  done; see [Snapshot & Restore](snapshot-restore.md#where-snapshots-go).

If the cluster runs an earlier build, check whether any of these apply. Each
link gives the check to run before you upgrade.

| If the cluster | See |
|---|---|
| is a high-availability (HA) cluster with a custom `podCIDR` or `serviceCIDR` | [HA clusters with custom CIDRs](../appendices/upgrade-reference.md#ha-clusters-with-custom-cidrs) |
| has a control-plane config file that sets `clusterDomain` | [Clusters whose config sets `clusterDomain`](../appendices/upgrade-reference.md#clusters-whose-config-sets-clusterdomain) |
| runs images loaded onto a node by hand | [Images loaded onto a node by hand](../appendices/upgrade-reference.md#images-loaded-onto-a-node-by-hand) |
| runs on Rocky Linux, RHEL or RLC with SELinux | [SELinux confinement](../appendices/upgrade-reference.md#selinux-confinement) |
| ran `stratum certs rotate-ca` on an earlier build | [Nodes that finalized a CA rotation on an earlier build](../appendices/upgrade-reference.md#nodes-that-finalized-a-ca-rotation-on-an-earlier-build) |
| has certificates in `system:masters` | [Clusters whose certificates carry system:masters](../appendices/upgrade-reference.md#clusters-whose-certificates-carry-systemmasters) |
| joins control planes with join tokens | [HA join tokens and the shared certs](../appendices/upgrade-reference.md#ha-join-tokens-and-the-shared-certs) |
| joins new workers during the upgrade | [Node client certificate approval](../appendices/upgrade-reference.md#node-client-certificate-approval) |
| has a `HelmChart` with `spec.valuesSecrets` | [HelmCharts that take values from Secrets](../appendices/upgrade-reference.md#helmcharts-that-take-values-from-secrets) |

## Stage the upgrade binary

Copy the upgrade binary to the same path on every node the upgrade touches, for
example `/var/lib/stratum/upgrade/stratum-v1.36.4`, and make it executable. A
cluster upgrade has no in-cluster way to copy it, and each node's upgrade Job
runs the file at that path.

In a cluster that mixes architectures, stage each node's own build at that path:
the `amd64` binary on `amd64` nodes, the `arm64` binary on `arm64` nodes. Every
copy must be the same build. A node with a different build staged refuses, and
the rollout stops without draining it. See
[The staged binary check](../appendices/upgrade-reference.md#the-staged-binary-check).

## Upgrade one node

1. Preview the upgrade. It prints the plan and changes nothing:

    ```console
    # /usr/local/bin/stratum upgrade --target v1.36.4 --from /var/lib/stratum/upgrade/stratum-v1.36.4 --dry-run
    ```

2. Run the upgrade:

    ```console
    # /usr/local/bin/stratum upgrade --target v1.36.4 --from /var/lib/stratum/upgrade/stratum-v1.36.4
    ```

    The installed `stratum` hands the upgrade to the binary named by `--from`
    and prints `handing off to the upgrade binary …`. The new binary continues
    with the same arguments. You do not need to run anything else.

3. Check the installed versions:

    ```console
    # /usr/local/bin/stratum version --components
    ```

On a controller, the upgrade imports the payload images and re-renders the
control-plane static pod manifests one component at a time. On every node, it
swaps the host binaries and restarts the
[supervisor](../appendices/glossary.md). It checks health after each
step. If a check fails, the node rolls back on its own.

`--target` is the bare Kubernetes version, `vMAJOR.MINOR.PATCH`. Leave out the
release suffix, such as `-stratum.0` or `-rc.2`. If you leave out `--target`, the
upgrade targets the version of the `--from` binary, or of the binary you run
when there is no `--from`. To install a prerelease
build, pass its Kubernetes version and point `--from` at the prerelease binary:

```console
# /usr/local/bin/stratum upgrade --target v1.37.1 \
  --from /var/lib/stratum/upgrade/stratum-v1.37.1-stratum.0-rc.3
```

If `stratum upgrade --help` on the installed binary does not say that `--from`
hands off to the upgrade binary, the installed release cannot hand off. Run the
staged binary directly instead:

```console
# /var/lib/stratum/upgrade/stratum-v1.36.4 upgrade
```

On a host with SELinux enforcing, a file copied into `/var/lib/stratum/upgrade`
takes that directory's label, `container_var_lib_t`, and systemd will not
execute a file with that label. `stratum upgrade --from` gives the upgrade
binary the installed binary's label before it hands off, wherever the binary
is staged, so it needs nothing more. A `--dry-run` or `--check` changes
nothing on the node, so it leaves the label as it is and says so.

Running the staged binary directly, as above, works from an interactive root
shell. To run it from a systemd unit or `systemd-run` instead, for example so
the upgrade survives a dropped SSH session, label it first:

```console
# chcon -t container_runtime_exec_t /var/lib/stratum/upgrade/stratum-v1.36.4
```

On a node already running this release or later, `restorecon` does the same
for a `stratum*` file directly in `/var/lib/stratum/upgrade`, because the node
registers that label for those files. Copying a file in does not label it, so
run it after every copy:
`sudo restorecon /var/lib/stratum/upgrade/stratum-v1.36.4`.

## Upgrade a whole cluster

`upgrade --cluster` drains and upgrades every node: control planes first, one at
a time, then workers. Run it on a control-plane node, and start it from the
staged upgrade binary, not from the installed `stratum`. Unlike a one-node
upgrade, it does not hand off to `--from`, and an older installed `stratum`
lacks steps the rollout depends on.

1. Preview the rollout. It prints one state for each node and changes nothing:

    ```console
    # /var/lib/stratum/upgrade/stratum-v1.36.4 upgrade --cluster --dry-run --target v1.36.4 \
      --from /var/lib/stratum/upgrade/stratum-v1.36.4
    ```

    A node already at `--target` shows `check build`: the run upgrades it only
    if it runs a different build. The other states are listed in
    [Plan states](../appendices/upgrade-reference.md#plan-states).

2. Run the rollout. `--cluster` refuses to run without `--yes`:

    ```console
    # /var/lib/stratum/upgrade/stratum-v1.36.4 upgrade --cluster --yes --target v1.36.4 \
      --from /var/lib/stratum/upgrade/stratum-v1.36.4
    ```

    To upgrade more than one worker at a time, add
    `--workers-max-unavailable <n>`. The default is `1`. The drain respects
    PodDisruptionBudgets however many workers drain at once.

3. Follow a node's upgrade while it runs:

    ```console
    # /usr/local/bin/stratum kubectl -n kube-system get jobs -l stratum.ciq.com/upgrade=<node>

    # /usr/local/bin/stratum kubectl -n kube-system logs job/stratum-upgrade-<node>
    ```

4. Check the result. The run ends with
   `Cluster upgrade complete: <n> upgraded, <m> already on this build`. Then
   check that every node reports the new version:

    ```console
    # /usr/local/bin/stratum kubectl get nodes
    ```

If you leave out `--target`, the rollout targets the version of the binary that
starts it. A node you cordoned before the run is drained and upgraded, and stays
cordoned afterwards. The run uncordons only the nodes it cordoned; see
[Cordon ownership](../appendices/upgrade-reference.md#cordon-ownership).

The first node that fails stops new nodes from starting, and nodes already in
flight finish. To continue, follow
[Recover from a halted cluster upgrade](#recover-from-a-halted-cluster-upgrade).

### A single-node cluster

Upgrading a controller restarts its API server. On a single-node cluster, that
is the API server the run talks to. The run keeps polling the node's upgrade Job
while the API server is down, then retries recording the new version and
uncordoning the node for up to 5 minutes. Expect connection errors in between.
They are not a failure.

Every workload is down from the drain until the node is uncordoned. Start the run
from the upgrade binary, as in step 2. An installed `stratum` from an earlier
release halts when the API server restarts. The node's Job still finishes the
upgrade and uncordons the node; re-run the cluster upgrade afterwards to finish
it.

### A new build of the same Kubernetes release

A `stratum` release can change what it installs, such as containerd or a fix,
without changing the Kubernetes version. To roll it out, run the cluster upgrade
with the same `--target` and the new build at `--from`. The run leaves a node
alone, without draining it, when its installed `stratum` matches the staged one
byte for byte. It upgrades every other node.

If the installed `stratum` that starts the rollout predates the build check, it
drains every node. For that first rollout, upgrade each node with
`sudo /usr/local/bin/stratum upgrade --from <binary>` instead.

## Recover from a halted cluster upgrade

If a run stops after it drained a node, it prints the recovery steps for that
node:

```text
  <node> is left cordoned, and its upgrade Job may still be running. To recover:
    1. <path> kubectl -n kube-system get job/<job>       (wait for it to succeed or fail; "not found" means it
       already finished and was cleaned up)
    2. <path> upgrade --cluster --target <target> --from <path> --yes    (finishes the node)
       or, if the node is at the target and healthy, and this run cordoned it (you did not cordon it yourself
       before the upgrade): <path> kubectl uncordon <node>
```

On a controller, the first line says the node `may be left cordoned`. A
controller's upgrade Job uncordons its node once the node's upgrade succeeds,
and that can happen before or after the run stops; see
[Uncordoning after a halted run](../appendices/upgrade-reference.md#uncordoning-after-a-halted-run).

`<path>` is the halted run's `--from` binary. Run every command as printed, with
`sudo`. A path that contains spaces or other shell characters is printed in
single quotes. The commands use that binary because the `stratum` on your `PATH`
may be an older release, and the node has no `kubectl` on its `PATH`.

1. Wait for the Job named in the message to succeed or fail. The Job runs on the
   node, so it keeps running after the run stops. If the Job is not found, it
   already finished. Re-running before the Job finishes is refused, because only
   one upgrade or rollback runs on a node at a time.
2. Preview the step 2 command with `--dry-run` in place of `--yes`. The plan
   lists the halted node as `finish`.
3. Run the step 2 command. It finishes the halted node and goes on to the nodes
   the halted run had not reached.

If you do not want to re-run yet, and the node is at the target and healthy, run
`sudo <path> kubectl uncordon <node>` to restore its workloads. Skip this if you
cordoned the node yourself before the run. Run the step 2 command later to
upgrade the remaining nodes and clear the annotations.

## Roll back a node

`stratum rollback` restores the node's previous binaries and restarts every host
process on them. It restores the newest backup set the last upgrade left; see
[What an upgrade leaves behind](../appendices/upgrade-reference.md#what-an-upgrade-leaves-behind).

1. Preview the rollback. It prints what it would restore and changes nothing:

    ```console
    # /usr/local/bin/stratum rollback --dry-run
    ```

2. Run the rollback:

    ```console
    # /usr/local/bin/stratum rollback
    ```

3. Check the installed versions with `sudo /usr/local/bin/stratum version --components`.

A rollback that restores host binaries needs only the stratum supervisor to be
running. It recovers a node whose kubelet or containerd is crash-looping on the
new binaries. A rollback that restores no host binary needs every host process
running. That is a rollback that only restores `--etcd-snapshot`, or one from an
upgrade interrupted before its host binaries were swapped. `stratum upgrade` refuses on a node with a host process down, and
suggests `stratum rollback` when an interrupted upgrade left a backup set that
can recover it. Roll back, then re-run the upgrade.

A control plane joins as an etcd learner, and its own controller promotes it to
a voter once it has caught up. A release from before learner joins does not
promote it. Before rolling back a control plane, run
`sudo /usr/local/bin/stratum etcdctl member list -w table` on another control
plane, and roll back only once this node's member shows `false` under
`IS LEARNER`. Run the check elsewhere because a learner's own etcd refuses
`member list`. If a rolled-back control plane is left a learner, its apiserver
stays down and the next control-plane join is refused. Promote it from another
control plane with `sudo /usr/local/bin/stratum etcdctl member promote <member ID>`.

A cluster initialized with a non-default `spec.network.clusterDomain` cannot be
rolled back to a release that predates custom-domain support.

### Rolling back across an etcd minor version

When the upgrade moved etcd to a newer minor version, a rollback on a single
control plane downgrades etcd on its own and keeps every write. On a cluster with
more than one control plane, the rollback refuses and changes nothing. Its error
names the etcd version it restores, as `<major>.<minor>`. To roll back:

1. On any one control plane, set `VERSION` to that version and start the etcd
   downgrade:

    ```console
    # /usr/local/bin/stratum etcdctl downgrade enable "$VERSION"
    ```

2. Wait until every member shows that version under `STORAGE VERSION`:

    ```console
    # /usr/local/bin/stratum etcdctl endpoint status --cluster -w table
    ```

3. Run `sudo /usr/local/bin/stratum rollback` on each control plane, one at a time. etcd ends
   the downgrade once every member runs the older version.

To stay on the newer version instead, run `sudo /usr/local/bin/stratum etcdctl downgrade cancel`.
The other cases, including an etcd that does not answer, are in
[etcd storage version during a rollback](../appendices/upgrade-reference.md#etcd-storage-version-during-a-rollback).

### Restoring etcd with a rollback

Use `--etcd-snapshot` when an upgrade changed data in etcd that you want to undo.
It restores etcd from the
[pre-upgrade snapshot](snapshot-restore.md#where-snapshots-go) after the binaries
are rolled back.

!!! warning "Restoring the snapshot discards later writes and stops the control plane"
    `--etcd-snapshot` stops the API server and etcd and resets etcd to the
    snapshot. Every write made after the snapshot is lost. It requires `--yes`.

```console
# /usr/local/bin/stratum rollback --etcd-snapshot /var/backups/etcd/pre-upgrade-<stamp>.db --yes
```

It works only on a single control plane. A restore resets etcd to a new
one-member cluster, so rollback refuses when etcd has more than one member, or
when it cannot confirm the member count. For an HA cluster, use
[`snapshot restore` and `rejoin`](snapshot-restore.md#restoring) instead.

## When an upgrade fails

| Message | What to do |
|---|---|
| `stratum upgrade: <operation> is already running on this node (pid <pid> holds <lock file>); nothing was changed` | Another upgrade, rollback or other node maintenance is running. Wait for it to finish, then retry. `--dry-run` is never refused this way. |
| `--target takes the bare Kubernetes version: use --target <version>, not "<target>" …` | Pass the version the message names. |
| `--target <t> does not match the upgrade binary <path>, which installs Kubernetes <v> — wrong artifact staged?` | Stage the binary for `<t>`, or pass `--target <v>`. |
| `the upgrade binary at <path> on this node is not the one the cluster upgrade is rolling out …` | Stage the same build at that path on every node, then re-run. |
| `containerd <installed version> is installed but the runtime serving /run/stratum/containerd/containerd.sock reports <running version> — …` | A containerd the supervisor did not start holds the socket. Remove it as described in [A containerd Stratum did not start](../runtime/index.md#a-containerd-stratum-did-not-start), then run the upgrade again. |
| `kubelet <installed> is installed but node <name> reports kubelet <running>` | The kubelet did not come back on the new version. Check its log, `/var/lib/stratum/log/kubelet.log`. |
| `the supervisor did not come back on <installed binary> with every component running within 3m0s (…)` | The upgrade keeps the new build and does not roll back. See [The supervisor restart](../appendices/upgrade-reference.md#the-supervisor-restart). |
| `the in-progress upgrade marker /var/lib/stratum/upgrade/in-progress.json …` | The marker of an interrupted upgrade cannot be read. Check the `*.bak-<stamp>` backups, then delete the file. |
| `<node> hosts a CoreDNS replica and no other replica became Ready within 5m0s; …` | Get a CoreDNS replica Ready on another node, then re-run the cluster upgrade. |

What each health check tests is listed in
[The node health gate](../appendices/upgrade-reference.md#the-node-health-gate).
