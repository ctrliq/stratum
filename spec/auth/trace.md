# auth: trace

Scenario → status → implementing files, written by `implement-spec`. Paths are relative to `~/Code/stratum`. `dropped` rows carry `@drop` in the feature file.
Totals: 99 implemented, 18 dropped, 14 blocked.


## api-requests-and-csrf.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Open dashboard writes carry the X-Stratum-CSRF header | implemented | dash-next/src/shared/fetchers/requests.ts, dash-next/src/shared/fetchers/sameOriginFetch.ts |
| Fleet dashboard writes carry the X-Fleet-CSRF header | implemented | dash-next/src/shared/fetchers/requests.ts, dash-next/src/shared/fetchers/sameOriginFetch.ts |
| The anti-forgery header name follows the backend | implemented | dash-next/src/shared/fetchers/requests.ts, dash-next/src/shared/fetchers/sameOriginFetch.ts |
| GET requests carry no anti-forgery header | implemented | dash-next/src/shared/fetchers/requests.ts, dash-next/src/shared/fetchers/sameOriginFetch.ts |
| A write without a known token is sent without the header | implemented | dash-next/src/shared/fetchers/requests.ts, dash-next/src/shared/fetchers/sameOriginFetch.ts |
| The token is taken from the sign-in answer | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx (csrfToken effect), dash-next/src/domain/auth/forms/ |
| The token is refreshed from every successful identity answer | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx |
| The token is withheld while the session is changing | blocked | no cross-tab fence or lock: session changes are serialized within a tab only and other tabs re-check with the server; needs a decision on whether the unified app keeps the open dashboard's fencing |
| Requests are same-origin JSON with the session cookie | implemented | dash-next/src/shared/fetchers/requests.ts (toHeaders, toInit) |
| A write without a body sends an empty JSON object | dropped | D34 |
| A request that gets no HTTP answer is reported as a network failure | implemented | dash-next/src/shared/fetchers/sameOriginFetch.ts (status "unknown" rather than 0), dash-next/src/domain/auth/utils/selectors.ts (failureOf: code "network") |
| An error answer is reported with a code and a message | implemented | dash-next/src/domain/auth/utils/selectors.ts (failureOf) |
| A success answer without JSON is still a success | implemented | dash-next/src/shared/fetchers/requests.ts, dash-next/src/shared/fetchers/sameOriginFetch.ts |
| A request can be bounded by a deadline | implemented | dash-next/src/shared/fetchers/requests.ts, dash-next/src/shared/fetchers/sameOriginFetch.ts |
| A request can be cancelled by its caller | implemented | dash-next/src/shared/fetchers/requests.ts, dash-next/src/shared/fetchers/sameOriginFetch.ts |
| Sign-in and sign-out requests are bounded | implemented | dash-next/src/shared/fetchers/requests.ts, dash-next/src/shared/fetchers/sameOriginFetch.ts |

## session-bootstrap.feature

| Scenario | Status | Implemented in |
|---|---|---|
| An existing session opens the dashboard directly | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/transforms.ts |
| No session sends the visitor to sign-in, remembering the destination | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/transforms.ts |
| Any failure of the identity request is treated as no session | dropped | — |
| A failed identity request at start-up is an error, not a signed-out state | implemented | dash-next/src/domain/auth/utils/transforms.ts (toSession), dash-next/src/domain/auth/components/SessionUnavailable/SessionUnavailable.tsx |
| A server that does not require sign-in opens the dashboard without a session check | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/transforms.ts |
| A session change during the start-up check falls back to sign-in | blocked | superseded by the @fixed re-check: a change during start-up re-reads /me instead of discarding the answer |
| The sign-in page redirects away when there is nothing to sign in to | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/transforms.ts |
| The sign-in page is shown when sign-in is required and no user is known | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/transforms.ts |
| The identity answer supplies the anti-forgery token | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/transforms.ts |
| The open dashboard keeps the user's name, provider and groups | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/transforms.ts |
| The fleet dashboard keeps the user's name, provider and roles | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/transforms.ts |

