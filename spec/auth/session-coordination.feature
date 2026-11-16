Feature: Coordinating session changes across requests and tabs
  The open dashboard treats every sign-in and sign-out as a change of the one
  session cookie the browser shares between tabs. Changes run one at a time,
  and answers that belong to a previous session are never applied.

  @oss
  # source: oss:src/session.js:222-225,276-279
  Scenario: Sign-in and sign-out requests run one at a time within a tab
    Given a sign-in or sign-out request is in flight
    When another sign-in or sign-out is started in the same tab
    Then the second request is sent only after the first has settled

  @oss
  # source: oss:src/session.js:226-227,270
  Scenario: Session changes are serialized across tabs when the browser supports it
    Given the browser provides cross-tab locks
    When two tabs start a sign-in or sign-out at the same time
    Then the two cookie-changing requests do not overlap

  @oss
  # source: oss:src/session.js:226-227,270
  Scenario: Without cross-tab locks only the current tab is coordinated
    Given the browser does not provide cross-tab locks
    When a sign-in or sign-out is started
    Then it runs immediately relative to other tabs and in order within its own tab

  @oss
  # source: oss:src/session.js:228-229,269-274
  Scenario: Waiting for another tab's session change is bounded
    Given another tab holds the session lock
    When this tab's sign-in or sign-out has waited 15 seconds for it
    Then the attempt is abandoned and reported as a network failure
    And no cookie-changing request is sent by this attempt

  @oss
  # source: oss:src/session.js:234
  # source: oss:src/session.js:313-319
  Scenario: A queued sign-in that has been overtaken sends nothing
    Given a sign-in is waiting its turn
    When a newer sign-in or a sign-out is started before the turn comes
    Then the waiting sign-in sends no request and reports that it was superseded

  @oss
  # source: oss:src/session.js:83-96,235,258-266
  Scenario: Other tabs are fenced off while the cookie is being replaced
    Given a sign-in or sign-out request is in flight in one tab
    Then every tab treats answers to requests made with the outgoing cookie as superseded
    And every tab withholds the anti-forgery token and write controls until the change completes

  @oss
  # source: oss:src/session.js:41-45,89
  Scenario: Only a non-secret marker is shared between tabs
    When a session change is published to other tabs
    Then browser storage receives only a random generation value and an in-progress flag under "stratum-dashboard-session-generation"
    And neither the token, the cookie nor the identity is written to browser storage

  @oss @drop
  # decision: D2
  # source: oss:src/session.js:58-66,69-72,87-88,269-273
  # source: oss:src/screens/login.jsx:38
  Scenario: An unreadable session marker blocks sign-in and sign-out
    Given the shared session marker in browser storage is not valid
    When the user submits the sign-in form
    Then no sign-in request is sent
    And the screen shows "Cannot reach the dashboard server."

  @oss @fixed
  # decision: D2
  # source: oss:src/session.js:58-66,69-72,87-88,269-273
  Scenario: An unreadable session marker is replaced instead of blocking sign-in
    Given the shared session marker in browser storage is not valid
    When the user submits the sign-in form
    Then the marker is replaced with a valid one and the sign-in request is sent

  @oss
  # source: oss:src/session.js:262-265
  Scenario: A failed session change still releases the fence
    Given a sign-in or sign-out request failed or timed out
    Then the in-progress flag is cleared so tabs can make requests again
    And every tab still treats the session as changed

  @oss
  # source: oss:src/session.js:83-92,262-265
  Scenario: A session change whose completion cannot be published leaves every tab fenced
    Given a sign-in or sign-out request has settled
    When browser storage refuses the write that would clear the in-progress flag
    Then every tab keeps withholding the anti-forgery token and write controls and treats answers as superseded
    And this lasts until a later sign-in or sign-out publishes a new marker

  @oss
  # source: oss:src/session.js:320-327
  Scenario: Requests started while a sign-in was pending do not leak into the new session
    Given data requests were started while a sign-in was pending
    When the sign-in succeeds
    Then answers to those earlier requests are discarded as superseded
    And the cached cluster data is emptied before the new session loads its own

  @oss
  # source: oss:src/session.js:283-292
  Scenario: An identity answer from before a session change is discarded
    Given an identity request is in flight
    When a sign-in, sign-out or another tab's session change happens before it answers
    Then the answer does not set the signed-in identity

  @oss
  # source: oss:src/session.js:344-350
  Scenario: Queued notifications from an older change cannot undo a newer sign-in
    Given this tab completed a sign-in
    When a delayed notification about an earlier session change arrives
    Then the current persisted marker is consulted rather than the notification's value
    And the completed sign-in is kept
