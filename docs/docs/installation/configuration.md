---
description: "The node configuration file, how a node picks its IP address, reusing a node name, the systemd unit and its environment, and SELinux labelling."
---

# Configuration

This page covers the settings a node reads beyond a first install: the
configuration file `/etc/stratum/config.yaml`, how a node picks its IP address,
how to reuse a node name, the systemd unit that restarts it after a reboot, and
SELinux labelling. Use it when the defaults on [Installation](index.md) do not fit your hosts or network.
Every key and its default is listed under
[Configuration keys](../appendices/paths-and-names.md#configuration-keys).

## The configuration file

Everything declarative lives in one strictly parsed file:

```yaml
# /etc/stratum/config.yaml
spec:
  # profile: <name>          # a node profile the binary registers; none by default
  operator: true
  selinux: true               # the default; false skips SELinux labelling, see SELinux
  network:
    nodeIP: 192.168.1.21        # this node's cluster address; autodetected when unset, see Node IP
    podCIDR: 10.245.0.0/16      # the default
    serviceCIDR: 10.96.0.0/12   # the default
    clusterDomain: cluster.local  # the default; fixed once the cluster is initialized
    cilium:                     # optional; the same on every control plane, see Cilium settings
      mtu: 9000
  vip:                        # the virtual IP (VIP) of an HA control plane; see High availability
    address: 192.168.1.10
```

`stratum up` reads the file at the default path, or the one you pass with
`--config`. It parses the file strictly: a misspelled key is an error, and
`stratum up` does not start until you fix it.

A node's role is not configuration. You choose it when you bring the node up,
with `stratum up` for a control plane and `stratum up --worker` for a worker.

!!! note "`spec.operator: false` and a profile that requires the operator are incompatible"
    A profile can make every kubelet serving certificate bootstrap-issued, and
    the operator approves those certificates. With the operator off, no kubelet
    gets a serving certificate, and `kubectl logs`, `exec` and `top` stop
    working cluster-wide. `stratum up` refuses this combination at startup.
    Without such a profile, the operator still approves workers' serving
    certificates; see
    [Disable the operator](../cluster-admin/hardening.md#disable-the-operator-and-remove-its-admission-webhook).

### On a worker

A worker join (`stratum up --worker --server ...`) reads only
`spec.network.nodeIP` and `spec.selinux` from the file. If the file sets any
other field, the join refuses to start and names the field, even when
`--node-ip` is given. On a worker, the profile and the network ranges come
from the `--profile`, `--pod-cidr`, `--service-cidr` and `--cluster-domain`
flags instead. `spec.operator` has no flag and is refused.

A key that is present counts as set, even when its value is `false`, `0` or
`""`, so `operator: false` is refused. A key with no value (`operator:`) and an
empty mapping (`vip: {}`) set nothing and are accepted.

Do not copy a control plane's file to a worker. Give the worker a file that sets
only its own address:

```yaml
# /etc/stratum/config.yaml on a worker
spec:
  network:
    nodeIP: 192.168.1.31
```

A worker restart (`stratum up --worker` without `--server`, which is what
`stratum.service` runs after a reboot) reads only `spec.selinux` from the file.
It keeps the saved setting when the file does not set it or cannot be read.

## Node IP

A node takes its IP from the first of these that is set:

1. `--node-ip`
2. `spec.network.nodeIP`
3. `$NODE_IP`
4. the source address of the host's default route

Stratum finds the default route's source address without sending a packet, so
the route needs no internet behind it. With no default route, `stratum up` uses
the host's global unicast IPv4 address if there is exactly one. Loopback,
link-local and IPv6 addresses do not count. With none, or more than one,
`stratum up` stops and asks you to set the address. Every start reads
`--node-ip` and `$NODE_IP`, and every start except a worker restart also reads
`spec.network.nodeIP`:

```console
# NODE_IP=192.168.1.21 stratum up
```

On a host with more than one network interface, set the address explicitly. The
default route may be on a network the other nodes cannot reach.

A worker join checks the address before it prepares the host or contacts the
cluster. The address must be one IPv4 address, or a `v4,v6` pair with the IPv4
address first. The join refuses anything else, and also refuses a loopback,
all-zeros, multicast or link-local address, which the kubelet never registers
as a node IP, and the broadcast address (`255.255.255.255`), which names every
host on the link rather than one node.

Every start logs the node IP and where the address came from.

### The saved address

A node saves its IP in its [NodeState](../appendices/glossary.md), the node's
saved state in `/var/lib/stratum/config/node.yaml`, at its first start, and keeps that address
on every later start. Editing `spec.network.nodeIP` or `$NODE_IP` afterwards has
no effect, so a rebooted node comes back on the address its certificates and
etcd membership name.

To move a worker to a new address, stop it, then start it with `--node-ip`.
`stratum up` refuses while the worker's supervisor runs, before it saves the
address. A worker that `stratum.service` started, as after a reboot, stops with
`systemctl`; one you started by hand stops when you press Ctrl+C in its
terminal:

```console
# systemctl stop stratum

# /usr/local/bin/stratum up --worker --node-ip 192.168.1.32
```

The worker saves the new address, so the bare `stratum up --worker` that
`stratum.service` runs after a reboot keeps it. The restart checks the address
as a join does, and against the worker's stack. A single-stack worker takes
only one IPv4 address. A dual-stack worker, one that joined with `--dual-stack`
or with a `v4,v6` pair, takes only a `v4,v6` pair with the IPv4 address first.
A refused restart stops without starting the worker and saves nothing. A worker
that joins again (`stratum up --worker --server ...`) picks its address afresh
from the list above.

A control plane's address cannot change in place. `--node-ip` outranks the
saved address on a control plane, but `stratum up` then refuses to start etcd
on an address its cluster does not know. To move a control plane in a
high-availability (HA) cluster:

1. From a remaining control plane, find this node's etcd member with
   `stratum etcdctl member list` and remove it with
   `stratum etcdctl member remove <member-id>`.
2. On this node, run `sudo /usr/local/bin/stratum uninstall --yes`. Without `--yes` it only
   lists what it would remove and keeps the node's old state, certificates and
   etcd data.
3. Join the node again with its new IP.

To recover a failed or interrupted join on the same node and address, run
`stratum up --join` again; see
[When a join is interrupted](../cluster-admin/high-availability.md#when-a-join-is-interrupted).

## Reusing a node name

The Stratum operator approves a worker's client certificate signing request
(CSR) only when no other node holds that node name. A name stays claimed after
its Node or CSR is deleted. A machine rebuilt under a name that was used before
therefore waits for you, even after `stratum uninstall` removed its old Node.
[If a worker's certificate request stays pending](index.md#if-a-workers-certificate-request-stays-pending)
shows how to list and inspect the request.

Give the rebuilt machine a new name when you can. To reuse the name, first make
sure the old node's credential can no longer authenticate. Then list the name
claims, delete the claim for that name and approve the request by hand:

```console
# /usr/local/bin/stratum kubectl -n kube-system get configmaps -l stratum.ciq.com/node-client-claim=true -o custom-columns=CLAIM:.metadata.name,NODE:.data.nodeName,OWNER:.data.csrName

# /usr/local/bin/stratum kubectl -n kube-system delete configmap <claim>

# /usr/local/bin/stratum kubectl certificate approve <csr>
```

When every control plane runs with `spec.operator: false`, nothing approves
these requests. Check each request and its node name, then approve it by hand
with `stratum kubectl certificate approve <csr>`.

## Network ranges and cluster domain

The pod and service CIDRs are fixed when the cluster is created. The cluster's
first control plane reads `spec.network.podCIDR` and `serviceCIDR` at its first
start and records them in `/var/lib/stratum/pki/netconfig`. A joining control
plane inherits the recorded ranges and checks its config against them. If
either field names a different range, the join stops and names the field,
your value and the cluster's. A fresh join stops before it issues a certificate
or touches etcd. A re-run that resumes an interrupted join is refused the
same way, although by then it has issued its certificates and may already
have added its etcd member. Remove the field or set it to the recorded range,
then run the join again.

On every later start a node uses its recorded ranges. If the file names
different ones, the node ignores them and logs a warning that changing them
needs a cluster rebuild. The `--pod-cidr`, `--service-cidr` and
`--cluster-domain` flags belong only to `--worker`; a control plane given one
stops and asks you to set the corresponding field in `spec.network` instead.
See [Network configuration](../networking-storage/index.md#network-configuration).

The cluster domain is recorded the same way. A file that names a different
domain is refused: `stratum up` does not start the control plane until you
remove the field or set it to the recorded domain. See
[Cluster domain](../networking-storage/index.md#cluster-domain).

## The systemd unit

On a systemd host, `stratum up` installs and enables
`/etc/systemd/system/stratum.service` every time it starts. The unit runs the
node's steady-state command, not the command you typed:

| Node | `ExecStart` |
|---|---|
| control plane | `stratum up --config <path>`, plus `--profile <name>` if the node runs a profile; a relative `--config` is saved as an absolute path |
| worker | `stratum up --worker`, which resumes from the saved NodeState |

The unit never holds the join flags `--join`, `--server`, `--token`,
`--token-file`, `--discovery-token-ca-cert-hash`, `--certificate-key` and
`--certificate-key-file`. They are single-use credentials, and a reboot never
joins again.

`stratum up` reinstalls and re-enables the unit whenever its command changes or
it is not enabled, so a failed `systemctl enable` is retried on the next start.
If it cannot install the unit, the node still starts and prints a warning. On a
host without systemd, Stratum skips the unit, and you restart the node after a
reboot yourself.

If the unit is masked, `stratum up` does not touch it. It warns and starts the
node without installing or enabling the unit. To restore reboot persistence,
unmask the unit the way it was masked, then run `stratum up` again. For a unit
masked with `systemctl mask`:

```console
# systemctl unmask stratum
```

For a unit masked with `systemctl mask --runtime`:

```console
# systemctl unmask --runtime stratum
```

To keep a node from starting at boot without uninstalling it, disable the unit:

```console
# systemctl disable --now stratum
```

`--now` also stops the node, but only when it runs as `stratum.service`, for
example after a reboot. A first `stratum up` you started by hand keeps running.

### Environment for the unit

A process systemd starts does not inherit your shell's environment. To give the
node environment variables across a systemd-managed restart, write them to
`/etc/stratum/stratum.env`. The unit reads it with:

```ini
EnvironmentFile=-/etc/stratum/stratum.env
```

The leading `-` makes the file optional, and Stratum never creates it. Its main
use is S3 credentials for scheduled snapshot uploads: `stratum snapshot
schedule` never writes environment credentials to disk, so unless
`snapshot.conf` holds them, the node needs them in its environment. See
[Snapshot & Restore](../cluster-admin/snapshot-restore.md#credentials-for-remote-storage).

The file holds secrets. Create it owned by root with mode `0600`:

```console
# install -m 0600 -o root -g root /dev/null /etc/stratum/stratum.env

$ sudoedit /etc/stratum/stratum.env
```

Write one `KEY=value` per line. systemd reads the file only when it starts the
unit, so a change takes effect on the next systemd-managed start, such as after
a reboot. A `stratum up` you start by hand uses your shell's environment
instead. `stratum uninstall` deletes the file with the rest of `/etc/stratum`.

## SELinux

On an SELinux host, Stratum labels its files on every start so that its
components run in the domains `container-selinux` defines, and each container
runs confined. The node needs the packages in
[Required packages](index.md#required-packages).

### What Stratum labels

Every `stratum up` registers these file contexts with `semanage fcontext`,
reusing the types `container-selinux` defines, and relabels the files to match:

| Path | Type |
|---|---|
| `/usr/local/bin/stratum` | `container_runtime_exec_t` |
| `/var/lib/stratum` and everything under it | `container_var_lib_t` |
| `/var/lib/stratum/bin/*` | `container_runtime_exec_t` |
| `/var/lib/stratum/dns` and everything under it, CoreDNS's upstream `resolv.conf` | `container_ro_file_t`, read-only to containers |
| `/run/stratum` and everything under it | `container_var_run_t` |
| `/etc/systemd/system/stratum.service` | `container_unit_file_t` |
| `/var/lib/stratum/containerd`, `/var/lib/stratum/kubelet`, `/run/stratum/containerd` | labelled as `/var/lib/containerd`, `/var/lib/kubelet` and `/run/containerd` are |

Stratum registers each context on the path the distribution's policy labels it
by. On Rocky Linux 9, which labels `/run` as `/var/run`, the `/run/stratum`
context is listed as `/var/run/stratum`, and the unit's as
`/usr/lib/systemd/system/stratum.*`. For the same reason, Stratum registers
copies of the policy's `/var/run/containerd` contexts under
`/var/run/stratum/containerd` there, instead of an equivalence.

Started by systemd, `stratum`, containerd, the shims and the kubelet run as
`container_runtime_t`. Stratum also creates and labels
`/opt/local-path-provisioner`, so the local-path provisioner can create volumes
there. On the start that first registers the contexts, the `stratum up` process
keeps the domain it started in. It moves to `container_runtime_t` when it next
restarts, for example at the next reboot.

The first start that registers the contexts relabels everything under
`/var/lib/stratum` and `/run/stratum`, and prints a line when it does. This
takes longer on a node with many images. The relabel stays on those
directories' filesystems, so it does not change the labels of running
containers' mounts or pod volumes. Later starts relabel only the binaries, the
unit file, the local-path directory and CoreDNS's upstream directory. Once a
node is labelled, payload extraction, `stratum upgrade` and `stratum rollback`
label each file before it replaces the old one.

Two files record the state of the relabel:

| File | Meaning |
|---|---|
| `/var/lib/stratum/selinux-relabel-pending` | a relabel started and has not finished; the next start relabels again |
| `/var/lib/stratum/selinux-labels` | the node was labelled at least once; it does not mean the latest relabel finished |

A start with SELinux disabled removes `selinux-labels`, so the next start with
it enabled relabels everything. `stratum uninstall` removes both.

Stratum records the contexts it registered in
`/var/lib/stratum/selinux-contexts`, and deletes any it no longer registers. It
never touches contexts you added for other paths. To list the contexts Stratum
added:

```console
# semanage fcontext -l -C
```

### Opting out

To run without these labels, set `spec.selinux: false` in the node's
`/etc/stratum/config.yaml` and restart Stratum. A worker reads the setting on
every start too. Stratum then registers no contexts, relabels nothing and labels
no file it writes. A profile that requires SELinux refuses `spec.selinux: false`.

A worker keeps a saved opt-out while the file does not set `spec.selinux`, so
removing the line does not end it. To turn labelling back on, set
`spec.selinux: true` and restart Stratum.

Opting out does not remove the contexts an earlier start registered.
`/var/lib/stratum/selinux-contexts` lists them, one per line: a file context as
`SPEC FILE-TYPE TYPE`, an equivalence as `TARGET = SOURCE`. Lines that start
with `policy` record the policy's own contexts; leave them. Delete each file
context, then each equivalence, then relabel. For example, on Rocky Linux 9:

```console
# semanage fcontext -d -f a '/var/run/stratum(/.*)?'

# semanage fcontext -d -e /var/lib/containerd /var/lib/stratum/containerd

# restorecon -R -x /var/lib/stratum /run/stratum /usr/local/bin/stratum /etc/systemd/system/stratum.service
```

`stratum uninstall` removes Stratum's contexts for you, each only while the
policy still holds it as Stratum registered it, so a context you set for the
same path stays. It then relabels `/usr/local/bin/stratum`, which it leaves in
place, to the policy's default.

### Containers

On a labelled node, Stratum's containerd runs with `enable_selinux = true` in
`/var/lib/stratum/config/containerd.toml`, and each container gets its own
SELinux label:

| Pod | Runs as |
|---|---|
| An ordinary pod | `container_t` with its own pair of categories, such as `s0:c123,c456`, so its processes cannot reach another pod's files |
| A privileged pod, the Cilium agent, and the node Jobs Stratum runs for upgrades and CA rotation | `spc_t`, which `container-selinux` leaves unconfined |
| etcd, kube-apiserver, kube-controller-manager, kube-scheduler and kube-vip | `spc_t`: their manifests set `seLinuxOptions.type: spc_t`, as they read the node's PKI, kubeconfigs and etcd data |

A pod's `seLinuxOptions`, such as `level`, are applied.

A node is labelled once `/var/lib/stratum/selinux-labels` exists. On a node that
opted out with `spec.selinux: false`, or that lacks the policy packages,
`enable_selinux` stays off. Every container then keeps the label it inherits,
all containers on the node share it, and SELinux separates nothing. Stratum
rewrites the containerd config and restarts containerd when this changes.
Running containers keep their labels until they are recreated.

**hostPath volumes.** A `container_t` pod cannot read a host directory with a
host type, such as `usr_t` or `var_lib_t`. Label a directory that pods use
`container_file_t`, or run the pod privileged, which the `baseline` and
`restricted` Pod Security levels refuse:

```console
# semanage fcontext -a -t container_file_t '/srv/data(/.*)?'

# restorecon -R /srv/data
```

Local-path volumes are `container_file_t:s0`, with no categories, so categories
do not separate them from one another.

A `container_t` pod cannot connect to Stratum's kubelet, which runs as
`container_runtime_t`. A device plugin cannot register through
`/var/lib/kubelet/device-plugins/kubelet.sock`, and a pod cannot read the
pod-resources API at `/var/lib/stratum/kubelet/pod-resources/kubelet.sock`, as
a GPU metrics exporter does. The connect fails with `permission denied`, which
a device plugin may report only as a registration timeout before it retries.
`container_device_plugin_t`, the type `container-selinux` defines for device
plugins, is refused too. Run such a pod as `spc_t`:

```yaml
securityContext:
  seLinuxOptions:
    type: spc_t
```

`privileged: true` runs it as `spc_t` too, and some plugins need it for other
reasons. The `baseline` and `restricted` Pod Security levels refuse both, so
run such a pod in a namespace that does not enforce them, such as one labelled
`pod-security.kubernetes.io/enforce: privileged`. The audit log shows the
refusal as `denied { connectto }` on `tclass=unix_stream_socket` with
`tcontext=system_u:system_r:container_runtime_t:s0` for the device-plugin
socket, and as `denied { write }` on `tclass=sock_file` for the pod-resources
socket.

**Troubleshooting.** An absence of access vector cache (AVC) denials does not
mean containers are confined: an unconfined domain has nothing to deny. To see
the label a container runs with, replace `PROCESS_NAME` with the name of a
process running in it. To list recent denials, run `ausearch`:

```console
$ ps -eZ | grep PROCESS_NAME

# ausearch -m AVC -ts recent
```

When its standard input is a pipe, as in a command passed to `ssh`, `ausearch`
searches that input instead of the audit log and finds no denials. Add
`--input-logs` there.
