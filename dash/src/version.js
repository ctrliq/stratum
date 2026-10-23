// The one node-version comparison the dashboard makes, shared by the Nodes row
// badge (screens/infra.jsx) and the header count live.js derives.
//
// `stratum.ciq.com/k8s-version` — the annotation the OSS operator stamps —
// carries the UPSTREAM Kubernetes version. `status.nodeInfo.kubeletVersion`
// carries the SKE build OF that version, which ends in `-stratum` by design:
// tools/inttest/check-single-node.sh asserts `<server>-stratum` on every CI
// run. Comparing the two raw therefore reported drift on every node of every
// Stratum cluster, and an indicator that is always on cannot distinguish the
// partial upgrade it exists to catch.
//
// A node whose kubelet is a different SKE BUILD of the same upstream version
// reads as aligned: the annotation holds only the upstream version, so there is
// nothing build-qualified to compare it against. Per-build drift would need the
// operator to stamp a build-qualified value first.
//
// `upstreamVersion`, not `k8sVersion`: that name already means the cluster's own
// server version in live.js, overview.jsx and ui/shell.jsx.
export const upstreamVersion = (v) => String(v || '').replace(/^v/, '').replace(/-stratum.*$/, '');

export const nodeDrift = (n) => !!n.stratumVersion && n.stratumVersion !== '—'
  && upstreamVersion(n.stratumVersion) !== upstreamVersion(n.version);
