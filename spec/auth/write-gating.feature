Feature: Offering write controls
  The dashboard hides controls that change things when it already knows they
  cannot work. This is a convenience, not authorization: the server and the
  cluster still decide every request.

  @diverged @oss
  # decision: D14
  # source: oss:src/session.js:144-157
  # source: oss:src/app.jsx:445-446
  Scenario: Any signed-in identity is offered write controls
    Given a signed-in user
    Then write controls are offered regardless of the user's groups
    And a write the cluster refuses is reported with the cluster's error message

  @diverged @pro
  # decision: D14
  # source: pro:src/fleet.js:48-55
  Scenario: Only a signed-in admin is offered write controls
    Given a signed-in user whose roles include "admin"
    Then write controls are offered

  @pro
  # decision: D14
  # source: pro:src/fleet.js:48-49
  Scenario: A signed-in user without the admin role is not offered write controls
    Given a signed-in user whose roles do not include "admin", or who has no roles
    Then write controls are not offered

  @oss @pro @fixed
  # decision: D14
  # decision: D4
  # source: oss:src/session.js:144-157
  # source: pro:src/fleet.js:48-55
  Scenario: Write gating follows the backend
    Then on the dashboard server every signed-in identity is offered write controls
    And on the fleet hub only a signed-in user with the "admin" role is

  @oss @pro
  # source: oss:src/session.js:152-156
  # source: pro:src/fleet.js:50-54
  Scenario: The login-free standalone mode offers write controls
    Given the resolved configuration is standalone with sign-in not required
    And there is no signed-in identity
    Then write controls are offered

  @oss @pro
  # source: oss:src/session.js:152-156
  # source: pro:src/fleet.js:50-54
  Scenario: A missing identity where sign-in is required offers no write controls
    Given the server requires sign-in
    And there is no signed-in identity
    Then write controls are not offered

  @pro @fleet
  # source: pro:src/fleet.js:53-54
  Scenario: A fleet hub with sign-in disabled offers no write controls
    Given the resolved configuration is fleet mode with sign-in not required
    And there is no signed-in identity
    Then write controls are not offered

  @oss
  # decision: D6
  # source: oss:src/session.js:147-151
  Scenario: A configuration that could not be loaded offers no write controls
    Given the configuration resolved with a configuration error
    Then write controls are not offered

  @oss
  # source: oss:src/session.js:145-146
  Scenario: Write controls are withdrawn while the session is changing
    Given a sign-in or sign-out is in progress in any tab, or the shared session marker cannot be read
    Then write controls are not offered

  @oss @pro
  # source: oss:src/session.js:120-132,159-161
  # source: pro:src/fleet.js:30-42,57-59
  Scenario: Write controls follow the identity as soon as it changes
    Given a screen showing write controls
    When the signed-in identity is established, replaced or cleared
    Then the controls appear or disappear without any other interaction
