Feature: Uninstalling a catalog application
  Uninstalling deletes the application's HelmChart after a confirmation; the
  cluster's operator then removes the Helm release, and the catalog follows
  the removal until the HelmChart is gone.

  # source: pro:src/screens/catalog.jsx:928-945
  @pro
  Scenario Outline: Uninstall is offered for a release that is not already being removed
    Given an admin has the detail panel open for an application whose release state is <state>
    Then the panel's footer shows <control>

    Examples:
      | state      | control                                                        |
      | deployed   | an "Uninstall" button                                          |
      | failed     | an "Uninstall" button                                          |
      | installing | an "Uninstall" button                                          |
      | removing   | a disabled "Uninstalling… <time since deletion>" indicator     |
      | stuck      | a "Retry uninstall" button                                     |

  # source: pro:src/screens/catalog.jsx:357-359,621,625-645
  # source: pro:src/catalog-rules.js:77-88
  @pro
  Scenario: Uninstall asks for confirmation naming exactly what will be deleted
    When an admin presses "Uninstall"
    Then a dialog titled "Uninstall <application name>?" opens
    And it states that the HelmChart "<its namespace>/<its name>" will be deleted and that the operator then runs helm uninstall, removing the release from the HelmChart's target namespace
    And it repeats the application's notes when it has any
    And it offers "Cancel" and "Uninstall"

  # source: pro:src/screens/catalog.jsx:638-640
  @pro
  Scenario: Missing details in the confirmation read as dashes
    Given the HelmChart reports no name, no namespace or no target namespace
    Then the corresponding part of the confirmation reads "—"

  # source: pro:src/screens/catalog.jsx:626,629
  @pro
  Scenario: Cancelling the confirmation deletes nothing
    When the admin presses "Cancel" or dismisses the dialog
    Then the dialog closes and no request is sent

  # source: pro:src/screens/catalog.jsx:341-355,630-631
  # source: pro:src/catalog-rules.js:77-88
  @pro
  Scenario: Confirming deletes the HelmChart in its own namespace
    Given the application's labelled HelmChart "loki" lives in namespace "apps"
    When the admin confirms the uninstall
    Then the dialog closes
    And the HelmChart "loki" in namespace "apps" is deleted, not a HelmChart of the same name in "kube-system"
    And every install and uninstall button is disabled until the request completes

  # source: pro:src/screens/catalog.jsx:351-354
  # decision: D2 — the message names the HelmChart's namespace, not the namespace the release is removed from (NOTES (a) 3)
  @pro @drop
  Scenario: An accepted uninstall is announced and then tracked from the cluster
    When the cluster accepts the deletion
    Then a notification reads "Uninstalling <application name> — the operator is removing the release from <the HelmChart's namespace>"
    And the installed releases are re-read immediately
    And the application shows "Removing" until its HelmChart disappears, after which it is offered for install again

  # source: pro:src/screens/catalog.jsx:351-354,640
  # decision: D2
  @pro @fixed
  Scenario: An accepted uninstall names the namespace the release is removed from
    When the cluster accepts the deletion
    Then a notification reads "Uninstalling <application name> — the operator is removing the release from <the HelmChart's target namespace>", the same namespace the confirmation named
    And the installed releases are re-read immediately
    And the application shows "Removing" until its HelmChart disappears, after which it is offered for install again

  # source: pro:src/screens/catalog.jsx:351-354
  # source: pro:src/k8sproxy.js:111-133
  @pro
  Scenario: A refused or failed uninstall is reported
    When the deletion request fails
    Then a notification reads "Error: " followed by the cluster's error message, else the response text, else "HTTP <status>"
    And the application keeps its current status

  # source: pro:src/screens/catalog.jsx:343-347
  # source: pro:src/catalog-rules.js:79-80
  @pro
  Scenario: A HelmChart without a name or namespace is never deleted by guesswork
    Given the application's HelmChart reports no namespace or no name
    When the admin confirms the uninstall
    Then no request is sent
    And a notification reads "Error: cannot uninstall <application name> — its HelmChart reports no namespace"

  # source: pro:src/screens/catalog.jsx:934-936
  @pro
  Scenario: A removal in progress cannot be requested again
    Given the release has been removing for less than 120 seconds
    Then the footer shows only the disabled "Uninstalling… <time>" indicator, to admins and read-only users alike

  # source: pro:src/screens/catalog.jsx:937-945
  @pro
  Scenario: A stuck removal can be retried
    Given the release has been removing for 120 seconds or more
    When an admin presses "Retry uninstall" and confirms the dialog
    Then the same deletion request is sent again

  # source: pro:src/screens/catalog.jsx:223-232
  @pro @fleet
  Scenario: An open confirmation is discarded when the cluster changes
    Given the uninstall confirmation is open for a release on cluster A
    When the user switches to cluster B
    Then the dialog is closed without sending any request

  # source: pro:src/screens/catalog.jsx:341-342,625,928,940
  # decision: D14 — hub: admin only; see "Install and uninstall are offered according to the backend's write gating" in catalog-detail.feature
  @pro
  Scenario: A read-only user cannot uninstall
    Given the signed-in user does not have the admin role
    Then no "Uninstall" or "Retry uninstall" button and no confirmation dialog is ever shown

  # source: pro:src/screens/catalog.jsx:625
  @pro
  Scenario: The confirmation disappears if the session loses the admin role
    Given the uninstall confirmation is open
    When the signed-in identity stops being an admin
    Then the dialog is no longer shown
