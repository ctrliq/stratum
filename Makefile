# Stratum Kubernetes Engine — build entrypoint.
#
# Everything a build depends on comes from build/versions.mk (binaries) and
# build/images.txt (images). If you find yourself adding a version literal
# here, you are in the wrong file.

include build/versions.mk

SHELL := /usr/bin/env bash

# Probe the daemon, THEN decide — the same shape tools/dev-release.sh uses.
# `docker version --format ... || uname -m` is wrong: an unreachable
# daemon writes a blank line to STDOUT *and* exits non-zero, so `||` fires and
# both outputs land here. $(shell) folds the newline to a space, make sees two
# words, and STAGING becomes `staging/linux- arm64` while the component build
# declares a target named `.stamps/linux-`. Nothing errors; the build just
# writes where nobody looks.
#
# An unreachable daemon FALLS BACK rather than failing. HOST_ARCH is expanded
# for every target, so a hard error here would break `make test`, `make lint`
# and `make help` on a machine that never needed docker — and `uname -m` is the
# right answer anyway, since amd64 and arm64 are both built natively. The targets
# that do need docker already fail on their own. What must not happen is a
# silently empty or split arch, which the TARGET_ARCH guard below refuses.
#
# The daemon's answer is passed through as-is, and stripped ONLY to decide
# whether it was blank. Squashing whitespace out of the value itself would turn
# a malformed `amd64 arm64` into the single word `amd64arm64`, which sails past
# both guards and builds for a platform nobody asked for.
HOST_ARCH   := $(shell \
	arch=; \
	if docker version >/dev/null 2>&1; then \
	  arch=$$(docker version --format '{{.Server.Arch}}' 2>/dev/null); \
	fi; \
	[ -n "$$(printf '%s' "$$arch" | tr -d '[:space:]')" ] || arch=$$(uname -m); \
	printf '%s' "$$arch" | sed 's/x86_64/amd64/;s/aarch64/arm64/')
TARGET_ARCH ?= $(HOST_ARCH)
TARGET_OS   ?= linux

# Every path below embeds TARGET_ARCH. An empty or multi-word value does not
# error on its own — it produces `staging/linux-/`, a tarball with a space in
# its name, or two targets where one was meant. Refuse instead.
ifneq ($(words $(TARGET_ARCH)),1)
$(error TARGET_ARCH must be exactly one word, got '$(TARGET_ARCH)')
endif
# $(words) counts fields, so it sees ' arm64 ' as one word and lets it through
# — and the surrounding space still reaches the paths below, giving a staging
# directory whose name ends in a space. Compare against the stripped value too.
ifneq ($(TARGET_ARCH),$(strip $(TARGET_ARCH)))
$(error TARGET_ARCH must not be padded with whitespace, got '$(TARGET_ARCH)')
endif
# Architecture values reach unquoted shell arguments and build paths. Accept
# only the two supported targets, not arbitrary single-word shell syntax.
ifeq ($(filter amd64 arm64,$(TARGET_ARCH)),)
$(error TARGET_ARCH must be amd64 or arm64, got '$(TARGET_ARCH)')
endif

VERSION     := $(shell tools/version.sh --stamp)
# The source a binary was built from and how, the same on every architecture:
# the cluster upgrade pins every node to it (tools/build-id.sh). Recursive, so
# it is computed at link time, after the dashboard it hashes has been built.
BUILD_ID     = $(shell tools/build-id.sh '$(strip $(TAGS))' '$(EMBEDDED_BINS)')
STAGING     := staging/$(TARGET_OS)-$(TARGET_ARCH)
BINSTAGE    := $(STAGING)/bin
IMGSTAGE    := $(STAGING)/images
IMAGETAR    := $(IMGSTAGE)/$(stratum_name)-images.tar
DASH_IMAGETAR := $(IMGSTAGE)/stratum-dash.tar
DASH_BIN    := $(STAGING)/stratum-dash
DIST        := dist
BIN         := $(DIST)/$(stratum_name)
ZIP         := $(DIST)/embedded-bins-$(TARGET_OS)-$(TARGET_ARCH).zip
COMPONENTS  := $(shell $(MAKE) -s -C build/components list)

# The exact binary set a release carries. Packaging names these explicitly
# rather than globbing the staging dir, so a binary dropped from a component's
# manifest (kube-proxy, dropped when Cilium replaced it) cannot linger in
# staging and silently ship in the next artifact.
PAYLOAD     := $(foreach c,$(COMPONENTS),$($(c)_binaries))

# Go build tags for every go build/vet/test in this file.
#
# Empty by default, so a release binary carries the real dashboard: with no tag,
# dash/embed_ui.go embeds `all:dist` and the Go build needs dash/dist to exist.
# That is why `build` and `test` depend on the `dash` target below —
# the SPA is a build input, not an optional extra.
#
# `TAGS=noui` is the escape hatch, not the default: it swaps the embedded SPA
# for an empty filesystem (dash/embed_noui.go), so `go build -tags noui ./...`
# needs neither Node nor a built dash/dist. Use it for a Go-only change you do
# not want to pay the SPA build for — the binary it produces still has
# `stratum dash`, which says on startup that it has no UI.
TAGS        ?=
GO_TAGS     := $(if $(strip $(TAGS)),-tags $(strip $(TAGS)),)

# The SPA is a build input unless it has been tagged out. One expression shared
# by `build` and `test`, so the two can never disagree about whether dash/dist
# has to be there.
#
# `smoke` deliberately does NOT consult it — see the note in that recipe. Its
# assertion is about the artifact, and TAGS is the variable that is wrong in the
# case it exists to catch, so keying it here would reintroduce the bypass it was
# written to close. Do not "simplify" the two into one.
#
# Commas become spaces first: `go build -tags a,b` is the idiomatic spelling, so
# TAGS=noui,foo has to count as tagged out. $(filter) splits on whitespace only,
# and without the subst it would see one word "noui,foo", match nothing, and
# build a dist that the noui embed then ignores.
comma       := ,
NEEDS_DASH  := $(if $(filter noui,$(subst $(comma), ,$(TAGS))),,dash)

# EMBEDDED_BINS=none builds the bare binary with no payload: components are
# expected at the staging path or on PATH. Used for dev and OS packages.
EMBEDDED_BINS ?= all

# The pause image containerd is told to use. containerd's built-in default points
# at registry.k8s.io, which an airgapped node cannot reach and which nothing at
# runtime is allowed to reference; this is the mirrored pin from the payload.
# Tag, not digest: the payload registers images by name:tag when ctr imports the
# archive, so a digest ref would not resolve locally. verify-payload is what
# holds the digest side honest.
SANDBOX_IMAGE := $(shell tools/mirror-images.sh --list | grep '/pause:' | sed 's/@.*//')

