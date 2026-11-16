Feature: Node list
  The Nodes screen lists every node of the cluster being viewed with its
  role, readiness, usage, pod count, version and age, and lets the user open
  a node's detail.

  # ---------------------------------------------------------------- summary

  # source: oss:src/screens/infra.jsx:82-98
  # source: oss:src/live.js:207-224
  # decision: D1
  @diverged @oss @drop
  Scenario: The summary line counts nodes whose Ready condition is True
    Given a cluster with 3 nodes, 2 of which report the Ready condition as "True"
    And 1 of the nodes is cordoned
    When the Nodes screen is shown
    Then the summary reads "2 of 3 ready · 1 cordoned"
    And a cordoned node that is Ready is counted as ready

  # source: pro:src/screens/infra.jsx:312-315,343-347
  # source: pro:src/live.js:509-532
  # decision: D1
  @diverged @pro
  Scenario: The summary line counts nodes the hub or the Ready condition states as ready
    Given a cluster with 3 nodes
    And the hub's node facts state 2 of them ready
    And 1 of the nodes is cordoned
    When the Nodes screen is shown
    Then the summary reads "2 of 3 ready · 1 cordoned"
    And a node whose readiness is unknown is not counted as ready

  # source: oss:src/screens/infra.jsx:83,97
  # source: pro:src/screens/infra.jsx:313-314,346
  @oss @pro
  Scenario: The summary line reports version drift only when some node drifts
    Given 2 nodes whose stamped Kubernetes version differs from their kubelet version
    When the Nodes screen is shown
    Then the summary ends with "· 2 version drift" in the warning colour

  # source: oss:src/screens/infra.jsx:83,97
  # source: pro:src/screens/infra.jsx:313-314,346
  @oss @pro
  Scenario: The summary line omits version drift when no node drifts
    Given no node's stamped Kubernetes version differs from its kubelet version
    When the Nodes screen is shown
    Then the summary shows no version drift segment

  # ------------------------------------------------------------------- rows

  # source: oss:src/screens/infra.jsx:104-145
  # source: pro:src/screens/infra.jsx:352-395
  @oss @pro
  Scenario: The table shows one row per node with fixed columns
    When the Nodes screen is shown
    Then the table has the columns Node, Role, Status, Zone, CPU, Memory, Pods, Version, Age and an unlabelled actions column
    And there is one row per node in the order the cluster returned them
    And the list is not filtered by the selected namespace

  # source: oss:src/screens/infra.jsx:104-145
  # source: pro:src/screens/infra.jsx:352-395
  # assumption: neither root renders an empty-state message for the node table; none is coded
  # decision: D2
  @oss @pro @drop
  Scenario: A cluster with no nodes shows an empty table
    Given the cluster reports no nodes
    When the Nodes screen is shown
    Then the table has no rows
    And the summary reads "0 of 0 ready · 0 cordoned"

  # source: oss:src/screens/infra.jsx:104-145
  # source: pro:src/screens/infra.jsx:352-395
  # decision: D2, D9
  @fixed @oss @pro
  Scenario: An empty node list and an unreadable node list are told apart
    When the cluster answers with no nodes
    Then the table area says there are no nodes
    And when the node list cannot be read or is forbidden the screen says so instead, keeping any rows it already had

  # source: oss:src/live.js:216,219
  # decision: D1
  @diverged @oss
  Scenario: A node is a control-plane node when it carries any of three labels
    Given a node that has the label "node-role.kubernetes.io/control-plane", or the label "node-role.kubernetes.io/master", or the label "stratum.ciq.com/control-plane" with the value "true"
    When the Nodes screen is shown
    Then the node's Role is "control-plane"
    And every other node's Role is "worker"

  # source: pro:src/live.js:527,530
  # source: pro:src/k8sident.js:15,22
  # decision: D1
  @diverged @pro @drop
  Scenario: A node is a control-plane node when it carries either of two labels
    Given a node that has the label "node-role.kubernetes.io/control-plane" or the label "stratum.ciq.com/control-plane" with any value
    When the Nodes screen is shown
    Then the node's Role is "control-plane"
    And a node carrying only "node-role.kubernetes.io/master" has the Role "worker"

  # source: oss:src/live.js:210,223-224
  # source: oss:src/ui/shell.jsx:72-96
  # source: oss:src/screens/infra.jsx:120
  # decision: D2
  @diverged @oss @drop
  Scenario Outline: Node status combines readiness and cordon into one verdict
    Given a node whose Ready condition is "<ready>" and which is <schedulable>
    When the Nodes screen is shown
    Then the node's Status reads "<status>"

    Examples:
      | ready   | schedulable   | status   |
      | True    | schedulable   | Ready    |
      | True    | cordoned      | Cordoned |
      | False   | schedulable   | NotReady |
      | False   | cordoned      | NotReady |
      | Unknown | schedulable   | NotReady |
      | absent  | schedulable   | NotReady |

  # source: pro:src/live.js:509-532
  # source: pro:src/ui/shell.jsx:62-81
  # source: pro:src/screens/infra.jsx:369
  # decision: D2
  @diverged @pro @drop
  Scenario Outline: Node status states readiness only and keeps unknown distinct
    Given no hub node fact is available and a node's Ready condition is "<ready>"
    When the Nodes screen is shown
    Then the node's Status reads "<status>"
    And being cordoned does not change the Status

    Examples:
      | ready   | status   |
      | True    | Ready    |
      | False   | NotReady |
      | Unknown | Unknown  |
      | absent  | Unknown  |

  # source: oss:src/live.js:210,223-224
  # source: pro:src/live.js:509-532
  # decision: D2
  @fixed @oss @pro
  Scenario Outline: Node status keeps unknown readiness distinct and still shows a cordon
    Given a node whose readiness is <readiness> and which is <schedulable>
    When the Nodes screen is shown
    Then the node's Status reads "<status>"

    Examples:
      | readiness | schedulable | status   |
      | ready     | schedulable | Ready    |
      | ready     | cordoned    | Cordoned |
      | not ready | schedulable | NotReady |
      | not ready | cordoned    | NotReady |
      | unknown   | schedulable | Unknown  |
      | unknown   | cordoned    | Unknown  |

  # source: pro:src/live.js:509-521,531
  @pro @fleet
  Scenario: The hub's node fact decides the status when the hub answers
    Given the hub serves node facts for the cluster
    And the fact for a node states its Ready condition as true
    When the Nodes screen is shown
    Then the node's Status reads "Ready" whatever the node object's own conditions say
    And a node for which the hub states no Ready condition reads "Unknown"

  # source: oss:src/ui/shell.jsx:90-92
  # source: oss:src/screens/infra.jsx:115
  # decision: D2
  @diverged @oss @drop
  Scenario: The node icon is red when not ready, amber when cordoned, green otherwise
    When the Nodes screen is shown
    Then a node that is not ready has an error-coloured icon
    And a ready, cordoned node has a warning-coloured icon
    And a ready, schedulable node has a success-coloured icon

  # source: pro:src/screens/infra.jsx:364
  # decision: D2
  @diverged @pro @drop
  Scenario: The node icon is green when Ready, red when NotReady, amber when unknown
    When the Nodes screen is shown
    Then a node whose status is "Ready" has a success-coloured icon even when cordoned
    And a node whose status is "NotReady" has an error-coloured icon
    And a node whose status is "Unknown" has a warning-coloured icon

  # source: oss:src/ui/shell.jsx:90-92
  # source: pro:src/screens/infra.jsx:364
  # decision: D2
  @fixed @oss @pro
  Scenario: The node icon colour follows the status shown beside it
    When the Nodes screen is shown
    Then a node whose Status is "Ready" has a success-coloured icon
    And a node whose Status is "Cordoned" has a warning-coloured icon
    And a node whose Status is "NotReady" has an error-coloured icon
    And a node whose Status is "Unknown" has a neutral-coloured icon

  # source: oss:src/live.js:233,235
  # source: pro:src/live.js:549,551
  @oss @pro
  Scenario: Zone and instance type come from the well-known node labels
    Given a node labelled "topology.kubernetes.io/zone=eu-1a"
    When the Nodes screen is shown
    Then the node's Zone reads "eu-1a"
    And a node without that label shows "—"
    And the instance type is read from "node.kubernetes.io/instance-type", falling back to "beta.kubernetes.io/instance-type", else "—"

  # ------------------------------------------------------------------ usage

  # source: oss:src/live.js:182-200,212-215,225-226
  # source: pro:src/live.js:477-495,523-526,533-534
  @oss @pro
  Scenario: CPU and memory show measured usage as a share of allocatable
    Given the node's kubelet stats summary reports 500 millicores and 2048 MiB in use
    And the node's allocatable is 2 CPUs and 8192 MiB
    When the Nodes screen is shown
    Then the node's CPU meter shows 25%
    And the node's Memory meter shows 25%

  # source: oss:src/live.js:98-102,211-215,225-226
  # source: pro:src/live.js:522-526,533-534
  @oss @pro
  Scenario: Usage falls back to the sum of pod requests when the kubelet summary is unavailable
    Given the kubelet stats summary of a node cannot be read
    And the non-terminated pods on the node request 1 CPU and 4096 MiB in total
    And the node's allocatable is 2 CPUs and 8192 MiB
    When the Nodes screen is shown
    Then the node's CPU meter shows 50%
    And the node's Memory meter shows 50%

  # source: oss:src/live.js:225-226
  # source: oss:src/ui/shell.jsx:176-178
  # source: pro:src/live.js:533-534
  # source: pro:src/ui/shell.jsx:170-172
  @oss @pro
  Scenario: Usage percentages are rounded and capped at 100, and are 0 without allocatable
    Given a node whose usage exceeds its allocatable
    When the Nodes screen is shown
    Then its meters show 100%
    And a node that reports no allocatable CPU or memory has 0 for that resource, which the list shows as "—" and the detail pane as "0%"

  # source: oss:src/ui/shell.jsx:176-178
  # source: oss:src/screens/infra.jsx:122-123
  # source: pro:src/ui/shell.jsx:170-172
  # source: pro:src/screens/infra.jsx:371-372
  @oss @pro
  Scenario: A node at 0% usage shows a dash instead of a meter in the list
    Given a ready node whose CPU usage rounds to 0%
    When the Nodes screen is shown
    Then its CPU cell shows "—", exactly as a node that is not ready does
    And its detail pane shows "0%" for the same resource

  # source: oss:src/screens/infra.jsx:122-123
  # decision: D1
  @diverged @oss
  Scenario: Usage meters are replaced by a dash for any node that is not ready
    Given a node whose Ready condition is not "True"
    When the Nodes screen is shown
    Then its CPU and Memory cells show "—" instead of a meter

  # source: pro:src/screens/infra.jsx:371-372
  # decision: D1
  @diverged @pro @drop
  Scenario: Usage meters are replaced by a dash only for a NotReady node
    Given one node with the status "NotReady" and one with the status "Unknown"
    When the Nodes screen is shown
    Then the NotReady node's CPU and Memory cells show "—"
    And the Unknown node still shows its meters

  # source: oss:src/live.js:211,227
  # source: oss:src/screens/infra.jsx:124
  # source: pro:src/live.js:522,535
  # source: pro:src/screens/infra.jsx:373
  @oss @pro
  Scenario: The Pods cell shows running pods over pod capacity
    Given 12 pods are scheduled on a node, 2 of them in the Succeeded or Failed phase
    And the node's allocatable pod count is 110
    When the Nodes screen is shown
    Then the node's Pods cell reads "10/110"
    And a node that reports no allocatable pod count uses 110 as the capacity

  # ---------------------------------------------------------------- version

  # source: oss:src/screens/infra.jsx:57-67
  # source: oss:src/live.js:228-229
  # source: pro:src/screens/infra.jsx:92-102
  # source: pro:src/live.js:536-537
  @oss @pro
  Scenario: A node with no stamped version shows only its kubelet version
    Given a node without the "stratum.ciq.com/k8s-version" annotation
    When the Nodes screen is shown
    Then the Version cell shows the kubelet version with no check mark and no drift badge

  # source: oss:src/screens/infra.jsx:57-67
  # source: pro:src/screens/infra.jsx:92-102
  @oss @pro
  Scenario: A node whose stamped version matches its kubelet shows a check mark
    Given a node whose stamped version and kubelet version are aligned
    When the Nodes screen is shown
    Then the Version cell shows the kubelet version followed by a green check mark

  # source: oss:src/version.js:19-22
  # source: oss:src/screens/infra.jsx:59-64
  # decision: D1
  @diverged @oss @drop
  Scenario: Version drift ignores a leading "v" and the "-stratum" build suffix
    Given a node stamped "1.30.2" whose kubelet reports "v1.30.2-stratum.1"
    When the Nodes screen is shown
    Then the node is aligned
    And a node stamped "1.30.3" whose kubelet reports "v1.30.2-stratum.1" shows the badge "drift → 1.30.3"
    And a node stamped "1.30.2" whose kubelet reports "v1.30.2+k3s1" shows a drift badge
    And hovering the cell shows "stratum.ciq.com/k8s-version: 1.30.3"

  # source: pro:src/version.js:11-17
  # source: pro:src/screens/infra.jsx:72,80-87,92-102
  # decision: D1
  @diverged @pro
  Scenario: Version drift compares major.minor.patch after cutting any build or prerelease suffix
    Given a node stamped "v1.36.4" whose kubelet reports "v1.36.4+k3s1"
    When the Nodes screen is shown
    Then the node is aligned
    And a node stamped "1.36" whose kubelet reports "v1.36.0-stratum.1" is aligned
    And a node stamped "v1.36.5" whose kubelet reports "v1.36.4" shows the badge "drift → 1.36.5"
    And hovering the badge shows "stamped k8s-version v1.36.5 differs from kubelet v1.36.4"

  # -------------------------------------------------------------- selection

  # source: oss:src/screens/infra.jsx:78-84,112,190-192
  # source: oss:src/app.jsx:375-382,483
  # source: pro:src/screens/infra.jsx:304-315,361,506-508
  # source: pro:src/app.jsx:345-354,508
  @oss @pro
  Scenario: Selecting a row opens the node's detail beside the list and records it in the URL
    When the user selects the row of node "worker-1"
    Then the row is highlighted
    And the detail pane for "worker-1" opens to the right of the list
    And the address gains the query parameter "node=worker-1"
    And a selected namespace stays in the address

  # source: oss:src/screens/infra.jsx:78-80
  # source: oss:src/app.jsx:263,383,483
  # source: pro:src/screens/infra.jsx:304-310
  # source: pro:src/app.jsx:164,355,508
  @oss @pro
  Scenario: A link carrying a node name opens that node's detail
    Given the address carries "node=cp-1"
    When the Nodes screen is shown
    Then the detail pane for "cp-1" is open

  # source: oss:src/screens/infra.jsx:84,190-191
  # source: pro:src/screens/infra.jsx:315,506-507
  @oss @pro
  Scenario: A link naming a node that does not exist opens no detail
    Given the address carries "node=gone" and no node has that name
    When the Nodes screen is shown
    Then no detail pane is open and the list takes the full width

  # source: oss:src/screens/infra.jsx:81,191
  # source: oss:src/app.jsx:375-382
  # source: pro:src/screens/infra.jsx:311,507
  # source: pro:src/app.jsx:345-354
  @oss @pro
  Scenario: Closing the detail pane clears the selection and the URL parameter
    Given the detail pane for "worker-1" is open
    When the user closes the detail pane
    Then the pane collapses and no row is highlighted
    And the "node" query parameter is removed from the address

  # source: oss:src/screens/infra.jsx:84,190-191
  # source: pro:src/screens/infra.jsx:315,506-507
  @oss @pro
  Scenario: The detail pane closes when the selected node disappears from the cluster
    Given the detail pane for "worker-1" is open
    When a refresh no longer returns "worker-1"
    Then the detail pane collapses

  # source: oss:src/screens/infra.jsx:78-84,190-191
  # source: pro:src/screens/infra.jsx:304-315,506-507
  @oss @pro
  Scenario: The detail pane reopens when a vanished selected node returns
    Given the detail pane for "worker-1" collapsed because a refresh no longer returned the node
    And the address still carries "node=worker-1"
    When a later refresh returns "worker-1" again
    Then the detail pane for "worker-1" opens by itself

  # source: oss:src/screens/infra.jsx:80
  # source: pro:src/screens/infra.jsx:310
  @oss @pro
  Scenario: Browser history moves between node details
    Given the user opened the detail of "worker-1" and then of "worker-2"
    When the user goes back in the browser history
    Then the detail pane shows "worker-1"
    And going back to an address without a "node" parameter closes the pane

  # ---------------------------------------------------------------- refresh

  # source: oss:src/screens/infra.jsx:100
  # source: oss:src/app.jsx:390-396,433
  # source: oss:src/live.js:447-458
  # decision: D1
  @diverged @oss
  Scenario Outline: The Refresh button reloads cluster data and reports the outcome
    When the user presses Refresh on the Nodes screen and <outcome>
    Then a toast reads "<toast>"

    Examples:
      | outcome                                            | toast                                 |
      | every read answers                                 | Refreshed                             |
      | some reads fail                                    | Refreshed — <stale parts> kept last data |
      | no read answers                                    | Refresh failed — keeping last data    |
      | the reload throws                                  | Refresh failed — keeping last data    |

  # source: oss:src/app.jsx:393-394
  # source: oss:src/live.js:450
  @oss
  Scenario: A refresh that finds the session ended shows no toast
    When the user presses Refresh and the server answers that the session has ended
    Then no toast is shown and the user is taken to sign-in

  # source: pro:src/screens/infra.jsx:349
  # source: pro:src/app.jsx:366-383,423
  # decision: D1
  @diverged @pro @drop
  Scenario Outline: The Refresh button reloads the selected cluster and reports the outcome
    When the user presses Refresh on the Nodes screen and <outcome>
    Then a toast reads "<toast>"

    Examples:
      | outcome                          | toast                              |
      | the cluster answers              | Refreshed                          |
      | the cluster cannot be reached    | Refresh failed — keeping last data |

  # source: pro:src/app.jsx:372-381
  @pro @fleet
  Scenario: A refresh overtaken by a cluster switch reports nothing
    Given the user pressed Refresh on cluster A
    When the user switches to cluster B before the refresh finishes
    Then the refresh shows no toast and changes no connection banner

  # source: pro:src/app.jsx:379-381
  @pro
  Scenario: A failed refresh raises the reconnecting banner only while auto-refresh is on
    When a manual refresh cannot reach the cluster
    Then the reconnecting banner is shown if auto-refresh is on
    And only the toast is shown if auto-refresh is paused

  # ------------------------------------------------------------ stale data

  # source: oss:src/live.js:242-245
  # decision: D1, D9
  @diverged @oss
  Scenario: Node rows are replaced only when both the node list and the pod list answer
    Given the Nodes screen shows 3 nodes
    When a reload reads the node list but the pod list fails
    Then the 3 rows keep their previous values
    And when a later reload answers both lists with zero nodes the table becomes empty

  # source: pro:src/live.js:436,554
  # decision: D1, D9
  @diverged @pro @drop
  Scenario: Node rows are replaced only when the reload returns at least one node
    Given the Nodes screen shows 3 nodes
    When a reload's node list fails or returns zero nodes
    Then the 3 rows keep their previous values

  # source: oss:src/app.jsx:314-323
  # decision: D1
  @diverged @oss @drop
  Scenario: The node list reloads 15 seconds after every load, whatever its outcome
    Given auto-refresh is on
    When 15 seconds pass after the previous load settled, whether it succeeded or failed
    Then the node list is reloaded without user action
    And no reload happens while auto-refresh is paused

  # source: pro:src/app.jsx:270-299
  # source: pro:src/load-outcome.js:41-44
  # source: pro:src/poll.js:9-10,19-26
  # decision: D1, D9
  @diverged @pro
  Scenario: The node list reloads 15 seconds after a successful load and backs off after a failed one
    Given auto-refresh is on
    When 15 seconds pass after a load that reached the cluster
    Then the node list is reloaded without user action
    And after a load that could not reach the cluster the next attempt is delayed by a growing, jittered back-off of at most 120 seconds
    And no reload happens while auto-refresh is paused

  # source: oss:src/app.jsx:470,504-506
  # decision: D1, D11
  @diverged @oss @drop
  Scenario: A "Loading…" hint is shown until the first load answers
    Given no load of the page has succeeded yet
    When the Nodes screen is shown
    Then a "Loading…" hint is shown over an empty table
    And the hint is never shown again once any load has succeeded

  # source: pro:src/app.jsx:477,527-529
  # decision: D1, D11
  @diverged @pro
  Scenario: A hint naming the cluster is shown until the first load answers
    Given no load of the page has succeeded yet
    When the Nodes screen of cluster "edge-7" is shown
    Then a "Loading edge-7…" hint is shown over an empty table
    And the hint is never shown again once any load has succeeded, including after a cluster switch
