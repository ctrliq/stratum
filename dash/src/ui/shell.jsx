// App shell + shared façades, all on the first-party controls in ./controls.jsx.
// - AppShell   -> the app frame: logo, titlebar (breadcrumbs), navbar, collapsible
//                 sidebar, page body, plus the cluster identity block under the logo.
// - StatusPill -> StatusBadge (via statusColor()).
// - Modal      -> Dialog.
// - Sparkline  -> the sparkline control (values=[...]).
// Screens import { StatusPill, Modal, Sparkline, statusColor } from '../ui/shell.jsx'.
//
// The frame's one piece of shared state — whether the sidebar is expanded —
// is a useState in AppShell, read by the frame, the nav items and the toggle.
import { useState, useRef, useEffect } from 'react';
import { Button, Badge, StatusBadge, Progress, Sparkline as BaseSparkline, Dialog } from './controls.jsx';
import { Icon } from '../icons.jsx';
import { navGroups } from '../registry.js';
import { sameCrdRoute } from '../crd-nav.js';
import stratumLogo from '../assets/stratum-logo.svg';
import stratumIcon from '../assets/stratum-icon.svg';

// The design system's viewport split: below it the sidebar is an overlay
// raised by a hamburger, at or above it a grid column that collapses to a rail.
const DESKTOP = '(min-width: 1024px)';

function useMediaQuery(query) {
  const [matches, setMatches] = useState(() => window.matchMedia(query).matches);
  useEffect(() => {
    const mq = window.matchMedia(query);
    const onChange = (e) => setMatches(e.matches);
    mq.addEventListener('change', onChange);
    setMatches(mq.matches);
    return () => mq.removeEventListener('change', onChange);
  }, [query]);
  return matches;
}

// Light-DOM dropdown menu. The panel must stay in the light DOM: inside a
// closed shadow root with no background it is see-through and unreachable
// by app CSS or ::part. It is styled from app.css (.menu-panel).
export function Menu({ trigger, minWidth = 200, children }) {
  const [open, setOpen] = useState(false);
  const ref = useRef(null);
  useEffect(() => {
    if (!open) return;
    const onDown = (e) => { if (ref.current && !ref.current.contains(e.target)) setOpen(false); };
    const onKey = (e) => { if (e.key === 'Escape') setOpen(false); };
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey);
    return () => { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey); };
  }, [open]);
  return (
    <div ref={ref} className="menu-anchor">
      <span onClick={() => setOpen((o) => !o)} style={{ display: 'inline-flex' }}>{trigger}</span>
      {open && (
        <div className="menu-panel" role="menu" style={{ minWidth }} onClick={() => setOpen(false)}>
          {children}
        </div>
      )}
    </div>
  );
}

export function MenuItem({ children, onClick, selected, disabled }) {
  return (
    <button type="button" role="menuitem" className={`menu-item${selected ? ' selected' : ''}`}
      disabled={disabled} aria-current={selected || undefined}
      onClick={disabled ? undefined : onClick}>
      {children}
    </button>
  );
}

// ---- status -> badge color -------------------------------------------------
// Kubernetes phases + the K8S.CLUSTER status values (healthy | degraded |
// disconnected | pending | provisioning).
const STATUS = {
  Running: ['success', 'Running'], Ready: ['success', 'Ready'], healthy: ['success', 'Healthy'],
  Complete: ['success', 'Complete'], Completed: ['success', 'Completed'], deployed: ['success', 'Deployed'],
  Deployed: ['success', 'Deployed'], Active: ['success', 'Active'], Bound: ['success', 'Bound'],
  Progressing: ['blue', 'Progressing'], Validating: ['blue', 'Validating'],
  Upgrading: ['blue', 'Upgrading'], pending: ['blue', 'Pending'], provisioning: ['blue', 'Provisioning'],
  Scheduled: ['gray', 'Scheduled'], Pending: ['gray', 'Pending'], Done: ['success', 'Done'],
  Degraded: ['warning', 'Degraded'], degraded: ['warning', 'Degraded'], Paused: ['warning', 'Paused'],
  SchedulingDisabled: ['warning', 'Cordoned'],
  Failed: ['error', 'Failed'], NotReady: ['error', 'NotReady'], CrashLoopBackOff: ['error', 'CrashLoop'],
  disconnected: ['error', 'Disconnected'],
  Terminating: ['warning', 'Terminating'],
};
export function statusColor(status) { return (STATUS[status] || ['gray'])[0]; }
export function statusLabel(status) { return (STATUS[status] || [null, status])[1]; }

