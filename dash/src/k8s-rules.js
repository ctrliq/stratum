// The object rules the two readers of Kubernetes API objects share: live.js's
// loader, which reshapes lists into the K8S slices the hand-written screens
// read, and the registry's column renderers (resources.jsx), which the generic
// kind lists run over the objects they fetch themselves. DOM-free and
// dependency-free, so node:test can pin the rules over fixture objects —
// rules-parity.test.mjs runs both paths over the same fixtures.
//
// Ready and Available answer different questions. Ready asks whether the
// replicas exist and run; Available asks whether they have survived
// minReadySeconds. So the one READY rule here reads readyReplicas over
// spec.replicas — what `kubectl get deployments` prints — while the Workloads
// screen's status column keeps reading availability, and a mid-rollout
// Deployment legitimately reads 3/3 and Progressing at the same time.

// Pod status: Terminating outranks everything (a deletionTimestamp exists),
// then a container stuck waiting — CrashLoopBackOff by name, every
// error-shaped wait reason (Err*, ImagePull*, Invalid*) as Failed — then the
// phase, which defaults to Pending while nothing has been reported yet.
export function podStatus(p) {
  if (p.metadata && p.metadata.deletionTimestamp) return 'Terminating';
  for (const c of (p.status && p.status.containerStatuses) || []) {
    const w = c.state && c.state.waiting;
    if (w && w.reason) {
      if (w.reason === 'CrashLoopBackOff') return 'CrashLoopBackOff';
      if (/Err|ImagePull|Invalid/.test(w.reason)) return 'Failed';
    }
  }
  return (p.status && p.status.phase) || 'Pending';
}

// A pod's Ready count: the ready containerStatuses over the total, falling
// back to the spec's containers while the kubelet has reported no statuses.
export function podReady(p) {
  const cs = (p.status && p.status.containerStatuses) || [];
  const total = cs.length || ((p.spec && p.spec.containers) || []).length;
  return cs.filter((c) => c.ready).length + '/' + total;
}

// Job status from the counters the apiserver itself maintains: succeeded
// through spec.completions (defaulting to 1, the non-parallel shape) is
// Complete, a failed pod is Failed, an active pod is Running, otherwise it
// has not started.
export function jobStatus(j) {
  const st = (j && j.status) || {};
  const completions = (j && j.spec && j.spec.completions) || 1;
  if ((st.succeeded || 0) >= completions) return 'Complete';
  if ((st.failed || 0) > 0) return 'Failed';
  if ((st.active || 0) > 0) return 'Running';
  return 'Scheduled';
}

// The one READY rule for the replica workloads (Deployment and StatefulSet,
// which carry the same field pair): readyReplicas over spec.replicas, the
// kubectl convention users know — never availableReplicas. DaemonSets are not
// served here: they report desiredNumberScheduled/numberReady instead.
export function workloadReady(o) {
  const desired = (o && o.spec && o.spec.replicas) || 0;
  const ready = (o && o.status && o.status.readyReplicas) || 0;
  return ready + '/' + desired;
}

// Relative age like "5s" / "12m" / "3h" / "4d"; a missing timestamp reads "—".
export function age(ts) {
  if (!ts) return '—';
  const s = Math.max(0, (Date.now() - new Date(ts).getTime()) / 1000);
  if (s < 90) return Math.round(s) + 's';
  const m = s / 60; if (m < 90) return Math.round(m) + 'm';
  const h = m / 60; if (h < 48) return Math.round(h) + 'h';
  return Math.round(h / 24) + 'd';
}

// Kubernetes quantity → millicores.
export function cpuToM(q) {
  if (q == null) return 0;
  if (typeof q === 'number') return q * 1000;
  const s = String(q);
  if (s.endsWith('m')) return parseFloat(s);
  if (s.endsWith('n')) return parseFloat(s) / 1e6;
  if (s.endsWith('u')) return parseFloat(s) / 1e3;
  return parseFloat(s) * 1000;
}

// Kubernetes quantity → MiB.
export function memToMi(q) {
  if (q == null) return 0;
  const m = String(q).match(/^(\d+(?:\.\d+)?)([A-Za-z]*)$/);
  if (!m) return 0;
  const n = parseFloat(m[1]);
  const u = { Ki: 1 / 1024, Mi: 1, Gi: 1024, Ti: 1048576, Pi: 1073741824,
              K: 1000 / 1048576, M: 1e6 / 1048576, G: 1e9 / 1048576, T: 1e12 / 1048576, '': 1 / 1048576 };
  return (m[2] in u) ? n * u[m[2]] : n / 1048576;
}
