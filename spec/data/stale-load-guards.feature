Feature: Guards against stale and overlapping loads
  A load takes seconds and several things can ask for one at once: the poll,
  the Refresh button, a cordon, a cluster switch, a sign-out. This feature
  records which load is allowed to change what the user sees.

  # --------------------------------------------------------- overlapping loads

  @diverged @oss
  # decision: D1, D9 — one read per kind at a time; a refresh requested during a read is served by a read that starts after it
  # source: oss:src/live.js:460-482
  Scenario: Only one load runs at a time
    Given a load is in flight
    When the user presses Refresh
    Then no second set of reads is issued while the first is still running
    And a new load starts as soon as the first one settles
    And the Refresh reports the outcome of that new load

  @oss
  # source: oss:src/live.js:463-481
  Scenario: Requests that arrive during a load share one follow-up load
    Given a load is in flight
    When the poll, the Refresh button and a cordon all ask for a load before it settles
    Then exactly one follow-up load runs after it
    And all three receive that follow-up load's outcome

  @oss
  # source: oss:src/live.js:463-465,475-478
  Scenario: A load requested after a write always starts after the write
    Given a load is in flight that began before a node was cordoned
    When the cordon succeeds and asks for a load
    Then the cordon does not reuse the load already in flight
    And the load it waits for reads the node after the cordon

  @oss
  # source: oss:src/live.js:475
  Scenario: A crashed load does not block the one queued behind it
    Given a load is in flight and another is queued behind it
    When the first load raises an unexpected error
    Then the queued load still runs

  @diverged @pro @drop
  # decision: D1 — the oss variant (one at a time) stands; the cluster-switch guards below still apply
  # source: pro:src/k8sclient.js:34-46
  # source: pro:src/live.js:326-333,457,466,481,754,842,846,855
  Scenario: Overlapping loads of the same cluster may run, and only the newest commits
    Given a poll's load of cluster "a" is in flight
    When the user presses Refresh and a second load of cluster "a" starts
    Then both loads issue their reads
    And only the second load may replace the snapshot
    And the first load ends as superseded and changes nothing the user sees

  # ------------------------------------------------------------ cluster switch

  @pro @fleet
  # source: pro:src/app.jsx:223-231
  # source: pro:src/data.js:63-67
  Scenario: Switching cluster empties the snapshot before the new cluster loads
    Given the dashboard shows nodes and pods of cluster "a"
    When the user switches to cluster "b"
    Then every per-cluster collection is emptied at once
    And no row of cluster "a" is shown under the name of cluster "b"
    And the list of clusters is kept

  @pro
  # source: pro:src/app.jsx:231
  Scenario: Reopening the same cluster does not empty a snapshot that already loaded
    Given cluster "a" loaded successfully earlier in this page
    When the shell is shown again for cluster "a" without a switch
    Then the snapshot is not emptied before the load

  @pro @fleet
  # source: pro:src/live.js:344,399,457,466,481,754,842,846,855
  # source: pro:src/k8sclient.js:29-46
  Scenario: A load overtaken by a cluster switch commits nothing
    Given a load of cluster "a" is in flight
    When the user switches to cluster "b" before it ends
    Then the load of cluster "a" replaces nothing in the snapshot
    And it ends as superseded
    And a console note says the stale fetch for "a" was discarded

  @pro @fleet
  # source: pro:src/k8sclient.js:14-19,34-46
  # source: pro:src/live.js:326-333
  Scenario: Switching away and back does not let the earlier load commit
    Given a load of cluster "a" is in flight
    When the user switches to cluster "b" and then back to cluster "a" before it ends
    Then the earlier load of cluster "a" still commits nothing
    And only the load started by the return to "a" may replace the snapshot

  @pro @fleet
  # source: pro:src/live.js:345-348
  # source: pro:src/k8sclient.js:54-56
  Scenario: Every read of one load targets the cluster the load began with
    Given a load of cluster "a" has issued its probe
    When the user switches to cluster "b"
    Then any read that load still issues is addressed to cluster "a", never "b"

  @pro @fleet
  # source: pro:src/app.jsx:416-417
  Scenario: Open logs and terminals are closed on a cluster switch
    Given pod logs and a terminal are open in the bottom drawer for cluster "a"
    When the user switches to cluster "b"
    Then the drawer is emptied and the terminal dialog is closed

  @pro @fleet
  # source: pro:src/app.jsx:436-446
  Scenario: A cordon that completes after a cluster switch reports nothing
    Given the user cordoned a node on cluster "a"
    When the user switches to cluster "b" before the cordon answers
    Then no cordon message is shown and no load is started because of it
    And the cordon itself was still sent to cluster "a"

  @pro @fleet
  # source: pro:src/app.jsx:221-261
  Scenario: The outcome of a switched-away first load does not touch the new cluster's indicators
    Given the first load of cluster "a" is in flight
    When the user switches to cluster "b" and the load of "a" then ends
    Then the "refreshed" time and the reconnecting banner are decided only by the load of cluster "b"

  # ------------------------------------------------------------ session change

  @oss
  # source: oss:src/live.js:109,150-154,177,185,379,467-468
  Scenario: A load overtaken by a sign-out or a new session writes nothing
    Given a load is in flight
    When the user signs out, or another browser tab replaces the session, before the load ends
    Then the load replaces nothing in the snapshot
    And it ends as superseded, with no "refreshed" time and no message

  @oss
  # source: oss:src/live.js:467-468
  Scenario: A load requested for a session that already ended does not start
    Given the session ended while a poll was waiting to fire
    When the poll asks for a load
    Then no read is issued

  @oss
  # decision: D2 — applies on both backends
  # source: oss:src/session.js:67-82,175-181
  # source: oss:src/data.js:43-51
  Scenario: Ending or replacing the session empties the snapshot
    Given the dashboard shows nodes and pods
    When the session ends, by sign-out, by a 401 or by another tab changing it
    Then every collection is emptied
    And the page no longer counts as having live data

  @pro @drop
  # decision: D2 — a leak across sessions (NOTES P11); the corrected behavior is "Ending or replacing the session empties the snapshot", applied on both backends
  # assumption: no session-revision guard exists in this root; a load in flight across a sign-out is not discarded by the loader, and the snapshot is not emptied on sign-out
  # source: pro:src/fleet.js:146-151
  # source: pro:src/data.js:63-67
  Scenario: Signing out does not empty the snapshot
    Given the dashboard shows nodes and pods
    When the user signs out
    Then the in-memory snapshot is left as it was until the page is reloaded or a cluster is switched
