Feature: Establishing the session at start-up
  After the configuration resolves, the dashboard decides whether a session
  exists before it shows anything but a splash line.

  @oss @pro
  # source: oss:src/app.jsx:167-176
  # source: oss:src/session.js:283-292
  # source: pro:src/app.jsx:96-104
  # source: pro:src/fleet.js:108-114
  Scenario: An existing session opens the dashboard directly
    Given the server requires sign-in
    And GET "/api/v1/me" answers with the signed-in user
    When the dashboard starts
    Then the shell is shown with that user in the user menu
    And no sign-in screen is shown

  @oss @pro
  # source: oss:src/app.jsx:167-172,218-221
  # source: pro:src/app.jsx:96-99,133-136
  Scenario: No session sends the visitor to sign-in, remembering the destination
    Given the server requires sign-in
    And GET "/api/v1/me" answers 401
    When the dashboard starts on a deep link
    Then the URL becomes "/login" without adding a history entry
    And the sign-in screen is shown alone, without the shell
    And the original path and query string are remembered for after sign-in

  @oss @pro @drop
  # decision: D2
  # source: oss:src/session.js:286-288
  # source: pro:src/fleet.js:109-110
  Scenario: Any failure of the identity request is treated as no session
    Given the server requires sign-in
    When GET "/api/v1/me" fails with a network error or any non-success status
    Then the sign-in screen is shown, not an error

  @oss @pro @fixed
  # decision: D2
  # source: oss:src/session.js:286-288
  # source: pro:src/fleet.js:109-110
  Scenario: A failed identity request at start-up is an error, not a signed-out state
    Given the server requires sign-in
    When GET "/api/v1/me" fails with a network error or a status other than 401
    Then an error is shown instead of the sign-in screen
    And only a 401 answer leads to the sign-in screen

  @oss @pro
  # source: oss:src/app.jsx:167,175-176
  # source: pro:src/app.jsx:96,102-104
  Scenario: A server that does not require sign-in opens the dashboard without a session check
    Given the server configuration says sign-in is not required
    When the dashboard starts
    Then no request is made to "/api/v1/me"
    And the shell is shown with no signed-in user

  @oss
  # source: oss:src/app.jsx:168-171
  # source: oss:src/session.js:287
  Scenario: A session change during the start-up check falls back to sign-in
    Given the server requires sign-in
    When the session is changed, here or in another tab, while the identity request is in flight
    Then the answer is discarded and the sign-in screen is shown

  @oss @pro
  # source: oss:src/app.jsx:224-226
  # source: pro:src/app.jsx:139-141
  Scenario: The sign-in page redirects away when there is nothing to sign in to
    Given the dashboard is open with a signed-in user, or sign-in is not required
    When the browser opens "/login"
    Then it is redirected to "/" without adding a history entry

  @oss @pro
  # source: oss:src/app.jsx:224-225
  # source: pro:src/app.jsx:139-140
  Scenario: The sign-in page is shown when sign-in is required and no user is known
    Given the server requires sign-in and no user is known
    When the browser opens "/login"
    Then the sign-in screen is shown

  @oss @pro
  # source: oss:src/session.js:283-292
  # source: pro:src/fleet.js:108-114
  Scenario: The identity answer supplies the anti-forgery token
    Given GET "/api/v1/me" answers with a user and a "csrfToken"
    When the dashboard starts
    Then later write requests carry that token

  @diverged @oss
  # decision: D4
  # source: oss:src/session.js:122-127
  Scenario: The open dashboard keeps the user's name, provider and groups
    Given the identity answer carries username, provider, groups and other fields
    When the identity is established
    Then only username, provider and groups are kept as the signed-in identity
    And the anti-forgery token is not part of the identity

  @diverged @pro
  # decision: D4
  # source: pro:src/fleet.js:32-37
  Scenario: The fleet dashboard keeps the user's name, provider and roles
    Given the identity answer carries username, provider, roles and other fields
    When the identity is established
    Then only username, provider and roles are kept as the signed-in identity
    And the anti-forgery token is not part of the identity
