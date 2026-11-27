# Kubernetes AI Conformance

This folder is everything we need to run the
[Kubernetes AI Conformance](https://github.com/cncf/k8s-ai-conformance) suite against
Stratum and submit the results for Kubernetes v1.37:

- `../run-ai-conformance.sh` runs the upstream test suite
  ([kubernetes-sigs/ai-conformance](https://github.com/kubernetes-sigs/ai-conformance))
  at the commit pinned as `ai_conformance_commit` in `build/versions.mk`. It
  works like `run-conformance.sh`: it uses a cluster that already exists and
  packages the evidence whether the run passes or fails.
- `setup.d/` holds Stratum's replacements for the upstream setup scripts that do
  not work on Stratum as written. The runner uses the upstream scripts for
  everything else. The chart versions these install are pinned in
  `build/versions.mk`, and the runner passes them in.
- `tools/test-ostests-ai-conformance.sh` tests the runner without a cluster, and
  `make lint` runs it.
- `PRODUCT.yaml` is the submission checklist, copied from the v1.37 template and
  partly filled in.

None of this has run yet. [Work left before the first run](#work-left-before-the-first-run)
lists what has to land first.

## The cluster

- Stratum at the release being certified. Its Kubernetes minor must match
  `kubernetesVersion` in `PRODUCT.yaml`.
- At least one control plane and one GPU worker. The GPU worker needs 2 NVIDIA
  GPUs: `TestHPAAutoscaling` holds 2 at once.
- On the GPU worker: the NVIDIA driver and the NVIDIA Container Toolkit, set up as
  [GPUs](../../../docs/docs/cluster-admin/gpus.md) describes, with the `nvidia`
  runtime handler. Do not install the device plugin or the DCGM exporter yourself:
  the runner installs them.
- On every node that may run the sandbox test: gVisor's `runsc`, declared as the
  `runsc` runtime handler.
- On every control plane, both RuntimeClasses:

  ```yaml
  spec:
    runtimeClasses:
      - nvidia
      - runsc
  ```

- The machine running the tests needs `git`, `go`, `helm`, `kubectl`, `python3`
  and `gotestsum` at the version `gotestsum_version` pins in `build/versions.mk`
  (`go install gotest.tools/gotestsum@v<version>`), a cluster-admin kubeconfig,
  and access to the internet: the setup scripts pull
  charts and manifests from upstream, and the tests pull `python:3.13-alpine` and
  `ubuntu:22.04`.

## Running it

```console
$ KUBECONFIG=/path/to/admin.conf PROVIDER=gcp ARCH=amd64 TAG=v1.37.2-stratum.0 \
    tools/ostests/run-ai-conformance.sh
```

`GPU=0` runs only the tests that need no GPU, on a cluster without one:
Gateway API, gang scheduling, AI service metrics, the Kubeflow Trainer operator and
workload sandboxing. `PROVIDER`, `ARCH` and `TAG` only name the results file.
The full run takes up to about 3 hours. `$TIMEOUT` sets `go test -timeout`
(default `240m`).

Results go to `_output/ai-conformance`, or to `$OUT`:

- `ai-conformance-<provider>-<arch>/` holds `e2e.log`, `junit.xml`,
  `results.json`, `summary.txt` and `test-args`, the flags the tests ran with.
  The runner empties it first, so it never holds an earlier run's files.
- `ai-conformance-results-<provider>-<arch>-<tag>.tar.gz` packs that folder.

The run passes only when the tests exit 0, `junit.xml` shows no failures and at
least one test that ran, and `e2e.log` is not empty. A failed run still writes
the tarball, and `summary.txt` names the failed tests.

## What each requirement uses

| Requirement | Level | Test | Installed by |
|---|---|---|---|
| `ai_inference` | MUST | `TestGatewayAPIInference` | upstream `50-agentgateway.sh` (Gateway API CRDs and agentgateway) |
| `gang_scheduling` | MUST | `TestGangScheduling` | upstream `50-gang-scheduler.sh` (Kueue) |
| `cluster_autoscaling` | MUST | skipped | N/A: Stratum ships no cluster autoscaler |
| `pod_autoscaling` | MUST | `TestHPAAutoscaling` | upstream `50-kube-prometheus-stack.sh` and `50-prometheus-adapter.sh` |
| `accelerator_metrics` | MUST | `TestAcceleratorPerformanceMetrics` | `setup.d/50-dcgm-exporter.sh` |
| `ai_service_metrics` | MUST | `TestAIServiceMetrics` | upstream `50-kube-prometheus-stack.sh` |
| `secure_accelerator_access` | MUST | `TestSecureAcceleratorAccess` | `setup.d/50-nvidia-device-plugin.sh` |
| `robust_controller` | MUST | `TestRobustCRDControllerOperation` | upstream `10-cert-manager.sh` and `40-kubeflow-trainer.sh` |
| `driver_runtime_management` | SHOULD | `TestAcceleratorDriverRuntimeManagement` | `setup.d/50-nvidia-device-plugin.sh` |
| `workload_sandboxing` | SHOULD | `TestWorkloadSandboxing` | `setup.d/50-gvisor-runtimeclass.sh` and the cluster setup above |
| `gpu_sharing`, `virtualized_accelerator`, `advanced_inference_ingress`, `high_performance_networking`, `disaggregated_inference` | SHOULD | none | documentation evidence only |

`driver_runtime_management` and `workload_sandboxing` become MUST in v1.38.

## Work left before the first run

1. **GPU pods without a RuntimeClass.** The suite's GPU pods ask for a GPU
   through the device plugin but set no `runtimeClassName`, and Stratum keeps runc as the
   default runtime. `setup.d/50-nvidia-device-plugin.sh` runs the device plugin
   with `deviceListStrategy: cdi-cri`, so containerd injects the GPU through CDI
   instead. Prove that on a GPU node with `TestSecureAcceleratorAccess` and
   `TestAcceleratorDriverRuntimeManagement` (which also needs `nvidia-smi` in a
   plain `ubuntu:22.04` pod). If it does not work, the NVIDIA DRA driver is the
   fallback.
2. **gVisor as a runtime handler.** Stratum's runtime handlers name an OCI
   runtime binary. Prove that `runsc` runs pods that way, or add what Stratum
   needs for it.
3. **Documentation evidence.** Every requirement without a test needs a public
   page as evidence, and the GPU setup in the runner should match what
   [GPUs](../../../docs/docs/cluster-admin/gpus.md) tells users. Write a docs
   page covering Gateway API with agentgateway, gang scheduling, monitoring,
   GPU sharing and sandboxing on Stratum.
4. **A GPU cluster** that meets [The cluster](#the-cluster).

## Submitting

1. Run the suite and check that every test except `TestAcceleratorClusterAutoscaling`
   passed.
2. Fill in `PRODUCT.yaml`: the metadata, then for each requirement its
   `status`, `evidence` and `notes`. Do not change any `id`, `description` or
   `level`. For a requirement with a test, the evidence is `e2e.log` and
   `junit.xml`. `k8sConformanceUrl` is Stratum's entry under `v1.37` in
   cncf/k8s-conformance.
3. Open a pull request to cncf/k8s-ai-conformance adding `v1.37/<dir>/` with
   `PRODUCT.yaml`, `e2e.log`, `junit.xml` and `results.json`. The vendor must
   have a CNCF participation form on file.
