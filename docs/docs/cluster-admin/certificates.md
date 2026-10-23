# Certificates

This page covers the certificates and certificate authorities (CAs) a Stratum
cluster runs on: how Stratum renews them, how you rotate them by hand, and how
you recover a rotation that halted. Use it to check certificate expiry, to
rotate the cluster CAs, or to move a cluster built by an earlier release onto
the newer CAs. The `stratum certs` commands run as root on a node, and the
cluster-wide ones run from a control plane. The `kubectl` commands need
cluster-admin access.

## Certificate commands

| Command | What it does |
|---|---|
| `sudo /usr/local/bin/stratum certs check` | Prints each leaf certificate's expiry and the age of the shared certificate key |
| `sudo /usr/local/bin/stratum certs rotate --yes` | Reissues the leaf certificates from the existing CAs |
| `sudo /usr/local/bin/stratum certs rotate --rekey-shared --yes` | Re-keys the shared certificate material of a high-availability (HA) cluster |
| `sudo /usr/local/bin/stratum certs rotate-ca --cluster --yes` | Rotates the cluster CAs on every node |
| `sudo /usr/local/bin/stratum certs migrate-etcd-ca --cluster --yes` | Moves a cluster built before the etcd CA onto it |
| `sudo /usr/local/bin/stratum certs migrate-kubelet-client-ca --cluster --yes` | Moves a cluster built before the kubelet client CA onto it |

Every command except `certs check` and `rotate-ca --check` needs `--yes`.
Without it, the command refuses and says what it would disrupt.

`rotate-ca`, `migrate-etcd-ca` and `migrate-kubelet-client-ca` also need an
operation selector:

- `--cluster` runs every phase across the cluster. `rotate-ca` and
  `migrate-kubelet-client-ca` run on every node. `migrate-etcd-ca` runs on
  every control plane, because workers run no etcd. Start a `--cluster` run
  from a control plane. On a worker, the command refuses.
- `--phase=<name>` runs one phase on the current node only. A `--cluster` run
  executes `--phase` on each node. You need it yourself only to recover a
  halted run by hand.
- `--rollback` undoes a failed run. `rotate-ca --rollback` reverts the current
  node only, even with `--cluster`. `migrate-etcd-ca --rollback` reverts the
  current node, or every control plane with `--cluster`.
  `migrate-kubelet-client-ca --rollback` needs `--cluster` and reverts every
  node.

Without `--cluster`, `--phase` or `--rollback`, the command refuses and lists
the phases.

## Leaf certificates

Stratum issues leaf certificates for 365 days. Each control plane checks its
leaves daily and rotates any that are within 45 days of expiry. Run
`certs rotate` to rotate the leaves at once instead. To turn the automatic
rotation off, set `spec.certRenew: false` in `/etc/stratum/config.yaml`. It is
on by default.

If the leaves have already expired, for example on a cluster powered off for
more than a year, the apiserver rejects the expired credentials, so a rotation
cannot take the cluster maintenance lease. It warns and rotates under the node
lock alone instead. Run `sudo /usr/local/bin/stratum certs rotate --yes` on
each control plane, one at a time, or let the daily check do it.

If a certificate file exists but cannot be read or parsed, `certs check` fails
and names the file, and the daily rotation fails with the same error until it
is fixed. `sudo /usr/local/bin/stratum certs rotate --yes` reissues a damaged
leaf only when its signer reissues it without reading the damaged file: it
cannot repair `etcd` or `apiserver-etcd-client` while the etcd CA is on disk,
or `apiserver-kubelet-client` unless the kubelet client trust is split. If a
directory stands where a leaf belongs, move it aside first, as the error shows:
rotation replaces the file by rename, which cannot replace a directory. A
damaged CA cannot be reissued this way. A forced rotation whose `certs check` fails
cannot use the expired-leaf fallback that skips the cluster maintenance lease,
so it still needs the lease.

`certs check` also lists the root CAs, which are valid for 10 years. Stratum
never renews a CA automatically. To replace one, run `rotate-ca`.

## The shared certificate key

An HA cluster keeps the control planes' shared certificate material in the
`stratum-certs` Secret, encrypted with the *certificate key* (see the
[Glossary](../appendices/glossary.md)). `certs check` reports when the key was
last set. It reads that from the cluster, so it does so on a control plane only.
On a worker, `certs check` says so in one line, and shows only that node's
certificates. The key changes only when one of these happens:

- `token create --control-plane` mints a control-plane join command.
- A `--cluster` run of `rotate-ca`, `migrate-etcd-ca` or
  `migrate-kubelet-client-ca` re-encrypts the shared material.
- You re-key it with `certs rotate --rekey-shared`.

