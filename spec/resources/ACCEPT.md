# resources — acceptance report

Checked 2026-11-13 against `dash-next/src/domain/resources`,
`dash-next/src/shared/components/ResourceList` and the code they depend on (shell, auth, shared
utils, the kind pages that mount the browser), by reading the code and running the unit tests of
the deciding pure functions (all passing). Nothing was started in a browser. Paths are relative to
`dash-next/`.

**151 scenarios checked: 136 SATISFIED, 3 PARTIAL, 0 VIOLATED, 1 NOT_IMPLEMENTED, 11 DROPPED,
0 BLOCKED, 0 UNVERIFIABLE — not accepted.**

| Feature | Kept | SATISFIED | PARTIAL | NOT_IMPLEMENTED | DROPPED |
|---|---|---|---|---|---|
| list | 36 | 36 | 0 | 0 | 5 |
| filtering | 12 | 12 | 0 | 0 | 0 |
| detail | 28 | 26 | 2 | 0 | 4 |
| edit | 14 | 14 | 0 | 0 | 0 |
| create | 13 | 13 | 0 | 0 | 1 |
| delete | 9 | 9 | 0 | 0 | 0 |
| write-gating | 12 | 11 | 0 | 1 | 0 |
| deep-links | 16 | 15 | 1 | 0 | 1 |

The ten rows the trace marks `blocked` (status badge, write gating, `?ns=`, navigation) were
traced into `shared`, `auth` and `shell`; nine are SATISFIED, one is the NOT_IMPLEMENTED below.

## Scenarios that are not SATISFIED

| Feature | Scenario | Verdict | Expected | Actual | Where |
|---|---|---|---|---|---|
| write-gating | Write controls are withdrawn while the session is being replaced (`@oss`) | NOT_IMPLEMENTED | While the session is changing or was taken over in another tab, no write control is offered until it settles | There is no "changing" state. `useCanWrite` is `isWriteOffered(config, identity)`; on another tab's change the provider re-reads `/api/v1/me` and keeps the previous identity (and the write controls) until the answer arrives. The marker written to local storage always says `inProgress: false` | `src/domain/auth/hooks/useCanWrite.ts:7-8`; `src/domain/auth/utils/predicates.ts:25-28`; `src/domain/auth/components/SessionProvider/SessionProvider.tsx:125-137`; `src/domain/auth/utils/transforms.ts:96-97` |
| detail | How YAML values are written | PARTIAL | An empty text is written `''` | It is written `""`. Every other row of the outline holds (checked by running `toYaml`). The unit test asserts the `""` form, so it passes for the divergent output | `src/shared/utils/formatters.ts:78-79`; `src/shared/utils/formatters.test.ts:107-109` |
| detail | Escape closes the panel | PARTIAL | With a panel open and focus not in one of its fields, Escape closes it | Escape is handled by `onKeyDown` on the list's `<section>`, so it is heard only while focus is somewhere inside that section (a row button, the search box, a panel button). With focus on the page body — a panel opened by a `?name=` link before anything was clicked, or after clicking a non-interactive area — the key never reaches the handler and nothing closes. The two "while typing" scenarios and the search-box scenario hold | `src/shared/components/ResourceList/ResourceList.tsx:122-129` |
| deep-links | A cluster-scoped object is named without a namespace | PARTIAL | `?name=<name>` opens the object with that name | Holds unless the bare name parses as a number or as `true` / `false`: the router decodes such a value to a non-string before validation, each route validates `name` with `z.string()`, validation fails and the route renders its error state instead of the list. Kubernetes allows such names (a ConfigMap `2024`, a PriorityClass `1000`). The same applies to a namespaced object linked without its namespace. `ns/123` is unaffected. The shell's own `ns` parser already accepts numbers | `src/routes/c.$cluster.configmaps.tsx:6` and the same line in every `src/routes/c.$cluster.<kind>.tsx`; compare `src/domain/shell/utils/parsers.ts:42-47` |

## Notes on SATISFIED scenarios worth a reviewer's eye

- list "The list says it is loading until the first answer arrives": the body shows skeleton
  placeholder rows (`DataTable` → `SkeletonRows`), no data rows and no empty message.
- list "A forbidden read is shown as its own state": the message is "You are not permitted to
  list <plural>."; the list keeps re-reading, but on the failure backoff (15 s → 120 s), so rows
  appear up to two minutes after access is granted rather than "on its usual schedule"
  (`src/shared/query/polling.ts:33-37`).
- list "Switching kind starts from a clean list": kinds the sidebar also counts (pods,
  deployments, statefulsets, daemonsets, jobs, cronjobs) share the sidebar's cached read, so their
  rows appear at once instead of "Loading…".
