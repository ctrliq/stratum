# AGENTS.md — Stratum dashboard (dash-next)

Architecture and style guidelines for `dash-next`, the unified Stratum dashboard: one app for
the single-cluster server and the fleet hub. The rules are Fuzzball's web UI rules, adapted.
Behavior is specified in `../spec/<domain>/*.feature`; `../spec/domains.md` maps domains to
code.

Stack: React 19, TypeScript, Vite, TanStack (Router, Query v5, Form, Table), Zod, Quantic design
system (`@ctrliq/quantic-react`, `@ctrliq/quantic-components`), SCSS modules, Vitest. No i18n
library: user-facing strings are plain English literals. Package manager is **pnpm** — never
`npm`/`npx`. Scripts: `pnpm lint`, `pnpm lint:css`, `pnpm typecheck`, `pnpm test`,
`pnpm generate`.

## Quantic playbooks

<!-- playbooks:start -->

`@ctrliq/quantic-playbooks` carries the procedures for building with Quantic: which
component to reach for, which token, how to wire a framework, how to style and override,
which icon, and how to report a defect.

See `node_modules/@ctrliq/quantic-playbooks/index.md` and read the one playbook whose
situation matches the task, and only that one. The index moves with the installed version,
so prefer it over guessing.

<!-- playbooks:end -->

## 1. Directory structure

```text
src/
  domain/<domain>/        # e.g. workloads, nodes, storage, clusters, auth
    pages/<DomainAction>/  # a folder, never a single file
      index.tsx            # the page component (same name as the folder)
      <DomainAction>.module.scss
      parts/               # sub-components scoped to this page (see below)
    forms/                 # form components (TanStack Form + Zod)
    modals/                # dialogs
    components/            # reused by 2+ things within this domain
    hooks/  schemas/  utils/  constants.ts  types/
  shared/                  # no domain knowledge: no domain types, no business rules
    components/  context/  hooks/  utils/  schemas/
    fetchers/              # transport for the generated clients (session cookie + CSRF)
    streaming/             # exec socket, log follow, watch: what OpenAPI cannot describe
  routes/                  # thin TanStack Router files, compose from domain/
  api/                     # generated OpenAPI clients: k8s/, dashboard/, fleet/
  styles/                  # global styles; breakpoint mixins come from Quantic (section 4)
```

- **Pages are always folders**, named `<Domain><Action>` in PascalCase (`PodsList`,
  `PodDetail`, `ClustersList`). `index.tsx` exports a component with the same name as the
  folder. The verb (`List`, `Detail`, `Editor`, `Login`, `Settings`) says what the page does; drop
  it only when a domain can have just one page (e.g. `Configuration`). Use a scope prefix to
  distinguish parallel pages: `OrgUsersList` vs `AccountUsersList`.
- **Page-scoped sub-components go in the page folder**, in `parts/` once there are more than a
  couple. `parts/` is flat until a cohesive sub-family of ≥3 components emerges that you can name
  in one word — then group them (`parts/nodes/`, `parts/panes/`). Page-scoped components never
  move to `domain/<x>/components/`; when a second page needs the same behavior, extract a fresh
  shared component into `components/` and drop the page-scoped original.
- **`domain/<x>/components/`** is for components reused by 2+ consumers within the domain. Starts
  flat; group into a sub-folder only when a nameable sub-family of ≥3 emerges. Keep the component
  filenames unchanged when grouping — the folder organizes, the filename is what readers see.
- **`shared/`** is domain-agnostic: no API calls, no domain types, no business rules. Anything
  reused across domains lives here.
- **Extract domain-agnostic logic to `shared/utils`; keep domain-specific logic in
  `domain/<x>/utils`.** Before writing a helper, check `shared/utils` — don't reimplement generic
  string/date/size/filter/`fp` helpers per domain. Move a domain helper to `shared/utils` the
  moment a second domain needs it.
- **Route files are thin** — they import from `domain/`, they contain no logic.

## 2. Imports

Enforced by `simple-import-sort` (fix with `pnpm lint --fix`). The `~/` alias maps to `src/`.

- **`~/` to cross a page-folder boundary; relative paths within one.** If the import would need
  `../..` or more to escape the current page folder, use `~/`. If it stays in the same page subtree
  (siblings, `./parts/x`, `../nodes/y`), use a relative path.
- **Group order**, blank line between groups: external packages → `~/…` (alphabetical) → relative
  (`./`, `../`) → stylesheet (`./Foo.module.scss`, always last).

