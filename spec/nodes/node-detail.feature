Feature: Node detail
  The detail pane of one node: status, capacity, system information,
  conditions, labels and taints, and the pods running on it.

  # source: oss:src/screens/infra.jsx:227-233
  # source: pro:src/screens/infra.jsx:533-539
  @oss @pro
  Scenario: The header identifies the node
    Given node "cp-1" is a control-plane node in zone "eu-1a", cordoned, created 3 days ago
    When its detail pane is open
    Then the title is "cp-1" and the subtitle is "control-plane · eu-1a"
    And the node's status is shown
    And a "Cordoned" badge is shown
    And the age reads "up 3d"

  # source: oss:src/screens/infra.jsx:231
  # source: pro:src/screens/infra.jsx:537
  @oss @pro
  Scenario: A schedulable node shows no Cordoned badge
    Given a node that is not cordoned
    When its detail pane is open
    Then no "Cordoned" badge is shown

  # --------------------------------------------------------------- capacity

  # source: oss:src/screens/infra.jsx:236-244,307-317
  # source: pro:src/screens/infra.jsx:542-550,618-628
  # decision: D2
  @oss @pro @drop
  Scenario: Capacity shows CPU, memory and pods
    Given a node at 42% CPU and 61% memory with 10 pods of a capacity of 110
    When its detail pane is open
    Then the CPU row reads "42%" followed by the node's instance type and the text "vCPU"
    And the Memory row reads "61% · of allocatable"
    And the Pods row reads "10/110"

  # source: oss:src/screens/infra.jsx:236-244,307-317
  # source: pro:src/screens/infra.jsx:542-550,618-628
  # decision: D2
  # assumption: the intended CPU detail is the node's allocatable CPU count; the legacy text puts the instance type in front of "vCPU"
  @fixed @oss @pro
  Scenario: Capacity shows CPU with a core count, memory and pods
    Given a node with 8 allocatable CPUs at 42% CPU and 61% memory with 10 pods of a capacity of 110
    When its detail pane is open
    Then the CPU row reads "42% · 8 vCPU"
    And the Memory row reads "61% · of allocatable"
    And the Pods row reads "10/110"

  # source: oss:src/screens/infra.jsx:19,314
  # source: pro:src/screens/infra.jsx:30,625
  @oss @pro
  Scenario Outline: Capacity bars change colour with the percentage
    Given a node at <pct>% CPU
    When its detail pane is open
    Then the CPU bar uses the <colour> colour

    Examples:
      | pct | colour  |
      | 70  | success |
      | 71  | warning |
      | 85  | warning |
      | 86  | error   |

  # source: oss:src/screens/infra.jsx:239-240
  # decision: D1
  @diverged @oss
  Scenario: Capacity reads 0% for any node that is not ready
    Given a node whose Ready condition is not "True"
    When its detail pane is open
    Then the CPU and Memory rows read "0%"

  # source: pro:src/screens/infra.jsx:545-546
  # decision: D1
  @diverged @pro @drop
  Scenario: Capacity reads 0% only for a NotReady node
    Given a node whose status is "NotReady"
    When its detail pane is open
    Then the CPU and Memory rows read "0%"
    And a node whose status is "Unknown" still shows its percentages

  # ------------------------------------------------------------ system info

  # source: oss:src/screens/infra.jsx:247-257
  # source: oss:src/live.js:228-233
  # decision: D1
  @diverged @oss @drop
  Scenario: System info lists seven facts read from the node
    When a node's detail pane is open
    Then the System info section lists Instance type, Zone, Kubelet, OS image, Stratum k8s-version, Container runtime and Kernel
    And the Kubelet value is the kubelet version exactly as the node reports it
    And any fact the node does not report shows "—"

  # source: pro:src/screens/infra.jsx:553-567
  # source: pro:src/live.js:536-551
  # decision: D2
  @diverged @pro @drop
  Scenario: System info lists nine facts including architecture and accelerators
    When a node's detail pane is open
    Then the System info section lists Instance type, Zone, Kubelet, OS image, Stratum k8s-version, Container runtime, Kernel, Architecture and Accelerators
    And the Kubelet value is the letter "v" followed by the kubelet version as the node reports it
    And any fact the node does not report shows "—"

  # source: oss:src/screens/infra.jsx:247-257
  # source: pro:src/screens/infra.jsx:553-567
  # decision: D1, D2
  @fixed @oss @pro
  Scenario: System info lists nine facts and shows the kubelet version as reported
    When a node's detail pane is open
    Then the System info section lists Instance type, Zone, Kubelet, OS image, Stratum k8s-version, Container runtime, Kernel, Architecture and Accelerators
    And the Kubelet value is the kubelet version exactly as the node reports it, with no added prefix
    And any fact the node does not report shows "—"

  # source: pro:src/live.js:542-548
  # source: pro:src/screens/infra.jsx:564-566
  # decision: D1
  @pro
  Scenario: Accelerators lists every extended resource in the node's capacity
    Given a node whose capacity includes "nvidia.com/gpu: 4" and "amd.com/gpu: 1"
    When its detail pane is open
    Then Accelerators reads "nvidia.com/gpu ×4, amd.com/gpu ×1"
    And a node whose capacity holds no resource name containing "/" reads "none"

  # ------------------------------------------------------------- conditions

  # source: oss:src/screens/infra.jsx:205-215,261-269
  # source: oss:src/live.js:238
  # decision: D1
  @diverged @oss
  Scenario Outline: Conditions show what the node itself reports
    Given a node that reports the condition <type> as <reported>
    When its detail pane is open
    Then the <type> row shows "<shown>" in the <colour> colour

    Examples:
      | type               | reported   | shown   | colour  |
      | Ready              | True       | True    | success |
      | Ready              | False      | False   | warning |
      | Ready              | Unknown    | Unknown | gray    |
      | MemoryPressure     | False      | False   | success |
      | MemoryPressure     | True       | True    | warning |
      | DiskPressure       | not at all | —       | gray    |
      | PIDPressure        | Unknown    | Unknown | gray    |
      | NetworkUnavailable | False      | False   | success |

  # source: oss:src/screens/infra.jsx:264-268
  @oss
  Scenario: Exactly five conditions are listed
    When a node's detail pane is open
    Then the Conditions section lists Ready, MemoryPressure, DiskPressure, PIDPressure and NetworkUnavailable in that order
    And any other condition the node reports is not listed

  # source: pro:src/screens/infra.jsx:518-521,571-579
  # decision: D1, D2
  @diverged @pro @drop
  Scenario Outline: Conditions are inferred from the node's status, not read from the node
    Given a node whose status is "<status>"
    When its detail pane is open
    Then the Ready row shows "<ready>"
    And the NetworkUnavailable row shows "<net>"
    And the MemoryPressure, DiskPressure and PIDPressure rows always show "False"

    Examples:
      | status   | ready | net   |
      | Ready    | False | False |
      | NotReady | True  | True  |
      | Unknown  | True  | False |

  # ------------------------------------------------------ labels and taints

  # source: oss:src/screens/infra.jsx:273-281
  # source: oss:src/live.js:239
  # decision: D1
  @diverged @oss
  Scenario: Labels and taints are the node's own
    Given a node with the labels "tier=edge" and "spot" (empty value) and the taint "dedicated=gpu:NoSchedule"
    When its detail pane is open
    Then the badges "tier=edge" and "spot" are shown
    And the taint badge "dedicated=gpu:NoSchedule" is shown in the warning colour
    And a node with no taints shows a "no taints" badge

  # source: pro:src/screens/infra.jsx:583-591
  # decision: D1, D2
  @diverged @pro @drop
  Scenario: Labels and taints are a fixed set derived from role, zone, instance type and cordon
    Given a cordoned control-plane node in zone "eu-1a" of instance type "m5.large"
    When its detail pane is open
    Then the badges "kubernetes.io/role=control-plane", "topology.kubernetes.io/zone=eu-1a" and "node.kubernetes.io/instance-type=m5.large" are shown
    And the badge "node.kubernetes.io/unschedulable:NoSchedule" is shown because the node is cordoned
    And the badge "control-plane:NoSchedule" is shown because the node is a control-plane node
    And none of the node's other labels or taints are shown

  # ------------------------------------------------------------------- pods

  # source: oss:src/screens/infra.jsx:204,284-297
  # source: pro:src/screens/infra.jsx:517,595-608
  @oss @pro
  Scenario: Pods on the node are listed with their status
    Given 3 loaded pods are scheduled on node "worker-1"
    When the detail pane of "worker-1" is open
    Then the section header reads "3 total"
    And each pod is listed by name with its status
    And pods from every namespace are included regardless of the selected namespace

  # source: oss:src/screens/infra.jsx:287,298-300
  # decision: D1
  @diverged @oss
  Scenario: With no loaded pods for the node, the count falls back and a notice is shown
    Given no loaded pod is scheduled on node "worker-1" and the node row counts 7 pods
    When the detail pane of "worker-1" is open
    Then the section header reads "7 total"
    And the text "Pod list not loaded for this node." is shown

  # source: pro:src/screens/infra.jsx:598,609-611
  # decision: D1
  @diverged @pro @drop
  Scenario: With no loaded pods for the node, the count falls back and a prototype notice is shown
    Given no loaded pod is scheduled on node "worker-1" and the node row counts 7 pods
    When the detail pane of "worker-1" is open
    Then the section header reads "7 total"
    And the text "Pod list not loaded for this node in the prototype." is shown

  # ---------------------------------------------------------------- actions

  # source: oss:src/screens/infra.jsx:216-225
  # source: oss:src/session.js:144-156
  # decision: D14, D16
  @diverged @oss
  Scenario: The footer offers host shell and cordon to a session that can write
    Given a signed-in user, or a server that requires no sign-in
    And the server has the node shell feature enabled
    When the detail pane of a ready, schedulable node is open
    Then the footer offers "Host shell" and "Cordon"

  # source: oss:src/screens/infra.jsx:216,225
  # source: oss:src/session.js:144-156
  # decision: D14
  @diverged @oss
  Scenario: The footer is absent for a session that cannot write
    Given the server requires sign-in and there is no signed-in identity, or the server configuration could not be read
    When a node's detail pane is open
    Then the pane has no footer actions

  # source: pro:src/screens/infra.jsx:522-531
  # source: pro:src/fleet.js:48-55
  # decision: D16, D17
  @diverged @pro @drop
  Scenario: The footer offers host shell and cordon to an admin
    Given a user with the "admin" role, or a standalone server that requires no sign-in
    When the detail pane of a Ready, schedulable node is open
    Then the footer offers "Host shell" and "Cordon & Drain"

  # source: pro:src/screens/infra.jsx:522-531
  # source: pro:src/fleet.js:48-55
  # decision: D14, D16, D17
  @fixed @pro @fleet
  Scenario: On the hub the footer offers Cordon to an admin and a host shell only when the backend enables it
    Given a user with the "admin" role
    When the detail pane of a Ready, schedulable node is open
    Then the footer offers "Cordon"
    And "Host shell" is offered only when the backend's configuration reports the node shell feature as enabled

  # source: pro:src/screens/infra.jsx:522,531
  # source: pro:src/fleet.js:48-55
  # decision: D14
  @diverged @pro
  Scenario: The footer is absent for a user without the admin role
    Given a signed-in user whose roles do not include "admin"
    When a node's detail pane is open
    Then the pane has no footer actions

  # source: oss:src/screens/infra.jsx:221-223
  # source: pro:src/screens/infra.jsx:527-529
  @oss @pro
  Scenario: A cordoned node offers Uncordon as the primary action
    Given a user allowed to write
    When the detail pane of a cordoned node is open
    Then the footer offers "Uncordon" as the emphasised action instead of the cordon action
