Feature: Custom Resources in the sidebar
  Under the "Custom Resources" section the sidebar grows one sub-group per
  API group that has custom resource definitions, with one row per kind
  leading to that kind's instances.

  # ---- what is listed --------------------------------------------------------

  @oss @pro
  # source: oss:src/crd-nav.js:31-42
  # source: oss:src/ui/shell.jsx:396-400
  # source: pro:src/crd-nav.js:19-30
  # source: pro:src/ui/shell.jsx:307-311
  Scenario: Definitions are grouped by API group and sorted
    Given the cluster has definitions in the groups "zeta.io" and "alpha.io"
    Then under "Custom Resources", after the "Definitions" row, there is one sub-group per API group
    And the sub-groups are ordered by group name: "alpha.io" then "zeta.io"
    And inside a sub-group the rows are ordered by their label, ties broken by their route

  @oss @pro
  # source: oss:src/crd-nav.js:38
  # source: pro:src/crd-nav.js:26
  Scenario Outline: A row is labelled with the kind
    Given a definition whose kind name is <kind> and plural is "widgets"
    Then its sidebar row is labelled "<label>"

    Examples:
      | kind      | label   |
      | "Widget"  | Widget  |
      | missing   | widgets |

  @oss @pro
  # source: oss:src/crd-nav.js:34-36
  # source: pro:src/crd-nav.js:22-24
  Scenario: A definition without a group or a plural gets no row
    Given a definition that names no API group or no plural
    Then it has no sidebar row and creates no sub-group

  @oss @pro
  # source: oss:src/crd-nav.js:8-17,38
  # source: oss:src/app.jsx:375-382
  # source: pro:src/crd-nav.js:1-8,26
  # source: pro:src/app.jsx:345-354
  Scenario: A row opens the kind's instances at its first served version
    Given a sub-group row for a definition with several versions
    When the user clicks the row
    Then the instance list opens at the first served version, or the first listed when none is served
    And the selected namespace is carried along

  @pro
  # source: pro:src/ui/shell.jsx:328-333
  Scenario: A sub-group header counts its kinds
    Given an API group with 4 definitions
    Then its sub-group header shows the group name and "4"

  # ---- open and closed -------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/shell.jsx:430-449
  # source: pro:src/ui/shell.jsx:323-343
  Scenario: Sub-groups start closed
    Given the user has never opened or closed a sub-group
    And the page on screen is not an instance list
    Then every sub-group shows only its header

  @oss @pro
  # source: oss:src/ui/shell.jsx:431-432
  # source: oss:src/crd-nav.js:19-29
  # source: pro:src/ui/shell.jsx:324-325
  # source: pro:src/crd-nav.js:10-17
  Scenario: The sub-group holding the page on screen opens itself
    Given the user has never opened or closed the sub-group "example.io"
    When an instance list for a kind in "example.io" is on screen
    Then the "example.io" sub-group is open

  @oss @pro
  # source: oss:src/ui/shell.jsx:240-244,398-399,432-436
  # source: pro:src/ui/shell.jsx:209-213,309-310,325-329
  Scenario: The user's choice for a sub-group wins and is remembered
    When the user opens or closes a sub-group by its header
    Then that sub-group stays as the user left it, whatever page is on screen
    And the choice is still in effect after a reload in the same browser

  @oss @pro
  # source: oss:src/ui/shell.jsx:432
  # source: pro:src/ui/shell.jsx:325
  Scenario: A sub-group the user closed stays closed even when it holds the page on screen
    Given the user closed the sub-group "example.io"
    When an instance list for a kind in "example.io" is on screen
    Then the "example.io" sub-group stays closed and no row is visibly marked as current

  @oss @pro
  # source: oss:src/crd-nav.js:19-29
  # source: oss:src/ui/shell.jsx:443
  # source: oss:src/app.jsx:124-125
  # source: pro:src/crd-nav.js:10-17
  # source: pro:src/ui/shell.jsx:338
  # source: pro:src/app.jsx:66-67
  Scenario: A kind's row is current for every version of the kind
    Given the sidebar row for "widgets" in "example.io" links to version "v1"
    When the instance list for "example.io", version "v1beta1", "widgets" is on screen
    Then that row is marked as current
    And the "Definitions" row is not

  @oss
  # source: oss:src/ui/shell.jsx:268,396
  Scenario: The sub-groups are hidden while the sidebar is collapsed to icons
    Given the sidebar is collapsed to its icon rail
    Then only the "Definitions" icon is shown under Custom Resources and no sub-group appears

  # ---- loading, refresh, failure --------------------------------------------

  @diverged @oss @drop
  # decision: D1 — no reason to wait on an unrelated load
  # source: oss:src/app.jsx:325-338
  Scenario: The sub-groups load once the first cluster snapshot has loaded
    Given the dashboard has just opened
    Then no sub-group is shown until the first cluster snapshot load has finished
    And then the definitions are read and the sub-groups appear

  @diverged @pro
  # source: pro:src/app.jsx:301-313
  Scenario: The sub-groups load as soon as a cluster is known
    Given the dashboard has just opened on a cluster
    Then the definitions are read at once, without waiting for the cluster snapshot
    And the sub-groups appear when that read answers

  @oss @pro
  # source: oss:src/app.jsx:332-338
  # source: pro:src/app.jsx:303-313
  Scenario: The sub-groups refresh every minute while auto-refresh is on
    Given auto-refresh is on
    Then the definitions are re-read every 60 seconds and the sub-groups updated

  @oss @pro
  # source: oss:src/app.jsx:332-338
  # source: pro:src/app.jsx:303-313
  Scenario: Pausing auto-refresh stops the minute refresh
    When the user pauses auto-refresh
    Then the definitions are read once more and then not again until auto-refresh resumes
    And resuming reads them immediately and restarts the minute refresh

  @diverged @oss
  # source: oss:src/app.jsx:390-396
  Scenario: A manual refresh also re-reads the definitions
    When the user triggers the dashboard's Refresh action
    Then the definitions are re-read along with the cluster snapshot

  @diverged @pro @drop
  # decision: D1 — oss re-reads them on Refresh
  # source: pro:src/app.jsx:366-383
  Scenario: A manual refresh does not re-read the definitions
    When the user triggers the dashboard's Refresh action
    Then only the cluster snapshot is re-read
    And the sub-groups change at their next minute refresh

  @oss @pro
  # source: oss:src/app.jsx:328-331
  # source: pro:src/app.jsx:306-309
  Scenario: A failed read keeps the sub-groups already shown
    Given sub-groups are shown
    When a later read of the definitions fails
    Then the sub-groups stay as they were, with no warning

  @oss @pro
  # source: oss:src/app.jsx:277,328-331
  # source: pro:src/app.jsx:301,306-309
  Scenario: A user who may not list definitions sees no sub-groups
    Given the signed-in user is not allowed to list custom resource definitions
    Then "Custom Resources" shows only the "Definitions" row
    And nothing explains why no kinds are listed

  @oss @pro
  # source: oss:src/app.jsx:328-338
  # source: pro:src/app.jsx:303-313
  Scenario: A definition created or deleted from the dashboard reaches the sidebar later
    Given the user created or deleted a definition from the Definitions list
    Then the sidebar's sub-groups do not change until their next read

  @pro @fleet
  # source: pro:src/app.jsx:301-313
  Scenario: Switching cluster empties the sub-groups until the new cluster answers
    Given sub-groups are shown for one cluster
    When the address changes to another cluster
    Then the sub-groups disappear at once
    And reappear from the new cluster's definitions when they are read
