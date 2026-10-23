---
description: "Three controllers behind a VIP, and managing etcd membership."
---

# High Availability

A highly available (HA) Stratum cluster runs three controllers. Each controller
runs the Kubernetes control plane and one member of the etcd cluster.
kube-vip, a static pod on each controller, holds a *[virtual IP (VIP)](../appendices/glossary.md)* on one
controller at a time and moves it to another when that controller fails.
Workers and `kubectl` reach the API server at the VIP. This page sets the VIP,
joins controllers, moves an existing single-controller cluster onto a VIP,
removes an etcd member, and recovers an interrupted join.

## Requirements

- **Three controllers.** etcd needs a majority of its members to serve reads
  and writes. Two members tolerate no failures: losing either one loses
  quorum, and the cluster stops accepting writes. Go from one controller to
  three.
- A free address for the VIP. With the default ARP mode, the address must be
  on the same L2 segment as the controllers. On a routed network, kube-vip
  announces it over BGP instead.
- The controllers can reach each other on ports 6443 and 2379-2380. See
  [Ports between nodes](../installation/index.md#ports-between-nodes).

## Set the VIP

Set the VIP in `/etc/stratum/config.yaml` on the first controller, before it
starts:

```yaml
spec:
  vip:
    address: 192.168.1.10
```

| Field | Default | Meaning |
|---|---|---|
| `address` | none | the VIP |
| `interface` | the interface that holds the node's default route | the interface kube-vip announces the VIP on |
| `bgp` | `false` | `true` announces the VIP over BGP instead of ARP, for routed networks |

Every kubeconfig Stratum writes targets the VIP, so losing a controller does
not change the address any client uses. `stratum token create` puts the VIP in
the join commands it prints, after `--server` for a worker and after `--join`
for a controller. A node joined with the printed command still reaches the API
server when the controller that minted its token fails.

If the controller is already running without a VIP, follow
[If a controller was already running standalone](#if-a-controller-was-already-running-standalone)
instead.

## Growing to three controllers

Join each new controller from a running one. Mint a new control-plane token
for each join.

!!! warning "Join controllers one at a time, or the join loses quorum"
    Start the next controller's join only after the previous one is `Ready`
    in `kubectl get nodes`. Adding a member takes etcd from one voter to two
    before the new member starts. Until it starts, the cluster cannot serve
    quorum reads or writes, including the API server's. A second join started
    alongside the first, for example from automation that joins every
    controller in parallel, refuses, most often with
    `HA join: cannot reach etcd at <ip>: context deadline exceeded`. The
    refusal adds no member, so re-run the same join command once the first
    controller is `Ready`.

1. On a running controller, mint a control-plane token:

    ```bash
    sudo /usr/local/bin/stratum token create --control-plane
    ```

    The command prints the controller join command with the token and the CA
    pin. It writes the *certificate key*, the secret that lets a joining
    controller decrypt the shared CA material, to a `0600` file, and prints
    the file's path rather than the key.

2. Copy the certificate key file to the joining controller, for example with
   `scp`.

3. On the joining controller, run the printed join command, with the path of
   the copied key file:

    ```bash
    sudo /usr/local/bin/stratum up --join https://<vip>:6443 \
      --token <token> \
      --discovery-token-ca-cert-hash sha256:<pin> \
      --certificate-key-file <path-to-copied-key-file>
    ```

    The joining controller downloads the shared CA material over an encrypted
    channel, generates its own certificates, and joins etcd as a new member.

4. Check that the new controller is `Ready`:

    ```bash
    kubectl get nodes
    ```

5. Repeat steps 1 to 4 for the third controller.

A control-plane token can read the shared CA material, so it prints only the
controller join command. Join workers with a separate token from
`stratum token create`.

A joining controller takes the cluster's VIP from the shared certificates, so
it needs no `spec.vip` of its own. It puts the VIP in its apiserver certificate
and starts kube-vip, so it can take over the VIP when the controller that holds
it goes away. The shared certificates carry only the VIP's address. If kube-vip
should announce the VIP on a different interface on this controller, set
`spec.vip.interface`. On a cluster that announces the VIP over BGP, give the
joining controller the same `vip` block, `bgp: true` included.

The join reaches etcd directly, at the existing controllers' own addresses,
not through the VIP. `token create --control-plane` publishes those addresses
with the shared certificates, and the joining controller adds itself through
whichever one answers.

### Join tokens and the shared certs

The shared CA material lives in the `stratum-certs` Secret in `kube-system`,
encrypted. Only a token in the control-plane bootstrap group can read it. On a
cluster upgraded from an earlier release, see
[HA join tokens and the shared certs](../appendices/upgrade-reference.md#ha-join-tokens-and-the-shared-certs).

A control-plane token has the same default 24-hour lifetime as a worker token.
`--ttl` sets a different lifetime, and `--ttl 0` mints a token that does not
expire. Every control-plane token owns `stratum-certs`, so Kubernetes deletes
the Secret once the last of those tokens expires or is deleted.
`stratum token list` shows the tokens that are still live.

Each `token create --control-plane` adds its own entry to `stratum-certs`,
encrypted under its own certificate key. Earlier join commands keep working
for a controller that joins with this release. A controller running an earlier
release reads only the newest entry, so it can join only with the most
recently minted command.

Each mint also removes the entries of join commands whose token has expired or
been deleted. The operator does the same on each pass unless it is turned off
(`spec.operator: false`). `stratum-certs` holds a few dozen pending
control-plane join commands at most. A mint past that limit is refused before
it writes `stratum-certs`. The refusal names the live token IDs, and the mint
revokes the token it created. To make room, do one of these:

- Delete the tokens you no longer need, then mint again:

    ```bash
    kubectl -n kube-system delete secret bootstrap-token-<id>
    ```

- Retire every pending control-plane join command at once with
  `sudo /usr/local/bin/stratum certs rotate --rekey-shared --yes`.

`stratum certs rotate --rekey-shared --yes` re-encrypts the shared material
under a fresh certificate key, without issuing a token or rotating the CA. No
earlier certificate key decrypts it afterwards, so every pending control-plane
join command stops working. It writes the new key to a file. `stratum certs
check` shows how long the current key has been in place. On a cluster built
by an early release, `stratum-certs` can have no owner, so Kubernetes never
deletes it. Re-key it when you no longer trust the key that was in place.

### After a CA rotation or migration

A `certs rotate-ca --cluster` run re-encrypts the shared material under a fresh
key and retires every pending control-plane join command. So does a
`certs migrate-etcd-ca --cluster` or `certs migrate-kubelet-client-ca --cluster`
run on a cluster with more than one controller. A join command minted before
such a run fails with `decrypt shared certs (wrong certificate-key?)` before it
adds the node to etcd. Mint a new one once the run has completed.

Every `stratum token create`, for a worker or with `--control-plane`, refuses
while one of these runs is in progress, and while a `rotate-ca --cluster`
rotation is unfinished. Finish the run, then mint the token.

### If the join refuses

A join with `spec.vip` checks the VIP before it adds itself to etcd. Something
at the VIP on port 6443 must present a certificate from this cluster's CA that
covers the VIP, and the VIP must not be a node's own address. The node-address
check covers both address families. It reads the existing controllers' Node
records through the join server. If the VIP fails, the join names the VIP it
checked and adds nothing to etcd.

What to do depends on the refusal:

- `cannot check whether VIP <vip> is a controller's node address`: the join
  could not read the controller Node records, or a controller has not
  registered its Node yet. Restore API access, or wait for that controller to
  register, then re-run the join.
- `the VIP <vip> does not answer as this cluster's apiserver`: if
  `spec.vip.address` in the joining node's config is wrong, set it to the
  cluster's VIP, the address the other controllers' kubeconfigs target. If it
  is right, bring the VIP up on the existing controllers first. The refusal
  lists the steps, which are steps 1 to 3 of
  [If a controller was already running standalone](#if-a-controller-was-already-running-standalone).
  Then re-run the join.
- `cannot check that the VIP <vip> serves this cluster`: the check never
  reached the VIP, because the join could not read a cluster CA from the
  kubeconfigs under `/var/lib/stratum/kubeconfig`. The problem is on the
  joining node, not with the address. Leave `spec.vip.address` as it is, fix
  what the refusal reports for each of those files, and re-run the join.
- `spec.vip.address <vip> is an existing controller's node address, not a VIP`,
  or `this node's own address`: the address is a node's own, for example the
  host copied from the `--join` URL. Set `spec.vip.address` to the cluster's
  VIP and re-run the join.
- `the join command was minted by a stratum release that predates the
  member-add fence grant`: run `sudo /usr/local/bin/stratum token create --control-plane` on
  a controller that runs this release, and re-run the join with the new
  command.

Mint join commands on a controller that runs this release. A join command
minted by an earlier release carries no VIP. A controller that joins with it
serves an apiserver certificate without the VIP, unless its own `spec.vip`
names it.

## If a controller was already running standalone

A cluster that began as a single controller has kubeconfigs that point at that
node rather than at a VIP. Bring the VIP up first, then move the clients onto
it.

1. On the controller, add the VIP to `/etc/stratum/config.yaml`:

    ```yaml
    spec:
      vip:
        address: 192.168.1.10
    ```

    kube-vip announces it on the interface that holds the node's default
    route. Set `interface` under `vip` if it belongs on another one, for
    example when the node IP is on a different NIC.

2. Restart Stratum on the controller:

    ```bash
    sudo systemctl restart stratum
    ```

    The restart reissues the apiserver certificate with the VIP in it, starts
    kube-vip, and points the controller's own kubeconfigs at the VIP.

3. Check that the VIP answers as the cluster's apiserver:

    ```bash
    curl --cacert /var/lib/stratum/pki/ca.crt https://192.168.1.10:6443/healthz
    ```

    This prints `ok`.

4. On each worker that joined before the VIP existed, move its clients:

    ```bash
    sudo /usr/local/bin/stratum ha-repoint --vip 192.168.1.10
    ```

5. Once for the whole cluster, from a controller, move Cilium onto the VIP:

    ```bash
    sudo /usr/local/bin/stratum ha-repoint --vip 192.168.1.10 --cilium
    ```

`ha-repoint` changes nothing until the VIP passes the check in step 3. It
refuses unless something at the VIP on port 6443 presents a certificate from
this cluster's CA that covers the VIP. This keeps a node from losing every API
client, and Cilium from losing the API server on every node, when nothing
serves the VIP.

Then join the new controllers as in
[Growing to three controllers](#growing-to-three-controllers).

## Removing a member

To remove a controller, remove its etcd member as well as shutting it down.
Otherwise etcd keeps counting the departed node toward quorum. Run these
commands on a surviving controller. `stratum etcdctl` connects to the etcd
member on the node you run it on.

1. List the members and find the departed member's ID:

    ```bash
    sudo /usr/local/bin/stratum etcdctl member list -w table
    ```

2. Remove it:

    ```bash
    sudo /usr/local/bin/stratum etcdctl member remove <ID>
    ```

!!! warning "A destroyed member that is not removed costs you quorum"
    etcd counts a destroyed node toward quorum until you remove its member.
    Three members, with one destroyed and not removed, tolerate no further
    failures.

## When a join is interrupted

A `stratum up --join` that fails, or whose node dies during the join, can
leave the node's etcd member in the cluster while the node's own state
(`/var/lib/stratum/config/node.yaml`) was never written. Before it adds the
member, the join writes a journal to
`/var/lib/stratum/config/join-intent.json`. The journal records the member's
name, peer URL and ID, and the prepared configuration. Stratum removes it once
a later start confirms this node's local etcd data, or once a failed join has
removed the member it added.

To recover, re-run the same join command. The re-run reads the journal and
checks it against the cluster's member list:

- The recorded member is still listed: the join adopts it and finishes without
  adding a second member. This works only from the address the journal
  recorded. If the node would join at a different address, the join refuses
  and tells you to remove the member.
- A member list backed by quorum confirms that the recorded member is absent:
  the join proceeds as a fresh one.
- The reply to the member add was lost: the member list decides. The join
  adopts a member that was added. It retries an add that never landed once,
  and only when a member list backed by quorum confirms the member is absent.

When etcd has no quorum, the join can still resume a matching recorded member
from the node's retained trust and prepared configuration, and the surviving
member's view of the membership. Resuming requires the recorded CA pin,
hostname, address, profile and VIP. The join refuses a fresh add, or a cleanup
of a stale journal, when it cannot confirm the member is absent. A journal
written by an earlier release has no prepared configuration, and needs a
working API server to resume.

While the journal exists and the node has no saved state, a bare
`stratum up`, without `--join`, refuses to start. Starting would bring the node
up as a separate single-member cluster while the joined cluster still counts it
toward quorum. Re-run the join instead.

Remove the journal by hand only after a member list backed by quorum confirms
that the recorded member is absent.
[Removing a member](#removing-a-member) needs a working quorum. If adding the
second voter lost quorum, resume the joining node at its recorded address. If
the node or its local recovery material is permanently lost, recover the
cluster from a snapshot. See [Snapshot & Restore](snapshot-restore.md).
