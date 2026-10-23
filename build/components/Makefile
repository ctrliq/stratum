# Per-component hermetic builds. Discovers component dirs, builds each in a
# container from build/versions.mk pins only, extracts the binaries into
# staging/linux-$(TARGET_ARCH)/bin/.
#
# Invoked from the repo root via `make component-<name>`; usable directly too.

include ../versions.mk

REPO_ROOT   := $(abspath ../..)
COMPONENTS  := $(notdir $(patsubst %/,%,$(dir $(wildcard */Dockerfile))))

# Native builds only (amd64 and arm64, both native, no
# emulation). Default to whatever the docker engine actually is.
#
# Kept character-for-character in step with the root Makefile's copy: the two
# derive the SAME staging path, so any disagreement stages binaries where the
# other half of the build will not look for them. tools/test-host-arch.sh
# asserts they agree under every daemon state.
#
# Probe the daemon, then decide. `docker version --format ... || uname -m` is
# wrong: an unreachable daemon writes a blank line to STDOUT *and* exits
# non-zero, so `||` fires and both outputs land here — $(shell) folds the
# newline to a space and STAMPS becomes the two words `.stamps/linux-` and
# `arm64`. This runs under /bin/sh, not bash: no SHELL := here.
HOST_ARCH   := $(shell \
	arch=; \
	if docker version >/dev/null 2>&1; then \
	  arch=$$(docker version --format '{{.Server.Arch}}' 2>/dev/null); \
	fi; \
	[ -n "$$(printf '%s' "$$arch" | tr -d '[:space:]')" ] || arch=$$(uname -m); \
	printf '%s' "$$arch" | sed 's/x86_64/amd64/;s/aarch64/arm64/')
TARGET_ARCH ?= $(HOST_ARCH)

# STAGING and STAMPS below both embed it; empty or split is a silently wrong
# path, not an error, so refuse it here the way the root Makefile does.
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

STAGING     := $(REPO_ROOT)/staging/linux-$(TARGET_ARCH)/bin
STAMPS      := $(REPO_ROOT)/.stamps/linux-$(TARGET_ARCH)
DOCKER      ?= docker

# STRATUM_VERSION is the suffix stamped into component binaries: the bare product
# name, not the full release version, so a hotfix re-release does not force a
# rebuild of every component.
STRATUM_VERSION := $(stratum_name)

.PHONY: all clean $(COMPONENTS) list
all: $(COMPONENTS)
list: ; @echo $(COMPONENTS)

# A component rebuilds when its Dockerfile, its patch queue, or the pin
# manifest changes — that is the whole input set of a hermetic build.
#
# The stamp NAME carries the pinned version+revision, so a pin change is a
# different stamp that does not exist yet and the rebuild fires regardless of
# file mtimes. An mtime prerequisite on versions.mk is not enough on its own:
# `git checkout` does not give make reliable mtimes, so a checkout that bumps
# a pin can leave the old stamp looking up to date, skip the rebuild, and
# `make package` can ship the previously built binary while every version
# string advertises the new pin.
# The Dockerfile/patches/versions.mk prerequisites stay, to catch every OTHER
# input change for the same pin.
define COMPONENT_RULE
$(1): $(STAMPS)/$(1)-$$($(1)_version)-$$($(1)_revision)
$(STAMPS)/$(1)-$$($(1)_version)-$$($(1)_revision): $(1)/Dockerfile ../versions.mk $$(wildcard $(1)/patches/*.patch)
	@mkdir -p $$(dir $$@) $(STAGING)
	@# One stamp per component: drop any stamp from a different pin so switching
	@# versions (either direction) re-extracts and the staged binary never lags
	@# the pin. The new stamp does not exist yet, so this removes only old ones.
	@rm -f $(STAMPS)/$(1)-*
	@echo "==> building $(1) v$$($(1)_version) for linux/$(TARGET_ARCH)"
	$(DOCKER) build $(DOCKER_BUILD_FLAGS) \
	  --platform linux/$(TARGET_ARCH) \
	  --build-arg BUILDIMAGE=$$($(1)_buildimage) \
	  --build-arg VERSION=$$($(1)_version) \
	  --build-arg REVISION=$$($(1)_revision) \
	  --build-arg TARGET_ARCH=$(TARGET_ARCH) \
	  --build-arg GO_TAGS='$$($(1)_build_go_tags)' \
	  --build-arg GO_CGO_ENABLED=$$($(1)_build_go_cgo_enabled) \
	  --build-arg GO_LDFLAGS_EXTRA='$$($(1)_build_go_ldflags_extra)' \
	  --build-arg GO_FLAGS='$$($(1)_build_go_flags)' \
	  --build-arg BINARIES='$$($(1)_binaries)' \
	  --build-arg STRATUM_VERSION=$(STRATUM_VERSION) \
	  -t stratum-build/$(1):$$($(1)_version) \
	  $(1)
	@$$(MAKE) --no-print-directory extract COMPONENT=$(1)
	@touch $$@
endef
$(foreach c,$(COMPONENTS),$(eval $(call COMPONENT_RULE,$(c))))

# Extraction without `docker run`, so nothing from the build image's
# runtime can leak into the artifact — just the container filesystem, exported
# and untarred.
.PHONY: extract
extract:
	@set -eu; \
	cid=$$($(DOCKER) create --platform linux/$(TARGET_ARCH) stratum-build/$(COMPONENT):$$(sed -n 's/^$(COMPONENT)_version *= *//p' ../versions.mk)); \
	trap "$(DOCKER) rm -f $$cid >/dev/null" EXIT; \
	mkdir -p $(STAGING); \
	$(DOCKER) export $$cid | tar -x -C $(STAGING) --strip-components=1 out/; \
	for b in $$(sed -n 's/^$(COMPONENT)_binaries *= *//p' ../versions.mk); do \
	  test -x $(STAGING)/$$b || { echo "extract: $$b missing from $(STAGING)" >&2; exit 1; }; \
	  printf '  %-28s %s\n' "$$b" "$$(sha256sum $(STAGING)/$$b 2>/dev/null | cut -c1-16 || shasum -a256 $(STAGING)/$$b | cut -c1-16)"; \
	done

clean:
	rm -rf $(STAMPS) $(STAGING)