// A node's dot colour. Readiness comes first — a cordoned node is still Ready,
// so cordon is only the amber middle state, never a stand-in for NotReady.
export const nodeTint = (n) => `var(--quantic-fg-${!n.ready ? 'error' : n.cordoned ? 'warning' : 'success'}-primary)`;

export function StatusPill({ status, size = 'sm' }) {
  return <StatusBadge color={statusColor(status)} size={size}>{statusLabel(status)}</StatusBadge>;
}

// ---- Modal façade over Dialog ---------------------------------------------
export function Modal({ open, onClose, title, subtitle, icon, size = 'md', children, footer }) {
  if (!open) return null;
  const header = title && (
    <div className="qc-dialog-title">
      <span className="row" style={{ gap: 8 }}>
        {icon && <Icon name={icon} size={16} />}
        {title}
      </span>
      {subtitle && <div className="dim" style={{ fontSize: 12, fontWeight: 400 }}>{subtitle}</div>}
    </div>
  );
  return (
    <Dialog size={size} onClose={onClose} header={header}
      footer={footer && <div className="modal-actions">{footer}</div>}>
      {children}
    </Dialog>
  );
}

// Is this keystroke the user typing into a field INSIDE `root`? Scoped to the
// pane on purpose: typing anywhere else on the page (the list's search box) must
// still leave Escape armed.
const TYPING_TAGS = new Set(['input', 'textarea']);
function isTypingInside(e, root) {
  if (!root) return false;
  const path = typeof e.composedPath === 'function' ? e.composedPath() : [e.target];
  if (!path.includes(root)) return false;
  return path.some((n) => n && n.nodeType === 1
    && (TYPING_TAGS.has(n.tagName.toLowerCase()) || n.isContentEditable));
}

// ---- PaneShell: detail-panel chrome for the right SplitterPane -------------
// Header (icon/title/subtitle + close), scrollable body, optional footer; fills
// its SplitterPane. Escape closes. This is the content of the right
// master-detail pane (the list sits in the left pane of the same SplitterGroup).
export function PaneShell({ title, subtitle, icon, onClose, footer, children }) {
  const rootRef = useRef(null);
  useEffect(() => {
    if (!onClose) return;
    // Escape closes the pane — but never while the user is typing in one of ITS
    // fields. The listener is on window, so without this guard an Escape inside
    // the config editor collapsed the pane and threw away the unsaved changes;
    // Cancel is the way out of edit mode.
    const k = (e) => { if (e.key === 'Escape' && !isTypingInside(e, rootRef.current)) onClose(); };
    window.addEventListener('keydown', k);
    return () => window.removeEventListener('keydown', k);
  }, [onClose]);
  return (
    <div ref={rootRef} className="pane-shell" role="region" aria-label={title || 'Details'}>
      <div className="pane-header">
        <div className="grow" style={{ minWidth: 0 }}>
          <div className="row pane-title" style={{ gap: 8 }}>
            {icon && (typeof icon === 'string' ? <Icon name={icon} size={16} /> : icon)}
            <span className="nowrap" style={{ minWidth: 0 }}>{title}</span>
          </div>
          {subtitle && <div className="pane-sub">{subtitle}</div>}
        </div>
        {onClose && (
          <button className="pane-close" onClick={onClose} title="Close" aria-label="Close">
            <Icon name="close" size={16} />
          </button>
        )}
      </div>
      <div className="pane-body">{children}</div>
      {footer && <div className="pane-footer">{footer}</div>}
    </div>
  );
}

// ---- Sparkline façade ------------------------------------------------------
export function Sparkline({ data, color = 'brand', variant = 'gradient' }) {
  return <BaseSparkline values={data} color={color} variant={variant} />;
}

