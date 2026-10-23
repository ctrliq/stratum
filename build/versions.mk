# SKE version-pin manifest — the ONLY place a third-party version may appear.
#
# Rules (enforced by tools/lint-versions.sh):
#   1. No version literal for these components appears anywhere else in the repo.
#   2. containerd-class components pin version AND git revision.
#   3. go_version tracks upstream Kubernetes' .go-version for the pinned tag
#      (enforced by tools/lint-go-version.sh).
#   4. Every pin carries a renovate annotation directly above it.
#
# Branch-aware Kubernetes pin policy, enforced by tools/lint-kube-images.sh
# (the gate reads the branch; the pin files cannot carry it):
#   - a release branch (release/* or release-<minor>) pins one version
#     everywhere: its go.mod k8s.io libraries are exactly kubernetes_version's
#     own upstream tag (1.37.1 pins them at v0.37.1) — the same version as the
#     built binaries and the mirrored kube-* images;
#   - main tracks the next minor early: its go.mod k8s.io libraries may drift
#     one minor ahead of kubernetes_version into a prerelease (v0.38.0-alpha.0
#     against 1.37.1 today), and the gate reports the drift as a WARN, never
#     a failure. Every branch that is not a release branch carries main's
#     rule;
#   - mise.toml's kubectl stays within upstream-supported skew of
#     kubernetes_version — one minor, older or newer.

# --- toolchain ---------------------------------------------------------------

# Matches kubernetes/kubernetes v$(kubernetes_version) .go-version exactly.
# Do not bump independently: bump kubernetes_version, then re-read .go-version.
# renovate: datasource=golang-version depName=go
go_version = 1.26.8

# renovate: datasource=docker depName=alpine versioning=docker
alpine_version = 3.24

default_buildimage = docker.io/library/golang:$(go_version)-alpine$(alpine_version)

# Node and pnpm build and publish the dashboard (dash/). Not a runtime
# dependency of anything a cluster runs: the payload's stratum-dash image
# carries the built bundle, not Node, and `stratum dash` serves that bundle. They are pinned
# for the same reason the renovate validator is — a toolchain nobody chose is a
# toolchain that fails the build for reasons that have nothing to do with the
# change. release.yml's dash-publish job is the only CI consumer.
# renovate: datasource=node-version depName=node
node_version = 24.22.1

# What dash/pnpm-lock.yaml was written by, and what dash/package.json's
# `packageManager` must say — corepack needs a literal there, so
# tools/lint-versions.sh asserts the two agree instead of forbidding it.
# Deliberately behind pnpm's latest major: moving it rewrites the lockfile, so
# it moves on its own Renovate PR with `pnpm test` and `pnpm run build` green,
# not as a side effect of a dashboard change.
# renovate: datasource=npm depName=pnpm
pnpm_version = 12.15.0

# --- kubernetes --------------------------------------------------------------

# renovate: datasource=github-tags depName=kubernetes/kubernetes
kubernetes_version = 1.35.9
kubernetes_revision = 329000efd0b26bd78155e51e4df449130f0fca00
kubernetes_buildimage = $(default_buildimage)
kubernetes_build_go_tags = providerless
kubernetes_build_go_cgo_enabled = 0
kubernetes_build_go_flags = -v
kubernetes_build_go_ldflags_extra = -extldflags=-static -w -s
# The host binaries SKE supervises or execs. Built with upstream's own make
# WHAT=... The control-plane binaries (kube-apiserver, kube-controller-manager,
# kube-scheduler) are NOT built or staged: they run as static
# pods from the upstream images pinned in build/images.txt. No kube-proxy:
# Cilium's kube-proxy replacement handles services.
kubernetes_binaries = kubelet kubectl

# --- containerd --------------------------------------------------------------

