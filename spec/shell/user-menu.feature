Feature: User menu and version display
  The header's last control names who is signed in, offers sign-out, and
  shows which dashboard and Kubernetes versions are running.

  # ---- who is signed in --------------------------------------------------------------

  @diverged @oss
  # source: oss:src/ui/shell.jsx:484-488,500-502
  Scenario: The open dashboard shortens a service account identity on the button
    Given the signed-in user is "system:serviceaccount:<namespace>:<name>"
    Then the user menu's button shows only "<name>"
    And the full identity is the button's tooltip and the menu's first line

  @diverged @pro @drop
  # decision: D1
  # source: pro:src/ui/shell.jsx:388-390
  Scenario: The fleet dashboard shows the user name in full on the button
    Given a signed-in user
    Then the user menu's button shows the whole user name

  @oss
  # source: oss:src/ui/shell.jsx:484-488
  Scenario Outline: Which part of an identity the open dashboard's button shows
    Given the signed-in user is "<identity>"
    Then the user menu's button shows "<shown>"

    Examples:
      | identity                        | shown   |
      | system:serviceaccount:ops:robot | robot   |
      | alice                           | alice   |
      | odd:name:                       | odd:name: |

  @diverged @oss
  # source: oss:src/ui/shell.jsx:494-495
  Scenario: Without sign-in the open dashboard names kubectl proxy as the user
    Given the server does not require sign-in
    Then the user menu shows "kubectl proxy" as the user

  @diverged @pro @drop
  # decision: D1
  # source: pro:src/ui/shell.jsx:373-376,393
  Scenario: Without sign-in the fleet dashboard names cluster-admin as the user
    Given the server does not require sign-in
    Then the user menu shows "cluster-admin" as the user with the role "cluster-admin"

  @oss @pro
  # source: oss:src/ui/shell.jsx:495
  # source: pro:src/ui/shell.jsx:374
  Scenario: A required sign-in with no known user shows a dash
    Given the server requires sign-in and no user is known
    Then the user menu shows "—" as the user

  @diverged @oss
  # decision: D4
  # source: oss:src/ui/shell.jsx:504-506
  Scenario: The open dashboard's menu shows the user and the identity provider, never a role
    When the user opens the user menu
    Then its first line reads "<user name> · <provider>", or just the user name when no provider is known
    And no role and no read-only marker is shown

  @diverged @pro
  # decision: D4
  # source: pro:src/ui/shell.jsx:375-376,392-395
  Scenario: The fleet dashboard's menu shows the user, roles and identity provider
    When the user opens the user menu
    Then its first line reads "<user name> · <roles, comma separated> · <provider>", leaving out whatever is unknown

  @pro
  # source: pro:src/ui/shell.jsx:372,383,394
  Scenario: A signed-in user who is not an admin is marked read-only
    Given the server requires sign-in
    And the signed-in user's roles do not include "admin"
    When the user opens the user menu
    Then a "Read-only" marker is shown beside the user

  @pro
  # source: pro:src/ui/shell.jsx:383
  Scenario: No read-only marker without required sign-in
    Given the server does not require sign-in
    When the user opens the user menu
    Then no "Read-only" marker is shown

  @oss @pro
  # source: oss:src/ui/shell.jsx:38-59,504,508
  # source: pro:src/ui/shell.jsx:26-47,392,397
  Scenario: The identity and version lines are information only
    When the user opens the user menu
    Then the identity line and the version line cannot be activated
    And the menu closes on a choice, an outside click or Escape

  # ---- versions ------------------------------------------------------------------------

  @diverged @oss
  # decision: D4
  # source: oss:src/ui/shell.jsx:497,508-510
  Scenario: The open dashboard's version line
    Given the configuration carries the server version
    When the user opens the user menu
    Then its last line reads "Stratum <server version>"

  @diverged @pro
  # decision: D4
  # source: pro:src/ui/shell.jsx:385,397-399
  Scenario: The fleet dashboard's version line
    Given the configuration carries the server version
    When the user opens the user menu
    Then its last line reads "Stratum PRO <server version>"

  @oss @pro
  # source: oss:src/ui/shell.jsx:496,509
  # source: oss:src/app.jsx:302
  # source: pro:src/ui/shell.jsx:384,398
  # source: pro:src/app.jsx:250
  Scenario: The version line adds the cluster's Kubernetes version once known
    Given the cluster's Kubernetes version is known from the cluster's identity or from the last data load
    When the user opens the user menu
    Then the version line ends with " · Kubernetes v<version>" with exactly one leading "v"

  @oss @pro
  # source: oss:src/ui/shell.jsx:496,509
  # source: pro:src/ui/shell.jsx:384,398
  Scenario: The version line leaves Kubernetes out until it is known
    Given no Kubernetes version is known yet
    When the user opens the user menu
    Then the version line shows only the dashboard's own version

  @oss @pro
  # source: oss:src/ui/shell.jsx:497,509
  # source: pro:src/ui/shell.jsx:385,398
  Scenario: A configuration without a version shows the product name alone
    Given the configuration carries no server version
    When the user opens the user menu
    Then the version line shows the product name with no version after it