# etcd's image tag carries registry.k8s.io's image-build revision suffix
# (the "-N"), which only build/images.txt knows; derive it from the pin the
# same way SANDBOX_IMAGE is derived so the two can never disagree.
ETCD_IMAGE := $(shell tools/mirror-images.sh --list | grep '/etcd:' | sed 's/@.*//')

# Every other image the binary stamps comes off the same list, the same way:
# the control-plane static pods (kube-apiserver, kube-controller-manager,
# kube-scheduler, kube-vip) and the image the node Jobs nsenter from (busybox,
# stamped as version.NsenterImage). build/images.txt is the pin surface for
# images, so a tag spelled here from build/versions.mk would be a second pin
# of the same tag, free to drift from the payload an airgap cluster runs; the
# mirrored ref is the name that payload registers, so a stamp derived from it
# cannot name a tag the bundle does not carry.
KUBE_APISERVER_IMAGE := $(shell tools/mirror-images.sh --list | grep '/kube-apiserver:' | sed 's/@.*//')
KUBE_CONTROLLER_MANAGER_IMAGE := $(shell tools/mirror-images.sh --list | grep '/kube-controller-manager:' | sed 's/@.*//')
KUBE_SCHEDULER_IMAGE := $(shell tools/mirror-images.sh --list | grep '/kube-scheduler:' | sed 's/@.*//')
KUBE_VIP_IMAGE := $(shell tools/mirror-images.sh --list | grep '/kube-vip:' | sed 's/@.*//')
NSENTER_IMAGE := $(shell tools/mirror-images.sh --list | grep '/busybox:' | sed 's/@.*//')

# CoreDNS and local-path stamp bare tags, not refs: their bundled manifests
# carry the registry and the image name themselves and substitute only the
# version (internal/addons/manifests). local-path's upstream tag carries the
# v its manifest spells for it, so the last substitution takes that v off.
COREDNS_VERSION := $(shell tools/mirror-images.sh --list | grep '/coredns:' | sed 's/@.*//;s/.*://')
LOCAL_PATH_VERSION := $(shell tools/mirror-images.sh --list | grep '/local-path-provisioner:' | sed 's/@.*//;s/.*://;s/^v//')

# kube-vip's version fact (internal/version.KubeVipVersion, what `stratum
# version --components` reports) is the tag of the image the payload carries, spelled
# bare like CoreDNS's: the ref stamped as KubeVipImage and the version the
# binary reports must be the same kube-vip, and both come off the image list —
# the one pin surface for images — rather than a second copy free to drift.
KUBE_VIP_VERSION := $(shell tools/mirror-images.sh --list | grep '/kube-vip:' | sed 's/@.*//;s/.*://;s/^v//')

# cilium's version fact (internal/version.CiliumVersion, what `stratum version
# --components` reports, and the tag tools/gen-cilium-manifest.sh renders the
# embedded manifest at) is the tag of the image the payload carries, spelled
# bare like CoreDNS's. It comes off the image list like every other image
# fact, not from a cilium_version copy in build/versions.mk held to these tags
# by a comment, so bumping the tags above is bumping the version.
CILIUM_VERSION := $(shell tools/mirror-images.sh --list | grep '/cilium:' | sed 's/@.*//;s/.*://;s/^v//')

# Every derivation above must find its entry. A grep that matches nothing
# expands EMPTY, and an empty value then links into a Go string stamp, the
# empty value overwriting the deliberate `unknown` sentinel internal/version
# carries —
# so a renamed or dropped pin builds clean and first shows up as a node Job
# pulling an empty container image mid CA-rotation or upgrade on an air-gapped
# cluster. Refuse to link here, as the TARGET_ARCH guards above refuse a
# silently wrong staging path. SANDBOX_IMAGE and ETCD_IMAGE are listed with
# the derivations above because the same failure reads identically through
# them: every image stamp GO_LDFLAGS carries must resolve against
# build/images.txt, whichever line derives it.
stamp_vars := SANDBOX_IMAGE ETCD_IMAGE KUBE_APISERVER_IMAGE \
	KUBE_CONTROLLER_MANAGER_IMAGE KUBE_SCHEDULER_IMAGE KUBE_VIP_IMAGE NSENTER_IMAGE \
	COREDNS_VERSION LOCAL_PATH_VERSION KUBE_VIP_VERSION CILIUM_VERSION
empty_stamps := $(foreach v,$(stamp_vars),$(if $(strip $($(v))),,$(v)))
ifneq ($(strip $(empty_stamps)),)
$(error these image stamps resolved to nothing from tools/mirror-images.sh --list (build/images.txt): $(empty_stamps))
endif

# The in-cluster dashboard image is built here, not mirrored, so it is named by
# the release rather than pinned in build/images.txt.
DASH_IMAGE  := $(image_registry)/stratum-dash:$(subst +,-,$(VERSION))

# Recursive (=) so BUILD_ID is taken at link time, after dash/dist is built.
GO_LDFLAGS = -s -w \
  -X github.com/ctrliq/stratum/internal/version.BuildID=$(BUILD_ID) \
  $(ENGINE_LDFLAGS)

# The engine's stamping, published per release (engine-ldflags) so an importer of
# pkg/cli links the same pins. BuildID is not in it: it names one binary. Every
# stamp addresses an engine package — including the five component versions, which
# internal/version carries — so an importer needs no main-package vars of its
# own; WithComponentVersions (deprecated) remains an override for one that stamps
# them anyway. CoreDNS's display stamp and its deployment stamp (internal/addons)
# stay single-sourced on COREDNS_VERSION.
ENGINE_LDFLAGS = \
  -X github.com/ctrliq/stratum/internal/version.StratumVersion=$(VERSION) \
  -X github.com/ctrliq/stratum/internal/version.K8sVersion=v$(kubernetes_version) \
  -X github.com/ctrliq/stratum/internal/version.EtcdVersion=v$(etcd_version) \
  -X github.com/ctrliq/stratum/internal/version.NsenterImage=$(NSENTER_IMAGE) \
  -X github.com/ctrliq/stratum/internal/version.EtcdImage=$(ETCD_IMAGE) \
  -X github.com/ctrliq/stratum/internal/version.KubeAPIServerImage=$(KUBE_APISERVER_IMAGE) \
  -X github.com/ctrliq/stratum/internal/version.KubeControllerManagerImage=$(KUBE_CONTROLLER_MANAGER_IMAGE) \
  -X github.com/ctrliq/stratum/internal/version.KubeSchedulerImage=$(KUBE_SCHEDULER_IMAGE) \
  -X github.com/ctrliq/stratum/internal/version.KubeVipImage=$(KUBE_VIP_IMAGE) \
  -X github.com/ctrliq/stratum/internal/addons.ImageRegistry=$(image_registry) \
  -X github.com/ctrliq/stratum/internal/addons.DashImage=$(DASH_IMAGE) \
  -X github.com/ctrliq/stratum/internal/runtime.SandboxImage=$(SANDBOX_IMAGE) \
  -X github.com/ctrliq/stratum/internal/version.ContainerdVersion=$(containerd_version) \
  -X github.com/ctrliq/stratum/internal/version.RuncVersion=$(runc_version) \
  -X github.com/ctrliq/stratum/internal/version.KubeVipVersion=$(KUBE_VIP_VERSION) \
  -X github.com/ctrliq/stratum/internal/version.CiliumVersion=$(CILIUM_VERSION) \
  -X github.com/ctrliq/stratum/internal/version.CorednsVersion=$(COREDNS_VERSION) \
  -X github.com/ctrliq/stratum/internal/addons.CorednsVersion=$(COREDNS_VERSION) \
  -X github.com/ctrliq/stratum/internal/addons.LocalPathVersion=$(LOCAL_PATH_VERSION)

