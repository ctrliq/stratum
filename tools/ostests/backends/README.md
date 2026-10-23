# ostests provisioning backends

`provision.sh` never speaks to a cloud API. It sources one of these and calls five
functions, so the install/join/verify logic is written once and runs anywhere.

The point is the dev loop. A five-node SSH install driver is not iterable at thirty
minutes per `pulumi up`, and nobody should need cloud credentials to work on the shape of
the script. `local` runs the same code path against containers for free; `gcp` is what CI
runs. GCP is the only cloud backend; any other `PROVISION_BACKEND` value fails in
`provision.sh` before anything is provisioned.

| backend | Pulumi program | CI authenticates with | state |
| --- | --- | --- | --- |
| `local` | — (containers) | — | — |
| `gcp` | `tools/ostests/gcp/Pulumi.yaml` | Workload Identity Federation | `gs://` |

The SSH half of the contract lives in `ssh-lib.sh`: `backend_ready`, `backend_ssh`,
`backend_scp` and `backend_tunnel`, plus the state-URL check and the `pulumi up` /
stack-output plumbing, so `gcp.sh` is only its arch→machine/image mapping and its
`backend_up`. The program emits four outputs (`controllers`, `controllersPrivate`,
`workers`, `workersPrivate`), paired by index.

## Why GCP

**Because it is the only provider in reach that covers both architectures.**
`release-publish.yml` loops `for arch in amd64 arm64` and puts a final release back into
draft unless it carries a conformance-results tarball for each — and the evidence must be native;
emulated runs do not count. GCP has E2/N2 for amd64 and T2A/C4A for arm64. An x86-only provider — Vultr,
for instance — would need an entire second backend for the arm64 leg alone.

The arm64 leg is also the one with prior art: a 3-CP + worker arm64 HA cluster has
been live-validated on T2A, Rocky 9 aarch64, every node reporting `architecture=arm64`.

**What GCP costs us:** no L2 ARP for instances and no instance-level BGP, so kube-vip
cannot be exercised here. VIP failover is tested on a lab with real layer-2
networking, where ARP actually works. Vultr's BGP + Reserved IPs could in principle
have tested the shipped VIP path, but that needs a manual approval request against a
shared account, so it was a maybe traded for a certainty.

## The contract

Select with `PROVISION_BACKEND` (default `gcp`).

| function | contract |
| --- | --- |
| `backend_up` | Provision the nodes. Emit one line per node on **stdout**: `<role> <handle> <node_ip>`, `role` ∈ `controller`\|`worker`. Everything else goes to stderr. |
| `backend_ready HANDLE` | Block until the node accepts commands. Non-zero on timeout. |
| `backend_ssh HANDLE CMD…` | Run `CMD…` on the node as root. Exit status and output are the command's. |
| `backend_scp SRC HANDLE DST` | Copy a local file to the node. |
| `backend_ssh_stdin HANDLE COMMAND` | Run a remote command with stdin forwarded (for secret-file transfers). |
| `backend_tunnel HANDLE PORT` | Make the node's apiserver reachable at `127.0.0.1:PORT` on this machine. Print the PID of the forwarding process on stdout **if one was started**, or nothing if the backend needs no process (local publishes the port at container create; the caller kills a PID only when given one). Non-zero if PORT cannot be served. |

### handle vs node_ip

`handle` is **opaque to `provision.sh`** — a public IP on GCP, a container name locally. It
is only ever passed back to the five functions above.

`node_ip` is the address **other cluster members use**: the join URL, `--node-ip`, cert SANs.
On GCP that is the private IP, because the firewall permits intra-cluster traffic by network
tag and a private address is in the apiserver cert SANs (`internal/pki/pki.go:239`) while the
public one is not — a join pointed at a public IP fails TLS verification.

### State

Pulumi requires an explicit state backend URL. Local file
state would work for a cluster created and destroyed inside one job, but a runner that
dies mid-job takes the state with it and orphans everything — which leans on the
sweeper, which has not yet been verified against live resources.

The job holds GCP credentials only, so `PULUMI_BACKEND_URL` must be a `gs://` bucket, and in
CI the backend refuses any other scheme (`ssh-lib.sh`, `_require_state_backend`): a
mismatch fails opaquely inside pulumi's first backend call, under credentials that cannot
reach it. The workflows set it from the `OSTESTS_PULUMI_BACKEND_URL` repository variable
in their "provider settings" step; the passphrase is the `OSTESTS_PULUMI_PASSPHRASE`
secret.

### Quota (the GCP project is shared)

Each cluster is 5 instances × 4 vCPUs, 5 ephemeral external IPs, and 5 × 40 GB of boot
disk. Three jobs provision them, and **they can overlap** — an `-rc` tag cut during the
Sunday window runs alongside the weekly:

| workflow | job | concurrent clusters | notes |
| --- | --- | --- | --- |
| weekly | `ostests` | 2 | `max-parallel: 2` over branch × arch (3 branches × 2 arches) |
| release | `conformance-test` | 2 | **no `max-parallel`**; one amd64 + one arm64 |
| weekly | `skew-extremes` | 1 | gated separately on `OSTESTS_WEEKLY_ENABLED`, currently off |

With skew off, the worst realistic overlap on GCP is weekly + release = **20 instances /
80 vCPUs / 20 in-use IPs / 800 GB**. Per architecture it is 3 legs, because the weekly's
two legs may both be the same arch while release always contributes one of each:
**60 vCPUs of E2 and 60 of T2A**. Skew flipping on adds one amd64 cluster to the same
Sunday: 25 / 100 / 25 / 1000 GB.

Before enabling `OSTESTS_ENABLED`, request in the chosen region:

| metric | ask | default |
| --- | --- | --- |
| `CPUS` (E2) | 60 | varies, often 24–72 |
| `T2A_CPUS` | 60 | **commonly 0 until requested** |
| `IN_USE_ADDRESSES` | 20 | **8** |
| `DISKS_TOTAL_GB` | 800 | usually ample |

T2A also exists in only a handful of zones — the gcp backend preflights the
machine-type/zone pair and fails with the real reason rather than letting the arm64 leg
read as flaky.

### Backport the hydrophone pins first

The weekly builds **each branch from its own checkout**, while taking the provisioning
harness from the workflow ref so old release branches do not need every harness change
backported before they can be tested. The hydrophone install still reads the tested branch's
`build/versions.mk`. `hydrophone_version` and `hydrophone_sha256_linux_{amd64,arm64}` exist only
on this branch today — until those pins are backported to every branch in
`build/branches.txt`, every release-branch leg (4 of the 6) fails at the install step
before provisioning anything, and so does the release conformance gate for any v1.35.x /
v1.36.x patch tag.

### Teardown is not here

No backend implements destroy. `provision.sh`'s contract is explicit that **the caller
destroys**, in an `always()` step, so that a failed run leaves the cluster up long enough
to look at. What teardown misses, `sweep.sh` reaps (nightly — a leak costs by the day —
behind `OSTESTS_SWEEP_ENABLED`).
