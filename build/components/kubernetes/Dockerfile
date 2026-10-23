# Hermetic Kubernetes build. Every version and flag arrives as a build arg from
# build/versions.mk — no literals here (enforced by tools/lint-versions.sh).
ARG BUILDIMAGE
FROM $BUILDIMAGE

ARG VERSION
ARG REVISION
ARG TARGET_ARCH
ARG GO_TAGS
ARG GO_CGO_ENABLED
ARG GO_LDFLAGS_EXTRA
ARG GO_FLAGS
ARG BINARIES
ARG STRATUM_VERSION

# Hermetic: never let the go directive pull a different toolchain over the wire.
ENV GOTOOLCHAIN=local

RUN apk add --no-cache bash git make rsync findutils coreutils grep tar which

WORKDIR /src
RUN git clone --depth=1 --branch "v$VERSION" https://github.com/kubernetes/kubernetes.git . \
 && test "$(git rev-parse HEAD)" = "$REVISION" \
      || { echo "revision mismatch: tag v$VERSION is $(git rev-parse HEAD), manifest pins $REVISION" >&2; exit 1; } \
 && git log -1 --format=%ct > /source-date-epoch

# ---- patch queue insertion point --------------------------------------------
# Patches from build/components/kubernetes/patches/ are copied in and applied
# here, after clone and before build. Empty in a normal build: SKE's posture is
# a near-empty queue, to keep the delta against upstream small.
COPY patches/ /patches/
RUN set -eu; \
    git config user.email build@stratum.invalid; \
    git config user.name "SKE Build"; \
    GIT_COMMITTER_DATE="$(cat /source-date-epoch) +0000"; export GIT_COMMITTER_DATE; \
    n=0; \
    for p in /patches/*.patch; do \
      [ -e "$p" ] || continue; \
      echo "applying $(basename "$p")"; \
      git am --keep-non-patch "$p" || { git am --abort || true; echo "patch failed to apply: $p" >&2; exit 1; }; \
      n=$((n+1)); \
    done; \
    echo "applied $n patch(es)"
# -----------------------------------------------------------------------------

# Reproducibility: timestamps derive from the pinned commit, never from now.
# Tree state is clean because git am produces real commits, so kube binaries
# self-report a clean build.
RUN set -eux; \
    export SOURCE_DATE_EPOCH="$(cat /source-date-epoch)"; \
    export KUBE_GIT_VERSION="v${VERSION}-${STRATUM_VERSION}"; \
    export KUBE_GIT_COMMIT="$(git rev-parse HEAD)"; \
    export KUBE_GIT_TREE_STATE=clean; \
    export KUBE_GIT_MAJOR="${VERSION%%.*}"; \
    export KUBE_GIT_MINOR="$(echo "$VERSION" | cut -d. -f2)"; \
    export KUBE_BUILD_PLATFORMS="linux/${TARGET_ARCH}"; \
    export CGO_ENABLED="${GO_CGO_ENABLED}"; \
    export GOFLAGS="${GO_FLAGS} -tags=${GO_TAGS}"; \
    export GOLDFLAGS="${GO_LDFLAGS_EXTRA}"; \
    what=""; for b in $BINARIES; do what="$what cmd/$b"; done; \
    make WHAT="$what"

# Collect into one flat dir so the extractor's docker-export tar has a single
# predictable path per component.
RUN set -eux; \
    mkdir -p /out; \
    for b in $BINARIES; do \
      cp "_output/local/bin/linux/${TARGET_ARCH}/$b" /out/; \
    done; \
    ls -l /out
