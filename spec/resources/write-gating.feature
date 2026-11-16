Feature: Who is offered write controls on resource lists
  Create, Edit, Delete, Scale, Restart and write-type contextual actions are
  shown or hidden as a group depending on the session. Hiding is a courtesy:
  the cluster still decides every write.

  # ---- who sees them ---------------------------------------------------------

  @diverged @oss
  # decision: D14 — both variants stand: this one on the dashboard server backend
  # source: oss:src/session.js:144-161
  # source: oss:src/ui/resource-list.jsx:83-93,166
  Scenario: Any signed-in user is offered the write controls
    Given the dashboard requires sign-in
    And a user is signed in, whatever their permissions on the cluster
    When a kind's list is open
    Then Create, and in the detail panel Edit, Delete and the kind's Scale and Restart, are offered
    And write-type contextual actions are offered

  @diverged @pro @fleet
  # decision: D14 — both variants stand: this one on the hub backend
  # source: pro:src/fleet.js:48-58
  # source: pro:src/ui/resource-list.jsx:64-74,158
  Scenario: Only a user with the admin role is offered the write controls
    Given the dashboard requires sign-in
    And the signed-in user's roles include "admin"
    When a kind's list is open
    Then Create, and in the detail panel Edit, Delete and the kind's Scale and Restart, are offered
    And write-type contextual actions are offered

  @pro @fleet
  # decision: D14 — hub backend only
  # source: pro:src/fleet.js:48-50
  # source: pro:src/ui/resource-list.jsx:64-74,158,286-291
  Scenario Outline: A signed-in user without the admin role gets a read-only list
    Given the dashboard requires sign-in
    And the signed-in user has <roles>
    When a kind's list is open
    Then no Create button is shown
    And the detail panel offers no Edit, Delete, Scale or Restart
    And write-type contextual actions are hidden while read-type ones remain

    Examples:
      | roles                |
      | only the viewer role |
      | no roles at all      |

  @oss @pro
  # source: oss:src/session.js:152-156
  # source: pro:src/fleet.js:49-54
  Scenario: With sign-in required and nobody signed in, no write control is offered
    Given the dashboard requires sign-in
    And there is no signed-in identity
    Then no kind's list offers Create, Edit, Delete, Scale, Restart or write-type contextual actions

  @oss @pro
  # source: oss:src/session.js:153-156
  # source: pro:src/fleet.js:50-54
  Scenario: A standalone dashboard that needs no sign-in offers the write controls
    Given the dashboard runs standalone and reports that sign-in is not required
    When a kind's list is open
    Then the write controls are offered although nobody is signed in

  @oss
  # decision: D6 — a configuration failure is an error screen, so no list is reachable in that state
  # source: oss:src/session.js:147-151
  Scenario: Write controls stay hidden while the dashboard's configuration could not be read
    Given the dashboard's configuration failed to load
    Then no write control is offered on any list, signed in or not

  @oss
  # source: oss:src/session.js:145-146
  Scenario: Write controls are withdrawn while the session is being replaced
    Given the session is changing or has been taken over in another browser tab
    Then no write control is offered on any list until the session settles

  @oss @pro
  # source: oss:src/session.js:159-161
  # source: pro:src/fleet.js:57-59
  Scenario: Controls follow the session without a reload
    Given a kind's list is open
    When the signed-in identity changes
    Then the write controls appear or disappear to match the new identity

  # ---- the read-only experience ----------------------------------------------

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:87,93,293-298,307
  # source: pro:src/ui/resource-list.jsx:68,74,286-291,300
  Scenario: Without write controls everything that only reads still works
    Given the session is not offered write controls
    When the user opens a kind's list
    Then the list loads, refreshes, filters by namespace and search, and honours deep links
    And clicking a row opens the detail panel with the summary, labels and the YAML or JSON manifest
    And the panel's footer offers the read-type contextual actions and "Close" only

  # ---- read-only kinds -------------------------------------------------------

  @pro @fleet
  # decision: D15 — read-only kinds stay read-only on the hub
  # source: pro:src/resources.jsx:259-274
  # source: pro:src/ui/resource-list.jsx:68,158
  Scenario: A kind marked read-only offers no writes to anyone
    Given a kind the dashboard marks as read-only
    And the signed-in user has the admin role
    When the kind's list is open
    Then no Create button is shown
    And the detail panel offers no Edit, Delete, Scale or Restart

  @oss
  # decision: D15 — on the dashboard server the generic edit stays available for every kind
  # source: oss:src/resources.jsx:254-257
  # source: oss:src/ui/resource-list.jsx:87,166
  Scenario: Every kind offers Create, Edit and Delete to a session with write controls
    Given the session is offered write controls
    Then every kind's list offers Create
    And every kind's detail panel offers Edit and Delete

  # ---- the cluster still decides ---------------------------------------------

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:84-86,128
  # source: pro:src/ui/resource-list.jsx:65-67,115
  Scenario: A write the cluster forbids is reported, not prevented
    Given the session is offered write controls
    And the cluster does not permit the user's write on this object
    When the user performs the write
    Then a notice reads "Error: <the cluster's refusal message>"
    And the controls remain offered
