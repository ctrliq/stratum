Feature: Overview health cards
  The bottom row of the Overview: workload health, the control plane and the
  cluster's own facts. The fleet dashboard adds the hub's engine-state card.

  # ----------------------------------------------------------- Workload Health

  @oss @pro
  # source: oss:src/screens/overview.jsx:39,149-157
  # source: pro:src/screens/overview.jsx:41,159-167
  Scenario: Workloads are counted into four health groups
    Given 20 workloads: 12 Running, 2 Progressing, 1 Degraded, 1 Failed, 3 Scheduled and 1 Complete
    Then the "Workload Health" card reads Running 12, Progressing 2, Degraded / Failed 2 and Scheduled (jobs) 4

  @oss @pro
  # source: oss:src/screens/overview.jsx:165-176
  # source: pro:src/screens/overview.jsx:175-186
  Scenario: Each health group has a bar showing its share of all workloads
    Given 20 workloads of which 12 are Running and 1 is Degraded
    Then the Running bar is filled to 60% and the Degraded / Failed bar to 5%
    And with no workloads at all every bar is empty and every count is 0

  @oss @pro
  # source: oss:src/screens/overview.jsx:152-155
  # source: oss:src/live.js:105
  # source: pro:src/screens/overview.jsx:162-165
  # source: pro:src/live.js:182-184,281,638
  Scenario: Workloads scaled to zero are counted with the scheduled jobs
    Given no hub's facts were loaded for the cluster
    And a Deployment that wants 0 replicas
    Then it is counted under "Scheduled (jobs)"

  @pro
  # source: pro:src/screens/overview.jsx:162-165
  # source: pro:src/live.js:638
  Scenario: A Deployment the hub gives no verdict for is in no health group
    Given the hub's facts do not list one Deployment, so its status is "Unknown"
    Then that Deployment is counted in none of the four groups
    And it still counts towards the total the bars are measured against

  # ------------------------------------------------------- pro: engine state

  @pro
  # source: pro:src/screens/overview.jsx:154-158
  Scenario: The hub's engine-state card leads the bottom row
    When the Overview is shown for a cluster
    Then the hub's engine-state card for that cluster is the first card of the bottom row
    And it is refreshed whenever the cluster's data is refreshed

  # -------------------------------------------------------------- Control Plane

  @oss @pro
  # source: oss:src/screens/overview.jsx:183-199
  # source: pro:src/screens/overview.jsx:193-214
  Scenario Outline: The control-plane card counts ready control-plane nodes and judges them
    Given the node rows include <total> control-plane nodes of which <ready> are ready
    Then the card reads "<ready> / <total> ready" with the caption "<caption>"
    And its badge reads "<badge>"

    Examples:
      | total | ready | caption                              | badge    |
      | 0     | 0     | no control-plane nodes visible       | Pending  |
      | 1     | 1     | single control-plane node            | Healthy  |
      | 1     | 0     | single control-plane node            | Degraded |
      | 3     | 3     | 3 control-plane nodes · kube-vip VIP | Healthy  |
      | 3     | 2     | 3 control-plane nodes · kube-vip VIP | Degraded |

  @oss @pro
  # source: oss:src/screens/overview.jsx:201-203
  # source: pro:src/screens/overview.jsx:216-218
  Scenario: Static pod counts are shown as running over total
    Given the load found 9 static pods in kube-system, 9 running, of which 3 apiserver and 3 etcd pods, all running
    Then the card reads "Static pods (kube-system)  9/9 running", "kube-apiserver  3/3" and "etcd  3/3"

  @oss @pro
  # source: oss:src/screens/overview.jsx:201-203
  # source: pro:src/screens/overview.jsx:216-218
  Scenario Outline: Control-plane rows show a dash when there is nothing to count
    Given <situation>
    Then the "<row>" row reads "<value>"

    Examples:
      | situation                                        | row                       | value        |
      | the cluster facts have not loaded                | Static pods (kube-system) | —            |
      | the cluster facts have not loaded                | kube-apiserver            | —            |
      | the load found no apiserver mirror pods          | kube-apiserver            | —            |
      | the load found no etcd mirror pods               | etcd                      | —            |
      | the load found no static pods at all             | Static pods (kube-system) | 0/0 running  |

  @oss @pro
  # source: oss:src/screens/overview.jsx:204
  # source: pro:src/screens/overview.jsx:219
  Scenario: Detected kube-vip pods are shown as running over total
    Given the load found 3 kube-vip pods of which 2 are running
    Then the kube-vip row reads "2/3 running" without a warning tone

  @diverged @oss @drop
  # decision: D1 — the pro variant stands
  # source: oss:src/screens/overview.jsx:204
  Scenario: Missing kube-vip is always flagged
    Given the load found no kube-vip pods, or the cluster facts have not loaded
    Then the kube-vip row reads "not detected" in the warning tone, whatever the number of control-plane nodes

  @diverged @pro
  # source: pro:src/screens/overview.jsx:199-203,219
  Scenario Outline: Missing kube-vip is flagged only where a virtual IP is expected
    Given the node rows include <control planes> control-plane nodes and no kube-vip pods were found
    Then the kube-vip row reads "<value>" <tone>

    Examples:
      | control planes | value          | tone                   |
      | 0              | not configured | without a warning tone |
      | 1              | not configured | without a warning tone |
      | 2              | not detected   | in the warning tone    |
      | 3              | not detected   | in the warning tone    |

  @diverged @oss @drop
  # decision: D1 — the pro variant stands
  # source: oss:src/screens/overview.jsx:205
  Scenario Outline: The operator row names the leader or says it was not detected
    Given <situation>
    Then the Operator row reads "<value>" <tone>

    Examples:
      | situation                                               | value        | tone                   |
      | the operator is active and led by "op-7f9c"             | op-7f9c      | without a warning tone |
      | the operator's Lease has expired                        | not detected | in the warning tone    |
      | no operator Lease exists                                | not detected | in the warning tone    |
      | the cluster facts have not loaded                       | not detected | in the warning tone    |

  @diverged @pro
  # source: pro:src/screens/overview.jsx:220
  Scenario Outline: The operator row tells "not detected" apart from "no verdict yet"
    Given <situation>
    Then the Operator row reads "<value>" <tone>

    Examples:
      | situation                                               | value         | tone                   |
      | the operator is active and led by "op-7f9c"             | op-7f9c       | without a warning tone |
      | the operator's state is "inactive"                      | not detected  | in the warning tone    |
      | the operator's state is "missing"                       | not detected  | in the warning tone    |
      | the operator's state is "unknown"                       | no verdict yet | in the warning tone   |
      | the operator's state is one this dashboard does not know | no verdict yet | in the warning tone  |
      | the cluster facts have not loaded                       | no verdict yet | in the warning tone   |

  @oss @pro
  # source: oss:src/screens/overview.jsx:184-185
  # source: pro:src/screens/overview.jsx:194-195
  # assumption: the card counts control-plane nodes from the node rows, not from the cluster facts, so it follows the node rows' freshness
  Scenario: The control-plane counts come from the node rows currently shown
    Given the node rows show 3 control-plane nodes
    And the cluster facts are from a different load
    Then the card's ready count is computed from the 3 node rows

  # ---------------------------------------------------------------- Cluster card

  @oss @pro
  # source: oss:src/screens/overview.jsx:220-240
  # source: pro:src/screens/overview.jsx:239-263
  Scenario: The cluster card lists the cluster's versions, source, CNI and default storage
    Given the Kubernetes version is "1.37.1", the Stratum version is "1.37.1-stratum.0" and the source is "local"
    And the CNI is "Cilium" and the default storage class is "longhorn"
    Then the "Cluster" card reads Kubernetes "v1.37.1", Stratum "1.37.1-stratum.0", Source "local", CNI "Cilium" and Default storage "longhorn"

  @oss @pro
  # source: oss:src/screens/overview.jsx:222-226
  # source: pro:src/screens/overview.jsx:243-247
  Scenario Outline: Unknown cluster facts are shown as a dash
    Given the cluster's <fact> is not known
    Then the "<row>" row reads "—"

    Examples:
      | fact                   | row             |
      | Kubernetes version     | Kubernetes      |
      | source                 | Source          |
      | CNI                    | CNI             |
      | default storage class  | Default storage |

  @oss @pro
  # source: oss:src/screens/overview.jsx:23-24
  # source: pro:src/screens/overview.jsx:23-24
  Scenario: The default storage class prefers the loaded storage classes over the cluster facts
    Given the loaded storage classes mark "fast" as default
    And the cluster facts, from an older load, name "slow"
    Then the Default storage row reads "fast"
    And when no loaded class is the default, the row reads what the cluster facts name

  @oss @pro
  # source: oss:src/screens/overview.jsx:226
  # source: pro:src/screens/overview.jsx:247
  Scenario: Clicking the default storage class opens the Storage screen
    When the user clicks the value of the Default storage row
    Then the Storage screen opens

  @diverged @oss @drop
  # decision: D1 — the pro fallback chain stands; on the dashboard server backend its first source is the server's own reported version (NOTES pair 7)
  # source: oss:src/screens/overview.jsx:223
  # source: oss:src/live.js:422
  Scenario: The Stratum version is the dashboard server's own version
    Given the dashboard server reports its version as "0.9.2"
    Then the Stratum row reads "0.9.2"
    And "—" when the server reports none

  @diverged @pro
  # source: pro:src/screens/overview.jsx:25-32,244
  Scenario Outline: The Stratum version falls back to a Stratum-built kubelet version
    Given the hub's record states the Stratum version as <recorded>
    And the node rows' kubelet versions are <kubelets>
    Then the Stratum row reads "<shown>"

    Examples:
      | recorded    | kubelets                                   | shown                   |
      | "2.1.0"     | "v1.37.1-stratum.0-rc.1"                   | 2.1.0                   |
      | nothing     | "v1.37.1-stratum.0-rc.1"                   | v1.37.1-stratum.0-rc.1  |
      | "—"         | "v1.37.1", "v1.37.1-stratum.0"             | v1.37.1-stratum.0       |
      | nothing     | "v1.37.1" and "v1.37.1+k3s1"               | —                       |

  @pro
  # source: pro:src/screens/overview.jsx:240,248
  Scenario Outline: The cluster card reports kubelet-serving certificate requests
    Given <situation>
    Then the "Kubelet-serving CSRs" row reads "<value>" <tone>

    Examples:
      | situation                                                     | value                    | tone                   |
      | the request list was read: 5 approved, 0 pending              | 5 approved · 0 pending   | without a warning tone |
      | the request list was read: 5 approved, 2 pending              | 5 approved · 2 pending   | in the warning tone    |
      | the request list could not be read, or was refused            | Unknown                  | without a warning tone |
      | the hardening facts have not loaded                           | —                        | without a warning tone |

  @pro @drop
  # decision: D2 — the literal "null" is printed (NOTES)
  # source: pro:src/screens/overview.jsx:248
  # source: pro:src/live.js:783-784
  # assumption: read from the code, not rendered; a render with hub facts stating availability "available" and no pending count would confirm the literal text
  Scenario: A pending count the hub does not state is printed as "null"
    Given the hub's facts say the request list is available but state no pending count
    And the cluster's own list shows 5 approved requests
    Then the "Kubelet-serving CSRs" row reads "5 approved · null pending" without a warning tone

  @pro @fixed
  # decision: D2
  # source: pro:src/screens/overview.jsx:248
  # source: pro:src/live.js:783-784
  Scenario: A pending count the hub does not state is shown as unknown
    Given the hub's facts say the request list is available but state no pending count
    And the cluster's own list shows 5 approved requests
    Then the "Kubelet-serving CSRs" row reads "5 approved · pending unknown" without a warning tone

  @pro
  # source: pro:src/screens/overview.jsx:241,249
  Scenario: The cluster card says when the hub observed its facts
    Given the hub observed the cluster's facts at 14:05:09
    Then the "Facts observed" row shows that time in the browser's local time format
    And "—" when no hub observation time is known

  @pro
  # source: pro:src/screens/overview.jsx:235-238
  Scenario: The cluster card makes no compliance claim
    When the Overview is shown
    Then the cluster card shows observed facts only, with no pass, fail or benchmark badge
