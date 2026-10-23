# Harness image for the integration suite: "a node with nothing installed",
# plus the tools the harness itself needs, and the ones `stratum uninstall`
# runs on a node: ip (iproute2) for Cilium's links, tc (iproute2-tc) for its tc
# filters, iptables-save and ip6tables-save (iptables) for its netfilter rules,
# and bpftool for its cgroup programs. Uninstall fails when any is missing, so a
# node image without them could not be uninstalled.
# openssl lets a check build a certificate request as a joining kubelet would.
#
# The Alpine version arrives as a build arg from build/versions.mk — the test
# harness is held to the same no-literals rule as the build.
ARG ALPINE_VERSION
FROM docker.io/library/alpine:$ALPINE_VERSION
RUN apk add --no-cache bash python3 iproute2 iproute2-tc iptables bpftool openssl
