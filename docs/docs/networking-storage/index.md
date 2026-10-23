# Networking & Storage

Stratum installs Cilium for pod networking, network policy and Services,
CoreDNS for cluster DNS, the `local-path` provisioner as the default
StorageClass, and etcd as the cluster datastore. This page says what each of
them does in a Stratum cluster, and how to configure the pod and service
ranges, the cluster domain and Cilium. Set the network values in
`/etc/stratum/config.yaml` before the first controller starts, because most of
them cannot change afterwards.

`stratum version --components` prints the versions of Cilium, CoreDNS, etcd
and the other embedded components. It does not print `local-path`, whose
version is the tag of the provisioner's image:

```console
$ kubectl -n local-path-storage get deploy local-path-provisioner \
  -o jsonpath='{.spec.template.spec.containers[0].image}'
```

## Cilium

Cilium is the cluster's only container network interface (CNI) plugin. The
operator applies it at bring-up. It provides pod networking, network policy
and Service load balancing.

Stratum does not ship kube-proxy. Cilium's kube-proxy replacement implements
Services, so the cluster has one service dataplane. Services behave as they do
on any Kubernetes cluster. A diagnostic script, DaemonSet or runbook that
expects a `kube-proxy` pod or kube-proxy's iptables rules does not find them.
See Cilium's
[kube-proxy replacement](https://docs.cilium.io/en/stable/network/kubernetes/kubeproxy-free/)
documentation for how it handles Services.

The maximum transmission unit (MTU), the interfaces Cilium attaches to, its
BGP control plane and native routing are set under `spec.network.cilium`. See
[Cilium settings](#cilium-settings).

## LoadBalancer Services

A `type: LoadBalancer` Service gets its address from Cilium's load-balancer IP
address management (LB-IPAM), out of a `CiliumLoadBalancerIPPool` you create.
On a flat layer 2 (L2) network, Cilium's *L2 announcements* make that address
reachable. One Cilium agent answers ARP requests for the address on the local
segment, and the other machines on the segment send their traffic for it to
that node. On a routed layer 3 (L3) fabric, advertise the addresses over BGP
instead, with `bgpControlPlane` in [Cilium settings](#cilium-settings).

L2 announcements are on in every cluster, but Cilium announces nothing until
you add a pool and a policy. For example, to announce addresses `192.0.2.200`
to `192.0.2.220` on interfaces whose names start with `eth`:

```yaml
apiVersion: cilium.io/v2
kind: CiliumLoadBalancerIPPool
metadata:
  name: lan
spec:
  blocks:
  - start: 192.0.2.200
    stop: 192.0.2.220
---
apiVersion: cilium.io/v2alpha1
kind: CiliumL2AnnouncementPolicy
metadata:
  name: lan
spec:
  interfaces: ["^eth[0-9]+"]
  loadBalancerIPs: true
```

The pool must hold free addresses on the same L2 segment as the interfaces the
policy names, outside any DHCP range. Without a `serviceSelector`, the policy
covers every LoadBalancer Service. Without a `nodeSelector`, any node can
answer. Cilium announces a Service that sets no `loadBalancerClass`, or sets
`loadBalancerClass: io.cilium/l2-announcer`.

!!! warning "Traffic is dropped with `externalTrafficPolicy: Local`"
    Cilium chooses the announcing node without regard to where the Service's
    pods run. With `externalTrafficPolicy: Local`, traffic can reach a node
    with no local endpoint and be dropped. Leave such Services on the default
    `Cluster` policy.

One node at a time announces each address: the holder of a Lease in
`kube-system` named `cilium-l2announce-<namespace>-<service>`. If that node
goes down, another agent takes the Lease once it expires, after 15 seconds,
and starts answering. All the traffic for one address passes through the node
that holds its Lease. Each agent renews its Leases every few seconds, and
those requests count against the agent's Kubernetes API rate limit. Test
before you announce hundreds of Services from one cluster.

A Stratum upgrade, or a change to [Cilium settings](#cilium-settings), that
changes the Cilium configuration restarts the Cilium agents one node at a time,
and restarts the Cilium operator. Existing connections keep flowing while an
agent restarts. No new pod starts on that node until the agent is back. On a
single-node cluster, that is the whole cluster.

## CoreDNS

CoreDNS serves cluster DNS at the usual Service address.

### Where pods' upstream DNS comes from

CoreDNS answers cluster names itself. It forwards every other name to the
resolvers of the node it runs on, as the upstream Kubernetes default does.
Private zones, such as a Route 53 private hosted zone, split-horizon names and
corporate DNS, resolve in pods once they resolve on the nodes that run CoreDNS.

A pod queries the CoreDNS Service, and the replica that answers can be on
another node. CoreDNS can run on any node, controllers included. Make site DNS
resolvable on every node, or different replicas give different answers.

Only CoreDNS forwards through a resolver that Stratum chooses. The kubelet
keeps the node's `/etc/resolv.conf`. Every other pod that uses the node's DNS
(`dnsPolicy: Default`, and host-network pods such as the control plane)
resolves exactly as the node does, including through loopback resolvers such
as a local dnsmasq or systemd-resolved's stub.

Each time Stratum starts on a node, it chooses what that node's CoreDNS pods
forward through:

| The node's resolvers | CoreDNS forwards through |
| --- | --- |
| `/etc/resolv.conf` names only nameservers a pod can reach | the node's `/etc/resolv.conf`, as the kubelet hands it to the pod |
| `/etc/resolv.conf` names none a pod can reach, and `/run/systemd/resolve/resolv.conf` names only reachable ones (systemd-resolved's `127.0.0.53` stub) | a copy of `/run/systemd/resolve/resolv.conf` |
| the first of those two files that names a reachable nameserver also names unreachable ones | a copy with only the reachable nameservers |
| neither file names a nameserver a pod can reach | `8.8.8.8` and `1.1.1.1` |

For every row but the first, Stratum writes the copy to
`/var/lib/stratum/dns/resolv.conf`, which the CoreDNS pod mounts. For the first
row, it removes that file. A CoreDNS pod on a node that has not written the
file, such as a node not yet upgraded in a rolling upgrade, makes the same
choice itself when it starts, by the same rows.

A pod has its own network namespace, so some of the node's nameservers are
unreachable from inside one. Stratum leaves these out:

- A loopback or unspecified address, such as `127.0.0.53` or `0.0.0.0`. Inside
  the CoreDNS pod, that address is CoreDNS itself, so CoreDNS would forward to
  itself and stop.
- An IPv6 link-local address (`fe80::`). It names one of the node's links, not
  the pod's.
- Any other IPv6 address, unless the cluster is dual-stack. An IPv4-only pod
  has no IPv6 route.

A node tells whether its pods have IPv6 from the pod and service ranges it
recorded. A worker that joined from a release that did not record those ranges
counts as IPv4-only, and says so when it leaves an IPv6 nameserver out. If such
a worker needs IPv6 resolvers, rejoin it with the command
`stratum token create` prints. Stratum keeps an IPv4 link-local resolver, such
as the AWS VPC resolver `169.254.169.253`, because the node routes it.

When the choice is not the first row, Stratum logs it at each start: the file
CoreDNS forwards through, its nameservers, and every nameserver left out with
the reason, or a file it could not read. For example:

```text
stratum: CoreDNS forwards through /var/lib/stratum/dns/resolv.conf (10.0.0.2, from /run/systemd/resolve/resolv.conf); left out 127.0.0.53 from /etc/resolv.conf: loopback
```

For the last row, the line starts with
`WARN: neither /etc/resolv.conf nor /run/systemd/resolve/resolv.conf names a nameserver a pod can reach`.
Give the node a nameserver that pods can reach, then follow the steps below.

There is no cluster-level forwarder setting. The operator restores the CoreDNS
ConfigMap if you edit it by hand. To change where pods resolve:

1. Change the resolver on every node that can run CoreDNS.
2. Restart Stratum on each of those nodes, so that it chooses again from the
   new resolvers. This does not restart running pods.

    ```console
    # systemctl restart stratum
    ```

3. Delete the CoreDNS pods. CoreDNS reads its upstream only when it starts.

    ```console
    $ kubectl -n kube-system delete pod -l k8s-app=kube-dns
    ```

Do not skip step 2. A copy that Stratum wrote to
`/var/lib/stratum/dns/resolv.conf` takes precedence over the node's current
resolvers, and it stays until Stratum starts again. For example, if a node
behind systemd-resolved's stub moves to new upstream resolvers and Stratum has
not restarted, new CoreDNS pods still forward through the old ones.

## local-path is the default StorageClass

The operator applies the `local-path` provisioner at bring-up and marks its
StorageClass as the default. It provisions a `PersistentVolume` from a
directory under `/opt/local-path-provisioner` on the node that runs the pod.

!!! warning "A local-path volume is lost with its node"
    A volume that `local-path` provisions lives on one node. The pod that uses
    it is pinned to that node. If the node is lost, so is the data.

    For durable or shared storage, install a Container Storage Interface (CSI)
    driver and make its StorageClass the default.

Stratum ships no other storage backend.

## etcd is the only datastore

The cluster stores its state in etcd, with TLS on every connection. Stratum
ships no other datastore. etcd has its own certificate authority, separate
from the Kubernetes root. See
[etcd's trust domain](../cluster-admin/hardening.md#etcds-trust-domain).

## Network configuration

```yaml
spec:
  network:
    podCIDR: 10.245.0.0/16      # the default
    serviceCIDR: 10.96.0.0/12   # the default
    clusterDomain: cluster.local  # the default
```

Set these before the first controller starts. On its first boot, the
controller records them in `/var/lib/stratum/pki/netconfig`: the pod CIDR, the
service CIDR and, only when it is not `cluster.local`, the cluster domain.
Joining control planes receive that file with the cluster certificates, so
every node agrees on the values. A joining node does not need `podCIDR`,
`serviceCIDR` or `clusterDomain` in its own config. It still reads
`spec.network.nodeIP`. If a joining control plane's config sets `podCIDR`,
`serviceCIDR` or `clusterDomain` to a value other than the cluster's, the join
is refused.

Once a node has the cluster's recorded CIDRs, they win on every start. If you
leave `spec.network` out, or leave one CIDR unset, Stratum does not reset it to
the default. If you set `podCIDR` or `serviceCIDR` to a different value, the
node keeps its recorded range and logs a warning on startup:

```text
stratum: WARNING: spec.network (pod "...", service "...") differs from this cluster's recorded CIDRs (pod ..., service ...); ignoring it -- changing them needs a cluster rebuild; if this node's recorded CIDRs are the wrong ones, restore /var/lib/stratum/pki/netconfig from another control plane
```

To change the CIDRs of a running cluster, rebuild it.

Before you upgrade an HA cluster with custom CIDRs from an older build, check
that every control plane recorded the same ranges. See
[HA clusters with custom CIDRs](../appendices/upgrade-reference.md#ha-clusters-with-custom-cidrs).

Host firewall rules that name the pod range must use the range set here. See
[Ports between nodes](../installation/index.md#ports-between-nodes).

For a dual-stack cluster, give both families in `podCIDR` and `serviceCIDR`,
as `"<v4>,<v6>"`.

### Cluster domain

`clusterDomain` is the DNS suffix of in-cluster names, as in
`kubernetes.default.svc.<clusterDomain>`. It sets the kubelet's
`clusterDomain`, the CoreDNS zone, the apiserver's service-account issuer and
audience, and a DNS name on the apiserver certificate. It must be a lowercase
DNS subdomain, such as `corp.internal`. Any other value stops `stratum up` with
an error.

A control plane that joins with `--join` takes the domain from the cluster. A
worker gets it from the `--cluster-domain` flag. A worker's config file may set
only `spec.network.nodeIP` and `spec.selinux`, so a worker join refuses a file
that sets `clusterDomain`. On a cluster with a non-default domain,
`stratum token create` adds the flag to the worker join command it prints:

```console
# /usr/local/bin/stratum up --worker --server https://<endpoint>:6443 \
  --token <token> --discovery-token-ca-cert-hash sha256:<pin> \
  --cluster-domain 'corp.internal'
```

Run the printed command as it is. A worker that joins without
`--cluster-domain` uses `cluster.local`, and its pods get DNS search domains
that the cluster's CoreDNS does not serve.

!!! warning "You cannot change the cluster domain after init"
    Choose the domain before the first controller starts. The apiserver
    certificates, the service-account tokens already issued and every worker
    keep the original domain. To move to a different domain, build a new
    cluster.

A control plane added with `--join` inherits the cluster's domain, so leave
`spec.network.clusterDomain` unset in its config file. On a restart, Stratum
ignores a changed CIDR with a warning, but refuses a changed domain. If the
config sets a different domain from the one the cluster was initialized with,
`stratum up` refuses to start the control plane. A `--join` refuses either
change. A fresh join refuses it before it issues the node's certificates or
adds an etcd member. A re-run that resumes an interrupted join refuses it too,
although by then the earlier run has issued the node's certificates and may
have added its etcd member. Fix the config file and run the command again.

### Cilium settings

By default, Cilium runs a VXLAN tunnel, attaches to every interface it
detects, chooses its own MTU, and has no BGP control plane.
`spec.network.cilium` changes that. Every field is optional. A config without
the block runs the defaults.

| Field | Default | Sets in `cilium-config` |
|---|---|---|
| `mtu` | detected from the devices | `mtu`. 1280 to 65535. In tunnel mode, Cilium subtracts the tunnel overhead from it for pods. |
| `devices` | detected | `devices`: the interfaces Cilium's datapath and NodePorts attach to. A trailing `+` matches a prefix, as in `fe-nic+`. |
| `bgpControlPlane` | `false` | `enable-bgp-control-plane`. You create the peers, advertisements and address pools as your own `CiliumBGPClusterConfig`, `CiliumBGPPeerConfig`, `CiliumBGPAdvertisement` and `CiliumLoadBalancerIPPool` resources. Put peer authentication Secrets (`authSecretRef`) in `kube-system`, which the Cilium agent can read. |
| `routingMode` | `tunnel` | `routing-mode`. `native` routes pod traffic over the underlay without encapsulation. |
| `ipv4NativeRoutingCIDR` | none | `ipv4-native-routing-cidr`. Required with `routingMode: native`: the range pods reach without masquerading, usually the pod CIDR or the fabric's. |
| `ipv6NativeRoutingCIDR` | none | `ipv6-native-routing-cidr`. Required with `routingMode: native` on a dual-stack cluster, and refused on a single-stack one. |
| `autoDirectNodeRoutes` | `false` | `auto-direct-node-routes`. Native routing only. Installs a route to each node's pod CIDR, which works only when the nodes share an L2 segment. On an L3 fabric, leave it off and advertise the pod CIDRs over BGP. |

For example, an L3 fabric with BGP to the host and jumbo frames:

```yaml
spec:
  network:
    cilium:
      mtu: 9216
      devices: [fe-nic+, fblo]
      bgpControlPlane: true
      routingMode: native
      ipv4NativeRoutingCIDR: 10.245.0.0/16
```

`stratum up` writes the settings into the Cilium manifest, and the operator
applies them. The Cilium agents and the Cilium operator read the settings only
when they start, so a change to the block restarts them. The settings persist
across the operator's periodic re-apply, `stratum up` restarts and upgrades.

!!! warning "Every control plane must carry the same block"
    The Cilium settings are cluster-wide, but each control plane renders them
    from its own config file, and the one that leads the operator applies
    them. A control plane joining with `--join` is refused if its
    `spec.network.cilium` differs from the cluster's. To change the settings
    later, edit the block on every control plane, then restart `stratum up` on
    each. A worker's config file cannot set the block.

!!! warning "An explicit `devices` list must include the node IP's interface"
    NodePorts answer on every address of the interfaces Cilium attaches to.
    That includes the control-plane virtual IP (VIP) on the node that holds
    it. With auto-detection, NodePorts answer on the node IP, even a node IP
    on a loopback interface. With an explicit list, NodePorts work only on the
    listed interfaces. Leave a management network out of the list to keep
    NodePorts off it, but list the interface that carries the node IP, or
    Services break on that address.

!!! warning "Changing the routing mode interrupts pod traffic"
    Switching between `tunnel` and `native` on a running cluster restarts
    every agent onto a different datapath. Pods lose connectivity across nodes
    until every agent has restarted. Choose the routing mode before the first
    controller starts.
