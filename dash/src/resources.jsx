// Resource registry — one descriptor per Kubernetes kind the sidebar exposes.
// The generic <ResourceList> engine (ui/resource-list.jsx) renders any descriptor:
// it fetches `path` through /k8s, filters by the selected namespace (if `namespaced`)
// and the search box, and renders `columns` in a <Table>. Add a kind = add an
// entry here (no new screen). Column render() receives the raw K8s object.
import { meta, nameOf, nsOf, labelPairs } from './k8sutil.js';
// The object rules the loader shares: one definition (src/k8s-rules.js) so the
// kind lists and the hand-written screens cannot drift the way the Deployment
// Ready cell once did.
import { age, podStatus, podReady, jobStatus, workloadReady } from './k8s-rules.js';
import { StatusPill } from './ui/shell.jsx';
import { reserveRoutes } from './registry.js';
import { servedVersion } from './crd-nav.js';

// ---- shared column helpers ------------------------------------------------
const T = (s) => <span className="mono" style={{ fontSize: 12 }}>{s}</span>;
const dim = (s) => <span className="dim mono" style={{ fontSize: 12 }}>{s ?? '—'}</span>;
const colName = { key: 'name', label: 'Name', render: (o) => <span className="mono node-name" style={{ fontSize: 12.5, fontWeight: 500 }}>{nameOf(o)}</span> };
const colNs = { key: 'ns', label: 'Namespace', render: (o) => dim(nsOf(o)) };
const colAge = { key: 'age', label: 'Age', render: (o) => dim(age(meta(o).creationTimestamp)) };
const pill = (s) => <StatusPill status={s} />;
const num = (n) => <span className="mono" style={{ fontSize: 12 }}>{n}</span>;

const ready = (rd, des) => `${rd || 0}/${des || 0}`;
const svcPorts = (s) => ((s.spec && s.spec.ports) || []).map((p) => `${p.port}${p.nodePort ? ':' + p.nodePort : ''}/${p.protocol || 'TCP'}`).join(', ') || '—';

