# auth: verification report

**Summary:** 114 scenarios checked (9 feature files) — VERIFIED 112, DRIFTED 1,
UNCITED 1 (one step of one scenario), MISSING 0, UNVERIFIABLE 0; UNCOVERED 8.
**Not ready to implement** by the skill's rule (DRIFTED, UNCITED and UNCOVERED
outstanding). None of the findings is severe: both non-VERIFIED rows are
wording or citation repairs, and six of the eight gaps are edge cases.

## How this was checked

- Completeness pass first: every listed source was read in full in both roots
  before any `.feature` file was opened (`oss:src/session.js`,
  `oss:src/screens/login.jsx`, `oss:src/app.jsx`; `pro:src/fleet.js`,
  `pro:src/auth-redirect.js`, `pro:src/screens/login.jsx`,
  `pro:src/ui/login-network.jsx`, `pro:src/app.jsx`), and an inventory was
  built from them.
- Citations: all 183 `# source:` lines resolve to an existing file and an
  in-range line span (checked mechanically), every scenario has at least one,
  and every `@oss` / `@pro` tag has a citation in that root.
- Accuracy sampling: no sampling. All 114 scenarios were re-derived from the
  code, including the 9 `@diverged` pairs. There is no `# assumption:` line in
  this domain. Citations outside the listed sources were opened too
  (`oss:src/k8sutil.js:75-76,84`, `pro:src/k8sproxy.js:69,97,114-115,129`).
- Reading only. Nothing was run, so timing behavior (15 s bounds, cross-tab
  locks, storage events) is confirmed from the code, not exercised.

## UNCOVERED behaviors

| # | Location | Behavior the code has | Suggested scenario title | Weight |
|---|---|---|---|---|
| 1 | `pro:src/screens/login.jsx:28,36`, `pro:src/auth-redirect.js:34-36` | Starting SSO is a full page navigation that carries only the `return` name. The in-app destination remembered for after sign-in is router state and is not sent, so it does not survive the SSO round trip. | SSO sign-in does not carry the remembered in-app destination | medium |
| 2 | `pro:src/app.jsx:112-117`, `pro:src/fleet.js:20-22,30-37` | On a 401 the fleet dashboard clears only the shell's copy of the user. The request layer keeps the identity and the anti-forgery token until the next identity check or sign-in. The matching oss scenario ("Expiry discards everything held for the ended session") is `@oss` only; `NOTES.md` bug 6 records this but no scenario does. | Session expiry in the fleet dashboard keeps the held identity and token | medium |
| 3 | `oss:src/session.js:269-273,338-341`, `oss:src/app.jsx:207-210` | With an unreadable session marker, sign-out sends neither the identity read nor the POST, but the local session is still discarded and the sign-in screen is shown. The scenario "An unreadable session marker blocks sign-in and sign-out" names sign-out in its title and only has sign-in steps. | Sign-out with an unreadable session marker sends nothing and still leaves the dashboard | medium |
| 4 | `pro:src/screens/login.jsx:40,42` | A username of only whitespace passes the empty check (it is not trimmed there), enables the button, and is posted as an empty string. `NOTES.md` bug 5 records it; no scenario does. | A whitespace-only username is submitted as an empty username | low |
| 5 | `oss:src/session.js:262-265,83-92` | If the marker cannot be written when a sign-in or sign-out ends, the in-progress flag stays in storage and every tab stays fenced (no anti-forgery token, no write controls, answers superseded) until a later change publishes a new marker. | A session change whose completion cannot be published leaves every tab fenced | low |
| 6 | `oss:src/session.js:346-349,67-82` | A storage event with no key (site storage cleared) or a removed marker is treated as a session change: identity, token and cached data are discarded and, where sign-in is required, the sign-in screen is shown. | Clearing browser storage in another tab ends this tab's session view | low |
| 7 | `oss:src/session.js:67-82`, `oss:src/app.jsx:191` | When sign-in is not required and the marker changes, the tab stays on its page (covered) but its cached cluster data is still emptied and in-flight answers are superseded. | A marker change without required sign-in still empties cached cluster data | low |
| 8 | `oss:src/session.js:163-165,173`, `pro:src/fleet.js:61-63,89` | Before the app registers its redirect (and after it unmounts) a 401 is only returned to its caller; nothing navigates. | A 401 before the shell is listening is returned to the caller only | low |

Source files under the domain's paths that no scenario cites: none.

Source files that belong to no domain in `spec/domains.md`:
`pro:src/ui/brand-logo.js`, `pro:src/icons.jsx`, `pro:src/app.css`. They look
like presentation, but the "Not specified" section names only the oss
equivalents, so the fleet ones are not formally placed.

## Non-VERIFIED scenarios

| File | Scenario | Verdict | Reason | Corrected citation or wording |
|---|---|---|---|---|
| `session-expiry.feature` | A member cluster's own 401 does not sign the user out | DRIFTED (low) | The first Then holds. The second ("the 401 is shown as that call's own error") is not in the cited range and is only true for writes and streams, which return the 401 as their failure. A read made through the defaulting getter swallows it: a console warning and the default value, nothing shown. | Citation: add `pro:src/k8sproxy.js:68-72,82-86,97-98,127-130`. Wording: "And the 401 is returned to that call as its own failure; a read that has a default falls back to it with a console warning". |
| `write-gating.feature` | Any signed-in identity is offered write controls | UNCITED (one step) | "a write the cluster forbids is reported from its 403 answer" has no source. The cited range `oss:src/session.js:144-157` decides only whether controls are offered; the 403 is mentioned in a comment at `oss:src/session.js:142-143`. The reporting itself is each write path's own toast, outside this domain. | Either drop the step here and leave it to the `resources` domain, or cite the write path that toasts the error (for the shell's own write: `oss:src/app.jsx:445-446`, `Error: <message>`). |

Verified with a remark (no action needed unless the wording is tightened):

- `session-expiry.feature` — "The identity check, sign-in and sign-out never
  trigger the expiry redirect": true for the start-up identity check. In the
  fleet dashboard the *confirmation* identity request after a proxied 401
  (`pro:src/fleet.js:131`) deliberately does trigger it, which the scenario
  "A 401 on a proxied Kubernetes call is confirmed with the hub first" covers.
  "The start-up identity check" would remove the ambiguity.
- `session-expiry.feature` — "Later 401s from requests already in flight keep
  the remembered destination" is tagged `@oss @pro` and holds in both, by
  different means: the fleet dashboard ignores a 401 while on `/login`
  (`pro:src/auth-redirect.js:14`); the open dashboard has no such check and
  relies on the session fence discarding late answers
  (`oss:src/session.js:196`, `oss:src/k8sutil.js:84,92`). A rewrite that keeps
  only one of the two mechanisms must still pass it.
- `sign-in.feature` — "A sign-in that yields no identity stays on the sign-in
  screen" (oss): verified, with a visible detour the scenario does not
  mention: the form navigates to the remembered destination and is redirected
  straight back to `/login` (`oss:src/screens/login.jsx:46-47`,
  `oss:src/app.jsx:218-220`).
- `sign-in-backdrop.feature` — the "when that preference changes" and "when
  the viewport becomes wider" steps rest on the listeners at
  `pro:src/ui/login-network.jsx:92-93`, which the citations leave out.

## Implementation-flavoured scenarios

- `session-coordination.feature` — "Only a non-secret marker is shared between
  tabs" names the storage key and the marker's two fields. Acceptable only if
  the rewrite must interoperate with a legacy tab during rollout; otherwise
  the behavior worth keeping is the second Then (no token, cookie or identity
  in browser storage).
- Nothing else in this domain names functions, components or state shapes.
