# Stratum Kubernetes Engine (SKE)

Stratum is a Kubernetes distribution that ships as one self-extracting binary.
The binary carries its component binaries and container images.
`build/versions.mk` pins every version. Components build hermetically from
unmodified upstream tags. The one exception is the dashboard, whose npm
dependencies resolve from the public registry.

The control plane runs as kubelet-managed static pods. The product layer on top
adds declarative cluster config, token joins, managed etcd with scheduled and S3
snapshots, a manifest deployer with pruning, a HelmChart operator, and rolling
upgrades. The CIS hardening profile and its auditor are part of
[Stratum PRO](docs/docs/pro/index.md). The engine keeps the profile mechanism
they are built on.

## What is in the box

| Component | How it ships | Role |
|---|---|---|
| kubernetes 1.37.1 | kubelet and kubectl built from source; control plane from upstream OCI images | kubelet and kubectl on the host; apiserver, controller-manager, scheduler as static pods |
| containerd 2.3.7 + runc 1.5.2 | built from source | container runtime, cgroup v2 only |
| etcd 3.7.0 | OCI image; etcdctl and etcdutl built from source | the only datastore, TLS, managed (join/snapshot/restore), runs as a static pod |
| kube-vip 1.3.1 | OCI image | API server VIP, a static pod on controllers |
| cilium 1.20.3 | OCI images, manifest stack | CNI, network policy, kube-proxy replacement |
| coredns 1.15.0 | OCI image, manifest stack | cluster DNS |
| local-path 0.0.37 | OCI images, manifest stack | default StorageClass |
| dashboard | React + Vite SPA, embedded in the binary | `stratum dash`: cluster views, pod `exec`, optional node shell |

## Quick start

Host tools come from [`mise.toml`](mise.toml): install
[mise](https://mise.jdx.dev/installing-mise.html), then run `mise install`. mise
does not provide Go, which `build/versions.mk` pins. It also does not provide
`docker` or `ssh`, which `make inttest` and the ostests backends need. It does
provide `node` and `pnpm` for `make build`. The binary embeds the dashboard SPA,
so building `stratum` also builds the dashboard. `TAGS=noui` skips it.

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
| `build/versions.mk` | The only pin file. It holds every version and build flag. |
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

The last step appends the payload ZIP to the binary. A ZIP reader finds the
central directory by scanning backwards from the end of the file. An ELF loader
ignores trailing bytes. So `dist/stratum` is both a working executable and a
working archive of its own payload. At runtime it opens itself and extracts
`bin/` and `images/` under `/var/lib/stratum`. It skips extraction when size and
mtime already match, because the payload holds about a third of a gigabyte of
images. The images ship in the same archive, so there is no separate airgap
bundle.

## Documentation

- [**Cutting a release**](RELEASE.md): who may, the RC and hotfix and batch rules, and the whole procedure end to end
- [**Public documentation**](docs/README.md): the user-facing MkDocs site, and how to build it locally
- [**Contributing**](CONTRIBUTING.md): building, testing, and the commit/pull request conventions
- [**Security**](SECURITY.md): reporting a vulnerability, supported versions, and how `stratum dash` handles credentials and the node shell

## Get Pro

CIQ supports Stratum through a [Stratum PRO](https://ciq.com/product/stratum)
license. Support is not sold separately. Stratum PRO adds the CIS hardening
profile, fleet management, single sign-on and cluster provisioning to the same
engine; the [Stratum PRO page](docs/docs/pro/index.md) lists what it adds.

## License

Stratum is licensed under the [Apache License, Version 2.0](LICENSE).
Third-party material checked into this repository keeps its own license; see
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). [LICENSING.md](LICENSING.md)
holds the policy for the licenses of the code Stratum ships.
