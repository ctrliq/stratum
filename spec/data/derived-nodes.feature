Feature: Derived node values
  What each node row carries after a load: role, readiness, utilisation, pod
  counts and the details read off the node object, plus the pressure
  conditions aggregated across nodes.

  # ---------------------------------------------------------------------- role

  @oss @pro
  # source: oss:src/live.js:216,219
  # source: pro:src/live.js:527,530
  # source: pro:src/k8sident.js:15,22
  Scenario Outline: A node's role comes from its labels
    Given a node labelled <labels>
    Then its role is "<role>"

    Examples:
      | labels                                          | role          |
      | "node-role.kubernetes.io/control-plane" (empty) | control-plane |
      | "stratum.ciq.com/control-plane" = "true"        | control-plane |
      | nothing role-related                            | worker        |

  @diverged @oss
  # source: oss:src/live.js:216
  Scenario Outline: The legacy master label counts and the Stratum label must say true
    Given a node labelled <labels>
    Then its role is "<role>"

    Examples:
      | labels                                     | role          |
      | "node-role.kubernetes.io/master" (empty)   | control-plane |
      | "stratum.ciq.com/control-plane" = "false"  | worker        |

  @diverged @pro @drop
  # decision: D1, D2 — the oss variant stands (NOTES P9)
  # source: pro:src/live.js:527
  Scenario Outline: The legacy master label is ignored and the Stratum label counts by presence
    Given a node labelled <labels>
    Then its role is "<role>"

    Examples:
      | labels                                     | role          |
      | "node-role.kubernetes.io/master" (empty)   | worker        |
      | "stratum.ciq.com/control-plane" = "false"  | control-plane |

  # ----------------------------------------------------------------- readiness

  @diverged @oss @drop
  # decision: D1 — the pro variant (three-way readiness, cordon separate) stands
  # source: oss:src/live.js:209-210,220-224,234
  Scenario Outline: A node is ready or not, with cordon shown in its status
    Given a node whose Ready condition is <ready condition> and which is <schedulable>
    Then the node counts as <counted>
    And its displayed status is "<status>"

    Examples:
      | ready condition | schedulable | counted   | status             |
      | "True"          | schedulable | ready     | Ready              |
      | "True"          | cordoned    | ready     | SchedulingDisabled |
      | "False"         | schedulable | not ready | NotReady           |
      | "False"         | cordoned    | not ready | NotReady           |
      | "Unknown"       | schedulable | not ready | NotReady           |
      | absent          | schedulable | not ready | NotReady           |

  @diverged @pro
  # decision: D1 — also the rule on the dashboard server backend, where no hub answers
  # source: pro:src/live.js:509-521,531-532,550
  Scenario Outline: Without hub facts a node is Ready, NotReady or Unknown, and cordon is separate
    Given no hub answers for the cluster
    And a node whose Ready condition is <ready condition> and which is <schedulable>
    Then its displayed status is "<status>"
    And it is flagged as cordoned: <cordoned>

    Examples:
      | ready condition | schedulable | status   | cordoned |
      | "True"          | schedulable | Ready    | no       |
      | "True"          | cordoned    | Ready    | yes      |
      | "False"         | schedulable | NotReady | no       |
      | "Unknown"       | schedulable | Unknown  | no       |
      | absent          | schedulable | Unknown  | no       |

  @pro
  # source: pro:src/live.js:509-521,531
  Scenario Outline: With hub facts the hub's statement decides a node's readiness
    Given the hub's facts for the cluster were loaded
    And for the node the facts state <fact>
    And the node object's own Ready condition is "True"
    Then its displayed status is "<status>"

    Examples:
      | fact                              | status   |
      | a Ready condition of true         | Ready    |
      | a Ready condition of false        | NotReady |
      | no Ready condition                | Unknown  |
      | nothing, the node is not listed   | Unknown  |

  # --------------------------------------------------------------- utilisation

  @oss @pro
  # source: oss:src/live.js:211-215,225-226
  # source: pro:src/live.js:522-526,533-534
  Scenario Outline: A node's CPU and memory percentages are usage over allocatable, capped at 100
    Given a node with allocatable CPU "<alloc cpu>" and memory "<alloc mem>"
    And its kubelet reports <used cpu> millicores and <used mem> Mi in use
    Then the node shows <cpu pct>% CPU and <mem pct>% memory

    Examples:
      | alloc cpu | alloc mem | used cpu | used mem | cpu pct | mem pct |
      | 4         | 16Gi      | 1000     | 4096     | 25      | 25      |
      | 2         | 8Gi       | 1234     | 2500     | 62      | 31      |
      | 2         | 8Gi       | 2600     | 9000     | 100     | 100     |
      | 4         | 16Gi      | 0        | 0        | 0       | 0       |

  @oss @pro
  # source: oss:src/live.js:211-215,225-226
  # source: pro:src/live.js:522-526,533-534
  Scenario: Without a usage summary a node's percentages come from pod requests
    Given a node with allocatable CPU "4" and memory "8Gi" whose kubelet did not answer
    And the pods on it that have not finished request 1500 millicores and 2048 Mi in total
    Then the node shows 38% CPU and 25% memory

  @oss @pro
  # source: oss:src/live.js:211
  # source: pro:src/live.js:522
  Scenario: Finished pods do not count towards a node
    Given a node runs 3 pods in phase "Running", 1 in phase "Pending", 2 in phase "Succeeded" and 1 in phase "Failed"
    Then the node's pod count is 4
    And only those 4 pods' requests enter its request-based percentages

  @oss @pro
  # source: oss:src/live.js:225-226
  # source: pro:src/live.js:533-534
  Scenario: A node that reports no allocatable figure shows zero percent
    Given a node whose status carries no allocatable CPU
    Then the node shows 0% CPU

  @oss @pro
  # source: oss:src/live.js:227
  # source: pro:src/live.js:535
  Scenario Outline: A node's pod capacity is its allocatable pod count, defaulting to 110
    Given a node whose allocatable pod count is <allocatable>
    Then its pod capacity is <capacity>

    Examples:
      | allocatable | capacity |
      | "250"       | 250      |
      | not stated  | 110      |

  # ------------------------------------------------------------------- details

  @oss @pro
  # source: oss:src/live.js:228-235
  # source: pro:src/live.js:536-551
  Scenario Outline: Node details are read off the node object and shown as a dash when absent
    Given a node object
    Then its <detail> is taken from <field>, or "—" when that is absent

    Examples:
      | detail             | field                                                         |
      | kubelet version    | the node info's kubelet version                               |
      | Stratum version    | the annotation "stratum.ciq.com/k8s-version"                  |
      | operating system   | the node info's OS image                                      |
      | container runtime  | the node info's container runtime version                     |
      | kernel             | the node info's kernel version                                |
      | zone               | the label "topology.kubernetes.io/zone"                       |

  @oss @pro
  # source: oss:src/live.js:233
  # source: pro:src/live.js:549
  Scenario: The instance type falls back to the beta label
    Given a node with no "node.kubernetes.io/instance-type" label
    And the label "beta.kubernetes.io/instance-type" set to "m5.large"
    Then its instance type is "m5.large"

  @oss
  # source: oss:src/live.js:236-239
  Scenario: Conditions, labels and taints are carried as the node states them
    Given a node with the taint key "dedicated", value "gpu" and effect "NoSchedule"
    And a taint with key "node.kubernetes.io/unreachable", no value and effect "NoExecute"
    Then its taints read "dedicated=gpu:NoSchedule" and "node.kubernetes.io/unreachable:NoExecute"
    And every condition type the node reports is available with its status
    And its labels are available unchanged

  @pro
  # source: pro:src/live.js:541-548
  Scenario: Extended resources are listed as accelerators
    Given a node whose capacity lists "cpu", "memory", "pods", "hugepages-2Mi" and "nvidia.com/gpu" = "2"
    Then its accelerators are exactly "nvidia.com/gpu" with count "2"
    And its CPU architecture is taken from the node info, or "—" when absent

  # ------------------------------------------------------- pressure conditions

  @oss @pro
  # source: oss:src/live.js:289-299
  # source: pro:src/live.js:589-597
  Scenario: Pressure conditions name the nodes on which they are true
    Given the nodes "a", "b" and "c"
    And "MemoryPressure" is "True" on "b" only
    Then the cluster-wide conditions are, in order, MemoryPressure, DiskPressure, PIDPressure and NetworkUnavailable
    And MemoryPressure names "b"
    And the other three name no node

  @diverged @oss
  # source: oss:src/live.js:285-299
  Scenario: A node that does not report a condition is counted as unknown, not as healthy
    Given the nodes "a", "b" and "c"
    And "DiskPressure" is "False" on "a", "Unknown" on "b" and not reported by "c"
    Then DiskPressure names no node as true
    And it names "b" and "c" as unknown
    And it states that 3 nodes were considered

  @diverged @pro @drop
  # decision: D1 — the oss variant (unknown nodes recorded) stands
  # source: pro:src/live.js:589-597
  Scenario: A node that does not report a condition is indistinguishable from a healthy one
    Given the nodes "a", "b" and "c"
    And "DiskPressure" is "False" on "a", "Unknown" on "b" and not reported by "c"
    Then DiskPressure names no node as true
    And nothing records that "b" and "c" are unknown

  # ------------------------------------------------------------- version drift

  @diverged @oss @drop
  # decision: D1 — the pro variant (hub normalisation) stands
  # source: oss:src/version.js:19-22
  Scenario Outline: A node drifts when its stamped version differs from its kubelet's upstream version
    Given a node stamped with the Stratum version "<stamped>" whose kubelet reports "<kubelet>"
    Then the node is <verdict>

    Examples:
      | stamped  | kubelet                    | verdict     |
      | 1.37.1   | v1.37.1-stratum.0          | aligned     |
      | v1.37.1  | v1.37.1-stratum.0-rc.1     | aligned     |
      | 1.37.1   | v1.37.2-stratum.0          | drifted     |
      | 1.37.1   | v1.37.1+k3s1               | drifted     |
      | 1.37     | v1.37.0-stratum.0          | drifted     |
      | —        | v1.37.1-stratum.0          | aligned     |

  @diverged @pro
  # source: pro:src/version.js:11-17
  # source: pro:src/live.js:737-738
  Scenario Outline: A node drifts when its stamped and kubelet versions differ after normalisation
    Given a node stamped with the Stratum version "<stamped>" whose kubelet reports "<kubelet>"
    Then the node is <verdict>

    Examples:
      | stamped  | kubelet                    | verdict     |
      | 1.37.1   | v1.37.1-stratum.0          | aligned     |
      | v1.37.1  | v1.37.1-stratum.0-rc.1     | aligned     |
      | 1.37.1   | v1.37.2-stratum.0          | drifted     |
      | 1.37.1   | v1.37.1+k3s1               | aligned     |
      | 1.37     | v1.37.0-stratum.0          | aligned     |
      | —        | v1.37.1-stratum.0          | aligned     |

  @pro
  # source: pro:src/live.js:738
  Scenario: With hub facts the cluster-level drift verdict is the hub's
    Given the hub's facts state that versions have drifted
    And every node's stamped and kubelet versions agree
    Then the cluster is reported as drifted

  @pro
  # source: pro:src/version.js:19-35
  # source: pro:src/live.js:738
  Scenario Outline: Without hub facts the cluster drifts on a stamp mismatch or a kubelet minor skew
    Given no hub answers for the cluster
    And the apiserver serves version "<serving>"
    And a node is stamped "<stamped>" and its kubelet reports "<kubelet>"
    Then the cluster is <verdict>

    Examples:
      | serving  | stamped | kubelet            | verdict     |
      | v1.37.1  | 1.37.1  | v1.37.1-stratum.0  | not drifted |
      | v1.37.1  | 1.37.0  | v1.37.1-stratum.0  | drifted     |
      | v1.37.1  | —       | v1.37.0-stratum.0  | not drifted |
      | v1.37.1  | —       | v1.36.4-stratum.0  | drifted     |
      | v1.37.1  | 1.37    | v1.37.1            | drifted     |
      | unknown  | 1.36.0  | v1.36.0            | not drifted |
