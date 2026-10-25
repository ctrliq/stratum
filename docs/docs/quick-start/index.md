# Quick Start

This page brings up a single-node cluster from the `stratum` binary, then adds a
worker. The first node runs both the control plane and your workloads, and it
needs no access to a container registry. You need a Linux host and root access
to it.

## Prerequisites

- A Linux host, amd64 or arm64, with cgroup v2. Stratum does not support
  cgroup v1.
- Linux kernel 4.19.57 or later. Kernel 5.10 or later is recommended.
- Root access to the host.
- About 2 GB of free disk space for the binary and its unpacked payload.
- On an SELinux host, such as Rocky Linux, Red Hat Enterprise Linux (RHEL) or
  Rocky Linux from CIQ (RLC), the SELinux policy packages. `stratum up` needs
  them when SELinux is enforcing. See [SELinux](../installation/index.md#selinux).

    ```console
    # dnf install container-selinux policycoreutils-python-utils libselinux-utils
    ```

## 1. Put the binary on the node

Copy the `stratum` file to the node, then install it and check that it runs:

```console
# install -m 0755 stratum /usr/local/bin/stratum

$ stratum version
```

A command shown after a `#` prompt runs as root, in a root shell or with
`sudo`. These docs call `stratum` by its full path, `/usr/local/bin/stratum`,
because `sudo` may not search `/usr/local/bin`. On stock Rocky Linux, for
example, sudo's `secure_path` leaves it out, so `sudo stratum` fails with
`command not found`.

## 2. Start a node

```console
# /usr/local/bin/stratum up
```

`stratum up` unpacks the payload, creates the cluster's certificates, imports
the container images and starts containerd and kubelet. It then starts etcd and
the control plane, and applies Cilium, CoreDNS and the `local-path`
StorageClass. The node registers as schedulable, so it runs workloads as well as
the control plane.

`stratum up` stays in the foreground as the node's supervisor. Run the commands
in the next steps from a second terminal. When the node has registered,
`stratum up` prints
`stratum: node <name> registered after <time>; labeling it control-plane and stamping its version`.

`stratum up` also installs and enables `stratum.service`, so the node starts
again after a reboot. See
[Reboot persistence](../installation/index.md#reboot-persistence).

!!! tip "Hand the node to systemd"
    To run the node under systemd instead of in your terminal, stop
    `stratum up` with Ctrl-C, then run `sudo systemctl start stratum`.

## 3. Use the cluster

```console
# /usr/local/bin/stratum kubectl get nodes

# /usr/local/bin/stratum kubectl get pods -A
```

`stratum kubectl` is the bundled kubectl. On a controller it uses
`/var/lib/stratum/kubeconfig/admin.conf` unless you set `KUBECONFIG` yourself.
For a client on another machine, copy that file.

The node reaches `Ready` once Cilium is up. CoreDNS and the `local-path`
StorageClass follow.

## 4. Open the dashboard

The cluster runs a dashboard on port `30443` of every node. Once the node has
registered, `stratum up` prints the dashboard's address and the command for a
sign-in token, on lines that begin `stratum: dashboard`. Print the token, then
open `https://<node address>:30443` and paste it:

```console
# /usr/local/bin/stratum kubectl -n stratum-dashboard get secret stratum-dashboard-admin-token \
  -o jsonpath='{.data.token}' | base64 -d; echo
```

The browser warns about the certificate until you trust the dashboard
certificate authority (CA). See
[Connect to the dashboard](../cluster-admin/index.md#connect-to-the-dashboard)
for that, the firewall port, and the other ways to connect.

## 5. Add a worker

1. On the controller, mint a join token:

    ```console
    # /usr/local/bin/stratum token create
    ```

    It prints a complete join command, including the CA pin: the hash that lets
    the new node check that it reached your cluster.

2. On the new node, install the binary as in step 1, then run the printed
   command. It has this form:

    ```console
    # /usr/local/bin/stratum up --worker --server https://<controller>:6443 \
      --token <token> \
      --discovery-token-ca-cert-hash sha256:<pin>
    ```

    On a cluster with a non-default pod CIDR, service CIDR or cluster domain,
    the printed command also carries `--pod-cidr`, `--service-cidr` or
    `--cluster-domain`. Keep them.

3. On the controller, check that the worker reaches `Ready`:

    ```console
    # /usr/local/bin/stratum kubectl get nodes
    ```

If the join stops with
`worker join needs --server, --token and --discovery-token-ca-cert-hash`, the
command is missing one of the three. Run the printed command whole. To keep the
token out of your shell history, pass it with `--token-file <file>` or in
`$STRATUM_TOKEN` in place of `--token`.

A token expires after 24 hours by default. A join with an expired token waits
for about 30 seconds, then fails with
`cluster-info carries no jws-kubeconfig-<token-id> signature`. Run
`sudo /usr/local/bin/stratum token list` on the controller to see the live tokens and their
remaining lifetime, then mint a fresh one. `--ttl 0` mints a token that never
expires, which `stratum token list` shows as `never` and you delete by hand. Run
`stratum token create --help` for `--ttl` and the other flags.

To add control-plane nodes instead, see
[Growing to three controllers](../cluster-admin/high-availability.md#growing-to-three-controllers).
The certificate-key file that `stratum token create --control-plane` writes
decrypts the shared cluster CA material. Delete it from both nodes once the new
controller is `Ready`.

## Next

- [Installation](../installation/index.md): topologies, configuration and the
  artifact
- [High Availability](../cluster-admin/high-availability.md): three controllers
  and a virtual IP
- [Hardening](../cluster-admin/hardening.md): the security controls every
  cluster runs
- [The dashboard](../cluster-admin/index.md#the-dashboard): connecting, tokens
  and configuration