## session-coordination.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Sign-in and sign-out requests run one at a time within a tab | implemented | dash-next/src/domain/auth/constants.ts (sessionScope), dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/hooks/useSignOut.ts |
| Session changes are serialized across tabs when the browser supports it | blocked | no cross-tab fence or lock: session changes are serialized within a tab only and other tabs re-check with the server; needs a decision on whether the unified app keeps the open dashboard's fencing |
| Without cross-tab locks only the current tab is coordinated | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx |
| Waiting for another tab's session change is bounded | blocked | no cross-tab fence or lock: session changes are serialized within a tab only and other tabs re-check with the server; needs a decision on whether the unified app keeps the open dashboard's fencing |
| A queued sign-in that has been overtaken sends nothing | blocked | no cross-tab fence or lock: session changes are serialized within a tab only and other tabs re-check with the server; needs a decision on whether the unified app keeps the open dashboard's fencing |
| Other tabs are fenced off while the cookie is being replaced | blocked | no cross-tab fence or lock: session changes are serialized within a tab only and other tabs re-check with the server; needs a decision on whether the unified app keeps the open dashboard's fencing |
| Only a non-secret marker is shared between tabs | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx |
| An unreadable session marker blocks sign-in and sign-out | dropped | — |
| An unreadable session marker is replaced instead of blocking sign-in | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx |
| A failed session change still releases the fence | blocked | no cross-tab fence or lock: session changes are serialized within a tab only and other tabs re-check with the server; needs a decision on whether the unified app keeps the open dashboard's fencing |
| A session change whose completion cannot be published leaves every tab fenced | blocked | no cross-tab fence or lock: session changes are serialized within a tab only and other tabs re-check with the server; needs a decision on whether the unified app keeps the open dashboard's fencing |
| Requests started while a sign-in was pending do not leak into the new session | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx |
| An identity answer from before a session change is discarded | blocked | no cross-tab fence or lock: session changes are serialized within a tab only and other tabs re-check with the server; needs a decision on whether the unified app keeps the open dashboard's fencing |
| Queued notifications from an older change cannot undo a newer sign-in | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx |

## session-expiry.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A 401 from the server's own API sends the user to sign-in | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/predicates.ts |
| A 401 is ignored when the server does not require sign-in | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/predicates.ts |
| The identity check, sign-in and sign-out never trigger the expiry redirect | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/predicates.ts |
| Later 401s from requests already in flight keep the remembered destination | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/predicates.ts |
| Expiry discards everything held for the ended session | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx (identity, token, cached data; host shells wait for the terminal domain) |
| Session expiry in the fleet dashboard keeps the held identity and token | dropped | — |
| An answer that arrives after the session changed is not acted on | implemented | dash-next/src/shared/fetchers/sameOriginFetch.ts, dash-next/src/shared/fetchers/requests.ts (toDiscardedError), dash-next/src/shared/fetchers/backend.ts (sessionRevision); this tab's own changes and another tab's completed change only (D36) |
| A 401 on a Kubernetes call means the session ended | dropped | — |
| A 401 on a proxied Kubernetes call is confirmed with the hub first | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/predicates.ts |
| A member cluster's own 401 does not sign the user out | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/shell/hooks/useReadWarnings.ts (console warning) |
| A hub that does not answer the confirmation in time does not sign the user out | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/predicates.ts |
| Simultaneous confirmations share one request | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/predicates.ts |
| No confirmation is made when sign-in is not required | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/predicates.ts |
| A sign-in or sign-out in another tab ends this tab's session view | dropped | — |
| A session change in another tab makes this tab check its session again | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx (storage event resets the session read and every session-bound read), dash-next/src/shared/fetchers/backend.ts (withSessionChanged) |
| Another tab's session change is ignored when sign-in is not required | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/predicates.ts |
| A marker change without required sign-in still empties cached cluster data | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/predicates.ts |
| Clearing browser storage in another tab ends this tab's session view | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx (treated as a session change: the tab re-checks with the server) |
| A 401 before the shell is listening is returned to the caller only | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx, dash-next/src/domain/auth/utils/predicates.ts |

## sign-in-backdrop.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The backdrop is decorative and fetches nothing | dropped | — |
| The backdrop does not animate for users who prefer reduced motion | dropped | — |
| The backdrop is not drawn on narrow viewports | dropped | — |
| The backdrop pauses while the tab is hidden | dropped | — |
| The backdrop follows the size of its area | dropped | — |