.PHONY: help
help:
	@echo "Stratum build targets (version: $(VERSION), target: $(TARGET_OS)/$(TARGET_ARCH))"
	@echo
	@echo "  make components            Build every component ($(COMPONENTS))"
	@echo "  make component-<name>      Build one component"
	@echo "  make dash                  Build the dashboard SPA into dash/dist"
	@echo "  make build                 Build the bare stratum binary (TAGS=noui skips the SPA)"
	@echo "  make images                Stage the OCI images from build/images.txt"
	@echo "  make dash-image            Build the in-cluster dashboard image into the image staging dir"
	@echo "  make package               Build + append the payload (the release artifact)"
	@echo "  make payload-asset         After package: carve its payload into dist/stratum-payload.zip"
	@echo "  make engine-ldflags        Write the engine's -X stamps to dist/stratum-ldflags.txt"
	@echo "  make lint                  The gate list: pins, patches, fences, docs CLI, self-tests"
	@echo "  make licenses              Check the licenses of the Go and npm code stratum ships"
	@echo "  make test                  Go tests"
	@echo "  make smoke                 Run the packaged binary in a clean container"
	@echo "  make inttest               Run the integration suite in containers"
	@echo "  make verify-payload        Payload image set == build/images.txt, by digest"
	@echo "  make mirror-images         Copy the pinned runtime images into $(image_registry)"
	@echo "  make verify-reproducible COMPONENT=runc"
	@echo "  make build-info sbom sign  Provenance artifacts"
	@echo "  make fences fences-check   Regenerate / verify Renovate version fences"
	@echo "  make days-behind schedule cves   Upstream freshness and signals"
	@echo "  make print-version print-cilium-version check-version"

# --- components ---------------------------------------------------------------

# component-% is deliberately NOT .PHONY: GNU make skips pattern-rule search
# for phony targets, so declaring it phony makes it a no-op.
.PHONY: components
components:
	$(MAKE) -C build/components all TARGET_ARCH=$(TARGET_ARCH)

component-%:
	$(MAKE) -C build/components $* TARGET_ARCH=$(TARGET_ARCH)

# --- the dashboard ------------------------------------------------------------

# Builds the SPA into dash/dist, which dash/embed_ui.go embeds. A prerequisite
# of `build` and `test` (see NEEDS_DASH), so `make package` gets it too — the
# packaged binary is not the release artifact without it.
#
# Phony, not a file rule on dash/dist: the inputs are dash/'s whole source tree
# plus a lockfile pnpm resolves, and a stale-by-mtime rule over that is the kind
# of thing that ships an old bundle once and is never trusted again. vite's
# build is under a second; the install is what costs, and pnpm skips that itself
# when node_modules already matches the lockfile.
#
# The guard is here rather than left to a `pnpm: command not found`, because the
# failure a box without Node hits is `make package` — several minutes in, on a
# line that does not mention Node. `mise install` provides both (mise.toml).
#
# It tests BOTH commands, not just pnpm: a box with a stale pnpm shim and no
# node would otherwise walk past a guard whose whole message is about Node and
# die inside pnpm's launcher instead.
# dash-bundle is the build alone, for `make licenses`, which needs the bundle
# and its package list but not the tests the dash CI job already runs. The
# tests build into their own temp directory, so they may run after the build.
.PHONY: dash dash-bundle
dash: dash-bundle
	cd dash && pnpm test

dash-bundle:
	@missing=; \
	  command -v node >/dev/null || missing="node"; \
	  command -v pnpm >/dev/null || missing="$${missing:+$$missing and }pnpm"; \
	  if [ -n "$$missing" ]; then \
	    echo "make dash: $$missing not found — the dashboard SPA needs Node $(node_version) and pnpm $(pnpm_version)." >&2; \
	    echo "           Install them with 'mise install'." >&2; \
	    if [ -z "$(filter dash dash-bundle,$(MAKECMDGOALS))" ]; then \
	      echo "           Or build without the UI: make $(MAKECMDGOALS) TAGS=noui" >&2; \
	    else \
	      echo "           (TAGS=noui does not help here: this target IS the SPA build." >&2; \
	      echo "            It is 'make build'/'make test'/'make package' that skip it.)" >&2; \
	    fi; \
	    exit 1; \
	  fi
	cd dash && pnpm install --frozen-lockfile && pnpm run build
	# vite exiting 0 is not the same as vite having emitted an entry point, and
	# index.html is what `//go:embed all:dist` must contain for `stratum dash`
	# to serve anything. Asserted here so every caller is covered by one
	# check: `build`, `test` and `package` through NEEDS_DASH, and release.yml's
	# dash-publish, which needs only `meta` and is therefore gated by nothing
	# else before it publishes @ctrliq/stratum-dash.
	@test -s dash/dist/index.html || { \
	  echo "make dash: pnpm run build left no dash/dist/index.html." >&2; \
	  echo "           The bundle has no entry point; nothing would be served." >&2; \
	  exit 1; }

# --- the public documentation site --------------------------------------------

# docs/ is the user-facing site (MkDocs Material). The target is what a
# developer runs and what CI runs, so the two cannot drift apart — the same
# reason `dash` is a target rather than three pnpm lines in a workflow.
#
# The venv lives inside docs/ and is gitignored, so this leaves nothing
# on the machine outside the repo and needs no global pip install.
DOCS_DIR := docs
DOCS_VENV := $(DOCS_DIR)/.venv

