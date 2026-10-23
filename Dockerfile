# The SKE release image: the packaged artifact, in a container.
#
# This is not how SKE is normally installed — it is one static binary on a host.
# The image exists for the places a container is the unit of distribution:
# stratum-in-docker for test harnesses, and CI systems that would rather pull an
# image than fetch a release asset.
#
# It carries the SAME artifact as the release: dist/stratum is built natively on
# a runner of the target architecture and copied in. Nothing is compiled here, so
# there is no second build to diverge from the signed one.
ARG ALPINE_VERSION
FROM docker.io/library/alpine:$ALPINE_VERSION

# The payload extracts to /var/lib/stratum and containerd needs a real
# filesystem there, so declare it rather than leaving it on the container's
# overlay — overlay-on-overlay does not mount.
VOLUME /var/lib/stratum

COPY dist/stratum /usr/local/bin/stratum
ENTRYPOINT ["/usr/local/bin/stratum"]
