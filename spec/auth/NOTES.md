# auth: notes

Extracted from `oss:src/session.js`, `oss:src/screens/login.jsx`, the auth
parts of `oss:src/app.jsx`, and `pro:src/fleet.js` (login, logout, me, CSRF),
`pro:src/auth-redirect.js`, `pro:src/screens/login.jsx`,
`pro:src/ui/login-network.jsx`, the auth parts of `pro:src/app.jsx`. Nobody
was available to answer questions; every judgement call is recorded here.

9 feature files, 114 scenarios, 9 `@diverged` pairs.

## Divergences and recommended variant

| # | Pair (file) | oss | pro | Keep | Reason |
|---|---|---|---|---|---|
| 1 | Sign-in form (`sign-in.feature`) | one service account token field | username + password | both, chosen by backend | They are two authentication models, not two takes on one: token passthrough for a single cluster, hub accounts for a fleet. Drive the form from what the server's configuration advertises. |
| 2 | Rejected credential message (`sign-in.feature`) | `invalid_token` → "The cluster did not accept that token." | `invalid_credentials` → "Invalid username or password." | both | Follows from 1; each message belongs to its form. |
| 3 | Unreachable server message (`sign-in.feature`) | "Cannot reach the dashboard server." | "Cannot reach the fleet manager." | both, by mode | Same branch, wording names the backend the user is talking to. |
| 4 | Sign-in that yields no identity (`sign-in.feature`) | stays on sign-in | opens the shell with "—" as the user | oss | A shell with no identity where sign-in is required cannot write and misleads; pro's behavior looks accidental. Neither variant tells the user why, which the rewrite should fix. |
| 5 | Identity fields kept (`session-bootstrap.feature`) | username, provider, groups | username, provider, roles | both fields | The two servers return different payloads; keep whichever the server sends. |
| 6 | Sign-out request (`sign-out.feature`) | re-reads `/api/v1/me` for the current anti-forgery token under the session lock, then posts | posts with the token it holds | oss | The held token can be stale or absent after expiry or a change in another tab; re-reading makes explicit sign-out work in those cases. |
| 7 | 401 from the Kubernetes proxy (`session-expiry.feature`) | means the session ended, go to sign-in | ask the hub's `/api/v1/me` first; only a 401 there signs out | pro | A hub passes member responses through, so a member's 401 must not sign the user out. The confirmation is harmless for a single cluster and required for a fleet. |
| 8 | Anti-forgery header name (`api-requests-and-csrf.feature`) | `X-Stratum-CSRF` | `X-Fleet-CSRF` | decided by the server contract | The client must send what each backend checks. If one frontend serves both backends the name has to come from configuration or the backends have to agree; this is a server decision, not a UI one. |
| 9 | Who is offered write controls (`write-gating.feature`) | any signed-in identity (the cluster's RBAC decides) | only the `admin` role | both, by mode | oss has no roles by design; the hub does and refuses non-admin writes. One shared check with a per-mode rule. |

## Bugs and oddities in the legacy code

Recorded as findings; the feature files describe what the code does.

1. **A sign-out the server refuses still looks successful** (both).
   `oss:src/session.js:339-341` and `pro:src/fleet.js:146-151` discard the
   local session whatever the POST answered, and the callers
   (`oss:src/app.jsx:207-210`, `pro:src/app.jsx:126-130`) go to sign-in
   without reading the result. If the cookie was not cleared, a reload signs
   the user straight back in. No message is shown.
2. **A corrupted cross-tab marker blocks sign-in with a misleading message**
   (oss). When the `stratum-dashboard-session-generation` value in browser
   storage is not valid JSON of the expected shape, every sign-in and sign-out
   attempt fails before sending anything (`oss:src/session.js:58-66,87-88,
   269-273`) and the form shows "Cannot reach the dashboard server."
   (`oss:src/screens/login.jsx:38`). Nothing repairs the value; the user has
   to clear site data.
3. **A sign-in in another tab leaves this tab on the sign-in form** (oss).
   The other tab's change retires this tab's session and shows sign-in
   (`oss:src/app.jsx:190-194`), but nothing re-checks `/api/v1/me`, so the tab
   shows the form although a valid cookie now exists. A reload fixes it.
4. **The "Sign-in failed (HTTP n)" fallback is unreachable** (both).
   `oss:src/screens/login.jsx:38` and `pro:src/screens/login.jsx:46` fall back
   to it only when the failure carries no message, but the request layer
   always supplies one (`oss:src/session.js:203`, `pro:src/fleet.js:94`).
5. **A whitespace-only username is submitted as empty** (pro). The guard
   checks the untrimmed value, the request sends the trimmed one
   (`pro:src/screens/login.jsx:40,42`).
6. **The fleet dashboard keeps the old identity and token after a 401**
   (pro). The 401 handler clears only the shell's own copy of the user
   (`pro:src/app.jsx:112-117`); the identity and anti-forgery token held by
   the request layer stay until the next sign-in or identity check. No screen
   reads them while the sign-in form is up, so nothing shows, but a rewrite
   should clear them the way oss does (`oss:src/app.jsx:185`).
7. **A sign-in with no identity fails silently** (oss). See divergence 4: the
   form empties and stays, with no error (`oss:src/app.jsx:198-203`).
8. **Fleet mode with sign-in disabled is silently read-only** (pro).
   `pro:src/fleet.js:53-54` offers writes without an identity only in
   standalone mode, while the user menu in that state shows "cluster-admin"
   and no read-only marker (`pro:src/ui/shell.jsx:373-383`). Whether a fleet
   hub can run with sign-in disabled at all is a server question.
9. **An unreachable configuration grants the fleet UI its login-free mode**
   (pro). Listed under the shell domain's notes (configuration loading); the
   auth consequence is that write controls are offered and no sign-in is
   asked for (`pro:src/cluster-context.js:50-53`, `pro:src/fleet.js:50-54`).
   The server still refuses the requests.
10. **Failures of the start-up identity check are indistinguishable from "not
    signed in"** (both). A 500 or a network error on `/api/v1/me` shows the
    sign-in form (`oss:src/session.js:288`, `pro:src/fleet.js:110`), and the
    sign-in then fails with its own message.

## Content in `pro` that may not be meant for publication

No secrets, credentials, internal hostnames or customer names found in the
`pro` files of this domain. Lower-grade items a human should glance at before
any of this code or its comments become public:

- Comments name private server packages and files of the fleet manager
  (`internal/api`, `internal/contract/contract.go`, `internal/contract/
  archive.go`) and describe hub behavior in detail (`pro:src/fleet.js:11,
  155-171, 194-236, 240-275`).
- The hub route `/cli/authorize` and the OIDC start route
  `/api/v1/auth/oidc/start` with its `return` parameter
  (`pro:src/auth-redirect.js:18-36`) describe a sign-in flow the open
  dashboard does not have.
- The username field's placeholder is `admin` (`pro:src/screens/login.jsx:72`)
  and the README documents the development mock's `admin`/`admin` login. Not a
  secret, but a hint at a default account name.
- "Stratum PRO" branding: logo, caption and alt text on the sign-in screen
  (`pro:src/screens/login.jsx:63,107`).

Outside the listed sources but seen on the way: the fleet repository's
agent-instructions file at its root names the team's internal issue tracker
host and several ticket keys. This repository's lint rejects both, so nothing
from that file may be copied here.

## Scope decisions

- **`probe` of the identity for the exec WebSocket** (`oss:src/session.js:
  297-308`) is left to the `terminal` domain, its only caller.
- **Support-archive download's own 401 hop** (`pro:src/fleet.js:340`) is left
  to the `support` domain; clusters, operations, support archives and
  registration tokens in `pro:src/fleet.js` belong to their own domains.
- **Request result contract** (`api-requests-and-csrf.feature`) is specified
  here because sign-in, sign-out and the identity check are its first users
  and both roots implement it identically. The `data` domain covers the
  Kubernetes transport; the anti-forgery header on Kubernetes writes is cited
  from both transports in the diverged pair so it is not lost between domains.
- **`sign-in-backdrop.feature`** records the decorative animation because
  `pro:src/ui/login-network.jsx` is a listed source. It is presentation with
  three behaviors worth keeping if the animation is kept at all (no data
  fetched before sign-in, reduced motion respected, paused when hidden). It is
  not tagged `@drop`; whether the unified sign-in screen keeps a backdrop is a
  design decision nobody was available to make.
- **`session-coordination.feature`** is `@oss` only. The fleet dashboard has
  no equivalent: no serialization of sign-in and sign-out, no cross-tab
  fencing, no discarding of answers from a previous session. Recommended for
  the unified app in both modes; the hazards it addresses (a late answer
  from the previous user's session painting into the new one) are not
  specific to token sign-in.
- **`@fleet`** is applied to SSO and the sign-in return target because both
  need hub routes. Username/password sign-in itself is tagged `@pro` without
  `@fleet`: the fleet manager also runs in standalone mode and the code does
  not tie the form to fleet mode.

## Not resolved

- Whether the open server ever returns sign-in providers, and whether the
  unified sign-in screen should offer token and password forms side by side
  when a backend advertises both. Nothing in either root answers this.
- Whether the two anti-forgery header names can be unified (server contract).
- The exact wording of the sign-in rejection codes other than
  `invalid_token` and `invalid_credentials` comes from the servers, which were
  not in scope.

## Fix pass

Applied `VERIFY.md` and `spec/decisions.md`. The domain now has 131 scenarios
(was 114) and still 9 `@diverged` pairs.

| | Count |
|---|---|
| Scenarios added (verifier's UNCOVERED 1 to 8) | 8 |
| Scenarios corrected (wording or citation) | 5 |
| Scenarios newly tagged `@drop` | 17 |
| `@fixed` scenarios added | 9 |
| Titles renamed | 0 |

**Corrected.** "A member cluster's own 401 does not sign the user out" (second
Then reworded, transport citation added); "Any signed-in identity is offered
write controls" (the 403 step now cites the shell's own write path and is
worded as what that path shows); "The identity check, sign-in and sign-out
never trigger the expiry redirect" (step now says the start-up identity
check); two backdrop citations gained their listener lines.

**D1, per pair.** Pairs 1, 2, 3 and 5 keep both variants, chosen by backend
(`# decision: D4`); pair 8 keeps both (`D5`); pair 9 keeps both (`D14`).
Pair 4: the pro variant is dropped by D1 and the oss variant by D2 (it fails
silently), replaced by "A sign-in that yields no identity is reported on the
sign-in screen". Pair 6: pro dropped. Pair 7: oss dropped (`D1`, `D7`); the
pro confirmation scenarios carry `D7` and now apply to both backends.

**D2, per bug in the list above.**

| Bug | Dropped | `@fixed` |
|---|---|---|
| 1 | "A sign-out the server did not confirm still shows the sign-in screen", "Sign-out without an obtainable anti-forgery token still leaves the dashboard", new "Sign-out with an unreadable session marker sends nothing and still leaves the dashboard" | "A sign-out the server did not confirm is reported" |
| 2 | "An unreadable session marker blocks sign-in and sign-out" | "An unreadable session marker is replaced instead of blocking sign-in" |
| 3 | "A sign-in or sign-out in another tab ends this tab's session view" | "A session change in another tab makes this tab check its session again" |
| 5 | new "A whitespace-only username is submitted as an empty username" | "A whitespace-only username cannot be submitted" |
| 6 | new "Session expiry in the fleet dashboard keeps the held identity and token" | none: the surviving oss scenario "Expiry discards everything held for the ended session" states it |
| 7 | "A sign-in that yields no identity stays on the sign-in screen" | see pair 4 |
| 10 | "Any failure of the identity request is treated as no session" | "A failed identity request at start-up is an error, not a signed-out state" |

Bugs 4 (dead fallback text, D3), 8 (a server question) and 9 (settled in the
shell domain by D6) got no `@fixed` scenario.

**Other decisions.** D4, D5 and D14 each got one `@fixed` scenario stating the
by-backend rule ("The sign-in form follows the backend the configuration
describes", "The anti-forgery header name follows the backend", "Write gating
follows the backend"). D20: all five backdrop scenarios are `@drop`. D6 is
noted on "A configuration that could not be loaded offers no write controls".

**Verifier findings not acted on.**

- "Only a non-secret marker is shared between tabs" still names the storage
  key. The verifier calls it acceptable if the rewrite must interoperate with
  a legacy tab during rollout; nobody was available to rule that out, and the
  title is in use by the implementer.
- "Later 401s from requests already in flight keep the remembered
  destination" was verified with a remark, not a defect; left as written.
- The three unplaced pro presentation files (`brand-logo.js`, `icons.jsx`,
  `app.css`) belong in `spec/domains.md`, which this pass may not edit.

**Open after this pass.** Added scenario "A session change whose completion
cannot be published leaves every tab fenced" describes what may be a defect
(tabs stay fenced until another sign-in), but it was not in the bug list, so
D2 was not applied; someone should decide. The `@fixed` wordings for bugs 1,
3 and 10 are this pass's own reading of "corrected"; they name no exact
message text.
