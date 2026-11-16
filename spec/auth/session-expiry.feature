Feature: Session expiry
  A session that ends while the dashboard is open sends the user back to
  sign-in and remembers where they were.

  @oss @pro
  # source: oss:src/session.js:198
  # source: oss:src/app.jsx:182-189
  # source: pro:src/fleet.js:89
  # source: pro:src/app.jsx:111-119
  # source: pro:src/auth-redirect.js:12-16
  Scenario: A 401 from the server's own API sends the user to sign-in
    Given a signed-in user on a page with a query string
    And the server requires sign-in
    When a request to the server's own API answers 401
    Then the URL becomes "/login" without adding a history entry
    And the sign-in screen is shown
    And the page and query string the user was on are remembered for after sign-in

  @oss @pro
  # source: oss:src/app.jsx:184
  # source: pro:src/auth-redirect.js:13
  Scenario: A 401 is ignored when the server does not require sign-in
    Given the server does not require sign-in
    When a request answers 401
    Then the user stays where they are

  @oss @pro
  # source: oss:src/session.js:198,245,254,286,300
  # source: pro:src/fleet.js:89,109,140,147
  Scenario: The identity check, sign-in and sign-out never trigger the expiry redirect
    When the start-up identity check, a sign-in or a sign-out answers 401
    Then the answer is handled by its caller
    And no session-expired redirect happens

  @oss @pro
  # source: oss:src/session.js:169-181,196
  # source: pro:src/auth-redirect.js:14
  Scenario: Later 401s from requests already in flight keep the remembered destination
    Given a 401 already sent the user to sign-in from a deep link
    When further requests that were in flight also answer 401
    Then the remembered destination is still the deep link, not "/login"

  @oss
  # source: oss:src/app.jsx:185-187
  # source: oss:src/session.js:175-181
  Scenario: Expiry discards everything held for the ended session
    Given a signed-in user with cluster data loaded and host shells open
    When a 401 ends the session
    Then the identity, the anti-forgery token and the cached cluster data are discarded
    And the open host shell sessions are ended locally

  @pro @drop
  # decision: D2
  # source: pro:src/app.jsx:112-117
  # source: pro:src/fleet.js:20-22,30-37
  Scenario: Session expiry in the fleet dashboard keeps the held identity and token
    Given a signed-in user
    When a 401 ends the session
    Then the sign-in screen is shown
    And the ended session's identity and anti-forgery token are still held until the next sign-in or identity check

  @oss
  # source: oss:src/session.js:196,214-216
  Scenario: An answer that arrives after the session changed is not acted on
    Given a request to the server's own API is in flight
    When the session changes before its answer arrives
    Then the request reports that it was superseded
    And a 401 in that answer does not send the user to sign-in

  # ---- 401 from the Kubernetes proxy -----------------------------------------

  @diverged @oss @drop
  # decision: D1
  # decision: D7
  # source: oss:src/k8sutil.js:84
  # source: oss:src/session.js:169-174
  Scenario: A 401 on a Kubernetes call means the session ended
    Given a signed-in user and a server that requires sign-in
    When a call through the Kubernetes proxy answers 401
    Then the user is sent to sign-in exactly as for a 401 from the server's own API

  @diverged @pro
  # decision: D7
  # source: pro:src/fleet.js:125-136
  # source: pro:src/k8sproxy.js:69,97,129
  Scenario: A 401 on a proxied Kubernetes call is confirmed with the hub first
    Given a signed-in user and a hub that requires sign-in
    When a call through the per-cluster Kubernetes proxy answers 401
    Then GET "/api/v1/me" is asked whether the hub session itself expired
    And only a 401 from that request sends the user to sign-in

  @pro @fleet
  # decision: D7
  # source: pro:src/fleet.js:116-136
  # source: pro:src/k8sproxy.js:68-72,82-86,97-98,127-130
  Scenario: A member cluster's own 401 does not sign the user out
    Given a proxied Kubernetes call answered 401
    When the hub's identity request answers successfully
    Then the user stays signed in
    And the 401 is returned to that call as its own failure
    And a read that has a default falls back to it with a console warning

  @pro
  # decision: D7
  # source: pro:src/fleet.js:125,131-133
  Scenario: A hub that does not answer the confirmation in time does not sign the user out
    Given a proxied Kubernetes call answered 401
    When the hub's identity request has not answered within 5 seconds or fails on the network
    Then the user stays signed in

  @pro
  # decision: D7
  # source: pro:src/fleet.js:130-135
  Scenario: Simultaneous confirmations share one request
    Given several proxied Kubernetes calls answer 401 at the same time
    Then a single identity request is made to the hub and its answer serves them all

  @pro
  # decision: D7
  # source: pro:src/fleet.js:128-129
  Scenario: No confirmation is made when sign-in is not required
    Given the server does not require sign-in
    When a proxied Kubernetes call answers 401
    Then no identity request is made and the user stays where they are

  # ---- other tabs ----------------------------------------------------------

  @oss @drop
  # decision: D2
  # source: oss:src/session.js:67-82,344-350
  # source: oss:src/app.jsx:190-194
  Scenario: A sign-in or sign-out in another tab ends this tab's session view
    Given two tabs of the dashboard are open on a server that requires sign-in
    When one tab signs in or signs out
    Then the other tab discards its identity, anti-forgery token and cached cluster data
    And shows the sign-in screen, remembering the page it was on

  @oss @fixed
  # decision: D2
  # source: oss:src/session.js:67-82,344-350
  # source: oss:src/app.jsx:190-194
  Scenario: A session change in another tab makes this tab check its session again
    Given two tabs of the dashboard are open on a server that requires sign-in
    When one tab signs in or signs out
    Then the other tab discards its identity, anti-forgery token and cached cluster data
    And asks the server who is signed in
    And shows the dashboard for that user, or the sign-in screen when nobody is, remembering the page it was on

  @oss
  # source: oss:src/app.jsx:191
  Scenario: Another tab's session change is ignored when sign-in is not required
    Given the server does not require sign-in
    When another tab publishes a session change
    Then this tab stays on its page

  @oss
  # source: oss:src/session.js:67-82
  # source: oss:src/app.jsx:191
  Scenario: A marker change without required sign-in still empties cached cluster data
    Given the server does not require sign-in
    When another tab publishes a session change
    Then this tab's cached cluster data is emptied
    And answers to its requests already in flight are discarded as superseded

  @oss
  # source: oss:src/session.js:67-82,346-349
  Scenario: Clearing browser storage in another tab ends this tab's session view
    Given this tab last saw a shared session marker
    When site storage is cleared or the marker is removed in another tab
    Then this tab treats it as a session change and discards its identity, token and cached cluster data
    And where sign-in is required it shows the sign-in screen

  @oss @pro
  # source: oss:src/session.js:163-165,173
  # source: pro:src/fleet.js:61-63,89
  Scenario: A 401 before the shell is listening is returned to the caller only
    Given the app has not yet started listening for ended sessions, or has stopped
    When a request answers 401
    Then the failure is returned to its caller and nothing navigates
