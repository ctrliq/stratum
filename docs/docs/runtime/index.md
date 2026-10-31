# Container Runtime

This page describes the container runtime Stratum runs on every node:
containerd and runc. It covers where the runtime keeps its files, how Stratum
configures it, how to recover from a containerd process Stratum did not start,
and how to pull images through a registry mirror.

## containerd and runc

Stratum ships containerd and runc, built from source, and its supervisor runs
containerd like any other component. The kubelet reaches containerd through
the Container Runtime Interface (CRI) socket.

| | |
|---|---|
| CRI endpoint | `/run/stratum/containerd/containerd.sock` |
| State | `/var/lib/stratum/containerd` and `/run/stratum/containerd` |
| Configuration | `/var/lib/stratum/config/containerd.toml` |
| cgroups | v2 only. `stratum up` refuses to start without the cgroup v2 hierarchy at `/sys/fs/cgroup` |
| Sandbox image | served from the payload, never pulled |

A pod runs under runc unless its `runtimeClassName` names a RuntimeClass from
[`spec.runtimeClasses`](../installation/configuration.md#runtimeclasses).

## Paths

Stratum's containerd keeps its socket and state under `/run/stratum` and
`/var/lib/stratum`, not at the containerd defaults. A containerd that a
distribution package installed can run on the same node without conflict, and
`stratum uninstall` removes Stratum's directories without touching the other
containerd's.

The kubelet's root directory is `/var/lib/stratum/kubelet`, not
`/var/lib/kubelet`. Point a tool that reads the kubelet's pod-resources API at
`/var/lib/stratum/kubelet/pod-resources/kubelet.sock`. On the upstream default
path it finds no socket, and some tools still run without an error. A GPU
metrics exporter, for example, then exports its metrics without the pod and
namespace they belong to.

Device plugins need no path change. The kubelet keeps the device-plugin
registration socket at `/var/lib/kubelet/device-plugins/kubelet.sock` whatever
its root directory. On an SELinux host, a pod that connects to either socket
must also run as `spc_t` or privileged. See the SELinux
[Containers](../installation/configuration.md#containers) section.

## The sandbox image comes from the payload

Every pod runs a `pause` container, the *sandbox image*. Upstream containerd
pulls it from `registry.k8s.io` when the first pod starts. Stratum points
containerd at the sandbox image in the payload instead, by the name the payload
registers, so no pod start needs a registry.

## Payload images and kubelet's pull-credential check

Before a pod uses an image that is already on the node, the kubelet can check
that the pod is allowed to pull it. Stratum exempts the payload registry's
namespace from that check. In release builds, the kubelet configuration sets
`imagePullCredentialsVerificationPolicy: NeverVerifyAllowlistedImages`, with
`ghcr.io/ctrliq/stratum/*` in `preloadedImagesVerificationAllowlist`. The
kubelet always uses a payload image from the node, even after something pulls
the same image under another name, such as `docker.io/library/busybox`.

The exemption covers every image under that namespace, not only the ones in
the payload. An image pulled under it with a pull secret can be used from the
node by any pod, without that secret.

Every other image is checked. An image the kubelet pulls, directly or through a
[registry mirror](#registry-mirrors), is checked against the credentials it was
pulled with, as upstream Kubernetes does by default.

The kubelet also checks an image you load onto a node by hand, for example with
`ctr images import`. Upstream's default policy never checks an image the
kubelet did not pull, so this differs from upstream Kubernetes. With `imagePullPolicy:
IfNotPresent` the check is a pull from the image's registry, which fails on an
airgapped node. With `imagePullPolicy: Never` the pod fails with
`ErrImageNeverPull` even though the image is on the node. Serve your own images
from a registry mirror instead. This applies to clusters you upgrade as well as
new ones: every `stratum up` rewrites the kubelet's configuration. See
[Images loaded onto a node by hand](../appendices/upgrade-reference.md#images-loaded-onto-a-node-by-hand).

## Configuration

Stratum writes containerd's configuration, `/var/lib/stratum/config/containerd.toml`,
from containerd's own default. It sets the cgroup driver, the sandbox image
and `enable_selinux`. That setting is on only for a node Stratum has labelled
for SELinux; on a node that opted out with `spec.selinux: false`, or that lacks
the policy packages, it is off and containers are not confined. See
[Containers](../installation/configuration.md#containers). It is a normal containerd
configuration file that you can read. When the file lacks one of those
settings, Stratum replaces the whole file with a fresh default, so do not rely
on hand edits to it.
containerd reads the file when it starts, so a change takes effect on the next
`stratum up` or in-place upgrade.

Stratum sets the same cgroup driver for containerd and the kubelet: the systemd
driver when systemd is PID 1, and cgroupfs otherwise.

## A containerd Stratum did not start

A containerd process left over from an earlier run can keep serving
`/run/stratum/containerd/containerd.sock` after the supervisor has lost track of
it. For example, a second `stratum up` killed part way can leave one behind. The
supervisor's own containerd then cannot bind the socket and exits. The
supervisor keeps restarting it, with a backoff of up to 30 seconds, while the
leftover containerd keeps serving pods.

When the supervisor's containerd exits and the socket is still live, the
supervisor prints this line once:

```text
supervisor: containerd exited but /run/stratum/containerd/containerd.sock is still served by another process — an orphan from an earlier run. This node is NOT healthy: ...
```

From then on, the supervisor reports containerd as `Foreign`, not as running. A
`Foreign` component fails the [upgrade and rollback health
gate](../appendices/upgrade-reference.md#the-node-health-gate). The supervisor keeps
restarting containerd. It clears `Foreign` once one of its own containerd
processes has stayed up for 5 seconds. That shows the process did not exit the
way it does when another process owns the socket. It does not prove that the
process holds the socket.

`stratum version --components` checks the same thing. It lists the versions of
the bundled components. When the CRI socket is live and the containerd serving
it reports a different version, it adds a line like this:

```text
WARN: containerd <installed version> is installed but the runtime serving /run/stratum/containerd/containerd.sock reports <running version> — another containerd, likely an orphan from an earlier run, holds the socket
```

It prints no warning when nothing serves the socket or when it cannot read
either version.

To recover, find the process that holds the socket, check it, and stop only
that process. Do not run `stratum uninstall` for this: it deletes the node's
state, including etcd data and certificates.

!!! warning "Stopping the wrong containerd stops the host's containers"
    Never kill a containerd that does not hold
    `/run/stratum/containerd/containerd.sock`. The host's own containerd, or the
    supervisor's short-lived child that is failing to start, is not the orphan.

1. Find the process that owns the socket. Match on the socket, not on the name
   `containerd`. A host that also runs Docker or the NVIDIA container toolkit has
   its own containerd, and a name match lists it too.

    ```console
    # ss -xlp 'src = /run/stratum/containerd/containerd.sock'
    ```

    If `ss` is not installed, use `fuser`:

    ```console
    # fuser -v /run/stratum/containerd/containerd.sock
    ```

2. Check what that process is. For an orphan from an earlier Stratum run, the
   path is a Stratum containerd, normally `/var/lib/stratum/bin/containerd`:

    ```console
    # readlink /proc/<pid>/exe
    ```

    After an upgrade the orphan's binary may have been replaced, so the path can
    end in ` (deleted)` or point at a backup copy such as
    `/var/lib/stratum/bin/containerd.bak-20260928-100000`. Both are still a
    Stratum containerd. A path anywhere else, such as `/usr/bin/containerd`, is
    not the orphan: leave it alone.

3. Stop only that process:

    ```console
    # kill <pid>
    ```

The supervisor's next restart takes over the socket. Run
`stratum version --components` again to confirm the warning is gone.

## Registry mirrors

Images in the payload never need a registry. To pull your own workload images
through a mirror, describe the mirror in `/etc/stratum/registries.yaml` on each
node. The format is the one RKE2 and k3s use, so an existing file works
without changes for the fields Stratum supports: `mirrors` endpoints, and
`auth` and `tls` under `configs`. A `configs` entry works with or without a
matching mirror: with none, images are pulled from that registry directly,
carrying its credentials and TLS settings. Stratum ignores a `rewrite` block
(see below).

In this example, `docker.io` and every other registry (`"*"`) pull through one
mirror. Entries under `configs` are keyed by the endpoint's `host:port`:

```yaml
mirrors:
  docker.io:
    endpoint:
      - "https://mirror.example.com:5000"
  "*":
    endpoint:
      - "https://mirror.example.com:5000"
configs:
  "mirror.example.com:5000":
    auth:
      username: <user>
      password: <password>
    tls:
      ca_file: /etc/pki/mirror-ca.crt
```

A `configs` entry can also stand alone, with no mirror at all — for a private
registry that is pulled from directly:

```yaml
configs:
  "registry.corp:5000":
    auth:
      username: <user>
      password: <password>
    tls:
      ca_file: /etc/pki/corp-ca.crt
```

The file can hold registry passwords, and Stratum does not change its mode.
Keep it owned by root with mode `0600`:

```console
# chown root:root /etc/stratum/registries.yaml

# chmod 0600 /etc/stratum/registries.yaml
```

Stratum translates the file into containerd's own registry configuration:
one `/etc/containerd/certs.d/<registry>/hosts.toml` per registry listed under
`mirrors` and per `configs` entry with usable auth or TLS — credentials, a CA
file, disabled verification, or a client certificate with its key (`_default`
for `"*"`) — and a drop-in, `/etc/containerd/conf.d/stratum-registry.toml`,
that points containerd at that directory. A `configs` entry's `auth` and `tls`
are written as root-level keys of that registry's `hosts.toml`: containerd
uses them when it talks to the registry itself, which it tries after any
mirror endpoints. For a loopback registry (`localhost` or a loopback address),
Stratum also writes a root-level `server = "https://<registry>"`, because
containerd would otherwise disable TLS verification for the registry it
configures implicitly. A `hosts.toml` that holds credentials is written with
mode `0600`.

Because a `configs` entry gets its own `certs.d` directory, containerd no
longer falls back to the `_default` catch-all (`"*"`) for that registry: a
`"*"` mirror's endpoints do not apply to a registry that has its own entry.

`configs."*"` applies only its `ca_file` and `insecure_skip_verify`. Its
`auth` and client certificate are not written, and `registry apply` and
`registry check` warn when they are set: the catch-all file cannot name a
server, and containerd turns TLS verification off for a loopback registry it
reaches that way, so the credentials could go to a local listener unverified.
Give each registry that needs credentials its own `configs` entry.

!!! warning "A configured registry's `hosts.toml` is replaced"
    For every registry listed under `mirrors` or `configs`, Stratum overwrites
    `certs.d/<registry>/hosts.toml`, including one you wrote by hand — a
    hand-written `hosts.toml` for a host that appears in `configs` (with or
    without a mirror) is replaced too. Entries for registries not in
    `registries.yaml` are left alone: when a mirror or registry config is
    removed, Stratum deletes only the files it generated.

Stratum applies the file each time the node starts. To apply a change without
restarting the node, preview it with `registry check`, which does not print
secrets, then write it with `registry apply`:

```console
# /usr/local/bin/stratum registry check

# /usr/local/bin/stratum registry apply
```

containerd reads `hosts.toml` on every pull, so changes to mirror endpoints,
credentials or TLS take effect at once. `apply` restarts containerd only when
the drop-in is created, changed or removed: on the first `apply`, and when the
last mirror or registry config is removed. Deleting `registries.yaml` and
running `apply` removes the mirrors and the drop-in, and restarts containerd.

Credentials are sent as a static `authorization` header (Basic or Bearer).
Registries that require Docker Hub's token flow may still reject it with 401;
point a mirror at such a registry instead of authenticating to it directly.

!!! note "`rewrite` is not supported"
    containerd's registry configuration has no path rewriting. A `rewrite`
    block is reported as a warning and ignored.
