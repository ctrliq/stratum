/* global window */
// ============================================================
// Stratum dashboard — data shells. NO mock data: every collection is
// empty and populated from the live cluster by live.js (loadLiveK8S):
// nodes, pods, workloads, events, namespaces, storage classes, the fleet,
// this cluster's identity and the Helm releases. Lists no
// screen displays (services, ingresses, PVCs, PVs, config maps, service
// accounts, RBAC, secrets) have no shell here — the registry screens fetch
// those kinds on demand. Logs and YAML are fetched live per-resource in the
// workload detail. Empty here = the API was unreachable; the UI renders empty
// states rather than fake data.
//
// K8S is a mutable singleton the screens read directly. resetK8S() puts every
// collection back to its empty shell.
// ============================================================
function emptyShell() {
  return {
    NAMESPACES: ["all-namespaces"],
    NODES: [],
    WORKLOADS: [],
    PODS: [],
    EVENTS: [],
    HELM: [],
    SAMPLE_YAML: "",
    LOG_POOL: [],
    STORAGE_CLASSES: [],
    CLUSTER_INFO: null,    // Overview cards — live
    CLUSTER: null,         // this cluster's identity, built by live.js — live
    // Cluster-wide CPU/memory totals + a rolling in-memory history of the 15s poll.
    // There is no time-series store in the distro, so `history` accumulates real
    // samples for as long as the tab is open (and starts empty on every reload
    // and on every cluster switch). cpu is in cores, mem in bytes; history
    // entries are integer percentages.
    FLEET: { nodes: 0, cpuUsed: 0, cpuTotal: 0, memUsed: 0, memTotal: 0,
             cpuPct: null, memPct: null, history: { cpu: [], mem: [] } },
    // Aggregated real node conditions: [{ type, trueNodes: [name,...] }]
    NODE_CONDITIONS: [],
  };
}

export const K8S = { ...emptyShell() };

// Re-initialise every collection in place (same object identity, so modules
// holding `K8S` keep working).
export function resetK8S() {
  Object.assign(K8S, emptyShell());
  if (typeof window !== "undefined") {
    window.__STRATUM_LIVE = false;
    window.__STRATUM_K8S_VERSION = "";
  }
}