# renovate: datasource=github-tags depName=containerd/containerd
containerd_version = 2.2.10
containerd_revision = 991ca6a8ca58b47346a42093210099e090bc6c1f
containerd_buildimage = $(default_buildimage)
containerd_build_go_tags = apparmor selinux
containerd_build_go_cgo_enabled = 0
containerd_build_go_ldflags_extra = -extldflags=-static -w -s
containerd_binaries = containerd containerd-shim-runc-v2 ctr

# --- runc --------------------------------------------------------------------

# renovate: datasource=github-tags depName=opencontainers/runc
runc_version = 1.5.2
runc_revision = 29dd3dc2b13b4123162e5fe132504bb4b15569f1
runc_buildimage = $(default_buildimage)
runc_build_go_tags = seccomp urfave_cli_no_docs
# runc needs cgo for libseccomp; static link against musl.
runc_build_go_cgo_enabled = 1
runc_build_go_ldflags_extra = -w -s
runc_binaries = runc

# --- kube-vip ----------------------------------------------------------------
# Control-plane load balancing: one API server VIP serving both in-cluster
# and external clients, as a single mechanism with no per-worker proxy. Ships
# as a static pod from the upstream image — the tag and matching digest are
# pinned in build/images.txt, the pin surface for images, and the version the
# binary reports (main.kubeVipVersion) is stamped from that tag by the
# Makefile — so no binary is built here and no version is pinned twice.

# --- etcd --------------------------------------------------------------------
# Pinned to what upstream Kubernetes $(kubernetes_version) declares in
# build/dependencies.yaml, NOT to etcd's latest release. Kubernetes' e2e and
# storage tests are run against this exact etcd; tracking latest instead would
# put SKE outside the tested combination.

# renovate: datasource=github-tags depName=etcd-io/etcd
etcd_version = 3.6.6
etcd_revision = d2809cf0019f84c221e026bb2ac6486d011b1d91
etcd_buildimage = $(default_buildimage)
etcd_build_go_tags =
etcd_build_go_cgo_enabled = 0
etcd_build_go_ldflags_extra = -extldflags=-static -w -s
# etcdctl and etcdutl only: the etcd server itself runs as a static pod from the
# upstream image pinned in build/images.txt. etcdctl stays a host
# binary for `stratum etcdctl`; etcdutl for offline snapshot restore, which
# etcdctl no longer has since etcd 3.6.
etcd_binaries = etcdctl etcdutl

# Deliberately absent: kine — etcd is the only datastore, so no
# SQL backend is pinned, built, or supported.

# --- release tooling ----------------------------------------------------------
# Tools the release pipeline runs. Pinned here for the same reason everything
# else is: a release that cannot say which hydrophone produced its conformance
# evidence has not produced evidence. cosign and syft come from their own
# official setup actions, which pin themselves.

# The config validator, which must be the Renovate the bot actually runs — an
# older one rejects options a newer one added, and the failure looks exactly like
# a broken config. Unpinned, `npx --package renovate` resolves different
# versions in CI and locally, so the same file can validate differently in
# two places.
# renovate: datasource=npm depName=renovate
renovate_version = 44.180.0

# renovate: datasource=github-releases depName=kubernetes-sigs/hydrophone
hydrophone_version = 0.8.0
# Release-tarball digests for the pinned version, verified at download time —
# the binary is extracted onto a runner's PATH with root, and TLS alone
# authenticates the channel, not the artifact. From the project's
# hydrophone_<v>_checksums.txt; bump together with hydrophone_version. The keys
# say amd64/arm64 like every other pin here; the release assets say x86_64/arm64
# (hydrophone_Linux_x86_64.tar.gz), which the install steps translate.
hydrophone_sha256_linux_amd64 = 873f75a701d22f0f59c62fd0153101e7a37155984bf497ae035eab226949bdf4
hydrophone_sha256_linux_arm64 = 3ad340d2891decbdc1f2f460a85beaf4fa114a823eb415446b52f9aa021f066b

