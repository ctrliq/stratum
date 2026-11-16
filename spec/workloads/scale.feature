Feature: Scaling a workload
  Workloads that run a set number of replicas can be scaled from their
  detail panel.

  @diverged @oss
  # decision: D15 — both variants stand: this one on the dashboard server backend
  # source: oss:src/resources.jsx:240,256
  Scenario Outline: Scale is offered for the replica-count workloads, replication controllers included
    Given the session is offered write controls
    When a detail panel is open for a <kind>
    Then the footer offers "Scale"

    Examples:
      | kind                  |
      | Deployment            |
      | StatefulSet           |
      | ReplicaSet            |
      | ReplicationController |

  @diverged @pro @fleet
  # decision: D15 — both variants stand: this one on the hub backend
  # source: pro:src/resources.jsx:257,270-273
  Scenario Outline: Scale is offered for deployments, stateful sets and replica sets only
    Given the signed-in user has the admin role
    When a detail panel is open for a <kind>
    Then the footer <offer> "Scale"

    Examples:
      | kind                  | offer          |
      | Deployment            | offers         |
      | StatefulSet           | offers         |
      | ReplicaSet            | offers         |
      | ReplicationController | does not offer |

  @oss @pro
  # source: oss:src/resources.jsx:240,256
  # source: pro:src/resources.jsx:257,273
  Scenario: Kinds without a replica count are never offered Scale
    When a detail panel is open for a Pod, DaemonSet, Job, CronJob or any non-workload kind
    Then the footer does not offer "Scale"

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:273,295,303-306,329-336
  # source: pro:src/ui/resource-list.jsx:266,288,296-299,322-329
  Scenario Outline: Scale swaps the manifest for a replica count
    Given a detail panel is open for a workload whose spec <replicas>
    When the user presses "Scale"
    Then the manifest is replaced by a number field labelled "Desired replicas" holding <prefill>
    And the footer offers "Cancel", "Scale" and "Close", and the left-hand "Scale" button is gone

    Examples:
      | replicas               | prefill |
      | asks for 3 replicas    | 3       |
      | asks for 0 replicas    | 0       |
      | names no replica count | 1       |

  @diverged @oss
  # source: oss:src/ui/resource-list.jsx:62,228-229,273
  Scenario: The prefilled count is the one seen when the row was clicked
    Given a detail panel was opened for a workload and its replica count has since changed on the cluster
    When the user presses "Scale"
    Then the field holds the count from when the row was clicked, not the current one

  @diverged @pro @drop
  # decision: D1, D2 — one workload's count must not prefill another's; the oss variant above stands
  # source: pro:src/ui/resource-list.jsx:44,216,266
  Scenario: The prefilled count is the first clicked workload's when the panel was left open
    Given a detail panel was opened for workload A and the user then clicked workload B without closing the panel
    When the user presses "Scale"
    Then the field holds A's replica count as it was when A was clicked
    And pressing the footer's "Scale" sets B to that count

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:304
  # source: pro:src/ui/resource-list.jsx:297
  Scenario: Cancel leaves the workload untouched
    Given the replica count field is shown
    When the user presses "Cancel"
    Then the panel returns to the read-only manifest and nothing is sent

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:131,128,233
  # source: pro:src/ui/resource-list.jsx:118,115,220
  Scenario Outline: A valid count scales the workload and closes the panel
    Given the replica count field holds <count> for workload "api"
    When the user presses "Scale" and the cluster accepts
    Then only the workload's replica count is changed, to <count>
    And a notice reads "<notice>"
    And the list is re-read immediately and the panel closes

    Examples:
      | count | notice                    |
      | 0     | Scaled api → 0 replicas   |
      | 1     | Scaled api → 1 replica    |
      | 5     | Scaled api → 5 replicas   |

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:128,131,233
  # source: pro:src/ui/resource-list.jsx:115,118,220
  Scenario: A refused scale is reported and the panel closes anyway
    Given the replica count field holds a valid count
    When the user presses "Scale" and the cluster rejects or forbids it
    Then a notice reads "Error: <the cluster's message>"
    And the panel closes, unlike a refused Edit

  @diverged @oss
  # source: oss:src/ui/resource-list.jsx:43-48,305
  Scenario Outline: Only a whole count the cluster can hold may be submitted
    Given the replica count field holds <entry>
    Then the footer's "Scale" button is <state>

    Examples:
      | entry      | state    |
      | 3          | enabled  |
      | 0          | enabled  |
      | 2147483647 | enabled  |
      | nothing    | disabled |
      | -1         | disabled |
      | 1.5        | disabled |
      | 1e3        | disabled |
      | 2147483648 | disabled |

  @diverged @pro @drop
  # decision: D1, D2 — an emptied field must not scale to zero; the oss validation variant stands
  # source: pro:src/ui/resource-list.jsx:118,298
  Scenario Outline: Any entry is coerced to a count and submitted
    Given the replica count field holds <entry>
    When the user presses "Scale"
    Then the workload is scaled to <sent> replicas

    Examples:
      | entry   | sent |
      | 3       | 3    |
      | nothing | 0    |
      | -1      | 0    |
      | 1.9     | 1    |

  @oss
  # source: oss:src/ui/resource-list.jsx:131
  Scenario: A count that is not valid is refused with a notice if it is ever submitted
    Given a scale is submitted with a value that is not a whole count
    Then a notice reads "Error: "<the value>" is not a replica count"
    And nothing is sent to the cluster

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:78,305
  # source: pro:src/ui/resource-list.jsx:59,298
  Scenario: Scale cannot be pressed twice
    Given a write from this list has been sent and not yet answered
    Then the footer's "Scale" button is disabled until the answer arrives
