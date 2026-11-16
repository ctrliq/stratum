Feature: Cluster usage trend and node conditions
  Below the node table the Nodes screen shows cluster-wide CPU and memory
  usage with a short trend, and a roll-up of pressure conditions across
  nodes.

  # ------------------------------------------------------------ usage cards

  # source: oss:src/screens/infra.jsx:149-162
  # decision: D1
  @diverged @oss
  Scenario: The usage cards are titled for the cluster
    When the Nodes screen is shown
    Then the usage cards are titled "Cluster CPU" and "Cluster Memory"

  # source: pro:src/screens/infra.jsx:471-484
  # decision: D1
  @diverged @pro @drop
  Scenario: The usage cards are titled for the fleet
    When the Nodes screen is shown
    Then the usage cards are titled "Fleet CPU" and "Fleet Memory"

  # source: oss:src/screens/infra.jsx:28-47,149-162
  # source: oss:src/live.js:260-282
  # source: pro:src/screens/infra.jsx:39-58,471-484
  # source: pro:src/live.js:563-586
  @oss @pro
  Scenario: A usage card shows the percentage, a caption and a trend
    Given measured usage of 3.5 of 16 cores and 12.0 of 64.0 GiB across 4 nodes
    And at least 3 usage samples have been collected
    When the Nodes screen is shown
    Then the CPU card shows "22%" and the caption "3.50 / 16 cores · 4 nodes"
    And the memory card shows "19%" and the caption "12.0 / 64.0 GiB · 4 nodes"
    And each card draws a sparkline of the collected samples

  # source: oss:src/screens/infra.jsx:153,160
  # source: pro:src/screens/infra.jsx:475,482
  @oss @pro
  Scenario: The caption uses the singular for one node
    Given measured usage covers exactly 1 node
    When the Nodes screen is shown
    Then the captions end with "· 1 node"

  # source: oss:src/screens/infra.jsx:30,37-43
  # source: pro:src/screens/infra.jsx:41,48-54
  @oss @pro
  Scenario: A card with fewer than three samples says it is collecting
    Given usage is available and 2 samples have been collected
    When the Nodes screen is shown
    Then the card shows "collecting · 2 of 24 samples" instead of a sparkline

  # source: oss:src/screens/infra.jsx:35,40-44
  # source: pro:src/screens/infra.jsx:46,51-55
  @oss @pro
  Scenario: A card with no usage data says so
    Given no node's kubelet stats summary has ever been read
    When the Nodes screen is shown
    Then the card shows "No usage data — kubelet Summary API unavailable"
    And no percentage is shown
    And the caption is "—"

  # source: oss:src/live.js:251,270-282
  # source: pro:src/live.js:561,574-586
  @oss @pro
  Scenario: The trend keeps the 24 most recent samples for as long as the page lives
    Given 24 samples have been collected
    When another load adds a sample
    Then the oldest sample is dropped and 24 remain
    And the samples are lost when the page is reloaded

  # source: pro:src/data.js:46-52
  # source: pro:src/app.jsx:231
  @pro @fleet
  Scenario: Switching cluster restarts the usage trend
    Given usage figures and a trend are shown for cluster A
    When the user switches to cluster B
    Then the cards start with no figures and an empty trend
    And returning to cluster A does not bring its earlier samples back

  # source: oss:src/live.js:248-259,437
  # decision: D1
  @diverged @oss
  Scenario: Cluster usage is recomputed only when every node reported usage
    Given a cluster of 4 nodes where one kubelet stats summary cannot be read
    When a load completes
    Then the usage cards keep the previous figures and no sample is added to the trend
    And the node rows still update, using pod requests for the unreachable node

  # source: pro:src/live.js:557-587
  # decision: D1
  @diverged @pro @drop
  Scenario: Cluster usage is recomputed over whichever nodes reported usage
    Given a cluster of 4 nodes where one kubelet stats summary cannot be read
    When a load completes
    Then the usage cards show used and total figures summed over the 3 reporting nodes
    And the caption reads "· 3 nodes"
    And a sample is added to the trend

  # source: oss:src/live.js:253-258
  @oss
  Scenario: A cluster that answers with no nodes resets the usage cards
    Given usage figures and a trend are shown
    When a load answers with an empty node list
    Then the cards show no percentage, the no-usage-data message and an empty trend

  # source: pro:src/live.js:563
  @pro
  Scenario: Usage cards keep their last figures when no node reports usage
    Given usage figures and a trend are shown
    When a load finds no node with readable usage
    Then the cards keep the previous figures and trend

  # source: oss:src/live.js:267-268
  # source: pro:src/live.js:571-572
  @oss @pro
  Scenario: Cluster percentages are rounded and capped at 100
    Given measured usage exceeds the summed allocatable
    When the Nodes screen is shown
    Then the card shows "100%"

  # ------------------------------------------------------------- conditions

  # source: oss:src/screens/infra.jsx:163-168
  # source: oss:src/data.js:37
  # source: pro:src/screens/infra.jsx:485-490
  # source: pro:src/data.js:54
  @oss @pro
  Scenario: The Conditions card says there is no node data before any load
    Given no cluster data has been loaded
    When the Nodes screen is shown
    Then the Conditions card shows "No node data"

  # source: oss:src/live.js:289-299
  # source: oss:src/screens/infra.jsx:168-183
  # source: pro:src/live.js:590-597
  # source: pro:src/screens/infra.jsx:490-499
  @oss @pro
  Scenario: The Conditions card rolls up four pressure conditions across nodes
    Given nodes "worker-1" and "worker-2" report DiskPressure as "True"
    When the Nodes screen is shown
    Then the card lists MemoryPressure, DiskPressure, PIDPressure and NetworkUnavailable in that order
    And DiskPressure shows a warning badge reading "2 nodes"
    And hovering the badge lists "worker-1, worker-2"
    And a condition true on a single node reads "1 node"

  # source: oss:src/live.js:289-299
  # source: oss:src/screens/infra.jsx:171-181
  # decision: D1
  @diverged @oss
  Scenario Outline: A condition no node reports as True distinguishes False from unknown
    Given a cluster of 3 nodes where no node reports MemoryPressure as "True"
    And <unknown> of them report it as "Unknown" or do not report it
    When the Nodes screen is shown
    Then MemoryPressure shows a <colour> badge reading "<badge>"

    Examples:
      | unknown | colour  | badge     |
      | 0       | success | False     |
      | 1       | gray    | 1 unknown |
      | 3       | gray    | —         |

  # source: pro:src/screens/infra.jsx:493-494
  # source: pro:src/live.js:590-597
  # decision: D1
  @diverged @pro @drop
  Scenario: A condition no node reports as True always reads False
    Given no node reports MemoryPressure as "True"
    And some nodes report it as "Unknown" or do not report it
    When the Nodes screen is shown
    Then MemoryPressure shows a success badge reading "False"

  # source: oss:src/live.js:290
  # decision: D1, D9
  @diverged @oss
  Scenario: The conditions roll-up keeps its last values when the node list cannot be read
    Given the Conditions card shows DiskPressure on 2 nodes
    When a reload fails to read the node list
    Then the card still shows DiskPressure on 2 nodes

  # source: pro:src/live.js:436,590-597
  # decision: D1, D2
  @diverged @pro @drop
  Scenario: The conditions roll-up reads False everywhere when the node list cannot be read
    Given the Conditions card shows DiskPressure on 2 nodes
    When a reload fails to read the node list but other reads succeed
    Then every condition on the card reads "False"
