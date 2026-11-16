# terminal — verification report

**Summary:** 133 scenarios checked — VERIFIED 131, DRIFTED 1, MISSING 0, UNCITED 0, UNVERIFIABLE 1; UNCOVERED 5. **Not ready to implement** (DRIFTED and UNCOVERED outstanding, and one feature-level assumption is contradicted by the code's own comments).

Method: completeness pass first, from an independent reading, in full, of `src/screens/terminal.jsx` and `src/ui/exec-logs-drawer.jsx` in both roots, `oss:src/node-shell.js`, `oss:src/ui/split.js`, both `src/app.jsx`, plus the transport the sessions ride on (`pro:src/k8sproxy.js`, `pro:src/k8sclient.js`, both `src/cluster-context.js`) and the splitter component that consumes `split.js` (`oss:src/ui/controls.jsx:216-344`). Accuracy pass: every citation of every scenario was opened, and **all 133 scenarios were re-derived from code** (no sampling), including the 3 `# assumption:` items and all 18 `@diverged` pairs.

| Feature file | Scenarios | VERIFIED | DRIFTED | UNVERIFIABLE |
|---|---|---|---|---|
| node-shell.feature | 39 | 39 | 0 | 0 |
| pod-logs.feature | 31 | 31 | 0 | 0 |
| pod-exec.feature | 25 | 24 | 0 | 1 |
| drawer.feature | 21 | 20 | 1 | 0 |
| shell-dialog.feature | 10 | 10 | 0 | 0 |
| split-panes.feature | 7 | 7 | 0 | 0 |

## UNCOVERED behaviors

| # | Location | Behavior | Suggested scenario title |
|---|---|---|---|
| 1 | `oss:src/app.jsx:435`, `oss:src/screens/terminal.jsx:216-227`; `pro:src/app.jsx:425`, `pro:src/screens/terminal.jsx:226-237` | An `exec` action that names a pod without being a pod object (`{ns, pod, container}`) opens the dialog titled "Shell · <pod>" straight on a terminal in that container, with no picker | An exec action naming a pod opens the shell dialog directly on that pod |
| 2 | `oss:src/app.jsx:435`, `oss:src/screens/terminal.jsx:220-222`; `pro:src/app.jsx:425`, `pro:src/screens/terminal.jsx:230-232` | An `exec` action that carries a name scopes the picker to pods whose name starts with it, and falls back to every pod when none matches | An exec action naming a workload scopes the pod picker to its pods |
| 3 | `oss:src/ui/exec-logs-drawer.jsx:214-215` with `oss:src/app.jsx:270`; `pro:src/ui/exec-logs-drawer.jsx:135-136` with `pro:src/app.jsx:174` | Losing write permission only hides tabs; the app keeps them. When permission returns, a hidden host-shell tab reappears and starts a new helper, and a pod tab that was on exec returns to exec with a new session | Regaining write permission restores the hidden tabs with new sessions |
| 4 | `oss:src/app.jsx:183-188,209-210,222-228`; `pro:src/app.jsx:112-117,126-130,137-144` | Signing out, or being sent to sign-in after a 401, unmounts the shell: every tab and session is closed and the drawer is empty after signing in again | Signing out or losing the session closes the drawer |
| 5 | `oss:src/ui/exec-logs-drawer.jsx:94-95` | On a 401 the log viewer both routes to sign-in and appends "[error] HTTP 401" (NOTES.md mentions it; no scenario states it) | An unauthenticated log request also shows an error line |

Items 1 and 2: NOTES.md (b) records these as dead paths and deliberately leaves them unspecified. That holds for pro, where nothing raises `exec` with those shapes. It is not provable for oss: `oss:src/app.jsx:489-490` hands `onAction` to every consumer-registered screen (`src/registry.js`), so an external screen can raise both. Either specify them or record a `@drop` decision.

Source files under the domain's paths that no scenario cites: none. Files read that belong to another domain: `pro:src/k8sproxy.js`, `pro:src/k8sclient.js`, `src/cluster-context.js` (data / clusters), `src/app.jsx` (shell). `oss:src/ui/controls.jsx` is listed under "Not specified" in `spec/domains.md`, yet it holds the part of the splitter that `split-panes.feature` relies on (state kept across collapse, the drag handler); see the citation notes.

## Non-VERIFIED scenarios

| File | Scenario | Verdict | Reason | Corrected citation or wording |
|---|---|---|---|---|
| drawer.feature | The drawer can be resized by dragging its top edge | DRIFTED | Last step is wrong. The drawer component is always mounted by the shell (`oss:src/app.jsx:509-511`, `pro:src/app.jsx:532-534`) and its height state is declared before the early return that hides it (`oss:src/ui/exec-logs-drawer.jsx:204,216`, `pro:src/ui/exec-logs-drawer.jsx:125,137`), so the height survives closing and reopening the drawer, navigation and (pro) a cluster switch. It resets only when the page is reloaded or the shell is unmounted by sign-out | "And the height is kept when the drawer is closed and reopened, and returns to 300 pixels after a page reload or a new sign-in". Add `# source: oss:src/app.jsx:509-511` and `# source: pro:src/app.jsx:532-534`. The same sentence in NOTES.md (b), "not remembered", needs the same correction |
| pod-exec.feature | A terminal that cannot be created says so | UNVERIFIABLE | The branch exists (`oss:src/screens/terminal.jsx:86,147`, `pro:src/screens/terminal.jsx:65,112`) but tests a statically imported binding (`if (!Terminal)`); a failed import fails the whole module before this code runs, so the state cannot be reached in a built app. Confirming it would need a build in which the import resolves to an empty value. The `xterm-missing` row of "The session header shows the connection state and the target" has the same limitation, and names an internal state | Either drop the scenario and that example row, or restate the requirement as behavior the rewrite must have: "When the terminal cannot be started, the pane says so and no connection is attempted" |

## Assumptions

- node-shell.feature, "Closing or leaving the page leaves open helpers behind" (pro registers no page-unload handler) — **settled by code, holds**: a search of `pro:src` for `pagehide`, `beforeunload` and `unload` returns nothing.
- node-shell.feature, "Signing out does not delete open helpers first" (pro) — **order settled by code, refusal not**: `pro:src/app.jsx:126-130` awaits logout before the shell unmounts, and `pro:src/fleet.js:146-151` clears the CSRF token and identity in that call, so the cleanup DELETE is sent afterwards with no CSRF header. That the hub then refuses it is server behavior the dashboard code cannot show; the scenario's own steps do not depend on it. Add `# source: pro:src/fleet.js:146-151`.
- split-panes.feature, feature level ("these rules mirror what the Quantic splitter already does") — **not settled, and contradicted by a comment in the code**: `oss:src/ui/controls.jsx:217-221` says the design system "parsed it identically but skipped the recompute whenever the pane list changed in the same update — always true under React — so its panes always came out 50/50 whatever the markup asked for". If that is still true of the version pro uses, pro's Nodes screen does not render the 64/36 split it declares (`pro:src/screens/infra.jsx:337`), and the feature's preamble "the fleet dashboard gets the same behavior from the design system's splitter" is wrong. Only running pro, or reading the pinned Quantic splitter source, can settle it.

## Scenarios that describe implementation rather than behavior

- split-panes.feature, all 7 scenarios: they restate the arithmetic of a helper (`oss:src/ui/split.js`) behind a widget the design system replaces, which `spec/domains.md` places under "Not specified" unless Quantic would not provide it. Given the contradiction above, the two that matter as acceptance criteria are "A split declared in percentages gives each pane that share" and "A collapsed pane gives its space to the others and gets it back"; the rest read as unit tests of the helper. NOTES.md already suggests moving the file out of this domain.
- pod-exec.feature: the `xterm-missing` state name (see above). The frame, channel and subprotocol wording elsewhere is the wire contract with the cluster and is appropriate.

## Citation notes on VERIFIED scenarios (behavior correct, citation could be tighter)

- split-panes.feature, "A collapsed pane gives its space to the others and gets it back": "or to wherever the user last dragged it" rests on the size state in `oss:src/ui/controls.jsx:235-246,272-277`, not on `split.js`.
- split-panes.feature, "Dragging the divider respects both panes' limits": the drag handler is `oss:src/ui/controls.jsx:262-277,293-301`.
- node-shell.feature, "A host shell opens as a tab in the bottom drawer": the server icon is `oss:src/ui/exec-logs-drawer.jsx:238`, `pro:src/ui/exec-logs-drawer.jsx:159` (cited from the next line).
- node-shell.feature, pro "The helper has no cluster-side bound beyond its one-hour sleep": the absence of a deadline and of automatic removal is in the code; "remains in the cluster as a completed pod" is Kubernetes behavior for a `restartPolicy: Never` pod, not something the dashboard code shows.
- pod-exec.feature, pro "The socket address is relative to the proxy of the cluster the session was opened on": in standalone mode the prefix is the configured one, "/k8s" by default (`pro:src/cluster-context.js:79`).
- pod-logs.feature, pro "A 401 on a log request routes to sign-in only if the hub session really expired": the "[error]" line is shown in both outcomes (`pro:src/k8sproxy.js:97-98`), not only when the session is still valid.

## NOTES.md accuracy

Sections (a), (b) and (d) were checked against the code. They hold except for: "Drawer height is … not remembered" (see the DRIFTED row), and "Dead paths in the shell dialog (both)" (reachable in oss through registered screens, see UNCOVERED 1 and 2). The URL, query and subprotocol tables in (d) match the code line for line.