// ---- Meter: compact CPU/Mem cell for dense tables --------------------------
// A width-bounded linear Progress + % label (a segmented capacity bar is too
// wide for a table row). Used by the workloads + pods tables.
export function Meter({ pct, w = 92 }) {
  if (pct == null || pct === 0) return <span className="dim">—</span>;
  const color = pct > 85 ? 'error' : pct > 70 ? 'warning' : 'success';
  return (
    <div className="row" style={{ gap: 8, width: w }}>
      <div style={{ flex: 1, minWidth: 0, overflow: 'hidden' }}>
        <Progress value={pct} color={color} size="sm" style={{ width: '100%' }} />
      </div>
      <span className="mono dim" style={{ fontSize: 11, width: 30, textAlign: 'right', flexShrink: 0 }}>{pct}%</span>
    </div>
  );
}

// Trend data comes from K8S.FLEET.history, a rolling buffer of real
// kubelet Summary API samples accumulated by live.js. Do not reintroduce a
// placeholder series: this dashboard has a no-mock-data policy (see data.js),
// and a synthetic curve visibly disagrees with the real per-node table
// beside it — render an empty or "collecting" state instead.

// ---- Cluster identity helpers (ClusterView → display strings) -------------
// "context" = displayName, else the first label value, else the name.
export function clusterContext(c) {
  if (!c) return '—';
  if (c.displayName && c.displayName !== c.name) return c.displayName;
  const vals = Object.values(c.labels || {});
  return vals[0] || c.displayName || c.name || c.id || '—';
}
export const clusterProvider = (c) => (c && c.provider) || '—';

// One sidebar row. `collapsed` is the desktop icon rail: the label and the
// trailing slot drop out and the icon centres, with the label as the tooltip.
function NavItem({ icon, label, selected, collapsed, after, onClick, variant }) {
  return (
    <button type="button" title={collapsed ? label : undefined}
      aria-label={collapsed ? label : undefined}
      className={`qc-nav-item${variant ? ` qc-nav-item--${variant}` : ''}`
        + `${selected ? ' is-selected' : ''}${collapsed ? ' is-collapsed' : ''}`}
      style={{ cursor: 'pointer' }} onClick={onClick}>
      {icon}
      {!collapsed && <span className="qc-nav-label">{label}</span>}
      {!collapsed && after}
    </button>
  );
}

// The stale-slice names the toggle's label may show, capped: an outage that
// stales every slice (an ended session names all of them) must not turn the
// label into a wall of names.
const staleLabel = (stale) => {
  const s = stale || [];
  return s.length <= 4 ? s.join(', ') : `${s.slice(0, 4).join(', ')} +${s.length - 4} more`;
};