- list "Pausing auto-refresh in the shell does not pause an open list": holds
  (`resourceListQuery` is always live), which deliberately differs from every other polled read.
- detail "A row can be opened from the keyboard" and edit "The editor … does not wrap or
  autocorrect": rest on the Quantic `Button` (click from Enter/Space bubbles to the row) and
  `CodeInput` (its bundle sets `spellcheck="false" autocomplete="off" autocorrect="off"
  autocapitalize="off"`); not exercised in a browser.
- detail "Map keys are written unquoted whatever characters they contain": holds for a colon, a
  space and a slash. A key containing colon-space (`a: b`) is quoted by the YAML writer, which is
  required for the output to be valid YAML.
- write-gating "Write controls stay hidden while the configuration could not be read": D6 — an
  error screen in production. Under `vite dev` the login-free fallback offers write controls.
- delete "Confirming deletes the object": the dialog closes as soon as Delete is pressed, before
  the cluster answers.

## DROPPED (11)

Checked that nothing reproduces them: the old `/pods` address (redirect is the shell's), the
merged forbidden/failed state, silent staleness (both), the fixed 15 s clock, the pro "keeps
pending edit / replica prefill" variants (the panel is keyed by uid/namespace/name and remounts),
the `''` / `[object Object]` YAML defect, the all-rows-highlighted defect, the `null` manifest
posted empty.

## EXTRA — behavior in the code that no resources scenario covers

| Behavior | Where | Looks like |
|---|---|---|
| Apply in the editor refuses valid JSON that is not an object with "The manifest must be a JSON object." (the spec states this for Create only) | `src/domain/resources/hooks/useResourceBrowser.ts:104-115`; `src/domain/resources/utils/parsers.ts:12-21` | spec gap — add the `edit` sibling |
| `openedQuery`: the detail panel can read the full object when a row is opened, showing "Loading…" or "Error: …" in the panel until it answers (used by Secrets, whose list carries no payload) | `src/domain/resources/components/LoadedDetail.tsx:19-42`; `src/domain/resources/components/ResourceBrowser.tsx:121-133` | spec gap here (the detail scenarios assume the listed object); check `config` covers it |
| `hasIdentityColumns={false}` removes the Namespace and Age columns; used for a CRD instance list whose definition is unknown, which contradicts "Every kind has a Name column and an Age column" for that case | `src/domain/resources/components/ResourceBrowser.tsx:61,98`; `src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx:57` | decide in `crds` or here |
| `tags` rendered between the title and the count line | `src/shared/components/ResourceList/ResourceList.tsx:132` | used by `crds` — fine |
| Scale mode, the Restart confirmation dialog and their notices live in the generic browser; a scale that the cluster refuses still closes the panel (`onSettled`) | `src/domain/resources/components/ResourceDetail.tsx:102-106,124-135,185-203`; `src/domain/resources/components/ResourceBrowser.tsx:154-162`; `src/domain/resources/hooks/useResourceBrowser.ts:134-144` | specified in `workloads`, not here — confirm there |
| The stale notice "The latest refresh failed. Showing the last data that loaded." is also shown when a list that had rows becomes forbidden | `src/domain/resources/utils/formatters.ts:52-53` | spec gap (minor) |
| The list/panel divider is user-resizable | `src/shared/components/ResourceList/ResourceList.tsx:136` | presentation |

## Trace inconsistencies (`spec/resources/trace.md`)

- "A status value is shown as a badge with a fixed wording" and "A status the dashboard does not
  know is shown verbatim" are `blocked`; they are implemented in
  `src/shared/components/ToneBadge/StatusCell.tsx` with `toStatusLabel` / `toStatusTone` in
  `src/shared/utils/formatters.ts:19-47,131-142`.
- The six write-gating rows `blocked` on "the shell through WriteAccessContext" are implemented:
  `src/domain/auth/utils/predicates.ts:25-28` (`isWriteOffered`), provided in
  `src/domain/shell/components/ShellFrame/ShellFrame.tsx:109-115`. Five are SATISFIED; "withdrawn
  while the session is being replaced" is NOT_IMPLEMENTED.
- "Navigating elsewhere keeps the namespace and drops the name" (`blocked`) is implemented in
  `src/domain/shell/utils/transforms.ts:87-95,176-186`; "A link carries the namespace to filter
  by" (`blocked`) in `src/domain/shell/hooks/useScreenPlace.ts` and
  `src/domain/shell/components/ShellFrame/ShellFrame.tsx:110-114`.
- No row marked implemented was found unimplemented, and no row points at a file that does not
  exist.
- Not mentioned by the trace: `src/domain/resources/components/LoadedDetail.tsx` and the
  per-kind route files `src/routes/c.$cluster.<kind>.tsx`, which own the `?name=` validation.
