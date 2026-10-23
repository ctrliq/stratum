# Cluster Administration

This section covers day-2 administration: the work you do on a running
cluster after installation. Every command is a subcommand of the `stratum`
binary that installed the cluster, so you need no other tool on the nodes. Most
commands run as root on a node, and the cluster-wide ones run from a control
plane.

| Page | What it covers |
|---|---|
| [High Availability](high-availability.md) | Three control planes behind a virtual IP (VIP), and how Stratum manages etcd membership |
| [Upgrades & Rollback](upgrades.md) | Rolling upgrades within the Kubernetes version skew rules, and rolling a node back |
| [Snapshot & Restore](snapshot-restore.md) | Scheduled etcd snapshots, restoring from one, and rejoining a member |
| [Certificates](certificates.md) | Certificate expiry and renewal, CA rotation, and recovering a rotation that halted |
| [Hardening](hardening.md) | The security controls every cluster runs. The Center for Internet Security (CIS) profile is in [Stratum PRO](../pro/index.md) |
| [MCP Server](mcp.md) | Serving Stratum's tools to an AI agent over the Model Context Protocol (MCP), and the tiers that bound it |

The rest of this page covers the day-2 tasks that have no page of their own:
running `kubectl` and `etcdctl`, deploying manifests from a node, the cluster
dashboard, and where the logs are.

## Talking to the cluster

`stratum kubectl` and `stratum etcdctl` are the versions built into the
`stratum` binary, so they match the cluster:

```console
# /usr/local/bin/stratum kubectl <args>

# /usr/local/bin/stratum etcdctl <args>
```

On a control plane, both configure themselves. `kubectl` uses
`/var/lib/stratum/kubeconfig/admin.conf`. `etcdctl` uses the etcd client
certificate and the `etcd-ca-bundle.crt` trust file. The bundle holds every CA
etcd can present, so it verifies etcd in every trust state, including part way
through a CA rotation.

A variable already set in your environment takes precedence over these
defaults. For example, to talk to a different etcd member while still using
this node's client certificate:

```console
# ETCDCTL_ENDPOINTS=https://<other-node>:2379 stratum etcdctl member list
```

On a worker there is no `admin.conf`, so `stratum kubectl` sets nothing. Pass
`--kubeconfig`, or copy a kubeconfig to the node.

!!! note "`stratum kubectl` ignores `~/.kube/config` on a control plane"
    On a control plane, `stratum kubectl` uses this cluster's `admin.conf`
    unless you set `KUBECONFIG`. To reach a different cluster from a
    `~/.kube/config`, set `KUBECONFIG` to that file.