// ---- the registry ---------------------------------------------------------
// path = all-namespaces list path; the engine filters namespaced kinds client-side.
export const RESOURCES = {
  // WORKLOADS
  pods: { label: 'Pods', kind: 'Pod', path: '/api/v1/pods', namespaced: true, pods: true,
    // write: true marks a row action as more than a read — resource-list.jsx
    // hides these when there is no session to act as. With one they
    // are offered and the apiserver decides. 'logs' has no such flag: it's a
    // read, and stays available.
    rowActions: [{ act: 'logs', icon: 'log', title: 'Logs' }, { act: 'exec', icon: 'terminal', title: 'Exec', write: true }],
    columns: [
    colName, colNs,
    { key: 'status', label: 'Status', render: (o) => pill(podStatus(o)) },
    { key: 'ready', label: 'Ready', render: (o) => T(podReady(o)) },
    { key: 'restarts', label: 'Restarts', render: (o) => num(((o.status && o.status.containerStatuses) || []).reduce((a, c) => a + (c.restartCount || 0), 0)) },
    { key: 'node', label: 'Node', render: (o) => dim((o.spec && o.spec.nodeName) || '—') },
    colAge ] },
  deployments: { label: 'Deployments', kind: 'Deployment', path: '/apis/apps/v1/deployments', namespaced: true, columns: [
    colName, colNs,
    { key: 'ready', label: 'Ready', render: (o) => T(workloadReady(o)) },
    { key: 'uptodate', label: 'Up-to-date', render: (o) => num((o.status || {}).updatedReplicas || 0) },
    { key: 'avail', label: 'Available', render: (o) => num((o.status || {}).availableReplicas || 0) },
    colAge ] },
  statefulsets: { label: 'StatefulSets', kind: 'StatefulSet', path: '/apis/apps/v1/statefulsets', namespaced: true, columns: [
    colName, colNs,
    { key: 'ready', label: 'Ready', render: (o) => T(workloadReady(o)) },
    colAge ] },
  daemonsets: { label: 'DaemonSets', kind: 'DaemonSet', path: '/apis/apps/v1/daemonsets', namespaced: true, columns: [
    colName, colNs,
    { key: 'desired', label: 'Desired', render: (o) => num((o.status || {}).desiredNumberScheduled || 0) },
    { key: 'ready', label: 'Ready', render: (o) => num((o.status || {}).numberReady || 0) },
    colAge ] },
  replicasets: { label: 'ReplicaSets', kind: 'ReplicaSet', path: '/apis/apps/v1/replicasets', namespaced: true, columns: [
    colName, colNs,
    { key: 'desired', label: 'Desired', render: (o) => num((o.spec || {}).replicas || 0) },
    { key: 'ready', label: 'Ready', render: (o) => num((o.status || {}).readyReplicas || 0) },
    colAge ] },
  replicationcontrollers: { label: 'Replication Controllers', kind: 'ReplicationController', path: '/api/v1/replicationcontrollers', namespaced: true, columns: [
    colName, colNs,
    { key: 'desired', label: 'Desired', render: (o) => num((o.spec || {}).replicas ?? 1) },
    { key: 'ready', label: 'Ready', render: (o) => num((o.status || {}).readyReplicas || 0) },
    colAge ] },
  jobs: { label: 'Jobs', kind: 'Job', path: '/apis/batch/v1/jobs', namespaced: true, columns: [
    colName, colNs,
    { key: 'completions', label: 'Completions', render: (o) => T(ready((o.status || {}).succeeded, (o.spec || {}).completions || 1)) },
    { key: 'status', label: 'Status', render: (o) => pill(jobStatus(o)) },
    colAge ] },
  cronjobs: { label: 'CronJobs', kind: 'CronJob', path: '/apis/batch/v1/cronjobs', namespaced: true, columns: [
    colName, colNs,
    { key: 'schedule', label: 'Schedule', render: (o) => T((o.spec || {}).schedule) },
    { key: 'suspend', label: 'Suspend', render: (o) => dim(String(!!(o.spec || {}).suspend)) },
    { key: 'active', label: 'Active', render: (o) => num(((o.status || {}).active || []).length) },
    colAge ] },

  // NETWORK
  services: { label: 'Services', kind: 'Service', path: '/api/v1/services', namespaced: true, columns: [
    colName, colNs,
    { key: 'type', label: 'Type', render: (o) => dim((o.spec || {}).clusterIP === 'None' ? 'Headless' : (o.spec || {}).type || 'ClusterIP') },
    { key: 'cip', label: 'Cluster IP', render: (o) => T((o.spec || {}).clusterIP || 'None') },
    { key: 'ports', label: 'Ports', render: (o) => dim(svcPorts(o)) },
    colAge ] },
  endpoints: { label: 'Endpoints', kind: 'Endpoints', path: '/api/v1/endpoints', namespaced: true, columns: [
    colName, colNs,
    { key: 'addrs', label: 'Addresses', render: (o) => dim(((o.subsets || []).flatMap((s) => (s.addresses || []).map((a) => a.ip))).slice(0, 4).join(', ') || '—') },
    colAge ] },
  endpointslices: { label: 'Endpoint Slices', kind: 'EndpointSlice', path: '/apis/discovery.k8s.io/v1/endpointslices', namespaced: true, columns: [
    colName, colNs,
    { key: 'service', label: 'Service', render: (o) => dim(meta(o).labels?.['kubernetes.io/service-name'] || '—') },
    { key: 'atype', label: 'Address Type', render: (o) => dim(o.addressType || '—') },
    { key: 'eps', label: 'Endpoints', render: (o) => num((o.endpoints || []).length) },
    { key: 'ports', label: 'Ports', render: (o) => dim((o.ports || []).map((p) => `${p.port ?? '*'}/${p.protocol || 'TCP'}`).join(', ') || '—') },
    colAge ] },
  ingresses: { label: 'Ingresses', kind: 'Ingress', path: '/apis/networking.k8s.io/v1/ingresses', namespaced: true, columns: [
    colName, colNs,
    { key: 'class', label: 'Class', render: (o) => dim((o.spec || {}).ingressClassName || '—') },
    { key: 'hosts', label: 'Hosts', render: (o) => dim(((o.spec || {}).rules || []).map((r) => r.host).filter(Boolean).join(', ') || '*') },
    colAge ] },
  ingressclasses: { label: 'Ingress Classes', kind: 'IngressClass', path: '/apis/networking.k8s.io/v1/ingressclasses', namespaced: false, columns: [
    colName,
    { key: 'controller', label: 'Controller', render: (o) => dim((o.spec || {}).controller) },
    colAge ] },
  networkpolicies: { label: 'Network Policies', kind: 'NetworkPolicy', path: '/apis/networking.k8s.io/v1/networkpolicies', namespaced: true, columns: [
    colName, colNs,
    { key: 'types', label: 'Policy Types', render: (o) => dim(((o.spec || {}).policyTypes || []).join(', ') || '—') },
    colAge ] },

  // CONFIG
  configmaps: { label: 'ConfigMaps', kind: 'ConfigMap', path: '/api/v1/configmaps', namespaced: true, columns: [
    colName, colNs,
    { key: 'keys', label: 'Keys', render: (o) => num(Object.keys(o.data || {}).length + Object.keys(o.binaryData || {}).length) },
    colAge ] },
  secrets: { label: 'Secrets', kind: 'Secret', path: '/api/v1/secrets', namespaced: true, columns: [
    colName, colNs,
    { key: 'type', label: 'Type', render: (o) => dim(o.type || 'Opaque') },
    { key: 'keys', label: 'Keys', render: (o) => num(Object.keys(o.data || {}).length) },
    colAge ] },
  resourcequotas: { label: 'Resource Quotas', kind: 'ResourceQuota', path: '/api/v1/resourcequotas', namespaced: true, columns: [
    colName, colNs,
    { key: 'scopes', label: 'Scopes', render: (o) => dim(((o.spec || {}).scopes || []).join(', ') || '—') },
    { key: 'hard', label: 'Hard Limits', render: (o) => num(Object.keys((o.status || {}).hard || (o.spec || {}).hard || {}).length) },
    colAge ] },
  limitranges: { label: 'Limit Ranges', kind: 'LimitRange', path: '/api/v1/limitranges', namespaced: true, columns: [
    colName, colNs,
    { key: 'limits', label: 'Limits', render: (o) => num(((o.spec || {}).limits || []).length) },
    colAge ] },
  horizontalpodautoscalers: { label: 'Horizontal Pod Autoscalers', kind: 'HorizontalPodAutoscaler', path: '/apis/autoscaling/v2/horizontalpodautoscalers', namespaced: true, columns: [
    colName, colNs,
    { key: 'ref', label: 'Reference', render: (o) => dim(`${((o.spec || {}).scaleTargetRef || {}).kind || '?'}/${((o.spec || {}).scaleTargetRef || {}).name || '?'}`) },
    { key: 'min', label: 'Min', render: (o) => num((o.spec || {}).minReplicas ?? '—') },
    { key: 'max', label: 'Max', render: (o) => num((o.spec || {}).maxReplicas ?? '—') },
    { key: 'cur', label: 'Replicas', render: (o) => num((o.status || {}).currentReplicas ?? 0) },
    colAge ] },
  poddisruptionbudgets: { label: 'Pod Disruption Budgets', kind: 'PodDisruptionBudget', path: '/apis/policy/v1/poddisruptionbudgets', namespaced: true, columns: [
    colName, colNs,
    { key: 'min', label: 'Min Available', render: (o) => dim(String((o.spec || {}).minAvailable ?? '—')) },
    { key: 'max', label: 'Max Unavailable', render: (o) => dim(String((o.spec || {}).maxUnavailable ?? '—')) },
    { key: 'cur', label: 'Current Healthy', render: (o) => num((o.status || {}).currentHealthy ?? 0) },
    colAge ] },
  priorityclasses: { label: 'Priority Classes', kind: 'PriorityClass', path: '/apis/scheduling.k8s.io/v1/priorityclasses', namespaced: false, columns: [
    colName,
    { key: 'value', label: 'Value', render: (o) => num(o.value) },
    { key: 'default', label: 'Global Default', render: (o) => dim(String(!!o.globalDefault)) },
    colAge ] },
  runtimeclasses: { label: 'Runtime Classes', kind: 'RuntimeClass', path: '/apis/node.k8s.io/v1/runtimeclasses', namespaced: false, columns: [
    colName,
    { key: 'handler', label: 'Handler', render: (o) => T(o.handler) },
    colAge ] },
  leases: { label: 'Leases', kind: 'Lease', path: '/apis/coordination.k8s.io/v1/leases', namespaced: true, columns: [
    colName, colNs,
    { key: 'holder', label: 'Holder', render: (o) => dim((o.spec || {}).holderIdentity || '—') },
    colAge ] },
  mutatingwebhookconfigurations: { label: 'Mutating Webhooks', kind: 'MutatingWebhookConfiguration', path: '/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations', namespaced: false, columns: [
    colName,
    { key: 'hooks', label: 'Webhooks', render: (o) => num((o.webhooks || []).length) },
    colAge ] },
  validatingwebhookconfigurations: { label: 'Validating Webhooks', kind: 'ValidatingWebhookConfiguration', path: '/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations', namespaced: false, columns: [
    colName,
    { key: 'hooks', label: 'Webhooks', render: (o) => num((o.webhooks || []).length) },
    colAge ] },

  // STORAGE
  persistentvolumeclaims: { label: 'Persistent Volume Claims', kind: 'PersistentVolumeClaim', path: '/api/v1/persistentvolumeclaims', namespaced: true, columns: [
    colName, colNs,
    { key: 'status', label: 'Status', render: (o) => pill((o.status || {}).phase) },
    { key: 'cap', label: 'Capacity', render: (o) => T(((o.status || {}).capacity || {}).storage || '—') },
    { key: 'sc', label: 'Storage Class', render: (o) => dim((o.spec || {}).storageClassName || '—') },
    colAge ] },
  persistentvolumes: { label: 'Persistent Volumes', kind: 'PersistentVolume', path: '/api/v1/persistentvolumes', namespaced: false, columns: [
    colName,
    { key: 'cap', label: 'Capacity', render: (o) => T(((o.spec || {}).capacity || {}).storage || '—') },
    { key: 'reclaim', label: 'Reclaim', render: (o) => dim((o.spec || {}).persistentVolumeReclaimPolicy) },
    { key: 'status', label: 'Status', render: (o) => pill((o.status || {}).phase) },
    { key: 'claim', label: 'Claim', render: (o) => dim((o.spec || {}).claimRef ? `${o.spec.claimRef.namespace}/${o.spec.claimRef.name}` : '—') },
    { key: 'sc', label: 'Storage Class', render: (o) => dim((o.spec || {}).storageClassName || '—') },
    colAge ] },
  storageclasses: { label: 'Storage Classes', kind: 'StorageClass', path: '/apis/storage.k8s.io/v1/storageclasses', namespaced: false, columns: [
    colName,
    { key: 'prov', label: 'Provisioner', render: (o) => T(o.provisioner) },
    { key: 'reclaim', label: 'Reclaim', render: (o) => dim(o.reclaimPolicy || 'Delete') },
    { key: 'binding', label: 'Binding', render: (o) => dim(o.volumeBindingMode || 'Immediate') },
    colAge ] },

  // ACCESS
  serviceaccounts: { label: 'Service Accounts', kind: 'ServiceAccount', path: '/api/v1/serviceaccounts', namespaced: true, columns: [
    colName, colNs,
    { key: 'secrets', label: 'Secrets', render: (o) => num((o.secrets || []).length) },
    colAge ] },
  roles: { label: 'Roles', kind: 'Role', path: '/apis/rbac.authorization.k8s.io/v1/roles', namespaced: true, columns: [
    colName, colNs, { key: 'rules', label: 'Rules', render: (o) => num((o.rules || []).length) }, colAge ] },
  rolebindings: { label: 'Role Bindings', kind: 'RoleBinding', path: '/apis/rbac.authorization.k8s.io/v1/rolebindings', namespaced: true, columns: [
    colName, colNs, { key: 'role', label: 'Role', render: (o) => dim((o.roleRef || {}).name) }, colAge ] },
  clusterroles: { label: 'Cluster Roles', kind: 'ClusterRole', path: '/apis/rbac.authorization.k8s.io/v1/clusterroles', namespaced: false, columns: [
    colName, { key: 'rules', label: 'Rules', render: (o) => num((o.rules || []).length) }, colAge ] },
  clusterrolebindings: { label: 'Cluster Role Bindings', kind: 'ClusterRoleBinding', path: '/apis/rbac.authorization.k8s.io/v1/clusterrolebindings', namespaced: false, columns: [
    colName, { key: 'role', label: 'Role', render: (o) => dim((o.roleRef || {}).name) }, colAge ] },

  // CLUSTER
  namespaces: { label: 'Namespaces', kind: 'Namespace', path: '/api/v1/namespaces', namespaced: false, columns: [
    colName,
    { key: 'status', label: 'Status', render: (o) => pill((o.status || {}).phase || 'Active') },
    { key: 'labels', label: 'Labels', render: (o) => dim(labelPairs(o).slice(0, 3).join(', ')) },
    colAge ] },

  // EXTENSIONS
  crds: { label: 'Custom Resource Definitions', kind: 'CustomResourceDefinition',
    path: '/apis/apiextensions.k8s.io/v1/customresourcedefinitions', namespaced: false,
    rowActions: [{ act: 'open-crd', icon: 'search', title: 'Browse instances' }],
    columns: [
      colName,
      { key: 'group', label: 'Group', render: (o) => dim((o.spec || {}).group || '—') },
      { key: 'ckind', label: 'Kind', render: (o) => T(((o.spec || {}).names || {}).kind) },
      { key: 'scope', label: 'Scope', render: (o) => dim((o.spec || {}).scope || '—') },
      { key: 'versions', label: 'Versions', render: (o) => dim(((o.spec || {}).versions || []).map((v) => v.name).join(', ') || '—') },
      colAge ] },
  helmcharts: { label: 'Helm Charts', kind: 'HelmChart', path: '/apis/stratum.ciq.com/v1/helmcharts', namespaced: true, columns: [
      colName, colNs,
      { key: 'chart', label: 'Chart', render: (o) => T((o.spec || {}).chart || '—') },
      { key: 'version', label: 'Version', render: (o) => dim((o.spec || {}).version || '—') },
      { key: 'target', label: 'Target NS', render: (o) => dim((o.spec || {}).targetNamespace || '—') },
      { key: 'phase', label: 'Phase', render: (o) => pill((o.status || {}).phase || 'Pending') },
      colAge ] },
  // Values overlaid onto the same-name+namespace HelmChart. No status subresource
  // (internal/addons/manifests/helmchart-crds.yaml), so there is no Phase column.
  helmchartconfigs: { label: 'Helm Chart Configs', kind: 'HelmChartConfig', path: '/apis/stratum.ciq.com/v1/helmchartconfigs', namespaced: true, columns: [
      colName, colNs,
      { key: 'values', label: 'Values', render: (o) => dim(((o.spec || {}).valuesContent || '').split('\n').filter((l) => l.trim()).length + ' lines') },
      colAge ] },
};