## sign-in.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The sign-in form asks for a service account token | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| The sign-in form asks for a username and a password | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| The sign-in form follows the backend the configuration describes | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/utils/predicates.ts (isDashboardServer) |
| A blank token cannot be submitted | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| A missing username or password cannot be submitted | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| A whitespace-only username is submitted as an empty username | dropped | — |
| A whitespace-only username cannot be submitted | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| The token is sent once, trimmed, and is not kept in the browser | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| The username is sent trimmed and the password as typed | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| The form is locked while a sign-in is in flight | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| A token the cluster rejects is reported | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| A wrong username or password is reported | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| An unreachable dashboard server is reported | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| An unreachable fleet manager is reported | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| Any other sign-in failure shows the server's own message | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| A rejected token stays in the field | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| A failed sign-in keeps the typed username and password | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| Retrying clears the previous error | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| A sign-in overtaken by another session change ends silently | blocked | no cross-tab fence or lock: session changes are serialized within a tab only and other tabs re-check with the server; needs a decision on whether the unified app keeps the open dashboard's fencing |
| A successful sign-in is not applied if the session changed meanwhile | blocked | no cross-tab fence or lock: session changes are serialized within a tab only and other tabs re-check with the server; needs a decision on whether the unified app keeps the open dashboard's fencing |
| Signing in returns to the page the user was heading for | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| Signing in with nowhere to return to opens the root | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| A remembered location of the sign-in page itself is replaced by the root | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| The signed-in identity comes from the sign-in answer | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| The cluster list is loaded before the dashboard opens after sign-in | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| A sign-in that yields no identity stays on the sign-in screen | dropped | — |
| A sign-in that yields no identity still opens the dashboard | dropped | — |
| A sign-in that yields no identity is reported on the sign-in screen | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |
| The sign-in screen shows the server version | implemented | dash-next/src/domain/auth/pages/AuthLogin/index.tsx, dash-next/src/domain/auth/forms/, dash-next/src/domain/auth/utils/ |

## sign-out.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Sign out is offered only when the server requires sign-in | implemented | dash-next/src/domain/auth/hooks/useSignOut.ts, dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx |
| No sign out without required sign-in | implemented | dash-next/src/domain/auth/hooks/useSignOut.ts, dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx |
| Signing out returns to the sign-in screen with no remembered destination | implemented | dash-next/src/domain/auth/hooks/useSignOut.ts, dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx |
| Fleet sign-out posts with the token it already holds | dropped | — |
| Open sign-out re-reads the anti-forgery token before posting | implemented | dash-next/src/domain/auth/hooks/useSignOut.ts, dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx |
| A session whose Kubernetes token was revoked can still sign out | implemented | dash-next/src/domain/auth/hooks/useSignOut.ts, dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx |
| Sign-out without an obtainable anti-forgery token still leaves the dashboard | dropped | — |
| Sign-out with an unreadable session marker sends nothing and still leaves the dashboard | dropped | — |
| A sign-out the server did not confirm still shows the sign-in screen | dropped | — |
| A sign-out the server did not confirm is reported | implemented | dash-next/src/domain/auth/hooks/useSignOut.ts, dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx |
| Open host shells are deleted before the session cookie is cleared | implemented | dash-next/src/domain/auth/hooks/useSignOut.ts, dash-next/src/domain/terminal/components/NodeShell/ownedHelpers.ts (releasedWithin) |
| A host shell delete cannot hold sign-out hostage | implemented | dash-next/src/domain/auth/hooks/useSignOut.ts, dash-next/src/domain/terminal/components/NodeShell/ownedHelpers.ts (3 s bound, console warning) |
| A refused host shell delete is reported and does not stop sign-out | implemented | dash-next/src/domain/auth/components/SessionProvider/SessionProvider.tsx (no expiry handling while a session change is in flight), dash-next/src/domain/terminal/components/NodeShell/ownedHelpers.ts |
| Sign-out waits briefly for host shells that are still being created | implemented | dash-next/src/domain/auth/hooks/useSignOut.ts, dash-next/src/domain/terminal/components/NodeShell/ownedHelpers.ts (settledWithin) |
| A sign-out overtaken by a new session leaves the new session alone | blocked | no cross-tab fence or lock: session changes are serialized within a tab only and other tabs re-check with the server; needs a decision on whether the unified app keeps the open dashboard's fencing |
| Signing out clears cached cluster data | implemented | dash-next/src/domain/auth/hooks/useSignOut.ts, dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx |

