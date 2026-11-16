Feature: Polling cadence and refresh outcomes
  How often the cluster snapshot is reloaded, how the cadence reacts to
  failures, and what a finished load tells the user. The wording and placement
  of the live toggle and the banner belong to the shell domain; what triggers
  them is recorded here because it is the loader's contract.

  # ------------------------------------------------------------------ cadence

  @oss @pro
  # source: oss:src/app.jsx:308-323
  # source: pro:src/app.jsx:270-299
  # source: pro:src/poll.js:9
  Scenario: The next poll is scheduled 15 seconds after the previous load settles
    Given live refresh is on
    When a load takes 4 seconds and succeeds
    Then the next load starts 15 seconds after that load finished, not 15 seconds after it started

  @oss @pro
  # source: oss:src/app.jsx:314-323
  # source: pro:src/app.jsx:270-299
  Scenario: Pausing live refresh stops the polling
    Given live refresh is on
    When the user pauses live refresh
    Then no further load is scheduled
    And a load already in flight is allowed to finish

  @oss @pro
  # source: oss:src/app.jsx:314-323
  # source: pro:src/app.jsx:270-299
  Scenario: Resuming live refresh waits a full interval before the first poll
    Given live refresh is paused
    When the user resumes live refresh
    Then the first poll runs 15 seconds later

  @diverged @oss
  # source: oss:src/app.jsx:312-315
  Scenario: Polling does not start until the first load has finished
    Given the first load is still in flight
    Then no poll is scheduled behind it

  @diverged @pro @drop
  # decision: D1 — the oss variant stands: no poll is queued behind an unfinished first load
  # source: pro:src/app.jsx:216-261,270-299
  Scenario: The first poll is scheduled 15 seconds after a cluster is opened even if its first load has not finished
    Given a cluster was just opened and its first load is still in flight
    When 15 seconds have passed since the cluster was opened
    Then a poll starts alongside the unfinished first load

  @pro @fleet
  # source: pro:src/app.jsx:263-299
  Scenario: Polling restarts when the selected cluster changes
    Given live refresh is on for cluster "a" and its poll has backed off
    When the user switches to cluster "b"
    Then the poll for cluster "a" is cancelled
    And polling for cluster "b" starts at the healthy 15 second cadence

  @diverged @oss @drop
  # decision: D1 — the pro variant (jittered backoff) stands
  # source: oss:src/app.jsx:317-320
  Scenario: A failed poll is retried at the same 15 second cadence
    Given live refresh is on
    When a load fails entirely
    Then the next load still starts 15 seconds later

  @diverged @pro
  # decision: D1, D9 — applies on both backends
  # source: pro:src/app.jsx:277-296
  # source: pro:src/load-outcome.js:41-44
  # source: pro:src/poll.js:9-26
  Scenario Outline: Consecutive failed polls back off exponentially with jitter
    Given live refresh is on
    When <failures> polls in a row have found the cluster unreachable or have crashed
    Then the next poll is scheduled between <shortest> and <longest> milliseconds later

    Examples:
      | failures | shortest | longest |
      | 1        | 15000    | 17250   |
      | 2        | 25500    | 34500   |
      | 3        | 51000    | 69000   |
      | 4        | 102000   | 120000  |
      | 5        | 102000   | 120000  |
      | 9        | 102000   | 120000  |

  @pro
  # source: pro:src/load-outcome.js:41-44
  Scenario: One successful poll returns to the healthy cadence
    Given four polls in a row failed
    When the next poll succeeds
    Then the poll after it is scheduled exactly 15000 milliseconds later
    And the count of consecutive failures is back to zero

  @pro
  # source: pro:src/load-outcome.js:27-32,41-44
  # source: pro:src/app.jsx:282-295
  Scenario: A poll superseded by a newer load is neither a success nor a failure
    Given two polls in a row failed
    When the next poll is superseded by a manual refresh of the same cluster
    Then the failure count stays at two
    And the following poll is scheduled as after two failures
    And the superseded poll changes neither the "refreshed" time nor the connection banner

  # --------------------------------------------------- outcome classification

  @pro
  # source: pro:src/load-outcome.js:27-32
  Scenario Outline: Every load ends in exactly one verdict
    When a load ends with <outcome>
    Then the "refreshed" time is <stamp>
    And the connection state becomes <connection>

    Examples:
      | outcome                                          | stamp       | connection      |
      | a committed snapshot                             | updated     | connected       |
      | the cluster unreachable                          | not updated | reconnecting    |
      | an unexpected error                              | not updated | reconnecting    |
      | being superseded by a newer load or a switch     | not updated | left as it was  |

  @oss
  # source: oss:src/live.js:447-458
  # source: oss:src/app.jsx:285-290
  Scenario Outline: Every load ends in one refresh verdict
    When a load ends with <outcome>
    Then the "refreshed" time is <stamp>
    And the refresh message is <message>

    Examples:
      | outcome                                   | stamp       | message                                   |
      | every read answered                       | updated     | "Refreshed"                               |
      | some reads answered, EVENTS stale         | updated     | "Refreshed — EVENTS kept last data"       |
      | no read answered                          | not updated | "Refresh failed — keeping last data"      |
      | the session ended (a 401)                 | not updated | none                                      |
      | being superseded by a session change      | not updated | none                                      |

  @oss
  # source: oss:src/live.js:436-438
  # source: oss:src/live.js:452-455
  Scenario: A long list of stale slices is shortened in the refresh message
    Given the cluster has no workloads, so workload usage is not also named stale
    When a load answers but leaves six slices stale: NODES, FLEET, NODE_CONDITIONS, PODS, CLUSTER_INFO and CLUSTER
    Then the refresh message is "Refreshed — NODES, FLEET, NODE_CONDITIONS, PODS +2 more kept last data"

  @oss
  # source: oss:src/live.js:452-455
  Scenario: Up to four stale slices are all named
    When a load answers but leaves four slices stale: NODES, FLEET, PODS and EVENTS
    Then the refresh message is "Refreshed — NODES, FLEET, PODS, EVENTS kept last data"

  @oss
  # source: oss:src/app.jsx:285-290
  # source: oss:src/ui/shell.jsx:270-278
  Scenario: Stale slices from the latest load are reported until a later load clears them
    Given a poll left "EVENTS" stale
    Then the live indicator says the data is stale and names what is stale
    When the next poll answers every read
    Then the live indicator says how many seconds ago the data was refreshed

  @diverged @oss
  # source: oss:src/app.jsx:273
  # source: oss:src/ui/shell.jsx:275-278
  Scenario: No refresh is claimed before a load has answered
    Given no load has answered yet in this page
    Then the live indicator says it is waiting for the first refresh

  @diverged @pro @drop
  # decision: D1, D2 — the oss variant stands: no refresh is claimed before a load answered (NOTES P8)
  # source: pro:src/app.jsx:181
  # source: pro:src/ui/shell.jsx:215
  Scenario: The refreshed time is set when the page opens, before any load
    Given the page was just opened and no load has answered yet
    Then the live indicator already states a "refreshed" time, counted from when the page opened

  # ------------------------------------------------------------ manual refresh

  @oss
  # source: oss:src/app.jsx:372
  # source: oss:src/app.jsx:390-396
  Scenario: A manual refresh shows its verdict as a transient message
    When the user presses Refresh and the load answers every read
    Then the message "Refreshed" is shown for about 2.8 seconds

  @oss
  # source: oss:src/app.jsx:390-396
  Scenario: A manual refresh that crashes says the last data is kept
    When the user presses Refresh and the load raises an unexpected error
    Then the message "Refresh failed — keeping last data" is shown

  @oss
  # source: oss:src/app.jsx:393-394
  # source: oss:src/live.js:450
  Scenario: A manual refresh that finds the session ended shows no message
    When the user presses Refresh and a read answers 401
    Then no refresh message is shown, because the sign-in screen is the report

  @oss @drop
  # decision: D2, D7 — a refresh that did refresh must say so; a 401 is confirmed with the identity check before it counts as an ended session
  # source: oss:src/live.js:120-126,141-144,439,447-450
  # source: oss:src/app.jsx:183-184
  Scenario: A 401 on one read while sign-in is not required still suppresses the refreshed time and message
    Given the server does not require sign-in
    When one read of a load answers 401, a kubelet usage read included, and the other reads answer
    Then the data that answered is shown
    And no "refreshed" time is recorded and no refresh message is shown, on every load for as long as the 401 lasts

  @oss @fixed
  # decision: D2, D7
  # source: oss:src/live.js:120-126,141-144,439,447-450
  Scenario: A 401 that is not an ended session is reported like any other failed read
    Given a read answers 401 and the identity check does not confirm an ended session
    When the load finishes with the other reads answered
    Then the "refreshed" time is updated
    And the kind whose read answered 401 is reported as not refreshed

  @pro
  # source: pro:src/app.jsx:366-383
  Scenario: A successful manual refresh confirms and clears the banner
    When the user presses Refresh and the load commits
    Then the message "Refreshed" is shown
    And the "refreshed" time is updated
    And any reconnecting banner is removed

  @pro
  # source: pro:src/app.jsx:376-381
  Scenario: A failed manual refresh says the last data is kept
    Given live refresh is on
    When the user presses Refresh and the cluster is unreachable
    Then the message "Refresh failed — keeping last data" is shown
    And the reconnecting banner is shown

  @pro
  # source: pro:src/app.jsx:379-381
  Scenario: A failed manual refresh while paused raises no banner
    Given live refresh is paused
    When the user presses Refresh and the cluster is unreachable
    Then the message "Refresh failed — keeping last data" is shown
    And no reconnecting banner is shown, because no poll would ever clear it

  @pro
  # source: pro:src/app.jsx:439-454
  Scenario: The reload after a cordon updates the refreshed time and banner without a message
    Given the user cordoned a node and the cluster accepted the change
    When the load that follows commits
    Then the "refreshed" time is updated and any reconnecting banner is removed
    And no refresh message is shown
    And when that load finds the cluster unreachable, the banner is raised only while live refresh is on
    And a load that crashes is reported in the console

  @pro @fleet
  # source: pro:src/app.jsx:372-377
  Scenario: A manual refresh overtaken by a cluster switch reports nothing
    Given the user pressed Refresh on cluster "a"
    When the user switches to cluster "b" before the refresh ends
    Then the refresh shows no message and changes neither the banner nor the "refreshed" time

  @pro @fleet
  # source: pro:src/app.jsx:384
  Scenario: Refreshing the list of clusters updates the "refreshed" time
    When the user refreshes the clusters list
    Then the hub's cluster list is requested
    And the "refreshed" time is updated whether or not the request succeeded

  # ------------------------------------------------------- reconnecting banner

  @pro
  # source: pro:src/app.jsx:286-292,544-550
  Scenario: An unreachable cluster raises a reconnecting banner
    Given live refresh is on
    When a poll finds the cluster unreachable
    Then a banner says "Reconnecting… the selected cluster may be unreachable, or the fleet manager's serving certificate may have changed — reload to retry."
    And the banner offers a "Reload" button that reloads the page

  @pro
  # source: pro:src/app.jsx:286-292
  Scenario: The banner goes away on the next successful poll
    Given the reconnecting banner is shown
    When a poll commits
    Then the banner is removed

  @pro
  # source: pro:src/app.jsx:238-249
  Scenario: A cluster that is down when opened raises the banner immediately
    Given live refresh is on
    When the user opens a cluster and its first load finds it unreachable
    Then the reconnecting banner is shown without waiting for a poll

  @pro
  # source: pro:src/app.jsx:238-261
  Scenario: A cluster opened while paused raises no banner
    Given live refresh is paused
    When the user opens a cluster and its first load finds it unreachable
    Then no reconnecting banner is shown
    And when the user pauses during a first load, that load's outcome changes neither the banner nor the "refreshed" time

  @pro
  # source: pro:src/app.jsx:270-273
  Scenario: Pausing live refresh clears the banner
    Given the reconnecting banner is shown
    When the user pauses live refresh
    Then the banner is removed

  @oss
  # assumption: no connection banner, backoff or reachability probe exists in this root
  # source: oss:src/app.jsx:285-290,308-323
  # source: oss:src/live.js:436-439
  # source: oss:src/ui/shell.jsx:270-278
  Scenario: An unreachable cluster raises no banner on the open dashboard
    Given live refresh is on
    When every read of a poll fails
    Then the screens keep showing the last data
    And the live indicator says the data is stale
    And no banner is shown and the "refreshed" time is not updated
