---
description: "Flags, refusals, objects, timings and per-release checks behind stratum upgrade and stratum rollback."
---

# Upgrade Reference

This page lists the detail behind [Upgrades & Rollback](../cluster-admin/upgrades.md):
the flags, the rules a cluster upgrade checks first, the objects and files an
upgrade leaves, and the checks for clusters on an earlier build. Use it to look
up a value or to read a symptom. The upgrade procedures are on the Upgrades &
Rollback page.

## Flags

`stratum upgrade`:

| Flag | Default | What it does |
|---|---|---|
| `--from <path>` | this binary's own payload | the upgrade binary, the new `stratum` binary staged on the node; required with `--cluster` |
| `--target <version>` | one node: the `--from` binary's version, else this binary's; `--cluster`: the version of the binary that starts the run | the bare Kubernetes version, `vMAJOR.MINOR.PATCH` |
| `--cluster` | off | roll the whole cluster; run it on a control-plane node |
| `--yes` | off | required with `--cluster`, unless `--dry-run` is set |
| `--workers-max-unavailable <n>` | `1` | with `--cluster`, how many workers upgrade at once; control planes always upgrade one at a time |
| `--drain-grace <seconds>` | `60` | with `--cluster`, the drain's grace period |
| `--soak <seconds>` | `15` | how long to wait before the health gate |
| `--skip-snapshot` | off | skip the pre-upgrade etcd snapshot a control plane takes |
| `--dry-run` | off | print the plan and change nothing |
| `--check` | off | report whether this node needs the upgrade, and change nothing; see [Exit codes of `--check`](#exit-codes-of-check); not with `--cluster` |

`stratum rollback`:

| Flag | Default | What it does |
|---|---|---|
| `--etcd-snapshot <file>` | none | also restore etcd from this snapshot; a single control plane only |
| `--yes` | off | required with `--etcd-snapshot`, unless `--dry-run` is set |
| `--soak <seconds>` | `15` | how long to wait after the rollback before the health gate |
| `--dry-run` | off | print the plan and change nothing |

## Preflight refusals

Before it changes anything, `upgrade --cluster` refuses when one of these holds.
The skew checks compare the target with the *lowest* node version, because
every node converges on the target. The downgrade check compares it with the
*highest*.

| Refusal | Cause |
|---|---|
| `target must be vMAJOR.MINOR.PATCH (e.g. v1.36.2), got "<target>"` | the target is not a bare version |
| `--target takes the bare Kubernetes version: use --target <base>, not "<target>" (the build suffix is not part of it)` | the target carries a build suffix such as `-stratum.0` or `-rc.2` |
| `cross-major change not allowed (target <t>, cluster at <v>)` | the target changes the major version |
| `downgrade not allowed (target <t> < highest node <v>)` | the target is below the highest node, patch versions included |
| `cannot skip minor versions (target <t>, lowest node <v>; upgrade one minor at a time)` | the target is more than one minor above the lowest node |
| `control-plane not quorum-healthy: <ready>/<total> CP nodes Ready` | half or fewer of the control planes are Ready |
| `control-plane has no margin: …` | a control plane is NotReady, and draining another would leave etcd without quorum. Three control planes with one NotReady are refused. Bring the NotReady one back, or upgrade it on its own with `stratum upgrade --from`, then retry |
| `node <node> has an unfinished upgrade to <v> …` | a node's interrupted upgrade went to a different version. Finish it with that `--target`, or run `stratum rollback` on that node |

A single node also refuses a `--target` its binary does not install:
`--target <t> does not match the upgrade binary <path>, which installs Kubernetes <v> — wrong artifact staged?`.

## Exit codes of `--check`

`sudo /usr/local/bin/stratum upgrade --from <binary> --check` asks whether this node already
runs the upgrade binary's version and build. It changes nothing.

| Exit code | Meaning | What a cluster upgrade does |
|---|---|---|
| `0` | the node already runs this version and build | leaves the node alone |
| `10` | the node needs the upgrade | upgrades it |
| `11` | the node refuses the upgrade: see [SELinux prerequisites](#selinux-prerequisites) | refuses to start, before any node is drained |
| `1` | it cannot tell, for example while another upgrade or rollback runs on the node | stops at that node without draining it |

## Plan states

`upgrade --cluster --dry-run` prints one state for each node:

| State | Meaning |
|---|---|
| `upgrade` | the node is behind `--target` and is upgraded |
| `check build (not drained if it already runs this build)` | the node is at `--target`; a short Job compares its installed `stratum` with the staged one, byte for byte, and the node is upgraded only if they differ |
| `finish (interrupted after its kubelet swap)` | an earlier upgrade to `--target` was interrupted after the kubelet was replaced; the run completes it |
| `skip (NotReady: its build cannot be checked; the run ends with an error naming it)` | the node is at `--target` but NotReady |

## The staged binary check

Each node runs its own copy of the file at `--from`. The run pins every copy to
the build of the copy on the node it runs on. It refuses to start if that
binary does not report its build ID or its version.

- Every node's copy must be the same build: the same source, built the same
  way (build tags, dashboard assets), on every architecture.
- A node of the same architecture as the node running the rollout must hold
  exactly the same bytes.
- A short Job checks each Ready node before it is cordoned. A node with another
  binary staged refuses, and the rollout stops without draining it.
- A NotReady node behind `--target` cannot run that Job. The upgrade checks its
  binary after the drain, and a mismatch leaves the node cordoned.

The installed `stratum` that starts the rollout makes the build check, not the
`--from` binary. An installed `stratum` from a release without the check drains
every node, and installs the new build only if the `--from` binary has the
check.

## What a cluster upgrade creates

| Object | Name | Purpose |
|---|---|---|
| Lease | `kube-system/stratum-cluster-upgrade` and `kube-system/stratum-cluster-maintenance` | allow one cluster maintenance run at a time: an `upgrade --cluster`, a certificate rotation, renewal or CA migration. A refusal names the holder as `<host>/<pid>/<id>` |
| Job | `stratum-upgrade-<node>`, labeled `stratum.ciq.com/upgrade=<node>` | runs the staged binary on that node |
| Node label | `stratum.ciq.com/control-plane=true` | marks control planes, which upgrade first and one at a time |
| Node annotation | `stratum.ciq.com/upgrade-cordoned=<kind>@<holder>` | who owns the node's cordon during the upgrade; see [Cordon ownership](#cordon-ownership) |
| Node annotation | `stratum.ciq.com/upgrade-in-progress=<version>` | set before a node's Job starts, removed when the node is back Ready at that version |
| Node annotation | `stratum.ciq.com/k8s-version=<version>` | the Kubernetes version the node runs |
| Node annotation | `stratum.ciq.com/version=<version>` | the Stratum build the node runs |

- Long node names. When `stratum-upgrade-<node>` would exceed 63 characters
  (a node name over 47), Stratum truncates the Job name and adds a hash suffix.
  When the node name is not a valid label value, for example over 63
  characters, the label value is `node-<hash>`. Every Job carries the node's full
  name in its `stratum.ciq.com/node` annotation.
- The leases. The run holds them for 60 seconds at a time and renews them
  every 20 seconds. A second `upgrade --cluster` refuses while they are held, and
  the error names the holder. If the first run's process is gone, the leases
  expire within 60 seconds. You do not need to delete them.
- The annotations. A node that still has `upgrade-in-progress` was
  interrupted during its upgrade. The next run finishes it, or refuses if it
  was going to a different version. Every node sets `k8s-version` and `version`
  on its own Node each time its kubelet starts, and a cluster upgrade sets them
  when it finishes a node.
- The control-plane label. Every control plane labels its own Node when it
  starts. If no Node has the label, the upgrade reads the etcd member list to
  tell control planes from workers, and refuses if it cannot read it.

## How the rollout proceeds

- Control planes upgrade first, one at a time, so etcd keeps its quorum.
  Workers follow, up to `--workers-max-unavailable` at once.
- Before it touches any node, the run imports the upgrade binary's images on
  every Ready node, up to four at a time. A node that cannot import them stops
  the run before anything is cordoned. A NotReady node is skipped with a warning.
- Each node is drained before it is upgraded. The drain uses the eviction API,
  so the API server enforces PodDisruptionBudgets however many nodes drain at
  once.
- CoreDNS has no PodDisruptionBudget, so the run drains at most one node that
  hosts a CoreDNS replica at a time. It drains that node only once another
  replica is Ready on another Ready node. It waits up to 5 minutes, then stops
  with the node cordoned. On a single-node cluster it prints
  `<node> is the only node; CoreDNS is down while it drains` and goes on.
- The first node failure stops new upgrades from starting. Nodes already in
  flight finish.
- The run ends with `Cluster upgrade complete: <n> upgraded, <m> already on this build`.

## Add-ons during a rolling upgrade

The operator renders the add-ons (Cilium, CoreDNS, the storage class, the
dashboard) from the binary the leading control plane runs. Once that control
plane is upgraded, the new manifests roll out to every node, including workers
still on the previous release. The add-ons are built to run on the oldest node
in the cluster, so this state is supported for the rest of the roll.

Those pods need the new release's images on nodes still on the previous
release. The images come from the upgrade binary's payload, because the mirror
the manifests name serves no anonymous pulls and an airgapped node reaches no
registry. `upgrade --cluster` imports them on every Ready node first; see
[How the rollout proceeds](#how-the-rollout-proceeds).

- A NotReady node skipped by the import may not start the new add-on pods until
  it is upgraded. If it is Ready again by its turn, its drain can stall until the
  drain times out. Re-run the cluster upgrade once it is Ready; the re-run
  imports its images first.
- Only a run started from a release with the import step imports the images.
  Start the run from the new binary. An upgrade binary without the step cannot
  import them either: the run warns and goes on.
- Upgrading node by node with `stratum upgrade`, and no `--cluster`, has no
  import step. Each node imports the new images only during its own upgrade.
  Until then, the new add-on pods cannot start on that node unless a
  [registry mirror](../runtime/index.md#registry-mirrors) serves their images.
  Without the Cilium agent, new pods there get no network, and pods being
  removed can stay `Terminating`. Use `upgrade --cluster`, or upgrade the
  remaining nodes right after the first control plane.
- CoreDNS on a node still on the older release picks its upstream resolver from
  the node's own resolvers (see
  [Where pods' upstream DNS comes from](../networking-storage/index.md#where-pods-upstream-dns-comes-from)),
  so names keep resolving. If pods on such a worker lose DNS during a roll,
  `kubectl -n kube-system logs <coredns pod> -c upstream-resolv` shows what that
  pod chose.

## Cordon ownership

The run uncordons only the nodes it cordoned. A node that is already cordoned
when the run reaches it is drained and upgraded, and stays cordoned afterwards.
This holds for a halted run, a re-run and the upgrade Job's own uncordon.

The run records who owns each cordon in the node's
`stratum.ciq.com/upgrade-cordoned` annotation, as `<kind>@<holder>`. The holder
is the run's identity on the lease, `<host>/<pid>/<start time>`, for example
`v1.37.1@ctl1/48211/1791249600000000000`. Do not set or edit the annotation.
The run removes it when the node finishes.

| Kind | Meaning |
| --- | --- |
| the target version, such as `v1.37.1` | the run cordoned a schedulable node, and undoes that cordon when the node finishes |
| `preexisting` | the node was already cordoned; the run leaves it cordoned |
| `released` | the node's upgrade Job already undid the run's cordon; a cordon added after that is yours |

- Only a kind that is a valid version counts as the upgrade's own. Any other
  value, including a misspelled kind or one with extra whitespace, counts as an
  administrator's cordon.
- A value with no `@<holder>` was written by an older `stratum`. The run reads
  it by its kind alone.
- A run that takes over the lease from a halted run takes over that run's
  claims, even if the halted run targeted a different version.
- A run never finishes a node whose claim names another run.
- If you cordon a node while the run has it cordoned, `kubectl cordon` changes
  nothing, so the run still uncordons the node. Cordon it again after the node
  finishes.
- Each cordon and uncordon write is checked against the version of the Node the
  run read. If something else changed the Node, the run reads it again and
  decides again.
- A node at `--target` that already runs the build can still carry a cordon
  from a halted run. If that cordon is the upgrade's own, the run uncordons the
  node and removes the annotation. The cordon is the upgrade's own when the kind
  is a valid version, or when there is no claim and the node has an
  `upgrade-in-progress` annotation. A run started by an older `stratum` does not
  record whether the node was already cordoned, so a node you cordoned before
  that older run also counts as the upgrade's own and is uncordoned. A claim left on a node that is no longer
  cordoned is marked `released`.

### Uncordoning after a halted run

When a controller's upgrade Job passes its last health gate, it uncordons its
own node if the node is cordoned and the cluster upgrade owns that cordon. It
then sets `upgrade-cordoned` to `released@<holder>` and logs:

```text
stratum upgrade: uncordoned <node> (the cluster upgrade that cordoned it may have halted)
```

- The `upgrade-in-progress` annotation stays on the node until you re-run the
  cluster upgrade.
- Workers do not uncordon themselves, because upgrading a worker does not
  restart the API server.
- A run started by an older `stratum` records no `upgrade-cordoned` annotation.
  The Job then treats the cordon as the upgrade's own when `upgrade-in-progress`
  names the target. That older run does not record whether the node was already
  cordoned, so the Job also uncordons a node you cordoned before the run. To
  keep a cordon you set, start the run from the upgrade binary.
- If the Job cannot reach the API server within 4 minutes, the upgrade still
  succeeds and the Job logs this warning:

    ```text
      WARN: could not check whether <node> is still cordoned: <error>; if the cluster upgrade cordoned it (you did not cordon it yourself before the upgrade), run: /usr/local/bin/stratum kubectl uncordon <node>
    ```

    Run the command with `sudo`, and only if the cluster upgrade cordoned the
    node. It uses the full path because `sudo` may not search `/usr/local/bin`.
    On Rocky Linux 9, for example, `sudo stratum` fails with `command not found`.

## The node health gate

After the host binaries are swapped, the upgrade waits for the node to pass a
health gate. `stratum rollback` uses the same gate. If the gate fails during an
upgrade, the node rolls back on its own. The gate passes only when all of these
hold:

- every host process is running under the supervisor, and none is
  [`Foreign`](../runtime/index.md#a-containerd-stratum-did-not-start)
- the containerd answering the container runtime interface (CRI) socket
  reports the version of the containerd installed on disk
- on a controller, the API server is healthy
- the kubelet version in the node's `nodeInfo` matches the kubelet installed on
  disk

| Timing | Value |
|---|---|
| each version check, in an upgrade | 30 seconds |
| the whole gate, in `stratum rollback` | 60 seconds, shared by every check, including the wait for host processes |

The gate reads `nodeInfo` with the node's own kubelet credential, so workers
are checked the same way as controllers.

## The supervisor restart

Replacing `/usr/local/bin/stratum` does not replace the running `stratum up`,
the supervisor that starts containerd, the kubelet and the operator. The last
step of a successful upgrade, and of `stratum rollback`, restarts the
supervisor on the installed `stratum` and runs the health gate again. Running
containers keep running. Restarting the supervisor restarts the operator with
everything else it runs; when the supervisor is not restarted, the operator is
restarted on its own after this step.

- A supervisor that supports it restarts itself in place and keeps its process
  ID.
- An older supervisor is restarted with `systemctl restart stratum` when systemd
  runs it. When you started it by hand, the upgrade warns and leaves it
  running. Stop it and start it again without the join flags:
  `sudo /usr/local/bin/stratum up --worker` on a worker, or `sudo /usr/local/bin/stratum up --config <path>`
  on a controller, adding `--profile <name>` if it runs a profile.
- If systemd cannot say whether it runs the supervisor, or the unit's
  `ExecStart` runs a binary other than `/usr/local/bin/stratum`, the upgrade
  reports an error and leaves the supervisor running. When the run changed the
  node, a controller's operator is still restarted on the installed build.
- A rerun of `stratum upgrade` on a node already at its target still restarts a
  supervisor that does not run the installed `stratum`. `stratum upgrade --check`
  reports such a node as needing the upgrade.
- The exception is an older supervisor you started by hand. The first upgrade
  or rollback that warns about it records that process. After that, a rerun on
  that node, its `--check` and its `--dry-run` repeat the warning and change
  nothing, and `--check` reports the node as current. A rerun of
  `stratum upgrade --cluster` shows the warning under the node's name and does
  not drain the node. Restarting the supervisor as described above ends the
  warning.
- If the supervisor does not come back on the installed `stratum` with every
  process running within 3 minutes, or the gate after it fails, the upgrade
  reports it and keeps the installed build. It does not roll back.

`sudo /usr/local/bin/stratum version --components` and the MCP `cluster_status` tool report a
supervisor that does not run the installed `stratum`. Both need root to ask the
supervisor.

## What an upgrade leaves behind

| Path | Contents |
|---|---|
| `<file>.bak-<stamp>` | the file an upgrade replaced; `<stamp>` is UTC, `YYYYMMDD-HHMMSS` |
| `/usr/local/bin/stratum.bak-<stamp>` | the previous `stratum` binary |
| `/var/lib/stratum/bin/*.bak-<stamp>` | the previous host binaries |
| `/var/lib/stratum/upgrade/manifests-bak-<stamp>` | the previous control-plane manifests |
| `/var/lib/stratum/upgrade/in-progress.json` | the running upgrade's backup stamp and target, from its first change until the health gate passes |
| `/var/backups/etcd/pre-upgrade-<stamp>.db` | the etcd snapshot a control plane takes before it changes anything, unless `--skip-snapshot` is set |

- All backups with the same stamp form one *backup set*. `stratum rollback`
  restores the newest set. After each successful upgrade, only the two newest
  sets are kept.
- A node with `in-progress.json` had an upgrade interrupted. `stratum upgrade`
  to the same target finishes it, and `stratum rollback` undoes it. If the file
  cannot be read, both commands refuse and name it. Check the `*.bak-<stamp>`
  backups, then delete the file by hand.
- The upgrade removes `in-progress.json` before it rewrites the host-process
  configs and arguments. A failure in that last step leaves no file. A rerun
  then treats the node as at the target, and `stratum rollback` does not restore
  those configs. Check the `stratum upgrade` output for an error after the
  health gate.

## etcd storage version during a rollback

An upgrade can move etcd to a newer minor version. The newer etcd moves its data
to its own storage version, and the older etcd refuses to start on that data.
Before it restores the older etcd, a rollback compares etcd's storage version
with the version it restores. The automatic rollback after a failed upgrade does
the same.

| Situation | What the rollback does |
|---|---|
| storage version not above the restored etcd's | goes ahead as usual |
| single control plane, newer storage version | runs etcd's own downgrade and waits up to 2 minutes for the storage version to come down, then restores; writes since the upgrade are kept; `stratum rollback --dry-run` shows the downgrade it would run |
| more than one control plane, newer storage version | refuses and changes nothing; the error names the version to downgrade to, as `<major>.<minor>` |
| etcd does not answer | stops etcd and reads the storage version from disk, keeping etcd's manifest at `/var/lib/stratum/upgrade/etcd-manifest-stopped.yaml` while it is stopped |

When etcd does not answer and its data is newer, or cannot be read, the rollback
puts etcd's manifest back and refuses. Only a running etcd can downgrade its
data, so get etcd answering on its current version and run the rollback again.
If the rollback fails while etcd is stopped, its error names the kept manifest,
and running `stratum rollback` again uses it.

## Checks for clusters on an earlier build

Each section below applies only when you upgrade from a build that predates the
behaviour it names.

### HA clusters with custom CIDRs

This applies to a high-availability (HA) cluster that uses a custom
`spec.network.podCIDR` or `serviceCIDR`, when its control planes run a build
from before the recorded CIDRs took precedence. On those builds, a control plane
that joined with `--join` could take the default CIDRs on a restart. The new
build keeps whatever a node has recorded, so an upgrade would keep the defaults
on that node. See
[Network configuration](../networking-storage/index.md#network-configuration)
for how the CIDRs are recorded.

1. Before you change any node, work out the cluster's pod and service CIDRs.
   Any controller on the affected builds could have recorded the defaults, so
   cross-check these sources:

    - `spec.network` in the `config.yaml` the cluster was first installed
      with, if you still have it.
    - The `kubernetes` Service's ClusterIP, the first address of the service
      CIDR:

        ```console
        $ kubectl get svc -n default kubernetes -o jsonpath='{.spec.clusterIPs}{"\n"}'
        ```

    - Each node's `.spec.podCIDRs`, which are inside the pod CIDR:

        ```console
        $ kubectl get nodes -o custom-columns=NAME:.metadata.name,PODCIDRS:.spec.podCIDRs
        ```

    - The CIDR flags on every control plane, from step 2, and each control
      plane's `/var/lib/stratum/pki/netconfig`. The file's first line is the
      pod CIDR and its second line is the service CIDR. A control plane
      that shows different ranges from the other sources has drifted, and
      step 3 fixes it.

    If the sources other than the control planes do not agree on one pod CIDR
    and one service CIDR, stop. See the last paragraph of this section.

2. On every control plane, check that both CIDR flags match the CIDRs from
   step 1:

    ```console
    # grep -h -e --service-cluster-ip-range -e --cluster-cidr \
      /var/lib/stratum/config/manifests/kube-apiserver.yaml \
      /var/lib/stratum/config/manifests/kube-controller-manager.yaml
    ```

3. On each control plane whose flags differ, set both CIDRs from step 1 in
   its `/etc/stratum/config.yaml`, or the file its unit passes with
   `--config`. The older build applies `spec.network` on every start, so
   copying `netconfig` alone does not survive a restart:

    ```yaml
    spec:
      network:
        podCIDR: <cluster pod CIDR>
        serviceCIDR: <cluster service CIDR>
    ```

    Restart Stratum with `sudo systemctl restart stratum`, and repeat step 2
    on that node. After the upgrade these values match the recorded CIDRs, so
    you can leave them in place. They produce no warning.

4. Check that every address in each Service's `.spec.clusterIPs` is inside
   the service CIDR of its IP family. Skip `ExternalName` Services and
   headless Services, whose cluster IP is `None`:

    ```console
    $ kubectl get svc -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,TYPE:.spec.type,IPS:.spec.clusterIPs
    ```

5. On every node, check that the kubelet's `clusterDNS` is the `kube-dns`
   Service's ClusterIP:

    ```console
    # grep clusterDNS /var/lib/stratum/config/kubelet.conf
    ```

6. On every node, pick an existing pod whose image has `nslookup`. It must not
   use the host network, must have `dnsPolicy: ClusterFirst`, and must have no
   custom `dnsConfig`. For a suitable pod this prints `ClusterFirst` (or
   `false ClusterFirst`) and nothing after it:

    ```console
    $ kubectl get pod -n <namespace> <pod> -o jsonpath='{.spec.hostNetwork} {.spec.dnsPolicy} {.spec.dnsConfig}{"\n"}'
    ```

    Check that its resolver is the `kube-dns` ClusterIP and that a lookup
    succeeds:

    ```console
    $ kubectl exec -n <namespace> <pod> -- cat /etc/resolv.conf

    $ kubectl exec -n <namespace> <pod> -- nslookup kubernetes.default
    ```

If the sources in step 1 disagree, or any check still fails after step 3, on a
control plane or a worker, stop. The cluster needs a separate recovery before
you upgrade it, and this documentation does not cover that recovery.

### Clusters whose config sets `clusterDomain`

This applies when you upgrade from a build that predates custom-domain support.
Those builds accepted `spec.network.clusterDomain` and ignored it, so every
cluster they initialized runs on `cluster.local`, whatever its config file says.

!!! warning "A mismatched clusterDomain stops the control plane from starting"
    After the upgrade, `stratum up` refuses to start a control plane whose
    config file sets a domain other than the one the cluster runs on. It fails
    with:

    `spec.network.clusterDomain "corp.internal" does not match this cluster's domain "cluster.local" recorded in /var/lib/stratum/pki/netconfig: the cluster domain cannot be changed after init; remove the field or set it to "cluster.local"`

    A CIDR that differs from the recorded one only produces a warning. A
    different domain is refused.

Before you upgrade, check the config file on every control plane (by default
`/etc/stratum/config.yaml`). If it sets `clusterDomain` to anything other than
`cluster.local`, remove the line or set it to `cluster.local`. The cluster keeps
running on `cluster.local` either way, because an existing cluster cannot change
its domain. Workers need no change: a worker takes the domain from the
`--cluster-domain` join flag, never from its config file.

A cluster initialized with a non-default `clusterDomain` cannot be rolled back
to a release that predates custom-domain support, because that release cannot
read or keep the domain the cluster records. A cluster on `cluster.local` rolls
back normally.

### Images loaded onto a node by hand

This applies when you upgrade from a build that predates the kubelet
pull-credential policy. Those builds left the kubelet on upstream's default,
which never checks an image the kubelet did not pull. The new build checks every
image outside the payload registry's namespace, as described in
[Payload images and kubelet's pull-credential check](../runtime/index.md#payload-images-and-kubelets-pull-credential-check).

After the upgrade, a pod whose image you loaded by hand, for example with
`ctr images import`, fails the next time its container starts on an airgapped
node. It fails with `ErrImagePull` if its `imagePullPolicy` is `IfNotPresent`, or
`ErrImageNeverPull` if it is `Never`. Before you upgrade, push those images to a
registry the nodes can reach, through a
[registry mirror](../runtime/index.md#registry-mirrors) if they are airgapped.

### SELinux confinement

This applies when you upgrade a cluster on Rocky Linux, RHEL or RLC from a
build that predates SELinux labelling. On a node whose host runs SELinux and has
the policy packages (see
[Required packages](../installation/index.md#required-packages)), the first
start of the new build does two things:

1. It registers Stratum's SELinux file contexts and relabels
   `/var/lib/stratum`, `/run/stratum` and `/opt/local-path-provisioner` once.
   On a node with many images this takes a while. It runs before containerd and
   the kubelet start again, within the 3 minutes the
   [supervisor restart](#the-supervisor-restart) allows.
2. It turns on containerd's SELinux labelling and restarts containerd. Running
   containers keep running, with the labels they started with.

From then on, every new pod runs as `container_t` with its own categories,
unless it is privileged (see [Containers](../installation/configuration.md#containers)).
A container process started before then shows a label without categories, such
as `system_u:system_r:spc_t:s0`, in `ps -eZ`.

#### Pods that mount a hostPath

A non-privileged pod that mounts a `hostPath` it did not get from Stratum is
denied it the next time it is recreated, unless the directory is labelled
`container_file_t`. Find such pods before you upgrade:

```console
$ kubectl get pods -A -o json | jq -r '.items[] | select([.spec.containers[].securityContext.privileged] | any | not) | select([.spec.volumes[]?.hostPath] | any) | .metadata.namespace + "/" + .metadata.name'
```

Label their directories, or set `spec.selinux: false` on the nodes that run
them, before you upgrade.

#### SELinux prerequisites

Before it drains any node, `upgrade --cluster` checks every Ready node that
still needs this build. It refuses to start, naming the nodes, when a node is
SELinux enforcing, or its profile requires SELinux, and one of these packages is
missing: container-selinux, policycoreutils, policycoreutils-python-utils or
libselinux-utils. Without them, Stratum's runtime and every container on the
node run unconfined. A profile that requires SELinux combined with
`spec.selinux: false` is refused too.

- Install the missing packages on the named nodes, or set `spec.selinux: false`
  in the node's config file, and re-run. A node whose profile requires SELinux
  has no opt-out. The refusal changes nothing.
- A permissive node without the packages is not refused. It is warned when it
  next starts.
- A NotReady node is not checked up front. If it needs this build and lacks the
  packages, it is cordoned and drained before its own upgrade refuses, and it
  stays cordoned. Bring it back, or check it by hand, before the run.
- The installed `stratum` that starts the rollout runs the check up front, and
  the staged `--from` binary runs it on each node. An older installed `stratum`
  runs no check up front, so a node is refused only at its own turn. An older
  staged binary cannot refuse at all.
- To find out before you start, run `sudo <from> upgrade --from <from> --check`
  on each node, with a `--from` from a release that has the check. Exit code
  `11` means the node is refused.
- `--dry-run` with `--cluster` does not run the check. On one node,
  `sudo /usr/local/bin/stratum upgrade --from <binary> --dry-run` does run it, and a node that
  needs the upgrade and fails it exits 11 instead of printing the plan.

#### Recreating pods that still run unconfined

Pods that were already running stay unconfined until they are recreated. The
drain skips DaemonSet and static pods, so those are the ones left behind. After
any run that upgraded a node, even one that then stops with an error,
`upgrade --cluster` prints the full steps to
recreate them, with exact commands, and runs none of them.

Run them only once every node runs the new build. A pod recreated on a node
still on the old build starts unconfined, and when that node is upgraded later
the drain skips it again, so it stays unconfined. After a run that stopped with
an error, finish the upgrade first. In outline:

1. Restart Stratum's DaemonSets and your own with `kubectl rollout restart`,
   and wait on each with `kubectl rollout status`.
2. A DaemonSet with the `OnDelete` update strategy is not recreated by a
   restart, and `rollout status` refuses it. Delete its pods one at a time
   instead, and wait for each replacement to be Ready.
3. Reboot each control-plane node, one at a time, to recreate its static pods.
   Start only when every control-plane node is upgraded and Ready, and
   `sudo /usr/local/bin/stratum etcdctl endpoint health --cluster` reports every
   member healthy. Recover a control plane the upgrade halted on first.
4. Before you drain a control plane that runs a CoreDNS replica, check that
   another replica, not Terminating, is Running and Ready on another Ready node:
   `sudo /usr/local/bin/stratum kubectl -n kube-system get pods -l k8s-app=kube-dns -o wide`.
   If none is, get one serving there first.
   On a single-node cluster, cluster DNS is down from the drain until you
   uncordon the node.
5. Drain the node with `kubectl drain <node> --ignore-daemonsets --delete-emptydir-data`,
   reboot it, and wait for it to be Ready.
6. On that node, wait until `sudo /usr/local/bin/stratum etcdctl endpoint health --cluster`
   reports every member healthy and
   `sudo /usr/local/bin/stratum kubectl --server https://127.0.0.1:6443 get --raw /readyz`
   prints `ok`. Ready alone is not enough, because the kubelet can report
   through another API server while the node's own etcd is down.
7. Uncordon the node, unless you cordoned it before the drain, and go on to the
   next control plane.

#### Rolling back across SELinux labelling

`stratum rollback` to a build whose control-plane manifests predate running as
`spc_t` turns off containerd's SELinux labelling before it restores them, so the
restored control plane can still read its own data. Containers it starts are
then unconfined. Upgrading to such a build with `stratum upgrade --from` fails at
the etcd gate and rolls back. To go back that far, set `spec.selinux: false` and
restart Stratum first.

### Nodes that finalized a CA rotation on an earlier build

This applies to a cluster that ran `stratum certs rotate-ca` on a build that
verified kubelet serving certificates only under a hardened profile. That
rotation did not renew bootstrap-issued kubelet serving certificates. A worker,
or a control plane whose profile bootstraps its serving certificate, can still
serve one signed by the certificate authority (CA) the rotation dropped. The new
build's API servers verify kubelet serving certificates in every profile, so
`kubectl logs`, `exec` and `port-forward` to such a node fail from the first
control-plane upgrade until that node is upgraded.

`upgrade --cluster` probes every kubelet before it starts, `--dry-run`
included. It names the nodes whose serving certificate the cluster CA does not
sign, and any node it could not check. It warns rather than refuses, because the
upgrade repairs them: when an upgraded node's kubelet starts, it moves the
untrusted certificate aside and requests a new one, which the operator approves.
A node whose supervisor the upgrade does not restart keeps the old certificate
until Stratum next restarts there (`sudo systemctl restart stratum`).

### Clusters whose certificates carry system:masters

Releases before the `admin.conf` and `super-admin.conf` split issued `admin`,
`kube-vip` and `apiserver-kubelet-client` in `system:masters`. After the
upgrade, each control plane's first start of the new release creates the
bindings the new certificates need. It then reissues every leaf and restarts its
control-plane components, one control plane at a time, once the cluster upgrade
has released the maintenance lease. Expect one more restart of each control
plane's components shortly after the upgrade finishes.

Copies of `admin.conf` taken off a control plane before the upgrade keep
`system:masters` until they expire. Replace them with copies of the new
`admin.conf`, and rotate the cluster CA if the old ones must stop working
sooner. See
[Upgraded clusters and system:masters](../cluster-admin/hardening.md#upgraded-clusters-and-systemmasters).

### HA join tokens and the shared certs

This applies when you upgrade an HA cluster from a build that gave every join
token read access to `stratum-certs`, the encrypted shared CA material. The new
build lets only control-plane tokens read it.

- During the upgrade, mint control-plane tokens only on an upgraded controller.
  A token minted by the earlier release is not in the control-plane bootstrap
  group. Its join fails with `Forbidden` once an upgraded controller has
  narrowed the grant, and a controller joining with this release refuses it at
  its etcd member-add before then.
- While `stratum upgrade --cluster` runs, do not mint control-plane tokens or
  join controllers. The rollout holds the cluster maintenance lease for its
  whole run, so `token create --control-plane` and `certs rotate --rekey-shared`
  refuse and name it as the holder.

After the last controller is upgraded:

1. Narrow the grant by minting any token on an upgraded controller. Until then,
   worker tokens can still read an existing `stratum-certs`. The next real join
   token does this, and so does a short-lived worker token you never hand out:
   `sudo /usr/local/bin/stratum token create --ttl 1h`.
2. Check that a worker token is refused (`no`) and a control-plane token is not
   (`yes`). The second check prints `no` on a cluster that has never uploaded
   the shared certs, because no grant exists yet.

    ```console
    # /usr/local/bin/stratum kubectl auth can-i get secret/stratum-certs -n kube-system --as=system:bootstrap:probe --as-group=system:bootstrappers --as-group=system:bootstrappers:stratum:nodes

    # /usr/local/bin/stratum kubectl auth can-i get secret/stratum-certs -n kube-system --as=system:bootstrap:probe --as-group=system:bootstrappers --as-group=system:bootstrappers:stratum:nodes --as-group=system:bootstrappers:stratum:control-planes
    ```

3. If `sudo /usr/local/bin/stratum kubectl -n kube-system get secret stratum-certs` finds the
   Secret, re-key it with `sudo /usr/local/bin/stratum certs rotate --rekey-shared --yes`, so a
   copy read after the re-key is useless with the old key. Do this before you hand
   out a new certificate-key file. Re-keying invalidates any key file already
   copied, so a join in progress needs a fresh `--control-plane` token. Skip
   this step when there is no Secret, because re-keying would create one with
   no owner that is never garbage-collected.

These steps do not protect ciphertext a worker token already read together with
the certificate key that decrypts it. If you do not trust the worker tokens and
nodes from before the upgrade, rotate the CA with
`sudo /usr/local/bin/stratum certs rotate-ca --cluster --yes`. That replaces the cluster CA,
the front-proxy CA and the ServiceAccount signing key. It does not replace the
etcd CA key, or the secrets a profile shares between control planes, such as an
at-rest encryption key, which the same Secret carries. Stratum has no rotation
for those, so a cluster whose shared certs and certificate key may both have
leaked should treat those keys as compromised.

#### Rolling back join token access

`stratum rollback` does not restore the old grant, and the
earlier release does not widen it again. Its control-plane tokens are not in the
control-plane bootstrap group, so their joins fail with `Forbidden`. Once every
controller runs the earlier release, delete the binding. The next
`stratum token create --control-plane` recreates it the way that release
expects, which gives worker tokens read access to `stratum-certs` again:

```console
# /usr/local/bin/stratum kubectl -n kube-system delete rolebinding stratum:certs-reader
```

### Node client certificate approval

Earlier releases bound `stratum-csr-autoapprove` to every worker join token, so
the controller-manager approved any node client certificate a token asked for,
including one naming a node that already existed. This release keeps that
binding with no subjects. The operator approves a worker's first client
certificate only for a node name no other node holds. Every upgraded controller
empties the binding when it starts, whether or not it runs an operator.

During the upgrade, mint join tokens on an upgraded controller, and join new
workers once the operator's leader is upgraded. Until then a worker's
certificate request stays pending. Approve it by hand as
[Installation](../installation/index.md) describes.

#### Rolling back node certificate approval

`stratum rollback` leaves the binding empty, and the earlier
release does not grant it again, because it treats an existing binding as
already set up. New workers then wait for you to approve their certificate
requests by hand. To return to the earlier behaviour, restore the subject.

!!! warning "Restoring the subject lets any worker token claim any node name"
    With the subject restored, every worker join token can claim any node
    name again, including an existing node's.

```console
# /usr/local/bin/stratum kubectl patch clusterrolebinding stratum-csr-autoapprove --type=json -p '[{"op":"add","path":"/subjects","value":[{"kind":"Group","apiGroup":"rbac.authorization.k8s.io","name":"system:bootstrappers:stratum:nodes"}]}]'
```

### HelmCharts that take values from Secrets

This release adds `spec.valuesSecrets` to `HelmChart` (see
[Values from Secrets](../cluster-admin/hardening.md#values-from-secrets)).
Earlier releases do not know the field. Their operator does not read the
Secrets, so it upgrades such a chart without their values, and their CRD does
not declare the field, so the API server drops it.

- During the upgrade, an operator leader still on the earlier release does
  this to every `HelmChart` that names `spec.valuesSecrets`. Add the field only
  once every control plane runs this release.
- Rolling back below this release does the same, and rolling forward again
  does not bring the field back. Before you roll back, move the values a chart
  must keep into `spec.valuesContent` or `spec.set`, where anyone who can read
  the `HelmChart` can read them, or reapply `spec.valuesSecrets` after rolling
  forward.
