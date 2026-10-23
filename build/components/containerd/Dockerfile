ARG BUILDIMAGE
FROM $BUILDIMAGE

ARG VERSION
ARG REVISION
ARG TARGET_ARCH
ARG GO_TAGS
ARG GO_CGO_ENABLED
ARG GO_LDFLAGS_EXTRA
ARG BINARIES
ARG STRATUM_VERSION

# Hermetic: never let the go directive pull a different toolchain over the wire.
ENV GOTOOLCHAIN=local
RUN apk add --no-cache bash git make

WORKDIR /src
RUN git clone --depth=1 --branch "v$VERSION" https://github.com/containerd/containerd.git . \
 && test "$(git rev-parse HEAD)" = "$REVISION" \
      || { echo "revision mismatch: tag v$VERSION is $(git rev-parse HEAD), manifest pins $REVISION" >&2; exit 1; } \
 && git log -1 --format=%ct > /source-date-epoch

# ---- patch queue insertion point --------------------------------------------
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

RUN set -eux; \
    export SOURCE_DATE_EPOCH="$(cat /source-date-epoch)"; \
    export CGO_ENABLED="${GO_CGO_ENABLED}"; \
    export GOARCH="${TARGET_ARCH}"; \
    make STATIC=1 \
         BUILDTAGS="${GO_TAGS}" \
         EXTRA_LDFLAGS="${GO_LDFLAGS_EXTRA}" \
         VERSION="v${VERSION}+${STRATUM_VERSION}" \
         REVISION="${REVISION}" \
         binaries

RUN set -eux; mkdir -p /out; for b in $BINARIES; do cp "bin/$b" /out/; done; ls -l /out
