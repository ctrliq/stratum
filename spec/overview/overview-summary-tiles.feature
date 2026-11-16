Feature: Overview summary tiles
  Four tiles under the header: Nodes Ready, CPU Utilization, Memory
  Utilization and Pods Scheduled.

  # --------------------------------------------------------------- Nodes Ready

  @diverged @oss
  # source: oss:src/screens/overview.jsx:26-32,60-69
  Scenario Outline: Nodes Ready reads the cluster's own counts and tells four situations apart
    Given <situation>
    Then the Nodes Ready tile reads "<value>"
    And its caption reads "<caption>"

    Examples:
      | situation                                                          | value  | caption                  |
      | no load has produced the cluster's identity yet                    | 0 / 0  | Not loaded yet           |
      | the cluster reports 3 nodes, 3 ready, and no node rows have loaded | 3 / 3  | node details unavailable |
      | the cluster reports 0 nodes                                        | 0 / 0  | No nodes                 |
      | the cluster reports 3 nodes, 2 ready, and node rows are shown      | 2 / 3  | 1 not ready              |
      | the cluster reports 3 nodes, 3 ready, and node rows are shown      | 3 / 3  | All healthy              |

  @oss
  # source: oss:src/screens/overview.jsx:27-31
  Scenario: Nodes Ready follows the cluster's counts even when the node rows are older
    Given the cluster's identity says 4 nodes with 4 ready
    And the node rows still show 3 nodes from an earlier load
    Then the Nodes Ready tile reads "4 / 4"

  @oss
  # source: oss:src/screens/overview.jsx:65-68
  Scenario: Missing node rows are reported before a not-ready count
    Given the cluster reports 3 nodes with 2 ready
    And no node rows have loaded
    Then the Nodes Ready tile reads "2 / 3"
    And its caption reads "node details unavailable", not "1 not ready"

  @diverged @pro @drop
  # decision: D1, D2 — the oss variant stands; "0 / 0 · All healthy" over no data is a defect (NOTES)
  # source: pro:src/screens/overview.jsx:34,70-74
  Scenario Outline: Nodes Ready counts the node rows whose status is Ready
    Given the node rows show <rows>
    Then the Nodes Ready tile reads "<value>"
    And its caption reads "<caption>"

    Examples:
      | rows                                         | value  | caption      |
      | 3 nodes, all Ready                           | 3 / 3  | All healthy  |
      | 3 nodes: 2 Ready and 1 NotReady              | 2 / 3  | 1 not ready  |
      | 3 nodes: 2 Ready and 1 Unknown               | 2 / 3  | 1 not ready  |
      | no nodes, because nothing has loaded yet     | 0 / 0  | All healthy  |

  @oss @pro
  # source: oss:src/live.js:220-224,423
  # source: oss:src/screens/overview.jsx:30-31,62
  # source: pro:src/live.js:500-508,531
  # source: pro:src/screens/overview.jsx:34
  Scenario: A cordoned node that is Ready counts as ready
    Given 3 nodes are Ready and one of them is cordoned
    Then the Nodes Ready tile reads "3 / 3"

  # ----------------------------------------------------------- CPU and memory

  @oss @pro
  # source: oss:src/screens/overview.jsx:35-37,70-79
  # source: pro:src/screens/overview.jsx:37-39,75-84
  Scenario: CPU and memory utilisation are the average over ready nodes
    Given three ready nodes at 20%, 40% and 75% CPU and 10%, 50% and 91% memory
    And one node that is not ready at 99% CPU and 99% memory
    Then the CPU Utilization tile reads "45%"
    And the Memory Utilization tile reads "50%"

  @oss @pro
  # source: oss:src/screens/overview.jsx:36-37
  # source: pro:src/screens/overview.jsx:38-39
  Scenario: With no ready node both utilisation tiles read zero
    Given no node is ready, or no node rows have loaded
    Then the CPU Utilization tile reads "0%"
    And the Memory Utilization tile reads "0%"

  @oss @pro
  # source: oss:src/screens/overview.jsx:12,73,78
  # source: pro:src/screens/overview.jsx:13,78,83
  Scenario Outline: The utilisation bar changes tone at 70 and 85 percent
    Given the average utilisation is <percent>%
    Then the tile's bar is filled to <percent>% in the <tone> tone

    Examples:
      | percent | tone    |
      | 0       | success |
      | 70      | success |
      | 71      | warning |
      | 85      | warning |
      | 86      | error   |
      | 100     | error   |

  @pro
  # source: pro:src/screens/overview.jsx:37
  Scenario: A node whose readiness is unknown is left out of the averages
    Given one Ready node at 30% CPU and one node of Unknown readiness at 90% CPU
    Then the CPU Utilization tile reads "30%"

  # ------------------------------------------------------------ Pods Scheduled

  @oss @pro
  # source: oss:src/screens/overview.jsx:33-34,80-84
  # source: pro:src/screens/overview.jsx:35-36,85-89
  Scenario: Pods Scheduled compares running pods with the nodes' pod capacity
    Given three nodes running 30, 25 and 11 unfinished pods, each with a pod capacity of 110
    Then the Pods Scheduled tile reads "66 / 330"
    And its caption reads "20% of capacity"

  @oss @pro
  # source: oss:src/screens/overview.jsx:33
  # source: pro:src/screens/overview.jsx:35
  Scenario: Pods on nodes that are not ready still count
    Given a ready node running 10 pods and a node that is not ready running 5 pods
    Then the Pods Scheduled tile counts 15 pods

  @oss @pro @drop
  # decision: D2 — a capacity of 1 that no node stated is shown (NOTES)
  # source: oss:src/screens/overview.jsx:33-34,82-83
  # source: pro:src/screens/overview.jsx:35-36,87-88
  Scenario: With no node rows the pods tile reads zero of one
    Given no node rows have loaded
    Then the Pods Scheduled tile reads "0 / 1"
    And its caption reads "0% of capacity"

  @oss @pro @fixed
  # decision: D2
  # source: oss:src/screens/overview.jsx:33-34,82-83
  # source: pro:src/screens/overview.jsx:35-36,87-88
  Scenario: With no node rows the pods tile states no capacity
    Given no node rows have loaded
    Then the Pods Scheduled tile reads "0 / 0"
    And it shows no percentage of capacity

  @oss @pro
  # source: oss:src/screens/overview.jsx:33
  # source: oss:src/live.js:211,227
  # source: pro:src/screens/overview.jsx:35
  # source: pro:src/live.js:522,535
  Scenario: Pods that are not assigned to a node are not counted
    Given 4 pods are Pending with no node assigned
    Then they are not part of the Pods Scheduled figure