.PHONY: docs docs-serve docs-venv
docs-venv:
	@command -v python3 >/dev/null || { \
	  echo "make docs: python3 not found — the docs site needs it to build." >&2; \
	  exit 1; }
	@python3 -m venv $(DOCS_VENV)
	@$(DOCS_VENV)/bin/pip install --quiet --disable-pip-version-check \
	  "mkdocs==$(mkdocs_version)" "mkdocs-material==$(mkdocs_material_version)"

# --strict fails on a broken link or a bad nav entry, but says nothing about a
# nav that quietly lost a page, so the count is asserted too. Change it
# deliberately, in both the test and the message below.
docs: docs-venv
	cd $(DOCS_DIR) && .venv/bin/mkdocs build --strict
	@n=$$(find $(DOCS_DIR)/site -name index.html | wc -l | tr -d ' '); \
	  [ "$$n" = 21 ] || { \
	    echo "make docs: built $$n pages, expected 21." >&2; \
	    echo "           A page was added or dropped — update the count if that was intended." >&2; \
	    exit 1; }

# Local preview. Not used by CI.
docs-serve: docs-venv
	cd $(DOCS_DIR) && .venv/bin/mkdocs serve

# --- the binary ---------------------------------------------------------------

.PHONY: build
build: $(NEEDS_DASH)
	@mkdir -p $(DIST)
	# Remove first: go build treats an existing output as up-to-date when the
	# inputs are unchanged, which would leave a previously appended payload
	# stuck on the "bare" binary.
	@rm -f $(BIN)
	CGO_ENABLED=0 GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) \
	  go build $(GO_TAGS) -trimpath -ldflags '$(GO_LDFLAGS)' -o $(BIN) ./cmd/$(stratum_name)
	@echo "built $(BIN) ($(VERSION), $(TARGET_OS)/$(TARGET_ARCH), payload=$(EMBEDDED_BINS))"

# The dashboard server alone, as the single-layer image the dashboard stack runs.
.PHONY: dash-image
dash-image: $(NEEDS_DASH)
	@mkdir -p $(IMGSTAGE)
	@rm -f $(DASH_BIN)
	CGO_ENABLED=0 GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) \
	  go build $(GO_TAGS) -trimpath -ldflags '$(GO_LDFLAGS)' -o $(DASH_BIN) ./cmd/stratum-dash
	go run ./tools/ociimage -bin $(DASH_BIN) -ref $(DASH_IMAGE) -os $(TARGET_OS) -arch $(TARGET_ARCH) -out $(DASH_IMAGETAR)
	@tools/verify-payload.sh --dash-tar $(DASH_IMAGETAR) $(DASH_IMAGE)

# The ZIP-append trick: a zip reader finds the central directory by scanning
# backwards from EOF, and an ELF loader ignores trailing bytes, so one file is
# both a working executable and a working archive. It is what
# makes a single-artifact control plane possible.
#
# ponytail: linux targets only. Appending to a Mach-O invalidates the code
# signature that arm64 macOS requires, so a darwin build must stay no-payload.
#
# The zip is scratch, and it holds a second uncompressed copy of the ~1G image
# tar. Nothing downstream reads it once it is appended, so it is removed here:
# keeping it alive alongside the staging tar and the binary put three copies of
# the same image set on a runner that has ~14G free.
.PHONY: package
package: build $(if $(filter none,$(EMBEDDED_BINS)),,images dash-image)
ifeq ($(EMBEDDED_BINS),none)
	@echo "EMBEDDED_BINS=none: skipping payload; $(BIN) will resolve components from the staging dir or PATH"
else
	@test -d $(BINSTAGE) || { echo "no $(BINSTAGE) — run 'make components' first" >&2; exit 1; }
	@test -f $(IMAGETAR) || { echo "no $(IMAGETAR) — run 'make images' first" >&2; exit 1; }
	@rm -f $(ZIP)
	cd $(STAGING) && zip -qX $(CURDIR)/$(ZIP) $(addprefix bin/,$(PAYLOAD))
	@test -f $(DASH_IMAGETAR) || { echo "no $(DASH_IMAGETAR) — run 'make dash-image' first" >&2; exit 1; }
	cd $(STAGING) && zip -qX0 $(CURDIR)/$(ZIP) images/$(notdir $(IMAGETAR)) images/$(notdir $(DASH_IMAGETAR))
	cat $(ZIP) >> $(BIN)
	@rm -f $(ZIP)
	@echo "packaged $(BIN) — $$(du -h $(BIN) | cut -f1), airgap-complete:"
	@echo "    $(words $(PAYLOAD)) binaries + $$(grep -cvE '^[[:space:]]*(#|$$)' build/images.txt) images"
endif

# The release's importer assets: the packaged binary's payload, carved off it
# byte for byte, and the engine's -X stamping, one flag per line.
PAYLOAD_ASSET := $(DIST)/$(stratum_name)-payload.zip
LDFLAGS_ASSET := $(DIST)/$(stratum_name)-ldflags.txt

.PHONY: payload-asset engine-ldflags
payload-asset:
	@test -f $(BIN) || { echo "no $(BIN) — run 'make package' first" >&2; exit 1; }
	tools/payload-asset.sh $(BIN) $(PAYLOAD_ASSET)

engine-ldflags:
	@mkdir -p $(DIST)
	@printf -- '-X %s\n' $(filter-out -X,$(ENGINE_LDFLAGS)) > $(LDFLAGS_ASSET)
	@echo "wrote $(LDFLAGS_ASSET) ($$(wc -l < $(LDFLAGS_ASSET) | tr -d ' ') stamps)"

# There is no separate airgap artifact: the images are part of the payload, so
# the one binary installs with no registry access at all. Staged as a plain OCI
# archive (not compressed) because the layers inside are already compressed and
# `ctr images import` consumes a tar directly.
#
# tools/build-image-archive.sh does the assembly with crane, not
# `docker pull --platform … && docker save` — see its header for the full why.
# The short version: the pins are multi-arch manifest-list digests and the node
# resolves each image by that list digest, but `docker save` from the containerd
# image store can preserve the list digest at the top while silently omitting
# some of the pulled platform's own config/layer blobs, shipping a tar that
# only fails at runtime with `ctr: content digest …: not found`. crane fetches
# every blob by digest and the script re-attaches the list manifest by its own
# bytes, so the archive is complete and self-consistent by construction — the
# same reason tools/mirror-images.sh copies with crane, not docker. Mirror-first
# with an upstream fallback (identical digest) and transient-only retry are
# preserved in the script.
.PHONY: images
# Verify the staged archive the moment it is built. The assembler is careful, but
# the airgap claim rests on completeness, so gate it rather than trust it:
# verify-payload --tar walks each manifest tree down to the config/layer blobs
# and fails the build here instead of at runtime as `ctr: content digest …: not
# found` stalling kubelet.
images: $(IMAGETAR)
	@tools/verify-payload.sh --tar $(IMAGETAR)