## 3. Code style (TypeScript / React)

Functional and declarative. Pure functions, immutability, composition.

- **Small, single-purpose, pure functions.** One job per function; compose small functions rather
  than growing a branchy do-everything one.
- **`type`, not `interface`** — the codebase is ~30:1 in favor of `type` aliases.
- **Single return per function** — no early returns / guard clauses. Use ternaries or if/else-if
  chains, and prefer ternaries and function composition over `if`/`else` where it stays readable.
- **`map`/`filter`/`reduce`/`Object.fromEntries`, not `forEach`** — no mutation; spread and
  immutable methods only.
- **Ternaries, not `&&`, for conditional JSX** — write `condition ? <X /> : null`.
- **No kind/type discriminator parameters.** Don't pass a `kind`/`type`/`variant` string to branch
  inside a function — write separate named functions (`renameJob`, `renameVolume`, not
  `renameEntity(kind, …)`).
- **Full, descriptive names** — `event` not `e`, `error` not `err`, `fieldProps` not `fp`. No
  single-letter or abbreviated identifiers.
- **Comments explain _why_, never _what_.** No banner/section-divider comments; if a comment is
  needed to explain what code does, make the code clearer instead.
- **Forms use TanStack Form + Zod** — don't hand-roll `useState` form state or manual validation.
- **Function names are relational or predicates, never imperative, never noun phrases.** Name a
  function for the conversion or relation it expresses — `toPodRow`, `fromQuantity`,
  `withNamespace`, `ownerOf`, `statusFor`, `inNamespace` — or as a predicate — `isPodReady`,
  `hasControlPlaneRole`. Not `resolveFilters`, `buildOptions`, `computeSizes`, `getStatus`
  (imperative); not `savedPreferences`, `podStatus` (noun phrase). React components, hooks
  (`useX`) and handler props (`onX`) follow React's own naming.
- **Less code.** Reuse before writing: a `shared/utils` helper, a Quantic component, a generated
  hook. Derive types from the generated API types (`Pick`, `keyof`, `Record`, indexed access)
  instead of redeclaring them. No `any`; no `as` to silence the compiler. Never hand-roll what
  TanStack Query already does (caching, de-duplication, polling, cancellation, stale guards).
- **No narration.** `quantic/no-narration` is an error: no comment that says what the code does,
  no train of thought, no banners. A comment that explains a tangle means the tangle should be a
  named, tested function.

## 4. Styles (SCSS modules)

- **No inline styles.** The only allowed `style` use is feeding a CSS custom property the
  stylesheet consumes: `style={{ '--progress': value } as CSSProperties}`. The styling decision
  stays in the `.module.scss`.
- **One stylesheet per `.tsx`, paired by basename** — `Foo.tsx` ↔ `Foo.module.scss`. A page's
  stylesheet is named after its folder, never `index.module.scss`
  (`WorkflowsList/index.tsx` ↔ `WorkflowsList/WorkflowsList.module.scss`).
- **A `.tsx` imports only its own paired stylesheet** — no cross-file `.module.scss` imports, even
  between siblings. Share styles by extracting a component (its own `.tsx` + `.module.scss`), not
  an orphan stylesheet.
- **Semantic class names — name by what it _is_, not how it looks.** Good: `.primaryAction`,
  `.navigation`, `.errorMessage`, `.toolbar`, `.detailPanel`. Bad: `.bigRedButton`, `.leftColumn`,
  `.container`, `.wrapper`, `.flexRow`. camelCase so JSX reads `styles.primaryAction`.
- **Boolean flags stay `isSomething`; the class carries the role.** Variable `isPrimary`, class
  `variantPrimary` / `stateLoading`. Compose with `clsx` object form, not `&&`:
  `clsx(styles.button, { [styles.variantPrimary]: isPrimary })`.
- **No styling via `data-*` attributes** — compose class names with `clsx`. `data-*` is the DOM
  contract for tests/automation; keep it free of styling.
- **Structural markup, not presentational.** Use the semantic element that matches the content's
  role (`button`, `nav`, `ul`/`li`, `section`, `header`, `table`, `dl`) — reach for `div`/`span`
  only when no semantic element fits. Markup expresses structure and meaning; presentation lives
  entirely in CSS.
