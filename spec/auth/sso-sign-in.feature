Feature: Single sign-on and sign-in return targets
  The fleet dashboard can hand sign-in to an identity provider, and can send a
  signed-in user back to a page the hub serves outside the dashboard.

  @pro @fleet
  # source: pro:src/screens/login.jsx:30-31,91-98
  Scenario: The SSO button is offered only when the hub advertises it
    Given the server configuration lists "oidc" among its sign-in providers
    When the sign-in screen is shown
    Then a "Sign in with SSO" button is shown below the password form

  @pro @fleet
  # source: pro:src/screens/login.jsx:30-31,91
  Scenario: No SSO button without an OIDC provider
    Given the server configuration lists no "oidc" provider
    When the sign-in screen is shown
    Then no "Sign in with SSO" button is shown

  @pro @fleet
  # source: pro:src/screens/login.jsx:36
  # source: pro:src/auth-redirect.js:34-36
  Scenario: Starting SSO leaves the dashboard with a full page navigation
    Given the "Sign in with SSO" button is shown
    When the user clicks it
    Then the browser navigates as a top-level request to "/api/v1/auth/oidc/start"

  @pro @fleet
  # source: pro:src/screens/login.jsx:94
  Scenario: SSO cannot be started while a password sign-in is in flight
    Given a password sign-in is in flight
    Then the "Sign in with SSO" button is disabled

  @pro @fleet
  # source: pro:src/screens/login.jsx:19-26
  Scenario Outline: A failed SSO round trip is explained with a fixed message
    Given the hub redirected back to the sign-in page with "?error=<code>"
    When the sign-in screen is shown
    Then it shows "<message>"

    Examples:
      | code             | message                                                           |
      | oidc_failed      | SSO sign-in failed. Try again or contact your administrator.     |
      | oidc_unavailable | SSO is unavailable. Try again later or use your local account.   |

  @pro @fleet
  # source: pro:src/screens/login.jsx:19-26
  Scenario: Unknown error codes in the URL are never displayed
    Given the sign-in page URL carries "?error=" with any other value
    When the sign-in screen is shown
    Then no error is shown and none of the URL text appears in the form

  @pro @fleet
  # source: pro:src/auth-redirect.js:22-30
  # source: pro:src/screens/login.jsx:29,49-52
  Scenario: A sign-in link can ask to return to the CLI authorization page
    Given the sign-in page URL carries "?return=cli-authorize"
    When the user signs in with username and password successfully
    Then the browser performs a full page load of "/cli/authorize"
    And the dashboard shell is not opened

  @pro @fleet
  # source: pro:src/auth-redirect.js:34-36
  # source: pro:src/screens/login.jsx:29,36
  Scenario: SSO carries the return target by name
    Given the sign-in page URL carries "?return=cli-authorize"
    When the user clicks "Sign in with SSO"
    Then the browser navigates to "/api/v1/auth/oidc/start?return=cli-authorize"

  @pro @fleet
  # source: pro:src/screens/login.jsx:28,36
  # source: pro:src/auth-redirect.js:34-36
  Scenario: SSO sign-in does not carry the remembered in-app destination
    Given an unauthenticated visitor was redirected to "/login" from a deep link
    When the user clicks "Sign in with SSO"
    Then the request that starts SSO carries the return name, if any, and nothing about the deep link

  @pro @fleet
  # source: pro:src/auth-redirect.js:22-30
  Scenario: Unknown return targets are ignored
    Given the sign-in page URL carries "?return=" with a value other than "cli-authorize"
    When the user signs in successfully
    Then the browser follows the normal in-app return instead
    And no path taken from the URL text is ever navigated to

  @pro @fleet
  # source: pro:src/auth-redirect.js:26-30
  # source: pro:src/screens/login.jsx:44-48
  Scenario: A failed sign-in does not follow the return target
    Given the sign-in page URL carries "?return=cli-authorize"
    When the sign-in fails
    Then the sign-in screen stays shown with the failure message