$(IMAGETAR): build/images.txt build/versions.mk tools/mirror-images.sh tools/build-image-archive.sh tools/crane-retry.sh
	@mkdir -p $(IMGSTAGE)
	tools/build-image-archive.sh $(TARGET_OS) $(TARGET_ARCH) $@

# --- checks -------------------------------------------------------------------

.PHONY: lint lint-shell licenses test smoke
# bash -n + shellcheck over every tracked shell script. Its own target so the
# dev loop can run just this; the lint target runs it as its prerequisite.
# CI relies on the runner's shellcheck, which is unpinned — the script prints
# its version and fails if it is missing rather than skipping.
lint-shell:
	tools/lint-shell.sh

lint: lint-shell
	tools/lint-versions.sh
	tools/test-lint-versions.sh
	tools/lint-kube-images.sh
	# The node-file write rule: every node file is written by its owning
	# package (internal/version's owning-package record) through the shared
	# atomic writer. The guard (tools/nodefilewrites) scans the whole repo, so
	# a new raw os.WriteFile or clientcmd.WriteToFile in a writer package
	# fails here, with the file:line and the rule in the message.
	go run ./tools/nodefilewrites .
	# The internal import-layering rule: every internal package imports only
	# packages of its own or a strictly lower layer, by the purpose table in
	# tools/importlayers/layers.txt. The guard (tools/importlayers) scans the
	# whole repo, so a new upward internal import fails here with the file:line
	# and both layers; a deliberate one is a reviewed exception with its reason
	# in tools/importlayers/exceptions.txt, and a stale exception fails too.
	go run ./tools/importlayers .
	# The committed Cilium manifest still matches a fresh render at the pinned
	# cilium version: a pin bump that forgets the regeneration, or a hand edit
	# of the generated file, fails here rather than shipping a manifest that
	# disagrees with build/images.txt. This gate renders helm in a container
	# through docker, so where docker is missing it is skipped — CI's runner
	# always has it.
	@if docker version >/dev/null 2>&1; then \
	  tools/gen-cilium-manifest.sh --check; \
	else \
	  echo "lint: docker not available — skipping the Cilium manifest render check (CI runs it)"; \
	fi
	tools/test-gen-cilium-manifest.sh
	# The version/revision pair, against the REAL manifest: the only other job
	# that compares them is the component build, which is the slowest in the
	# gauntlet and does not run on a release-branch push at all.
	tools/lint-revisions.sh
	# go_version against the pinned tag's .go-version. Exit 3 is "upstream
	# unreachable": the gate has already warned, and it is not a finding.
	tools/lint-go-version.sh || [ $$? -eq 3 ]
	tools/lint-patches.sh
	# The docs describe a CLI that exists: every stratum verb and flag in a
	# code fence or inline span is one the binary defines. All tracked *.md,
	# not the diff — a flag renamed in pkg/cli strands a page that touches
	# nothing under docs/. The scoping rules live in the script's header.
	tools/lint-docs-cli.sh
	# Exit 3 is "rate limited, inconclusive", the same semantics as
	# lint-go-version above: an anonymous run can hit GitHub's 60/hr limit,
	# and the script has already printed a RATELIMIT line for every patch
	# it could not reach. Not a finding. The local run is the anonymous
	# one; CI hands this target the job's GITHUB_TOKEN.
	tools/check-droppable-patches.sh || [ $$? -eq 3 ]
	# The committed Renovate fences against upstream compatibility data —
	# the component support matrix, checked rather than remembered. Exit 3
	# is "rate limited, inconclusive" (tools/upstream_sources.py's exit code
	# for exactly that), tolerated like the network gates above.
	tools/gen-fences.sh --check || [ $$? -eq 3 ]
	tools/test-fences.sh
	tools/test-lint-shell.sh
	tools/test-lint-licenses.sh
	tools/test-lint-tracker-keys.sh
	tools/test-cve-watch.sh
	tools/test-check-supervisor-restart.sh
	tools/test-ostests-provision.sh
	tools/test-ostests-skew.sh
	tools/test-ostests-skew-verdict.sh
	tools/test-ostests-sweep.sh
	tools/test-ostests-conformance.sh
	tools/test-ostests-backends.sh
	tools/test-ostests-matrix.sh
	tools/test-inttest-header.sh
	tools/test-check-lifecycle-prune.sh
	tools/test-version.sh
	tools/test-build-id.sh
	tools/test-revisions.sh
	tools/test-go-version.sh
	tools/test-dev-release.sh
	# This self-test drives a make of its own and asserts on that make's
	# stdout, so the parent make's context is scrubbed: a make run under a
	# make (MAKELEVEL in its environment) prepends "Entering directory"
	# banners to exactly the output the test counts words in. Bare — a CI
	# step, a dev loop — it sees no parent make and stays hermetic.
	env -u MAKELEVEL -u MAKEFLAGS tools/test-host-arch.sh
	tools/test-verify-payload.sh
	tools/test-payload-asset.sh
	tools/test-push-importer-assets.sh
	tools/test-mirror-images.sh
	tools/test-stamp-invalidation.sh
	# The N-3 pin gate against the REAL manifest, not a fixture: skew_kubelet_version
	# carries no renovate annotation (it cannot — see the pin), so a kubernetes_version
	# minor bump that forgets it has to fail here, on the PR, rather than turning the
	# weekly skew suite into a no-skew suite that passes.
	tools/ostests/run-skew.sh --check-pins
	# A config Renovate rejects stops the bot's updates silently, invisible
	# for weeks unless a gate catches it. Pinned from build/versions.mk like
	# everything else: an unpinned npx can hand CI an older validator
	# major than the local one, and the two disagree about which options
	# exist — a validator nobody chose fails the build for reasons that have
	# nothing to do with the config.
	#
	# `A && B || C` also runs C when the VALIDATOR fails, printing "npx not
	# installed" for a malformed renovate.json5 and exiting 0. Keep the guard
	# separate: it tests only for npx; the validator's own exit status is what
	# make sees.
	# One recipe line: a separate guard line's `exit 0` leaves only that line's
	# shell, so the validator would run anyway.
	# The downloaded validator needs no GitHub credentials. Keep the API
	# gates authenticated while stripping both token variables from npx.
	@if command -v npx >/dev/null; then \
	  env -u GITHUB_TOKEN -u GH_TOKEN npx --yes --package renovate@$(renovate_version) renovate-config-validator --strict renovate.json5; \
	else \
	  echo "lint: npx not installed — skipping renovate config validation (mise install)"; \
	fi
	# No tracker keys or internal links anywhere in the tree: readers outside
	# the team cannot open them, so a comment states its reason instead of
	# citing a ticket.
	tools/lint-tracker-keys.sh

