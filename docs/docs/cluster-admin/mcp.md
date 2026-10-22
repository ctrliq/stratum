# MCP Server

`stratum mcp` serves Stratum's own tools, and a generic Kubernetes toolset, over
the [Model Context Protocol](https://modelcontextprotocol.io). An MCP-speaking
client — an editor, an agent runner, a chat client — can then read cluster state
and, if you let it, change it.

The server is CIQ's own code. It takes no dependency on any third-party MCP
server.

```bash
sudo /usr/local/bin/stratum mcp
```

That starts a read-only server on stdio. Most clients spawn it themselves rather
than having you run it by hand:

```json
{
  "mcpServers": {
    "stratum": {
      "command": "sudo",
      "args": ["stratum", "mcp"]
    }
  }
}
```

## The three tiers

What the server exposes is decided when you start it, not per call.

| Started with | Tools available | What they can do |
|---|---|---|
| no flags | 18 | read cluster and node state |
| `--allow-write` | +8 | apply manifests, scale, snapshot, mint join tokens |
| `--allow-destructive` | +11 | upgrade, roll back, rotate CAs, restore etcd, remove restore copies, uninstall |

The flags are independent — `--allow-destructive` on its own does not enable the
write tier.

**A tool above the tier you enabled is not registered at all.** It does not
appear in the client's tool list, so the agent never learns it exists. That is
deliberate: a tool that appeared and then refused would tell the model the
capability is there and invite it to look for another way to the same effect.

!!! warning "The tiers are not a security boundary"
    Kubernetes RBAC is the only hard boundary. The tiers decide what this server
    *offers*; RBAC decides what the credential is actually *allowed to do*.

    A server started with no flags but running as `cluster-admin` is one
    misconfiguration away from full cluster access, and nothing at the protocol
    layer can take that back. Give it a dedicated ServiceAccount with the narrowest
    role that works — see [Least privilege](#least-privilege).

    Some tools do not use that credential at all: they run on this node's own
    `super-admin.conf`, and for them the tier flag and running the server as root are
    the only controls. The table under [The destructive tier](#the-destructive-tier)
    lists the destructive ones, and [Choosing a cluster](#choosing-a-cluster)
    names the read-tier exceptions (`token_list`, `upgrade_plan`).

## What it exposes

**Cluster state, through the Kubernetes API.** `pod_list`, `pod_get`,
`pod_logs`, `node_list`, `namespace_list`, `event_list`, and `resource_list` /
`resource_get` for anything else the cluster serves, including custom resources.

**Stratum's own state, from the node.** `cluster_status` reports the supervised
process table — what the supervisor *intends* to be running versus what is,
which is not visible from the Kubernetes API at all. `certs_check`,
`snapshot_list`, `snapshot_restore_copies`, `token_list` and `registry_check`
report the rest. `upgrade_plan` and `uninstall_preview` show what a change would do without
doing it.

Every list is bounded and says so. A truncated result carries `truncated: true`
and a token to page with, because an agent handed the first hundred of four
thousand pods with no indication will reason about them as though they were all
of them.

There is no `kubectl` passthrough, and no tool takes a free-form command. Every
input is a typed field, and the resource to act on is resolved through discovery
rather than pasted into an argument list.

Every result is fenced as untrusted data. A pod log line, an event or an
annotation can say anything its author likes, including text written to look
like instructions. So the text of each result, and of each error, arrives
between `<untrusted-data id="...">` and `</untrusted-data id="...">` markers that
share a random id chosen for that call, and anything inside that resembles a
marker is defused first. That includes errors the server raises before a tool
runs, such as an argument of the wrong type. The server's instructions tell the
client the fenced text is data, never instructions.

The structured form of a result (`structuredContent`) is not fenced, so clients
that parse it against the tool's output schema keep working. Anything in its
strings that resembles a marker is defused in the same way, and the server's
instructions tell the client those strings are untrusted too. Because the
fenced text is no longer bare JSON, a client that needs the result as JSON
should read `structuredContent`.

## Choosing a cluster

With no configuration the server looks, in order, at `--kubeconfig`,
`$KUBECONFIG`, `~/.kube/config`, this node's own `admin.conf`, and finally
in-cluster credentials.

Files come before in-cluster on purpose: if you have a kubeconfig, that is the
cluster you mean, even when the server happens to be running inside a different
one.

The generic Kubernetes tools take an optional `context`, so one
server can address several clusters. `context_list` reports the valid values,
and is the first call worth making when more than one cluster is in play.
`upgrade_plan` is not one of them: it takes no context.

`token_list` is a local-admin exception. Bootstrap tokens are Secrets in `kube-system`,
which the least-privilege role below deliberately cannot read, so that one tool
uses this node's own `super-admin.conf` and always reports on the cluster the node
belongs to. It returns token IDs and expiry only — never the secret half.

`upgrade_plan` is a local-admin read too. It reads this node's internal credential
(`super-admin.conf` when present, otherwise `admin.conf`),
so it always previews the cluster this node belongs to — the same cluster
`upgrade_apply` with `scope: cluster` rolls — and `--kubeconfig` and
`--context` do not move it.

```bash
sudo /usr/local/bin/stratum mcp --kubeconfig /path/to/config --context staging
```

`ha_repoint` with `cilium` makes its cluster-wide API calls through the
credential and default context the server was started with, so it needs the
rights listed under [The destructive tier](#the-destructive-tier). It refuses
any cluster but this node's own: its `kube-system` namespace must be the one
this node's `super-admin.conf` sees, compared by UID.

`upgrade_apply` with `scope: cluster` runs its rollout — the drains, the
per-node upgrade Job and the upgrade lease — through this node's own
`super-admin.conf`, so `--kubeconfig` and `--context` cannot point it at another
cluster, and the server's credential needs no upgrade rights. The per-node
upgrade work itself runs as root on each node and is not bounded by RBAC.

## Running it over HTTP

The HTTP transport needs a bearer token. Loopback is not a boundary: every
local user, and every `hostNetwork` pod on the node, can reach `127.0.0.1`, and
the tools run as root. Create the token in a directory and a file only root
can read or write:

```bash
sudo install -d -m 0700 /etc/stratum/mcp
sudo install -m 0600 /dev/null /etc/stratum/mcp/token
openssl rand -hex 32 | sudo tee /etc/stratum/mcp/token >/dev/null
```

Then start the server with it:

```bash
sudo /usr/local/bin/stratum mcp --transport http --addr 127.0.0.1:8092 \
    --token-file /etc/stratum/mcp/token
```

The server refuses to start if:

- `--token-file` is missing;
- the path is not a regular file (a directory or a FIFO, say);
- the file is not owned by the user running the server;
- other users can read the file (any mode beyond `0600`);
- the token is shorter than 32 characters.

Leading and trailing whitespace in the file is ignored.

The server reads the token once, at startup. To rotate it, write a new token
to the file and restart the server.

The token is root-equivalent: whoever holds it can call every tool in the tier
you enabled, as root. Give it only to a client you would already trust with
root on this node.

Every request must carry `Authorization: Bearer <token>`. One without it, or
with the wrong token, gets `401 Unauthorized` before it reaches any tool. Put
the header in the client's config:

```json
{
  "mcpServers": {
    "stratum": {
      "type": "http",
      "url": "http://127.0.0.1:8092",
      "headers": {
        "Authorization": "Bearer <token>"
      }
    }
  }
}
```

The client's config now holds the token, so keep that file private too. On a
shared host, don't pass the token on a command line, as with `curl -H`:
other local users can read every process's arguments from
`/proc/<pid>/cmdline`.

!!! warning "The HTTP transport has no TLS"
    It binds loopback by default for that reason, and refuses a routable
    `--addr` unless you pass `--insecure-listen`. Over the network the bearer
    token would cross in the clear, and anyone who saw it could call every
    tool you enabled.

    If it has to be reachable off-box, put something that terminates TLS in
    front of it, and pass `--insecure-listen`.

Logs go to stderr. On the stdio transport stdout carries the protocol, so
nothing else may be written there.

## Least privilege

Do not point the server at `admin.conf` for day-to-day use. It is
`cluster-admin`, so every tier gets the same unlimited credential and the tier
flags become the only thing between an agent and the cluster.

Give it a ServiceAccount instead:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: stratum-mcp
  namespace: kube-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: stratum-mcp-read
rules:
  - apiGroups: [""]
    resources: [pods, namespaces, nodes, events, services, configmaps,
                persistentvolumeclaims, persistentvolumes]
    verbs: [get, list, watch]
  - apiGroups: [""]
    resources: [pods/log]
    verbs: [get]
  - apiGroups: [apps]
    resources: [deployments, statefulsets, daemonsets, replicasets]
    verbs: [get, list, watch]
  - apiGroups: [batch]
    resources: [jobs, cronjobs]
    verbs: [get, list, watch]
```

**Secrets are deliberately absent, and the server refuses them anyway.**
`resource_get` and `resource_list` will not return a Secret at any tier, even
with `cluster-admin` — a Secret's contents in an agent's context window is the
worst read this server could perform, and it cannot be taken back. Leaving
Secrets out of the role is still worth doing: it is the layer that holds if the
refusal is ever changed, and it covers the token the kubelet would mount into a
pod the server applies. If a workflow genuinely needs a Secret, read it with
`kubectl`.

Check what you granted by asking the API server rather than re-reading the YAML:

```bash
kubectl auth can-i get secrets --as=system:serviceaccount:kube-system:stratum-mcp
kubectl auth can-i create clusterrolebindings --as=system:serviceaccount:kube-system:stratum-mcp
```

Both should answer `no`. A `yes` to either means the tier flags are the only
thing protecting the cluster.

## What it will never do

Some things are refused regardless of tier, because they would let an agent
grant itself more than you gave it:

- **Reading Secrets** — `resource_get` and `resource_list` refuse them at every
  tier, including read. This is the one refusal that applies to a read.
- **Writing RBAC** — roles and bindings. Writing a ClusterRoleBinding is
  self-promotion to `cluster-admin`, which would make the tiers meaningless.
- **Writing admission control** — webhook configurations, and the CEL-native
  `ValidatingAdmissionPolicy` and `MutatingAdmissionPolicy` objects with their
  bindings. All of them can disable or intercept the rules meant to contain the
  server; a MutatingAdmissionPolicy needs no endpoint or certificate to do it.
- **Writing Secrets, ServiceAccounts, CSRs or APIServices** — identity and
  credentials by another route.
- **Writing a Node object** — clearing a control-plane taint places workloads
  where they can read the cluster's credentials. Deleting a node is a real
  administrative action and stays at the destructive tier instead.
- **Installing a CRD with a conversion webhook** — the API server would then
  call that endpoint on every read and write of the kind, and the reply
  replaces the object. The CRD itself is fine; the webhook is not.
- **Creating or modifying PVs/PVCs** — a hostPath PV mounted through a claim
  would bypass the direct pod hostPath check. Deletion still requires the
  destructive tier.
- **Creating or modifying StorageClasses** — a storage class tells a
  provisioner where on the node to make volumes, and `local-path` takes a
  `pathPattern` that, with `allowUnsafePathPattern`, puts them anywhere.
  Deleting one is allowed at the write tier.
- **Writing anything in `local-path-storage`** — for every volume, the
  `local-path` provisioner starts a helper pod on the node with a node
  directory mounted, and that pod never passes through this server.
  `local-path-config` holds the helper's pod spec, its script and the
  directories it may use, and the `local-path-provisioner` Deployment chooses
  which ConfigMap and image it runs, so writing either runs a pod of the
  client's choosing on the node. Apply, delete and scale are all refused in
  that namespace; change it with `kubectl`.
- **Writing a HelmChart or a HelmChartConfig** — the pod specs a chart renders
  never pass through this server, so none of the checks below could apply to
  them. A HelmChart in a privileged namespace (`kube-system` by default) is
  installed with the operator's own cluster-admin identity, and a
  HelmChartConfig is merged into the HelmChart of the same name, so writing
  either one there changes what the operator installs as cluster-admin.
- **Writing the operator's ConfigMaps in `kube-system`** —
  `stratum-addon-inventory` lists the objects the operator deletes as
  cluster-admin, and a `stratum-node-client-claim-*` ConfigMap holds a node
  name against reuse by a later join. Writing either would get around the
  tiers, so both are refused for apply and delete.
- **Applying a pod spec that escapes its namespace** — `hostPath` volumes,
  `hostNetwork`, `hostPID`, `hostIPC`, `privileged`, added Linux capabilities
  and an unmasked `procMount`. Each reaches the node itself.
- **Applying a pod spec that carries a Secret** — a secret volume, a projected
  secret or `serviceAccountToken` source, a `secretKeyRef`, an `envFrom`
  `secretRef`, an `imagePullSecrets` reference, or a volume that names a Secret
  for the kubelet to deliver (`iscsi`, `rbd`, `cephfs`, `cinder`, `scaleIO`,
  `storageos` and `flexVolume` `secretRef`, `csi` `nodePublishSecretRef`,
  `azureFile` `secretName`). A container that mounts a Secret can print it, and
  `pod_logs` is read-tier, so this is the read above by a longer route.
  `imagePullSecrets` and the volume references need no container at all: the
  kubelet presents the credential to whatever registry the image names, or to
  whatever endpoint the volume names, such as an iSCSI portal's CHAP login.
- **Mounting a claim backed by a node-path volume** — writing a
  PersistentVolume or a claim is already refused, but an operator may have left
  a `hostPath` or `local` PV behind, and mounting a claim bound to one reaches
  the node's filesystem with no `hostPath` anywhere in the pod spec. So the
  claims a pod spec names are resolved, and refused when bound to such a PV
  or, while still pending, when one could take them by class or reservation.
  A claim the cluster will create from a template (an `ephemeral` volume or a
  StatefulSet's `volumeClaimTemplates`) is refused when an existing node-path
  PV could take it: by the template's `volumeName` (which binds only that PV),
  by a matching storage class (a template with no class matches any class), or
  by a PV pre-reserved for a claim name the template could produce. A
  StatefulSet re-adopts an existing `<template>-<set>-<ordinal>` claim by name,
  so those claims are checked the same way. An `ephemeral` volume's claim is
  `<pod>-<volume>`, where `<pod>` is a name the workload's pods can get (such
  as `<set>-<ordinal>`, or `<deployment>-` and generated characters), so claims
  of other workloads are not counted. A PV the default `local-path` class
  created is a directory made for its claim, not a path into the node's files,
  so it is not refused: workloads on `local-path` apply and update normally.
  Such a PV is recognised by its provisioner, `rancher.io/local-path`, and its
  path, `/opt/local-path-provisioner/<pv>_<namespace>_<claim>` for the claim it
  is reserved for. Another provisioner's PVs over node paths (the static
  local-volume provisioner's, say) are refused like hand-made ones. Set
  `storageClassName` to `local-path` to apply a template refused here.

These checks apply to any object in a manifest that carries a `containers`
list, at any depth: a Pod's spec, a workload's template, a PodTemplate's, or one
inside a custom resource. A pod spec a custom resource holds in another shape,
such as a single `container` or a list of `steps`, is not recognised, so keep
the boundary for those in RBAC and admission control. A manifest nested more
than 64 levels deep is refused.

Because server-side apply keeps fields owned by other managers, these checks
run against the object already in the cluster as well as against your manifest.
An apply that only changes a container's command is still refused if the
workload it is updating carries one of the fields above. The same applies to a
StatefulSet whose templates could bind a node-path PV: the controller creates
new claims from its `volumeClaimTemplates` on every scale-up, so the update is
refused while such a PV exists. `resource_scale` itself does not run these
checks — it runs the write-policy list only — so prefer `resource_apply` for
workloads with claim templates.

Among the generic Kubernetes tools, `resource_delete` and `resource_apply` are
registered at the write tier and then refuse some targets without
`--allow-destructive`. Deleting a namespace, PersistentVolume,
PersistentVolumeClaim, node or CRD needs it, as does creating or changing a
namespace or CRD, because an apply can be as hard to undo as a delete: it can
rewrite a CRD's schema or a namespace's finalizers.

And some tools simply do not exist: there is no `etcdctl` (raw datastore access,
including removing a member), no `up` (it stands up host state with no
confirmation gate), and no `operator` or `dash` (daemons, not request/response).

## The destructive tier

`--allow-destructive` reaches tools that drain nodes, wipe etcd, replace the
cluster CA and uninstall Stratum from a node. Each states its blast radius in
the first line of its description, because that is what the model reads before
deciding to call it.

There is no second confirmation per call. Starting the server with the flag *is*
the confirmation — it is the equivalent of having typed `--yes`, decided by a
human once rather than by the agent each time.

The practical advice: **do not leave a server running with
`--allow-destructive`.** Start one for the operation, watch it, and stop it.

Most of what this tier does is *host* work — certificates on disk, the
supervisor, the node's own state — so RBAC narrows less of it than you might
expect. One tool runs on the server's credential, and RBAC bounds it:

| Tool | Credential |
|---|---|
| `ha_repoint` with `cilium` | the server's |
| `upgrade_apply` with `scope: cluster` | this node's `super-admin.conf`: the rollout's drains, Job and lease ride it, and `--kubeconfig` and `--context` cannot point it elsewhere |
| `upgrade_apply` with `scope: node`, `rollback`, `certs_rotate`, `snapshot_restore`, `snapshot_rejoin`, `snapshot_prune_restores`, `uninstall`, `ha_repoint` without `cilium` | this node's own: host work on this node, and API writes through `super-admin.conf` (`snapshot_restore` restarts the cluster's Stratum-managed workloads; a node upgrade cordons and uncordons this node) |
| `certs_rotate_ca`, `certs_migrate_etcd_ca`, `certs_migrate_kubelet_client_ca` | this node's `super-admin.conf`, for every phase and for the cluster-wide run |
| `token_create`, `token_list` (write and read tiers) | this node's `super-admin.conf` |

For the tools on this node's credentials, RBAC on the server's credential
neither enables nor limits what they do: the tier flag, and running the server
as root on this node, are the only controls. `token_create` writes a bootstrap Secret, the
bindings that let it join a node, and the shared control-plane certificates,
none of which the server's own credential should ever be able to write. Do not
add Secret or RBAC permissions to the selected MCP kubeconfig for these: they
do not use it.

`upgrade_apply` with `scope: cluster` runs only an upgrade binary an operator
staged in `/var/lib/stratum/upgrade`. `from` is the binary's name there, or its
full path. The binary must be a regular file, not a symlink, owned by root and
writable by no one else, in a directory that is the same. Each node checks
the file at that path the same way before it runs it as root, and a node of
the same architecture also checks that it has the same sha256 as the binary on
the node serving MCP, so the model chooses among the binaries you staged and
cannot name another program. Stage the same binary at that path on every node
first, as for [`stratum upgrade --cluster`](upgrades.md).

`certs_rotate_ca` takes the same options as the CLI, and refuses the same
combinations. Set `cluster` to run every phase across the cluster, and leave
`phase` empty; set `phase` to run one phase on this node. Not both. It takes the
same workload choice as the CLI. Set
`restart_workloads` (with `cluster`) to have Stratum restart the Deployments,
DaemonSets and StatefulSets that call the API server before `sign-new`, or
`skip_workload_check` to turn the check off. Set one, not both. With neither, a cluster rotation on
a cluster running such workloads refuses to start, and a `sign-new` phase refuses
until they have restarted. See
[Workloads that call the API server](certificates.md#workloads-that-call-the-api-server).
Set `skip_token_check` to finalize without checking for mounted tokens signed by
the old service-account key, for compromise recovery. It needs `force`, and
without it the tool refuses. See
[Before `finalize`: mounted tokens](certificates.md#before-finalize-mounted-tokens).

The one tool on the server's credential needs these rights. Bind them for the
operation and remove the binding afterwards. The objects in `kube-system` go
in a Role and RoleBinding there:

```yaml
# Role in kube-system.
# ha_repoint with cilium points the Cilium agents and operator at the VIP.
- apiGroups: [apps]
  resources: [daemonsets]
  resourceNames: [cilium]
  verbs: [get, update]
- apiGroups: [apps]
  resources: [deployments]
  resourceNames: [cilium-operator]
  verbs: [get, update]
```

The rest is cluster-wide, in a ClusterRole:

```yaml
# ha_repoint: find this node, and compare kube-system's UID with this node's
# own cluster.
- apiGroups: [""]
  resources: [nodes]
  verbs: [get]
- apiGroups: [""]
  resources: [namespaces]
  resourceNames: [kube-system]
  verbs: [get]
```

`upgrade_apply` with `scope: cluster` needs none of these: it lists, cordons,
drains and stamps every node, runs the per-node upgrade Job and holds the
upgrade lease through this node's internal credential (`super-admin.conf` when
present, otherwise `admin.conf`), which already has them. The
right to update the Cilium DaemonSet is still the right to run a program as
root on every node — its agent runs privileged on every node — so treat these
bindings as cluster-admin for as long as they exist. They are also the
credential the read and write tools use, so while they are bound,
`resource_apply`, `pod_logs` and the rest have these rights too. Bind them to a
kubeconfig used only by a server started for the operation.

Snapshot and token listings accept `limit` and `continue_token`. Pass the returned
`continue` value to retrieve the next page. Snapshot cursors are tied to the
inventory: if snapshots are added, removed, or changed between calls, restart
without a cursor. Token pages use the Kubernetes API continuation token and the
same request timeout and response-size cap as the generic Kubernetes tools.
