Feature: Cordon, uncordon and drain
  A user allowed to write can stop and resume scheduling on a node from the
  node table or the node's detail pane. Neither dashboard evicts pods.

  # --------------------------------------------------------- who is offered

  # source: oss:src/screens/infra.jsx:130-140
  # source: oss:src/session.js:144-156
  # decision: D14
  @diverged @oss
  Scenario: Row actions are offered to any session that can write
    Given a signed-in user, or a server that requires no sign-in
    When the Nodes screen is shown
    Then every node row offers a cordon or uncordon button
    And the buttons are absent when the server requires sign-in and nobody is signed in
    And the buttons are absent when the server configuration could not be read

  # source: pro:src/screens/infra.jsx:379-389
  # source: pro:src/fleet.js:48-55
  # decision: D14
  @diverged @pro
  Scenario: Row actions are offered only to admins
    Given a user with the "admin" role, or a standalone server that requires no sign-in
    When the Nodes screen is shown
    Then every node row offers a cordon or uncordon button
    And the buttons are absent for a signed-in user without the "admin" role

  # source: oss:src/screens/infra.jsx:136-138,221-223
  # decision: D1, D17
  @diverged @oss
  Scenario: The cordon action is labelled Cordon
    Given a schedulable node and a user allowed to write
    Then the row button's tooltip reads "Cordon"
    And the detail pane's button reads "Cordon"

  # source: pro:src/screens/infra.jsx:385-387,527-529
  # decision: D1, D17
  @diverged @pro @drop
  Scenario: The cordon action is labelled as cordon and drain
    Given a schedulable node and an admin
    Then the row button's tooltip reads "Cordon / Drain"
    And the detail pane's button reads "Cordon & Drain"

  # source: oss:src/screens/infra.jsx:136-138
  # source: pro:src/screens/infra.jsx:385-387
  @oss @pro
  Scenario: A cordoned node offers Uncordon in its row
    Given a cordoned node and a user allowed to write
    Then the row button's tooltip reads "Uncordon"

  # source: oss:src/screens/infra.jsx:133,136
  # source: pro:src/screens/infra.jsx:382,385
  @oss @pro
  Scenario: Pressing a row action does not select the row
    When the user presses the cordon button in a node's row
    Then the node's detail pane does not open because of that press

  # ---------------------------------------------------------------- cordon

  # source: oss:src/app.jsx:442-448
  # source: pro:src/app.jsx:433-438
  @oss @pro
  Scenario: Cordoning marks the node unschedulable at once, without confirmation
    When the user presses the cordon action for node "worker-1"
    Then no confirmation is asked
    And the node "worker-1" is patched with a merge patch setting "spec.unschedulable" to true
    And a toast reads "worker-1 cordoned — no new pods will schedule"

  # source: oss:src/app.jsx:442-448
  # source: pro:src/app.jsx:433-438
  @oss @pro
  Scenario: Uncordoning marks the node schedulable again
    When the user presses Uncordon for node "worker-1"
    Then the node "worker-1" is patched with a merge patch setting "spec.unschedulable" to false
    And a toast reads "worker-1 uncordoned — schedulable again"

  # source: oss:src/app.jsx:445-447
  # source: pro:src/app.jsx:436-442
  @oss @pro
  Scenario: A successful cordon or uncordon reloads the cluster data
    When a cordon or uncordon succeeds
    Then the cluster data is reloaded immediately
    And the node's row and detail reflect the new scheduling state without a manual refresh

  # source: oss:src/app.jsx:445-447
  # source: pro:src/app.jsx:436-439
  @oss @pro
  Scenario: A rejected cordon shows the server's error and changes nothing
    When the cluster rejects the patch, for example because the user is forbidden
    Then a toast reads "Error: " followed by the server's message
    And the cluster data is not reloaded

  # source: oss:src/app.jsx:372,446
  # source: pro:src/app.jsx:342,438
  @oss @pro
  Scenario: The toast disappears on its own
    When a cordon toast is shown
    Then it disappears after about 2.8 seconds

  # source: oss:src/app.jsx:442-448
  # source: pro:src/app.jsx:433-456
  # decision: D17
  @oss @pro
  Scenario: Cordoning never evicts or deletes pods
    Given pods are running on node "worker-1"
    When the user presses the cordon action for "worker-1"
    Then the only change sent to the cluster is the node's "spec.unschedulable" field
    And the pods already on the node keep running

  # source: oss:src/app.jsx:447
  @oss
  Scenario: A failed reload after a successful cordon keeps the last data silently
    Given a cordon succeeded
    When the following reload fails
    Then the screen keeps the previous data and shows no further message

  # source: pro:src/app.jsx:437,444-446
  @pro @fleet
  Scenario: A cordon overtaken by a cluster switch reports nothing
    Given the user pressed cordon on a node of cluster A
    When the user switches to cluster B before the patch answers
    Then no toast is shown and cluster A's reload does not alter what cluster B shows

  # source: pro:src/app.jsx:447-454
  @pro
  Scenario: A reload after cordon that cannot reach the cluster raises the reconnecting banner
    Given a cordon succeeded and auto-refresh is on
    When the following reload cannot reach the cluster
    Then the reconnecting banner is shown
    And the banner is not raised when auto-refresh is paused
