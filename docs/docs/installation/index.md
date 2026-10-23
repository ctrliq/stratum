# Installation

This page installs Stratum on a node and brings up a single node or a
cluster of nodes and workers. It covers what each host needs first, including
the host firewall, and how to remove Stratum again. You need root on every node
and the `stratum` artifact. Settings beyond a first install, such as the node IP,
the systemd unit and SELinux labelling, are on [Configuration](configuration.md).

## The artifact

Stratum ships as one executable with an appended payload:

```
stratum
  ELF                supervisor + operator
  payload/bin        the component binaries
  payload/images     OCI archive of the container images
```

The artifact is the airgap package. It carries every component and image, so an
install needs no registry, no image list to pre-pull and no registry
credentials. An SELinux host also needs its distribution's SELinux policy
packages, from its own mirror; see [Required packages](#required-packages).

`stratum which` reports what a staged tree contains. `stratum stage --root <dir>`
unpacks the payload without starting anything.

## Prerequisites

| | |
|---|---|
| OS | Linux, **cgroup v2 only** |
| Privileges | root |
| Architecture | amd64 or arm64, both built natively |
| Network | none required for install; node IP autodetection needs a default route or a single global IPv4 address, see [Node IP](configuration.md#node-ip) |
| Firewall | firewalld may be running; see [Host firewall](#host-firewall) |
| Packages | on an SELinux host, the SELinux policy packages; for `stratum uninstall`, `tc`, `iptables` and `bpftool`; see [Required packages](#required-packages) |

### Required packages

On a host where SELinux is enabled, Stratum needs these packages from the
distribution's AppStream and BaseOS repositories:

* `container-selinux`, which defines the SELinux types for a container runtime
  and its files.
* `policycoreutils-python-utils`, which provides `semanage`.
* `libselinux-utils`, which provides `matchpathcon`. `policycoreutils`
  requires it, so it is installed wherever SELinux is.

```bash
sudo dnf install container-selinux policycoreutils-python-utils libselinux-utils
```

When SELinux is enforcing, `stratum up` refuses a new node without these
packages and names what is missing. A profile that requires SELinux refuses it
in permissive mode too. On any other host, and on a node that was brought up
before, `stratum up` prints a warning and continues. Stratum applies the labels
on the first start after you install the packages. To apply them straight
away, run `sudo systemctl restart stratum`. Stratum was tested with
`container-selinux` 2.245 on Rocky Linux 9.8.

`stratum uninstall` needs three more tools to remove Cilium's datapath:

* `tc`, from `iproute-tc` on Rocky Linux and RHEL.
* `iptables-save` and `ip6tables-save`, from `iptables`.
* `bpftool`, from `bpftool`.

```bash
sudo dnf install iproute-tc iptables bpftool
```

Uninstall looks for these tools only in `/usr/sbin`, `/usr/bin`, `/sbin` and
`/bin`, never on your `PATH`. A tool installed anywhere else counts as missing.

### SELinux

Stratum installs and runs on an SELinux-enforcing host, which is the default
on Rocky Linux, RHEL and RLC. On every start, `stratum up` registers SELinux
file contexts for its paths and relabels its files. Containers run unconfined
on this release. [SELinux](configuration.md#selinux) on the
Configuration page lists what Stratum labels, how containers run and how to
turn labelling off.

### Host settings

`stratum up` prepares the host on every start:

* It loads the `overlay` and `br_netfilter` modules, and lists them in
  `/etc/modules-load.d/k8s.conf` so they load again after a reboot.
* It turns swap off.
* It writes `/etc/sysctl.d/99-stratum.conf` with the settings pod networking
  needs (`ip_forward` and `bridge-nf-call-iptables`). A profile that declares
  sysctls gets its own drop-in beside it, `/etc/sysctl.d/99-stratum-<profile>.conf`.
* It raises `fs.inotify.max_user_instances` to at least 8192 and
  `fs.inotify.max_user_watches` to at least 524288. With the distribution
  default of 128 instances, a node with a few dozen pods runs out, and a
  workload fails with `too many open files`. A host already set higher keeps
  its value.
* It makes the root filesystem a shared mount (`mount --make-rshared /`, which
  applies to every mount under it) when it is private. systemd does this at
  boot, but other init systems do not, and containerd refuses the host-path
  mounts Cilium's pods make on a private root. A root that is already shared,
  or a slave, is left alone.

`stratum stage` and `stratum up` set their own umask to `022`, so the node's
file modes do not depend on the shell or unit that starts them. Hardened hosts,
such as those following a Center for Internet Security (CIS) benchmark, often
set root's umask to `027` or `077`. Without the fixed umask, containers that run
as a non-root user could not read their own image or mounted ConfigMaps, and
CoreDNS would crash-loop. Other `stratum` commands keep the caller's umask.
Files Stratum creates with an explicit `0600` or `0700` mode, such as keys, keep
that mode.

## Host firewall

Stratum does not configure a host firewall for you. On a host that runs
firewalld, `stratum up` adds the bindings pods on the node need, and you open
the ports nodes use to reach each other. Read this section before the first
`stratum up` on any host where firewalld is running.

!!! warning "Turning firewalld off removes the only filter on the host"
    Cilium, as Stratum configures it, runs with its host firewall off
    (`enable-host-firewall: "false"`). Nothing in the Stratum stack filters
    traffic to the node itself. With firewalld stopped, every listener in
    [What a node exposes](#what-a-node-exposes), including the API server and
    the kubelet, answers on every interface, the public one included. If pods
    cannot reach the API server or a worker cannot join, fix the firewalld
    configuration with the steps below instead of stopping firewalld. Protect
    the nodes with firewalld, a network firewall or security group, or both.

### What a node exposes

These listeners answer on every interface that no firewall filters:

| Port | Process | Nodes |
|---|---|---|
| 6443/tcp | kube-apiserver | control planes |
| 10250/tcp | kubelet | all |
| 9448/tcp | Stratum operator admission webhook | control planes |
| 9963/tcp | cilium-operator | wherever the operator pod runs (any node) |
| 9964/tcp | cilium-envoy | all |
| 8472/udp | Cilium VXLAN | all |
| 30443/tcp | the dashboard's NodePort (`spec.dashboard.nodePort`). Cilium answers it before netfilter, so firewalld does not filter it | all, while the dashboard is on |

etcd listens on `127.0.0.1` and the node IP only, so it is reachable wherever
the node IP is.

### firewalld

RLC and RHEL ship with firewalld running, and so do some providers' Rocky Linux
images. firewalld puts an interface that belongs to no zone in the default
zone, which rejects pod traffic to the node. Without a binding for the pod
interfaces, CoreDNS and local-path-provisioner never become Ready and log
`dial tcp 10.96.0.1:443: connect: no route to host`.

When firewalld is running, `stratum up` adds two interface bindings on every
start, in both the runtime and the permanent configuration:

| Interface pattern | Zone | Purpose |
|---|---|---|
| `lxc+` | `trusted` | Cilium's host-side pod interfaces, so pods can reach services on their own node |
| `lxcbr+` | the host's default zone, unless you already bound `lxcbr+` to another zone that is not `trusted` | keeps the standard LXC bridge, `lxcbr0`, out of the `trusted` zone |

`stratum up` never runs `firewall-cmd --reload`. It records the bindings it
adds in `/var/lib/stratum/firewalld.json`. `stratum uninstall` removes only
those, and keeps any binding you made yourself.

On a node an earlier release brought up, `stratum up` also removes the
pod-range source binding that release added to the `trusted` zone, and prints
`firewalld: removed unsafe trusted source <range>`. It removes only a binding
it recorded as its own, so one you add yourself stays.

These bindings cover pods on the same node only. To set up the rest:

1. Allow the [ports between nodes](#ports-between-nodes) in the host firewall
   and in any network firewall between the nodes.
2. On every node, apply [a tested firewalld configuration](#a-tested-firewalld-configuration),
   which opens those ports to the node subnet and admits pods on other nodes.

When `stratum up` cannot add the `lxc+` binding safely, it does not add it,
prints a `WARN: firewalld:` line and starts the node anyway. Pods on that node then
cannot reach it. Fix the cause, then run `sudo systemctl restart stratum`:

| The warning says | Cause | Fix |
|---|---|---|
| `interface lxc+ belongs to zone "<zone>"` | you bound `lxc+` to another zone; Stratum does not move it | remove it from that zone, or make that zone admit pod traffic |
| `lxc+ also matches <interfaces>, which is not a Cilium pod interface` | a bridge or physical interface other than `lxcbr*` has a name that starts with `lxc` | rename that interface, or bind each Cilium interface by its exact name with `firewall-cmd --zone=trusted --add-interface=lxc<id>` |
| `cannot list interfaces to check what lxc+ would match` | `stratum up` cannot read `/sys/class/net` | check that `/sys` is mounted and readable by root |
| `cannot establish a non-trusted zone for lxcbr+` | the default zone is `trusted`, or `firewall-cmd` failed | set a default zone other than `trusted` |
| `lxcbr+ already belongs to trusted` | you bound `lxcbr+` to the `trusted` zone | move `lxcbr+` to a zone that is not `trusted` |
| `cannot check daemon state` | `firewall-cmd --state` failed | check that firewalld and its D-Bus service respond |

A veth you created yourself with a name that starts with `lxc` also matches
`lxc+`, and Stratum cannot tell it from a pod interface.

### Ports between nodes

On a multi-node cluster, nodes have to reach each other on their node IPs.
`stratum up` opens none of these ports, because which interfaces and zones face
the other nodes depends on your network:

| Port | From → to | Used by |
|---|---|---|
| 6443/tcp | every node → control planes | API server; workers join through it |
| 10250/tcp | control planes → every node | kubelet: `kubectl logs`, `exec` and `top` |
| 2379-2380/tcp | control plane ↔ control plane | etcd client and peer traffic |
| 9448/tcp | control plane ↔ control plane | Stratum operator admission webhook; an API server can call it on any control plane |
| 8472/udp | every node ↔ every node | Cilium VXLAN: pod traffic between nodes |
| 4240/tcp | every node ↔ every node | Cilium health checks |
| ICMP echo (types 8 and 0) | every node ↔ every node | Cilium health checks |

The table covers IPv4 and the Cilium configuration Stratum ships, a VXLAN tunnel
with kube-proxy replacement.

Pods also have to reach the node. A pod's call to the `kubernetes` Service
(`10.96.0.1:443`) reaches the API server as `<node IP>:6443`, from the pod's
own address. The `lxc+` binding admits a pod on the same node. A pod on another
node needs a rule for the pod range (`10.245.0.0/16` by default), which the
configuration below adds.

### A tested firewalld configuration

This configuration adds a `stratum` zone for traffic from the node subnet,
opens the ports between nodes in it, and trusts the pod range. Apply it on
every node, before or after `stratum up`.

1. Check which firewalld backend the node uses:

    ```bash
    sudo grep -E '^[[:space:]]*FirewallBackend[[:space:]]*=' /etc/firewalld/firewalld.conf
    ```

    `FirewallBackend=nftables` is the tested backend. When the file is readable
    and `grep` prints nothing (exit 1), firewalld uses nftables too. When
    `grep` reports an error, such as a missing file, the check says nothing
    about the backend.

    !!! warning "On the iptables backend, a reload can flush Cilium's rules"
        With `FirewallBackend=iptables`, `firewall-cmd --reload` can remove
        Cilium's rules along with firewalld's own, and break pod networking on
        a running node. On that backend, do not run the last command of the
        next step on a node that is running Stratum.

2. Run these commands. Replace `10.50.0.0/24` with your node subnet, and
   `10.245.0.0/16` with your pod range if you changed it.

    !!! warning "The `stratum` zone admits only what you add to it"
        After the reload, firewalld matches every connection from the node
        subnet against the `stratum` zone instead of the interface's own zone.
        Services that zone allowed no longer apply to those connections. The
        commands add `ssh`. Add any other port you use to manage the nodes from
        the node subnet before you run the reload.

    ```bash
    sudo firewall-cmd --permanent --new-zone=stratum
    sudo firewall-cmd --permanent --zone=stratum --add-source=10.50.0.0/24
    sudo firewall-cmd --permanent --zone=stratum \
      --add-port=6443/tcp --add-port=10250/tcp --add-port=2379-2380/tcp \
      --add-port=8472/udp --add-port=4240/tcp --add-port=9448/tcp
    sudo firewall-cmd --permanent --zone=stratum --add-service=ssh
    sudo firewall-cmd --permanent --zone=trusted --add-source=10.245.0.0/16
    sudo firewall-cmd --reload
    ```

    Keep `--add-service=ssh` in its own command. firewalld 2.4.3 rejects
    `--add-service` combined with `--add-port` with `argument --add-service:
    not allowed with argument --add-port`, and then adds neither.

3. Check the result:

    ```bash
    sudo firewall-cmd --zone=stratum --list-all
    sudo firewall-cmd --get-zone-of-interface=lxc+
    kubectl get pods -A
    ```

    The `stratum` zone lists the node subnet, the ports and `ssh`. Once
    `stratum up` has run, `lxc+` belongs to `trusted`. Every pod reaches
    `Running`, CoreDNS included.

How the configuration behaves:

- firewalld matches a zone bound to a source before a zone bound to an
  interface. The `stratum` zone does not compete with the zone NetworkManager
  assigns to each connection. Traffic from outside the node subnet, including
  the public interface, still falls into that interface's own zone.
- firewalld zones accept ICMP unless you block it, so the health checks need no
  ICMP rule.
- Stratum never removes bindings you made, so these survive `stratum up` and
  `stratum uninstall`.
- On the nftables backend, reloading, restarting or stopping firewalld leaves
  Cilium's rules in place. Cilium keeps them in the `ip` `filter`, `nat`,
  `mangle` and `raw` tables, and firewalld manages only its own
  `inet firewalld` table.

!!! warning "The pod range is trusted on every interface"
    The `trusted` source binding admits every packet with a pod-range source
    address, whichever interface it arrives on. For this reason `stratum up`
    does not add it for you. Make sure nothing outside the cluster can send
    your nodes traffic from the pod range, for example by dropping it at your
    network firewall.

!!! note "What was tested"
    The configuration was tested on Rocky Linux 9.8 (firewalld 1.3.4, nftables
    backend, SELinux enforcing) with one control plane and two workers. Workers
    joined, pods, Services and DNS worked across nodes, `kubectl logs` and
    `exec` worked, and Cilium's health checks passed from every node. It kept
    working through `firewall-cmd --reload` and `systemctl restart firewalld`.
    The commands as shown, with `ssh` in its own command, were also tested on
    Rocky Linux 10.2 with firewalld 2.4.3. `9448/tcp` and the `lxc+` and
    `lxcbr+` bindings `stratum up` adds were not part of the multi-node test.

### Troubleshooting: `no route to host` to port 6443

firewalld rejects a packet with an ICMP "administratively prohibited" reply,
which Go reports as `connect: no route to host`. When a worker join fails with
`dial tcp <control-plane-ip>:6443: connect: no route to host`, or a pod logs
`dial tcp 10.96.0.1:443: connect: no route to host`, check the host firewall
before the routes. A worker join retries an unreachable control plane for up to
two minutes, then fails.

To see what firewalld rejects, log denied packets on the receiving node and
read the kernel log. Changing the setting reloads firewalld, so use these
commands only on the nftables backend while Stratum runs (see
[the backend check](#a-tested-firewalld-configuration)):

```bash
sudo firewall-cmd --set-log-denied=all
sudo journalctl -k | grep _REJECT
sudo firewall-cmd --set-log-denied=off
```

On the iptables backend, do not change the setting on a running node. Read
what is already logged instead:

```bash
sudo firewall-cmd --get-log-denied
sudo journalctl -k | grep _REJECT
```

If the first command prints `off`, firewalld logs no rejects, and the kernel
log shows none.

An entry such as `filter_IN_public_REJECT ... DPT=6443` names the zone that
rejected the packet and the port it was for.

## Topologies

**Node (default).**

```bash
sudo /usr/local/bin/stratum up
```

One node that is both control plane and workload host. The control plane runs
as kubelet-managed static pods, and the node is registered and schedulable. Use
it for a single-node cluster or a hyperconverged deployment. For a dedicated
control-plane machine, taint the node after it registers:

```bash
kubectl taint nodes <node> node-role.kubernetes.io/control-plane:NoSchedule
```

To add more control planes, see [High availability](../cluster-admin/high-availability.md).

**Worker (workload-only).**

```bash
sudo /usr/local/bin/stratum up --worker --server https://<endpoint>:6443 \
  --token <token> --discovery-token-ca-cert-hash sha256:<pin>
```

Run `stratum token create` on a control plane to print this command. It puts
the cluster's virtual IP (VIP) after `--server` when one is configured, and
otherwise the address of the control plane you run it on. With a VIP, the
worker still reaches the API server when that control plane fails; see
[High availability](../cluster-admin/high-availability.md#set-the-vip).
`stratum token create` refuses, rather than prints, a VIP or control-plane
address that another host cannot dial: a loopback, all-zeros or multicast
address, the broadcast address (`255.255.255.255`), a value that is not an IP
address, or a link-local address, which is not routed beyond its own network
link. To name another endpoint, such as a load balancer, run
`stratum token create --server https://<endpoint>:6443`. The command prints the
URL you give with `--server` as it is, without checking it.

On a cluster with a non-default pod CIDR, service CIDR or cluster domain,
`stratum token create` adds `--pod-cidr`, `--service-cidr` or
`--cluster-domain` to the command. Run the command it prints as it is.

A worker reads only its node IP and `spec.selinux` from
`/etc/stratum/config.yaml`. Do not copy a control plane's file to a worker; see
[On a worker](configuration.md#on-a-worker).

### If a worker's certificate request stays pending

The worker's kubelet asks for its client certificate with the join token. The
Stratum operator approves the request only when no other node holds that node
name. A name stays claimed after its Node or certificate signing request (CSR)
is deleted, so a token cannot obtain a second certificate for a name that was
already used.

A request the operator does not approve stays pending, and the operator writes
the reason on it. List the pending requests, then read one's reason, the node
name it asks for and the join token that sent it (`system:bootstrap:<token-id>`):

```bash
sudo /usr/local/bin/stratum kubectl get csr --field-selector spec.signerName=kubernetes.io/kube-apiserver-client-kubelet
sudo /usr/local/bin/stratum kubectl get csr <csr> -o jsonpath='{.metadata.annotations.stratum\.ciq\.com/node-client-pending}{"\n"}{.metadata.annotations.stratum\.ciq\.com/requested-node}{"\n"}{.spec.username}{"\n"}'
```

`kubectl describe csr <csr>` shows the same annotations. To check the subject
the request carries, decode it:

```bash
sudo /usr/local/bin/stratum kubectl get csr <csr> -o jsonpath='{.spec.request}' | base64 -d | openssl req -noout -subject
```

Deny a request you do not expect, and delete the token that sent it:

```bash
sudo /usr/local/bin/stratum kubectl certificate deny <csr>
sudo /usr/local/bin/stratum kubectl -n kube-system delete secret bootstrap-token-<token-id>
```

The operator denies a request whose join token has expired or been deleted.
Rejoin that machine with a new token. To reuse the name of a rebuilt machine, or
to approve requests on a cluster without the operator, see
[Reusing a node name](configuration.md#reusing-a-node-name).

## Reboot persistence

On a systemd host, `stratum up` installs and enables
`/etc/systemd/system/stratum.service` every time it starts. After a reboot,
systemd runs `stratum up` again and the node rejoins the cluster without anyone
logging in. The unit runs `stratum up --config <path>` on a control plane and
`stratum up --worker` on a worker, never the join flags.
[The systemd unit](configuration.md#the-systemd-unit) covers the unit's
environment file, a masked unit and how to keep a node from starting at boot.

## Uninstall

`stratum uninstall` removes Stratum from one node. Without `--yes` it lists what
it would stop and delete, and changes nothing:

```bash
sudo /usr/local/bin/stratum uninstall          # shows what it would stop and delete
sudo /usr/local/bin/stratum uninstall --yes    # does it
```

!!! warning "Uninstall deletes the node's etcd data and cluster state"
    `--yes` stops every container Stratum runs on the node and deletes `/var/lib/stratum`,
    which holds the node's etcd data, certificates and keys. It cannot be
    undone. On a single-node cluster, take a snapshot first; see
    [Snapshot & Restore](../cluster-admin/snapshot-restore.md).

Before you uninstall a node that belongs to a larger cluster:

- Drain it from a control plane, so its pods move and their disruption budgets
  hold: `kubectl drain <node> --ignore-daemonsets --delete-emptydir-data`.
- For a control plane, remove its Node and its etcd member from a remaining
  control plane; see [Removing a member](../cluster-admin/high-availability.md#removing-a-member).
  The operator removes a worker's Node about 2 minutes after the worker is
  gone. Without an operator, run `kubectl delete node <node>` yourself.

Uninstall checks the node before it changes anything and refuses while another
maintenance operation runs. If a step fails, it carries on, names the failure
and exits nonzero; run it again to finish.

Uninstall also refuses in a private PID namespace, such as a container started
without `--pid=host`: there it cannot see whether another kubelet or container
runtime on the host shares what it would remove. Run it on the host. If you
have checked the host yourself, add `--assume-no-foreign-runtimes`; `--yes`
and `--force` do not imply it. While another kubelet or container runtime runs,
such as RKE2 or k3s, uninstall keeps `/etc/cni/net.d`, says so and exits
nonzero.

If `stratum uninstall --yes` still cannot finish, clean up by hand:

1. Stop the node and keep it from starting at boot:

    ```bash
    sudo systemctl disable --now stratum
    ```

2. Reboot the node. A reboot clears the network namespaces, Cilium's links,
   rules and BPF pins, and the pod cgroups that Stratum's containers left.
3. Run `sudo /usr/local/bin/stratum uninstall --yes` again, and run any `firewall-cmd` or
   `firewall-offline-cmd` command it prints.
4. If it still fails, remove the firewalld bindings and delete what uninstall
   deletes, then reload systemd. On a host running firewalld, check which zone
   holds each pod binding:

    ```bash
    sudo firewall-cmd --get-zone-of-interface=lxc+
    sudo firewall-cmd --get-zone-of-interface=lxcbr+
    ```

    Remove each binding you did not make yourself, in both the runtime and the
    permanent configuration:

    ```bash
    sudo firewall-cmd --zone=<zone> --remove-interface=<pattern>
    sudo firewall-cmd --permanent --zone=<zone> --remove-interface=<pattern>
    ```

    On an SELinux host, remove Stratum's file contexts next, before you delete
    `/var/lib/stratum`, which holds `selinux-contexts`, the record
    of the contexts Stratum registered. Print the record and the local
    customizations:

    ```bash
    sudo cat /var/lib/stratum/selinux-contexts
    sudo semanage fcontext -l -C
    ```

    Delete each file context, then each equivalence, that the record lists and
    `semanage` still shows exactly as recorded, as
    [Opting out](configuration.md#opting-out) shows. Skip the lines that start
    with `policy`. Leave a context that differs from the record, and any the
    record does not list: Stratum does not own them. Then relabel the binary,
    which stays:

    ```bash
    sudo restorecon /usr/local/bin/stratum
    ```

    Then delete the files. Uninstall leaves `/usr/local/bin/stratum` in place.
    Leave `/opt/local-path-provisioner` alone if it holds volume data.

    ```bash
    sudo rm -rf /var/lib/stratum /etc/stratum /run/stratum /var/log/stratum /var/lib/stratum-helm /etc/cni/net.d /etc/containerd/certs.d
    sudo rm -f /etc/systemd/system/stratum.service /etc/containerd/conf.d/stratum-registry.toml /etc/sysctl.d/99-stratum*.conf /etc/modules-load.d/k8s.conf /usr/local/bin/stratum.bak-*
    sudo systemctl daemon-reload
    ```
