/* global window, fetch */
// ============================================================
// live.js — wire the Stratum dashboard to the real apiserver.
// Every read goes through the one Kubernetes transport (k8sutil.js's
// k8sFetch), which owns the proxy prefix, the 15s deadline and the 401 rule;
// this module reshapes k8s API objects into the K8S structures the screens
// expect. Anything that errors or has no API equivalent keeps the data.js
// empty shell. CPU/MEM are REAL usage from the kubelet Summary API
// (/nodes/<n>/proxy/stats/summary); falls back to request-allocation if the
// Summary API is unavailable.
//
// A load resolves an outcome — { answered, unauthorized, stale } — instead of
// resolving silently on failures: `answered` says whether any slice of K8S was
// rewritten this round, `unauthorized` says the session ended (the transport
// has already routed it to sign-in through session.js's handler), and `stale`
// names the slices that kept their last good data because a read did not
// answer. The refresh verdict the poll tick and the Refresh button act on is
// refreshVerdict() below.
// ============================================================
import { K8S } from "./data.js";
import { apiBase, currentConfig, secretSummariesServed } from "./cluster-context.js";
import { k8sFetch } from "./k8sutil.js";
import { currentSessionRevision, isSessionCurrent } from "./session.js";
import { age, cpuToM, memToMi, podStatus, podReady, jobStatus, workloadReady } from "./k8s-rules.js";

  // No request may hang: the next poll is scheduled only after the current load
  // settles, so one wedged kubelet would otherwise stop auto-refresh for good.
  // The transport enforces the deadline on every call; the loader names it.
  const TIMEOUT_MS = 15000;
  const HELM_RELEASE = "helm.sh/release.v1";
  const SECRETS_TABLE = "/api/v1/secrets?fieldSelector=" + encodeURIComponent("type=" + HELM_RELEASE) + "&includeObject=Metadata";
  const SECRETS_SUMMARY = "/api/v1/secret-summaries";
  const OPERATOR_LEASE = "/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-operator";
  // For a single object that legitimately may not exist: 404 is an ANSWER
  // ("there is none"), every other failure is still "we could not ask", so
  // ABSENT counts as answered and a 500 does not.
  const ABSENT = Object.freeze({ absent: true });
  const items = (o) => Array.isArray(o?.items) ? o.items : [];
  const object = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
  const listResponse = (v) => object(v) && Array.isArray(v.items);
  const leaseResponse = (v) => object(v) && object(v.metadata)
    && (v.spec === undefined || object(v.spec));
  // The server emits summaries, not Secret objects. Every item must answer
  // the schema before an empty Helm selection can replace last-good releases.
  const secretSummaryResponse = (v) => listResponse(v) && v.items.every((s) => object(s)
    && typeof s.name === "string" && s.name.length > 0
    && typeof s.namespace === "string" && s.namespace.length > 0
    && typeof s.type === "string" && Number.isInteger(s.keys) && s.keys >= 0
    && (s.creationTimestamp === null || typeof s.creationTimestamp === "string")
    && (s.helm === undefined ? s.type !== HELM_RELEASE : object(s.helm)
      && ["name", "version", "status"].every((field) => typeof s.helm[field] === "string")));
  const versionResponse = (v) => object(v) && typeof v.gitVersion === "string" && v.gitVersion.trim().length > 0;
  const tableResponse = (v) => object(v) && Array.isArray(v.columnDefinitions)
    && v.columnDefinitions.every((c) => object(c) && typeof c.name === "string")
    && ["Type", "Data"].every((name) => v.columnDefinitions.some((c) => c.name === name))
    && Array.isArray(v.rows) && v.rows.every((r) => object(r) && Array.isArray(r.cells)
      && r.cells.length === v.columnDefinitions.length && object(r.object?.metadata));
  const summaryResponse = (v) => object(v) && object(v.node)
    && typeof v.node.cpu?.usageNanoCores === "number" && typeof v.node.memory?.workingSetBytes === "number"
    && Array.isArray(v.pods) && v.pods.every((p) => object(p) && object(p.podRef)
      && typeof p.podRef.namespace === "string" && p.podRef.namespace.length > 0
      && typeof p.podRef.name === "string" && p.podRef.name.length > 0);
  const podUsageReported = (p) => typeof p.cpu?.usageNanoCores === "number"
    && typeof p.memory?.workingSetBytes === "number";
  function secretTableSummaries(t) {
    const cols = (t.columnDefinitions || []).map((c) => c.name);
    return { items: (t.rows || []).map((r) => {
      const m = (r.object && r.object.metadata) || {}, l = m.labels || {};
      const c = Object.fromEntries(cols.map((n, i) => [n, (r.cells || [])[i]]));
      return { name: m.name, namespace: m.namespace, type: c.Type, keys: c.Data, creationTimestamp: m.creationTimestamp,
        ...(c.Type === HELM_RELEASE ? { helm: { name: l.name, version: l.version, status: l.status } } : {}) };
    }) };
  }

  // Which gate reads stand behind which K8S slice: when one of a slice's reads
  // did not answer, the slice kept its last good data and the load's outcome
  // names it stale. Per-node Summary failures are recorded separately below:
  // node rows fall back to requests, while fleet and workload usage retain
  // their last complete sample. This table and the
  // `ok(...)` gates on the writes below must agree: the gates decide what is
  // replaced, this decides what is reported.
  const GATED_BY = {
    NODES: ["/api/v1/nodes", "/api/v1/pods"],
    FLEET: ["/api/v1/nodes"],
    NODE_CONDITIONS: ["/api/v1/nodes"],
    NAMESPACES: ["/api/v1/namespaces"],
    PODS: ["/api/v1/pods"],
    WORKLOADS: ["/apis/apps/v1/deployments", "/apis/apps/v1/statefulsets", "/apis/apps/v1/daemonsets",
      "/apis/batch/v1/jobs", "/apis/batch/v1/cronjobs"],
    EVENTS: ["/api/v1/events"],
    STORAGE_CLASSES: ["/apis/storage.k8s.io/v1/storageclasses"],
    HELM: [SECRETS_TABLE, SECRETS_SUMMARY],
    CLUSTER_INFO: ["/version", "/api/v1/nodes", "/api/v1/pods", "/apis/apps/v1/daemonsets",
      "/apis/storage.k8s.io/v1/storageclasses", OPERATOR_LEASE],
    CLUSTER: ["/version", "/api/v1/nodes"],
  };

  const acc = (m) => (m || []).reduce((s, c) => {
    const r = (c.resources && c.resources.requests) || {};
    s.cpu += cpuToM(r.cpu); s.mem += memToMi(r.memory); return s;
  }, { cpu: 0, mem: 0 });
  const podReq = (p) => acc((p.spec && p.spec.containers) || []);
  const restartsOf = (p) => ((p.status && p.status.containerStatuses) || []).reduce((a, c) => a + (c.restartCount || 0), 0);
  const firstImage = (tmpl) => { const cs = (tmpl && tmpl.spec && tmpl.spec.containers) || []; return cs.length ? cs[0].image : "—"; };
  const wlStatus = (rd, des) => des === 0 ? "Scheduled" : rd >= des ? "Running" : rd > 0 ? "Progressing" : "Degraded";

  // Load (or re-load) this cluster into the K8S singleton. Never resets on its
  // own, so the 15 s poll keeps the last good data when a fetch fails.
  const superseded = (unauthorized = false) => ({ answered: false, unauthorized, stale: [], superseded: true });
  async function load(revision) {
    // ---- one round of reads, and what it answered for ----
    // `failed` holds the gate reads that did not answer this round, `written`
    // the K8S slices actually rewritten, `unauthorized` whether any read came
    // back 401 — the session ended, and the transport has already routed it to
    // sign-in through session.js's handler.
    const failed = new Set();
    const written = new Set();
    let unauthorized = false;
    const warn = (path, r) => console.warn("Stratum live:", `${path} -> HTTP ${r.status}${r.error ? ": " + r.error : ""}`);
    const recordRead = (path, r, validate = () => true, soft = false) => {
      if (r.ok && validate(r.body)) return r.body;
      if (r.status === 401) unauthorized = true;
      if (!soft) failed.add(path);
      warn(path, r.ok ? { ...r, error: "malformed response shape" } : r);
      return null;
    };

    // A read through the transport. A failed read returns null, never an empty
    // shell: the caller has to be able to tell "the cluster has none of these"
    // from "we could not ask", and replace its slice of K8S only on a real
    // answer — an empty list included.
    const read = async (path, validate) => {
      const r = await k8sFetch("GET", path, { timeoutMs: TIMEOUT_MS });
      return recordRead(path, r, validate);
    };
    const G = read;
    const GList = (path) => read(path, listResponse);
    // An unreachable kubelet leaves node rows on request-based numbers. Fleet
    // and workload usage retain complete samples; the outcome marks them
    // stale below when a node's Summary read did not answer.
    const GSoft = async (path) => {
      const r = await k8sFetch("GET", path, { timeoutMs: TIMEOUT_MS });
      return recordRead(path, r, summaryResponse, true);
    };
    const GOne = async (path) => {
      const r = await k8sFetch("GET", path, { timeoutMs: TIMEOUT_MS });
      if (r.status === 404) return ABSENT;
      return recordRead(path, r, leaseResponse);
    };
    // Only a real answer replaces a slice; put() records which slices did.
    const put = (slice, value) => {
      if (!isSessionCurrent(revision)) return;
      written.add(slice); K[slice] = value;
    };

    // Secret summaries use the server API base; metadata-only Tables select
    // their representation through Accept. Both retain the shared transport's
    // deadline, cancellation and unauthorized handling.
    const GSecrets = () => secretSummariesServed()
      ? k8sFetch("GET", SECRETS_SUMMARY, { base: "", timeoutMs: TIMEOUT_MS }).then((r) => recordRead(SECRETS_SUMMARY, r, secretSummaryResponse))
      : k8sFetch("GET", SECRETS_TABLE, { accept: "application/json;as=Table;g=meta.k8s.io;v=v1", timeoutMs: TIMEOUT_MS })
        .then((r) => {
          const t = recordRead(SECRETS_TABLE, r, tableResponse);
          return ok(t) ? secretTableSummaries(t) : null;
        });
    const ok = (r) => r != null;

    const K = K8S;
    const [ver, nodesR, podsR, nsR, depR, ssR, dsR, jobR, cronR, scR, secR, evR] = await Promise.all([
      G("/version", versionResponse),
      GList("/api/v1/nodes"), GList("/api/v1/pods"), GList("/api/v1/namespaces"),
      GList("/apis/apps/v1/deployments"), GList("/apis/apps/v1/statefulsets"), GList("/apis/apps/v1/daemonsets"),
      GList("/apis/batch/v1/jobs"), GList("/apis/batch/v1/cronjobs"),
      GList("/apis/storage.k8s.io/v1/storageclasses"),
      GSecrets(), GList("/api/v1/events"),
    ]);
    if (!isSessionCurrent(revision)) return superseded(unauthorized);

    const pods = items(podsR);

    // ---- Real CPU/Mem usage via kubelet Summary API (no metrics-server needed) ----
    const nodeNames = items(nodesR).map((n) => n.metadata.name);
    const summaries = await Promise.all(nodeNames.map((nm) =>
      GSoft("/api/v1/nodes/" + encodeURIComponent(nm) + "/proxy/stats/summary")));
    if (!isSessionCurrent(revision)) return superseded(unauthorized);
    const nodeUse = {};   // node -> { cpu (millicores), mem (Mi) }
    const podUse = {};    // "ns/name" -> { cpu (millicores), mem (Mi) }
    summaries.forEach((s, i) => {
      if (!s) return;
      if (s.node) nodeUse[nodeNames[i]] = {
        cpu: ((s.node.cpu && s.node.cpu.usageNanoCores) || 0) / 1e6,
        mem: ((s.node.memory && s.node.memory.workingSetBytes) || 0) / 1048576 };
      (s.pods || []).forEach((p) => {
        if (!podUsageReported(p)) return;
        const ref = p.podRef || {};
        podUse[ref.namespace + "/" + ref.name] = {
          cpu: ((p.cpu && p.cpu.usageNanoCores) || 0) / 1e6,
          mem: ((p.memory && p.memory.workingSetBytes) || 0) / 1048576 };
      });
    });
    const usageComplete = ok(nodesR) && summaries.every(ok);
    // Kubelets can report node usage before pod cgroup metrics are available.
    // Such a response refreshes fleet usage, but cannot replace workload usage.
    const podUsageComplete = usageComplete && summaries.every((s) => s.pods.every(podUsageReported));
    console.log("Stratum: kubelet Summary usage for " + Object.keys(nodeUse).length + "/" + nodeNames.length + " nodes");

    // ---- Nodes ----
    const nodes = items(nodesR).map((n) => {
      const labels = n.metadata.labels || {};
      const conds = (n.status && n.status.conditions) || [];
      const isReady = (conds.find((c) => c.type === "Ready") || {}).status === "True";
      const onNode = pods.filter((p) => p.spec.nodeName === n.metadata.name && !["Succeeded", "Failed"].includes(p.status && p.status.phase));
      const allocCpu = cpuToM(n.status.allocatable && n.status.allocatable.cpu);
      const allocMem = memToMi(n.status.allocatable && n.status.allocatable.memory);
      const req = onNode.reduce((s, p) => { const r = podReq(p); s.cpu += r.cpu; s.mem += r.mem; return s; }, { cpu: 0, mem: 0 });
      const use = nodeUse[n.metadata.name] || null;   // real usage if available, else fall back to requests
      const isCp = ("node-role.kubernetes.io/control-plane" in labels) || ("node-role.kubernetes.io/master" in labels) || (labels["stratum.ciq.com/control-plane"] === "true");
      const anns = n.metadata.annotations || {};
      return {
        name: n.metadata.name, role: isCp ? "control-plane" : "worker",
        // Readiness and schedulability are independent — a cordoned node is still
        // Ready. `status` is only the display verdict (NotReady outranks Cordoned);
        // every readiness count reads `ready`.
        ready: isReady,
        status: !isReady ? "NotReady" : (n.spec.unschedulable ? "SchedulingDisabled" : "Ready"),
        cpu: allocCpu ? Math.min(100, Math.round((use ? use.cpu : req.cpu) / allocCpu * 100)) : 0,
        mem: allocMem ? Math.min(100, Math.round((use ? use.mem : req.mem) / allocMem * 100)) : 0,
        pods: onNode.length, podCap: parseInt((n.status.allocatable && n.status.allocatable.pods) || "110", 10),
        version: (n.status.nodeInfo || {}).kubeletVersion || "—",
        stratumVersion: anns["stratum.ciq.com/k8s-version"] || "—",   // stamped by the operator after upgrade
        os: (n.status.nodeInfo || {}).osImage || "—",
        runtime: (n.status.nodeInfo || {}).containerRuntimeVersion || "—",
        kernel: (n.status.nodeInfo || {}).kernelVersion || "—",
        instance: labels["node.kubernetes.io/instance-type"] || labels["beta.kubernetes.io/instance-type"] || "—",
        age: age(n.metadata.creationTimestamp), cordoned: !!n.spec.unschedulable,
        zone: labels["topology.kubernetes.io/zone"] || "—",
        // Real conditions / labels / taints, so the screens can show what the
        // object says instead of synthesising it from role and cordon.
        conditions: Object.fromEntries(conds.map((c) => [c.type, c.status])),
        labels, taints: (n.spec.taints || []).map((t) => `${t.key}${t.value ? "=" + t.value : ""}:${t.effect || ""}`),
      };
    });
    // Every node row carries pod counts and, without the Summary API, request-based
    // usage — both read off the pod list. A node list that answered while /pods did
    // not would replace good rows with zeroes, so this slice needs both.
    if (ok(nodesR) && ok(podsR)) put("NODES", nodes);

    // ---- Fleet aggregates + rolling history ----
    // Fleet samples require usage from every node, so an unreachable kubelet
    // cannot shrink the totals or append an incomplete sample to history.
    // Node rows still use request-based fallback independently.
    const HISTORY_MAX = 24;
    const fleetNodes = items(nodesR).filter((n) => nodeUse[n.metadata.name]);
    // A cluster that answered with no nodes has no fleet, so the totals and the
    // history go with them. Nodes present but no Summary API is a different
    // thing — usage is unavailable, not zero — and keeps the last good sample.
    if (ok(nodesR) && !items(nodesR).length) {
      put("FLEET", { nodes: 0, cpuUsed: 0, cpuTotal: 0, memUsed: 0, memTotal: 0,
                     cpuPct: null, memPct: null, history: { cpu: [], mem: [] } });
    } else if (fleetNodes.length && fleetNodes.length === nodeNames.length) {
      let cpuUsedM = 0, cpuTotalM = 0, memUsedMi = 0, memTotalMi = 0;
      fleetNodes.forEach((n) => {
        const u = nodeUse[n.metadata.name];
        cpuUsedM += u.cpu; memUsedMi += u.mem;
        cpuTotalM += cpuToM(n.status.allocatable && n.status.allocatable.cpu);
        memTotalMi += memToMi(n.status.allocatable && n.status.allocatable.memory);
      });
      const cpuPct = cpuTotalM ? Math.min(100, Math.round(cpuUsedM / cpuTotalM * 100)) : null;
      const memPct = memTotalMi ? Math.min(100, Math.round(memUsedMi / memTotalMi * 100)) : null;
      const prev = (K.FLEET && K.FLEET.history) || { cpu: [], mem: [] };
      const push = (arr, v) => {
        if (v == null) return arr;
        const next = arr.concat(v);
        return next.length > HISTORY_MAX ? next.slice(next.length - HISTORY_MAX) : next;
      };
      put("FLEET", {
        nodes: fleetNodes.length,
        cpuUsed: cpuUsedM / 1000, cpuTotal: cpuTotalM / 1000,      // cores
        memUsed: memUsedMi * 1048576, memTotal: memTotalMi * 1048576, // bytes
        cpuPct, memPct,
        history: { cpu: push(prev.cpu || [], cpuPct), mem: push(prev.mem || [], memPct) },
        max: HISTORY_MAX,
      });
    }

    // ---- Real node conditions (aggregated across the fleet) ----
    // `unknownNodes` is the point: a node that does not report a condition, or
    // reports it Unknown, is not a node where it is False, and an empty
    // trueNodes list on its own cannot tell those apart.
    const condOf = (n, type) => (((n.status && n.status.conditions) || []).find((c) => c.type === type) || {}).status;
    if (ok(nodesR)) put("NODE_CONDITIONS", ["MemoryPressure", "DiskPressure", "PIDPressure", "NetworkUnavailable"]
      .map((type) => {
        const named = (pred) => items(nodesR).filter((n) => pred(condOf(n, type))).map((n) => n.metadata.name);
        return {
          type,
          trueNodes: named((v) => v === "True"),
          unknownNodes: named((v) => v === undefined || v === "Unknown"),
          nodes: items(nodesR).length,
        };
      }));

    // ---- Namespaces ----
    const nsList = items(nsR);
    if (ok(nsR)) put("NAMESPACES", ["all-namespaces", ...nsList.map((n) => n.metadata.name)]);

    // ---- Pods ----
    if (ok(podsR)) put("PODS", pods.map((p) => {
      const r = podReq(p);
      const u = podUse[p.metadata.namespace + "/" + p.metadata.name];   // real usage, else requests
      return { name: p.metadata.name, ns: p.metadata.namespace, node: p.spec.nodeName || "—",
        status: podStatus(p), ready: podReady(p), restarts: restartsOf(p),
        cpu: Math.round(u ? u.cpu : r.cpu), mem: Math.round(u ? u.mem : r.mem), age: age(p.metadata.creationTimestamp),
        ip: (p.status && p.status.podIP) || "—", qos: (p.status && p.status.qosClass) || "—" };
    }));

    // ---- Workloads ----
    // READY on every row reads the shared workloadReady rule (readyReplicas over
    // replicas, the kubectl convention). `status` is a different answer, not a
    // restatement of it: Ready and Available answer different questions, so a
    // mid-rollout Deployment shows 3/3 while its availability is still 1, and
    // the Workloads status column reports that as Progressing.
    const wl = [];
    items(depR).forEach((d) => { const des = d.spec.replicas || 0, av = (d.status && d.status.availableReplicas) || 0;
      wl.push({ name: d.metadata.name, kind: "Deployment", ns: d.metadata.namespace, ready: workloadReady(d), desired: des, updated: (d.status && d.status.updatedReplicas) || 0, available: av, status: wlStatus(av, des), age: age(d.metadata.creationTimestamp), image: firstImage(d.spec.template), restarts: 0, cpu: 0, mem: 0 }); });
    items(ssR).forEach((s) => { const des = s.spec.replicas || 0, rd = (s.status && s.status.readyReplicas) || 0;
      wl.push({ name: s.metadata.name, kind: "StatefulSet", ns: s.metadata.namespace, ready: workloadReady(s), desired: des, updated: (s.status && s.status.updatedReplicas) || 0, available: rd, status: wlStatus(rd, des), age: age(s.metadata.creationTimestamp), image: firstImage(s.spec.template), restarts: 0, cpu: 0, mem: 0 }); });
    items(dsR).forEach((s) => { const des = (s.status && s.status.desiredNumberScheduled) || 0, rd = (s.status && s.status.numberReady) || 0;
      wl.push({ name: s.metadata.name, kind: "DaemonSet", ns: s.metadata.namespace, ready: `${rd}/${des}`, desired: des, updated: (s.status && s.status.updatedNumberScheduled) || 0, available: rd, status: wlStatus(rd, des), age: age(s.metadata.creationTimestamp), image: firstImage(s.spec.template), restarts: 0, cpu: 0, mem: 0 }); });
    items(jobR).forEach((j) => { const su = (j.status && j.status.succeeded) || 0, comp = (j.spec && j.spec.completions) || 1;
      wl.push({ name: j.metadata.name, kind: "Job", ns: j.metadata.namespace, ready: `${su}/${comp}`, desired: comp, updated: 0, available: su, status: jobStatus(j), age: age(j.metadata.creationTimestamp), image: firstImage(j.spec.template), restarts: 0, cpu: 0, mem: 0 }); });
    items(cronR).forEach((c) => wl.push({ name: c.metadata.name, kind: "CronJob", ns: c.metadata.namespace, ready: "—", desired: 0, updated: 0, available: 0, status: "Scheduled", age: age(c.metadata.creationTimestamp), image: firstImage(c.spec.jobTemplate && c.spec.jobTemplate.spec && c.spec.jobTemplate.spec.template), restarts: 0, schedule: c.spec.schedule, cpu: 0, mem: 0 }));
    // Only complete Summary samples can replace workload usage. Keep prior
    // usage by kind/namespace/name while readiness and other fields refresh.
    // New workloads with no complete sample have no usage to display yet.
    if (podUsageComplete) {
      const sumBy = (pred) => { let cpu = 0, mem = 0; for (const key in podUse) { const i = key.indexOf("/"); if (pred(key.slice(0, i), key.slice(i + 1))) { cpu += podUse[key].cpu; mem += podUse[key].mem; } } return { cpu: Math.round(cpu), mem: Math.round(mem) }; };
      wl.forEach((w) => { const t = sumBy((pns, pname) => pns === w.ns && (pname === w.name || pname.indexOf(w.name + "-") === 0)); w.cpu = t.cpu; w.mem = t.mem; });
    } else {
      const key = (w) => `${w.kind}/${w.ns}/${w.name}`;
      const previous = new Map(K.WORKLOADS.map((w) => [key(w), w]));
      wl.forEach((w) => { const old = previous.get(key(w)); w.cpu = old?.cpu || 0; w.mem = old?.mem || 0; });
    }
    if ([depR, ssR, dsR, jobR, cronR].every(ok)) {
      put("WORKLOADS", wl);
    }

    // ---- Events ----
    const evs = items(evR);
    if (ok(evR)) put("EVENTS", evs.map((e) => {
      const io = e.involvedObject || {}; const seen = e.lastTimestamp || e.eventTime || (e.metadata && e.metadata.creationTimestamp);
      return { type: e.type || "Normal", reason: e.reason || "", obj: `${(io.kind || "").toLowerCase()}/${io.name || ""}`,
        ns: e.metadata.namespace || io.namespace || "—", msg: e.message || "", count: e.count || 1, age: age(seen), lastSeen: new Date(seen).getTime() || 0 };
    }).sort((a, b) => b.lastSeen - a.lastSeen).slice(0, 50));

    // ---- Storage ----
    if (ok(scR)) put("STORAGE_CLASSES", items(scR).map((s) => ({ name: s.metadata.name, provisioner: s.provisioner, reclaim: s.reclaimPolicy || "Delete",
      binding: s.volumeBindingMode || "Immediate", expand: !!s.allowVolumeExpansion,
      default: (s.metadata.annotations || {})["storageclass.kubernetes.io/is-default-class"] === "true", age: age(s.metadata.creationTimestamp) })));

    // ---- Helm releases (from helm.sh/release.v1 secrets) ----
    // chart/appVersion live in the gzipped release blob (not decoded in-browser → "—");
    // name/namespace/revision/status come from the secret's helm labels.
    const helmLatest = {};
    items(secR).filter((s) => s.helm).forEach((s) => {
      const l = s.helm;
      const nm = l.name || s.name, rev = parseInt(l.version || 0, 10) || 0;
      const k = s.namespace + "/" + nm;
      if (!helmLatest[k] || rev > helmLatest[k].revision) helmLatest[k] = {
        name: nm, ns: s.namespace, chart: "—", appVersion: "—",
        status: l.status || "—", revision: rev, updated: age(s.creationTimestamp),
      };
    });
    if (ok(secR)) put("HELM", Object.values(helmLatest));

    // ---- This cluster's identity (from the apiserver) ----
    const cpNodes = nodes.filter((n) => n.role === "control-plane");
    const k8sVersion = (ver && ver.gitVersion) ? ver.gitVersion.replace(/^v/, "") : "";

    const leaseR = await GOne(OPERATOR_LEASE);
    if (!isSessionCurrent(revision)) return superseded(unauthorized);

    // A leader-election Lease outlives the process that held it: it is only
    // evidence of a live operator until renewTime + leaseDurationSeconds passes.
    const lease = leaseR === ABSENT ? null : leaseR;
    const leaseSpec = (lease && lease.spec) || {};
    const leaseExpiry = leaseSpec.renewTime
      ? new Date(leaseSpec.renewTime).getTime() + (leaseSpec.leaseDurationSeconds || 0) * 1000 : 0;
    const operatorActive = leaseExpiry > Date.now();
    const operatorLeader = leaseSpec.holderIdentity || "—";
    // Cluster info for the Overview cards (all real / cluster-visible).
    // The control plane runs as kubelet-managed static pods (mirror pods in
    // kube-system) fronted by a kube-vip VIP; we count what the API shows.
    const cniDs = items(dsR).find((d) => /cilium/i.test(d.metadata.name));
    const defaultSC = (K.STORAGE_CLASSES || []).find((s) => s.default);
    const kubeSystemPods = pods.filter((p) => p.metadata.namespace === "kube-system");
    const isMirror = (p) => { const a = p.metadata.annotations || {}; return !!a["kubernetes.io/config.mirror"] || a["kubernetes.io/config.source"] === "file"; };
    const staticPods = kubeSystemPods.filter(isMirror);
    const runningCount = (list) => list.filter((p) => (p.status && p.status.phase) === "Running").length;
    const apiserverPods = staticPods.filter((p) => /^kube-apiserver-/.test(p.metadata.name));
    const etcdPods = staticPods.filter((p) => /^etcd-/.test(p.metadata.name));
    const kubeVipPods = kubeSystemPods.filter((p) => /kube-vip/.test(p.metadata.name));
    // Six reads feed these cards (version, nodes, pods, daemonsets, storage
    // classes, the operator lease). Any one of them failing would blank a card
    // whose last-good value is still the truth, so all six have to answer.
    if ([ver, nodesR, podsR, dsR, scR, leaseR].every(ok)) put("CLUSTER_INFO", {
      version: k8sVersion,
      cni: cniDs ? "Cilium" : "—",
      defaultStorageClass: defaultSC ? defaultSC.name : "—",
      controlPlane: cpNodes.length,
      controlPlaneReady: cpNodes.filter((n) => n.ready).length,
      staticPods: { total: staticPods.length, running: runningCount(staticPods),
        apiserver: { total: apiserverPods.length, running: runningCount(apiserverPods) },
        etcd: { total: etcdPods.length, running: runningCount(etcdPods) } },
      kubeVip: { total: kubeVipPods.length, running: runningCount(kubeVipPods) },
      operatorActive, operatorLeader,
    });

    // The one cluster this dashboard serves, described entirely from what its
    // own apiserver just told us: fields the apiserver cannot supply are simply
    // absent, and the screens render them as "—".
    if (ok(ver) && ok(nodesR)) put("CLUSTER", {
      id: "local", name: "local", displayName: "local", labels: {}, source: "local",
      k8sVersion, stratumVersion: (currentConfig() || {}).version || "",
      nodeCount: nodes.length, readyNodes: nodes.filter((n) => n.ready).length,
      controlPlaneNodes: cpNodes.length,
      status: nodes.length && nodes.every((n) => n.ready) ? "healthy" : "degraded",
      k8sBase: apiBase(),
    });

    if (written.size) {
      window.__STRATUM_LIVE = true;
      console.log(`Stratum: live data loaded — ${nodes.length} nodes, ${pods.length} pods, ${wl.length} workloads`);
    }

    // The load's outcome: whether any slice of K8S was rewritten this round,
    // whether the session ended, and which slices kept their last good data
    // because one of their reads did not answer.
    const stale = Object.keys(GATED_BY).filter((slice) => GATED_BY[slice].some((path) => failed.has(path)));
    if (ok(nodesR) && nodeNames.length && fleetNodes.length !== nodeNames.length && !stale.includes('FLEET')) stale.push('FLEET');
    if (wl.length && !podUsageComplete && !stale.includes('WORKLOADS')) stale.push('WORKLOADS');
    return { answered: written.size > 0, unauthorized, stale };
  }

  // The refresh-outcome decision, extracted from the Shell's effects so the
  // suite can read what "refreshed" means. Only a load that answered may stamp
  // the refreshed-at time; a total failure says so; a partial success stamps
  // but names the stale slices instead of claiming a full refresh; an ended
  // session stamps nothing, because sign-in is the report.
  function refreshVerdict(outcome) {
    const o = outcome || {};
    const stale = o.stale || [];
    if (o.unauthorized || o.superseded) return { stamp: false, message: null };
    if (!o.answered) return { stamp: false, message: "Refresh failed — keeping last data" };
    if (stale.length) {
      const named = stale.length <= 4 ? stale.join(", ")
        : `${stale.slice(0, 4).join(", ")} +${stale.length - 4} more`;
      return { stamp: true, message: `Refreshed — ${named} kept last data` };
    }
    return { stamp: true, message: "Refreshed" };
  }

  // One load at a time, whoever asks. The 15s poll, the Refresh button and the
  // cordon handler all call this, and two loads at once race each other into the
  // same K8S singleton — the slower reply wins, whatever it is. A caller that
  // arrives during a load waits for the NEXT one rather than joining the one
  // already in flight, so a refresh issued after a write still sees the write.
  let running = null;   // the load in flight
  let queued = null;    // { revision, promise } waiting behind it
  function loadOnce(revision) {
    if (!isSessionCurrent(revision)) return Promise.resolve(superseded());
    if (!running) {
      running = load(revision).finally(() => { running = null; });
      return running;
    }
    if (!queued || queued.revision !== revision) {
      const next = { revision, promise: null };
      next.promise = running.catch(() => {}).then(() => {
        if (queued === next) queued = null;
        return loadOnce(revision);
      });
      queued = next;
    }
    return queued.promise;
  }

  export function loadLiveK8S() { return loadOnce(currentSessionRevision()); }
  export { refreshVerdict };
