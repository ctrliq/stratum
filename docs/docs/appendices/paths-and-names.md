---
description: "Where Stratum keeps things on a node, the Kubernetes objects it creates, and its default timings."
---

# Paths and Names

This page lists the files Stratum reads and writes on a node, the configuration
keys it accepts, the Kubernetes objects it creates and its default timings. Use
it to find where something lives or what a default is. Most rows link to the
page that explains them.

## Configuration you write

| Path | Contents |
|---|---|
| `/etc/stratum/config.yaml` | the declarative ClusterConfig; see [Configuration](../installation/configuration.md#the-configuration-file) |
| `/etc/stratum/stratum.env` | optional environment for the unit, such as S3 credentials; you create it, root-owned, mode `0600`; see [Environment for the unit](../installation/configuration.md#environment-for-the-unit) |
| `/etc/stratum/registries.yaml` | registry mirrors, in the RKE2/k3s format; you create it, root-owned, mode `0600`; see [Registry mirrors](../runtime/index.md#registry-mirrors) |
| `/etc/stratum/snapshot.conf` | snapshot retention and S3 target, written by `stratum snapshot schedule`; created with mode `0600` and may hold S3 credentials, so keep an existing file root-owned and `0600`; see [Where retention and the upload target come from](snapshot-reference.md#where-retention-and-the-upload-target-come-from) |
| `/var/lib/stratum/manifests/` | manifests the operator applies; see [Deploying manifests](../cluster-admin/index.md#deploying-manifests) |

## Node state

| Path | Contents |
|---|---|
| `/var/lib/stratum/` | all state: PKI, kubeconfigs, binaries, images, node state |
| `/var/lib/stratum/bin/` | the payload's binaries, including the ones `stratum kubectl` and `stratum etcdctl` run |
| `/var/lib/stratum/images/` | the payload's image archives |
| `/var/lib/stratum/pki/` | certificates and keys, and any secret the node's profile shares between control planes |
| `/var/lib/stratum/config/node.yaml` | the persisted NodeState |
| `/var/lib/stratum/config/manifests/*.yaml` | the rendered control-plane static pod manifests; see [Overview](../overview/index.md) |
| `/var/lib/stratum/kubeconfig/admin.conf` | the admin kubeconfig, on control planes; revocable, see [admin.conf and super-admin.conf](../cluster-admin/hardening.md#adminconf-and-super-adminconf) |
| `/var/lib/stratum/kubeconfig/super-admin.conf` | the `system:masters` kubeconfig Stratum itself uses, on control planes |
| `/var/lib/stratum/upgrade/in-progress.json` | present from an upgrade's first change until its health gate passes; see [What an upgrade leaves behind](upgrade-reference.md#what-an-upgrade-leaves-behind) |
| `<file>.bak-<stamp>` | the file an upgrade replaced; see [What an upgrade leaves behind](upgrade-reference.md#what-an-upgrade-leaves-behind) |
| `/var/backups/etcd/` | local etcd snapshots, unless `spec.snapshots.dir` sets another directory; pre-upgrade snapshots always go here. Local retention prunes only `etcd-snapshot-<host>-<stamp>.db`, and `snapshot list` shows only local files ending in `.db`; see [Snapshot files](snapshot-reference.md#snapshot-files) |
| `/var/lib/stratum/etcd.pre-restore-<stamp>/`, `/var/lib/stratum/etcd.failed-restore-<stamp>/` | the copies of etcd's data a snapshot restore keeps; `stratum snapshot prune-restores --yes` removes them; see [What restore does to the datastore](snapshot-reference.md#what-restore-does-to-the-datastore) |
| `/var/lib/stratum/log/<component>.log` | logs of the supervised components; see [Logs](../cluster-admin/index.md#logs) |
| `/run/stratum/stratum.sock` | the control socket (root only) |
| `/run/stratum/containerd/containerd.sock` | the CRI endpoint |

## Files Stratum writes outside its own directories

| Path | Written | See |
|---|---|---|
| `/etc/systemd/system/stratum.service` | on systemd hosts | [Reboot persistence](../installation/index.md#reboot-persistence) |
| `/etc/sysctl.d/99-stratum.conf` | on every start | [Host settings](../installation/index.md#host-settings) |
| `/etc/sysctl.d/99-stratum-<profile>.conf` | under a profile that declares sysctls | [Host settings](../installation/index.md#host-settings) |
| `/etc/modules-load.d/k8s.conf` | on every start | [Host settings](../installation/index.md#host-settings) |
| `/etc/containerd/certs.d/<registry>/hosts.toml` | when `registries.yaml` has mirrors, or configs entries with auth or tls | [Registry mirrors](../runtime/index.md#registry-mirrors) |
| `/etc/containerd/conf.d/stratum-registry.toml` | when `registries.yaml` has mirrors, or configs entries with auth or tls | [Registry mirrors](../runtime/index.md#registry-mirrors) |

## Configuration keys

Under `spec` in `/etc/stratum/config.yaml`:

| Key | Default | See |
|---|---|---|
| `certRenew` | `true` | [Leaf certificates](../cluster-admin/certificates.md#leaf-certificates) |
| `dashboard.enabled` | `true` while the operator runs | [The dashboard](../cluster-admin/index.md#the-dashboard) |
| `dashboard.nodePort` | `30443`; `0` for a ClusterIP only | [The dashboard](../cluster-admin/index.md#the-dashboard) |
| `dashboard.publicURL` | none | [The dashboard](../cluster-admin/index.md#the-dashboard) |
| `dashboard.tlsSecretName` | none: a certificate from the dashboard CA | [The dashboard](../cluster-admin/index.md#the-dashboard) |
| `dashboard.enableNodeShell` | `true`; a profile can make it `false` | [The dashboard](../cluster-admin/index.md#the-dashboard) |
| `dashboard.tokenSecrets` | `true` | [The dashboard](../cluster-admin/index.md#the-dashboard) |
| `snapshots.intervalHours` | `12` | [Schedule settings](../cluster-admin/snapshot-restore.md#schedule-settings) |
| `snapshots.disabled` | `false` | [Schedule settings](../cluster-admin/snapshot-restore.md#schedule-settings) |
| `snapshots.dir` | `/var/backups/etcd`; a relative path or one with a `..` element is ignored | [The snapshot directory](snapshot-reference.md#the-snapshot-directory) |
| `snapshots.retain` | `5` | [Where retention and the upload target come from](snapshot-reference.md#where-retention-and-the-upload-target-come-from) |
| `snapshots.s3` | none | [Where retention and the upload target come from](snapshot-reference.md#where-retention-and-the-upload-target-come-from) |

## Kubernetes objects and labels

| Name | Kind | See |
|---|---|---|
| `stratum.ciq.com/control-plane=true` | Node label | [What a cluster upgrade creates](upgrade-reference.md#what-a-cluster-upgrade-creates) |
| `stratum.ciq.com/k8s-version` | Node annotation | [What a cluster upgrade creates](upgrade-reference.md#what-a-cluster-upgrade-creates) |
| `stratum.ciq.com/version` | Node annotation | [What a cluster upgrade creates](upgrade-reference.md#what-a-cluster-upgrade-creates) |
| `stratum.ciq.com/upgrade-in-progress` | Node annotation | [What a cluster upgrade creates](upgrade-reference.md#what-a-cluster-upgrade-creates) |
| `kube-system/stratum-cluster-upgrade` | Lease | [What a cluster upgrade creates](upgrade-reference.md#what-a-cluster-upgrade-creates) |
| `stratum-join-maintenance-fence` | ValidatingAdmissionPolicy and ValidatingAdmissionPolicyBinding | [The join token's maintenance fence](../cluster-admin/hardening.md#the-join-tokens-maintenance-fence) |
| `stratum-upgrade-<node>` | Job, in `kube-system` | [What a cluster upgrade creates](upgrade-reference.md#what-a-cluster-upgrade-creates) |
| `stratum.ciq.com/upgrade=<node>` | Job label | [What a cluster upgrade creates](upgrade-reference.md#what-a-cluster-upgrade-creates) |
| `stratum.ciq.com/stack=<stack>` | label on objects applied from a manifest stack | [Deploying manifests](../cluster-admin/index.md#deploying-manifests) |
| `stratum-dashboard` | Namespace, Deployment and Service | [The dashboard](../cluster-admin/index.md#the-dashboard) |
| `stratum-dashboard/stratum-dashboard-tls` | Secret | [The dashboard](../cluster-admin/index.md#the-dashboard) |
| `stratum-dashboard/stratum-dashboard-session` | Secret | [The dashboard](../cluster-admin/index.md#the-dashboard) |
| `kube-system/stratum-dashboard-ca` | Secret, the dashboard's own CA | [The dashboard](../cluster-admin/index.md#certificate) |
| `stratum-dashboard/stratum-dashboard-admin-token`, `stratum-dashboard-viewer-token` | Secret, sign-in tokens | [The dashboard](../cluster-admin/index.md#the-dashboard) |
| `stratum-dashboard-admin`, `stratum-dashboard-viewer` | ServiceAccount and ClusterRoleBinding (`cluster-admin`, `view`) | [The dashboard](../cluster-admin/index.md#the-dashboard) |

## Timings and defaults

| What | Value | See |
|---|---|---|
| Leaf certificate lifetime | 365 days | [Leaf certificates](../cluster-admin/certificates.md#leaf-certificates) |
| Automatic leaf rotation | daily check, rotates within 45 days of expiry | [Leaf certificates](../cluster-admin/certificates.md#leaf-certificates) |
| Scheduled snapshot interval | 12 hours | [Schedule settings](../cluster-admin/snapshot-restore.md#schedule-settings) |
| Snapshots kept per control plane | 5 | [Where retention and the upload target come from](snapshot-reference.md#where-retention-and-the-upload-target-come-from) |
| Manifest deployer pass | every 20 seconds; unchanged files re-applied every 5 minutes | [Deploying manifests](../cluster-admin/index.md#deploying-manifests) |
| Cluster-upgrade lease | 60 seconds, renewed every 20 | [What a cluster upgrade creates](upgrade-reference.md#what-a-cluster-upgrade-creates) |
| Stale restore download temp files | older than 15 minutes, removed by the next `s3://` restore of the same name | [Downloading an s3:// snapshot](snapshot-reference.md#downloading-an-s3-snapshot) |
| Rejoin gives up | after 5 minutes without progress, or 30 minutes in all | [Learners and the wait](snapshot-reference.md#learners-and-the-wait) |
| Rollback wait before the health gate | 15 seconds (`--soak`) | [Flags](upgrade-reference.md#flags) |
