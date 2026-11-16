Feature: Live refresh
  Cluster data refreshes itself while the header's live toggle is on. The
  toggle's label says how fresh the data is, and a manual refresh reports its
  outcome as a toast.

  # ---- the toggle ----------------------------------------------------------------------

  @oss @pro
  # source: oss:src/app.jsx:272,499
  # source: oss:src/ui/shell.jsx:333-339
  # source: pro:src/app.jsx:176,522
  # source: pro:src/ui/shell.jsx:267-272
  Scenario: Auto-refresh is on when the shell opens and can be paused
    Given the shell has just opened
    Then the live toggle is on
    When the user clicks it
    Then auto-refresh stops and the toggle reads "Paused — click to resume auto-refresh"
    When the user clicks it again
    Then auto-refresh resumes

  @oss @pro
  # source: oss:src/app.jsx:314-323
  # source: pro:src/app.jsx:270-299
  Scenario: Resuming waits a full interval before the next refresh
    Given auto-refresh was paused
    When the user resumes it
    Then the next automatic refresh happens 15 seconds later, not immediately

  @diverged @oss
  # source: oss:src/ui/shell.jsx:270-278
  # source: oss:src/app.jsx:273,285-290
  Scenario Outline: The open dashboard's live label only claims what happened
    Given auto-refresh is on
    And <state>
    Then the live toggle reads "<label>"

    Examples:
      | state                                                     | label                                                    |
      | no load has answered yet                                  | Live — waiting for the first refresh; click to pause      |
      | the last load answered completely N seconds ago           | Live — refreshed Ns ago; click to pause                   |
      | the last load kept old data for some lists                | Live — data stale (<names of those lists>); click to pause |

  @diverged @pro @drop
  # decision: D1
  # source: pro:src/ui/shell.jsx:215,269
  # source: pro:src/app.jsx:181
  Scenario: The fleet dashboard's live label always claims a refresh
    Given auto-refresh is on
    Then the live toggle reads "Live — refreshed Ns ago; click to pause"
    And before any load has answered the time is counted from when the shell opened

  @oss
  # source: oss:src/ui/shell.jsx:224-227,275-276
  Scenario: A long list of stale data is shortened in the label
    Given the last load kept old data for more than four lists
    Then the live toggle names the first four and ends with "+<remaining> more"

  @oss @pro @drop
  # decision: D2
  # source: oss:src/ui/shell.jsx:270
  # source: pro:src/ui/shell.jsx:215
  Scenario: The age in the label is only as fresh as the last redraw
    Given the label reads "refreshed Ns ago"
    When time passes without anything else on the page changing
    Then the number in the label does not count up by itself

  @oss @pro @fixed
  # decision: D2
  # source: oss:src/ui/shell.jsx:270
  # source: pro:src/ui/shell.jsx:215
  Scenario: The age in the label keeps counting
    Given the label reads "refreshed Ns ago"
    When time passes without anything else on the page changing
    Then the number in the label keeps up with the time since the last refresh

  # ---- automatic refresh -----------------------------------------------------------------

  @diverged @oss @drop
  # decision: D1
  # source: oss:src/app.jsx:308-323
  Scenario: The open dashboard refreshes 15 seconds after the previous load settles
    Given auto-refresh is on and the first load has settled
    Then each automatic refresh starts 15 seconds after the previous one finished, however long that one took
    And a refresh that fails keeps the data already shown and the cadence unchanged

  @diverged @pro
  # source: pro:src/app.jsx:270-299
  # source: pro:src/load-outcome.js:28-44
  # source: pro:src/poll.js:9-27
  Scenario: The fleet dashboard backs off while its cluster is unreachable
    Given auto-refresh is on
    Then while refreshes succeed each one starts 15 seconds after the previous one finished
    And after the n-th consecutive failed or unreachable refresh the wait is 15 seconds doubled n−1 times (15, 30, 60, 120), with up to 15 percent jitter either way, never below 15 seconds and never above 120 seconds
    And the first successful refresh returns the wait to 15 seconds

  @oss
  # source: oss:src/app.jsx:315
  Scenario: Automatic refresh does not start before the first load has settled
    Given the first data load is still in flight
    Then no automatic refresh is scheduled yet

  @pro
  # source: pro:src/app.jsx:270-273,297
  Scenario: Automatic refresh follows the cluster in the URL
    When the user moves to another cluster
    Then the refresh schedule of the previous cluster stops and a new one starts for the new cluster

  @pro
  # source: pro:src/app.jsx:286-295
  # source: pro:src/load-outcome.js:28,42-43
  Scenario: A refresh overtaken by a newer load changes nothing
    Given an automatic refresh was overtaken by a newer load of the same cluster
    Then it neither counts as a failure nor as a success
    And it changes neither the reconnecting banner nor the refreshed time

  # ---- manual refresh --------------------------------------------------------------------

  @diverged @oss
  # source: oss:src/app.jsx:390-396
  # source: oss:src/live.js:447-458
  Scenario Outline: The open dashboard reports a manual refresh precisely
    When the user asks for a refresh and <outcome>
    Then <toast>

    Examples:
      | outcome                                         | toast                                                                    |
      | every list answered                             | the toast reads "Refreshed"                                               |
      | some lists kept their old data                  | the toast reads "Refreshed — <those lists> kept last data"                |
      | the cluster did not answer at all               | the toast reads "Refresh failed — keeping last data"                      |
      | the refresh raised an error                     | the toast reads "Refresh failed — keeping last data"                      |
      | the session had ended or changed                | no toast is shown                                                         |

  @diverged @pro @drop
  # decision: D1
  # source: pro:src/app.jsx:366-383
  # source: pro:src/load-outcome.js:28-33
  Scenario Outline: The fleet dashboard reports a manual refresh as done or failed
    When the user asks for a refresh and <outcome>
    Then <toast>

    Examples:
      | outcome                                              | toast                                                 |
      | the load completed                                   | the toast reads "Refreshed"                           |
      | the cluster was unreachable or the load raised       | the toast reads "Refresh failed — keeping last data"  |
      | the user switched cluster before it finished         | no toast is shown                                     |
      | a newer load of the same cluster overtook it         | no toast is shown                                     |

  @pro
  # source: pro:src/app.jsx:372-377
  Scenario: A manual refresh overtaken by a cluster switch or a newer load stays silent
    When the user switched cluster before a manual refresh finished, or a newer load of the same cluster overtook it
    Then no toast is shown and neither the banner nor the refreshed time changes

  @oss
  # source: oss:src/app.jsx:392
  Scenario: A manual refresh also reloads the custom resource sections of the sidebar
    When the user asks for a refresh
    Then the list of custom resource definitions behind the sidebar is requested again

  @oss @pro
  # source: oss:src/app.jsx:285-290
  # source: oss:src/live.js:450-451
  # source: pro:src/app.jsx:252,288,382,453
  Scenario: Only a load that answered moves the refreshed time
    When a cluster data load fails, is unreachable or is superseded
    Then the "refreshed" time shown by the live toggle is not updated

  # ---- reconnecting banner (fleet dashboard) -------------------------------------------------

  @pro
  # source: pro:src/app.jsx:286-292,544-550
  Scenario: An unreachable cluster raises a reconnecting banner
    Given auto-refresh is on
    When a refresh fails or finds the cluster unreachable
    Then a banner announces "Reconnecting… the selected cluster may be unreachable, or the fleet manager's serving certificate may have changed — reload to retry."
    And the data already shown is kept
    And the banner disappears after the next successful refresh

  @pro
  # source: pro:src/app.jsx:548
  Scenario: The banner offers a page reload
    Given the reconnecting banner is shown
    When the user clicks "Reload"
    Then the whole page is reloaded

  @pro
  # source: pro:src/app.jsx:238-249
  Scenario: The banner appears at once when a cluster is down on opening it
    Given auto-refresh is on
    When the user opens or switches to a cluster whose first load fails or is unreachable
    Then the reconnecting banner is shown without waiting for the next automatic refresh

  @pro
  # source: pro:src/app.jsx:271-273
  Scenario: Pausing clears the banner
    Given the reconnecting banner is shown
    When the user pauses auto-refresh
    Then the banner disappears

  @pro
  # source: pro:src/app.jsx:379-381
  Scenario: A failed manual refresh while paused only toasts
    Given auto-refresh is paused
    When a manual refresh fails
    Then the failure toast is shown and no reconnecting banner is raised

  @pro
  # source: pro:src/app.jsx:504
  Scenario: The overview is told when its data is stale
    Given the reconnecting banner is shown
    Then the overview screen is marked as showing stale data together with the last refreshed time

  @pro @fleet
  # source: pro:src/app.jsx:205,270-299,544-550
  Scenario: The remembered cluster keeps refreshing, and can raise the reconnecting banner, on the fleet and operations pages
    Given auto-refresh is on and the fleet page or the operations page is shown
    Then the remembered cluster's data keeps refreshing automatically, backing off while it is unreachable
    And the reconnecting banner is shown on those pages too when a refresh fails

  @pro
  # source: pro:src/app.jsx:254-261
  Scenario: Pausing during a cluster's first load drops the hint and ignores that load's outcome
    Given a cluster's first data load is in flight
    When the user pauses auto-refresh
    Then the loading hint disappears at once and the load is not started again
    And when that load settles it neither moves the refreshed time nor raises the reconnecting banner