// ---- AppShell — the app frame ---------------------------------------------
// `cluster` is this cluster's identity (K8S.CLUSTER, built by live.js); `config`
// the /api/v1/config payload; `me` the /api/v1/me payload (null when auth is off).
// `stale` is the last live load's slices that kept their last good data — the
// label may claim "refreshed" only when it is empty.
export function AppShell({ active, crdGroups, onNavigate, counts, crumbs, namespace, onNamespace, namespaces,
  live, updated, stale, onToggleLive, onPalette, cluster, config, me, onSignOut, children }) {
  // per-section collapse state, persisted across reloads
  const [navCollapsed, setNavCollapsed] = useState(() => {
    try { return JSON.parse(localStorage.getItem('stratum.navCollapsed') || '{}'); } catch { return {}; }
  });
  const setNavState = (key, value) => setNavCollapsed((c) => {
    const next = { ...c, [key]: value };
    try { localStorage.setItem('stratum.navCollapsed', JSON.stringify(next)); } catch { /* ignore */ }
    return next;
  });
  // A built-in group's state is stored under its id and a registered group's
  // under its label, each in its own namespace: a registration labelled
  // 'storage' is a different section from the built-in `storage` group and
  // must not collapse with it. A bare label key is what an earlier build
  // stored, so a section collapsed then stays collapsed now.
  const navKey = (g, gi) => (g.id ? `id:${g.id}` : g.label ? `label:${g.label}` : `_g${gi}`);
  const groupCollapsed = (g, key) => !!(navCollapsed[key] ?? navCollapsed[g.label]);

  // Whole-sidebar expand/collapse. Expanded on desktop, closed on a small
  // viewport until the hamburger opens it — and reset whenever that changes,
  // which is what the DS layout did on every breakpoint crossing.
  const desktop = useMediaQuery(DESKTOP);
  const [asideExpanded, setAsideExpanded] = useState(desktop);
  useEffect(() => { setAsideExpanded(desktop); }, [desktop]);
  // Escape dismisses the sidebar only while it is an overlay. The DS bound this
  // unconditionally, so on a desktop every Escape also collapsed the nav to the
  // rail — including the ones that close the detail pane or cancel an edit.
  useEffect(() => {
    if (desktop) return undefined;
    const k = (e) => { if (e.key === 'Escape') setAsideExpanded(false); };
    document.addEventListener('keydown', k);
    return () => document.removeEventListener('keydown', k);
  }, [desktop]);
  const rail = desktop && !asideExpanded;   // icon-only rail

  const ago = updated ? Math.max(0, Math.round((Date.now() - updated) / 1000)) : 0;
  // The toggle's honesty rule: the label may claim "refreshed" only when a load
  // actually stamped a time AND nothing from it went stale — stale data is
  // reported as stale, and a never-answered session claims no refresh at all.
  const staleSlices = stale || [];
  const liveLabel = staleSlices.length
    ? `Live — data stale (${staleLabel(staleSlices)}); click to pause`
    : updated ? `Live — refreshed ${ago}s ago; click to pause`
      : 'Live — waiting for the first refresh; click to pause';
  const links = (config && config.links) || {};
  // The sidebar is the registry's (src/registry.js): NAV plus whatever a
  // consumer registered. A link row is dropped unless the server's config
  // gives that link a URL.
  const groups = navGroups().map((g) => ({ ...g, items: g.items.filter((it) => !it.link || links[it.link]) })).filter((g) => g.items.length);
  const path = crumbs && crumbs.length ? crumbs : ['Overview'];
  // Inside the header bar on desktop, on its own row below it when the sidebar
  // becomes an overlay — where the DS layout put it.
  const breadcrumbs = (
    <nav className="qc-layout-titlebar crumbs" aria-label="Breadcrumb">
      <span className="crumb">{(cluster && cluster.name) || 'Cluster'}</span>
      {path.map((c, i) => (
        <span key={i} className="row" style={{ gap: 7, minWidth: 0 }}>
          <span className="crumb-sep">›</span>
          <span className={`crumb${i === path.length - 1 ? ' current' : ''}`}>{c}</span>
        </span>
      ))}
    </nav>
  );

  return (
    <div className={`qc-layout${asideExpanded ? ' is-aside-expanded' : ''}`}>
      <header className="qc-layout-header">
        {!desktop && (
          <Button variant="outline" size="sm" icon aria-label="Toggle sidebar"
            aria-expanded={asideExpanded} onClick={() => setAsideExpanded((v) => !v)}>
            <Icon name="menu" size={20} />
          </Button>
        )}
        {desktop && breadcrumbs}

        <div className="qc-layout-navbar qc-navbar">
          <Menu minWidth={190} trigger={
            <Button variant="outline" size="sm" style={{ minWidth: 170 }}>
              <Icon name="ns" size={13} />
              {namespace === 'all-namespaces' ? 'All namespaces' : namespace}
              <Icon name="chevDown" size={12} />
            </Button>
          }>
            {(namespaces || ['all-namespaces']).map((n) => {
              const lbl = n === 'all-namespaces' ? 'All namespaces' : n;
              return (
                <MenuItem key={n} selected={n === namespace} onClick={() => onNamespace(n)}>
                  {lbl}
                </MenuItem>
              );
            })}
          </Menu>
          {onPalette && (
            <button type="button" className="qc-navbar-item" onClick={onPalette}
              aria-label="Search — command palette (⌘K)">
              <Icon name="search" size={16} />
            </button>
          )}
          {onToggleLive && (
            <button type="button" className={`qc-navbar-item${live ? ' is-selected' : ''}`}
              onClick={onToggleLive}
              aria-label={live ? liveLabel : 'Paused — click to resume auto-refresh'}>
              <Icon name={live ? 'refresh' : 'play'} size={16} />
            </button>
          )}
          <UserMenu config={config} me={me} cluster={cluster} onSignOut={onSignOut} />
        </div>
      </header>

      {!desktop && breadcrumbs}

      <div className="qc-layout-backdrop" aria-hidden="true" onClick={() => setAsideExpanded(false)} />

      <aside className="qc-layout-aside" tabIndex={-1} inert={!desktop && !asideExpanded ? '' : undefined}>
        <div className="qc-layout-logo">
          {rail ? (
            <div className="brand" style={{ display: 'flex', justifyContent: 'center' }}>
              <img src={stratumIcon} alt="Stratum" height={28} />
            </div>
          ) : (
            <div className="row brand brand-full">
              <img src={stratumLogo} alt="Stratum" height={28} />
            </div>
          )}
        </div>

        <nav className="qc-sidebar" role="navigation" aria-label="Main">
          <div className="qc-sidebar-body">
            <ClusterIdentity cluster={cluster} />
            {groups.map((g, gi) => {
              const key = navKey(g, gi);
              const isCollapsed = !!g.label && groupCollapsed(g, key);
              return (
                <div key={key} className={`nav-group${isCollapsed ? ' collapsed' : ''}${g.label ? '' : ' nav-group-unlabeled'}`}>
                  {g.label ? (
                    <button type="button" className="nav-group-header" onClick={() => setNavState(key, !isCollapsed)}
                      aria-expanded={!isCollapsed} title={`${isCollapsed ? 'Expand' : 'Collapse'} ${g.label}`}>
                      <span className="grow nav-group-label">{g.label}</span>
                      <Icon name="chevDown" size={13} className="nav-group-chevron" />
                    </button>
                  ) : (gi > 0 && <div className="nav-group-header" aria-hidden="true" />)}
                  <div className="nav-items">
                    {g.items.map((it) => {
                      const href = it.link ? links[it.link] : null;
                      const onClick = href
                        ? () => window.open(href, '_blank', 'noopener,noreferrer')
                        : () => onNavigate(it.id);
                      const after = (
                        <>
                          {href && <Icon name="external" size={12} className="dim" />}
                          {it.count && counts && counts[it.count] != null && (
                            <Badge color="gray" size="sm">{counts[it.count]}</Badge>
                          )}
                        </>
                      );
                      return (
                        <NavItem key={it.id} label={it.label} selected={active === it.id}
                          collapsed={rail} onClick={onClick} after={after}
                          icon={<Icon name={it.icon} size={18} />} />
                      );
                    })}
                    {g.crds && !rail && (crdGroups || []).map((sg) => (
                      <CrdSubgroup key={sg.group} sub={sg} active={active} onNavigate={onNavigate}
                        stored={navCollapsed[`crd:${sg.group}`]}
                        onToggle={(open) => setNavState(`crd:${sg.group}`, open)} />
                    ))}
                  </div>
                </div>
              );
            })}
          </div>

          {desktop && (
            <div className="qc-sidebar-toggle">
              <NavItem variant="secondary" collapsed={rail}
                label={asideExpanded ? 'Collapse' : 'Expand sidebar'}
                onClick={() => setAsideExpanded((v) => !v)}
                icon={<Icon name={asideExpanded ? 'panelLeftClose' : 'panelLeft'} size={18} />} />
            </div>
          )}
        </nav>
      </aside>

      <main className="qc-layout-body">
        <div className="qc-page">{children}</div>
      </main>
    </div>
  );
}

