# Overview

This page explains how a Stratum node works: what the `stratum` binary carries,
which processes run on a node, the two node roles, where configuration and state
live, and how nodes join a cluster. Read it before you plan a cluster, or when
you need to know where to look for a symptom. To bring up a node first, see
[Quick Start](../quick-start/index.md).

## One binary carries the cluster

The `stratum` executable has a ZIP *payload* appended to it. The payload holds
the component binaries, such as kubelet and containerd, and an Open Container
Initiative (OCI) image archive with the control-plane and add-on images.

`stratum up` unpacks the payload to `/var/lib/stratum` and imports the images
into containerd. A node reaches Ready without pulling an image from a registry.
For what the artifact contains, see [The artifact](../installation/index.md#the-artifact).

## How a node runs

Stratum runs the control plane as *static pods*: pods that kubelet starts from
manifest files on the node, not from the API server. See
[Static Pods](https://kubernetes.io/docs/tasks/configure-pod-container/static-pod/)
in the Kubernetes documentation. The control plane is etcd,
kube-apiserver, kube-controller-manager and kube-scheduler, plus kube-vip when
the cluster has a virtual IP (VIP). Each one shows as a mirror pod in
`kubectl get pods -n kube-system`.

containerd and kubelet run on the host as child processes of `stratum up`. On a
control-plane node, so does the Stratum *operator*, which applies the cluster's
add-ons and approves node certificates. The *supervisor* in `stratum up`
restarts each process with backoff when it exits.

`stratum up` installs and enables one systemd unit, `stratum.service`, which
runs `stratum up` again after a reboot. The supervisor then starts kubelet, and
kubelet starts the static pods. Stratum installs no unit per component. See
[Reboot persistence](../installation/index.md#reboot-persistence).

On every start of a control-plane node, Stratum writes the control-plane
manifests in `/var/lib/stratum/config/manifests/` from the node's configuration
and state. Do not edit them by hand, because the next start overwrites your
change. Change the [configuration](../installation/configuration.md#configuration)
instead.

To find a component's logs, see [Logs](../cluster-admin/index.md#logs).

## Node roles

You choose a node's role when you bring it up:

| Role | Command | Runs |
|---|---|---|
| Node, the default | `stratum up` | the control plane and workloads; the node registers as schedulable |
| Worker | `stratum up --worker` | workloads only; the node joins an existing cluster |

A *dedicated control plane* is a default node that you taint so that it runs no
workloads. See [Topologies](../installation/index.md#topologies).

## Configuration and state

| Path | What it is |
|---|---|
| `/etc/stratum/config.yaml` | the *ClusterConfig*, the file you write: network ranges, VIP, storage, profile, operator, snapshots, certificate renewal, dashboard and SELinux settings. Stratum parses it strictly, so a misspelled key is an error. |
| `/var/lib/stratum/config/node.yaml` | the *NodeState*, which Stratum writes: the node's role, addresses and etcd membership. Stratum writes it when the node first comes up, and `stratum snapshot restore` and `stratum snapshot rejoin` update it. |

Stratum generates every component's flags from these two files. For the keys and
their defaults, see [Configuration](../installation/configuration.md#configuration) and
[Paths and Names](../appendices/paths-and-names.md).

## How nodes join a cluster

The first control-plane node starts the cluster with `stratum up`. On that node,
`stratum token create` mints a join token and prints the complete command for a
new worker. `stratum token create --control-plane` prints the command for a new
control-plane node, which joins with `stratum up --join`.

Each join command carries a *CA pin*: the `sha256:` hash of the cluster's
certificate authority (CA). The joining node checks the CA it receives against
the pin, and against a signature made with the join token. If either check
fails, the join stops. This is how the node confirms that it reached your
cluster.

A worker's kubelet then requests its certificates from the cluster. The Stratum
operator approves a client certificate only for a node name that no other node
has claimed, and a serving certificate only for addresses the node owns. A
client certificate request it does not approve stays pending; see
[Topologies](../installation/index.md#topologies) for how to read and resolve one.

To add a worker, see [Add a worker](../quick-start/index.md#5-add-a-worker). To
add control-plane nodes, see
[Growing to three controllers](../cluster-admin/high-availability.md#growing-to-three-controllers).

## What Stratum does not include

- kube-proxy. Cilium handles Services; see
  [Networking & Storage](../networking-storage/index.md).
- A second Container Network Interface (CNI) plugin, or a datastore other
  than etcd.
- Machine provisioning over SSH. `stratum upgrade --cluster` upgrades an
  existing cluster through the Kubernetes API. Provision the machines with your
  own tooling.
- Management of more than one cluster. The dashboard shows the cluster that
  serves it; see [Stratum PRO](../pro/index.md).

## Next

- [Quick Start](../quick-start/index.md): a single node and a worker
- [Installation](../installation/index.md): prerequisites, topologies and configuration
- [High Availability](../cluster-admin/high-availability.md): three controllers and a VIP
- [Glossary](../appendices/glossary.md): the terms these pages use
