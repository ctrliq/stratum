---
description: "The security controls every Stratum cluster runs. The CIS profile is in Stratum PRO."
---

# Hardening

This page describes the security controls every Stratum cluster runs, whatever
its profile, and the steps that keep them in force: the separate certificate
authorities, the administrator kubeconfigs, the join token's limits and the
operator's HelmChart installs. Use it to check what a credential can reach, to
revoke one, or to recover from a hold the operator reports. The hardening
profile that adds more controls ships with [Stratum PRO](../pro/index.md).

## The CIS profile and `hardening check`

The Center for Internet Security (*CIS*) publishes the
[CIS Kubernetes Benchmark](https://www.cisecurity.org/benchmark/kubernetes),
its hardening standard for Kubernetes clusters. The *CIS profile* is a Stratum
profile that configures a cluster to that benchmark. It and the
`hardening check` auditor that verifies it ship with
[Stratum PRO](../pro/index.md).

The engine keeps the profile mechanism: `--profile <name>` and `spec.profile`
select a profile the binary registers. A node that names a profile its binary
does not carry refuses to start.

Without a profile, the apiserver writes no audit log, and etcd stores Secrets
unencrypted. Both come with the CIS profile.
[etcd's trust domain](#etcds-trust-domain) keeps etcd's keyspace from every
certificate except etcd's and the apiserver's. Anyone who can read etcd's data
directory or a snapshot on a control plane can still read every Secret, so
protect those files and snapshots, or run the CIS profile.

## Pod security contexts

The fields of a pod's or container's `securityContext`, as described in the
Kubernetes task
[Configure a Security Context for a Pod or Container](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/),
work on Stratum as follows.

| Field | On Stratum |
|---|---|
| `runAsUser`, `runAsGroup`, `supplementalGroups`, `fsGroup` | Applied. On volumes the kubelet manages ownership for, such as `emptyDir`, `fsGroup` owns new files and sets the volume's setgid bit. The default `local-path` StorageClass provisions `hostPath` volumes, which `fsGroup` does not change. |
| `runAsNonRoot` | Applied: a container whose image runs as root is refused unless `runAsUser` names another user. |
| `fsGroupChangePolicy` | Accepted, and on an `emptyDir` the volume root takes the `fsGroup`. That `OnRootMismatch` skips the recursive change on an already-matching volume is not verified. Like `fsGroup`, it does not apply to `local-path` volumes. |
| `supplementalGroupsPolicy` | Applied. `Strict` leaves out the groups the image's `/etc/group` gives the user; `Merge`, the default, includes them. |
| `privileged`, `capabilities`, `allowPrivilegeEscalation`, `readOnlyRootFilesystem` | Applied. |
| `seccompProfile: RuntimeDefault`, `Unconfined` | Applied. |
| `seccompProfile: Localhost` | Applied. Profiles live under the kubelet's root directory, which on Stratum is `/var/lib/stratum/kubelet/seccomp/`, not `/var/lib/kubelet/seccomp/`. |
| `appArmorProfile` | Only on hosts whose kernel has AppArmor, such as Ubuntu; Rocky Linux and RHEL use SELinux instead. Not verified on an AppArmor host. |
| `procMount` | Applied: `Unmasked` (which needs `hostUsers: false`) leaves `/proc` paths such as `/proc/kcore` unmasked; `Default` masks them. |
| `hostUsers: false` (user namespaces) | Applied where the kernel and the filesystem under `/var/lib/stratum` support ID-mapped mounts (verified on kernel 6.8). |
| `seLinuxOptions` | Applied on a labelled SELinux node, where containers run as `container_t` with their own categories (see [Containers](../installation/configuration.md#containers)). On a host without SELinux, or a node that opted out or lacks the policy packages, accepted but not applied. |
| `seLinuxChangePolicy` | `MountOption` and `Recursive` are accepted. On a labelled SELinux node the kubelet relabels volumes for the pod's label as upstream Kubernetes does; elsewhere there is nothing to relabel. |

To use a Localhost seccomp profile, put the file on every node that may run the
pod, and name it relative to that directory:

```console
# mkdir -p /var/lib/stratum/kubelet/seccomp/profiles

# cp audit.json /var/lib/stratum/kubelet/seccomp/profiles/
```

```yaml
securityContext:
  seccompProfile:
    type: Localhost
    localhostProfile: profiles/audit.json
```

## etcd's trust domain

etcd has **its own certificate authority**, separate from the Kubernetes root.
etcd accepts a client whose certificate chains to the authority it trusts. With
its own CA, a certificate the cluster root signs, such as a kubelet's, cannot
read or write etcd's keyspace, which holds every Secret.

The etcd CA signs two certificates on each control plane: etcd's own, which is
both its server and its peer certificate, and `apiserver-etcd-client`, which the
apiserver uses to reach etcd. Once a cluster is on the etcd CA, those are the
only certificates etcd accepts as a client. Each has full access to the
keyspace, as the apiserver needs. etcd runs without its own users and roles.

etcd also requires client certificate authentication on both its client and
peer ports, and runs with self-signed auto-TLS off on both. The peer port also
checks the certificate's name: it accepts only `CN=etcd`, which etcd's own
certificate carries, so `apiserver-etcd-client` cannot join the etcd cluster as
a member.

A cluster that predates the split trusts the cluster root in etcd. Move it onto
its own etcd CA with
[`certs migrate-etcd-ca`](certificates.md#migrating-an-existing-cluster-onto-the-etcd-ca).

If a controller loses power while that migration runs, the next run keeps
checking the affected member's etcd until it serves. Its pauses add up to about
90 seconds, so the whole wait can take a few minutes. If the run still refuses,
the message says what it saw and what to do:

- Read etcd's log on that node, under `/var/log/pods/kube-system_etcd-*`, and
  re-run with `--resume` once etcd is serving. Remove the node only if its etcd
  does not start.
- If etcd is not listening at all and the log shows a certificate and key
  mismatch, run `sudo /usr/local/bin/stratum certs migrate-etcd-ca --phase=issue-new --yes` on
  that node. It rewrites the pair and restarts etcd. Then re-run with
  `--resume` once etcd is serving.

## The kubelet's client trust

Every kubelet serves an API on `:10250` that can read any pod's logs, run
commands in its containers and forward its ports. It accepts a client only if
the client's certificate chains to a CA in
`/var/lib/stratum/pki/kubelet-client-ca-bundle.crt`.

On a cluster created by this release that file holds **its own CA**,
`kubelet-client-ca`, which signs only `apiserver-kubelet-client` certificates.
The apiserver uses them for `kubectl logs`, `exec`,
`attach`, `port-forward` and the node proxy. Nothing the cluster root signs,
including `admin`, `kube-vip`, every worker's kubelet client and any certificate
the controller-manager issues for an approved request, can authenticate to a
kubelet.

- **Control planes** create the CA with the cluster and share it with joining
  control planes through the encrypted `stratum-certs` Secret, like the other
  CAs.
- **Workers** read the bundle from the `stratum-kubelet-client-ca` ConfigMap in
  `kube-public` when they join. It is published beside `cluster-info`, readable
  the same anonymous way, and fetched over TLS verified by the cluster CA the
  join token pinned. The signed `cluster-info` kubeconfig records whether the
  cluster uses the separate CA. On a split cluster, a missing ConfigMap or a
  denied read stops the join; it never installs the cluster root as kubelet
  client trust.
- **`certs rotate-ca`** rotates this CA with the cluster root, in the same
  phases. `trust-new` makes every kubelet trust the new and the old one, workers
  through the republished ConfigMap. `sign-new` reissues
  `apiserver-kubelet-client` under the new one. `finalize` drops the old one,
  and a rollback restores it. The cluster root never enters a kubelet's bundle.
  A worker with a missing kubelet client-CA bundle refuses a root trust refresh
  until the original bundle is restored, since its mode can no longer be
  inferred safely from that file.

Join a cluster created by this release with this release or later. An earlier
release's worker writes the cluster root into its kubelet's bundle, so it
refuses the apiserver's kubelet client and `kubectl logs` and `exec` to it
fail. An earlier release's control plane refuses the join outright, because the
shared certificates carry a CA it does not know.

A cluster created by an earlier release keeps the cluster root in that file.
Move an earlier cluster onto its own kubelet client CA with
`stratum certs migrate-kubelet-client-ca --cluster --yes`; see
[Migrating an existing cluster onto the kubelet client CA](certificates.md#migrating-an-existing-cluster-onto-the-kubelet-client-ca).

## The front-proxy client key

The apiserver believes the `X-Remote-User` and `X-Remote-Group` headers of any
client that presents a certificate signed by the front-proxy CA with a common
name listed in `--requestheader-allowed-names`. That is how the aggregation
layer passes the caller's identity to an extension API server such as
metrics-server, so it stays on. It also means that
`/var/lib/stratum/pki/front-proxy-client.key`, and `front-proxy-ca.key` that can
sign another, **can act as any user, including `system:masters`**. Treat both as
cluster-admin credentials.

Where they live:

- On every control plane, under `/var/lib/stratum/pki` (0700), owned by root and
  0600. The apiserver uses the client pair to reach extension API servers and
  Stratum's own admission webhook.
- In the `stratum-certs` Secret in `kube-system`, encrypted under the
  certificate-key, so that a joining control plane receives the same CA. Anyone
  who holds both the Secret and the certificate-key holds these keys.
- **Never on a worker.** A worker joins with the cluster root certificate and a
  bootstrap token only.

`stratum certs rotate` and the renewal timer reissue `front-proxy-client` with a
new key; `stratum certs rotate-ca` replaces the front-proxy CA as well. A
leaf rotation does not revoke the old leaf, which stays valid until it expires.
If the key may have leaked, run `certs rotate-ca` through `finalize`, which is
the step that stops the apiserver trusting the old CA.

A key found on a worker, or one that was readable by anyone but root, may
already have been copied: remove or tighten it, then run
`stratum certs rotate-ca --cluster --yes` through `finalize`.


## The apiserver's kubelet client

The apiserver reaches every kubelet's API on `:10250` for `kubectl logs`, `exec`,
`attach`, `port-forward` and the node proxy with `apiserver-kubelet-client`. Its
certificate names the user `kube-apiserver-kubelet-client` and carries **no
group**. Each kubelet authorizes it through the apiserver. The
ClusterRoleBinding `stratum:kube-apiserver-kubelet-admin` grants it the upstream
`system:kubelet-api-admin` role: the kubelet API and nothing else.

Every controller re-creates the binding when it starts, or restores it if its
subjects or role were changed. Deleting it breaks `kubectl logs` and `exec` until
the next controller start.

A control plane upgraded from an earlier release moves to this certificate on
its first start of the new release; see
[Upgraded clusters and system:masters](#upgraded-clusters-and-systemmasters).

## admin.conf and super-admin.conf

Each control plane holds two administrator kubeconfigs, as kubeadm does:

| File | Certificate | Rights | Used by |
|---|---|---|---|
| `/var/lib/stratum/kubeconfig/admin.conf` | `CN=admin`, group `stratum:cluster-admins` | `cluster-admin`, through the ClusterRoleBinding `stratum:cluster-admins` | `stratum kubectl`, `stratum mcp` when given no other kubeconfig, and people |
| `/var/lib/stratum/kubeconfig/super-admin.conf` | `CN=super-admin`, group `system:masters` | everything; RBAC never applies | Stratum itself: the operator, the start-up bindings, certificate rotation, snapshot restore, upgrades |

Both are root-only (0600) and stay on the node they were issued on. Use
`admin.conf` for day-to-day work, and keep `super-admin.conf` for recovery when
RBAC itself is broken.

**Revoking admin.conf.** Delete the binding:

```console
# /usr/local/bin/stratum kubectl --kubeconfig /var/lib/stratum/kubeconfig/super-admin.conf \
  delete clusterrolebinding stratum:cluster-admins
```

Every `admin.conf` on every control plane is refused from then on, without a CA
rotation, and the cluster keeps running on `super-admin.conf`. Stratum creates
the binding once per cluster and records that in the ConfigMap
`kube-system/stratum-cluster-admins-binding`, so a restart or upgrade does not
put it back. The first controller to start claims that ConfigMap before it
creates the binding, and no other controller creates it. If the claiming
controller stops in between, `admin.conf` stays refused, and the other
controllers warn, naming it, until its next `stratum up` finishes the job. To
restore access, create the binding again:

```console
# /usr/local/bin/stratum kubectl --kubeconfig /var/lib/stratum/kubeconfig/super-admin.conf \
  create clusterrolebinding stratum:cluster-admins \
  --clusterrole=cluster-admin --group=stratum:cluster-admins
```

Restoring an etcd snapshot taken before the binding existed creates it again,
matching the restored cluster.

A control plane upgraded from an earlier release gets `super-admin.conf`, and
an `admin.conf` that the binding governs, on its first start of the new
release; see [Upgraded clusters and system:masters](#upgraded-clusters-and-systemmasters).

## kube-vip's identity

On a control plane that holds a [virtual IP (VIP)](../appendices/glossary.md),
kube-vip elects the VIP's holder with the Lease `kube-system/plndr-cp-lock`.
Its certificate names the user `system:kube-vip` and carries **no group**. The
Role and RoleBinding `kube-system/stratum:kube-vip` let that user create the
Lease, and get and update it by name, and nothing else. kube-vip runs as a pod
with `NET_ADMIN` and `NET_RAW`. Its pod mounts only
`/var/lib/stratum/kubeconfig/kube-vip/`, so it cannot read the admin kubeconfig
beside the others.

Every controller with a VIP creates the Role and RoleBinding through its local
apiserver when it starts, and checks them every five minutes, restoring them if
they were changed or deleted, as an etcd restore from an older snapshot does.
Until they exist, kube-vip cannot take the Lease and no node holds the VIP.

A control plane upgraded from an earlier release moves to this certificate on
its first start of the new release; see
[Upgraded clusters and system:masters](#upgraded-clusters-and-systemmasters).

## The join token's maintenance fence

A joining control plane holds the cluster maintenance fence, the Leases
`kube-system/stratum-cluster-maintenance` and
`kube-system/stratum-cluster-upgrade`, around its etcd member-add, with its
bootstrap token. So the Role `kube-system/stratum:certs-reader` also lets the
control-plane bootstrap group (`system:bootstrappers:stratum:control-planes`)
create Leases in `kube-system`, and get, update and delete those two. RBAC
cannot limit a create by name, so the ValidatingAdmissionPolicy
`stratum-join-maintenance-fence` and its binding of the same name hold that
group to its own fence. A control-plane join token may write only those two
Leases, with a lease duration of at most 60 seconds, under a holder that starts with its own
`system:bootstrap:<token-id>/`, and it may never overwrite or delete a Lease
another run holds, even an expired one. A leaked join token therefore cannot
take the fence from a running upgrade, rotation or migration, or create other
Leases. The policy also covers the shared bootstrap group
`system:bootstrappers:stratum:nodes`: an older RoleBinding can still grant that
group lease permissions while it is being narrowed to control-plane tokens.
These restrictions remain in force if that binding update fails or an API
server still caches the old binding. Other identities are unaffected.

The token also cannot attach owner references or finalizers to its Leases.
Those fields survive a later maintenance run's takeover, so they could
otherwise cause token deletion to remove that run's fence, or prevent its
release.

Every upload of the shared certificates creates the policy and its binding, or
restores them if they were changed, then verifies enforcement for each bootstrap
group on every serving API server before it grants the Leases. That is
`stratum token create --control-plane`, the MCP `token_create` tool,
`stratum certs rotate --rekey-shared`, and the re-upload that
`stratum certs rotate-ca --cluster` makes, and that the migrations make on a
cluster with more than one control plane. If the policy cannot be put in place,
the command fails before it grants the Leases. Do not delete the policy or its
binding: until the next upload puts them back, a control-plane join token can
create Leases and overwrite the maintenance fence.

A leaked control-plane join token can still take the fence for itself and keep
renewing it while the token is live. That holds off every upgrade, rotation,
migration and control-plane mint, and their refusals name the holder as
`system:bootstrap:<token-id>/…`. Delete that token:

```console
# /usr/local/bin/stratum kubectl -n kube-system delete secret bootstrap-token-<token-id>
```

Its renewals then fail. An ordinary Lease expires within 60 seconds of its last
renewal, but admission cannot check the clock: a leaked token can record a
renewal time in the future and postpone expiry. Inspect both fence holders:

```console
# /usr/local/bin/stratum kubectl -n kube-system get lease stratum-cluster-maintenance stratum-cluster-upgrade -o yaml
```

After revoking the token, delete any of these Leases whose holder starts with
`system:bootstrap:<token-id>/`, using its current resource version so a racing
maintenance run's replacement is preserved:

```console
# /usr/local/bin/stratum kubectl delete --raw /apis/coordination.k8s.io/v1/namespaces/kube-system/leases/<lease-name> -f - <<'EOF'
{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"resourceVersion":"<resourceVersion>"}}
EOF
```

Do not delete a Lease held by another maintenance run. If its renewal time is
in the future, check that run's clock and confirm it has stopped before
clearing its fence.

## kube-scheduler's files

kube-scheduler reads only its own kubeconfig,
`/var/lib/stratum/kubeconfig/scheduler/scheduler.kubeconfig`, which embeds its
certificate and key. Its pod mounts only that directory, so a compromised
scheduler cannot read the cluster CA's key or the administrator credentials.

An upgrade writes the new kubeconfig before it restarts kube-scheduler. The
copy at the old path, `/var/lib/stratum/kubeconfig/scheduler.kubeconfig`, is
kept current, because a rollback restores a manifest that names it. The new pod
cannot read it.

kube-controller-manager keeps its mounts: it signs certificates and service
account tokens with the cluster CA's and the service account keys, so it holds
the cluster's root credentials by design.

## Upgraded clusters and system:masters

Earlier releases issued `admin`, `kube-vip` and `apiserver-kubelet-client` in
`system:masters`, which bypasses role-based access control (RBAC). A leaf is
otherwise reissued only when it is missing or lacks a subject alternative name
(SAN), so an upgrade alone leaves those certificates in place.

The first start of the new release on each control plane reissues them. An
upgrade makes that start itself. Once its apiserver answers, the controller:

1. finds the leaves whose subject this release no longer issues;
2. makes sure the bindings their new subjects need exist:
   `stratum:cluster-admins`, `stratum:kube-apiserver-kubelet-admin`, and, on a
   VIP node, `kube-system/stratum:kube-vip`;
3. reissues every leaf, rewrites the kubeconfigs and restarts the control-plane
   components, as `stratum certs rotate` does.

The bindings come first, so `admin.conf`, kube-vip and `kubectl logs` keep
working while the certificates change. The rotation takes the cluster
maintenance lease, so control planes move one at a time and never during a
cluster upgrade or another maintenance operation.

- Without a VIP, a controller renews the lease through another control plane's
  apiserver while its own restarts, so the lease does not run out part-way
  through. If no other apiserver renews it, the controller logs a warning and
  renews through its own.
- A controller that cannot take the lease retries every 30 seconds, and logs
  once that it is waiting.
- A reissue that fails for any other reason is logged at once and retried after
  a minute, then after twice as long each time, up to 30 minutes. Every attempt
  restarts the control plane.
- kube-vip must restart, and its old process exit within two minutes, or the
  reissue counts as failed.
- The controller log reports which leaves it found and when it has reissued
  them. A controller stopped part-way through finishes the reissue on its next
  start.

**Copies made before the upgrade.** An `admin.conf` copied off a control plane
before the upgrade still carries `system:masters`. RBAC cannot revoke it, and it
stays valid until it expires, a year after it was issued. To cut such copies
off, rotate the cluster CA with `stratum certs rotate-ca --cluster --yes`
through `finalize`.

## HelmChart installs run as the requester

The operator installs a `HelmChart`'s rendered objects with the **requester's**
authority, not its own cluster-admin identity:

- A mutating admission webhook stamps the writer onto the `HelmChart` as
  `stratum.ciq.com/requester`, at creation and on every spec change.
- The webhook signs the resource identity, the desired spec and the requester
  with a hash-based message authentication code (HMAC). The key is a cluster
  secret only the operator can read, so an unsigned or altered stamp, such as
  a forged `requester: system:masters`, fails verification.
- The operator applies the release **impersonating** the requester. The
  apiserver's RBAC checks every rendered object, namespaced and cluster-scoped,
  as that user. A tenant cannot use the operator to create anything they could
  not create themselves, such as a `ClusterRoleBinding` or a `Role` that widens
  their own access.
- A `HelmChartConfig` overlay with the chart's name must carry the same
  username, UID and groups as the chart's requester.

A namespaced `HelmChart` installs only into its own namespace, unless it lives
in one of the operator's `--privileged-namespaces`, by default `kube-system`.
Charts in a privileged namespace, the bundled add-ons among them, install as the
operator.

The apiserver reaches the webhook over mutual TLS with its front-proxy client
certificate, and the webhook checks it against the front-proxy CA. A tenant pod
cannot get a signature by sending a fabricated AdmissionReview.

The webhook runs with `failurePolicy: Fail`:

- While it is unavailable, tenant CREATE and UPDATE requests fail. Existing
  workloads keep running, and the `kube-system` add-ons are exempt.
- Give each `HelmChart` an explicit `metadata.name`, not `generateName`. The
  name is part of the signature.
- Metadata-only writes by controllers keep the existing requester.
- The operator never uninstalls an unverified tenant release as itself. It
  keeps the finalizer, so an administrator can recover the release.

!!! note "Existing tenant HelmCharts after an upgrade"
    A tenant `HelmChart` that already had a deployed release keeps running after
    the upgrade. Its next spec change through `kubectl apply` or `kubectl edit`
    stamps it. An unstamped `HelmChart` outside a privileged namespace is refused
    only when the operator attempts an install or upgrade. If one reports
    `Failed` with `no requester identity recorded`, change its spec. Reapplying
    identical content does not stamp it.

    The requester needs the RBAC to create the chart's objects, the Helm release
    `Secret` included, in the target namespace. Without it, the install fails.

### Chart repository credentials

A `HelmChart` fetched from an HTTP(S) repository through `spec.repo`
authenticates with the entry of the operator's repository configuration `/var/lib/stratum/helm/repositories.yaml`
whose URL names the same repository: the URLs match when their scheme and
host are equal ignoring case, their paths are equal as written ignoring only
a trailing slash — the comparison never cleans empty or dot segments, so a
path that resolves elsewhere on the server does not match (path case is
significant) — and their query strings are equal exactly
(`internal/operator/chartfetch.go`, `repoURLEntry` and `normalizeRepoURL`).
A `repo/chart` reference instead uses the entry named by its repository, and
an absolute `spec.chart` URL is never matched against entry URLs: it takes
the credentials of the registered repository whose cached index lists that
archive, when one does (`chartRepositoryEntry`) — the TLS and credential
rules below are the chosen entry's either way. The entry's `caFile`, `certFile` and
`keyFile` secure the connection the way they do for a named repository (`chartFetchTLSConfig` feeds the request transport),
and a repository whose index points its chart archive at another host, or
redirects there, receives the entry's username and password and its client
certificate only when the entry sets `pass_credentials_all: true`: the
credentials are bound to the URL the entry names, and Helm's HTTP getter
forwards them to another host only with that flag (`pkg/getter` HTTP getter's
origin check), while a request to any other scheme, host or port goes out on a
connection without the client certificate (`originTransport`). A `spec.repo` that matches no
entry fetches anonymously, unless the chart archive's owning repository is
itself registered with a cached index, whose credentials the owner scan then
applies (`chartRepositoryEntry`) — so a public repository such as the
`spec.repo: https://prometheus-community.github.io/helm-charts` above keeps
working with or without a repositories.yaml.

### Values from Secrets

Anyone who can read a `HelmChart` can read its spec, and encryption at rest
does not cover it. Keep a value such as a password in a Secret in the
`HelmChart`'s namespace and name it in `spec.valuesSecrets`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: grafana-params
  namespace: kube-system
stringData:
  values.yaml: |
    grafana:
      adminPassword: <password>
---
apiVersion: stratum.ciq.com/v1
kind: HelmChart
metadata:
  name: kube-prometheus-stack
  namespace: kube-system
spec:
  repo: https://prometheus-community.github.io/helm-charts
  chart: kube-prometheus-stack
  valuesSecrets:
    - name: grafana-params
      keys: [values.yaml]
```

Each listed key holds a `values.yaml` body. They are merged in list order,
over `spec.valuesContent` and the `HelmChartConfig` and under `spec.set` and
`spec.setString`.

The field is honoured only for a `HelmChart` in a privileged namespace
(`kube-system` by default), which installs as the operator. Anyone who can
write a Secret there is already as trusted, which is also why a
`HelmChartConfig` there needs no matching requester. Elsewhere the chart is
refused: a Secret's writer, who need not be able to write the `HelmChart`,
would steer the release with its requester's authority.

- A missing Secret or key fails the chart with a message that names it; the
  first reconcile after it exists installs.
- A change to the Secret upgrades the release.
- The values appear in neither the `HelmChart` nor its status.
  `status.specHash` covers the Secret's `resourceVersion`, not its content,
  and the public inputs as written, so any edit to `spec.valuesContent`, the
  `HelmChartConfig`'s `valuesContent`, `spec.set` or `spec.setString`
  upgrades the release, even one that leaves the merged values the same.
- A failed install, upgrade or recovery reports only that it failed: Helm's
  error can quote values, so it is only in the operator's log. Once the chart
  has installed with Secret values the operator sets
  `status.valuesFromSecrets: true` and keeps doing so after
  `spec.valuesSecrets` is removed, because Helm's release history, which a
  recovery can roll back to, still holds them.
- An uninstall that leaves objects Helm cannot verify holds such a chart as
  for any other, but the hold names the objects only in the operator's log:
  a template can name an object after a value.
- Helm keeps the release's values in its release Secret, as for any value.

### Requester signing keys and signature rollout

Every operator refreshes the shared `kube-system/stratum-operator-requester-hmac`
Secret every 30 seconds. Admission and reconciliation use immutable snapshots.
A transient read error preserves the working snapshot; a failed initial load
keeps retrying. Replacing the signing key retains all previous verification
keys in the Secret's `retained-keys` map before the replacement becomes active,
so stored stamps remain verifiable after restarts and repeated rotations.
Keep that map when managing the Secret. A key identifier always names the same
key material; give a replacement key a fresh identifier. Operators reject a
reused identifier instead of discarding its previous verifier.
Deleting the Secret does not revoke a
key: a running operator restores it from its held snapshot. Revocation needs
all operators stopped and an administrator-led recovery of affected objects.

New-format signing is disabled by default. Follow this order when migrating:

1. Upgrade every control-plane operator, including possible rollback targets,
   to a release that verifies the versioned field policy. Verify the running
   binaries on every instance; elapsed release time does not establish adoption.
2. After adoption, enable the versioned signer explicitly:

   ```console
   # /usr/local/bin/stratum kubectl -n kube-system patch secret stratum-operator-requester-hmac \
     --type=merge -p '{"stringData":{"binding-version":"v2-hmac-sha256"}}'
   ```

   Allow 30 seconds for every operator to refresh. New admissions then carry
   the legacy signature, versioned signature, key identifier, and an
   authenticated list of the spec fields present at admission.
3. Before extending the CRD schema, migrate existing tenant `HelmChart` and
   `HelmChartConfig` objects through authenticated **spec-changing** updates
   by their intended requester. Metadata-only updates and identical applies
   preserve the existing stamp. Check that each object has the
   `stratum.ciq.com/requester-sig-v2` and `stratum.ciq.com/requester-fields-v2`
   annotations before introducing new defaults.

The versioned binding covers every admitted field's exact value, including
values equal to the admission-time default. Later schema fields absent from
that authenticated list are tolerated only when they equal their served schema
default. Changing an already admitted field to a new default does not authorize
the change. The policy supports later top-level defaults; a future change that
defaults inside an already admitted object or list needs a new binding version
and the same verifier-first rollout.

Before **changing or removing an existing schema default**, migrate every
affected tenant object whose authenticated field list does not include that
field, including objects that already carry a v2 signature. While the old
default is still served, have the intended requester submit an authenticated
**spec-changing** update that preserves the field's intended value. Check that
the new `stratum.ciq.com/requester-fields-v2` annotation includes the field before
changing the schema. Legacy objects must acquire the v2 signature and field
list through the same update.

Metadata and status writes can persist a later default without adding it to
the authenticated field list. For example, an object created before a field
exists can store its later default of `3` through a status write. If that
default then changes to `4`, the stored `3` is outside both the original field
list and the current default policy, and verification refuses the object.
Metadata-only updates and identical applies do not perform the required
migration. Authenticated spec-changing migration before the schema change
binds the stored `3`, so it remains valid and a later unauthorized change to
`4` is rejected. The verifier does not infer historical defaults from stored
values; skipping this migration leaves affected objects failing closed until
their intended requester repairs the stamp through a spec-changing update.

Legacy verification remains available while old stamps exist. Its compatibility
policy preserves the released schema's fields and tolerates later default-only
fields; it cannot reconstruct arbitrary sequences of schema changes for newly
created legacy-only objects. Activate and migrate before extending the schema.
Keep every key needed by stored stamps; removing a verification key requires
authenticated restamping or an explicit administrator recovery first.

### What Helm v4 changes for the requester

The operator runs Helm v4. For a tenant `HelmChart`, that changes only the
rights the requester needs for uninstalls:

- **Installs** keep Helm v3's client-side apply, so they need the same
  `create` rights as before. **Upgrades and rollbacks** follow the apply
  method the release recorded, so they stay client-side too. The operator
  also keeps Helm v3's waits: it watches only `Job` and `Pod` hooks until they
  finish, waits for a deleted hook to be gone before creating it again, and
  waits for the CRDs a chart installs to be established.
- **Uninstalls** need `get` on every kind of object the release created,
  because Helm v4 reads each object to confirm that the release owns it
  before deleting it. When the chart has `pre-delete` or `post-delete` hooks,
  the uninstall also needs both `create` and `patch` on each hook object's
  kind, because Helm v4 always applies those hook objects with server-side
  apply.

The operator uninstalls a release when its `HelmChart` is deleted, and also to
clear a release that a crash left pending when the release has no earlier
revision that served to roll back to (one that is `deployed`, or `superseded`
after its install or upgrade completed), or one that a failed recovery left
uninstalling. A pending release that has such a revision
is rolled back to it instead. A recovery that fails sets the `HelmChart` to
phase `Failed` with a `status.message` that starts with
`recover <status> release:`; unlike the hold described below, it needs no
action, and the operator retries it about a minute later.

### Recovering from an incomplete uninstall

When Helm cannot read an object to confirm that the release owns it, it leaves
the object in place but still deletes the release record. The read fails, for
example, with a 403 when the requester lacks `get` on that kind, or with a
transient apiserver error. The operator does not treat this as a clean
uninstall: it sets the `HelmChart` to phase `Failed` with a `status.message`
that starts with `uninstall incomplete:` and lists only the objects whose
ownership Helm could not verify. The message takes one of two forms:

```text
uninstall incomplete: remove these objects by hand, then the stratum.ciq.com/helmchart finalizer: <objects>
uninstall incomplete: remove these objects by hand, then change the chart source (spec.repo, spec.chart, spec.version or spec.chartContent) or the merged values (spec.valuesContent, spec.set, spec.setString, or the same-name HelmChartConfig's valuesContent) to reinstall; other spec fields, and values edits that leave the merged values the same, do not reinstall, except on a HelmChart with spec.valuesSecrets, where any edit to those values fields or to a Secret it names reinstalls: <objects>
```

The first appears when the `HelmChart` is being deleted, and the second when
the operator was uninstalling a pending or uninstalling release to recover
it, as described above. Beside the message, the operator records the hold in
a `status.uninstallIncomplete` field: the objects the message lists, which of
the two paths it asks for, and the fingerprint of the chart source and merged
values (the `specHash`) current when the hold began. For a hold written by an
older operator version, the operator promotes its message to this field
before continuing a reconcile. While the recorded `specHash` still matches,
the operator keeps the `stratum.ciq.com/helmchart` finalizer and does no
further Helm work for the `HelmChart`. This is the *hold*. A change to the
chart or merged values lets the operator record a new install or upgrade
attempt and clear the field, even if that attempt fails. A deleted `HelmChart`
stays held until a person removes its finalizer.

!!! warning "Do not change the HelmChart or its HelmChartConfig until the objects are removed"
    A change to the chart or to the merged values ends the hold before you
    finish, and the operator does not restore it: the changed `specHash`
    differs from the one the hold recorded, so the operator may attempt to
    reinstall the chart over leftover objects even if you put the spec back
    as it was.
    Finish the cleanup promptly; see
    [When the hold ends early](#when-the-hold-ends-early).

To recover:

If the message says `any objects of release <namespace>/<name> that remain`,
the operator cannot list what the interrupted uninstall left. Check earlier
operator logs for a report if one was recorded. Otherwise, find remaining
objects using the recorded release name and namespace and the chart's
resources, including cluster-scoped resources, then verify their Helm
ownership metadata before removing them.

1. For each object the message lists, or each remaining object you found for
   a generic hold, make the check Helm could not make:
   confirm that it carries the label `app.kubernetes.io/managed-by: Helm` and
   the annotations `meta.helm.sh/release-name` and
   `meta.helm.sh/release-namespace` set to this release's name and namespace.
2. Remove by hand each such object that passes the check, and nothing else.
   If the operator logged Helm's full report, it also names any object that a
   `helm.sh/resource-policy: keep` annotation kept and any object that another
   release owns; leave those objects alone.
3. If the message asks for the finalizer, remove the
   `stratum.ciq.com/helmchart` finalizer from the `HelmChart` (for example
   with `stratum kubectl edit`).
4. If the message asks for a spec change, change the chart or the merged
   values as described below. For a tenant `HelmChart`, the tenant must make
   that change.

While the hold stands, the operator reinstalls only after a change that moves
the `specHash` away from the one `status.uninstallIncomplete` records — a
change to one of these:

- The chart: `spec.repo`, `spec.chart`, `spec.version` or `spec.chartContent`.
- The merged values, which the operator builds by parsing and layering
  `spec.valuesContent`, the `valuesContent` of the `HelmChartConfig` with the
  same name, the Secrets `spec.valuesSecrets` names, `spec.set` and
  `spec.setString`. A change to one of those Secrets counts, whatever it
  changes in the values.

These changes do not trigger a reinstall:

- An edit that leaves the merged values the same, such as adding a comment,
  reformatting or reordering keys in a `valuesContent`, or changing a value
  that a later layer overrides (for example, a `spec.valuesContent` key that
  `spec.set` also sets). This does not hold for a `HelmChart` with
  `spec.valuesSecrets`, where any edit to those fields counts.
- A change to any other field, such as `spec.timeout`; these fields are not
  part of the `specHash`. A `spec.targetNamespace` outside a privileged
  namespace — a target other than the `HelmChart`'s own namespace — is still
  refused, but that refusal replaces the message without ending the hold.

!!! warning "An administrator's spec change reinstalls a tenant chart with administrator rights"
    The admission webhook records whoever changes the spec as the requester,
    and the operator reinstalls as that user. Only the tenant should change a
    tenant `HelmChart`'s spec.

Deleting the `HelmChartConfig`, or setting its `valuesContent` to an empty
string, is not refused, because the operator checks the overlay's requester
only while its `valuesContent` is not empty. A `valuesContent` that holds only
a comment, whitespace or `{}` is still checked. If deleting or emptying the
overlay changes the merged values, the operator reinstalls the chart as the
tenant, without the overlay's values.

### When the hold ends early

The hold ends when the operator records a changed chart for an install or
upgrade attempt, or when a person removes the finalizer of a deleted
`HelmChart`. It does not end when another status
replaces the `uninstall incomplete:` message: the hold lives in
`status.uninstallIncomplete`, and while the spec the hold recorded is still
the current one, the operator regenerates the message from it. For a
`HelmChart` that is not being deleted, these replace the message while the
hold still holds the reinstall:

- A values parse error.
- A refused `spec.targetNamespace`: outside a privileged namespace, one other
  than the `HelmChart`'s own namespace.

Two other refusals replace the message only once a change has already moved
the `specHash` away from the one the hold records: the operator reaches them
only past the gate that, while the hold's `specHash` still matches, stops the
reconcile first.

- A refused requester stamp.
- An administrator's edit to the `HelmChartConfig` that changes the merged
  values but leaves any text in its `valuesContent`, even only a comment or
  `{}`. The operator refuses it, because the overlay's requester no longer
  matches the chart's.

The hold's field survives both refusals. Undo the change, and the hold holds
the reinstall again and regenerates its message; keep the change, and once
the refusal clears the operator may attempt to reinstall over the leftover
objects.

A change to the chart or to the merged values can end the hold: it moves the
`specHash` away from the one the hold records, so the operator clears the
field when it records the install or upgrade attempt. The attempt can fail;
the field is still gone, and a later retry may install over leftover objects
even if the spec is put back as it was.

The operator retries a failed write of the message and the field recorded
beside it, and holds the `HelmChart` in its own memory until the write
succeeds. That memory does not survive the operator restarting, or another
operator instance taking over as leader, before the write succeeds. The hold
itself survives, over any number of restarts: before each uninstall the
operator writes a marker to `status.message` — `uninstalling release` for a
deleted `HelmChart`'s uninstall, `uninstalling release to recover` for the
uninstall that recovers a pending or uninstalling release. The recovery's
marker also records the unresolved recovery in `status.unrecordedRecovery`;
the status writes a reconcile makes before it decides on the release leave
that record standing. A reconcile after the restart, finding that evidence
with the release gone, cannot tell what the earlier uninstall left, so it
records the hold with `any objects of release <namespace>/<name> that remain`
in place of the list Helm reported, and the hold ends the way any hold does:
a person removes the finalizer of a `HelmChart` being deleted, or changes the
spec of one that is not — the change permits an install attempt, so whatever
of the release remains is best removed first. It stands even when the
interrupted uninstall left nothing, since the operator cannot tell. Earlier
operator logs may contain Helm's report if the uninstall returned before the
restart; a crash immediately after the purge can leave no report.

## Disable the operator and remove its admission webhook

Setting `spec.operator: false` stops that node from running the operator on its
next `stratum up`.

The operator approves kubelet serving certificate requests. A token-joined
worker requests one in every profile. Without a running operator, a new worker
cannot get a serving certificate automatically. Until an administrator approves
its certificate signing request (CSR), `kubectl logs`, `exec` and `top` fail for
pods on it. Disabling the
operator does not remove certificates already issued to existing workers; they
continue serving them. If all operators stop, their renewal requests remain
unapproved unless an administrator approves them. `certs rotate-ca --phase=sign-new`
refuses when it needs a new serving certificate. Access also fails if an
existing serving certificate expires without renewal. Under a profile that
requires the operator, Stratum refuses `spec.operator: false` at startup. In an
HA cluster, disable and stop the
operator on **every control-plane node** before removing its cluster-wide
webhook. A running instance recreates the webhook configuration. A normal
shutdown leaves the configuration in place, so an outage does not bypass
admission.

After all operator instances have stopped, use an administrator kubeconfig:

```console
# stratum kubectl delete mutatingwebhookconfiguration stratum-operator-requester --ignore-not-found

# stratum kubectl -n kube-system delete service stratum-operator-webhook --ignore-not-found

# stratum kubectl -n kube-system delete endpointslices -l kubernetes.io/service-name=stratum-operator-webhook
```

Tenant HelmChart writes then no longer depend on the webhook, but no HelmChart
reconciliation occurs while the operator is disabled. Re-enabling it restores
admission enforcement; submit a spec-changing update to establish the requester
for CRs created while it was disabled.

Serving instances refresh their EndpointSlice heartbeat every minute. Surviving
instances prune peers after three missed renewals (up to approximately four
minutes including the next sweep). Admission may fail while a crashed endpoint
is awaiting removal. If all instances are down, admission remains fail closed
until an operator recovers or an administrator intentionally performs the cleanup
above. Keep control-plane clocks synchronized for heartbeat expiry.