RBAC grants `admin.conf` its rights, so you can revoke them. The
`system:masters` kubeconfig beside it, `super-admin.conf`, is for recovery. See
[admin.conf and super-admin.conf](hardening.md#adminconf-and-super-adminconf).

## Deploying manifests

The operator applies every `*.yaml` and `*.yml` file in
`/var/lib/stratum/manifests` with server-side apply. Drop a file there on a
control plane and it is applied on the next reconcile pass, within about 20
seconds. A file is applied again when its contents change, and every 5 minutes
otherwise.

How a file is treated depends on where it sits:

| Location | Applied | Removed from the cluster when the file goes |
|---|---|---|
| `/var/lib/stratum/manifests/<file>.yaml` | yes | no |
| `/var/lib/stratum/manifests/<stack>/<file>.yaml` | yes | yes |

A first-level subdirectory is a *stack*. Every object applied from it is
labeled `stratum.ciq.com/stack=<stack>`. When an object disappears from a
stack's files, the operator deletes it from the cluster. Loose files are only
applied, never pruned.

The directory name becomes a label value, so a stack name must be a valid
Kubernetes label value: at most 63 characters, made of letters, digits, `-`,
`_` and `.`, and starting and ending with a letter or digit. Stratum does not
check the name, so every object in a stack with an invalid name fails to
apply.

!!! warning "Give namespaced objects in a stack an explicit namespace"
    An object with no `metadata.namespace` is applied in `default`, but the
    operator records it with no namespace. When you later remove it from the
    stack, the delete misses it: the operator logs it as pruned, forgets it,
    and leaves it running in `default`. Set `metadata.namespace` on every
    namespaced object in a stack, `default` included.

!!! note "Pruning needs at least one file left"
    If no `*.yaml` or `*.yml` file remains anywhere in
    `/var/lib/stratum/manifests`, the operator skips the pass entirely and
    deletes nothing. Objects from a stack whose files were all removed stay
    in the cluster until a file is added again.

!!! tip "Write a stack file elsewhere, then move it into place"
    A stack file with no objects in it pauses pruning, because it may still
    be being written: `helm template app > myapp/app.yaml` empties the file
    before helm finishes. A file cut off part-way can still parse, though,
    and its missing objects would be pruned. Render to a temporary path and
    `mv` it into the stack's directory. To delete a stack, remove its
    directory.

To list a stack's workloads and Services, run the first command. `get all`
skips ConfigMaps, Secrets, RBAC objects and CRDs, so to list every kind the
stack labeled, run the second:

```console
# /usr/local/bin/stratum kubectl get all -A -l stratum.ciq.com/stack=<stack>

# /usr/local/bin/stratum kubectl api-resources --verbs=list -o name \
  | xargs -n1 /usr/local/bin/stratum kubectl get -A --ignore-not-found -o name \
      -l stratum.ciq.com/stack=<stack>
```

!!! warning "Some stack names are taken"
    Stratum writes its bundled add-ons to this directory as the stacks `crds`,
    `node-mark`, `cilium`, `coredns`, `local-path` and `dashboard`, and rewrites them on
    every start. Use other names for your own stacks. An edit to a bundled
    stack's file is overwritten.

Only the operator's leader applies the directory, and it reads its own node's
copy. In an HA cluster, put the same files on every control plane, or apply
them with `kubectl` instead.

Do not confuse this directory with `/var/lib/stratum/config/manifests`, which
holds the control-plane static pod manifests that the kubelet runs.

## The dashboard

Every control plane stages a `dashboard` stack, and the operator runs it in the
cluster: a single `stratum-dashboard` Deployment in the `stratum-dashboard`
namespace, served over HTTPS on NodePort `30443` of every node, and of the VIP
on a cluster that has one. Its image is in the payload, so it starts with no
registry access. To see its objects:

```console
# stratum kubectl -n stratum-dashboard get deploy,svc,secret
```

The dashboard pod itself has no cluster permissions. It forwards each request
with the token you signed in with, so you see and change exactly what that
token's RBAC allows.

### Connect to the dashboard

1. Find the address. The dashboard answers on port `30443` of every node,
   at `https://<node address>:30443`. A node's address is its `INTERNAL-IP`:

    ```console
    # stratum kubectl get nodes -o wide

    # stratum kubectl -n stratum-dashboard get svc stratum-dashboard
    ```

    With a VIP, `https://<vip>:30443` works too. If you set `publicURL`, use that
    instead.

2. Check the path to the port. Cilium answers NodePort traffic before the
   host's netfilter rules see it, so firewalld neither needs `30443/tcp` opened
   nor can block it: the dashboard is reachable on every node address that
   routes to you. To limit who can reach it, filter `30443/tcp` in a network
   firewall or security group in front of the nodes, or set `nodePort: 0` and
   use a port-forward (below). If you changed `nodePort`, that is the port.

    If another Service already holds the port, the operator serves the
    dashboard on a ClusterIP only and logs why, and the dashboard Service
    names the holder in an annotation:

    ```console
    # stratum kubectl -n stratum-dashboard get svc stratum-dashboard \
      -o jsonpath='{.metadata.annotations.stratum\.ciq\.com/nodeport-held-by}{"\n"}'
    ```

    Set `spec.dashboard.nodePort` to a free port on every control plane and
    restart `stratum up` on each, or move the other Service off the port. Once
    the port is free, the operator puts the NodePort back on its next pass.
    Until then, use a port-forward.

3. Trust the certificate. The certificate is signed by the dashboard's own
   CA, which your browser does not trust yet. Export the CA:

    ```console
    # stratum kubectl -n stratum-dashboard get secret stratum-dashboard-tls \
      -o jsonpath='{.data.ca\.crt}' | base64 -d > stratum-ca.crt
    ```

    Then add it to the trust store of the machine that runs the browser:

    | OS | Command |
    |---|---|
    | macOS | `sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain stratum-ca.crt` |
    | RHEL, Rocky, Fedora | `sudo cp stratum-ca.crt /etc/pki/ca-trust/source/anchors/ && sudo update-ca-trust` |
    | Debian, Ubuntu | `sudo cp stratum-ca.crt /usr/local/share/ca-certificates/ && sudo update-ca-certificates` |
    | Windows (admin PowerShell) | `certutil -addstore -f Root stratum-ca.crt` |

    Firefox keeps its own store: import the file under *Settings → Privacy &
    Security → Certificates → View Certificates → Authorities*.

    You are adding a root to that machine, so it matters what the root can
    vouch for. The dashboard CA is name-constrained: it can only sign for the
    dashboard's own names, `localhost`, the `publicURL` host, and addresses in
    the subnets of your nodes and VIP (a /24 for IPv4, a /64 for IPv6). It
    cannot sign another CA, and it is separate from the cluster CA, so a
    `rotate-ca` does not change it. Its key is in
    `kube-system/stratum-dashboard-ca`: whoever can read that Secret can sign
    for those names, so treat read access to Secrets in `kube-system`
    accordingly.

    For a quick look, you can accept the browser's warning instead. Do not do
    that routinely: the warning is the only sign of an attacker in the middle.
    To skip this step, serve your own certificate with `tlsSecretName`.

4. Get a token. The stack keeps two in Secrets:

    | Secret | ServiceAccount | Access |
    |---|---|---|
    | `stratum-dashboard-admin-token` | `stratum-dashboard-admin` | `cluster-admin` |
    | `stratum-dashboard-viewer-token` | `stratum-dashboard-viewer` | the built-in `view` role: read most resources, no Secrets, no changes |

    ```console
    # stratum kubectl -n stratum-dashboard get secret stratum-dashboard-admin-token \
      -o jsonpath='{.data.token}' | base64 -d; echo
    ```

    Or use a token for your own ServiceAccount; it can be short-lived:

    ```console
    # stratum kubectl create token <serviceaccount> -n <namespace> --duration=8h
    ```

5. Sign in. Open the address, paste the token into **Service account
   token**, and choose **Sign in**. The session lasts 12 hours, survives a
   restart of the dashboard pod, and ends early if the cluster stops accepting
   the token. Sign out from the user menu (**Sign out**).

The stored tokens do not expire. Anyone who can read Secrets in
`stratum-dashboard` can take the admin one, so limit that access, or set
`tokenSecrets: false` and hand out short-lived tokens instead. When the API
server stops accepting a stored token, as after a `rotate-ca` that replaces the
service-account signing key, the operator replaces the Secret within about five
minutes and the cluster fills in a new token. Get it again and sign in with it.

### Without a NodePort

With `nodePort: 0`, or when the node port is not reachable, forward the Service
to your own machine. This needs only access to the API server:

```console
# stratum kubectl -n stratum-dashboard port-forward svc/stratum-dashboard 8443:443
```

Then open `https://localhost:8443`. The certificate names `localhost` and
`127.0.0.1`, so a trusted dashboard CA works here too.

### From a node, without the deployment

`stratum dash` serves the same dashboard as a process on a control plane, with
no in-cluster objects. It listens on `127.0.0.1:8443` over plain HTTP, the same
trust model as `kubectl proxy`:

```console
# /usr/local/bin/stratum dash
```

From your own machine, tunnel to it over SSH and open `http://127.0.0.1:8443`:

```console
$ ssh -L 8443:127.0.0.1:8443 <user>@<control plane>
```

If something on the node already holds port 8443 (`listen tcp 127.0.0.1:8443:
bind: address already in use`), pick another with `--listen` and tunnel to that
one:

```console
# /usr/local/bin/stratum dash --listen 127.0.0.1:18443

$ ssh -L 8443:127.0.0.1:18443 <user>@<control plane>
```

To serve it on another address, give it a certificate with `--tls-cert` and
`--tls-key`; it refuses plain HTTP off loopback. Sign in with a token as above.

### When it does not connect

| Symptom | Check |
|---|---|
| The browser times out or the connection is refused | the pod is Ready (`stratum kubectl -n stratum-dashboard get pods`), a network firewall or security group in front of the node allows 30443, and you used `https://` |
| The browser warns about the certificate | the dashboard CA is not trusted on this machine (step 3), or you used an address the certificate does not name: list its names with the command below. The operator adds a newly joined node to the certificate within a minute |
| *The cluster did not accept that token.* | the token was cut short when pasted, has expired, or was replaced after a `rotate-ca`; get it again |
| *Cannot reach the dashboard server.* | the page loaded but the API calls fail: reload, then check the pod's logs (`stratum kubectl -n stratum-dashboard logs deploy/stratum-dashboard`) |
| Pages show *forbidden* | the token's RBAC does not allow it; the viewer token cannot read Secrets or change anything |

```console
$ openssl s_client -connect <address>:30443 </dev/null | openssl x509 -noout -ext subjectAltName
```

### Certificate

The operator writes the serving certificate into the `stratum-dashboard-tls`
Secret. It is signed by the dashboard CA, which the operator keeps in
`kube-system/stratum-dashboard-ca`, out of the dashboard pod's reach. No
Kubernetes component trusts that CA, and its name constraints (step 3 of
[Connect to the dashboard](#connect-to-the-dashboard)) keep it from vouching
for any other site. The certificate names the Service, `localhost`, every node
address, the VIP and the `publicURL` host. The operator reissues it when a node
joins and before it ages out, and the dashboard picks up the new one without a
restart. Disabling the dashboard keeps the CA, so browsers that trust it keep
trusting the dashboard when it is turned back on.

The operator replaces the dashboard CA, and logs that it did, when it no longer
covers the dashboard: when `publicURL` changes, or a node joins from a subnet
the CA does not include. Export and trust the new CA after that. To replace it
yourself, for example if its key may have leaked, delete the Secret; the
operator creates a new CA and reissues the certificate:

```console
# stratum kubectl -n kube-system delete secret stratum-dashboard-ca
```

To serve your own certificate instead, put it in a `kubernetes.io/tls` Secret in
`stratum-dashboard` and name it in `tlsSecretName`.

### Configuration

Configure it under `spec.dashboard` in `/etc/stratum/config.yaml`, with the same
block on every control plane:

```yaml
spec:
  dashboard:
    enabled: true                         # false removes the stack
    nodePort: 30443                       # 30000-32767; 0 serves on a ClusterIP only
    publicURL: https://dash.example.com   # where users reach it, when that is a proxy or a name; https only
    tlsSecretName: dash-cert              # your certificate instead of the dashboard CA's
    enableNodeShell: true                 # the privileged node shell; a profile can turn it off by default
    tokenSecrets: true                    # keep the admin and viewer sign-in tokens as Secrets
```

The dashboard needs the operator: with `spec.operator: false` it is off, and
`enabled: true` is refused. Setting `enabled: false` and restarting Stratum on
each control plane removes the stack file, and the operator deletes the
namespace and everything in it. Sessions are signed with the key in the
`stratum-dashboard-session` Secret.

## Logs

| Log | Where |
|---|---|
| Supervised components (containerd, kubelet, the operator) | `/var/lib/stratum/log/<component>.log` |
| The control-plane static pods | `stratum kubectl -n kube-system logs <pod>` |
