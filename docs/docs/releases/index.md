# Releases & Support

## Version string

```
v1.36.4-stratum.3
 │       │
 │       └── the Stratum build number for this Kubernetes patch
 └────────── the Kubernetes version this release is built from
```

The Kubernetes version leads because it is the question asked most often and it
determines the support window. The suffix increments when Stratum ships a change
against an unchanged Kubernetes build.

```bash
stratum version
```

## What a release contains

Every release is the same single artifact, for amd64 and arm64, both built
natively. Each carries its own component binaries and images, so a release is
self-describing: the artifact *is* the version.

Published alongside it: an SBOM, signatures, and provenance for the build.

## Support model

Stratum follows upstream Kubernetes rather than setting its own cadence. Upstream
publishes a minor roughly every four months and maintains each for about fourteen,
so roughly three minors are supported at any time.

A Stratum release for a given Kubernetes minor lands **behind** the upstream date,
deliberately: each build is validated — conformance, HA failover, upgrade and
restore — before it is published. Shipping late with evidence is preferred to
shipping on the upstream date without it.

Within a supported minor, patch releases pick up upstream Kubernetes and etcd
patches including CVE fixes, and correct defects in Stratum's own code.

!!! warning "Pre-release"
    Stratum has not cut its first tagged release. The version scheme and support
    model described here apply **from the first tagged release onward**. Until
    then, treat every build as a development snapshot with no support window and
    no upgrade guarantee.

## Upgrading between releases

See [Upgrades & Rollback](../cluster-admin/upgrades.md). The short version:
one minor at a time, no downgrades, no cross-major changes, and the tooling
refuses rather than letting you find out afterwards.

### Changes awaiting release