- **Minimal markup** — no wrapper divs that exist only to style; delegate layout to CSS.
- **Mobile-first** — base styles for mobile, then Quantic's breakpoint mixins:
  `@use '@ctrliq/quantic-css/mixins/breakpoints' as breakpoints;` then
  `@include breakpoints.above-tablet-landscape { … }` (`above-mobile`, `above-tablet-portrait`,
  `above-tablet-landscape`, `above-desktop`, `above-large-desktop`; the widths are Quantic's
  breakpoint tokens). There are no app-defined breakpoints.

## 5. Prefer the design system over custom UI

Reach for `@ctrliq/quantic-react` / `@ctrliq/quantic-components` and Quantic design tokens before
building a component or writing custom CSS. Don't hardcode hex colors, spacing, or font sizes —
use tokens. Only add custom CSS for genuinely app-specific layout.

## 6. Testing

- **Unit-test pure functions** (Vitest) — utilities and business logic. Every pure util should have
  tests.
- **Don't test side effects, impure functions, or chase 100% coverage.** No jsdom, no component
  rendering tests: extract the decision into a pure function and test that.
- **No dedicated tests for Zod schemas** — validating a schema library against itself adds no value.

## 7. Utility modules

One file per _category_ of pure function, named the same in every domain, each with a
`.test.ts` beside it. Never a `helpers.ts`, never one file per function.

| File            | Holds                                | Examples                            |
| --------------- | ------------------------------------ | ----------------------------------- |
| `predicates.ts` | boolean questions                    | `isPodReady`, `isNodeCordoned`      |
| `selectors.ts`  | pick data out of a structure         | `containerStatusesOf`, `ownerOf`    |
| `transforms.ts` | reshape API objects into view models | `toPodRow`, `withNamespace`         |
| `filters.ts`    | narrow lists                         | `inNamespace`, `withLabel`          |
| `formatters.ts` | value → display string               | `toAge`, `toMebibytes`              |
| `parsers.ts`    | string → value                       | `fromQuantity`, `fromLabelSelector` |
| `reducers.ts`   | state transitions                    | log buffer, exec session            |

Domain-agnostic versions live once in `shared/utils`; a domain's `utils/` holds only what is
specific to it. Check `shared/utils` before writing a helper.

## 8. API layer

- Three generated clients, regenerated with `pnpm generate`, never hand-edited:
  `src/api/k8s` (from `openapi/k8s/k8s.subset.json`, a subset of the upstream Kubernetes spec
  plus Stratum's CRDs), `src/api/dashboard` and `src/api/fleet` (hand-written specs in
  `openapi/`). `*Fetcher.ts` and `*Context.ts` in each are written once and owned by us.
- **No hand-written request helpers.** Every request is a generated hook. If a kind or endpoint
  is missing, add it to `openapi/k8s/allowlist.ts` (then `pnpm generate:k8s-spec`) or to the
  hand-written spec, and regenerate.
- Three sanctioned exceptions, each one module under `shared/`: arbitrary CRD instances
  (unknown at build time), streaming (exec, log follow, watch), and patch content types.
- The backend is one of two servers, told apart by unauthenticated `GET /api/v1/config`
  (see `openapi/NOTES.md`): the single-cluster dashboard server (`auth.providers` is `["token"]`,
  Kubernetes at `/k8s`, CSRF header `X-Stratum-CSRF`) or the fleet hub (`local` / `oidc`,
  Kubernetes at `/api/v1/clusters/{id}/k8s` in fleet mode, CSRF header `X-Fleet-CSRF`). Both use
  an HttpOnly session cookie; the browser never holds a token. Page code never builds a URL,
  names a CSRF header or branches on which server it is talking to beyond the config's
  `mode` and `features`.

## 9. Runtime architecture

- **Transport** is `src/shared/fetchers`: `sameOriginFetch` (session cookie, CSRF header on
  writes, merge-patch content type on `PATCH` unless a header overrides it, redirects refused on
  writes, 15 s deadline, `keepalive` passthrough, `ApiError = { status, payload, reason?,
headers? }` on failure — `reason` is `timeout`, `aborted`, `redirect` or `network`, tested with
  `hasReason(error, 'timeout')`; `headers` are the lower-cased response headers of an answered
  error; a binary download with a file name resolves to a `File`), `backend.ts` (the one piece
  of module state: CSRF header and token, Kubernetes base, the unauthorized handler — set at
  boot and at sign-in with `withBackend`), `requests.ts` (pure helpers, `hasStatus(error, 403)`),
  `queryKeys.ts`. The three `*Fetcher.ts` / `*Context.ts` files under `src/api` are thin
  adapters over it. Nothing else calls `fetch`.