# renovate: datasource=github-releases depName=google/go-containerregistry
crane_version = 0.22.1
# Release-tarball digests, verified before crane is extracted onto a runner's
# PATH, where it is handed a GITHUB_TOKEN that can push packages. From the
# project's checksums.txt for the pinned version; bump together with
# crane_version. Keyed amd64/arm64; the assets say x86_64/arm64.
crane_sha256_linux_amd64 = 0ab7a1d6932a213aed964ce97666c3077fe691c8606413674a8b3e0b9ec4cda0
crane_sha256_linux_arm64 = 898c0cff975f898a33e8c4580bdafb0e7c02c7faa33374e946762f97c4ab7110

# The merge driver backport.yaml resolves cherry-pick conflicts with, installed
# per run with `cargo install --locked`. versioning=semver because the crate
# datasource defaults to cargo versioning, which reads a bare 0.20.0 as the
# range ^0.20.0 and would never propose a patch release.
# renovate: datasource=crate depName=mergiraf versioning=semver
mergiraf_version = 0.20.0

# The kubelet the weekly skew-extremes suite runs on one worker: three minors
# behind $(kubernetes_version), which is the far edge of Kubernetes' published
# version-skew policy for kubelet-vs-apiserver. Upstream's own release binary
# from dl.k8s.io — SKE builds no N-3 kubelet, because build/branches.txt carries
# two minors, and the envelope claims the kubelet UPSTREAM shipped at N-3 works.
# tools/ostests/run-skew.sh has the full rationale.
#
# Deliberately NOT renovate-annotated: a bot can express "newest 1.33.x" but not
# "three minors behind kubernetes_version", so it would walk this pin up to N and
# quietly turn the suite into a no-skew test. Bump it BY HAND with every
# kubernetes_version minor bump; `tools/ostests/run-skew.sh --check-pins`
# (wired into `make lint`) fails until you do.
skew_kubelet_version = 1.32.13
# Release-binary digests for the pinned version, verified before the binary is
# installed onto a node, from dl.k8s.io/release/v$(skew_kubelet_version)/bin/
# linux/<arch>/kubelet.sha256. Bump together with skew_kubelet_version.
skew_kubelet_sha256_linux_amd64 = 4bc04c3f50725fa29ac9f0626c553941cfe07d128b3a313b7f5d176b07ab786e
skew_kubelet_sha256_linux_arm64 = 46b5b8d313b47aa027e5a874fb8301503e848f313b56f54b9a2697f75409913c

# --- the runtime image mirror -------------------------------------------------
# Every image a running cluster uses is mirrored into this namespace by digest
# (tools/mirror-images.sh) and referenced from here — never from registry.k8s.io,
# quay.io or Docker Hub. build/images.txt keeps the upstream refs because that
# is what Renovate watches and what the mirror copies FROM; the digest is
# identical on both sides, so the two names address the same bytes.
#
# Not a version pin: no literal of it is forbidden elsewhere, because it is the
# one string that has to appear in the bundled manifests for them to work.
image_registry = ghcr.io/ctrliq/stratum

# --- the public documentation site ------------------------------------------
# docs/ is built with MkDocs Material (`make docs`). Pinned here like
# every other tool a job installs: an unpinned `pip install mkdocs-material`
# would re-render the published site from a generator nobody chose, with no repo
# change to point at.
#
# mkdocs-material has an announced 2.0 rewrite with no migration path, and warns
# about it on every build. renovate.json5 only automerges minor/patch, so that
# major arrives as its own PR that has to pass `make docs` before anyone reads
# it — which is the point of pinning it rather than tracking a range.
# renovate: datasource=pypi depName=mkdocs
mkdocs_version = 1.6.1
# renovate: datasource=pypi depName=mkdocs-material
mkdocs_material_version = 9.7.7

# --- SKE's own version ------------------------------------------------------
# Full version string is v$(kubernetes_version)-stratum.<N>, derived by
# tools/version.sh from git tags.
stratum_name = stratum
