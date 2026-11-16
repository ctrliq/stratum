# auth: acceptance report

Checked on 2026-11-13 against `dash-next/src` by reading the code and running
`pnpm exec vitest run src/domain/auth src/shared/fetchers` (all passing). Nothing was
run in a browser.

**131 scenarios checked: 92 SATISFIED, 8 PARTIAL, 14 BLOCKED, 17 DROPPED — not accepted.**

The 14 BLOCKED scenarios are all the open dashboard's cross-tab session fence; no decision
in `decisions.md` settles it, so they stay open. The 8 PARTIAL ones are listed first below.

Paths are relative to `dash-next/src`.

## Scenarios that are not satisfied

### PARTIAL

| Feature | Scenario | Expected | Actual | Where |
|---|---|---|---|---|
| sign-in.feature | The sign-in form follows the backend the configuration describes | Any configuration without `token` shows the username and password form | The password form is rendered only when `providers` contains `local`. A hub answering `providers: ["oidc"]` shows only the SSO button; a hub answering `providers: []` (allowed by `openapi/NOTES.md`: "subset of local, oidc, may be empty") shows the heading and no way to sign in | `domain/auth/pages/AuthLogin/index.tsx:54-56` |
| api-requests-and-csrf.feature | Requests are same-origin JSON with the session cookie | The request asks for JSON | No `Accept: application/json` header is ever set; credentials and the JSON content type on a body do hold | `shared/fetchers/requests.ts:65-72` |
| api-requests-and-csrf.feature | A request that gets no HTTP answer is reported as a network failure | Failure with status 0, code `network`, no exception | The failure is a rejected promise with `status: 'unknown'`; `failureOf` maps it to code `network` with the browser's message. Contract-only difference, no visible effect; needs a spec amendment or a decision | `shared/fetchers/sameOriginFetch.ts:24-28`, `domain/auth/utils/selectors.ts:25-29` |
| session-expiry.feature | A member cluster's own 401 does not sign the user out | A read that has a default falls back to it with a console warning | The user stays signed in and the 401 reaches the call, but there is no default fallback and no console warning; the query just fails | `shared/fetchers/sameOriginFetch.ts:36-45`, `domain/auth/components/SessionProvider/SessionProvider.tsx:66-84` |
| session-expiry.feature | A session change in another tab makes this tab check its session again | The tab discards its identity and token, then asks the server | The identity and token are kept while `GET /api/v1/me` is in flight and are only replaced by its answer. If that request times out (5 s) or fails on the network, the old identity and CSRF token stay in use against a possibly different cookie. Cached data is reset either way | `domain/auth/components/SessionProvider/SessionProvider.tsx:125-137`, `:66-84` |
| session-expiry.feature | Clearing browser storage in another tab ends this tab's session view | Identity, token and data are discarded and the sign-in screen is shown | Treated like any other marker change: the tab re-checks with the server and stays signed in when the server says so. Consistent with the `@fixed` sibling, but this scenario is not tagged and no decision covers it | `domain/auth/components/SessionProvider/SessionProvider.tsx:126-131` |
| sign-out.feature | A refused host shell delete is reported and does not stop sign-out | A 401 on that delete does not trigger the session-expired redirect | The delete goes through the normal Kubernetes transport, so a 401 calls `onUnauthorized`, which asks `/api/v1/me` (up to 5 s, added on top of the 3 s bound) and expires the session if that is 401. Sign-out then still completes and clears the remembered destination | `domain/terminal/components/NodeShell/ownedHelpers.ts:54-63`, `shared/fetchers/sameOriginFetch.ts:42-44` |
| sign-out.feature | Sign-out waits briefly for host shells that are still being created | After 3 s a console error says the job may need cleanup | The 3 s wait is there; nothing is written to the console for a creation that did not finish, and that job is never deleted | `domain/terminal/components/NodeShell/ownedHelpers.ts:75-94` |

### BLOCKED

All of these carry the same reason in `trace.md`: there is no cross-tab fence or lock.
Confirmed: nothing uses `navigator.locks`, the marker in `localStorage` is only ever written
(`inProgress` is always `false`), never read, and no request is tagged with a session
revision. The reason still holds.

