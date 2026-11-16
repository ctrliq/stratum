Feature: Overview node utilisation and recent events
  The middle row of the Overview: a short table of nodes with their CPU and
  memory, beside the most recent events.

  # ---------------------------------------------------------- Node Utilization

  @oss @pro
  # source: oss:src/screens/overview.jsx:89-100
  # source: pro:src/screens/overview.jsx:94-105
  Scenario: The node table shows the first six nodes
    Given the cluster has 9 nodes
    When the Overview is shown
    Then the "Node Utilization" table lists the first 6 nodes, in the order the cluster returned them
    And its columns are Node, Role, CPU, Memory and Pods

  @oss @pro
  # source: oss:src/screens/overview.jsx:109,112
  # source: pro:src/screens/overview.jsx:114,117
  Scenario Outline: A node row shows a short role and its pod count
    Given a node with the role "<role>" running 12 unfinished pods
    Then its row shows the role "<shown>" and the pod count 12

    Examples:
      | role          | shown   |
      | control-plane | control |
      | worker        | worker  |

  @oss @pro
  # source: oss:src/screens/overview.jsx:110-111
  # source: oss:src/ui/shell.jsx:176-188
  # source: pro:src/screens/overview.jsx:115-116
  # source: pro:src/ui/shell.jsx:170-182
  Scenario Outline: A ready node's CPU and memory are shown as a small bar with a percentage
    Given a ready node at <percent>% CPU
    Then its CPU cell shows <cell>

    Examples:
      | percent | cell                                   |
      | 42      | a bar in the success tone and "42%"    |
      | 71      | a bar in the warning tone and "71%"    |
      | 86      | a bar in the error tone and "86%"      |
      | 0       | "—"                                    |

  @diverged @oss @drop
  # decision: D1 — the pro variant stands, following the three-way readiness kept in the data domain
  # source: oss:src/screens/overview.jsx:110-111
  Scenario: A node that is not ready shows dashes instead of utilisation
    Given a node whose Ready condition is "False", "Unknown" or absent
    Then its CPU and Memory cells show "—"

  @diverged @pro
  # source: pro:src/screens/overview.jsx:115-116
  Scenario: Only a NotReady node shows dashes instead of utilisation
    Given a node whose status is "NotReady"
    Then its CPU and Memory cells show "—"
    And a node whose status is "Unknown" still shows its utilisation bars

  @diverged @oss @drop
  # decision: D1 — the tint is readiness only; cordon gets its own marker (see the @fixed scenario below)
  # source: oss:src/screens/overview.jsx:105
  # source: oss:src/ui/shell.jsx:90-92
  Scenario Outline: The node icon is tinted by readiness first, then cordon
    Given a node that is <state>
    Then its icon is tinted in the <tone> tone

    Examples:
      | state                   | tone    |
      | ready and schedulable   | success |
      | ready and cordoned      | warning |
      | not ready               | error   |
      | not ready and cordoned  | error   |

  @diverged @pro
  # source: pro:src/screens/overview.jsx:110
  Scenario Outline: The node icon is tinted by readiness only
    Given a node whose status is "<status>"
    Then its icon is tinted in the <tone> tone

    Examples:
      | status   | tone    |
      | Ready    | success |
      | NotReady | error   |
      | Unknown  | warning |

  @pro @drop
  # decision: D2 — a cordoned node is invisible on the Overview (NOTES)
  # source: pro:src/screens/overview.jsx:110
  # source: pro:src/live.js:531,550
  Scenario: A cordoned Ready node looks the same as any Ready node on the Overview
    Given a node that is Ready and cordoned
    Then its icon is tinted in the success tone
    And nothing in its Overview row says it is cordoned

  @pro @fixed
  # decision: D1, D2
  # source: pro:src/screens/overview.jsx:110
  # source: pro:src/live.js:531,550
  Scenario: A cordoned node carries its own marker beside the readiness tint
    Given a node that is Ready and cordoned
    Then its icon is tinted in the success tone
    And its row carries a separate marker saying it is cordoned

  @oss @pro
  # source: oss:src/screens/overview.jsx:103-104
  # source: oss:src/app.jsx:383,481
  # source: pro:src/screens/overview.jsx:108-109
  # source: pro:src/app.jsx:355,504
  Scenario: Clicking a node name opens that node on the Nodes screen
    Given the node "cp-1" is listed
    When the user clicks its name
    Then the Nodes screen opens with "cp-1" selected
    And the name's tooltip reads "Open cp-1 on the Nodes page"

  @oss @pro
  # source: oss:src/screens/overview.jsx:92
  # source: pro:src/screens/overview.jsx:97
  Scenario: "View all" opens the Nodes screen
    When the user presses "View all" on the node table
    Then the Nodes screen opens

  @oss @pro
  # source: oss:src/screens/overview.jsx:115-117
  # source: pro:src/screens/overview.jsx:120-122
  Scenario: With no node rows the table says so
    Given no node rows have loaded
    Then the node table shows the single line "No node data yet."

  # -------------------------------------------------------------- Recent Events

  @oss @pro
  # source: oss:src/screens/overview.jsx:128
  # source: oss:src/live.js:346-352
  # source: pro:src/screens/overview.jsx:133
  # source: pro:src/live.js:672-678
  Scenario: The events card shows the six most recently seen events
    Given the cluster has 40 events
    Then the "Recent Events" card lists the 6 most recently seen, newest first

  @oss @pro
  # source: oss:src/screens/overview.jsx:38,124
  # source: oss:src/live.js:352
  # source: pro:src/screens/overview.jsx:40,129
  # source: pro:src/live.js:678
  Scenario: The card's subtitle counts warnings among the events that were loaded
    Given 50 events were loaded, of which 7 are of type "Warning"
    Then the subtitle reads "7 warnings"
    And warnings older than the 50 most recent events are not counted

  @oss @pro
  # source: oss:src/screens/overview.jsx:129-140
  # source: pro:src/screens/overview.jsx:134-145
  Scenario: An event line shows its reason, repeat count, age and object
    Given a Warning event "BackOff" about "pod/web-1", seen 14 times, last seen 3 minutes ago
    Then its line shows "BackOff", "×14", "3m" and "pod/web-1"
    And it carries a warning icon

  @oss @pro
  # source: oss:src/screens/overview.jsx:135
  # source: pro:src/screens/overview.jsx:140
  Scenario: An event seen once shows no repeat count
    Given a Normal event "Scheduled" seen once
    Then its line shows no "×" count
    And it carries an informational icon

  @oss @pro
  # source: oss:src/screens/overview.jsx:125
  # source: pro:src/screens/overview.jsx:130
  Scenario: "All events" opens the Events screen
    When the user presses "All events" on the events card
    Then the Events screen opens

  @oss @pro
  # source: oss:src/screens/overview.jsx:142
  # source: pro:src/screens/overview.jsx:147
  Scenario: With no events the card says so
    Given no events were loaded
    Then the events card shows "No recent events."
    And its subtitle reads "0 warnings"

  @oss @pro
  # source: oss:src/app.jsx:375-382
  # source: pro:src/app.jsx:345-354
  Scenario: Links from the Overview keep the selected namespace
    Given the namespace "shop" is selected
    When the user follows "View all", "All events" or a node name from the Overview
    Then the screen that opens still has the namespace "shop" selected