- **The selected cluster** comes from `ClusterContext` (`src/shared/context`), provided by the
  `/c/$cluster` layout route. Every generated Kubernetes hook reads it, puts it in the query key
  (`['k8s', clusterId, …]`) and in the URL. To address another cluster, pass `clusterId` in the
  hook's variables.
- **Server state lives in TanStack Query only.** No global data singleton, no hand-rolled
  loader. Lists poll with `refetchInterval`; each kind is its own query, so one kind failing or
  forbidden never affects another. Derive view models from query data with pure functions.
- **Routes**: every cluster screen is `/c/$cluster/<screen>`; a single-cluster backend uses the
  cluster id `local`. Cross-cutting product decisions are in `../spec/decisions.md` and override
  any scenario they contradict.

## 10. Adding a screen

- **A list of a Kubernetes kind**: `domain/<d>/constants.ts` (a `ResourceKind` and the kind's own
  columns; Name, Namespace and Age are added for you), `domain/<d>/utils/transforms.ts`
  (`toXRow(object)`), `domain/<d>/pages/XList/index.tsx` (call the generated list hook with
  `resourceListQuery`, render `<ResourceBrowser kind list columns toRow writes linkedName />`,
  with optional `actions`, `isReadOnly`, `writes.scale`, `writes.onRestart`, `tags` (under the
  title), `hasIdentityColumns={false}` (Name only) and `openedQuery` (a generated `…Query`
  factory that loads the full object when a row is opened, for lists read without their
  payload — see `SecretsList`)), and a route file. Manifests reach `writes` typed as the list's
  object; the cluster, not the browser, validates them.
  The model is `src/domain/config/pages/ConfigMapsList/`.
- **Cluster screen route**: `src/routes/c.$cluster.<segment>.tsx`, rendering the page and
  validating its search parameters. Segments, labels, icons and the mode or feature each screen
  needs are data in `src/domain/shell/constants.ts` (`screens`); the shell supplies the frame, so
  a page renders content only.
- **Fleet page route**: `src/routes/<name>.tsx` (top level, not under `/c/$cluster`), wrapping
  the page in `FleetShell` from `~/domain/shell/components/ClusterShell/ClusterShell`.
- **What the shell provides**: `ClusterContext`, `NamespaceContext`, `WriteAccessContext` (or
  `useCanWrite()`), `LiveRefreshContext` with `polledWhen(...)` for polling, `useServerConfig()`
  for mode and features (`hasFeature` in `shared/utils/predicates`), `useScreenNavigation()` for
  in-app links, `useManualRefresh()` for a page's own Refresh button, `usePaletteAction()` to
  offer a command-palette action, `toast.*` for notices. A fleet page has no selected cluster.
- **Shared building blocks**: `ToneBadge` and `StatusCell` (the one status badge; tones are
  `shared/types/tone`, `toStatusTone` / `toStatusLabel` in `shared/utils/formatters`),
  `TabbedLists` (several lists under one heading; nested titles become `h2`), `Hinted`,
  `PageTitle`, `ResourceList`, `DataTable`, `EmptyState`.
- **Kinds unknown at build time**: `shared/fetchers/arbitraryKinds.ts` through
  `shared/hooks/useArbitraryKind.ts`.

## Review checklist

- File placement: pages are folders named `<Domain><Action>`; page-scoped code in `parts/`, not in
  `domain/components/`; `shared/` has no domain knowledge; generic helpers in `shared/utils` (not
  reimplemented per domain); routes are thin.
- Imports: `~/` across page boundaries, relative within; groups ordered and separated.
- Code: small single-purpose pure functions; `type` over `interface`; single return (no early
  returns); no `forEach`; ternaries over
  `&&`; no kind/variant branch params; full names; relational or predicate function names (no
  imperative, no noun-phrase); no narration; types derived from generated API types; TanStack
  Form (no manual form state).
- Styles: no inline styles; one paired stylesheet per `.tsx`; no cross-file scss imports; semantic
  camelCase class names; `clsx` object form (no `data-*` styling); structural/semantic markup (not
  div soup); minimal markup; mobile-first.
- Design system: Quantic components + tokens over custom UI; no hardcoded hex/spacing.
- Tests: pure functions covered; no schema tests; no coverage-chasing.
- Utils: one file per category, same names in every domain, tests beside them; generic helpers
  in `shared/utils`.
- API: generated clients not hand-edited; no hand-written request helpers; page code builds no
  URLs and reads no tokens.
