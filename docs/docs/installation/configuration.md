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
`stratum.service` runs after a reboot) reads only `spec.selinux` from the file,
and `spec.profile` and `spec.cis` only to refuse a change of profile. It keeps
the saved settings when the file does not set them or cannot be read, and warns
when it cannot read the file.

A worker keeps the profile it joined with. A restart that names a different
one, with `--profile`, `--cis`, `spec.profile` or `spec.cis`, refuses and
changes nothing, and after a reboot the worker stays down until the file and
its profile agree. To move a worker to another profile:

1. Stop the worker: `systemctl stop stratum` for a worker that
   `stratum.service` started, or Ctrl+C in the terminal of one you started by
   hand.
2. Remove `spec.profile` and `spec.cis` from its config file, since a worker
   join refuses them.
3. Run `stratum token create` on a control plane, and run the worker join line
   it prints on the worker with its `--profile` set to `<name>`. On a cluster
   that runs a profile the line already ends with `--profile`, so change its
   value; otherwise add `--profile <name>`. The join replaces the worker's
   saved state with what the line and the config file give, so also add each
   setting the worker saved in `/var/lib/stratum/config/node.yaml` that its
   config file does not set: `--node-ip` and `--dual-stack`.
   Otherwise the worker re-joins without them.

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

```bash
sudo NODE_IP=192.168.1.21 stratum up
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

```bash
sudo systemctl stop stratum
sudo /usr/local/bin/stratum up --worker --node-ip 192.168.1.32
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

```bash
sudo /usr/local/bin/stratum kubectl -n kube-system get configmaps -l stratum.ciq.com/node-client-claim=true -o custom-columns=CLAIM:.metadata.name,NODE:.data.nodeName,OWNER:.data.csrName
sudo /usr/local/bin/stratum kubectl -n kube-system delete configmap <claim>
sudo /usr/local/bin/stratum kubectl certificate approve <csr>
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
unmask the unit the way it was masked, then run `stratum up` again:

```bash
sudo systemctl unmask stratum             # masked with systemctl mask
sudo systemctl unmask --runtime stratum   # masked with systemctl mask --runtime
```

To keep a node from starting at boot without uninstalling it, disable the unit:

```bash
sudo systemctl disable --now stratum
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

```bash
sudo install -m 0600 -o root -g root /dev/null /etc/stratum/stratum.env
sudoedit /etc/stratum/stratum.env
```

Write one `KEY=value` per line. systemd reads the file only when it starts the
unit, so a change takes effect on the next systemd-managed start, such as after
a reboot. A `stratum up` you start by hand uses your shell's environment
instead. `stratum uninstall` deletes the file with the rest of `/etc/stratum`.

## SELinux

