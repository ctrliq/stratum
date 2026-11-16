Feature: Kubernetes request transport
  Every Kubernetes request the dashboard makes goes to a same-origin proxy
  path. This feature records what a request carries, how long it may take,
  how it is cancelled and what the caller gets back. Sign-in screens belong
  to the auth domain; only the transport's reaction to a 401 is recorded here.

  # ---------------------------------------------------------------- addressing

  @oss
  # source: oss:src/cluster-context.js:81
  # source: oss:src/k8sutil.js:69-70,80
  Scenario: Requests go to the proxy prefix the server configuration names
    Given the server configuration names the Kubernetes proxy prefix "/k8s"
    When the dashboard requests the Kubernetes path "/api/v1/pods"
    Then the browser requests "/k8s/api/v1/pods" on the dashboard's own origin

  @oss
  # source: oss:src/cluster-context.js:81
  Scenario: The proxy prefix defaults to /k8s when the configuration names none
    Given the server configuration names no Kubernetes proxy prefix
    When the dashboard requests the Kubernetes path "/version"
    Then the browser requests "/k8s/version"

  @oss
  # source: oss:src/k8sutil.js:4-6,69-70
  Scenario: The proxy prefix is resolved again for every request
    Given a request was already sent under one proxy prefix
    When the server configuration is re-read and names a different prefix
    Then the next request uses the new prefix

  @pro
  # source: pro:src/cluster-context.js:78-81
  Scenario: A standalone hub serves the one cluster at the configured prefix
    Given the hub configuration says the mode is "standalone"
    When the dashboard requests the Kubernetes path "/api/v1/pods" for any cluster id
    Then the browser requests "/k8s/api/v1/pods", or the configured prefix when one is named

  @pro @fleet
  # source: pro:src/cluster-context.js:78-81
  Scenario: A fleet hub serves each cluster under its own proxy path
    Given the hub configuration says the mode is "fleet"
    When the dashboard requests the Kubernetes path "/api/v1/pods" for the cluster "edge/01"
    Then the browser requests "/api/v1/clusters/edge%2F01/k8s/api/v1/pods"

  @pro
  # source: pro:src/cluster-context.js:68,84
  # source: pro:src/k8sproxy.js:135-146
  Scenario: Requests not tied to a screen's cluster follow the current selection
    Given no cluster has been selected yet
    When a request is made without naming a cluster
    Then it is addressed to the cluster id "local"

  @pro @fleet
  # source: pro:src/k8sclient.js:54-56,62-82
  # source: pro:src/app.jsx:209-214
  Scenario: Requests started for one cluster stay on that cluster after a switch
    Given a screen was opened on cluster "a"
    And an action on that screen is still in progress
    When the user switches to cluster "b"
    And the action issues another request
    Then that request is still addressed to cluster "a"

  @oss @pro
  # source: oss:src/k8sutil.js:80-83
  # source: pro:src/k8sproxy.js:67,93,119
  Scenario: Every request carries the session cookie and asks for JSON
    When the dashboard sends any Kubernetes request
    Then the request is same-origin and includes the session cookie
    And the request header "Accept" is "application/json"

  @oss
  # source: oss:src/live.js:159-161
  # source: oss:src/k8sutil.js:69-70
  Scenario: A read may ask the apiserver for a different representation
    When the dashboard reads Secrets as a metadata-only table
    Then the request header "Accept" is "application/json;as=Table;g=meta.k8s.io;v=v1"

  # --------------------------------------------------------------------- writes

  @diverged @oss
  # decision: D5 — stands on the dashboard server backend; the header follows the backend
  # source: oss:src/k8sutil.js:75-76
  Scenario: Writes carry the CSRF token in the X-Stratum-CSRF header
    Given the session holds a CSRF token
    When the dashboard sends a POST, PUT, PATCH or DELETE through the proxy
    Then the request carries the token in the header "X-Stratum-CSRF"

  @diverged @pro
  # decision: D5 — stands on the hub backend; the header follows the backend
  # source: pro:src/k8sproxy.js:114-115
  Scenario: Writes carry the CSRF token in the X-Fleet-CSRF header
    Given the session holds a CSRF token
    When the dashboard sends a POST, PUT, PATCH or DELETE through the proxy
    Then the request carries the token in the header "X-Fleet-CSRF"

  @oss @pro
  # source: oss:src/k8sutil.js:75-76
  # source: pro:src/k8sproxy.js:67,93,114-115
  Scenario: Reads never carry the CSRF token
    Given the session holds a CSRF token
    When the dashboard sends a GET through the proxy
    Then the request carries no CSRF header

  @oss @pro
  # source: oss:src/k8sutil.js:75-76
  # source: pro:src/k8sproxy.js:114-115
  Scenario: A write without a known CSRF token is still sent, without the header
    Given the session holds no CSRF token
    When the dashboard sends a write through the proxy
    Then the request is sent with no CSRF header
    And the server's answer, a refusal included, is what the caller sees

  @oss @pro
  # source: oss:src/k8sutil.js:22-26,74,118
  # source: pro:src/k8sproxy.js:34-38,113,145-146
  Scenario Outline: A PATCH is sent under the content type of its patch kind
    When the dashboard sends a "<kind>" patch
    Then the request header "Content-Type" is "<content type>"

    Examples:
      | kind      | content type                               |
      | merge     | application/merge-patch+json               |
      | strategic | application/strategic-merge-patch+json     |
      | json      | application/json-patch+json                |

  @oss @pro
  # source: oss:src/k8sutil.js:118
  # source: pro:src/k8sproxy.js:145-146
  Scenario: A PATCH with no stated kind is a merge patch
    When the dashboard sends a patch without naming its kind
    Then the request header "Content-Type" is "application/merge-patch+json"

  @oss @pro
  # source: oss:src/k8sutil.js:74,82
  # source: pro:src/k8sproxy.js:113,119
  Scenario: A create or replace sends its body as JSON
    When the dashboard sends a POST or PUT with a body
    Then the body is the JSON encoding of the object
    And the request header "Content-Type" is "application/json"

  @oss @pro
  # source: oss:src/k8sutil.js:74,82
  # source: pro:src/k8sproxy.js:113,119
  Scenario: A request without a body carries no content type
    When the dashboard sends a DELETE with no body
    Then the request has no "Content-Type" header and no body

  @diverged @pro
  # decision: D8 — applies on both backends
  # source: pro:src/k8sproxy.js:15-17,109-110,119,123-125
  Scenario: A redirected write is refused, not followed
    When the proxy answers a write with a redirect
    Then the dashboard does not request the redirect target
    And the write fails with the message "the write was redirected, and the dashboard does not follow redirects on writes"
    And the method, body, CSRF token and session cookie are not replayed anywhere

  @diverged @oss @drop
  # decision: D1, D8 — writes refuse redirects; the pro variant stands
  # assumption: unverified at runtime; a browser test in which the proxy answers a PATCH with a same-origin 307 and the target records the replayed method, body and CSRF header would confirm it
  # source: oss:src/k8sutil.js:80-83
  # assumption: the request sets no redirect policy, so the browser's default (follow) applies; no guard against redirects exists in this root
  Scenario: A redirected write is followed by the browser
    When the proxy answers a write with a 307 or 308 redirect to a same-origin path
    Then the browser repeats the write against the redirect target

  # ------------------------------------------------------- results and errors

  @oss @pro
  # source: oss:src/k8sutil.js:59-62,90,112-118
  # source: pro:src/k8sproxy.js:104-106,121
  Scenario: A write that cannot reach the server reports a failure instead of crashing
    When a write fails at the network level
    Then the action that issued it receives a failed result with status 0 and the browser's error message
    And no unhandled error is raised

  @oss @pro
  # source: oss:src/k8sutil.js:93-94
  # source: pro:src/k8sproxy.js:126-128
  Scenario Outline: A refused request's error message comes from the best available source
    When the proxy answers a write with status 422 and the body <body>
    Then the reported error message is "<message>"

    Examples:
      | body                                    | message          |
      | {"message":"replicas must be >= 0"}     | replicas must be >= 0 |
      | {"error":"csrf"}                        | csrf             |
      | the plain text "upstream unavailable"   | upstream unavailable |
      | nothing at all                          | HTTP 422         |

  @oss @pro
  # source: oss:src/k8sutil.js:93,98
  # source: pro:src/k8sproxy.js:126,132
  Scenario: A successful write with an empty or non-JSON body still succeeds
    When the proxy answers a write with status 200 and no JSON body
    Then the write is reported as successful with no returned object

  @diverged @oss
  # source: oss:src/k8sutil.js:95-97
  Scenario: A successful read that carries no JSON is a failed read
    When the proxy answers a GET with status 200 and an empty or non-JSON body
    Then the read is reported as failed with the message "Kubernetes response did not contain a JSON answer"

  @diverged @pro @drop
  # decision: D1 — the oss variant (a named failed read) stands
  # source: pro:src/k8sproxy.js:62-76
  # source: pro:src/live.js:417-425
  Scenario: A successful read that cannot be decoded counts as an outage
    When the proxy answers a GET with status 200 and a body that is not JSON
    Then the read fails without an HTTP status
    And a cluster load treats it the same as a network failure

  @oss
  # source: oss:src/k8sutil.js:69,80-82
  # source: oss:src/node-shell.js:95-110
  Scenario: A cleanup request may be marked to outlive the page
    Given the dashboard opened a host shell
    When the page is being closed and the dashboard deletes what it created
    Then the delete is sent as a keepalive request, so it completes after the page is gone
    And nothing waits for its answer

  @oss
  # source: oss:src/k8sutil.js:88-90
  Scenario: A response whose body cannot be read keeps its HTTP status
    When the proxy answers with status 502 and the body cannot be read
    Then the request is reported as failed with status 502 and the browser's error message

  @diverged @oss
  # source: oss:src/k8sutil.js:101-110
  Scenario: A list read that fails for any reason yields "no answer" and a console warning
    When a screen's read fails with an HTTP error, a network error, a timeout or a cancellation
    Then the screen receives the fallback value it asked for instead of an error
    And one warning naming the path and the failure is written to the console

  @diverged @pro @drop
  # decision: D1 — the oss variant stands: the warning always names the path
  # source: pro:src/k8sproxy.js:62-86
  # source: pro:src/k8sclient.js:73-78
  Scenario: A failed list read yields the fallback and a warning that names the path only for an HTTP error
    When a screen's read fails with an HTTP error, a network error, a timeout or a cancellation
    Then the screen receives the fallback value it asked for instead of an error
    And one warning is written to the console
    And the warning names the path and status for an HTTP error, and carries only the browser's own message otherwise

  # ------------------------------------------------------ deadlines and cancel

  @diverged @oss
  # decision: D8 — applies to every request on both backends
  # source: oss:src/k8sutil.js:28-31,42-57,77
  Scenario: Every non-streaming request is abandoned after 15 seconds by default
    Given a request states no deadline of its own
    When the proxy accepts the request and then never answers
    Then the request is aborted 15 seconds after it was sent
    And it is reported as failed with status 0

  @diverged @pro @drop
  # decision: D1, D2, D8 — unbounded requests are the cause of the hanging load (NOTES P1); the oss 15 second default stands and is the corrected behavior
  # source: pro:src/k8sproxy.js:47-48,64-67,111-119
  Scenario: A request has a deadline only when its caller states one
    Given a request states no deadline of its own
    When the proxy accepts the request and then never answers
    Then the request stays pending until its caller cancels it or the page goes away

  @oss @pro
  # source: oss:src/k8sutil.js:42-57,69,77
  # source: pro:src/k8sproxy.js:47-60
  Scenario: A caller's deadline replaces the default
    Given a request states a deadline of 3 seconds
    When the proxy does not answer within 3 seconds
    Then the request is aborted at 3 seconds

  @oss @pro
  # source: oss:src/k8sutil.js:33-37,46-49
  # source: pro:src/k8sproxy.js:40-53
  Scenario: A caller's cancellation and the deadline bound the same request
    Given a request has both a deadline and a cancellation handle from its caller
    When the caller cancels before the deadline
    Then the request is aborted immediately, without waiting for the deadline

  @oss @pro
  # source: oss:src/k8sutil.js:48
  # source: pro:src/k8sproxy.js:51
  Scenario: A request whose caller already cancelled is aborted at once
    Given the caller cancelled before the request was issued
    When the request is issued with that cancellation handle
    Then it is aborted without waiting for an answer

  @pro
  # decision: D8 — requests are cancelled when their screen, cluster or namespace changes
  # source: pro:src/k8sclient.js:50-53,62-72,93-98
  # source: pro:src/app.jsx:211-214
  Scenario: Leaving a cluster cancels its in-flight reads and closes its terminals
    Given reads and terminal connections are open against cluster "a"
    When the user switches to cluster "b"
    Then the reads still in flight against cluster "a" are aborted
    And the terminal connections to cluster "a" are closed

  @pro
  # source: pro:src/k8sclient.js:50-53,81-82
  Scenario: Leaving a cluster does not cancel writes already issued against it
    Given a delete was issued against cluster "a" to clean up a pod created there
    When the user switches to cluster "b" before the delete answers
    Then the delete is not aborted and still lands on cluster "a"

  @pro
  # source: pro:src/k8sclient.js:50-53,80
  # source: pro:src/k8sproxy.js:88-102
  Scenario: A followed log stream is cancelled only by whoever opened it
    Given a pod log is being followed
    When reads against that cluster are cancelled because the selection moved
    Then the log stream continues until its own cancellation handle is used

  @pro
  # source: pro:src/k8sproxy.js:88-102
  Scenario: A streaming body is handed over unparsed
    When the dashboard opens a followed pod log
    Then the response body is delivered as a raw stream, never parsed as JSON

  @pro
  # source: pro:src/k8sproxy.js:94-99,101
  Scenario Outline: A stream that cannot start reports why
    When opening a stream ends with <outcome>
    Then the caller is told the stream failed with status <status> and the message "<message>"

    Examples:
      | outcome                                   | status | message      |
      | status 403 and the text "forbidden"       | 403    | forbidden    |
      | status 500 and an empty body              | 500    | HTTP 500     |
      | a network error "Failed to fetch"         | 0      | Failed to fetch |

  @pro
  # source: pro:src/k8sproxy.js:94-99
  Scenario: A stream that answers without a body is a failed stream
    When opening a stream answers with status 200 and no body
    Then the caller is told the stream failed with status 200 and the message "HTTP 200"

  @pro
  # source: pro:src/k8sclient.js:83-92
  Scenario: Terminal connections use the exec channel protocol and are forgotten when they close
    When the dashboard opens a terminal connection to a pod
    Then the WebSocket is opened under the cluster's proxy path with the subprotocol "v4.channel.k8s.io"
    And once the socket closes it is no longer tracked for cleanup

  # -------------------------------------------------------------- 401 handling

  @diverged @oss @drop
  # decision: D1, D7 — a proxied 401 is confirmed with the identity check first, on both backends
  # source: oss:src/k8sutil.js:62-64,84
  # source: oss:src/session.js:163-174
  # source: oss:src/app.jsx:181-189
  Scenario: A 401 from the proxy ends the session and sends the user to sign-in
    Given the server requires sign-in
    And the user is on "/nodes?node=cp-1"
    When any proxied Kubernetes request answers 401
    Then the cached cluster data and session are cleared
    And the user is taken to the sign-in screen, which remembers "/nodes?node=cp-1"
    And the request that hit the 401 still reports its failure to whatever issued it

  @oss
  # source: oss:src/app.jsx:183-184
  Scenario: A 401 from the proxy is ignored when the server requires no sign-in
    Given the server does not require sign-in
    When a proxied Kubernetes request answers 401
    Then the user stays where they are
    And the request is reported as a failure with status 401

  @oss
  # source: oss:src/k8sutil.js:65-66,84
  # source: oss:src/app.jsx:71
  Scenario: Sign-out cleanup may see a 401 without triggering the sign-in redirect
    Given the user is signing out and the dashboard is deleting host shells it opened
    When one of those cleanup deletes answers 401
    Then no sign-in redirect is triggered by that answer
    And the failure is reported in the console

  @diverged @pro
  # decision: D7 — applies on both backends
  # source: pro:src/k8sproxy.js:18-24,69,97,129
  # source: pro:src/fleet.js:116-136,89
  Scenario: A 401 from the proxy sends the user to sign-in only when the hub confirms its own session expired
    Given the hub requires sign-in
    When a proxied Kubernetes request answers 401
    Then the dashboard asks the hub who the user is
    And when the hub also answers 401, the user is taken to the sign-in screen

  @diverged @pro
  # decision: D7 — applies on both backends
  # source: pro:src/k8sproxy.js:18-24,127-131
  # source: pro:src/fleet.js:116-136
  Scenario: A 401 that the hub does not confirm is shown as the cluster's own refusal
    Given the hub requires sign-in
    When a proxied write answers 401
    And the hub answers the identity check with anything other than 401, a timeout or network error included
    Then the user is not signed out
    And the write is reported as failed with the 401's own error message
    And the result says the hub session is still valid

  @pro
  # source: pro:src/fleet.js:125-136
  Scenario: The hub identity check after a 401 is bounded and shared
    Given several proxied requests answer 401 at the same moment
    When the dashboard checks the hub session
    Then a single identity request is made for all of them
    And it is abandoned after 5 seconds, which counts as "session still valid"

  @pro
  # source: pro:src/fleet.js:127-129
  Scenario: No identity check is made when the hub requires no sign-in
    Given the hub does not require sign-in
    When a proxied request answers 401
    Then no identity request is made and the user is not redirected

  # ------------------------------------------------------ session-change guards

  @oss
  # source: oss:src/k8sutil.js:71-72
  # source: oss:src/session.js:67-82,106-110
  Scenario: No request is sent while the session is being replaced
    Given another browser tab is in the middle of signing in or out
    When the dashboard is about to send a Kubernetes request
    Then the request is not sent
    And it is reported as failed with the message "The session changed"

  @oss
  # source: oss:src/k8sutil.js:92
  Scenario: An answer that arrives after the session changed is discarded
    Given a Kubernetes request is in flight
    When the user signs out, or a different session is established, before it answers
    Then the answer's content is not delivered
    And the request is reported as failed with the message "The session changed"

  @oss
  # source: oss:src/k8sutil.js:92
  Scenario: A discarded 401 answer still reports status 401
    Given a Kubernetes request is in flight
    When the session changes and the request then answers 401
    Then the request is reported as failed with status 401 and the message "The session changed"
    And any other discarded answer is reported with status 0

  @oss
  # source: oss:src/k8sutil.js:84
  # source: oss:src/session.js:169-174
  Scenario: A 401 for a request from a previous session does not sign the new session out
    Given a request was sent under one session
    And a new session has since been established
    When the old request answers 401
    Then the user is not redirected to sign-in