# The license gate: the Go packages linked into ./cmd/stratum for linux/amd64
# and linux/arm64, and the npm packages vite bundles into dash/dist, against
# the license allowlist. dash-bundle builds the dashboard, which the Go run's
# build embeds and whose build writes the npm list the gate reads; under
# TAGS=noui there is no dashboard, and the gate skips its npm check. TAGS is
# passed explicitly: make exports a variable to a recipe only when it came from
# the command line or the environment, never a value this Makefile sets itself.
licenses: $(if $(NEEDS_DASH),dash-bundle)
	TAGS='$(strip $(TAGS))' tools/lint-licenses.sh

# gofmt is a gate, not a suggestion: `go vet` does not check formatting, so
# without this the tree drifts and every later diff carries reformatting noise
# that hides the real change. Prints the offenders — `gofmt -w ./cmd ./internal
# ./pkg ./tools` fixes them.
test: $(NEEDS_DASH)
	@unformatted=$$(gofmt -l ./cmd ./internal ./pkg ./tools); \
	  [ -z "$$unformatted" ] || { echo "gofmt: not formatted:"; echo "$$unformatted"; exit 1; }
	go vet $(GO_TAGS) ./...
	# -timeout 25m: the default per-package budget is 10m, and the CPU-heavy
	# suites (internal/certs, internal/pki: RSA keygen, x509 paths) need more
	# than that on the two-core CI runners. The flag bounds the whole binary,
	# so the budget stays per package.
	go test -timeout 25m $(GO_TAGS) ./...
	# tools/licensecheck is its own module, so ./... above stops at its
	# go.mod and never reaches it.
	cd tools/licensecheck && go vet ./... && go test ./...
	# Helm's own pkg/kube suite (about 30s), against the module versions THIS
	# module resolves: the operator's installs, upgrades, uninstalls and
	# rollbacks all go through it, and stratum's k8s.io and controller-runtime
	# pins are newer than the ones Helm v3 tested it with.
	go test helm.sh/helm/v3/pkg/kube
	tools/check-cli-api-boundary.sh

# envtest runs internal/mcp/kube against a REAL kube-apiserver and etcd — the
# only way to test discovery, RESTMapper resolution and short names, none of
# which we define: the apiserver does, and a fake would just encode today's
# guess about it.
#
# The control-plane binaries are ~170MB and are NOT a build dependency, so
# they are fetched on demand rather than by `make test`. Without them the
# envtest cases skip and the rest of the suite still runs, which is what a
# contributor who is not touching this package wants. CI fetches them, so the
# gate is real there.
#
# The version follows kubernetes_version, so the apiserver under test is the
# one Stratum ships — but by MINOR, not by exact patch. envtest publishes its
# control-plane archives per patch release and they lag behind Kubernetes
# itself: a patch bump here can leave `setup-envtest use` with nothing
# to fetch on any platform, which fails this target everywhere including CI.
# The control plane a test runs against does not have to be the exact patch we
# ship, only the same minor, so ask for the newest patch of that minor and let
# setup-envtest resolve it. $(basename) trims the patch component.
#
# setup-envtest itself is a `tool` directive in go.mod rather than a pin here
# — that keeps it under renovate's gomod manager and out of lint-versions.sh's
# way.
ENVTEST_DIR ?= $(CURDIR)/.envtest
envtest_version = $(basename $(kubernetes_version)).x

# Every package with envtest cases. pkg/cli is on the list because the MCP
# protocol tests drive real tools against the real API server from
# there — leaving it off would make `make test-envtest` report green while
# skipping most of what envtest is here to cover. internal/operator is on the
# list so the HelmChart requester-stamp and defaulting cases run against the
# real control plane rather than only fakes.
ENVTEST_PACKAGES ?= ./internal/mcp/... ./pkg/cli/... ./internal/operator/...

.PHONY: envtest-assets
envtest-assets:
	@go tool setup-envtest use $(envtest_version) --bin-dir $(ENVTEST_DIR) -p path

# The MCP server is the one place in this tree with concurrency a test can
# actually provoke: handlers run off the one stdio transport, and the
# destructive tools serialize on the operation lock (opMu, the destructive
# tools' sole exclusion). A regression that lets two operation bodies run
# at once races on the shared state those bodies touch rather than merely
# interleaving output.
#
# An overlap flag only fires when the scheduler happens to run the bodies
# together, and a buggy version passes that sampling often enough to be
# useless; the race detector flags the unsynchronized access itself. -race
# is what turns the concurrency case into a real one.
#
# Scoped to these packages rather than added to `make test`: turning the
# detector on for the whole tree is a repo-wide runtime decision, not
# something the MCP server should make on everyone's behalf.
RACE_PACKAGES ?= ./internal/mcp/...

.PHONY: test-race
test-race:
	go test $(GO_TAGS) -race -count=1 $(RACE_PACKAGES)

.PHONY: test-envtest
test-envtest:
	@assets=$$(go tool setup-envtest use $(envtest_version) --bin-dir $(ENVTEST_DIR) -p path) && \
	  echo "KUBEBUILDER_ASSETS=$$assets" && \
	  KUBEBUILDER_ASSETS=$$assets go test $(GO_TAGS) -count=1 $(ENVTEST_PACKAGES)