`stratum uninstall` refuses to run where it can't see the host's processes,
such as a container started without `--pid=host`, because there it can't tell
whether another kubelet or container runtime shares what it would remove.
`--assume-no-foreign-runtimes` overrides that once you have checked the host.
It also keeps `/etc/cni/net.d` when another kubelet or runtime is running. See
[Uninstall](../installation/index.md#uninstall).

`stratum uninstall` no longer fails on hosts whose bpftool can't list the root
cgroup's programs, such as bpftool 7.4 and earlier on a kernel with tcx. It
checks the kernel's loaded programs instead, and still reports a Cilium cgroup
program that stays loaded. See [Uninstall](../installation/index.md#uninstall).

`stratum upgrade --from` gives the upgrade binary the installed binary's
SELinux label before handing off to it, so systemd can execute it wherever it
was staged; `--dry-run` and `--check` leave the label alone. Stratum also
registers that label for `stratum*` files in `/var/lib/stratum/upgrade`, so
`restorecon` gives a staged binary a label systemd can execute. Copying a file
in does not label it: run `restorecon` or `chcon` before running it directly
from a systemd unit. On a node with SELinux, the first start of this release
registers the new file context and relabels `/var/lib/stratum` once, as a
change to Stratum's file contexts always does. See
[Upgrades & Rollback](../cluster-admin/upgrades.md#upgrade-one-node).

The CIS profile and its auditor are part of Stratum PRO. The engine keeps the
profile seam, the controls that are on in every cluster and the mechanisms a
profile is built from, but registers no profile of its own.
`stratum up --profile cis`, and a node whose saved state records the `cis`
profile, refuses to start before it stages a payload or rewrites a file, and an
upgrade of a `cis` node to this engine release refuses before swapping any
binary; move such a node onto a Stratum PRO binary with
`stratum upgrade --from`, one node at a time. `stratum hardening check`,
`stratum hardening encrypt-existing` and the MCP tools `hardening_check` and
`hardening_encrypt_existing` are not in this engine, the dashboard has no
Hardening screen, and `/api/v1/config` has no `cisProfile` field. See
[The CIS profile and `hardening check`](../cluster-admin/hardening.md#the-cis-profile-and-hardening-check)
and [Stratum PRO](../pro/index.md).

`stratum upgrade --cluster` imports the upgrade binary's images on every Ready
node before it upgrades the first one. The add-ons roll out to every node as
soon as one control plane is upgraded, and a node still on the previous release
had no way to get their images: the mirror serves no anonymous pulls and an
airgapped node reaches no registry, so the new Cilium pods could not start
there and that node's drain stalled. The run imports them only when it is
started from a release with this change, so start it from the new binary. See
[Add-ons during a rolling upgrade](../appendices/upgrade-reference.md#add-ons-during-a-rolling-upgrade).

A `HelmChart` can take values from Secrets in its namespace with
`spec.valuesSecrets`, so a password need not sit in the `HelmChart`, where
anyone who can read it can read the password. Only a `HelmChart` in a
privileged namespace (`kube-system` by default) may use it. See
[Values from Secrets](../cluster-admin/hardening.md#values-from-secrets).
Earlier releases do not know the field: an operator on one upgrades such a
chart without the Secret values and drops the field, during a rolling upgrade
or after a rollback. See
[HelmCharts that take values from Secrets](../appendices/upgrade-reference.md#helmcharts-that-take-values-from-secrets).

A cluster upgrade no longer leaves a node without Stratum's SELinux labels behind. Before it drains
any node, it checks every Ready node that still needs this build for the
container-selinux, policycoreutils, policycoreutils-python-utils and
libselinux-utils packages, and refuses to start — naming the nodes — when an
enforcing node, or one whose profile requires SELinux, lacks any of them.
Install the packages on the named nodes, or set `spec.selinux: false` in the
node's config file (a node whose profile requires SELinux has no opt-out), and
re-run. A permissive node without the packages is not refused, and is warned
when it next starts. `upgrade --check` reports the same refusal as exit code
11. See
[SELinux confinement](../appendices/upgrade-reference.md#selinux-confinement).

The dashboard's node shell is on by default. A cluster whose config leaves out
`spec.dashboard.enableNodeShell` offers a root shell on its nodes to any
dashboard user whose RBAC can create privileged pods, the `cluster-admin`
sign-in token included, from the first restart on this release. Set
`enableNodeShell: false` on every control plane before upgrading to keep it
withheld. See
[The dashboard](../cluster-admin/index.md#the-dashboard).

Cluster certificate commands now stop gracefully on the first SIGHUP, as they
do on SIGINT and SIGTERM. `stratum certs rotate-ca --cluster` and
`stratum certs migrate-etcd-ca --cluster` stop at the next step boundary and
allow cleanup to run; previously SIGHUP terminated them immediately. A second
signal forces termination. Follow the command's recovery guidance before
resuming. See the [certificate lifecycle upgrade notes](../cluster-admin/certificates.md#upgrading-from-an-earlier-release).

`stratum certs migrate-etcd-ca --cluster` now waits, with pauses that add up to
about 90 seconds, for a member whose etcd refused the connection or accepted it
but did not answer TLS — what a just-power-cycled control plane looks like
before it rejoins raft — and, if it still refuses, names the steps to take. See
[the etcd trust domain](../cluster-admin/hardening.md#etcds-trust-domain).

The kubelets have their own client CA. On a cluster created by this release,
a certificate the cluster root signs, `admin`'s included, no longer reaches a
kubelet's API, and the API servers reach the kubelets with an RBAC identity
rather than `system:masters`. A cluster created earlier keeps the cluster root
as its kubelets' client trust until `stratum certs migrate-kubelet-client-ca
--cluster --yes` moves it. See the
[certificate lifecycle upgrade notes](../cluster-admin/certificates.md#upgrading-from-an-earlier-release).

kube-vip no longer runs as `system:masters`. Its certificate names the user
`system:kube-vip` with no group, and the Role `kube-system/stratum:kube-vip`
grants it only its election Lease. Its pod no longer mounts the directory
holding the admin kubeconfig. See
[kube-vip's identity](../cluster-admin/hardening.md#kube-vips-identity).

kube-scheduler's pod mounts only its own kubeconfig, which moves to
`/var/lib/stratum/kubeconfig/scheduler/`. It no longer mounts the PKI
directory or the directory holding the admin kubeconfigs. An upgrade writes the
new file before restarting kube-scheduler. See
[kube-scheduler's files](../cluster-admin/hardening.md#kube-schedulers-files).

NodePorts answer on every address of the interfaces Cilium attaches to, as
kube-proxy's do, including a kube-vip VIP. Before, Cilium served them on each
node's primary address only, so a NodePort reached through the VIP answered
only from the controller holding it. Upgrading restarts the Cilium agents
once, one node at a time.

On hosts booted without systemd, `/` was not a shared mount, and containerd
refused the host-path mounts Cilium's pods make ("not a shared or slave
mount"), so the node never became Ready. `stratum up`
now makes `/` a shared mount with `mount --make-rshared /` when it is private;
a root that is already shared, or a slave, is left alone, and a failing mount
warns without stopping the start.

Every cluster runs a dashboard, on NodePort `30443` of every node unless
`spec.dashboard` moves or disables it. A cluster where another Service already
holds that port keeps the other Service and serves the dashboard on a ClusterIP until the port is free. Set
`spec.dashboard.nodePort` on every control plane to give the dashboard a
NodePort of its own. See
[Connect to the dashboard](../cluster-admin/index.md#connect-to-the-dashboard).

`admin.conf` is no longer `system:masters`. Its certificate carries the group
`stratum:cluster-admins`, which the ClusterRoleBinding `stratum:cluster-admins`
binds to `cluster-admin`, so deleting that binding revokes every `admin.conf`
without a CA rotation. A new root-only `super-admin.conf` keeps
`system:masters` for Stratum's own clients and for recovery. See
[admin.conf and super-admin.conf](../cluster-admin/hardening.md#adminconf-and-super-adminconf).

An upgraded cluster moves off `system:masters` on its own. On each control
plane's first start of this release, once the bindings the new certificates
need exist, Stratum reissues every leaf and restarts that control plane's
components, one control plane at a time. Copies of `admin.conf` made before the
upgrade keep `system:masters` until they expire or the cluster CA is rotated. See
[Upgraded clusters and system:masters](../cluster-admin/hardening.md#upgraded-clusters-and-systemmasters).

HelmCharts whose `spec.repo` is an `oci://` reference install. Previously every one
failed with `unable to lookup chart ..., missing registry client`; only
`http(s)://` repositories and `chartContent` worked.

`stratum uninstall` now removes the local-path provisioner's volume directory
`/opt/local-path-provisioner`, which `stratum up` creates on SELinux hosts and
the provisioner creates on its first volume, and which earlier uninstalls left
behind. It is removed only when it is an empty plain directory: when it holds
anything, such as the data of volumes the provisioner made, or is a mount point
or a symlink, uninstall keeps it and prints the path and the reason.

etcd's data directory `/var/lib/stratum/etcd`, and the copies a snapshot restore
keeps beside it, are now 0700 on every cluster. A host tool that reads them
without root, such as a backup or monitoring agent, loses access once the node
restarts on this release. Run such a tool as root, or give it access another
way, before upgrading.

`stratum uninstall --yes` removes the `OLD_CILIUM_*` iptables and ip6tables
chains a Cilium agent leaves behind when it stops after renaming its chains
and before deleting the renamed ones; previously those chains survived the
uninstall and a re-run reported the node clean. The same uninstall no longer
saves `/run/stratum-uninstall-netns.json` as incomplete because a container or
pod sandbox was removed between listing it and reading its spec: one that a
second listing no longer names is gone, with no namespace to detach.
Previously one such container made every re-run fail with the same error until
a reboot.

A HelmChart fetched from an HTTP(S) repository now authenticates against the
operator's `/var/lib/stratum/helm/repositories.yaml`: the entry whose URL
names the same repository supplies the username and password and the CA and
client-certificate files, and its credentials reach another host only when the
entry sets `pass_credentials_all: true`. Previously the credentials of a
`spec.repo` fetch were never sent, so a private HTTP repository was reachable
only inline (`chartContent`) or as a named `repo/chart` reference whose index
was already cached.

**Breaking:** `stratum mcp --transport http` requires `--token-file`, and
refuses to start without it. Loopback was no boundary: any local user or
`hostNetwork` pod on the node could call the server's tools, which run as root.
Before upgrading a node that serves MCP over HTTP, create a token file of mode
`0600`, owned by the user that runs the server, holding at least 32 characters (`openssl rand -hex 32`), add
`--token-file` to the command or unit that starts the server, and have each
client send `Authorization: Bearer <token>`. The stdio transport is unchanged.
See [Running it over HTTP](../cluster-admin/mcp.md#running-it-over-http).