## sso-sign-in.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The SSO button is offered only when the hub advertises it | implemented | dash-next/src/domain/auth/pages/AuthLogin/, dash-next/src/domain/auth/utils/parsers.ts, dash-next/src/domain/auth/utils/transforms.ts |
| No SSO button without an OIDC provider | implemented | dash-next/src/domain/auth/pages/AuthLogin/, dash-next/src/domain/auth/utils/parsers.ts, dash-next/src/domain/auth/utils/transforms.ts |
| Starting SSO leaves the dashboard with a full page navigation | implemented | dash-next/src/domain/auth/pages/AuthLogin/, dash-next/src/domain/auth/utils/parsers.ts, dash-next/src/domain/auth/utils/transforms.ts |
| SSO cannot be started while a password sign-in is in flight | implemented | dash-next/src/domain/auth/pages/AuthLogin/, dash-next/src/domain/auth/utils/parsers.ts, dash-next/src/domain/auth/utils/transforms.ts |
| A failed SSO round trip is explained with a fixed message | implemented | dash-next/src/domain/auth/pages/AuthLogin/, dash-next/src/domain/auth/utils/parsers.ts, dash-next/src/domain/auth/utils/transforms.ts |
| Unknown error codes in the URL are never displayed | implemented | dash-next/src/domain/auth/pages/AuthLogin/, dash-next/src/domain/auth/utils/parsers.ts, dash-next/src/domain/auth/utils/transforms.ts |
| A sign-in link can ask to return to the CLI authorization page | implemented | dash-next/src/domain/auth/pages/AuthLogin/, dash-next/src/domain/auth/utils/parsers.ts, dash-next/src/domain/auth/utils/transforms.ts |
| SSO carries the return target by name | implemented | dash-next/src/domain/auth/pages/AuthLogin/, dash-next/src/domain/auth/utils/parsers.ts, dash-next/src/domain/auth/utils/transforms.ts |
| SSO sign-in does not carry the remembered in-app destination | implemented | dash-next/src/domain/auth/pages/AuthLogin/, dash-next/src/domain/auth/utils/parsers.ts, dash-next/src/domain/auth/utils/transforms.ts |
| Unknown return targets are ignored | implemented | dash-next/src/domain/auth/pages/AuthLogin/, dash-next/src/domain/auth/utils/parsers.ts, dash-next/src/domain/auth/utils/transforms.ts |
| A failed sign-in does not follow the return target | implemented | dash-next/src/domain/auth/pages/AuthLogin/, dash-next/src/domain/auth/utils/parsers.ts, dash-next/src/domain/auth/utils/transforms.ts |

## write-gating.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Any signed-in identity is offered write controls | implemented | dash-next/src/domain/auth/hooks/useCanWrite.ts, dash-next/src/domain/auth/utils/predicates.ts (isWriteOffered), dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx (WriteAccessContext) |
| Only a signed-in admin is offered write controls | implemented | dash-next/src/domain/auth/hooks/useCanWrite.ts, dash-next/src/domain/auth/utils/predicates.ts (isWriteOffered), dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx (WriteAccessContext) |
| A signed-in user without the admin role is not offered write controls | implemented | dash-next/src/domain/auth/hooks/useCanWrite.ts, dash-next/src/domain/auth/utils/predicates.ts (isWriteOffered), dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx (WriteAccessContext) |
| Write gating follows the backend | implemented | dash-next/src/domain/auth/hooks/useCanWrite.ts, dash-next/src/domain/auth/utils/predicates.ts (isWriteOffered), dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx (WriteAccessContext) |
| The login-free standalone mode offers write controls | implemented | dash-next/src/domain/auth/hooks/useCanWrite.ts, dash-next/src/domain/auth/utils/predicates.ts (isWriteOffered), dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx (WriteAccessContext) |
| A missing identity where sign-in is required offers no write controls | implemented | dash-next/src/domain/auth/hooks/useCanWrite.ts, dash-next/src/domain/auth/utils/predicates.ts (isWriteOffered), dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx (WriteAccessContext) |
| A fleet hub with sign-in disabled offers no write controls | implemented | dash-next/src/domain/auth/hooks/useCanWrite.ts, dash-next/src/domain/auth/utils/predicates.ts (isWriteOffered), dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx (WriteAccessContext) |
| A configuration that could not be loaded offers no write controls | implemented | dash-next/src/domain/auth/hooks/useCanWrite.ts, dash-next/src/domain/auth/utils/predicates.ts (isWriteOffered), dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx (WriteAccessContext) |
| Write controls are withdrawn while the session is changing | blocked | no cross-tab fence or lock: session changes are serialized within a tab only and other tabs re-check with the server; needs a decision on whether the unified app keeps the open dashboard's fencing |
| Write controls follow the identity as soon as it changes | implemented | dash-next/src/domain/auth/hooks/useCanWrite.ts, dash-next/src/domain/auth/utils/predicates.ts (isWriteOffered), dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx (WriteAccessContext) |