// One CRD API group under Custom Resources: a header naming the group over a
// row per kind. Closed until opened, except that the group holding the page on
// screen opens itself; a stored choice, once made, wins either way. A row
// links to its CRD's first served version but owns every version of it, so
// /crds/<group>/<other version>/<plural> lights the same row.
function CrdSubgroup({ sub, active, stored, onToggle, onNavigate }) {
  const holdsActive = sub.items.some((it) => sameCrdRoute(it.id, active));
  const open = stored === undefined ? holdsActive : !!stored;
  return (
    <div className={`nav-subgroup${open ? '' : ' collapsed'}`}>
      <button type="button" className="nav-subgroup-header" aria-expanded={open}
        title={`${open ? 'Collapse' : 'Expand'} ${sub.group}`} onClick={() => onToggle(!open)}>
        <Icon name="chevDown" size={12} className="nav-subgroup-chevron" />
        <span className="grow nav-subgroup-label">{sub.group}</span>
      </button>
      {open && (
        <div className="nav-subgroup-items">
          {sub.items.map((it) => (
            <NavItem key={it.id} label={it.label} selected={sameCrdRoute(it.id, active)}
              onClick={() => onNavigate(it.id)} icon={<Icon name="puzzle" size={16} />} />
          ))}
        </div>
      )}
    </div>
  );
}

