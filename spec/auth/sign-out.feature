Feature: Sign out
  Signing out ends the server session and returns the browser to the sign-in
  screen.

  @oss @pro
  # source: oss:src/ui/shell.jsx:494,507
  # source: pro:src/ui/shell.jsx:373,396
  Scenario: Sign out is offered only when the server requires sign-in
    Given the server requires sign-in
    When the user opens the user menu
    Then a "Sign out" entry is shown

  @oss @pro
  # source: oss:src/ui/shell.jsx:494,507
  # source: pro:src/ui/shell.jsx:373,396
  Scenario: No sign out without required sign-in
    Given the server does not require sign-in
    When the user opens the user menu
    Then no "Sign out" entry is shown

  @oss @pro
  # source: oss:src/app.jsx:204-211
  # source: pro:src/app.jsx:126-130
  Scenario: Signing out returns to the sign-in screen with no remembered destination
    Given a signed-in user
    When the user chooses "Sign out"
    Then the URL becomes "/login" without adding a history entry
    And the sign-in screen is shown
    And a following sign-in lands on "/"

  @diverged @pro @drop
  # decision: D1
  # source: pro:src/fleet.js:146-151
  Scenario: Fleet sign-out posts with the token it already holds
    Given a signed-in user
    When the user chooses "Sign out"
    Then a POST to "/api/v1/logout" is sent with the anti-forgery header
    And the held anti-forgery token and identity are discarded

  @diverged @oss
  # source: oss:src/session.js:240-256,333-342
  Scenario: Open sign-out re-reads the anti-forgery token before posting
    Given a signed-in user in a browser that supports cross-tab locks, or no token is held locally
    When the user chooses "Sign out"
    Then GET "/api/v1/me" is read first for the current "csrfToken"
    And a POST to "/api/v1/logout" is sent with that token in the anti-forgery header
    And the local identity, token and cached cluster data are discarded

  @oss
  # source: oss:src/session.js:246-248
  Scenario: A session whose Kubernetes token was revoked can still sign out
    Given the identity request answers 401 but its body still carries a "csrfToken"
    When the user chooses "Sign out"
    Then the sign-out POST is sent with that token

  @oss @drop
  # decision: D2
  # source: oss:src/session.js:248-251,339-341
  # source: oss:src/app.jsx:207-210
  Scenario: Sign-out without an obtainable anti-forgery token still leaves the dashboard
    Given the identity request fails, or succeeds without a "csrfToken"
    When the user chooses "Sign out"
    Then no sign-out POST is sent
    And the local session is discarded and the sign-in screen is shown

  @oss @drop
  # decision: D2
  # source: oss:src/session.js:269-273,338-341
  # source: oss:src/app.jsx:207-210
  Scenario: Sign-out with an unreadable session marker sends nothing and still leaves the dashboard
    Given the shared session marker in browser storage is not valid
    When the user chooses "Sign out"
    Then neither the identity read nor the sign-out POST is sent
    And the local session is discarded and the sign-in screen is shown

  @oss @pro @drop
  # decision: D2
  # source: oss:src/session.js:254-256,339-341
  # source: oss:src/app.jsx:207-210
  # source: pro:src/fleet.js:146-151
  # source: pro:src/app.jsx:126-130
  Scenario: A sign-out the server did not confirm still shows the sign-in screen
    Given a signed-in user
    When the sign-out request fails or is refused
    Then the local identity and anti-forgery token are discarded anyway
    And the sign-in screen is shown without an error message

  @oss @pro @fixed
  # decision: D2
  # source: oss:src/session.js:248-256,339-341
  # source: oss:src/app.jsx:207-210
  # source: pro:src/fleet.js:146-151
  # source: pro:src/app.jsx:126-130
  Scenario: A sign-out the server did not confirm is reported
    Given a signed-in user
    When the sign-out cannot be sent, or the server fails or refuses it
    Then a message says that signing out failed
    And the dashboard does not present the session as ended

  @oss
  # source: oss:src/app.jsx:87-110
  Scenario: Open host shells are deleted before the session cookie is cleared
    Given a signed-in user with one or more node host shells open
    When the user chooses "Sign out"
    Then a delete is attempted for each open host shell while the session still stands
    And the sign-out request is sent only after those attempts settle

  @oss
  # source: oss:src/app.jsx:60,66-81
  Scenario: A host shell delete cannot hold sign-out hostage
    Given a host shell delete that never answers
    When the user chooses "Sign out"
    Then that delete is abandoned after 3 seconds
    And the failure is written to the browser console naming the node and pod
    And sign-out proceeds

  @oss
  # source: oss:src/app.jsx:71-75
  Scenario: A refused host shell delete is reported and does not stop sign-out
    Given a host shell delete that the cluster refuses with a status other than 404
    When the user chooses "Sign out"
    Then the failure is written to the browser console
    And sign-out proceeds
    And a 401 on that delete does not trigger the session-expired redirect

  @oss
  # source: oss:src/app.jsx:89-101
  Scenario: Sign-out waits briefly for host shells that are still being created
    Given a host shell whose creation has not finished
    When the user chooses "Sign out"
    Then sign-out waits up to 3 seconds for the creation to finish before deleting shells
    And if it still has not finished a console error says its job may need cleanup

  @oss
  # source: oss:src/app.jsx:97,106,109
  # source: oss:src/app.jsx:207-208
  Scenario: A sign-out overtaken by a new session leaves the new session alone
    Given the user chose "Sign out"
    When a sign-in or another session change happens before sign-out completes
    Then the browser is not sent to the sign-in screen by the earlier sign-out
    And the newer session keeps its cookie

  @oss
  # source: oss:src/session.js:333-342
  # source: oss:src/session.js:175-181
  Scenario: Signing out clears cached cluster data
    Given a signed-in user viewing cluster data
    When sign-out completes
    Then the cluster data held in the browser is emptied
