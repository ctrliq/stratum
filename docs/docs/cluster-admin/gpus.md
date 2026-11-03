---
description: "Giving pods an NVIDIA GPU: the runtime handler, the RuntimeClass, the device plugin, and the traps on unified-memory hosts."
---

# GPUs

To give pods an NVIDIA GPU, a node declares NVIDIA's container runtime as a
runtime handler, the cluster publishes a RuntimeClass for it, and NVIDIA's
device plugin advertises the node's GPUs to the scheduler. Stratum does the
first two from its configuration file. This page is the whole procedure,
followed by two traps that are easy to hit on a host with one GPU or with
unified memory. It was verified with v1.37.2-stratum.0 on a GB10 node running
Rocky Linux from CIQ Pro AI 9.8 with SELinux enforcing, NVIDIA driver
580.173.02, NVIDIA Container Toolkit 1.20.1 and device plugin v0.17.4.

## Prerequisites

- The NVIDIA driver and the NVIDIA Container Toolkit on every GPU node:
  `nvidia-smi` lists the GPUs, and `/usr/bin/nvidia-container-runtime` exists.
  Stratum configures its own containerd, so do not run
  `nvidia-ctk runtime configure`. The toolkit's package also writes a CDI spec
  to `/var/run/cdi`, which `stratum runtime check` reports; the steps on this
  page do not use it.

## 1. Declare the handler and its RuntimeClass