On an SELinux host, Stratum labels its files on every start so that its
components run in the domains `container-selinux` defines. Containers run
unconfined on this release (see [Containers](#containers)). The node needs the packages in
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

```bash
sudo semanage fcontext -l -C
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

```bash
sudo semanage fcontext -d -f a '/var/run/stratum(/.*)?'
sudo semanage fcontext -d -e /var/lib/containerd /var/lib/stratum/containerd
sudo restorecon -R -x /var/lib/stratum /run/stratum /usr/local/bin/stratum /etc/systemd/system/stratum.service
```

`stratum uninstall` removes Stratum's contexts for you, each only while the
policy still holds it as Stratum registered it, so a context you set for the
same path stays. It then relabels `/usr/local/bin/stratum`, which it leaves in
place, to the policy's default.

### Containers

On this release Stratum's containerd runs with `enable_selinux = false` in
`/var/lib/stratum/config/containerd.toml` on every node, labelled or not, and
containers run unconfined. Release 1.35 ships containerd 2.2, which labels a
container's stdio pipes `container_runtime_t`, so a confined container cannot
re-open `/dev/stdout` or `/dev/stderr`. nginx, and any image that logs through
a symlink to them, would crash-loop. Every container keeps the label it
inherits, all containers on the node share it, and SELinux separates nothing.
A pod's `seLinuxOptions` are accepted but not applied. Releases 1.36 and later
confine containers.

A node that a 1.35 release candidate confined has its containerd config
rewritten and containerd restarted on its next start. Running containers keep
their labels until they are recreated.

Local-path volumes are `container_file_t:s0`, with no categories, so categories
do not separate them from one another.

**Troubleshooting.** An absence of access vector cache (AVC) denials does not
mean containers are confined: an unconfined domain has nothing to deny. To see
the label a container runs with, replace `PROCESS_NAME` with the name of a
process running in it. To list recent denials, run `ausearch`:

```bash
ps -eZ | grep PROCESS_NAME
sudo ausearch -m AVC -ts recent
```

When its standard input is a pipe, as in a command passed to `ssh`, `ausearch`
searches that input instead of the audit log and finds no denials. Add
`--input-logs` there.

## AppArmor

On a host with AppArmor, such as Ubuntu, each container runs under
containerd's profile, `cri-containerd.apparmor.d`, unless its pod sets another
`appArmorProfile`. runc switches to that profile after it has dropped to the
container's capabilities. When the kernel setting
`kernel.apparmor_restrict_unprivileged_unconfined` is `1`, as on Ubuntu 26.04,
the kernel then stacks the profile on `unconfined` instead of switching to it,
unless runc itself runs under a profile. Ubuntu's `apparmor` package ships one,
named `runc`, for `/usr/bin/runc` and `/usr/sbin/runc`, but Stratum's runc is
`/var/lib/stratum/bin/runc`.

So when the setting is `1`, every `stratum up` loads a `runc` profile that also
covers `/var/lib/stratum/bin/runc`, before containerd starts. It loads it with
`apparmor_parser`, from the `apparmor` package, which is also the package that
sets the restriction:

- It is the host's `/etc/apparmor.d/runc` with `/var/lib/stratum/bin/runc`
  added to the paths it attaches to, so Ubuntu's own runc keeps the profile
  Ubuntu gives it.
- If the host has no `/etc/apparmor.d/runc`, or it is disabled in
  `/etc/apparmor.d/disable/`, the profile covers `/var/lib/stratum/bin/runc`
  alone.
- If Stratum cannot add its runc to the host's file, which needs exactly one
  `profile runc <path>` line, it loads that profile of its own instead and
  warns. The host's own runc then runs without the host's profile until the
  host loads it again.
- It then starts `/var/lib/stratum/bin/runc` stopped, before any of its code
  runs, to read the label the kernel gave it, and the start fails unless that
  label is `runc`.
- Reloading the host's profiles, as `systemctl reload apparmor` does, puts the
  host's own `runc` profile back. Stratum looks again as soon as the kernel's
  AppArmor policy changes, and every minute besides, and loads its profile
  again. It logs a failure to load it when the failure first happens
  and when it changes.
- A kernel that forbids `ptrace` altogether (`kernel.yama.ptrace_scope` set to
  `3`) does not allow reading runc's label. The start then warns that it could
  not check, and Stratum does
  not load its profile again when the host's profiles are reloaded: run
  `sudo systemctl restart stratum` after you reload them.
- A cluster upgrade checks on each node, before it changes the node, that the
  profile can be built and compiled there, and refuses the node when it cannot.
- `stratum uninstall` loads the host's own `runc` profile again, or removes the
  `runc` profile when the host loads none.

When the setting is `0`, as on Ubuntu 24.04, Stratum loads nothing. To see the
setting:

```console
$ cat /proc/sys/kernel/apparmor_restrict_unprivileged_unconfined
```

**Without the profile**, a container's processes run as
`cri-containerd.apparmor.d//&unconfined`, and containerd's profile denies
signals between them. `kill` fails with `Permission denied`, `timeout` does not
stop its command, an exec liveness probe that signals a child fails, and
programs that signal their own processes, such as PostgreSQL, stop working.
Host profiles also apply inside the container: Ubuntu's profile for
`/usr/bin/nslookup` makes `nslookup` in a container exit with code 139. The
kernel log shows the denials:

```console
# journalctl -k | grep 'apparmor="DENIED"' | grep 'operation="signal"'
```

**Running containers keep their label.** Exec sessions started after the
profile was loaded get the container's profile alone, but a process started
before keeps the stacked label until its pod is recreated or the node reboots.
That includes the control plane's static pods, which deleting the pod does not
recreate, so rebooting the node is the simplest way. This lists the processes
still stacked:

```console
# ps -eo label,pid,comm | grep '//&unconfined'
```

**Localhost profiles.** Stopping a container, runc sends its signals as `runc`.
A profile of your own that mediates signals must accept them from runc, as
containerd's profile does, or the pod cannot be stopped: its deletion hangs, and
the kernel logs `apparmor="DENIED" operation="signal" ... signal=kill
peer="runc"`. Add this rule to such a profile:

```text
signal (receive) peer=runc,
```

A profile with no `signal` rules at all does not mediate signals and needs
nothing.

**Unconfined pods.** A pod with `appArmorProfile: {type: Unconfined}` runs as
`runc (unconfined)` rather than `unconfined`. Like the host's own runc, it is
then exempt from Ubuntu's restriction on unprivileged user namespaces,
`kernel.apparmor_restrict_unprivileged_userns`.

**Runtime handlers.** A handler in `spec.node.runtimeHandlers` whose binary
runs `runc` from its `PATH` runs Stratum's runc and is covered. A handler whose
own binary is the OCI runtime, at a path no host profile covers, runs
unconfined, and its containers are stacked the same way. Stratum does not cover
it. To cover it, add its path to the paths `/etc/apparmor.d/runc` attaches to
and restart Stratum, which builds its profile from that file.
