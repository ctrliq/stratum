---
description: "Terms this documentation uses in a specific way."
---

# Glossary

This page defines the terms these pages use in a specific way, and the acronyms
they use. Where a term has a page that explains it in context, the entry links
to it.

**Airgap**: installing with no registry reachable. Stratum needs no separate
mode for it, because the artifact carries its images.

**Artifact**: the single `stratum` executable with its appended payload. It
holds everything a node runs, so a node fetches nothing to install.

**CA pin**: the `sha256:` hash printed in a join command. A joining node checks
the authority it is offered against this, so it can tell your cluster from
something impersonating it.

**Certificate key**: the additional secret `token create --control-plane`
writes to a 0600 file, printing its path but not the key. It lets a joining
controller decrypt the shared CA material; pass it with
`--certificate-key-file`.

**CIS**: the Center for Internet Security, which publishes the
[CIS Kubernetes Benchmark](https://www.cisecurity.org/benchmark/kubernetes), its
hardening standard for Kubernetes.

**CIS profile**: the profile that configures a cluster to the CIS
Kubernetes Benchmark. It ships with [Stratum PRO](../pro/index.md); see
[Hardening](../cluster-admin/hardening.md#the-cis-profile-and-hardening-check).

**ClusterConfig**: the declarative configuration at `/etc/stratum/config.yaml`.
Stratum parses it strictly, so an unknown key is an error.

**Control socket**: `/run/stratum/stratum.sock`, root-only, speaking
`restart`/`stop`/`start`/`status`. Certificate rotation, snapshot restore and
upgrades act on individual components through it.

**CRI**: the Container Runtime Interface, the API kubelet uses to run
containers. On Stratum, containerd serves it at
`/run/stratum/containerd/containerd.sock`.

**Dedicated control plane**: a node the operator has tainted
(`node-role.kubernetes.io/control-plane:NoSchedule`), so it runs the control
plane but no workloads.

**etcd CA**: etcd's own certificate authority, separate from the Kubernetes
root, so a certificate issued for the cluster is not a valid etcd client. See
[etcd's trust domain](../cluster-admin/hardening.md#etcds-trust-domain).

**Learner**: an etcd member with no vote. A rejoining control plane joins as a
learner and becomes a voter once it has caught up. See
[Rejoin](snapshot-reference.md#rejoin).

**Node**: the `stratum up` default: one machine that is both control plane (run
as kubelet-managed static pods) and workload host, registered and schedulable.

**NodeState**: the persisted per-node facts at
`/var/lib/stratum/config/node.yaml`: role, addresses, etcd membership.

**Payload**: the ZIP appended to the executable, holding the component binaries
and the OCI image archive. `stratum stage` unpacks it.

**Profile**: `--profile <name>` or `spec.profile`. A named set of hardening
settings the binary registers: apiserver flags, kubelet settings, config files,
shared secrets and sysctls. The engine renders what a profile declares and
ships no profile of its own. The CIS profile is in
[Stratum PRO](../pro/index.md).

**Raft**: the consensus algorithm etcd members use to agree on the cluster's
data. A member that has rejoined raft takes part in that agreement again.

**Rendered manifests**: the control-plane static pod manifests, generated from
configuration on every start and written to
`/var/lib/stratum/config/manifests/<name>.yaml`. Kubelet runs these.

**Seed**: the control plane a cluster is rebuilt from, such as the one that ran
`stratum snapshot restore`. The other control planes rejoin it. See
[Rejoining a member](../cluster-admin/snapshot-restore.md#rejoining-a-member).

**Snapshot directory**: the local directory etcd snapshots go to,
`/var/backups/etcd` unless `spec.snapshots.dir` sets another. See
[The snapshot directory](snapshot-reference.md#the-snapshot-directory).

**Supervisor**: the process model. Components run as child processes of the
running `stratum` command, restarted with backoff and driven through the
control socket. Stratum installs one systemd unit, `stratum.service`, which
restarts the supervisor after a reboot.

**Trust bundle**: the set of certificate authorities trusted at a given time, as
distinct from the single authority that signs. Rotation widens the
bundle, moves the signer, then narrows it.

**VIP**: the virtual IP that kube-vip serves on the controllers. Workers and
`kubectl` reach the cluster at this one address. A LoadBalancer Service's
address is a different one: Cilium's LB-IPAM assigns it and a Cilium agent
announces it. See
[LoadBalancer Services](../networking-storage/index.md#loadbalancer-services).

**Worker**: `stratum up --worker`. A workload-only node with no control plane.
