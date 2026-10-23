/* global window */
// ============================================================
// node-shell.js — lifecycle of the node host-shell debug Job.
//
// The `kubectl debug node/` pattern, as a batch/v1 Job: the Job's template is
// the privileged hostPID pod pinned to the node, which terminal.jsx's NodeShell
// nsenters into through the Job-derived pod (the pod's name is the Job's
// generateName plus a suffix). The Job lives here rather than inside the
// component's effect because the create/delete pairing is the part worth
// testing, and so are the two stops no component can make: the drawer closing
// while the create POST is still in flight (the POST's own continuation
// deletes), and the tab closing (the pagehide handler below sends each open
// Job's delete best-effort). Whatever the deletes fail to land, the Job's
// activeDeadlineSeconds ends it in the cluster — and once it is finished, the
// TTL controller's ttlSecondsAfterFinished deletes the finished Job, its pod
// with it, with no session needed.
// ============================================================
import { currentSessionRevision, subscribeSessionChanges } from './session.js';
import { k8sGet, k8sPost, k8sFetch } from './k8sutil.js';

export const NODE_SHELL_NS = 'default';
// The name prefix, node label and delete query the server's node-shell gate
// keys on (internal/dashboard/nodeshell.go judges a Job create by the
// template's name prefix and hostPID, and exec/attach by the Job-derived pod's
// name prefix) — defined once here, beside the create body and the owned-shell
// registry.
export const NODE_SHELL_PREFIX = 'stratum-node-shell-';
export const NODE_SHELL_NODE_LABEL = 'stratum.ciq.com/node-shell';
// Job deletes default to orphaning pods; every close must request cascading GC.
export const NODE_SHELL_DELETE_QUERY = '?gracePeriodSeconds=1&propagationPolicy=Background';
const JOBS = `/apis/batch/v1/namespaces/${NODE_SHELL_NS}/jobs`;
const PODS = `/api/v1/namespaces/${NODE_SHELL_NS}/pods`;
const POLL_MS = 1500, POLL_TRIES = 80; // ~2min, enough for a cold image pull

// The longest host-shell session the dashboard offers, in seconds. It is the
// pod template's spec.activeDeadlineSeconds and the Job's own deadline, and
// the container sleeps exactly this long: the deadline is the shell's real
// lifetime, and a shorter sleep would cut a session short for no gain while a
// longer one would never be reached.
const SHELL_MAX_SECONDS = 3600;

// How long a finished Job may linger before the TTL controller deletes it —
// cleanup that needs no session, tab or sweep. The Job finishes
// when its pod exits (the sleep ends, the pod fails, or a deadline hit), and
// deleting the Job by hand only accelerates the same teardown.
const TTL_AFTER_FINISHED_SECONDS = 600;

// Only this lifecycle can register a host shell. A matching name returned
// to another editor is not evidence that the dashboard owns it. Sign-out
// reads both the registered Jobs and creates still waiting for an API answer.
const owned = new Map();
const pendingCreates = new Set();
let session = 0;
export function openNodeShells() { return [...owned.values()]; }
export function pendingNodeShellCreates() { return [...pendingCreates]; }
// Cleanup authority belongs to the session that created a shell. Once it
// ends, old stop callbacks and delayed creates must not use new credentials.
export function endNodeShellSession() {
  session += 1;
  owned.clear();
  pendingCreates.clear();
  open.clear();
}

subscribeSessionChanges(endNodeShellSession);

