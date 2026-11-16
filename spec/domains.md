# Spec domains

Project configuration for the `define-spec`, `verify-spec`,
`implement-spec` and `accept-spec` skills. The spec captures what the two
existing dashboards do, as the acceptance checklist for the unified rewrite
in `dash-next/`.

## Source roots

| Alias | Path | What |
|---|---|---|
| `oss` | `~/Code/stratum/dash` | open single-cluster dashboard |
| `pro` | `~/Code/stratum-pro/stratum-dash` | fleet dashboard |

Citations are `# source: <alias>:<path>:<lines>`, path relative to the root.

## Tags

| Tag | Meaning |
|---|---|
| `@oss` | the open dashboard does this today |
| `@pro` | the fleet dashboard does this today |
| `@diverged` | both do it, differently; one scenario per variant, each also tagged with its root |
| `@drop` | deliberately not carried into the rewrite; a `# decision:` comment gives the reason |
| `@fleet` | only meaningful when the backend is a fleet hub |
| `@fixed` | corrected behavior the rewrite implements in place of a legacy defect; sits beside the `@drop` scenario recording the defect |

A scenario identical in both roots carries `@oss @pro` and both citations.

## Domains

The legacy code is organised by technical layer, not by domain, so several
domains share source files; a domain owns the parts of a file that concern
it. Implementation paths are relative to `~/Code/stratum`.

| Domain | Legacy sources | Implementation path |
|---|---|---|
| `auth` | oss: `src/session.js`, `src/screens/login.jsx`, auth parts of `src/app.jsx`; pro: `src/fleet.js` (login, logout, me, CSRF), `src/auth-redirect.js`, `src/screens/login.jsx`, `src/ui/login-network.jsx`, auth parts of `src/app.jsx` | `dash-next/src/domain/auth` |
| `shell` | oss: `src/app.jsx`, `src/ui/shell.jsx`, `src/ui/palette.jsx`, `src/cluster-context.js`, `src/version.js`, `src/registry.js`, `src/index.js`, `src/main.jsx`; pro: `src/app.jsx`, `src/ui/shell.jsx`, `src/ui/palette.jsx`, `src/nav.js`, `src/cluster-context.js`, `src/version.js`, `src/version-view.js`, `src/main.jsx` | `dash-next/src/domain/shell` |
| `data` | oss: `src/live.js`, `src/data.js`, `src/k8sutil.js`, `src/k8s-rules.js`; pro: `src/live.js`, `src/data.js`, `src/k8sutil.js`, `src/k8srules.js`, `src/k8sproxy.js`, `src/k8sclient.js`, `src/k8sident.js`, `src/load-outcome.js`, `src/poll.js` | `dash-next/src/shared` and `dash-next/src/api` |
| `overview` | both: `src/screens/overview.jsx` | `dash-next/src/domain/overview` |
| `resources` | both: `src/resources.jsx`, `src/ui/resource-list.jsx`, `src/screens/tabbed.jsx`, `src/ui/codeview.jsx` — the generic list, detail, YAML view/edit and delete behavior every kind shares | `dash-next/src/shared/components/ResourceList` and `dash-next/src/domain/resources` |
| `workloads` | both: `src/screens/workloads.jsx`, workload descriptors in `src/resources.jsx` (pods, deployments, statefulsets, daemonsets, jobs, cronjobs) | `dash-next/src/domain/workloads` |
| `networking` | both: descriptors in `src/resources.jsx` (services, ingresses, ingress classes, network policies, endpoint slices) | `dash-next/src/domain/networking` |
| `storage` | both: descriptors in `src/resources.jsx`, storage tab of `src/screens/tabbed.jsx` | `dash-next/src/domain/storage` |
| `config` | both: descriptors in `src/resources.jsx` (configmaps, secrets) | `dash-next/src/domain/config` |
| `access` | both: descriptors in `src/resources.jsx`, RBAC tab of `src/screens/tabbed.jsx` | `dash-next/src/domain/access` |
| `namespaces` | both: descriptors in `src/resources.jsx`, namespace handling in `src/ui/shell.jsx` | `dash-next/src/domain/namespaces` |
| `crds` | both: `src/crd-nav.js`, CRD parts of `src/resources.jsx` and `src/app.jsx`; pro: `src/crd-columns.js` | `dash-next/src/domain/crds` |
| `nodes` | both: nodes part of `src/screens/infra.jsx`; oss: `src/node-shell.js` | `dash-next/src/domain/nodes` |
| `events` | both: events part of `src/screens/infra.jsx` | `dash-next/src/domain/events` |
| `helm` | both: helm part of `src/screens/infra.jsx` | `dash-next/src/domain/helm` |
| `terminal` | both: `src/screens/terminal.jsx`, `src/ui/exec-logs-drawer.jsx`; oss: `src/ui/split.js` | `dash-next/src/domain/terminal` |
| `clusters` | pro: `src/screens/clusters.jsx`, fleet parts of `src/fleet.js`, `src/enginestate.js`, `src/ui/enginestate.jsx`, cluster switching in `src/k8sclient.js` and `src/ui/shell.jsx` | `dash-next/src/domain/clusters` |
| `operations` | pro: `src/screens/operations.jsx`, operations parts of `src/fleet.js`, `src/poll.js` | `dash-next/src/domain/operations` |
| `catalog` | pro: `src/screens/catalog.jsx`, `src/catalog.js`, `src/catalog-rules.js`, `src/ui/fields.jsx` | `dash-next/src/domain/catalog` |
| `hardening` | pro: `src/screens/hardening.jsx`, `src/evidence-backup.jsx`, `src/evidence-certificate.jsx` | `dash-next/src/domain/hardening` |
| `support` | pro: support-archive parts of `src/fleet.js` and whichever screen offers them | `dash-next/src/domain/support` |

Cross-cutting decisions, including ownership of kinds this table does not
name, are in [decisions.md](decisions.md).

## Not specified

Presentation that the Quantic design system replaces is not behavior:
`oss:src/ui/controls.jsx`, `controls.css`, `theme.css`, `app.css`,
`icons*.js(x)`, fonts, logos, and `pro:src/ui/brand-logo.js`, `pro:src/icons.jsx`,
`pro:src/app.css`. A control's behavior is specified only where
the user depends on it and Quantic would not provide it by default.

`pro:mock/` is a development mock of the hub, a reference for the API
contract, not dashboard behavior.

## Per-domain files

- `<domain>/*.feature` — scenarios.
- `<domain>/VERIFY.md` — the latest `verify-spec` report.
- `<domain>/NOTES.md` — divergences with a recommended winner, and bugs or
  oddities noticed in the legacy code.
- `<domain>/trace.md` — scenario → implementing files, written by
  `implement-spec`.
