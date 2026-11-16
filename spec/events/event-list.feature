Feature: Event list
  The Events screen shows the cluster's most recent Kubernetes events,
  filterable by type and by the selected namespace. The fleet dashboard can
  also collapse events that share a reason.

  # ------------------------------------------------------------------ rows

  # source: oss:src/screens/infra.jsx:348-366
  # source: pro:src/screens/infra.jsx:676-683,693-702
  @oss @pro
  Scenario: Each event is a row with type, reason, object, message, count and last seen
    Given the cluster has events
    When the Events screen is shown
    Then the table has the columns Type, Reason, Object, Message, Count and Last seen
    And there is one row per event
    And the event's namespace is not shown in any column

  # source: oss:src/live.js:347-352
  # source: pro:src/live.js:673-678
  @oss @pro
  Scenario: Events are ordered most recently seen first
    Given event A was last seen 5 minutes ago and event B 1 minute ago
    When the Events screen is shown
    Then event B is listed above event A

  # source: oss:src/live.js:351-352
  # source: pro:src/live.js:677-678
  @oss @pro
  Scenario: Events without a usable time are listed last
    Given an event that carries no parsable timestamp
    When the Events screen is shown
    Then it is listed after every dated event
    And it is among the first left out when more than 50 events exist

  # source: oss:src/live.js:349,351
  # source: pro:src/live.js:675,677
  @oss @pro
  Scenario Outline: An event's last-seen time falls back through three fields
    Given an event that carries <fields>
    When the Events screen is shown
    Then its Last seen is the age of its <used>

    Examples:
      | fields                                          | used               |
      | a last timestamp                                | last timestamp     |
      | no last timestamp but an event time             | event time         |
      | neither, only its creation timestamp            | creation timestamp |

  # source: oss:src/live.js:352
  # source: pro:src/live.js:678
  @oss @pro
  Scenario: Only the 50 most recently seen events of the whole cluster are kept
    Given the cluster has 200 events across all namespaces
    When the Events screen is shown with all namespaces selected
    Then 50 rows are shown, the 50 most recently seen

  # source: oss:src/live.js:352
  # source: oss:src/screens/infra.jsx:334
  # source: pro:src/live.js:678
  # source: pro:src/screens/infra.jsx:646
  # decision: D2
  @oss @pro @drop
  Scenario: The 50-event cap is applied before the namespace filter
    Given the 50 most recently seen events of the cluster are all in namespace "kube-system"
    And namespace "shop" has older events
    When the Events screen is shown with namespace "shop" selected
    Then the table is empty

  # source: oss:src/live.js:352
  # source: pro:src/live.js:678
  # decision: D2, D9
  @fixed @oss @pro
  Scenario: A selected namespace shows its own most recent events
    Given the 50 most recently seen events of the cluster are all in namespace "kube-system"
    And namespace "shop" has older events
    When the Events screen is shown with namespace "shop" selected
    Then the most recently seen events of "shop" are listed, up to 50

  # source: oss:src/live.js:350
  # source: oss:src/screens/infra.jsx:359
  # source: pro:src/live.js:676
  # source: pro:src/screens/infra.jsx:697
  @oss @pro
  Scenario: The object is shown as lower-cased kind and name
    Given an event about the Pod "web-7c9"
    When the Events screen is shown
    Then its Object reads "pod/web-7c9"

  # source: oss:src/screens/infra.jsx:361
  # source: oss:src/live.js:351
  # source: pro:src/screens/infra.jsx:699
  # source: pro:src/live.js:677
  @oss @pro
  Scenario: Count shows a multiplier only for repeated events
    Given one event that occurred 4 times and one that occurred once or reports no count
    When the Events screen is shown
    Then the first shows "×4" and the second shows "1"

  # source: oss:src/screens/infra.jsx:357
  # source: oss:src/live.js:350
  # source: pro:src/screens/infra.jsx:695
  # source: pro:src/live.js:676
  @oss @pro
  Scenario: Warning events are highlighted and a missing type is Normal
    Given a Warning event and an event that carries no type
    When the Events screen is shown
    Then the Warning event's type badge uses the warning colour
    And the event without a type is shown as "Normal" in gray

  # source: oss:src/screens/infra.jsx:334,357
  # source: pro:src/screens/infra.jsx:646,695
  @oss @pro
  Scenario: An event of another type appears only under All
    Given an event whose type is neither "Warning" nor "Normal"
    When the Events screen is shown
    Then it is listed under All with a gray badge showing its own type
    And it is listed under neither the Warning nor the Normal filter
    And the filters match an event's type without regard to letter case

  # source: oss:src/screens/infra.jsx:354-365
  # source: pro:src/screens/infra.jsx:684-703
  # assumption: no empty-state message is coded for this table in either root
  # decision: D2
  @oss @pro @drop
  Scenario: No matching events shows an empty table
    Given no event matches the current filters
    When the Events screen is shown
    Then the table has headers and no rows, with no explanatory message

  # source: oss:src/screens/infra.jsx:354-365
  # source: pro:src/screens/infra.jsx:684-703
  # decision: D2, D9
  @fixed @oss @pro
  Scenario: No events, no matches and an unreadable event list are told apart
    When no event matches the current filters
    Then the table area says there are no matching events
    And when the events cannot be read or are forbidden the screen says so instead, keeping any rows it already had

  # ----------------------------------------------------------------- header

  # source: oss:src/screens/infra.jsx:340
  # source: pro:src/screens/infra.jsx:667
  # decision: D2
  @oss @pro @drop
  Scenario: The subtitle counts all loaded warnings regardless of filters or age
    Given 7 of the loaded events are Warnings, 3 of them in namespace "shop" and some older than an hour
    When the Events screen is shown with namespace "shop" and the Normal filter selected
    Then the subtitle reads "7 warnings in the last hour"

  # source: oss:src/screens/infra.jsx:340
  # source: pro:src/screens/infra.jsx:667
  # decision: D2
  @fixed @oss @pro
  Scenario: The subtitle counts the warnings it claims to count
    Given 7 loaded events are Warnings, 4 of them last seen within the past hour
    When the Events screen is shown
    Then the subtitle reads "4 warnings in the last hour"

  # source: oss:src/app.jsx:456-457
  # source: oss:src/registry.js:34
  # source: oss:src/ui/shell.jsx:385-386
  # source: pro:src/app.jsx:467-468
  # source: pro:src/nav.js:8
  # source: pro:src/ui/shell.jsx:301-302
  @oss @pro
  Scenario: The navigation entry for Events carries the number of loaded warnings
    Given 7 of the loaded events are Warnings
    Then the Events navigation entry shows the count 7

  # ---------------------------------------------------------------- filters

  # source: oss:src/screens/infra.jsx:333-334,343-345
  # source: pro:src/screens/infra.jsx:644,646,670-672
  @oss @pro
  Scenario Outline: The type filter narrows the rows
    Given the loaded events include Warning and Normal events
    When the user selects the "<filter>" filter
    Then <rows> are listed
    And the selected filter button is shown as active

    Examples:
      | filter  | rows                |
      | All     | events of any type  |
      | Warning | only Warning events |
      | Normal  | only Normal events  |

  # source: oss:src/screens/infra.jsx:333
  # source: pro:src/screens/infra.jsx:644
  @oss @pro
  Scenario: The type filter starts on All and is not remembered
    Given the user selected the Warning filter
    When the user leaves the Events screen and returns
    Then the filter is back on All
    And the filter is not part of the address

  # source: oss:src/screens/infra.jsx:334
  # source: oss:src/app.jsx:262,484
  # source: oss:src/live.js:351
  # source: pro:src/screens/infra.jsx:646
  # source: pro:src/app.jsx:163,509
  # source: pro:src/live.js:677
  @oss @pro
  Scenario: The selected namespace narrows the rows
    Given events in namespaces "shop" and "kube-system"
    When namespace "shop" is selected
    Then only events whose own namespace, or else whose object's namespace, is "shop" are listed
    And with all namespaces selected every loaded event is listed

  # source: oss:src/live.js:351
  # source: oss:src/screens/infra.jsx:334
  # source: pro:src/live.js:677
  # source: pro:src/screens/infra.jsx:646
  @oss @pro
  Scenario: An event with no namespace appears only when all namespaces are selected
    Given an event about a cluster-scoped object that carries no namespace
    When a specific namespace is selected
    Then that event is not listed

  # ---------------------------------------------------------------- refresh

  # source: oss:src/app.jsx:314-323
  # source: pro:src/app.jsx:270-299
  # decision: D9
  @oss @pro
  Scenario: Events refresh with the cluster's periodic reload
    Given auto-refresh is on
    When the periodic reload completes
    Then the event list reflects the newly read events
    And the Events screen offers no refresh button of its own

  # source: oss:src/live.js:348
  # decision: D1, D9
  @diverged @oss
  Scenario: Events are replaced whenever the events read answers, even with none
    Given the Events screen shows 12 events
    When a reload reads zero events
    Then the table becomes empty
    And when a reload fails to read events the 12 events stay

  # source: pro:src/live.js:448,674
  # decision: D1, D9
  @diverged @pro @drop
  Scenario: Events are replaced only when the reload returns at least one event
    Given the Events screen shows 12 events
    When a reload reads zero events or fails to read events
    Then the 12 events stay on screen

  # ---------------------------------------------------------------- grouping

  # source: pro:src/screens/infra.jsx:645,673
  @pro
  Scenario: Events can be grouped by reason
    Given the Events screen is shown ungrouped
    When the user presses "Group by reason"
    Then the button is shown as active and the table shows one row per distinct reason
    And pressing it again returns to one row per event

  # source: pro:src/screens/infra.jsx:678-692
  @pro
  Scenario: Grouped rows replace the object and message with an object count
    When events are grouped by reason
    Then the columns are Type, Reason, Objects, Count and Last seen
    And there is no Message column

  # source: pro:src/screens/infra.jsx:650-661,689-690
  @pro
  Scenario: A group sums occurrences and counts distinct objects
    Given three "FailedScheduling" events: pod "a" in "shop" ×2, pod "b" in "shop" ×1 and pod "a" in "test" ×3
    When events are grouped by reason
    Then the "FailedScheduling" row shows "3 objects" and "×6"
    And a group that touches a single object reads "1 object"

  # source: pro:src/screens/infra.jsx:690,699
  @pro
  Scenario: A group with a single occurrence reads "×1"
    Given a reason that occurred exactly once
    When events are grouped by reason
    Then its Count reads "×1", although the same event ungrouped reads "1"

  # source: pro:src/screens/infra.jsx:653,656,687
  @pro
  Scenario: A group is a Warning if any of its events is a Warning
    Given a reason that occurs as both Normal and Warning events
    When events are grouped by reason
    Then that group's type is "Warning"

  # source: pro:src/screens/infra.jsx:657,660,691
  @pro
  Scenario: Groups show and are ordered by their most recent event
    Given reason "BackOff" was last seen 1 minute ago and reason "Pulled" 10 minutes ago
    When events are grouped by reason
    Then "BackOff" is listed above "Pulled"
    And each group's Last seen is that of its most recent event

  # source: pro:src/screens/infra.jsx:646,650-652
  @pro
  Scenario: Grouping applies to the filtered events
    Given the Warning filter and namespace "shop" are selected
    When events are grouped by reason
    Then the groups are built only from Warning events in "shop"

  # source: pro:src/screens/infra.jsx:645
  @pro
  Scenario: Grouping starts off and is not remembered
    Given the user grouped events by reason
    When the user leaves the Events screen and returns
    Then events are shown ungrouped
