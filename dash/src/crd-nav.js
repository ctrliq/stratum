// The Custom Resources section of the sidebar: one sub-group per CRD API
// group, one row per kind. DOM-free so node:test can pin it over fixtures.
//
// A row's id is also its route: `crds/<group>/<version>/<plural>` is the path
// app.jsx's parseLocation() resolves to that CRD's instance list, so the
// shell's onNavigate(id) needs no special case for these rows.

// The version a CRD's instances are read at: the first served one, else the
// first listed. The sidebar rows and the instance list must agree on it.
export function servedVersion(spec) {
  const versions = (spec && spec.versions) || [];
  return (versions.find((x) => x.served) || versions[0] || {}).name || 'v1';
}

export function crdRoute(spec) {
  return `crds/${spec.group}/${servedVersion(spec)}/${(spec.names || {}).plural}`;
}

// Whether two CRD routes name the same resource: the same API group and
// plural, whatever the version. Anything that is not a CRD route matches
// nothing.
export function sameCrdRoute(a, b) {
  const parse = (r) => {
    const parts = typeof r === 'string' ? r.split('/') : [];
    return parts.length === 4 && parts[0] === 'crds' ? `${parts[1]}/${parts[3]}` : null;
  };
  const ka = parse(a);
  return ka !== null && ka === parse(b);
}

export function crdNavGroups(crds) {
  const byGroup = new Map();
  for (const crd of crds || []) {
    const spec = (crd && crd.spec) || {};
    const plural = (spec.names || {}).plural;
    if (!spec.group || !plural) continue;
    if (!byGroup.has(spec.group)) byGroup.set(spec.group, []);
    byGroup.get(spec.group).push({ id: crdRoute(spec), label: (spec.names || {}).kind || plural });
  }
  const byLabel = (a, b) => a.label.localeCompare(b.label) || a.id.localeCompare(b.id);
  return [...byGroup.keys()].sort().map((group) => ({ group, items: byGroup.get(group).sort(byLabel) }));
}
