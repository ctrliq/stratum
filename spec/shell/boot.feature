Feature: Boot sequence
  From an empty page to a usable dashboard: configuration first, then the
  session, then (for a fleet) the cluster list, then the shell and its first
  data load.

  @oss @pro
  # source: oss:src/main.jsx:10
  # source: oss:src/app.jsx:139-145,213
  # source: pro:src/main.jsx:11
  # source: pro:src/app.jsx:73-79,132
  Scenario: Nothing but a splash line is shown until the configuration resolves
    When the page loads
    Then GET "/api/v1/config" is requested before any other dashboard request
    And only a single line of splash text is shown until it resolves

  @diverged @oss
  # decision: D4
  # source: oss:src/app.jsx:213
  Scenario: The open dashboard's splash names the cluster
    When the page is loading its configuration or session
    Then the splash reads "Connecting to the cluster…"

  @diverged @pro
  # decision: D4
  # source: pro:src/app.jsx:132
  Scenario: The fleet dashboard's splash names the fleet manager
    When the page is loading its configuration, session or cluster list
    Then the splash reads "Connecting to the fleet manager…"

  @oss @pro @fixed
  # decision: D4
  # source: oss:src/app.jsx:213
  # source: pro:src/app.jsx:132
  Scenario: The splash does not name a backend before the configuration says which one it is
    When the page is still loading its configuration
    Then the splash text names neither the cluster nor the fleet manager
    And once the configuration has resolved the splash names the backend it describes

  @oss
  # source: oss:src/app.jsx:156-179
  Scenario: The open dashboard boots configuration, then session, then shell
    Given the configuration resolved without error
    When the session step is satisfied or not required
    Then the shell is shown
    And the first cluster data load starts once the shell is shown

  @pro
  # source: pro:src/app.jsx:90-107
  Scenario: The fleet dashboard loads the cluster list before showing the shell
    Given the configuration resolved
    When the session step is satisfied or not required
    Then the cluster list is requested
    And the shell is shown only after that request settles

  @pro
  # source: pro:src/app.jsx:96-99,102
  Scenario: The cluster list is not requested while sign-in is pending
    Given the server requires sign-in and there is no session
    When the page loads
    Then the sign-in screen is shown without requesting the cluster list

  @pro @drop
  # decision: D13
  # source: pro:src/app.jsx:102
  # source: pro:src/live.js:291-310
  Scenario: A cluster list that cannot be loaded still opens the shell on one local cluster
    Given the cluster list request fails or returns no clusters
    And no clusters are known yet
    When boot completes
    Then the shell opens against a single stand-in cluster named "local"

  @pro @fixed
  # decision: D13
  # source: pro:src/app.jsx:102
  # source: pro:src/live.js:291-310
  Scenario: A cluster list that cannot be loaded opens the shell without a made-up cluster
    Given the fleet hub's cluster list request fails or returns no clusters
    When boot completes
    Then the shell opens and shows the fleet as empty or unreadable
    And no stand-in cluster is listed

  @oss
  # decision: D6
  # source: oss:src/app.jsx:166,217
  # source: oss:src/app.jsx:244-252
  Scenario: A configuration error stops boot with a visible message
    Given the configuration resolved with a configuration error
    When boot completes
    Then the page shows "The dashboard could not read its configuration, so it cannot show the cluster." followed by the error
    And neither the sign-in screen nor the shell is shown
    And no session check is made

  @diverged @oss @drop
  # decision: D1
  # source: oss:src/app.jsx:292-306,470,504-506
  Scenario: The open dashboard shows a loading hint during the first data load
    Given the shell has just been shown and no cluster data has loaded in this page yet
    When the first data load is in flight
    Then the content area shows "Loading…" in its corner
    And the hint disappears when the load settles, whether it succeeded or failed

  @diverged @pro
  # source: pro:src/app.jsx:217-261,477,527-529
  Scenario: The fleet dashboard names the cluster it is loading
    Given a cluster page was opened and no cluster data has loaded in this page yet
    When the data load for that cluster is in flight
    Then the content area shows "Loading <cluster id>…" in its corner
    And the hint is not shown on the fleet page or the operations page

  @oss @pro
  # source: oss:src/app.jsx:299-300
  # source: pro:src/app.jsx:232-235
  Scenario: A first data load that throws leaves an empty, working shell
    Given the first data load raises an error
    Then the error is written to the browser console
    And the shell stays usable with empty lists

  @oss @pro
  # source: oss:src/app.jsx:256,494-521
  # source: pro:src/app.jsx:155-157,517-554
  Scenario: Shell state survives navigation between screens
    Given a toast is showing and the bottom drawer is open
    When the user navigates to another screen of the same cluster
    Then the toast, the drawer and the loaded cluster data are kept

  @pro @fleet
  # source: pro:src/app.jsx:205,217-220
  Scenario: The fleet page keeps loading the remembered cluster in the background
    Given the browser opens "/clusters"
    Then the data of the remembered cluster is still loaded
    And no loading hint is shown