// ---- write capability per kind --------------------------------------------
// Which write ops the detail modal offers. Scale = deployments/statefulsets/
// replicasets; Restart (rollout) = deployments/statefulsets/daemonsets; edit +
// delete = every kind. Whether a write SUCCEEDS is still gated by rbac.yaml.
const CAN_SCALE = new Set(['deployments', 'statefulsets', 'replicasets', 'replicationcontrollers']);
const CAN_RESTART = new Set(['deployments', 'statefulsets', 'daemonsets']);
// The icon each kind shows in the sidebar and the command palette.
const ICONS = {
  pods: 'box', deployments: 'cube', statefulsets: 'layers', daemonsets: 'server', replicasets: 'copy',
  replicationcontrollers: 'copy', jobs: 'log', cronjobs: 'clock',
  services: 'share', endpoints: 'link', endpointslices: 'link', ingresses: 'globe', ingressclasses: 'globe', networkpolicies: 'shield',
  configmaps: 'sliders', secrets: 'key', resourcequotas: 'gauge', limitranges: 'sliders', horizontalpodautoscalers: 'arrowUp',
  poddisruptionbudgets: 'shield', priorityclasses: 'layers', runtimeclasses: 'cube', leases: 'clock',
  mutatingwebhookconfigurations: 'link', validatingwebhookconfigurations: 'shield',
  persistentvolumeclaims: 'database', persistentvolumes: 'database', storageclasses: 'layers',
  serviceaccounts: 'user', roles: 'shield', rolebindings: 'link', clusterroles: 'shield', clusterrolebindings: 'link',
  namespaces: 'ns', crds: 'puzzle', helmcharts: 'helm', helmchartconfigs: 'helm',
};
for (const [id, d] of Object.entries(RESOURCES)) {
  d.icon = ICONS[id] || 'box';
  d.ops = ['edit', ...(CAN_SCALE.has(id) ? ['scale'] : []), ...(CAN_RESTART.has(id) ? ['restart'] : []), 'delete'];
}

