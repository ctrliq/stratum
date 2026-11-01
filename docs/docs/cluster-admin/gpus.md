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
- A `runc` on the host's `PATH`, such as `/usr/bin/runc`.
  `nvidia-container-runtime` adds the GPU to the container and then runs it
  with the first `runc` it finds there; Stratum's own `runc` is not on the
  `PATH`.

## 1. Declare the handler and its RuntimeClass

On a GPU node, declare `nvidia` as a runtime handler in
[`spec.node.runtimeHandlers`](../installation/configuration.md#node-labels-taints-and-runtime-handlers).
On a control plane, publish a RuntimeClass for it with
[`spec.runtimeClasses`](../installation/configuration.md#runtimeclasses):

```yaml
# /etc/stratum/config.yaml on a control plane that has a GPU
spec:
  node:
    runtimeHandlers:
      nvidia: /usr/bin/nvidia-container-runtime
  runtimeClasses:
    - nvidia
```

A control plane reads both on every start, so restart it after editing the
file:

```bash
sudo systemctl restart stratum
```

A worker reads `spec.node` only when it joins. Join a GPU worker with
`--runtime-handler nvidia=/usr/bin/nvidia-container-runtime`, or give an
already-joined worker the handler without restarting it:

```bash
sudo stratum runtime apply --runtime-handler nvidia=/usr/bin/nvidia-container-runtime
```

If `apply` reports that containerd has not loaded the handlers, run
`sudo stratum runtime restart`. Running containers keep running while
containerd restarts.

Then check the node:

```bash
sudo stratum runtime check
```

It exits 1 if containerd will not run the handler. Once containerd has loaded
it, the node carries the label `runtime.stratum.ciq.com/nvidia=true`, and the
`nvidia` RuntimeClass schedules its pods onto the nodes with that label only.
`runc` stays the default, so only pods that name the RuntimeClass run under
NVIDIA's runtime. See [Runtime handlers](../runtime/index.md#runtime-handlers)
for what `apply`, `check` and `restart` do.

## 2. Install the device plugin

```bash
sudo stratum kubectl apply -f \
  https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.17.4/deployments/static/nvidia-device-plugin.yml
```

NVIDIA's static manifest assumes `nvidia` is containerd's default runtime. It
is not, so the plugin's pod needs `runtimeClassName: nvidia`, or it cannot load
NVML and logs `No devices found`. On an SELinux node it also needs to run as
`spc_t`, or it cannot register with the kubelet. Patch the DaemonSet for both:

```bash
sudo stratum kubectl patch daemonset nvidia-device-plugin-daemonset -n kube-system \
  --type merge -p '{"spec":{"template":{"spec":{"runtimeClassName":"nvidia","securityContext":{"seLinuxOptions":{"type":"spc_t"}}}}}}'
```

The RuntimeClass also limits the DaemonSet to the GPU nodes, so its pods do
not run on the nodes that have no GPU. A pod that runs as `spc_t` has the
host's full access; see
[Pods that use the kubelet's sockets](hardening.md#pods-that-use-the-kubelets-sockets).

Use v0.17.4 or newer. On GPUs with unified memory, such as the DGX Spark's
GB10 and Jetson, NVML returns `Not Supported` for the per-device memory query.
Older plugins treat that as fatal and crash-loop with
`error getting device memory: Not Supported`.

Confirm that the node advertises its GPU:

```bash
sudo stratum kubectl get node <gpu-node> \
  -o jsonpath='{.status.allocatable.nvidia\.com/gpu}{"\n"}'
```

A number above `0` means the scheduler can place GPU pods on the node.

## 3. Verify with a pod

```bash
cat <<'MANIFEST' | sudo stratum kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: gpu-check
spec:
  runtimeClassName: nvidia
  restartPolicy: Never
  containers:
  - name: smi
    image: nvidia/cuda:12.6.3-base-ubuntu24.04
    command: ["nvidia-smi", "--query-gpu=name,driver_version,compute_cap", "--format=csv"]
    resources:
      limits:
        nvidia.com/gpu: 1
MANIFEST
sudo stratum kubectl logs gpu-check
```

A GPU pod needs both fields. `runtimeClassName: nvidia` runs it under NVIDIA's
runtime, which gives it the driver libraries; `nvidia.com/gpu: 1` gives it a
GPU. A pod with only the limit gets a GPU but no driver libraries, and fails
to start with `exec: "nvidia-smi": executable file not found in $PATH`.

## GPU metrics

A GPU metrics exporter such as NVIDIA's DCGM exporter reads the kubelet's
pod-resources API to label each metric with the pod that uses the GPU. On
Stratum that socket is under the kubelet's root directory,
`/var/lib/stratum/kubelet/pod-resources`, not `/var/lib/kubelet/pod-resources`.
Set the exporter's Helm value `kubeletPath` to it:

```yaml
kubeletPath: /var/lib/stratum/kubelet/pod-resources
```

On the default path the exporter still runs and exports metrics, but none of
them carry a `pod` or `namespace` label, and it logs no error. On an SELinux
node it also needs to run as `spc_t`, like the device plugin.

## Two traps for a model server

**With one GPU per node, use `Recreate`.** A Deployment's default rolling
update starts the new pod before it stops the old one. When the old pod holds
the node's only GPU, the new pod cannot schedule
(`FailedScheduling: Insufficient nvidia.com/gpu`) and the rollout never
finishes. Set `spec.strategy.type: Recreate` on a Deployment whose pods each
take a whole GPU.

**On a unified-memory host, set a memory limit.** A DGX Spark reports its whole
128 GB as GPU memory. An inference server that allocates too much does not get
a GPU out-of-memory error: the host runs out of memory, and the machine can go
down. A `resources.limits.memory` on the pod turns that into an OOM-killed
container. Size it below what the host needs to stay up; on a 128 GB DGX Spark,
`100Gi` left enough for the node. Run one model server per host.

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
from the roughly 121 GiB the GB10 reports as GPU memory.

## Moving from `nvidia-ctk`

A node set up by hand, with `nvidia-ctk runtime configure` and a RuntimeClass
you applied yourself, keeps working, and `stratum runtime check` warns that
`/etc/containerd/conf.d/99-nvidia.toml` defines the `nvidia` runtime too. To
move it to the steps above:

1. Declare the handler and the RuntimeClass as in
   [step 1](#1-declare-the-handler-and-its-runtimeclass). Stratum takes over a
   RuntimeClass named `nvidia` that you applied, and adds its node selector
   within a few minutes. A
   RuntimeClass's handler cannot change, so if yours names a handler other
   than `nvidia`, delete it first.
2. Remove `/etc/containerd/conf.d/99-nvidia.toml`, then run
   `sudo stratum runtime restart` and `sudo stratum runtime check`.

`stratum uninstall` removes Stratum's runtime-handler file with the rest of
`/var/lib/stratum`. It does not remove a file in `/etc/containerd/conf.d` that
`nvidia-ctk` wrote, since Stratum did not create it.