On a GPU node, declare `nvidia` as a runtime handler in
[`spec.node.runtimeHandlers`](../installation/configuration.md#node-labels-taints-and-runtime-handlers).
On every control plane, with a GPU or not, publish a RuntimeClass for it with
[`spec.runtimeClasses`](../installation/configuration.md#runtimeclasses). The
list must be the same on every control plane, or the operator stops applying
the cluster's add-ons until it is:

```yaml
# /etc/stratum/config.yaml on a control plane that has a GPU
spec:
  node:
    runtimeHandlers:
      nvidia: /usr/bin/nvidia-container-runtime
  runtimeClasses:
    - nvidia
```

A control plane reads both on every start, so restart each control plane
after editing its file:

```console
# systemctl restart stratum
```

A worker reads `spec.node` only when it joins. Join a GPU worker with
`--runtime-handler nvidia=/usr/bin/nvidia-container-runtime`, or give an
already-joined worker the handler without restarting it:

```console
# /usr/local/bin/stratum runtime apply --runtime-handler nvidia=/usr/bin/nvidia-container-runtime
```

`--runtime-handler` replaces the handlers the worker saved; it does not add
to them. On a worker that already has other handlers, give each of them with
its own `--runtime-handler` too, or `apply` removes them and their labels.

If `apply` reports that containerd has not loaded the handlers, run
`sudo /usr/local/bin/stratum runtime restart`. Running containers keep running
while containerd restarts.

Then check the node:

```console
# /usr/local/bin/stratum runtime check
```

It exits 1 if containerd will not run the handler. Once containerd has loaded
it, the node carries the label `runtime.stratum.ciq.com/nvidia=true`, and the
`nvidia` RuntimeClass schedules its pods onto the nodes with that label only.
`runc` stays the default, so only pods that name the RuntimeClass run under
NVIDIA's runtime. See [Runtime handlers](../runtime/index.md#runtime-handlers)
for what `apply`, `check` and `restart` do.

## 2. Install the device plugin

```console
# /usr/local/bin/stratum kubectl apply -f \
  https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.17.4/deployments/static/nvidia-device-plugin.yml
```

NVIDIA's static manifest assumes `nvidia` is containerd's default runtime and
that every node it runs on has a GPU. Patch its DaemonSet for Stratum:

- `runtimeClassName: nvidia`, or the plugin cannot load NVML and logs
  `No devices found`.
- A `nodeSelector` on the handler's label, so the DaemonSet runs only on the
  GPU nodes. The RuntimeClass's node selector reaches each pod, not the
  DaemonSet's choice of nodes, so without it every node without a GPU gets a
  plugin pod that stays Pending.
- A toleration for `stratum.ciq.com/unverified`, so the plugin runs on a GPU
  worker that joined with the startup taint and has not been
  [released](../installation/configuration.md#releasing-and-holding-a-node) yet. Without
  it the node never advertises its GPU, and a burn-in job that asks for one
  never schedules. A merge patch replaces the whole list, so the patch keeps
  NVIDIA's own `nvidia.com/gpu` toleration too.
- On an SELinux node, `spc_t`, or the plugin cannot register with the kubelet.

```console
# /usr/local/bin/stratum kubectl patch daemonset nvidia-device-plugin-daemonset -n kube-system \
  --type merge -p '{"spec":{"template":{"spec":{"runtimeClassName":"nvidia","nodeSelector":{"runtime.stratum.ciq.com/nvidia":"true"},"tolerations":[{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoSchedule"},{"key":"stratum.ciq.com/unverified","operator":"Exists","effect":"NoSchedule"}],"securityContext":{"seLinuxOptions":{"type":"spc_t"}}}}}}'
```

SELinux does not confine a pod that runs as `spc_t`; see
[Pods that use the kubelet's sockets](hardening.md#pods-that-use-the-kubelets-sockets).

Use v0.17.4 or newer. On GPUs with unified memory, such as the DGX Spark's
GB10 and Jetson, NVML returns `Not Supported` for the per-device memory query.
Older plugins treat that as fatal and crash-loop with
`error getting device memory: Not Supported`.

Confirm that the node advertises its GPU:

```console
# /usr/local/bin/stratum kubectl get node <gpu-node> \
  -o jsonpath='{.status.allocatable.nvidia\.com/gpu}{"\n"}'
```

A number above `0` means the scheduler can place GPU pods on the node.

## 3. Verify with a pod

```console
# cat <<'MANIFEST' | /usr/local/bin/stratum kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: gpu-check
spec:
  runtimeClassName: nvidia
  restartPolicy: Never
  tolerations:
  - key: stratum.ciq.com/unverified
    operator: Exists
    effect: NoSchedule
  containers:
  - name: smi
    image: nvidia/cuda:12.6.3-base-ubuntu24.04
    command: ["nvidia-smi", "--query-gpu=name,driver_version,compute_cap", "--format=csv"]
    resources:
      limits:
        nvidia.com/gpu: 1
MANIFEST

# /usr/local/bin/stratum kubectl wait pod/gpu-check --for=jsonpath='{.status.phase}'=Succeeded --timeout=5m

# /usr/local/bin/stratum kubectl logs gpu-check
name, driver_version, compute_cap
NVIDIA GB10, 580.173.02, 12.1

# /usr/local/bin/stratum kubectl delete pod gpu-check
```

The first run waits for the image to pull. The toleration lets the pod run on
a node that has not been released yet, as a burn-in job does.

A GPU pod needs both fields. `runtimeClassName: nvidia` runs it under NVIDIA's
runtime, which gives it the driver libraries; `nvidia.com/gpu: 1` gives it a
GPU. A pod with only the limit gets a GPU but no driver libraries, and fails
to start with `exec: "nvidia-smi": executable file not found in $PATH`.

## GPU metrics

A GPU metrics exporter such as NVIDIA's DCGM exporter reads the kubelet's
pod-resources API to label each metric with the pod that uses the GPU. On
Stratum that socket is under the kubelet's root directory,
`/var/lib/stratum/kubelet/pod-resources`, not `/var/lib/kubelet/pod-resources`.
Set the exporter's Helm value `kubeletPath` to it. Like the device plugin, the
exporter also needs `runtimeClassName: nvidia`, for the driver libraries, and a
`nodeSelector` on the handler's label, so it runs only on the GPU nodes. The
chart leaves both empty:

```yaml
runtimeClassName: nvidia
nodeSelector:
  runtime.stratum.ciq.com/nvidia: "true"
kubeletPath: /var/lib/stratum/kubelet/pod-resources
```

On the default `kubeletPath` the exporter still runs and exports metrics, but
none of them carry a `pod` or `namespace` label, and it logs no error. On an
SELinux node it also needs to run as `spc_t`, like the device plugin.

## Two traps for a model server

**With one GPU per node, use `Recreate`.** A Deployment's default rolling
update starts the new pod before it stops the old one. When the old pod holds
the node's only GPU, the new pod cannot schedule
(`FailedScheduling: Insufficient nvidia.com/gpu`) and the rollout never
finishes. Set `spec.strategy.type: Recreate` on a Deployment whose pods each
take a whole GPU.

**On a unified-memory host, budget the model server's memory.** A DGX Spark
reports its whole 128 GB as GPU memory. An inference server that allocates too
much does not get a GPU out-of-memory error: the host runs out of memory, and
the machine can go down. A `resources.limits.memory` on the pod does not
prevent that on its own. GPU allocations may not be charged to the pod's
memory cgroup, so they can grow past the limit. Cap what the server allocates
instead, with its model size and its context or KV-cache budget, and leave the
host room to stay up. Run one model server per host. A memory limit still
bounds the server's other allocations.

A Deployment that does both:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ollama
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: ollama
  template:
    metadata:
      labels:
        app: ollama
    spec:
      runtimeClassName: nvidia
      containers:
      - name: ollama
        image: ollama/ollama:latest
        env:
        - name: OLLAMA_CONTEXT_LENGTH
          value: "8192"
        resources:
          limits:
            nvidia.com/gpu: 1
            memory: 100Gi
```

`OLLAMA_CONTEXT_LENGTH` caps the default context, which Ollama otherwise sizes
from the roughly 121 GiB the GB10 reports as GPU memory. It is the budget that
keeps the server's GPU allocations down; the `100Gi` limit bounds the rest.

## Moving from `nvidia-ctk`

A node set up by hand, with `nvidia-ctk runtime configure` and a RuntimeClass
you applied yourself, keeps working. Once you declare the handler in step 1,
`stratum runtime check` warns that `/etc/containerd/conf.d/99-nvidia.toml`
defines the `nvidia` runtime too. To move it to the steps above:

1. Declare the handler and the RuntimeClass as in
   [step 1](#1-declare-the-handler-and-its-runtimeclass).
2. Delete the `nvidia` RuntimeClass you applied. Stratum leaves a RuntimeClass
   it did not publish as it is, so yours would keep scheduling GPU pods onto
   nodes without the handler. Stratum publishes its own, with the node
   selector, within a few minutes, and the `get` prints that selector once it
   is there:

    ```console
    # /usr/local/bin/stratum kubectl delete runtimeclass nvidia
    # /usr/local/bin/stratum kubectl get runtimeclass nvidia -o jsonpath='{.scheduling.nodeSelector}{"\n"}'
    ```

    Running pods keep running. Until the new class is there, a new pod that
    names it is refused.
3. Check whether the file makes `nvidia` containerd's default runtime:

    ```console
    # grep default_runtime_name /etc/containerd/conf.d/99-nvidia.toml
    ```

    If it does, GPU pods that do not name a RuntimeClass run under NVIDIA's
    runtime now, and their replacements run under `runc`, without the driver
    libraries, once the file is gone. Set `runtimeClassName: nvidia` on those
    workloads' pod templates first.
4. Remove `/etc/containerd/conf.d/99-nvidia.toml`, then run
   `sudo /usr/local/bin/stratum runtime restart` and
   `sudo /usr/local/bin/stratum runtime check`.
5. Once `check` passes, the node carries `runtime.stratum.ciq.com/nvidia=true`.
   Patch your device plugin's DaemonSet as in
   [step 2](#2-install-the-device-plugin). Patching before the label is there
   takes the plugin off the node, because its new node selector does not
   match yet. Running the patch again is harmless.

`stratum uninstall` removes Stratum's runtime-handler file with the rest of
`/var/lib/stratum`. It does not remove a file in `/etc/containerd/conf.d` that
`nvidia-ctk` wrote, since Stratum did not create it.