// Split a list path "<base>/<plural>" and build the instance / create paths.
// The registry `path` is the all-namespaces list path; a namespaced instance
// inserts /namespaces/<ns> before the plural.
function splitPath(p) { const i = p.lastIndexOf('/'); return [p.slice(0, i), p.slice(i + 1)]; }
// Every descriptor key is a route this dashboard owns: app.jsx resolves
// RESOURCES *after* the extension registry, so without this a consumer could
// register `deployments` or `persistentvolumes` and silently replace the
// built-in list. Declared from here rather than listed in registry.js because
// this object IS the list, and registry.js cannot import it — the import edge
// already runs the other way (this file → ui/shell.jsx → registry.js).
reserveRoutes(Object.keys(RESOURCES));

export function instancePath(descriptor, o) {
  const [base, plural] = splitPath(descriptor.path);
  return descriptor.namespaced
    ? `${base}/namespaces/${nsOf(o)}/${plural}/${nameOf(o)}`
    : `${descriptor.path}/${nameOf(o)}`;
}
// POST target for a new object; namespaced create needs the object's (or the
// currently-selected) namespace.
export function createPath(descriptor, obj, fallbackNs) {
  const [base, plural] = splitPath(descriptor.path);
  if (!descriptor.namespaced) return descriptor.path;
  const ns = (obj && obj.metadata && obj.metadata.namespace) || fallbackNs || 'default';
  return `${base}/namespaces/${ns}/${plural}`;
}

// Build a live descriptor from a CustomResourceDefinition object so the generic
// engine can browse that CRD's instances.
// Uses the CRD's served version (falls back to the first) + its plural/scope/kind.
export function crdDescriptor(crd) {
  const spec = crd.spec || {};
  const v = servedVersion(spec);
  const group = spec.group;
  const plural = (spec.names || {}).plural;
  const kind = (spec.names || {}).kind || plural;
  const namespaced = spec.scope === 'Namespaced';
  const base = group ? `/apis/${group}/${v}` : `/api/${v}`;
  return {
    label: `${kind} (${group || 'core'})`, kind,
    path: `${base}/${plural}`, namespaced, crd: true,
    columns: [colName, ...(namespaced ? [colNs] : []), colAge],
    ops: ['edit', 'delete'],
  };
}

// items with the same shape across API groups; search default = name + namespace.
export function resourceSearch(descriptor, o) {
  return (nameOf(o) + ' ' + nsOf(o)).toLowerCase();
}
