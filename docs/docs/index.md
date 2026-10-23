# Stratum Kubernetes Engine

Stratum Kubernetes Engine (SKE) is a Kubernetes distribution that ships as one
executable, `stratum`, that installs a cluster without a container registry.
This page lists what a cluster runs and where to start reading.

On a Linux host where you have root access, one command brings a single node to
Ready:

```bash
sudo /usr/local/bin/stratum up
```

## What a cluster runs

| Component | Role |
|---|---|
| Kubernetes | kube-apiserver, kube-controller-manager, kube-scheduler, kubelet and kubectl |
| etcd | the cluster datastore |
| containerd and runc | the container runtime |
| kube-vip | the API server's virtual IP (VIP), on control-plane nodes, when you configure one |
| Cilium | the Container Network Interface (CNI) plugin: pod networking, network policy and Service load balancing |
| CoreDNS | cluster DNS |
| local-path | the default StorageClass, backed by a directory on each node |
| Stratum dashboard | a web console for the cluster, on port `30443` of every node |

To print the version of each component a binary carries, run
`stratum version --components`.

## Where to start

<div class="grid cards" markdown>

-   ### [Quick Start](quick-start/index.md)

    Bring up a single node and add a worker. Start here if you want a cluster
    in front of you before you read anything else.

-   ### [Overview](overview/index.md)

    How a node runs, the two node roles, where configuration and state live,
    and how nodes join a cluster.

</div>

<div class="grid cards" markdown>

-   ### [Installation](installation/index.md)

    The artifact, prerequisites, topologies and the configuration file.

-   ### [Cluster Administration](cluster-admin/index.md)

    Day-2 operations: high availability, upgrades and rollback, snapshots and
    restore, and the security controls every cluster runs.

</div>

The other sections:

- [Container Runtime](runtime/index.md): containerd, runc and registry mirrors.
- [Networking & Storage](networking-storage/index.md): Cilium, LoadBalancer
  Services, CoreDNS, local-path and the network settings.
- [Releases & Support](releases/index.md): the version string and the support
  model.
- [Appendices](appendices/index.md): on-disk paths and names, the snapshot
  rules and a glossary. For commands and flags, run `stratum --help`.

These pages document the open-source engine. [Stratum PRO](pro/index.md) is a
commercial edition of the same engine, in a binary of its own.