# Runs the release artifact the way a user would: a clean container with nothing
# installed, no volume write-back into the repo.
#
# pipefail is load-bearing. Without it a pipeline's exit status is its
# LAST command's, so `cmd | head` reports head's success and set -e never sees
# the failure — a check of a missing staged binary would print "not found"
# here and pass.
#
# No `head` is fed by a pipe in this recipe, for two different reasons that
# are easy to conflate. A missing binary in `cmd | head` fails under pipefail
# with 127, not SIGPIPE: it writes nothing, head reads EOF and exits 0, and
# pipefail surfaces the 127 that set -e then acts on.
#
# SIGPIPE is the separate hazard. `cmd | head -N` takes 141 only when cmd issues
# another write after head has already exited, so it turns on how cmd buffers
# rather than on a line count. In this image: below about one stdio
# flush it never fires; in a narrow band around it (~1.9KB) it fires on some
# runs and not others, and differently for different producers at the same size;
# past ~8KB it fires every time. So `| head` under pipefail is either reliably
# fatal or — worse, because it reads as a flake — intermittently fatal,
# depending on the command. `tar -tf` over the 491MB image archive is in the
# RELIABLE zone, the one line here at risk. The two version checks sit two
# orders of magnitude below the band. Redirecting first sidesteps both hazards
# and leaves the command's own exit status the thing set -e judges. The pipes
# that remain (| sort, | tr) are safe: their readers consume all input.
#
# Why the payload comparison earns its place: `zip` exits 0 when a file it was
# named is missing, and package passes -q, which suppresses even the warning
# (Makefile:133). So a build that fails to produce a PAYLOAD-declared binary
# packages and ships silently, and this diff is the only thing between that and
# a signature. It cannot catch the opposite — a binary dropped from a component
# manifest — because package names members from $(PAYLOAD) rather than globbing
# staging, so both sides of the diff move together; see the PAYLOAD comment at
# the top of this file.
#
# The re-stage check compares inode, size and mtime across two stage calls.
# Permissions prove nothing: the container is uid 0 with CAP_DAC_OVERRIDE, so
# root writes into a 555 directory freely, and a re-extract can still print
# "extraction was skipped" and pass. extract() is CreateTemp+Rename, so a real
# re-extract always changes the inode.
#
# ls -A, not ls: extract() writes .<name>.tmp-* into the destination before
# renaming, and busybox ls hides dotfiles, so a half-written member would be
# invisible to both the listing and the diff.
#
# The version greps are the only place anything asserts that a staged binary IS
# the pinned version. verify-payload covers the image set by digest; the ldflags
# in `version --components` only report the pin back. A component built at the
# wrong version would otherwise pass every gate and be signed.
#
# Backport note: main no longer stages etcd as a binary — the control plane
# runs as kubelet-managed static pods, which dropped etcd from etcd_binaries —
# but release-1.35 and release-1.36 predate that move and still stage etcd, so
# the etcd->etcdctl swap must NOT be carried over. Everything else here should
# be: their etcd check is masked by the same missing pipefail, so carrying that
# half makes their check real, and their PAYLOAD is 12 binaries to main's 8 —
# ten of which nothing executes. `etcd --version` exits 0 with a few lines of
# output — sound as an assertion, and far enough below the SIGPIPE band that
# pipefail will not turn it into a flaky gate.
smoke: verify-payload
	@test -f $(BIN) || { echo "no $(BIN) — run 'make package' first" >&2; exit 1; }
	# The dashboard is part of the artifact, so it is asserted like the rest of it
	# rather than trusted. Without this, the one thing that produces a
	# release binary with no UI — a `TAGS=noui` that reaches a release workflow —
	# is invisible: every job stays green, `stratum dash` serves its placeholder,
	# and nobody finds out until someone opens it. -a because the binary is not
	# text; the marker is dash/index.html's own root div, which `//go:embed
	# all:dist` carries verbatim and the SPA handler serves.
	#
	# Deliberately NOT guarded by $(NEEDS_DASH), which is derived from TAGS.
	# TAGS is the variable that is wrong in the scenario this exists to catch: a
	# workflow exporting TAGS=noui hands the same value to `package` and to
	# `smoke`, so a TAGS-keyed check would switch itself off precisely when the
	# no-UI artifact it should reject goes by. The assertion is about the
	# ARTIFACT, so it does not consult how the artifact was asked for.
	#
	# ALLOW_NOUI is the deliberate opt-out, for a local `make package TAGS=noui
	# && make smoke`. It is a separate variable on purpose: nothing in any
	# workflow sets it, and nothing should.
	@if [ -n "$(ALLOW_NOUI)" ]; then \
	  echo "dashboard: ALLOW_NOUI set — not asserting the SPA is in $(BIN)"; \
	else \
	  grep -aq '<div id="root">' $(BIN) || { \
	    echo "$(BIN) carries no dashboard: dash/index.html is not in the binary." >&2; \
	    echo "  A release artifact must embed the SPA. Was this built with TAGS=noui?" >&2; \
	    echo "  For a deliberate UI-less build, run: make smoke ALLOW_NOUI=1" >&2; \
	    exit 1; }; \
	  echo "dashboard: SPA embedded in $(BIN)"; \
	fi
	@echo "=== stratum version --components ==="
	docker run --rm --platform $(TARGET_OS)/$(TARGET_ARCH) \
	  -v "$$PWD/$(DIST):/dist:ro,z" alpine /dist/$(stratum_name) version --components
	@echo "=== staging, twice (second run must skip extraction) ==="
	docker run --rm --platform $(TARGET_OS)/$(TARGET_ARCH) \
	  -e EXPECTED_PAYLOAD="$(PAYLOAD)" \
	  -e EXPECT_K8S="$(kubernetes_version)" \
	  -e EXPECT_ETCD="$(etcd_version)" \
	  -v "$$PWD/$(DIST):/dist:ro,z" alpine sh -c '\
	    set -e; \
	    set -o pipefail; \
	    cp /dist/$(stratum_name) /tmp/stratum; \
	    /tmp/stratum stage --root /tmp/root >/dev/null; \
	    find /tmp/root/bin /tmp/root/images -maxdepth 1 -type f -exec stat -c "%n %i %s %Y" {} + | sort > /tmp/before; \
	    /tmp/stratum stage --root /tmp/root >/dev/null; \
	    find /tmp/root/bin /tmp/root/images -maxdepth 1 -type f -exec stat -c "%n %i %s %Y" {} + | sort > /tmp/after; \
	    if ! diff -u /tmp/before /tmp/after; then \
	      echo "the second stage re-extracted the payload instead of skipping" >&2; \
	      exit 1; \
	    fi; \
	    echo "second stage changed no inode, size or mtime: extraction was skipped"; \
	    ls -lA /tmp/root/bin /tmp/root/images; \
	    for t in /tmp/root/images/*.tar; do tar -tf "$$t"; done > /tmp/imglist; head -3 /tmp/imglist; \
	    printf "%s\\n" $$EXPECTED_PAYLOAD | sort > /tmp/want; \
	    ls -A /tmp/root/bin | sort > /tmp/got; \
	    if ! diff -u /tmp/want /tmp/got; then \
	      echo "the staged set is not the declared payload (see diff above)" >&2; \
	      exit 1; \
	    fi; \
	    echo "staged set equals the declared payload ($$(wc -l < /tmp/got | tr -d " ") binaries)"; \
	    /tmp/root/bin/etcdctl version > /tmp/etcdctl.out 2>&1; head -2 /tmp/etcdctl.out; \
	    grep -q "$$EXPECT_ETCD" /tmp/etcdctl.out || { \
	      echo "staged etcdctl does not report the pinned $$EXPECT_ETCD" >&2; cat /tmp/etcdctl.out >&2; exit 1; }; \
	    /tmp/root/bin/etcdutl version > /tmp/etcdutl.out 2>&1; head -2 /tmp/etcdutl.out; \
	    grep -q "$$EXPECT_ETCD" /tmp/etcdutl.out || { \
	      echo "staged etcdutl does not report the pinned $$EXPECT_ETCD" >&2; cat /tmp/etcdutl.out >&2; exit 1; }; \
	    /tmp/root/bin/kubectl version --client > /tmp/kubectl.out 2>&1; head -2 /tmp/kubectl.out; \
	    grep -q "v$$EXPECT_K8S" /tmp/kubectl.out || { \
	      echo "staged kubectl does not report the pinned v$$EXPECT_K8S" >&2; cat /tmp/kubectl.out >&2; exit 1; }'