| Feature | Scenario | What the code does instead |
|---|---|---|
| api-requests-and-csrf.feature | The token is withheld while the session is changing | The held token keeps being sent until the session query changes (`SessionProvider.tsx:100-102`) |
| session-bootstrap.feature | A session change during the start-up check falls back to sign-in | The trace says the change "re-reads /me"; it does not. `fetchQuery` joins the request already in flight, so the answer from before the change is applied (`SessionProvider.tsx:66-74`) |
| session-coordination.feature | Session changes are serialized across tabs when the browser supports it | Serialized within one tab only (`constants.ts:22`, mutation `scope`) |
| session-coordination.feature | Waiting for another tab's session change is bounded | No waiting on other tabs |
| session-coordination.feature | A queued sign-in that has been overtaken sends nothing | A queued sign-in is sent when its turn comes |
| session-coordination.feature | Other tabs are fenced off while the cookie is being replaced | Other tabs learn of the change only after it completed |
| session-coordination.feature | A failed session change still releases the fence | A failed change publishes nothing |
| session-coordination.feature | A session change whose completion cannot be published leaves every tab fenced | A failed `localStorage` write is ignored (`SessionProvider.tsx:36-43`) |
| session-coordination.feature | An identity answer from before a session change is discarded | Not discarded: a `/me` answer that lands after sign-out puts the identity back until the next 401 |
| session-expiry.feature | An answer that arrives after the session changed is not acted on | Queries are cancelled on sign-out; a mutation in flight is not, and its late 401 would expire a newer session |
| sign-in.feature | A sign-in overtaken by another session change ends silently | Not tracked |
| sign-in.feature | A successful sign-in is not applied if the session changed meanwhile | Applied |
| sign-out.feature | A sign-out overtaken by a new session leaves the new session alone | Not tracked |
| write-gating.feature | Write controls are withdrawn while the session is changing | Write controls follow the identity only |

## Satisfied through a decision

- `A write without a body sends an empty JSON object` — the code sends no body (D34). D34
  reasons from the hub's handlers only; the same `fetchLogout` is used against the dashboard
  server, whose handler was not checked.
- `A configuration that could not be loaded offers no write controls` — there is no app to
  offer them in (D6). Under `vite dev` the login-free fallback does offer writes.
- `A sign-out the server did not confirm is reported` — holds for network failures, 403 and
  a `/me` answer without a token. A 401 from `/me` without a token, or from the logout POST,
  is treated as "already signed out" and leaves the dashboard without a message
  (`domain/auth/hooks/useSignOut.ts:19-30`).

## Notes on satisfied scenarios

- `An unreadable session marker is replaced instead of blocking sign-in`: the marker is
  never read, so it cannot block; it is rewritten only after a successful sign-in.
- `An error answer is reported with a code and a message`: the body is parsed by content
  type. A JSON error served without a JSON content type, or a body with `message` but no
  `error`, is reported as `http_<status>` with the raw text or `HTTP <status>`
  (`shared/fetchers/sameOriginFetch.ts:13-22`, `domain/auth/utils/selectors.ts:25-35`).
- `The form is locked while a sign-in is in flight`: holds through disabled controls; there
  is no guard in `onSubmit` itself.
- Expiry handling compares against `config.k8sBase` unguarded while the transport uses
  `config.k8sBase || '/k8s'`; an empty `k8sBase` would make every 401 a "Kubernetes" 401
  (`SessionProvider.tsx:109` versus `utils/transforms.ts:29`).

## EXTRA behavior

| Behavior | Where | Looks like |
|---|---|---|
| A signed-in visitor opening `/login?return=cli-authorize` is sent to `/cli/authorize` by a full page load; the spec says a signed-in visitor on `/login` goes to `/` | `domain/auth/pages/AuthLogin/index.tsx:32-38` | spec gap |
| The session-check error screen has a "Try again" button | `domain/auth/components/SessionUnavailable/SessionUnavailable.tsx:18-20` | spec gap |
| A 401 from `/me` or from logout during sign-out counts as signed out | `domain/auth/hooks/useSignOut.ts:19-20` | spec gap |
| Host shells are deleted on `pagehide` with `keepalive` | `domain/terminal/components/TerminalHost/TerminalHost.tsx:93-101` | belongs to `terminal` |
| Writes refuse redirects (`redirect: 'manual'`) | `shared/fetchers/requests.ts:89` | D8 |
| The log stream calls the unauthorized handler outside the shared transport | `shared/streaming/logStream.ts:64-66` | consistent with D7 |
| After another tab's change every session-bound query is reset even when the server confirms the same user | `domain/auth/components/SessionProvider/SessionProvider.tsx:128-130` | follows the `@fixed` scenario |

## Trace inconsistencies

- `A write without a body sends an empty JSON object` is marked `dropped`; the scenario has
  no `@drop` tag. It is a D34 override.
- `A session change during the start-up check…`: the blocked reason describes a re-read
  that does not happen (see above).
- `Sign-out waits briefly…` and `A refused host shell delete…` are marked implemented; each
  misses one step (see PARTIAL).
- `Without cross-tab locks…`, `Only a non-secret marker…` and `An unreadable session
  marker is replaced…` are implemented by the absence of the fence, not by code in the
  files named.
- Every traced path exists. Rows cite `forms/`, `utils/` and `pages/AuthLogin/` as
  directories, so `RequireSession.tsx`, `SignInForm.tsx`, `CredentialField.tsx`,
  `schemas/signIn.ts` and `shared/fetchers/backend.ts` are never named.
- Totals line says 18 dropped; 17 scenarios carry `@drop`.