// Creates the Job, reports its progress through the callbacks, and returns the
// stop function that tears it down. onPod(podName) fires once the Job's pod
// exists — the Job-derived pod name the drawer execs into, not the Job's own
// name — onReady() once it is Running, onError(message) on any failure; none
// of them fire after stop().
//
// `image` is the container image for the debug pod (the Job template's one
// container), and it comes from the server (/api/v1/config →
// features.nodeShellImage), never a literal here: the payload-carried busybox
// so the pod starts on an airgapped node with no registry. Fail closed if it
// is missing — building a pod with a default image is exactly how the old
// hard-coded `alpine:3.20` reached Docker Hub.
export function startNodeShell(node, image, { onPod, onReady, onError }) {
  currentSessionRevision();
  const startedIn = session;
  if (!image) {
    // Report through the same channel as any other failure and create nothing.
    // Honor the same contract as the normal path — nothing fires after stop() —
    // by gating the deferred callback behind a cancellation flag the returned
    // stop sets, so an unmount before the microtask runs stays silent.
    let cancelled = false;
    queueMicrotask(() => { if (!cancelled) onError('the server did not provide a node-shell image (features.nodeShellImage); this build cannot open a host shell'); });
    return () => { cancelled = true; };
  }
  let cancelled = false, created = null;
  // Fire-and-forget delete (k8sFetch never throws), called by whichever half
  // knows the Job's name: the stop function when the create POST has already
  // answered, the POST's own continuation when stop got there first, or the
  // pagehide handler below when the tab goes away. The delete is against the
  // JOB — accelerating the TTL teardown — never the pod, which the garbage
  // collector removes with the Job. The name is forgotten only once the
  // apiserver has accepted the DELETE, so a transient failure is retried by
  // whichever half runs next rather than stranding a privileged Job; a 404
  // means someone else got there first. Only the pagehide half passes
  // keepalive: the page that fired it is going away, so the request has to
  // survive on its own and nobody can await the answer.
  const destroy = (keepalive) => {
    currentSessionRevision();
    if (!created || startedIn !== session) return;
    k8sFetch('DELETE', `${JOBS}/${created}${NODE_SHELL_DELETE_QUERY}`, { keepalive })
      .then((d) => { if (d.ok || d.status === 404) retire(); });
  };
  // The pagehide half of the pairing: the same delete destroy() sends, on a
  // keepalive request. Registered once per shell, forgotten with the name.
  const unloadDelete = () => destroy(true);
  const retire = () => { owned.delete(created); created = null; open.delete(unloadDelete); };
  const body = {
    apiVersion: 'batch/v1', kind: 'Job',
    metadata: { generateName: NODE_SHELL_PREFIX, labels: { app: 'stratum-node-shell', [NODE_SHELL_NODE_LABEL]: node } },
    spec: {
      // The TTL controller deletes a finished Job — its pod with it — this
      // long after it finishes, with no session and no sweep. The
      // drawer-close, pagehide and sign-out deletes are only
      // best-effort acceleration of that same teardown.
      ttlSecondsAfterFinished: TTL_AFTER_FINISHED_SECONDS,
      // Bounds the Job itself: a pod that never starts (an unschedulable
      // node) would otherwise hold the Job open past any session, because
      // the pod's own deadline never begins.
      activeDeadlineSeconds: SHELL_MAX_SECONDS,
      // One shell per create, as the bare pod was: the default backoffLimit
      // (6) would replace a failed shell pod with another privileged hostPID
      // pod the drawer never execs into. A failed pod fails the Job, and the
      // TTL teardown above owns what happens next.
      backoffLimit: 0,
      template: {
        metadata: { labels: { app: 'stratum-node-shell', [NODE_SHELL_NODE_LABEL]: node } },
        spec: {
          nodeName: node, hostPID: true, restartPolicy: 'Never',
          // The kubelet bounds active execution from the pod's start time,
          // independently of the tab. The deadline does not delete the Pod
          // object. The server gate reads the template's name prefix and
          // hostPID.
          activeDeadlineSeconds: SHELL_MAX_SECONDS,
          tolerations: [{ operator: 'Exists' }],
          containers: [{
            name: 'shell', image, command: ['sleep', String(SHELL_MAX_SECONDS)],
            stdin: true, tty: true, securityContext: { privileged: true },
          }],
        },
      },
    },
  };
  const creating = k8sPost(JOBS, body);
  pendingCreates.add(creating);
  creating.then(() => pendingCreates.delete(creating), () => pendingCreates.delete(creating));
  (async () => {
    try {
      const r = await creating;
      if (startedIn !== session) return;
      if (!r.ok) throw new Error('create debug job → HTTP ' + r.status + ' ' + String(r.error || '').slice(0, 180));
      created = r.body.metadata.name;
      owned.set(created, { pod: created, path: JOBS, node, retire });
      open.add(unloadDelete);
      if (cancelled) { destroy(); return; }
      // The create answers with the Job, not the pod. The batch controller
      // stamps the Job with the selector its pods carry, so the pod the
      // drawer execs into is found by listing pods with that selector once
      // it exists; its name is the Job's generateName plus a suffix.
      const match = r.body.spec && r.body.spec.selector && r.body.spec.selector.matchLabels;
      const selector = match && Object.keys(match).length
        ? Object.entries(match).map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(v)}`).join(',')
        : 'job-name=' + encodeURIComponent(created);
      let pod = null, announced = false;
      for (let i = 0; i < POLL_TRIES && !cancelled; i++) {
        await new Promise((s) => setTimeout(s, POLL_MS));
        if (cancelled || startedIn !== session) return;
        if (!pod) {
          const list = await k8sGet(`${PODS}?labelSelector=${selector}`, null);
          if (cancelled || startedIn !== session) return;
          const items = (list && Array.isArray(list.items)) ? list.items : [];
          if (items.length && items[0] && items[0].metadata && items[0].metadata.name) pod = items[0].metadata.name;
        }
        if (!pod) continue;
        if (!announced) { announced = true; onPod(pod); }
        const pd = await k8sGet(`${PODS}/${pod}`, null);
        if (cancelled || startedIn !== session) return;
        if (!pd) continue;
        const ph = pd.status && pd.status.phase;
        if (ph === 'Running') { if (!cancelled) onReady(); return; }
        if (ph === 'Failed' || ph === 'Succeeded') throw new Error('debug pod ' + ph);
      }
      if (!cancelled) throw new Error('timed out waiting for the debug pod to start (slow image pull on the node?)');
    } catch (e) {
      // The error panel stays mounted, so cleanup will not run for as long as
      // the operator leaves it open — and on a timeout the Job may still come
      // up and sit there privileged for the rest of its hour while the UI says
      // it failed. Delete it here; destroy() is what keeps the eventual cleanup
      // from repeating a DELETE that has already landed.
      if (!cancelled && startedIn === session) { destroy(); onError(e.message); }
    }
  })();
  return () => {
    cancelled = true;
    destroy();
  };
}

// ---- the tab going away ----------------------------------------------------
// Every stop above is one the page performs; the tab closing is the one stop
// nothing running on the page can perform. So the module keeps the shells whose
// Job may still exist and registers one pagehide handler that fires each of
// their deletes as the tab goes away. The request rides a keepalive fetch —
// the one delivery the browser may still attempt from a page that is unloading
// — rather than navigator.sendBeacon, which cannot carry the X-Stratum-CSRF
// header the proxy requires on every write. It is best-effort in every
// direction: the browser may drop the request mid-teardown (pagehide does not
// guarantee delivery), the apiserver may refuse what does arrive, and neither
// failure may be surfaced — the page is going away either way. What bounds the
// Job when nothing here lands is its activeDeadlineSeconds plus the TTL
// controller's ttlSecondsAfterFinished, not this handler.
const open = new Set();
// Guarded so importing the module without a window (a non-browser context)
// stays harmless; every real caller has one, as does the test's stub.
if (typeof window !== 'undefined' && typeof window.addEventListener === 'function') {
  window.addEventListener('pagehide', () => {
    currentSessionRevision();
    for (const unloadDelete of [...open]) unloadDelete();
  });
}