// ---- Cluster identity (under the logo) ------------------------------------
// Eyebrow "<context> · <provider>" over the cluster name. A downstream build
// would make this a switcher button; there is one cluster here, so it is a
// label. It keeps the .switcher-wrap/.sw-*
// classes so the icon-rail collapse rule in app.css still applies.
function ClusterIdentity({ cluster }) {
  const name = cluster ? (cluster.name || cluster.id) : 'Cluster';
  return (
    <div className="switcher-wrap">
      <div className="switcher row" title={name}
        style={{ width: '100%', justifyContent: 'flex-start', textAlign: 'left' }}>
        <span className="sw-glyph"><Icon name="cube" size={16} /></span>
        <span className="sw-text col" style={{ gap: 1, minWidth: 0, alignItems: 'flex-start' }}>
          <span className="eyebrow nowrap" style={{ maxWidth: '100%' }}>{clusterContext(cluster)} · {clusterProvider(cluster)}</span>
          <span className="sw-name nowrap" style={{ maxWidth: '100%' }}>{name}</span>
        </span>
      </div>
    </div>
  );
}

// ---- User menu -------------------------------------------------------------
// Bound to /api/v1/me when the server requires auth (the Kubernetes user the
// session's token belongs to, and Sign out). With auth.required === false
// there is no session at all: that is the kubectl-proxy dev mode, where
// whatever ran `kubectl proxy` is the identity.
//
// The header shows the last segment of a colon-separated identity: a service
// account token's `system:serviceaccount:<ns>:<name>` reads as `<name>`. The
// full identity stays in the menu and the tooltip, since the namespace is what
// tells two same-named accounts apart. A name with no colon, or one ending in
// a colon, is shown whole.
export function shortIdentity(name) {
  const s = String(name ?? '');
  const tail = s.slice(s.lastIndexOf(':') + 1);
  return tail || s;
}

// No role and no read-only badge: under bearer-token passthrough the
// dashboard does not know what the user may do. The apiserver decides per
// request, so claiming either here would be a guess.
function UserMenu({ config, me, cluster, onSignOut }) {
  const authed = !!(config && config.auth && config.auth.required);
  const username = (me && me.username) || (authed ? '—' : 'kubectl proxy');
  const k8sVer = (cluster && cluster.k8sVersion) || window.__STRATUM_K8S_VERSION || '';
  const ver = (config && config.version) || '';
  return (
    <Menu minWidth={250} trigger={
      <Button variant="ghost" size="sm" title={username}>
        <Icon name="user" size={15} /> {shortIdentity(username)} <Icon name="chevDown" size={12} />
      </Button>
    }>
      <MenuItem disabled>
        {username}{me && me.provider ? ` · ${me.provider}` : ''}
      </MenuItem>
      {authed && onSignOut && <MenuItem onClick={onSignOut}><Icon name="logout" size={13} /> Sign out</MenuItem>}
      <MenuItem disabled>
        <span className="mono" style={{ fontSize: 11 }}>Stratum {ver}{k8sVer ? ` · Kubernetes v${String(k8sVer).replace(/^v/, '')}` : ''}</span>
      </MenuItem>
    </Menu>
  );
}

// ---- Placeholder (screens not yet wired) ----------------------------------
export function Placeholder({ title = 'Coming soon' }) {
  return (
    <div className="content-wide col" style={{ gap: 12 }}>
      <h1 className="h1">{title}</h1>
      <div className="dim">This screen has not been built yet.</div>
    </div>
  );
}
