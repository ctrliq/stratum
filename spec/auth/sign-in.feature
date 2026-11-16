Feature: Sign in
  The sign-in screen is the only thing shown while a required session is
  missing. The open dashboard signs in with a Kubernetes service account
  token; the fleet dashboard signs in with a username and password.

  # ---- credentials ---------------------------------------------------------

  @diverged @oss
  # decision: D4
  # source: oss:src/screens/login.jsx:50-77
  Scenario: The sign-in form asks for a service account token
    Given the server requires sign-in and there is no session
    When the sign-in screen is shown
    Then it shows the heading text "Sign in with a Kubernetes token"
    And a single masked field labelled "Service account token" with autocomplete off
    And a hint explaining how to create a token with "kubectl create token <serviceaccount> -n <namespace>"
    And a "Sign in" button

  @diverged @pro
  # decision: D4
  # source: pro:src/screens/login.jsx:57-89
  Scenario: The sign-in form asks for a username and a password
    Given the server requires sign-in and there is no session
    When the sign-in screen is shown
    Then it shows the heading text "Sign in to your fleet manager"
    And a "Username" field and a masked "Password" field
    And a "Sign in" button

  @oss @pro @fixed
  # decision: D4
  # source: oss:src/screens/login.jsx:50-77
  # source: pro:src/screens/login.jsx:57-89
  Scenario: The sign-in form follows the backend the configuration describes
    Given the server requires sign-in and there is no session
    When the configuration lists "token" among its sign-in providers
    Then the token form is shown
    And for any other configuration the username and password form is shown

  @oss
  # source: oss:src/screens/login.jsx:31,74
  Scenario: A blank token cannot be submitted
    Given the sign-in screen is shown
    When the token field is empty or holds only whitespace
    Then the "Sign in" button is disabled
    And submitting the form sends no request

  @pro
  # source: pro:src/screens/login.jsx:40,86-87
  Scenario: A missing username or password cannot be submitted
    Given the sign-in screen is shown
    When the username or the password is empty
    Then the "Sign in" button is disabled
    And submitting the form sends no request

  @pro @drop
  # decision: D2
  # source: pro:src/screens/login.jsx:40,42,86-87
  Scenario: A whitespace-only username is submitted as an empty username
    Given the user typed only spaces as the username and a password
    Then the "Sign in" button is enabled
    When the user submits the form
    Then a POST to "/api/v1/login" carries an empty username

  @pro @fixed
  # decision: D2
  # source: pro:src/screens/login.jsx:40,42,86-87
  Scenario: A whitespace-only username cannot be submitted
    Given the user typed only spaces as the username and a password
    Then the "Sign in" button is disabled
    And submitting the form sends no request

  @oss
  # source: oss:src/screens/login.jsx:33
  # source: oss:src/session.js:313-318
  Scenario: The token is sent once, trimmed, and is not kept in the browser
    Given the user pasted a token with surrounding whitespace
    When the user submits the form
    Then a POST to "/api/v1/login" carries the token without the surrounding whitespace
    And the token is not written to browser storage

  @pro
  # source: pro:src/screens/login.jsx:42
  # source: pro:src/fleet.js:139-144
  Scenario: The username is sent trimmed and the password as typed
    Given the user typed a username with surrounding whitespace and a password
    When the user submits the form
    Then a POST to "/api/v1/login" carries the trimmed username and the password unchanged

  @oss @pro
  # source: oss:src/screens/login.jsx:31-34,59,63,74-75
  # source: pro:src/screens/login.jsx:40-43,68,72,79,86-88
  Scenario: The form is locked while a sign-in is in flight
    Given the user submitted the sign-in form
    When the server has not answered yet
    Then the button reads "Signing in…" and is disabled
    And the input fields are disabled
    And a second submission sends no request

  # ---- failures ------------------------------------------------------------

  @diverged @oss
  # decision: D4
  # source: oss:src/screens/login.jsx:36-39
  Scenario: A token the cluster rejects is reported
    Given the sign-in screen is shown
    When the server answers the sign-in with the error code "invalid_token"
    Then the screen shows "The cluster did not accept that token."
    And the sign-in screen stays shown

  @diverged @pro
  # decision: D4
  # source: pro:src/screens/login.jsx:44-47
  Scenario: A wrong username or password is reported
    Given the sign-in screen is shown
    When the server answers the sign-in with the error code "invalid_credentials"
    Then the screen shows "Invalid username or password."
    And the sign-in screen stays shown

  @diverged @oss
  # decision: D4
  # source: oss:src/screens/login.jsx:38
  # source: oss:src/session.js:193-195
  Scenario: An unreachable dashboard server is reported
    Given the sign-in screen is shown
    When the sign-in request fails without an HTTP response
    Then the screen shows "Cannot reach the dashboard server."

  @diverged @pro
  # decision: D4
  # source: pro:src/screens/login.jsx:46
  # source: pro:src/fleet.js:82-84
  Scenario: An unreachable fleet manager is reported
    Given the sign-in screen is shown
    When the sign-in request fails without an HTTP response
    Then the screen shows "Cannot reach the fleet manager."

  @oss @pro
  # source: oss:src/screens/login.jsx:38
  # source: oss:src/session.js:199-205
  # source: pro:src/screens/login.jsx:46
  # source: pro:src/fleet.js:90-96
  Scenario: Any other sign-in failure shows the server's own message
    Given the sign-in screen is shown
    When the server refuses the sign-in with another error
    Then the screen shows the message from the error response
    And when the response has no message it shows the error code, else the response text, else "HTTP <status>"

  @oss
  # source: oss:src/screens/login.jsx:36-44
  Scenario: A rejected token stays in the field
    Given the user submitted a token
    When the sign-in fails
    Then the token is still in the field so it can be corrected
    And the field is only cleared after a successful sign-in

  @pro
  # source: pro:src/screens/login.jsx:44-48
  Scenario: A failed sign-in keeps the typed username and password
    Given the user submitted a username and password
    When the sign-in fails
    Then both fields keep their values

  @oss @pro
  # source: oss:src/screens/login.jsx:32
  # source: pro:src/screens/login.jsx:41
  Scenario: Retrying clears the previous error
    Given the sign-in screen shows an error
    When the user submits the form again
    Then the error disappears while the new attempt is in flight

  @oss
  # source: oss:src/screens/login.jsx:35
  # source: oss:src/session.js:214-216,319
  Scenario: A sign-in overtaken by another session change ends silently
    Given the user submitted a token
    When another sign-in or sign-out starts, here or in another tab, before the answer is applied
    Then the screen shows no error and does not leave sign-in for this attempt

  @oss
  # source: oss:src/screens/login.jsx:41,46
  Scenario: A successful sign-in is not applied if the session changed meanwhile
    Given the server accepted the token
    When the session changes before the dashboard opens
    Then the screen does not navigate away on behalf of the earlier sign-in

  # ---- after signing in ----------------------------------------------------

  @oss @pro
  # source: oss:src/screens/login.jsx:27,47
  # source: oss:src/app.jsx:218-220
  # source: pro:src/screens/login.jsx:28,54
  # source: pro:src/app.jsx:133-135
  Scenario: Signing in returns to the page the user was heading for
    Given an unauthenticated visitor opened a deep link with a query string
    And was redirected to "/login"
    When the user signs in successfully
    Then the browser lands on the original path and query string
    And the sign-in page is replaced in the history rather than added to it

  @oss @pro
  # source: oss:src/screens/login.jsx:27,47
  # source: pro:src/screens/login.jsx:28,54
  Scenario: Signing in with nowhere to return to opens the root
    Given the user opened "/login" directly
    When the user signs in successfully
    Then the browser lands on "/" and follows the root redirect

  @oss @pro
  # source: oss:src/screens/login.jsx:47
  # source: pro:src/screens/login.jsx:54
  Scenario: A remembered location of the sign-in page itself is replaced by the root
    Given the remembered location is "/login"
    When the user signs in successfully
    Then the browser lands on "/"

  @oss
  # source: oss:src/app.jsx:198-203
  # source: oss:src/screens/login.jsx:45-47
  Scenario: The signed-in identity comes from the sign-in answer
    Given the server accepted the token and returned the user
    When the dashboard opens
    Then the user menu shows that user without a further identity request

  @pro
  # source: pro:src/app.jsx:121-125
  Scenario: The cluster list is loaded before the dashboard opens after sign-in
    Given the server accepted the credentials
    When the dashboard opens
    Then the cluster list has been requested and applied first

  @diverged @oss @drop
  # decision: D2
  # source: oss:src/app.jsx:198-203
  Scenario: A sign-in that yields no identity stays on the sign-in screen
    Given the server accepted the token but returned no user
    And the follow-up identity request also yields no user
    When the sign-in completes
    Then the sign-in screen stays shown with an empty token field and no error

  @diverged @pro @drop
  # decision: D1
  # source: pro:src/app.jsx:121-125
  # source: pro:src/ui/shell.jsx:373-374
  Scenario: A sign-in that yields no identity still opens the dashboard
    Given the server accepted the credentials but returned no user
    And the follow-up identity request also yields no user
    When the sign-in completes
    Then the dashboard opens and the user menu shows "—" as the user name

  @oss @fixed
  # decision: D2
  # source: oss:src/app.jsx:198-203
  # source: oss:src/screens/login.jsx:45-47
  Scenario: A sign-in that yields no identity is reported on the sign-in screen
    Given the server accepted the credentials but returned no user
    And the follow-up identity request also yields no user
    When the sign-in completes
    Then the sign-in screen stays shown with a message that the signed-in user could not be established

  @oss @pro
  # source: oss:src/screens/login.jsx:78
  # source: pro:src/screens/login.jsx:100
  Scenario: The sign-in screen shows the server version
    Given the server configuration carries a version
    When the sign-in screen is shown
    Then the version is shown under the form
    And nothing is shown there when the configuration has no version
