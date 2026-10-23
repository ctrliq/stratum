# Stratum Kubernetes Engine (SKE)

A new Kubernetes distribution: one self-extracting binary that carries its
component binaries AND container images (no airgap variant — the artifact is
airgap-complete), pins every version in one file, builds hermetically from
unmodified upstream tags (with one exception: the dashboard's npm dependencies
resolve from the public registry), and runs the control plane as
kubelet-managed static pods with a full product layer on top — declarative
cluster config, token joins, managed etcd with scheduled/S3 snapshots, a
manifest deployer with pruning, a HelmChart operator, and rolling upgrades.
The CIS hardening profile and its auditor are part of
[Stratum PRO](docs/docs/pro/index.md); the engine keeps the profile mechanism
they are built on.

## What is in the box

| Component | How it ships | Role |
|---|---|---|
| kubernetes 1.36.5 | kubelet and kubectl built from source; control plane from upstream OCI images | kubelet and kubectl on the host; apiserver, controller-manager, scheduler as static pods |
| containerd 2.3.7 + runc 1.5.2 | built from source | container runtime, cgroup v2 only |
| etcd 3.6.8 | OCI image; etcdctl and etcdutl built from source | the only datastore, TLS, managed (join/snapshot/restore), runs as a static pod |
| kube-vip 1.3.1 | OCI image | API server VIP, a static pod on controllers |
| cilium 1.20.3 | OCI images, manifest stack | CNI, network policy, kube-proxy replacement |
| coredns 1.14.7 | OCI image, manifest stack | cluster DNS |
| local-path 0.0.37 | OCI images, manifest stack | default StorageClass |
| dashboard | React + Vite SPA, embedded in the binary | `stratum dash`: cluster views, pod `exec`, optional node shell |

## Naming

The product is **Stratum Kubernetes Engine**, SKE for short. Every *code*
identifier uses the standalone name `stratum` — the binary, the module path,
`/var/lib/stratum/bin`, the version suffix, the patch prefix.

That split is deliberate, not an inconsistency to tidy up. "Kubernetes"
in a product name is a Linux Foundation mark usable only after CNCF conformance
certification, so the combination name cannot be load-bearing in code that ships
before certification. `stratum` works standalone at every stage.

## Quick start

Host tools come from [`mise.toml`](mise.toml): install
[mise](https://mise.jdx.dev/installing-mise.html), then `mise install`. It does
not provide Go (that pin lives in `build/versions.mk`), nor `docker` and `ssh`,
which `make inttest` and the ostests backends need. It does provide `node` and
`pnpm`, which `make build` needs: the dashboard SPA is embedded in the binary, so
building it is part of building `stratum` (`TAGS=noui` skips it).

```bash
make help                      # targets, current version, target platform
make components                # build the host components (Kubernetes takes ~25 min)
make dash                      # build the dashboard SPA into dash/dist
make images                    # pull and stage the pinned OCI images
make package                   # the release artifact: dist/stratum (~610 MB, airgap-complete)
make smoke                     # run it in a clean container
make inttest                   # real clusters in containers: Ready node, network, day-2
```

```bash
# then, on a node:
stratum up                       # a single schedulable node to Ready, no registry
```

```bash
make lint                      # pin and patch governance
make test                      # go vet + go test
make verify-payload            # embedded image set == build/images.txt, by name and digest
make fences-check              # version fences still match upstream compatibility data
make days-behind               # how far behind upstream each branch is
make verify-reproducible COMPONENT=runc
```

## Layout

| Path | What |
|---|---|
| `build/versions.mk` | **The** pin file. Every version and build flag. Nothing else may pin a version. |
| `build/images.txt` | Runtime OCI images, digest-pinned. Staged into the binary's payload. |
| `build/components/<name>/Dockerfile` | Hermetic per-component build. All versions arrive as build args. |
| `build/components/<name>/patches/` | Ordered `git am` queue. Ships empty. |
| `tools/` | Governance: version lint, patch lint, droppable-patch detection, version derivation, build-info. |
| `tools/inttest/` | Integration checks. The CI matrix is generated from this directory. |
| `cmd/stratum`, `pkg/cli` | The binary's entry point and its command-line interface. |
| `internal/assets` | Runtime staging of the appended ZIP payload (binaries + images). |
| `dash/` | The dashboard SPA (React + Vite) and the `//go:embed` that carries it into the binary. |
| `internal/dashboard` | The `stratum dash` server: apiserver proxy, session layer, node-shell gate. |
| `internal/` | The control plane and product layer: PKI, static pods, host-process supervision, etcd, upgrades, hardening. |
| `renovate.json5` | Bump grouping and per-branch version fences (generated). |
| `docs/` | The public documentation site (MkDocs Material). Published, not read in-repo. |

## How the pieces fit

```
build/versions.mk  ──┐
                     ├─→ per-component build containers ─→ staging/<os>-<arch>/bin/
build/components/ ───┘                                            │
build/images.txt  ─────→ docker pull @digest + save ─────→ staging/<os>-<arch>/images/
                                                                  │
                                                                  ├─→ embedded-bins.zip
                                                                  │        │
                                                  cmd/stratum ──────┴────────┴─→ dist/stratum
                                                                        (cat zip >> binary)
```

The last step is the interesting one. A ZIP reader locates the central directory
by scanning backwards from EOF, and an ELF loader ignores trailing bytes, so
`dist/stratum` is simultaneously a working executable and a working archive of its
own payload. At runtime it opens itself, extracts `bin/` and `images/` under
`/var/lib/stratum`, and skips extraction entirely when size and mtime already
match — which matters when the payload is a third of a gigabyte of images.
Carrying the images in the same archive is what removes the separate airgap
bundle.

## Documentation

- [**Cutting a release**](RELEASE.md) — who may, the RC and hotfix and batch
  rules, and the whole procedure end to end
- [**Public documentation**](docs/README.md) — the user-facing MkDocs site,
  and how to build it locally
- [**Contributing**](CONTRIBUTING.md) — building, testing, and the commit and
  pull request conventions
- [**Security**](SECURITY.md) — reporting a vulnerability, supported versions,
  and how `stratum dash` handles credentials and the node shell

## What is not here

These are deferred, and nothing blocks them: an SSH installer that builds a
fleet from zero, channel-polling autopilot CRs, konnectivity, SLSA attestations,
and a nightly rebuild-and-compare.

## License

Stratum is licensed under the [Apache License, Version 2.0](LICENSE).
Third-party material checked into this repository keeps its own license; see
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). [LICENSING.md](LICENSING.md)
holds the policy for the licenses of the code Stratum ships.