A cluster you install once and never grow keeps its install-time key until one
of these happens. The `--cluster` runs retire every control-plane join command
minted before them (see
[Growing to three controllers](high-availability.md#growing-to-three-controllers)).

`certs rotate --rekey-shared` mints a fresh key and re-encrypts the shared
material. After it runs, no earlier key decrypts anything, and every pending
control-plane join command stops working. Stratum writes the new key to a
`0600` file and prints the file's path, never the key. Existing control planes
need no action.

## Upgrading from an earlier release

These behaviours can change what a script or runbook written for an earlier
release does.

`rotate-ca --cluster --yes` refuses to start on a cluster that runs operators
or other workloads that call the API server, and lists those workloads. A
script that runs it unattended must pass `--restart-workloads` or
`--skip-workload-check`. See
[Workloads that call the API server](#workloads-that-call-the-api-server).

Every `--cluster` rotation checks the service-account tokens mounted on each
node before `finalize`, then waits 2 minutes. If a node still mounts a token
the old key signed, the run keeps checking for up to 60 minutes, then halts
before `finalize`. See
[Before `finalize`: mounted tokens](#before-finalize-mounted-tokens).

The API server verifies each kubelet's serving certificate against the cluster
CA, in every profile. The operator approves the kubelets' serving certificate
signing requests (CSRs). With the operator off, a new worker cannot obtain a
serving certificate automatically, and an existing worker cannot renew its
certificate automatically (see
[Disable the operator](hardening.md#disable-the-operator-and-remove-its-admission-webhook)).
During a rotation:

- `sign-new` renews every worker's serving certificate under the new CA. It
  refuses when the serving certificate is missing or its renewal CSR stays
  unapproved.
- A control plane whose profile bootstraps its serving certificate renews it at
  the start of its own `finalize`, and restarts its kubelet.
- `rotate-ca --cluster` refuses to finalize when a reachable worker serves a
  certificate the new CA did not sign. It also refuses when the kubelet of a
  control plane whose profile does not bootstrap its serving certificate cannot
  be verified against the new CA.
- A worker's `finalize` refuses when its serving certificate is missing,
  unreadable or still signed by the old CA.

A node that finalized a rotation on an earlier release can still serve a
certificate from the CA that rotation dropped. See
[Nodes that finalized a CA rotation on an earlier build](../appendices/upgrade-reference.md#nodes-that-finalized-a-ca-rotation-on-an-earlier-build).

A cluster created by this release has a fourth CA, `kubelet-client-ca`, and its
kubelets trust it alone. A certificate the cluster root signs, `admin`'s
included, does not reach a kubelet's API (see
[The kubelet's client trust](hardening.md#the-kubelets-client-trust)).
Role-based access control (RBAC),
not `system:masters`, authorizes `apiserver-kubelet-client` (see
[The apiserver's kubelet client](hardening.md#the-apiservers-kubelet-client)).

- A cluster created by an earlier release keeps the cluster root as its
  kubelets' client trust until you run
  [`certs migrate-kubelet-client-ca`](#migrating-an-existing-cluster-onto-the-kubelet-client-ca).
- Join a cluster created by this release only with nodes of this release or
  later.
- `certs rotate-ca` rotates `kubelet-client-ca` with the cluster root. It
  refuses to start on a node part way through the migration.

`certs rotate-ca --cluster`, `certs migrate-etcd-ca --cluster` and
`certs migrate-kubelet-client-ca --cluster` handle SIGHUP like SIGINT and
SIGTERM. The first signal asks the command to stop at the next step boundary,
and cleanup runs. Follow the printed recovery guidance before you resume a
stopped run. A second signal forces termination. When a shell or `nohup` set
the signal to be ignored, Stratum uses SIGKILL for that forced stop.

## Rotating the cluster CAs

`certs rotate-ca` replaces the cluster root CA, the front-proxy CA, the
kubelet client CA and the service-account signing key. It runs in three
phases, so that no node ever trusts a CA another node does not:

| Phase | What it does |
|---|---|
| `trust-new` | Stages the new CAs and adds them to every trust bundle, beside the old ones |
| `sign-new` | Promotes the new CAs, reissues the leaves under them, and signs new service-account tokens with the new key |
| `finalize` | Drops the old CAs and the old service-account key |

Between `trust-new` and `finalize`, every node trusts both the old and the new
CAs, written *old‖new* on this page.

Run the rotation from a control plane:

```bash
sudo /usr/local/bin/stratum certs rotate-ca --cluster --restart-workloads --yes
```

The sections below follow the run in order: what it does to your workloads,
the checks before each phase, and what it prints when a check fails.

### Restart workloads after `rotate-ca`

The phases protect Stratum's own components, not your workloads. A pod that
talks to the API server through in-cluster config (operators, cert-manager,
ingress controllers, CoreDNS, Cilium) can fail in two ways during a rotation:

- From `sign-new`, a cached CA causes `x509` errors. The API server starts
  serving a certificate from the new CA. The pod's mounted `ca.crt` already
  holds both CAs, but most clients load the CA pool once, at startup, and never
  re-read it. Every API call fails with
  `x509: certificate signed by unknown authority` until the pod restarts. The
  error does not clear by itself. A controller stuck in leader election never
  exits, so Kubernetes never restarts it for you.
- After `finalize`, an old-key token causes `401` errors. From `sign-new`, the
  API server signs new service-account tokens with the new key. It accepts
  tokens signed by either key until `finalize` drops the old one. A pod
  restarted after `sign-new` holds a new-key token and is unaffected. A pod
  still holding an old-key token at `finalize` gets `401 Unauthorized`, because
  it was never restarted or because its client caches the token. `rotate-ca`
  refuses to finalize while a pod still mounts an old-key token. See
  [Before `finalize`: mounted tokens](#before-finalize-mounted-tokens).

Pods that do not call the API keep serving traffic. Controllers, DNS records
for new Services, ingress route changes and certificate renewal all stop until
the affected workloads restart.

The restart that clears the `x509` errors must happen after the new CA is
published and before `sign-new`. `rotate-ca` checks for it, and refuses until
you choose who restarts the workloads (see
[Workloads that call the API server](#workloads-that-call-the-api-server)):

- With `--cluster --restart-workloads`, Stratum restarts the workloads between
  `trust-new` and `sign-new`. You restart nothing for `x509`.
- With `--cluster --skip-workload-check`, Stratum restarts only its own
  add-ons. Restart your workloads once, when the command completes. They are
  degraded from `sign-new` until then.
- With `--phase`, restart the workloads after `trust-new` has completed on
  every node and before you run `sign-new`. Otherwise `sign-new` refuses.

A pod restarted before `sign-new` is issued an old-key token. A client that
re-reads its token file picks up a new-key token when the kubelet refreshes
it. That client needs no restart after `sign-new`, provided it was restarted
before it. A workload you handle yourself with `--skip-workload-check` still
needs its restart, for `x509`. Two cases still get `401` after `finalize`:

- Clients that cache their service-account token in memory keep the old-key
  token they started with, even after a restart before `sign-new`. Restart them
  after `sign-new` has completed on every control plane and before `finalize`.
  The `finalize` check reads mounted token files, so it cannot see these
  clients. `--cluster` runs `finalize` itself, so to restart them in that
  window, run the phases with `--phase`. Otherwise restart them when
  `--cluster` completes. They get `401` until then.
- Long-lived tokens from `kubernetes.io/service-account-token` Secrets, and
  kubeconfigs built from them (CI/CD credentials, Argo CD cluster Secrets), were
  signed with the old key and never rotate. They get `401` after `finalize`,
  and restarting the pod that mounts them does not help. Delete and recreate
  each Secret so it is issued a token under the new key, then update any
  kubeconfig built from it. To list them:

    ```bash
    kubectl get secrets -A --field-selector type=kubernetes.io/service-account-token
    ```

`rotate-ca` prints a warning before it starts. `rotate-ca --cluster` prints:

```text
WARNING: workloads that use in-cluster API config (operators, cert-manager, ingress controllers, Cilium)
         keep the CA they started with, so from sign-new they fail with x509 errors until restarted.
         Stratum restarts its own add-ons before sign-new. Your Deployments, DaemonSets and StatefulSets
         are restarted before sign-new with --restart-workloads; with --skip-workload-check they are
         not, and you restart them once, when this command completes.
         A client that re-reads its service-account token needs no restart after sign-new, if it was
         restarted before it.
         Finalize refuses while any node still mounts a token signed with the old key. That scan reads
         mounted token files, so it cannot see a client that caches its token in memory: that client
         holds an old-key token even after a restart before sign-new, and is rejected with 401 once
         finalize drops the key. Restart it after sign-new has completed on every control plane and
         before finalize. This command runs finalize itself, so use --phase runs for that window;
         otherwise restart it when this command completes.
         Tokens from kubernetes.io/service-account-token Secrets do not rotate: recreate those
         Secrets (and any kubeconfig built from them) after finalize; a restart does not help.
```

`rotate-ca --phase` prints:

```text
WARNING: workloads that use in-cluster API config (operators, cert-manager, ingress controllers, Cilium)
         keep the CA they started with, so from sign-new they fail with x509 errors until restarted.
         Restart all of them, Stratum's own add-ons included, after trust-new has completed on every
         node and before sign-new; sign-new refuses until they have, unless you pass --skip-workload-check.
         A client that re-reads its service-account token needs no restart after sign-new, if it was
         restarted before it.
         A client that caches its service-account token in memory holds an old-key token even after
         that restart, and is rejected with 401 once finalize drops the key. Restart it after sign-new
         has completed on every control plane and before finalize.
         --phase=finalize on a control plane refuses while that node mounts a token signed with the
         old key. It checks that control plane only, and finalize on a worker does not check: scan
         every node before the first control plane finalizes. The scan reads mounted token files, so
         it does not see a client that caches its token in memory.
         Tokens from kubernetes.io/service-account-token Secrets do not rotate: recreate those
         Secrets (and any kubeconfig built from them) after finalize; a restart does not help.
```

Under `--cluster`, each node's Job runs `rotate-ca --phase`, so every node's
Job log also carries the `--phase` warning. The warning is expected there and
does not mean the phase failed. Follow the `--cluster` warning.

Stratum deletes a Job that succeeds. A failed Job stays in place, and the
error `rotate-ca --cluster` prints ends with the command to read its log, of
the form `kubectl -n kube-system logs job/stratum-carotate-<phase>-<node>`.
Job names are cut to 63 characters, so on a long node name the real name is
shorter than that pattern. Copy it from the error, or list the Jobs:

```bash
kubectl -n kube-system get jobs -l stratum.ciq.com/carotate
```

To recover a workload that fails, do a rolling restart of every Deployment,
DaemonSet and StatefulSet that uses in-cluster config:

```bash
kubectl -n kube-system rollout restart daemonset/cilium deployment/cilium-operator deployment/coredns
kubectl -n cert-manager rollout restart deployment
kubectl -n <namespace> rollout restart deployment/<name>
```

The last line stands for each of your own operators, ingress controllers and
API clients. To find the workloads you missed, search recent pod logs for
`x509` and `401`. In a test cluster with one control plane and about 40 pods,
every restarted pod was Ready within 28 seconds, with no `x509` or `401` errors
afterwards.

### Workloads that call the API server

`rotate-ca` counts a pod as an API client when both of these are true:

- A container mounts a service-account token. The token comes from a projected
  volume with a service-account token source (the `kube-api-access-*` volume
  every pod gets, or any other name), or from a
  `kubernetes.io/service-account-token` Secret. The container can be a regular,
  init or ephemeral one. A volume that no container mounts does not count.
- The ServiceAccount of that token is a subject of a RoleBinding or
  ClusterRoleBinding. For a projected token, that is the pod's own
  ServiceAccount. For a `kubernetes.io/service-account-token` Secret, it is the
  account the Secret was issued for (its `kubernetes.io/service-account.name`
  annotation), which can be another's. The binding names the account (as a
  ServiceAccount subject, or as the user
  `system:serviceaccount:<namespace>:<name>`) or the
  `system:serviceaccounts:<namespace>` group. A binding to the cluster-wide
  `system:serviceaccounts` group does not count. Kubernetes binds that group
  itself, so it would select every pod.

`rotate-ca` ignores finished pods. Pods that are being deleted still count,
because their containers keep running on the old CA until the grace period
ends. Stratum groups pods by the controller that owns them, so it reports a
Deployment's ReplicaSet pods as the Deployment. It reports Jobs and bare Pods,
but never restarts them. Recreate those yourself.

Whoever creates a pod writes its owner reference, so Stratum trusts the
reference only when the Deployment, DaemonSet or StatefulSet it names exists
with the same UID and has a valid name. Stratum reports a pod whose reference
fails that test as the pod itself, and never restarts it. Names that are not
plain are printed quoted, and the commands in the listings are quoted for a
shell.

| Flag | Applies to | Effect |
|---|---|---|
| `--restart-workloads` | `--cluster` | Stratum restarts the Deployments, DaemonSets and StatefulSets it found, after the new CA is published and before `sign-new`, and waits for them to roll out. |
| `--skip-workload-check` | `--cluster`, `--phase=sign-new` | Turns off both checks below. Stratum restarts nothing of yours, and those workloads fail with `x509` errors from `sign-new` until you restart them. |

The command refuses before it changes anything if you pass:

- both flags: `--restart-workloads and --skip-workload-check contradict each
  other: pass one`
- `--restart-workloads` without `--cluster`: `--restart-workloads goes with
  --cluster`

### Before `trust-new` (`--cluster`)

Before the first phase, `rotate-ca --cluster` lists the API clients. If it
finds any and you passed neither flag, it refuses to start and changes nothing:

```text
refusing to start: 1 workload(s) use in-cluster API credentials, and unless they restart after the new CA is published (trust-new) and before sign-new they fail with x509 errors:
  Deployment apipoller/apipoller: stratum kubectl -n apipoller rollout restart deployment/apipoller
  Pass --restart-workloads to have Stratum restart the Deployments, DaemonSets and StatefulSets among them (Jobs and bare Pods are never restarted), or --skip-workload-check to rotate without this check and restart them yourself
```

The check runs before any change, because a run that halts while `trust-new`
is still running on some node must be resumed or rolled back before anything
else can change. The list leaves out Stratum's own add-ons, because the run
always restarts them.

Two more checks run before that one, with or without `--restart-workloads`.
They cover Stratum's own add-ons as well as your workloads, so they can refuse
even when you have no workloads of your own. Each refuses to start and changes
nothing, because a restart cannot fix the cause and the run would otherwise
halt after `trust-new`.

The first check finds a StatefulSet or DaemonSet that a restart does not
replace whole. A rollout restart leaves some or all of its pods on the old CA
when:

- a StatefulSet has a `rollingUpdate` partition, which leaves the pods below
  it.
- a StatefulSet or DaemonSet has the `OnDelete` update strategy, which does not
  recreate pods on a restart.

The refusal lists each workload with its reason, then the fix: one line for
Stratum's own add-ons and one for your workloads, as they apply:

```text
refusing to start: 2 workload(s) are not restarted whole by Stratum, so their pods stay on the old CA and the run would halt after trust-new, to be finished by hand:
  StatefulSet db/postgres: a rollingUpdate partition leaves the pods below it
  DaemonSet kube-system/cilium: the OnDelete update strategy does not recreate pods on a restart
  Switch the update strategy of Stratum's own add-ons to RollingUpdate and run this again: Stratum restarts them whatever the flags say, and waits for a rollout that OnDelete never completes
  Set the partition to 0 or the update strategy to RollingUpdate for your own workloads, or pass --skip-workload-check to rotate without this check and restart them yourself
```

Set the partition to 0, or switch the update strategy to `RollingUpdate`, then
run `rotate-ca --cluster` again. For a StatefulSet with a partition:

```bash
stratum kubectl -n db patch statefulset postgres \
  -p '{"spec":{"updateStrategy":{"rollingUpdate":{"partition":0}}}}'
```

For a StatefulSet or DaemonSet with `OnDelete`:

```bash
stratum kubectl -n kube-system patch daemonset cilium \
  -p '{"spec":{"updateStrategy":{"type":"RollingUpdate"}}}'
```

For Stratum's add-ons, such as Cilium, switching to `RollingUpdate` is the only
fix. Stratum restarts them and waits for their rollout whatever the flags say,
and an `OnDelete` rollout never completes, so `--skip-workload-check` does not
help.

If one of your own workloads needs to keep the partition or `OnDelete`, pass
`--skip-workload-check` instead, and replace its pods yourself between
`trust-new` and `sign-new`:

- For a StatefulSet with a partition, restart it, wait for the rollout, then
  delete the pods below the partition. With a partition of 1, that is
  `postgres-0`:

    ```bash
    stratum kubectl -n db rollout restart statefulset/postgres
    stratum kubectl -n db rollout status statefulset/postgres
    stratum kubectl -n db delete pod postgres-0
    ```

- For an `OnDelete` StatefulSet or DaemonSet of yours, delete every pod of it.

The second check finds a `kube-root-ca.crt` ConfigMap written in the future.
Stratum tells a restarted pod from a stale one by comparing its start time with
the time kube-controller-manager last wrote the namespace's
`kube-root-ca.crt`, read from the ConfigMap's `managedFields`. A namespace
admin can set those fields. If the time is more than 5 minutes ahead of this
host's clock, every pod in the namespace would count as stale however often it
restarts. Stratum checks every namespace with an API client, including one
whose only clients are Stratum's own add-ons, such as `kube-system`:

```text
refusing to start: apipoller/kube-root-ca.crt was written by kube-controller-manager at 2026-10-09T20:00:00Z, more than 5m0s ahead of this host's clock; its managedFields are not to be trusted (a namespace admin can set them), so pods cannot be told apart by start time
  Delete that ConfigMap so kube-controller-manager recreates it, or pass --skip-workload-check to rotate without this check
```

Delete the ConfigMap. kube-controller-manager recreates it with a current write
time. Then run `rotate-ca --cluster` again:

```bash
stratum kubectl -n apipoller delete configmap kube-root-ca.crt
```

If you pass `--skip-workload-check` instead, Stratum does not check whether
your workloads restarted, so restart them yourself between `trust-new` and
`sign-new`.

If someone forward-dates the ConfigMap after this check, the run still halts
before `sign-new` with the `was written by kube-controller-manager at` error.
Delete the ConfigMap, restart the workloads in that namespace, and finish by
hand as described in the next section.

### Between `trust-new` and `sign-new` (`--cluster`)

Once `trust-new` has completed on every node, the run:

1. Rebuilds its admin client to trust old‖new.
2. Waits up to 5 minutes for the `kube-root-ca.crt` ConfigMap in every
   namespace to carry the new CA.
3. Restarts Cilium, the Cilium operator, CoreDNS and local-path-provisioner.
   With `--restart-workloads`, it also restarts your Deployments, DaemonSets
   and StatefulSets. It waits up to 10 minutes for them to roll out. It prints
   a Job or bare Pod as `is not restarted` and continues.
4. Waits for the old pods to be gone. A rollout is done when the new pods are
   ready, but the old pods keep running on the old CA until their grace period
   ends. The run also waits out the 2-minute margin for a pod whose containers
   have not started yet (see
   [Before `sign-new`](#before-sign-new-phasesign-new)). This wait is also up
   to 10 minutes. A pod that is still on the old CA, and is neither terminating
   nor waiting out the margin, ends the wait at once and is listed. Such a pod
   belongs to a StatefulSet with a partition, an `OnDelete` workload, or a
   workload that appeared since the first check.
5. Checks, once, that no client is left on the old CA (see the next section).

The wait in step 4 is bounded as a whole, including each API call it makes.
When the old pods outlive it, the halt lists each pod with what is left of its
grace period:

```text
1 workload(s) still have pods running on the old CA while they terminate, 10m0s after the rollouts; from sign-new they fail with x509 errors:
  Deployment kong/ingress: still terminating on the old CA: ingress-7d9c-x2 (4m12s of its grace period left)
  They go when their grace period ends; sign-new can run once they have
```

If a step fails, the run halts before `sign-new`. Every node already trusts
old‖new and nothing has been promoted. To continue with `--cluster`, fix the
cause and run `sudo /usr/local/bin/stratum certs rotate-ca --cluster --yes` again, with the
same `--restart-workloads` or `--skip-workload-check` flag as the first run.
It skips the phases every node completed.

To finish the rotation by hand instead:

1. Fix the cause the halt names.
2. Run `stratum certs rotate-ca --phase=sign-new --yes` on each control plane,
   then on each worker. Add `--skip-workload-check` only for workloads you will
   not restart, such as Jobs. Unlike `--cluster`, this runs the
   [client check](#before-sign-new-phasesign-new) on each control plane and
   does not wait for the node to be Ready.
3. Confirm every node is Ready, `kubectl get csr` shows none pending and the
   local API server's `/healthz` responds.
4. Follow [Finishing a rotation by hand](#finishing-a-rotation-by-hand) from
   step 1. It checks every node's mounted tokens and kubelet serving
   certificates before the first `finalize`, finalizes the control planes and
   then the workers one at a time, and marks the rotation record complete.

To abandon the rotation, follow
[Abandoning a rotation](#abandoning-a-rotation).

This hand procedure is only for a halt after `trust-new` has completed on
every node. A halt while `trust-new` is still running on some node leaves nodes
on different trust. To resume that rotation, run
`sudo /usr/local/bin/stratum certs rotate-ca --cluster --yes` again from the control plane
that started the run, or from another control plane that `stratum certs check`
shows at `trust-new`. Do not resume from a control plane that has not run
`trust-new`: it stages a root of its own. A rotation started with `--force`
cannot be rolled back, and resumes only with `--force` (see
[Recovering a lost key](#recovering-a-lost-key-force)).

### Before `sign-new` (`--phase=sign-new`)

On a control plane, `rotate-ca --phase=sign-new` checks every API client before
it promotes the new CA, Stratum's own add-ons included. It refuses while any
running container of theirs started before, or in the same second as, the
write that gave the namespace's `kube-root-ca.crt` the new CA. That covers
regular, init, native sidecar and ephemeral containers, including those of
pods that are being deleted.

Two more cases count, because the kubelet may not have synced the new `ca.crt`
into the pod yet:

- For a pod created before that write, a container that started up to 2
  minutes after it.
- For the same 2 minutes, a container of such a pod that has not started yet
  (Pending, or waiting out a `CrashLoopBackOff`). It can still start with the
  old `ca.crt`.

```text
sign-new refused: 1 workload(s) use in-cluster API credentials and have not restarted since the new CA was published; from sign-new they fail with x509 errors:
  Deployment apipoller/apipoller: stratum kubectl -n apipoller rollout restart deployment/apipoller
  Restart them before sign-new, or pass --skip-workload-check to sign-new to proceed anyway
```

Run the listed commands, wait for the rollouts, then run the phase again. A
workload whose old pods are still terminating is listed with the grace period
they have left. Wait for them to go. The new CA stays staged, so you need to
roll nothing back.

A workload whose pods have not started yet is listed with the time the
2-minute margin ends. It needs no restart. Run the phase again after that time:

```text
  Deployment apipoller/apipoller: not settled on the new CA: apipoller-5f8c-k2 (not running yet; it can still start on the old CA until 2026-10-09T00:40:00Z)
```

The phase also refuses when:

- `kube-root-ca.crt` does not carry the new CA yet in some namespace. Finish
  `trust-new` on every node and give kube-controller-manager time to republish
  it.
- It cannot tell when a namespace's `kube-root-ca.crt` was republished, because
  the ConfigMap's `managedFields` record no kube-controller-manager write. Pass
  `--skip-workload-check` to proceed.
- That write time is more than 5 minutes ahead of this host's clock (`was
  written by kube-controller-manager at ..., more than 5m0s ahead of this host's
  clock`). Delete the namespace's `kube-root-ca.crt` so kube-controller-manager
  recreates it, restart the namespace's API clients, and run the phase again.
  Or pass `--skip-workload-check` to proceed.

Under `--cluster`, the run makes this check itself, once, after the restarts
and the wait for the old pods, and halts before `sign-new` if it fails. A
rollout can succeed and still leave containers on the old CA, and a workload
can appear after the first check. The check does not count the Jobs and bare
Pods the run already listed as not restarted. Each node's `sign-new` Job then
runs with `--skip-workload-check`, because the run has done the check, or was
told not to.

!!! note "Restart with a rollout, not in place"
    A container that restarts in place within about a minute of the republish
    can still read the old `ca.crt`, because the kubelet has not synced the
    volume yet. A rollout creates new pods, which mount the current ConfigMap.

### Before `finalize`: mounted tokens

`finalize` drops the old service-account signing key. After that, the API
server rejects every token the old key signed, with `401 Unauthorized`. Before
any node finalizes, `rotate-ca` checks that no pod still mounts such a token.

The check reads every file in each projected volume under
`/var/lib/stratum/kubelet/pods/`, because the pod spec chooses the path of the
token inside the volume. For each file that holds a token, it compares the key
ID (`kid`) in the token's header with the ID of the old key. It changes
nothing. A file or directory it cannot read fails the check.

With `--cluster`, after `sign-new` has completed on every node, the run:

1. Scans every node: the node it runs on directly, and every other node with a
   Job.
2. Restarts the kubelet once on each node that mounts an old-key token, then
   scans that node again. Pods keep running through a kubelet restart, and the
   restarted kubelet re-issues their tokens within seconds.
3. Scans again every minute until no node mounts an old-key token, for up to
   60 minutes. The kubelet refreshes a token the restart did not re-issue on
   its own schedule, within about 50 minutes. The 60 minutes include the scans
   themselves, so a scan still running at the limit ends the check.
4. Once every node is clean, waits 2 minutes, so that clients re-read their
   token files. client-go re-reads its token file once a minute.
5. Checks node health again: every node is Ready, the local API server is
   healthy, and no CSR is pending. The token check can restart kubelets and
   take up to an hour, so a node can go NotReady in the meantime. A failure
   halts the run before any node finalizes, with the same `finalize refused`
   message.

The 2-minute wait runs on every `--cluster` rotation, even when no node had an
old-key token. A run where one node had stale tokens prints:

```text
==> Phase 3/3: finalize preflight (no node mounts a token signed by the old service-account key)
restarting the kubelet: 4 mounted token(s) are signed by the old service-account key
  no node mounts a token signed by the old service-account key; waiting 2m0s for clients to re-read their token files
==> Phase 3/3: finalize (drop the old CA)
```

While a node is not clean, the run prints a `waiting for the kubelets to
re-issue tokens signed by the old service-account key` line each minute that
lists the node and its pods.

If a node still mounts an old-key token after 60 minutes, or a node cannot be
scanned, the run halts before any node finalizes. Every node still trusts both
CAs and the API server still accepts both keys, so nothing is broken:

```text
finalize refused (cluster left in the safe old‖new state): after 1h0m0s the old service-account key still could not be cleared for finalize: these nodes still mount them: <node>: <namespace>/<pod> (volume <volume>/<path>) (clients holding a token it signed would get 401)
  the run halted in sign-new. Re-run `stratum certs rotate-ca --cluster --yes` from this control plane, …
```

When a scan was still running at the limit, the list ends with
`the scan of <node> did not finish`. To continue, see
[Resuming a halted rotation](#resuming-a-halted-rotation), or
follow [Finishing a rotation by hand](#finishing-a-rotation-by-hand).

The run also warns about each `kubernetes.io/service-account-token` Secret that
holds an old-key token. These warnings do not stop the run:

```text
  WARN: Secret <namespace>/<name> holds a token signed by the old service-account key; it stops working at finalize. Delete the Secret and create it again to get a new one.
```

Each node's `finalize` Job runs with the internal `--tokens-checked` flag,
because the run has already checked every node.

With `--phase=finalize` on a control plane, `rotate-ca` checks the tokens on
that node only. It does not restart the kubelet and does not wait. If the node
mounts an old-key token, it refuses and lists the pods:

```text
finalize refused: 2 token(s) mounted on this node are signed by the old service-account key, which finalize drops (their clients would get 401): <namespace>/<pod> (volume <volume>/<path>), <namespace>/<pod> (volume <volume>/<path>). Restart this node's kubelet to re-issue them and run finalize again, or pass --skip-token-check
```

The check covers one node, and `finalize` on a worker does not check at all,
so check every node before you finalize the first one. See
[Finishing a rotation by hand](#finishing-a-rotation-by-hand).

A control plane that no longer has the old key
(`/var/lib/stratum/pki/sa.pre-carotate.pub`) skips the check only when its
`sa-bundle.pub` holds a single key or does not exist. Either means it has
already finalized or never rotated, so you can run `finalize` again after a
failed restart. If the old key is missing but the bundle still holds two keys,
or the bundle exists but cannot be read or parsed, `finalize` refuses, because
it cannot tell which tokens the old key signed. The `--skip-token-check` the
message suggests also needs `--force`. Pass both only if you accept the `401`
errors that follow (see [Skipping the token check](#skipping-the-token-check)).

### What the token check does not catch

- Clients that keep the token in memory and never re-read the file. Restart
  them after `sign-new` has completed on every control plane and before
  `finalize`, or they get `401` once it drops the old key. The 2-minute wait
  covers clients that re-read the file, as client-go does.
- Tokens from `kubernetes.io/service-account-token` Secrets, and kubeconfigs
  built from them. `--cluster` warns about the Secrets but does not wait for
  them. See [Restart workloads after `rotate-ca`](#restart-workloads-after-rotate-ca).

### Skipping the token check

| Flag | Applies to | Effect |
|---|---|---|
| `--skip-token-check` | `--cluster`, `--phase=finalize`; needs `--force` | Finalizes without checking the mounted tokens. Pods that still hold an old-key token get `401` once the old key is dropped. |

The flag is for compromise recovery, when you need the old key gone at once and
cannot wait for every token to be re-issued. Without `--force`, the command
refuses before it changes anything:

```text
stratum certs rotate-ca: --skip-token-check is for compromise recovery and goes with --force
```

The check also tells you to pass the flag when it cannot read the old key:

```text
cannot tell which tokens the old service-account key signed: <reason> (--skip-token-check finalizes without the check)
```

## Recovering a halted rotation

A halted `rotate-ca --cluster` run does not finish on its own. Its message
names the cause and the phase it halted in. This section covers resuming the
run, abandoning it, finishing it by hand, the refusals you meet when phases
run out of order, and a rotation after a lost key.

### Resuming a halted rotation

To resume the rotation, fix the cause and run
`sudo /usr/local/bin/stratum certs rotate-ca --cluster --yes` again. Pass the
`--restart-workloads` or `--skip-workload-check` the first run had, and
`--force` if it had that too. The retry resumes the same rotation. It skips
each phase that every node has completed, and prints
`Skipping globally completed phase <phase>` for it. A phase that stopped part
way resumes on the nodes that have not finished it.

You can run the retry from any control plane that still holds that rotation's
material. After a halt in `trust-new`, run it from a control plane that
`stratum certs check` shows at `trust-new` (see
[Between `trust-new` and `sign-new`](#between-trust-new-and-sign-new-cluster)).
The retry resumes even after some nodes have finalized. The run saved the old
service-account key ID, so it can scan the tokens again after those nodes have
dropped the old key.

The `kube-system/stratum-ca-rotation` ConfigMap holds the rotation's progress:
the run ID, the last phase every node completed and the old service-account
key ID. It holds no key material. When the rotation completes, the run marks
the record finished, so the next `rotate-ca --cluster` starts a new rotation
from any control plane. Do not delete the ConfigMap while a rotation is
unfinished, because the retry needs it to resume.

If the run cannot mark the record finished, it halts with
`every node finalized: the rotation is done, but its shared record could not be marked complete`.
Do not roll back any node. Every one has finalized, and its old CA is gone.
Run `sudo /usr/local/bin/stratum certs rotate-ca --cluster --yes` again. It records the
completion, rotates nothing, and exits `1` with
`had already finished on every node`. That exit does not mean the rotation
failed. It says this run rotated nothing. Or mark the record complete by hand:

```bash
kubectl -n kube-system patch configmap stratum-ca-rotation --type merge -p '{"data":{"complete":"true"}}'
```

### Abandoning a rotation

You can abandon a rotation only before any node has finalized, and only if it
was not started with `--force`. After that, finish it forward.

1. From a control plane, check that every node can roll back. The check changes
   nothing:

    ```bash
    sudo /usr/local/bin/stratum certs rotate-ca --rollback --cluster --check
    ```

2. When the check passes, run `sudo /usr/local/bin/stratum certs rotate-ca --rollback --yes`
   on every control plane, then on every worker.
3. Once every rollback has succeeded, delete the rotation record:

    ```bash
    kubectl -n kube-system delete configmap stratum-ca-rotation --ignore-not-found
    ```

Until you delete the record, `stratum token create --control-plane`,
`stratum certs rotate --rekey-shared`, `stratum certs migrate-etcd-ca` and
`stratum certs migrate-kubelet-client-ca` refuse, and a new rotation may
refuse.

The check refuses in these cases:

- A control plane lost an old key during the rotation. It can only go forward,
  so if the others rolled back, the cluster would be split across two roots.
- The control planes would not roll back to the same root, as after a
  `finalize` on some of them.
- A worker's `sign-new` renewed the kubelet's client certificate under the new
  CA, which control planes back on the old root would not trust.
- The rollback could not read a node's trust files.

When the check refuses, finish the rotation forward instead, with these
exceptions:

- If it names workers that do not trust the new CA, run
  `--phase=trust-new --yes` on each of them, and run the check again before any
  node runs `sign-new`.
- If it names a control plane that does not trust the new CA, no command can
  give that node the staged CA once the run has ended. Restore the lost key from
  a backup, run the check again, and roll back every node when it passes.
- If only workers refuse while a control plane does not trust the new CA, roll
  back every other node. Then remove each refusing worker
  (`stratum uninstall`) and join it again.

`stratum certs check` on a node shows the same answer for that node, as its
`rollback:` line, with the active signer of each of the three pairs.

### Finishing a rotation by hand

Use this procedure when `--cluster` halted with `finalize refused (cluster left
in the safe old‖new state)`, or once you have run `sign-new` by hand on every
node after a halt before or in `sign-new`. At that point every node has
completed `sign-new` and no node has finalized. `--phase=finalize` checks only
its own control plane and does not wait, so you do the cluster-wide check
yourself.

!!! note "Not run end to end"
    This procedure has not been run end to end on a cluster. Its commands match
    what `--cluster` runs on each node.

1. On a control plane, compute the ID of the old service-account key. It is the
   same on every control plane:

    ```bash
    OLD_KID=$(sudo openssl pkey -pubin -in /var/lib/stratum/pki/sa.pre-carotate.pub -outform DER \
      | openssl dgst -sha256 -binary | openssl base64 -A | tr '+/' '-_' | tr -d '=')
    echo "$OLD_KID"
    ```

2. On every node, control planes and workers, scan the mounted tokens. Pass the
   same `OLD_KID` everywhere, because workers do not hold the old key:

    ```bash
    sudo /usr/local/bin/stratum certs rotate-ca --scan-tokens --old-kid "$OLD_KID" --restart-kubelet --yes
    ```

    `--restart-kubelet` restarts the node's kubelet once if it finds old-key
    tokens, then scans again. The command exits `0` when the node is clean. It
    exits `10` when it still finds old-key tokens, and prints one
    `stale-token <pod-uid> <volume>/<path>` line for each, for example
    `stale-token 3f2c…9a1e kube-api-access-x7k2p/token`. To name a pod from its
    UID:

    ```bash
    kubectl get pods -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,UID:.metadata.uid | grep <pod-uid>
    ```

    Delete or restart those pods, or wait for the kubelet to refresh their
    tokens. To scan again, run the command without `--restart-kubelet`.

3. Repeat step 2 until every node exits `0` in the same pass.
4. Wait 2 minutes, so that clients re-read their token files.
5. Check that the cluster is still healthy on the new CA. The run checks the
   same things before it finalizes. Check that every node is Ready, that no CSR
   is Pending, and that `/healthz` answers `ok` on each control plane:

    ```bash
    kubectl get nodes
    kubectl get csr
    kubectl get --raw /healthz
    ```

    If a check fails, fix that first. It is also what the run halts on when its
    message names a node that is not Ready or a pending CSR instead of tokens.

6. Check that every kubelet serves a certificate the new CA signed. The checks
   in step 5 all pass while a kubelet still serves an old-CA certificate. Like
   the run, this checks that each certificate is signed by the new CA and names
   the node's IP. Run it on a control plane, where `sign-new` has made `ca.crt`
   the new CA:

    ```bash
    for ip in $(kubectl get nodes -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}'); do
      case $ip in *:*) addr="[$ip]:10250" ;; *) addr="$ip:10250" ;; esac
      echo | openssl s_client -connect "$addr" -CAfile /var/lib/stratum/pki/ca.crt -verify_ip "$ip" -verify_return_error >/dev/null 2>&1 && echo "$ip ok" || echo "$ip not verified against the new CA"
    done
    ```

    On a cluster whose profile bootstraps control-plane serving certificates,
    skip the control planes. Their kubelets renew their serving certificates in
    their own `finalize`. A node that cannot be reached also prints
    "not verified". Make its kubelet reachable and check again. For a worker
    that is not on the new CA, run `--phase=sign-new` on it again. For a control
    plane, restart Stratum there (`sudo systemctl restart stratum`). If you
    finalize while a kubelet serves an old-CA certificate, `kubectl logs` and
    `exec` to that node break.

7. Finalize the control planes one at a time, starting with the one that ran
   `--cluster`. Wait for each to finish before you start the next:

    ```bash
    sudo /usr/local/bin/stratum certs rotate-ca --phase=finalize --yes
    ```

    Each control plane checks its own tokens again and refuses if it finds one.
    If that happens, go back to step 2 and do not finalize the other nodes yet.

8. Finalize the workers the same way, one at a time.
9. Run `stratum certs check` on every node to confirm that each one is on the
   new CA.
10. On a control plane, mark the rotation record complete. Until you do,
    `stratum token create --control-plane`, `stratum certs rotate --rekey-shared`
    and both `stratum certs migrate-…` commands refuse, and the next
    `rotate-ca --cluster` resumes this rotation instead of starting a new one:

    ```bash
    kubectl -n kube-system patch configmap stratum-ca-rotation --type merge -p '{"data":{"complete":"true"}}'
    ```

    A rotation that no `--cluster` run started has no record. Skip this step.

After the first control plane finalizes, do not roll back a node that has
finalized, because its old CA is gone. Until then, you can still
[abandon the rotation](#abandoning-a-rotation), unless it was started with
`--force`.

### Phases run out of order

Each phase checks that this node completed the phase before it. A refused phase
changes nothing on the node, so you can fix the order and run it again. Every
refusal offers two ways out: run the missing phase on this node, or
[abandon the rotation](#abandoning-a-rotation) (not a rotation started with
`--force`). Run `stratum certs check` to see where the node stands.

| Node | Phase refused | It refuses when | To continue |
|------|---------------|-----------------|-------------|
| Control plane | `finalize` | New CA material is still staged, even a lone `ca-new.crt` left by an interrupted `trust-new`, or this node's `sign-new` never promoted a new CA | Run `--phase=sign-new` on this node, then `finalize`. If `sign-new` ran but its restart failed, run it again once the cause is fixed. If `sign-new` is refused for want of complete staged material, use `--rollback` (not after `--force`) |
| Worker | `sign-new` | The worker's `ca-bundle.crt`, `kubelet-client-ca-bundle.crt`, `kubelet.kubeconfig` or `bootstrap-kubeconfig` holds fewer than two distinct CAs, so its `trust-new` has not run or stopped part way | Run `--phase=trust-new` on this node again, then `sign-new` |
| Worker | `finalize` | The CA that `finalize` drops still signs the kubelet client certificate. A worker with no CA left to drop is not checked, so you can run a finished `finalize` again | Run `--phase=sign-new` on this node, then `finalize` |
| Worker | `sign-new` or `finalize` | The kubelet has no usable client certificate. `sign-new` renews with the current certificate, so it cannot repair this | Remove the worker with `stratum uninstall` and join it again with a token from `stratum token create` |
| Worker | `sign-new` | The kubelet has no serving certificate, or its renewal CSR under the new CA stays pending or is denied | Check that the operator is running (`spec.operator: false` turns it off) and why `stratum kubectl get csr` shows the request unapproved. A CSR for a subject alternative name (SAN) the node no longer reports among its addresses stays pending. Approve the CSR by hand if you trust it, then run `--phase=sign-new` on this node again |
| Worker | `finalize` | The kubelet serving certificate is missing, unreadable, or still signed by the CA `finalize` drops | Run `--phase=sign-new` on this node again, then `finalize` |
| Control plane (`--cluster`) | `finalize` | A worker's kubelet serves a certificate the new CA did not sign, or so does the kubelet of a control plane whose profile does not bootstrap its serving certificate, or that kubelet cannot be checked at its InternalIP | For a worker, run `--phase=sign-new` on it again. For a control plane, make its kubelet reachable and restart Stratum there (`sudo systemctl restart stratum`) so the kubelet serves its new-CA certificate. Then run `--cluster` again |

The control-plane refusal reads:

```text
certs: finalize refused: this node's sign-new never promoted a new CA, so finalize would collapse trust to the current CA and discard the rotation — run --phase=sign-new first, or `stratum certs rotate-ca --rollback --yes` to abandon it (see `stratum certs check`)
```

A control plane records its progress in two files in `/var/lib/stratum/pki/`.
Do not edit them by hand:

| File | Written | Removed |
|---|---|---|
| `carotate-promoted` | when `sign-new` has promoted the new CA and service-account key, reissued the leaves and restarted the control plane | by `finalize`, a new `trust-new`, or a rollback |
| `carotate-finalized` | by `finalize` | by a new `trust-new` or a rollback |

A control plane passes `finalize` only with `carotate-promoted` and nothing
staged left, or with `carotate-finalized`, so you can re-run a `finalize` whose
restart failed. A control plane that never rotated has neither file, so
`finalize` is refused there too. `finalize`, `rollback` and `trust-new` fail if
they cannot delete a rotation file.

If the control-plane restart at the end of `sign-new` fails, the node is
refused at `finalize`, because a static pod may still serve a certificate the
old CA signed. Run `sign-new` again there once the cause is fixed. Its staged
material is already used, so it skips the promotion, reissues the leaves,
restarts the control plane and writes the file. `sign-new` also checks that
all the staged files are present before it promotes anything, and refuses,
changing nothing, if one is missing.

!!! warning "Upgrading a control plane in the middle of a rotation blocks `finalize`"
    A control plane that ran `sign-new` under an earlier Stratum release has no
    `carotate-promoted` file. If you upgrade it before `finalize`, `finalize` is
    refused. [Abandon the rotation](#abandoning-a-rotation) on every node and
    rotate again.
    A control plane that finalized under an earlier release has no
    `carotate-finalized` file. A `finalize` that stopped at the restart cannot
    be re-run after the upgrade either, though its files are already written and
    only the restart is outstanding.

### `trust-new` on a control plane that already promoted

Once a control plane's `sign-new` has promoted the new CA, the backups in
`/var/lib/stratum/pki/` (`ca.pre-carotate.crt` and the rest) are the only way
back to the old CA. A second staging would clear the `carotate-promoted`
record, and the next `sign-new` would overwrite those backups with the new CA.
Stratum guards against this in two places.

Normally, `trust-new` sees that this node already applied it, with or without
`carotate-promoted`, and skips the phase. It writes nothing and does not
refuse:

```text
trust-new is already applied on this node — skipping (the rotation resumes at sign-new)
```

The phase runs only when new CA material is staged on the node again, for
example after a `--fetch-shared` on a follower control plane. If the node has
`carotate-promoted` and has not finalized, `trust-new` then refuses before it
fetches or writes anything. Any program that stages a rotation through
Stratum's `pki` package gets the same refusal.

#### A promoted node with new CA material staged again

You can recognize this node by three things in `/var/lib/stratum/pki/`:
`carotate-promoted`, `ca.pre-carotate.crt`, and `ca-new.crt` with `ca-new.key`.
The refusal reads:

```text
pki: refusing to stage a CA rotation (nothing has been changed): this node has already promoted its new CA, and staging again would replace the staged material and clear the promotion record, so a later sign-new would overwrite the ca.pre-carotate.* backups with the new CA and a rollback could no longer restore the original. New CA material is staged on this node again, so finalize would refuse and sign-new would skip: do not finalize this node. Abandon the rotation with the rollback below: run `stratum certs rotate-ca --rollback --cluster --check` from a control plane, then `stratum certs rotate-ca --rollback --yes` on every control plane and then every worker
```

This node cannot finish the rotation forward. The only way out is the
rollback, which restores the original CA from the backups the refusal
protects:

1. Do not run `--phase=sign-new`, `--phase=trust-new --fetch-shared`, or any
   later phase on this node. `sign-new` skips it as applied, and the fetch is
   refused.
2. From a control plane, check that every node can roll back. The check
   changes nothing:

    ```console
    # /usr/local/bin/stratum certs rotate-ca --rollback --cluster --check
    ```

3. If the check passes, run `sudo /usr/local/bin/stratum certs rotate-ca --rollback --yes`
   on every control plane, then on every worker.
4. Delete the rotation record:

    ```console
    $ kubectl -n kube-system delete configmap stratum-ca-rotation --ignore-not-found
    ```

5. If you still want the new CA, start again with
   `sudo /usr/local/bin/stratum certs rotate-ca --cluster --yes`.

If the check refuses, for example because another node has already finalized,
do not roll back. No tested route takes this node forward. Stop and escalate.

A node whose `sign-new` promoted the new CA but stopped before it wrote
`carotate-promoted` is not limited to the rollback. Its staged files are the
rest of that promotion, and the refusal reads:

```text
pki: refusing to stage a CA rotation (nothing has been changed): this node has already promoted its new CA, and staging again would replace the staged material and clear the promotion record, so a later sign-new would overwrite the ca.pre-carotate.* backups with the new CA and a rollback could no longer restore the original. This node's sign-new stopped before it recorded the promotion: run `stratum certs rotate-ca --phase=sign-new --yes` on it again to finish, and finalize only after that. Or abandon the rotation with the rollback below: run `stratum certs rotate-ca --rollback --cluster --check` from a control plane, then `stratum certs rotate-ca --rollback --yes` on every control plane and then every worker
```

Run `sudo /usr/local/bin/stratum certs rotate-ca --phase=sign-new --yes` on
that node again, and run `finalize` only after it succeeds. Or abandon the
rotation with steps 2 to 5 above.

### Backups left by an earlier rotation

`finalize` and `rollback` delete the `.pre-carotate` backups when they finish.
Earlier releases ignored a failure to delete them, so the backups of a finished
rotation could stay behind. A rollback of the next rotation would then restore
the CA from before the finished rotation, not the one the node was using, and
peers would reject the node.

On a control plane, `trust-new` now removes these backups before it fetches or
stages anything, when no rotation is active on the node: nothing is staged,
`ca-bundle.crt` holds one CA, and no promoted rotation is waiting for
`finalize`. It removes only a backup that differs from the file it backs up,
and only when every backup does. If any backup equals its live file, the node
is a rollback that restored the CA but stopped before its cleanup. A retried
rollback needs all of its backups, so `trust-new` keeps every one of them and
carries on.

If a live file is missing or cannot be read, `trust-new` cannot tell which
backups are obsolete. It removes none of them and refuses:

```text
pki: cannot tell which backups of an earlier CA rotation are obsolete, so none were removed (nothing has been changed): <error>
```

Repair or restore that file, then run `trust-new` again. With `--force`, a
missing live file does not refuse: the cleanup keeps every backup, because a
backup may be the only copy of a key that was lost.

!!! warning "Nodes staged by an earlier release are not cleaned"
    If an earlier release ran `trust-new` on a control plane that still had
    such backups, and you upgraded before `sign-new`, Stratum cannot tell those
    backups from a promotion whose staging was repeated, so it leaves them. On
    that node, `/var/lib/stratum/pki/` holds both `ca-new.crt` and
    `ca.pre-carotate.crt` but no `carotate-promoted`. Do not roll it back
    before `sign-new`: the rollback would restore the stale backups' CA.
    Finish the rotation forward instead. `sign-new` overwrites the backups with
    the CA the node is using, and from then on a rollback is safe.

    Before `sign-new`, every node must trust the new CA, and Stratum has no
    check that confirms it. Neither a passing `--rollback --cluster --check`
    nor the `trusts-new=` field that `--rollback --check` prints shows that a
    node trusts this rotation's new CA. Do not run `--phase=trust-new` on a
    control plane either: with nothing staged, a control plane mints its own
    new CA, different from the rest of the cluster's. Instead:

    - If the rotation record is unfinished and a control plane still holds
      the staged material, resume the rotation from that control plane. Follow
      [Resuming a halted rotation](#resuming-a-halted-rotation).
    - If the run has ended, or you cannot confirm that every node trusts the
      new CA, stop and escalate for cluster-specific recovery. Do not follow
      the generic abandonment procedure on this stale-backup node, roll it
      back, or run `sign-new`. Preserve its staged files and backups.

### Recovering a lost key (`--force`)

If a control plane has lost the root CA's private key (`ca.key` under
`/var/lib/stratum/pki`), the front-proxy CA's key, or the service-account key,
`rotate-ca` refuses unless you pass `--force`. With `--force` the rotation goes
ahead without them and ends on a new root whose key is on every control plane:

```bash
sudo /usr/local/bin/stratum certs rotate-ca --cluster --force --yes
```

Workloads are handled as in any rotation (see
[Restart workloads after `rotate-ca`](#restart-workloads-after-rotate-ca)).
You do not need to remove or rejoin any node:

- The old root's certificate (`ca.crt`) stays in every trust bundle until
  `finalize`, as in any rotation. Every node and client, workers included,
  verifies the new root by it. Only the key is lost. The certificate is public.
- From `trust-new` until `sign-new` promotes the new root, the staged root
  (`ca-new.*`) signs in place of the lost key. It signs for the
  controller-manager's certificate signer and the operator's webhook
  certificate. The staged service-account key does the same for a lost
  `sa.key`.
- `--cluster` passes `--force` to the other control planes' `trust-new`, so a
  control plane that lost the same keys goes ahead too.

The keys are the same on every control plane. If only one control plane lost
them, you can instead run a plain rotation (without `--force`) from a control
plane that still holds them.

!!! warning "A rotation started with `--force` cannot be rolled back"
    A control plane that lost a key has no old signer to return to. Rolling back
    the others would leave them trusting only the old root while it can only go
    forward. `rotate-ca --rollback` refuses on every node that ran its
    `trust-new`, and changes nothing:

    ```text
    pki: refusing to roll back (nothing has been changed): this rotation was started with --force: ...
    ```

If the run halts, finish it forward. Fix the cause and run
`sudo /usr/local/bin/stratum certs rotate-ca --cluster --force --yes` again from the control
plane that started it, with the `--restart-workloads` or
`--skip-workload-check` the first run had. The retry skips the phases every
node completed, and resumes an unfinished one with the material already staged
(see [Resuming a halted rotation](#resuming-a-halted-rotation)).

A halt in `sign-new` also prints the way to finish by hand:

1. Run `sudo /usr/local/bin/stratum certs check` on every node, and fix the cause the halt
   names.
2. Run `sudo /usr/local/bin/stratum certs rotate-ca --phase=sign-new --yes` on each control
   plane, then on each worker, where `sign-new` has not completed. A control
   plane whose `sign-new` promoted and then stopped finishes where it stopped.
3. Follow [Finishing a rotation by hand](#finishing-a-rotation-by-hand).

The old root's certificate is required, even with `--force`. Without it nothing
could verify the new root, and every component would fail TLS once `trust-new`
restarts it, so `rotate-ca` refuses before it changes anything. It also refuses
a `ca.crt` that is not one of the certificates in `ca-bundle.crt`, such as a
file copied from another cluster:

```text
certs: refusing to rotate (nothing has been changed): /var/lib/stratum/pki/ca.crt is missing, or is not a certificate in ca-bundle.crt: cannot rotate the CA without the certificate of the root the cluster trusts: ...
```

Put the certificate back and run the rotation again. Copy `ca.crt` from another
control plane, or, if this node's `ca-bundle.crt` holds a single certificate,
copy that file to `ca.crt`.

The same holds for the other public halves:

- Stratum rebuilds a lost `sa.pub` from `sa.key` when that key is still there.
- Without `front-proxy-ca.crt` (or with one that `front-proxy-ca-bundle.crt`
  does not carry), or without both `sa.pub` and `sa.key`, `trust-new` refuses
  before it changes anything (`cannot rotate the CA without the old front-proxy
  CA certificate and service-account public key: ...`). Copy the file from
  another control plane, or copy a single-entry `front-proxy-ca-bundle.crt` or
  `sa-bundle.pub` to it.
- On a cluster whose kubelets have their own client CA (see
  [The kubelet's client trust](hardening.md#the-kubelets-client-trust)),
  `kubelet-client-ca.crt` is a fourth public half. Without it, or with one that
  `kubelet-client-ca-bundle.crt` does not carry, `trust-new` refuses the same
  way. A lost `kubelet-client-ca.key` needs `--force` like the other keys.

A worker that fetches a bundle sharing no CA with its own refuses it in every
phase. No rotation produces such a bundle:

```text
refusing to apply the fetched trust bundle: it shares no certificate with this node's on-disk bundle (...), which is a wholesale trust pivot rather than a rotation: every rotation, --force included, keeps the old root in the bundle until finalize, so do not trust this bundle. If the cluster's control planes were rebuilt on a new root, remove this node (`sudo /usr/local/bin/stratum uninstall --yes`) and join it again with a token from `stratum token create` on a control plane
```

## The `stratum-carotate` key Secret

While `rotate-ca --cluster` runs, it stages the encryption key (AES) that decrypts the shared
CA material (`ca.key`, `sa.key`, `front-proxy-ca.key`) in the
`kube-system/stratum-carotate` Secret. Anyone with `get secrets` in
`kube-system` can read it, so it must exist only while a rotation is in flight.

The run deletes the Secret when it ends, whether it completes or halts, with
one exception. If it halts in `trust-new` before every other node has finished
that phase, a node may still need the key to fetch the staged material. The
run then keeps the Secret and says so
(`the staged key Secret ... is left in place`). A retry stages its own key in
that Secret, under the same run ID, and deletes it the same way.

Before it deletes the Secret, the run waits up to 60 seconds for the API server
to pass its health check, so the delete does not land inside the API server
restart. If the API server is still down after that, the run tries the delete
anyway. If the delete fails, the run prints a `WARN` with the manual command.

On a control plane, a successful `rotate-ca --phase finalize` or
`rotate-ca --rollback` also removes a `stratum-carotate` left behind by an
earlier failed run. It does so only if the Secret holds the key of the
`rotate-ca --cluster` run whose `sign-new` phase this node completed. The node
records that run when it promotes the new signer, and keeps the record until
the key is known to be gone, so a finalize or rollback you retry after a
failure still removes the key. If the Secret is already gone, they print
nothing.

Three markers tie the key to its run:

| Marker | What it holds |
|---|---|
| `stratum.ciq.com/carotate-run` annotation on `stratum-carotate` | A random ID for the `rotate-ca --cluster` or `migrate-kubelet-client-ca --cluster` run that staged it. On a key that `migrate-etcd-ca` staged, a one-way digest of that migration's certificate key, never the key itself |
| `stratum.ciq.com/carotate-operation` annotation on `stratum-carotate` | The command that staged it: `rotate-ca`, `migrate-etcd-ca` or `migrate-kubelet-client-ca`. None of them overwrites a key, or the encrypted material, that another staged |
| `/var/lib/stratum/pki/carotate-run-id`, mode `0600`, on each control plane | The ID of the run whose `sign-new` the node completed. Stratum removes it once that run's key is gone. Do not edit it by hand |

`migrate-etcd-ca` stages the same Secret, and its followers read it while the
migration runs. Finalize and rollback therefore leave the Secret in place and
print a `WARN` when the `kube-system/stratum-etcdca-migration-lock` Secret
exists, or when they cannot read it. The warning names the lock's holder and
start time. If that migration run was killed and is not coming back, remove the
lock first, then the key:

```bash
kubectl -n kube-system delete secret stratum-etcdca-migration-lock stratum-carotate
```

Finalize and rollback also print a `WARN` and leave the Secret in place in
three other cases:

- They cannot read `stratum-carotate`. The warning prints the manual delete
  command.
- The Secret is not the key of this node's run. This covers a node with no
  `carotate-run-id` record (for example, a rollback from the `trust-new`
  phase), a key from a later rotation or from `migrate-etcd-ca`, and a rotation
  driven by manual `--phase` commands. Another rotation or migration may still
  be fetching the key, or a halted one may need it to resume. A running
  `--cluster` rotation or migration holds the `stratum-cluster-maintenance` and
  `stratum-cluster-upgrade` Leases in `kube-system`, and
  `kubectl -n kube-system get lease` shows their holders. Once no rotation or
  migration is running or needs the key to resume, delete it by hand with the
  command the warning prints.
- A newer run replaces the Secret while they are cleaning up. The Secret
  belongs to that run, and you have nothing to remove.

`certs check` warns when it finds the Secret while this node reports no CA
rotation in progress:

```text
WARN: kube-system/stratum-carotate exists while no CA rotation is in progress on this node.
      It holds the AES key that decrypts the shared cluster CA material (ca.key, sa.key,
      front-proxy-ca.key). If no rotate-ca or migrate-etcd-ca run is active, remove it:

        kubectl -n kube-system delete secret stratum-carotate
```

The check knows only this node's rotation phase. Before you delete the Secret
by hand, confirm that no `rotate-ca` or `migrate-etcd-ca` run is active on any
control plane. If `stratum-etcdca-migration-lock` exists, delete the lock first,
as above.

## Migrating an existing cluster onto the etcd CA

Every released Stratum issues etcd's certificates from a dedicated etcd CA,
separate from the Kubernetes root (see [Hardening](hardening.md)). A cluster
built by a released Stratum needs nothing here. `certs migrate-etcd-ca` is for
a cluster built from a development checkout older than the etcd CA.

```bash
sudo /usr/local/bin/stratum certs migrate-etcd-ca --cluster --yes
```

It runs three phases across every control plane, and completes each one
everywhere before it starts the next:

| Phase | What it does |
|---|---|
| `trust-both` | etcd trusts the etcd CA and the cluster root. Every certificate still works |
| `issue-new` | Reissues etcd's certificates under the etcd CA |
| `drop-root` | etcd stops trusting the cluster root. This is the phase that separates etcd's trust from the Kubernetes root |

etcd's trust widens before any certificate moves, and narrows only after every
certificate has moved, so no member presents a certificate its peers do not
accept. `drop-root` starts only once the command has confirmed, against each
member's running etcd, that every control plane completed `issue-new`. Workers
hold no etcd certificate, so the command touches only control planes.

If the control planes are not all at the same phase, the command stops and
prints the phase each one runs. Read that table, then run the command again
with `--resume`. The command reports a node whose etcd it cannot reach as
`unknown`, and `--resume` does not override that.

If a run stops part way, the cluster stays in the last phase every node
completed, which is a working state. Fix the node and run the command again
with `--resume`, or go back with `--rollback`:

```bash
sudo /usr/local/bin/stratum certs migrate-etcd-ca --cluster --rollback --yes
```

!!! warning "Rolling back after `drop-root` makes etcd trust the cluster root again"
    On a cluster that has finished `drop-root`, a rollback lets any certificate
    the cluster root signs reach etcd again, so it also needs `--force`:

    ```bash
    sudo /usr/local/bin/stratum certs migrate-etcd-ca --cluster --rollback --force --yes
    ```

That form runs in two passes. It re-adds the cluster root to every control
plane's etcd trust first, and only then reissues the certificates. Do not undo
the migration one node at a time. A node whose certificate its peers do not yet
accept leaves the etcd cluster at once.

## Migrating an existing cluster onto the kubelet client CA

A cluster created by this release has its kubelets trust only
`kubelet-client-ca`, which signs the API servers' kubelet client certificate
and nothing else (see [Hardening](hardening.md#the-kubelets-client-trust)). A
cluster created by an earlier release has its kubelets trust the cluster root.
Any certificate the root signs, `admin` and every node's kubelet client
included, can then read logs, run commands and forward ports on any node.
`certs migrate-kubelet-client-ca` moves such a cluster onto its own CA. Run it
from a control plane once every node runs this release:

```bash
sudo /usr/local/bin/stratum certs migrate-kubelet-client-ca --cluster --yes
```

It runs three phases. Each runs on this control plane, then the other control
planes, then the workers, before the next one starts:

| Phase | What it does |
|---|---|
| `trust-both` | Every kubelet trusts `kubelet-client-ca` and the cluster root. This control plane creates the CA, the others fetch it, and workers read it from the `stratum-kubelet-client-ca` ConfigMap |
| `issue-new` | Every control plane reissues `apiserver-kubelet-client` under `kubelet-client-ca` and restarts its API server |
| `drop-root` | Every kubelet stops trusting the cluster root. This is the phase that stops root-signed certificates from reaching the kubelets |

The kubelets trust the new CA before any certificate moves, and stop trusting
the root only after every API server's certificate has moved, so `kubectl logs`
and `exec` keep working throughout. Each phase restarts the kubelets or the API
servers it changes. Running pods are not restarted. Before `drop-root`, the
command checks that every kubelet accepts the API servers' new certificate.
Afterwards, it checks that every kubelet refuses a certificate the cluster root
signed.

Do not join nodes or run `certs rotate-ca` while the migration runs. Stratum
refuses a CA rotation on a node part way through the migration until the
migration finishes or is rolled back.

If a run stops part way, every kubelet still accepts the API servers. Fix the
cause and run the same command again. Each node skips the phases it has already
completed. To go back to the cluster root instead:

```bash
sudo /usr/local/bin/stratum certs migrate-kubelet-client-ca --cluster --rollback --yes
```

The rollback runs the phases in reverse. Every kubelet trusts the cluster root
again, then every control plane reissues `apiserver-kubelet-client` under it,
then every node drops `kubelet-client-ca`. On a cluster where any node has
completed `drop-root`, the rollback lets root-signed certificates reach the
kubelets again, so it also needs `--force`.

### A cluster parked part way

A run that halted, or was stopped, leaves its nodes in one of the migration
states. Until `drop-root` completes on a node, its kubelet still accepts
certificates the cluster root signs.

| State | Meaning |
|---|---|
| `not-migrated` | The kubelet trusts the cluster root alone. The run has not reached this node, or a rollback finished on it |
| `trust-both` | The kubelet trusts `kubelet-client-ca` and the cluster root |
| `issue-new` | As `trust-both`, and this control plane's `apiserver-kubelet-client` is already signed by `kubelet-client-ca` |
| `migrated` | The kubelet trusts `kubelet-client-ca` alone |

While any node is in `trust-both` or `issue-new`, `certs rotate-ca` refuses to
start on it. Finish the migration by running the same `--cluster` command
again, or go back with `--rollback`.

Run `--cluster` again rather than `--phase` by hand. `--phase` runs one phase
on this node only. A control plane other than the one that created
`kubelet-client-ca` can fetch the CA only while a `--cluster` run has it staged.