# The airgap claim rests on one invariant: the payload's image set equals the
# pinned set in build/images.txt, by digest. It is a release gate; here it is
# a target.
# The integration suite. Each check is a file in tools/inttest/; the matrix generator
# turns the directory into CI jobs, so adding a test needs no workflow edit.
.PHONY: inttest inttest-matrix
inttest:
	@set -euo pipefail; \
	for s in $$(tools/inttest-matrix.sh | python3 -c \
	     'import json,sys; [print(j["script"]) for j in json.load(sys.stdin)["include"] if not j["pending"]]'); do \
	  tools/inttest-run.sh "$$s"; \
	done

inttest-matrix: ; @tools/inttest-matrix.sh

# Copying the pinned runtime images into the SKE namespace. A release gate, not
# a build step: the pipeline runs it before the images are needed, and `make
# images` falls back to upstream (same digest) when it has not run yet.
.PHONY: mirror-images verify-mirror
mirror-images:  ; tools/mirror-images.sh --push
verify-mirror:  ; tools/mirror-images.sh --verify

.PHONY: verify-payload
verify-payload:
	@test -f $(BIN) || { echo "no $(BIN) — run 'make package' first" >&2; exit 1; }
	DASH_REF=$(DASH_IMAGE) tools/verify-payload.sh $(BIN)

# verify-images checks the raw staged archive (before packaging) — the same
# completeness walk verify-payload does on the finished binary, run early so a
# broken `make images` fails at its source, not three steps later at runtime.
.PHONY: verify-images
verify-images:
	@test -f $(IMAGETAR) || { echo "no $(IMAGETAR) — run 'make images' first" >&2; exit 1; }
	tools/verify-payload.sh --tar $(IMAGETAR)

# Two clean builds of the same pins must produce identical bytes.
# Setting SOURCE_DATE_EPOCH is not enough; asserting it is the point.
.PHONY: verify-reproducible
verify-reproducible:
	@test -n "$(COMPONENT)" || { echo "usage: make verify-reproducible COMPONENT=runc" >&2; exit 1; }
	@set -euo pipefail; \
	first=$$(mktemp -d); second=$$(mktemp -d); \
	trap 'rm -rf $$first $$second' EXIT; \
	for round in 1 2; do \
	  echo "=== $(COMPONENT): clean build $$round/2 ==="; \
	  rm -f .stamps/linux-$(TARGET_ARCH)/$(COMPONENT)-*; \
	  $(MAKE) -s -C build/components $(COMPONENT) TARGET_ARCH=$(TARGET_ARCH) \
	    DOCKER_BUILD_FLAGS=--no-cache >/dev/null; \
	  dest=$$first; [ $$round = 2 ] && dest=$$second; \
	  for b in $($(COMPONENT)_binaries); do cp $(BINSTAGE)/$$b $$dest/; done; \
	done; \
	echo "=== comparing ==="; \
	for b in $($(COMPONENT)_binaries); do \
	  a=$$(shasum -a256 $$first/$$b | cut -d' ' -f1); \
	  c=$$(shasum -a256 $$second/$$b | cut -d' ' -f1); \
	  if [ "$$a" = "$$c" ]; then printf '  %-28s IDENTICAL %s\n' "$$b" "$${a:0:16}"; \
	  else printf '  %-28s DIFFERS   %s != %s\n' "$$b" "$${a:0:16}" "$${c:0:16}"; fail=1; fi; \
	done; \
	[ -z "$${fail:-}" ] || { echo "verify-reproducible: FAILED — build is not deterministic" >&2; exit 1; }; \
	echo "verify-reproducible: ok"

# --- version ------------------------------------------------------------------

# Upstream tracking.
.PHONY: fences fences-check days-behind schedule cves
fences:       ; tools/gen-fences.sh --write
fences-check: ; tools/gen-fences.sh --check
days-behind:  ; tools/days-behind.sh
schedule:     ; tools/upstream-schedule.sh
cves:         ; tools/cve-watch.sh

.PHONY: print-version print-cilium-version check-version
print-version: ; @echo $(VERSION)
# The version gen-cilium-manifest.sh renders and checks the embedded manifest
# at: the same CILIUM_VERSION the binary stamps, read through make rather than
# re-derived, so the render check cannot disagree with the stamped binary.
print-cilium-version: ; @echo $(CILIUM_VERSION)
check-version: ; @tools/version.sh --check

# --- provenance ---------------------------------------------------------------

.PHONY: build-info sbom sign
build-info:
	tools/build-info.sh $(DIST)

# One recipe line, not two: `exit 0` in a guard only leaves that line's shell,
# so a separate guard line would print "skipping" and then run the tool anyway.
sbom:
	@set -euo pipefail; \
	if ! command -v syft >/dev/null; then \
	  echo "sbom: syft not installed — skipping (install: mise install)"; exit 0; \
	fi; \
	for a in $(BIN); do \
	  [ -e "$$a" ] || continue; \
	  echo "syft $$a"; syft scan "file:$$a" -o spdx-json > "$$a.spdx.json"; \
	done

sign:
	@set -euo pipefail; \
	if ! command -v cosign >/dev/null; then \
	  echo "sign: cosign not installed — skipping (install: mise install; CI signs keylessly via GitHub OIDC)"; exit 0; \
	fi; \
	for a in $(BIN) $(PAYLOAD_ASSET) $(LDFLAGS_ASSET) $(DIST)/*.spdx.json; do \
	  [ -e "$$a" ] || continue; \
	  echo "cosign $$a"; cosign sign-blob --yes "$$a" --output-signature "$$a.sig" --output-certificate "$$a.pem"; \
	done

.PHONY: clean
clean:
	rm -rf $(DIST) staging .stamps build-info.json dash/dist dash/.bundle
	$(MAKE) -C build/components clean TARGET_ARCH=$(TARGET_ARCH)
