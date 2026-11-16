Feature: Derived pod values
  The status, readiness, restart count and usage every pod row shows, whether
  the row comes from the cluster snapshot or from the Pods list.

  # -------------------------------------------------------------------- status

  @oss @pro
  # source: oss:src/k8s-rules.js:15-29
  # source: pro:src/k8srules.js:63-81
  Scenario Outline: A pod's status is the first rule that matches
    Given a pod with <condition>
    Then its status is "<status>"

    Examples:
      | condition                                                              | status           |
      | a deletion timestamp, whatever else is true                            | Terminating      |
      | a deletion timestamp and a container waiting in CrashLoopBackOff       | Terminating      |
      | a container waiting with reason "CrashLoopBackOff"                     | CrashLoopBackOff |
      | a container waiting with reason "ErrImagePull"                         | Failed           |
      | a container waiting with reason "ImagePullBackOff"                     | Failed           |
      | a container waiting with reason "CreateContainerConfigError"           | Failed           |
      | a container waiting with reason "InvalidImageName"                     | Failed           |
      | a container waiting with reason "RunContainerError"                    | Failed           |
      | a container waiting with reason "ContainerCreating" and phase Pending  | Pending          |
      | a container waiting with reason "PodInitializing" and phase Pending    | Pending          |
      | phase "Running" and no waiting container                               | Running          |
      | phase "Succeeded"                                                      | Succeeded        |
      | phase "Failed"                                                         | Failed           |
      | phase "Unknown"                                                        | Unknown          |
      | no reported status at all                                              | Pending          |

  @oss @pro
  # source: oss:src/k8s-rules.js:21-27
  # source: pro:src/k8srules.js:65-71
  Scenario: The first waiting container with a recognised reason decides
    Given a pod whose first container waits with "ImagePullBackOff" and whose second waits with "CrashLoopBackOff"
    Then its status is "Failed"

  @oss @pro
  # source: oss:src/k8s-rules.js:21
  # source: pro:src/k8srules.js:65
  # assumption: only regular container statuses are examined; init container statuses are not read
  Scenario: A crash-looping init container does not change the status
    Given a pod in phase "Pending" whose init container waits with "CrashLoopBackOff"
    And whose regular containers have reported no waiting reason
    Then its status is "Pending"

  @diverged @pro
  # source: pro:src/k8srules.js:72-80
  Scenario: A pod the scheduler cannot place is Unschedulable
    Given a pod in phase "Pending"
    And its "PodScheduled" condition is "False" with reason "Unschedulable"
    Then its status is "Unschedulable"

  @diverged @oss @drop
  # decision: D1 — the pro variant (Unschedulable) stands
  # source: oss:src/k8s-rules.js:28
  Scenario: A pod the scheduler cannot place is Pending
    Given a pod in phase "Pending"
    And its "PodScheduled" condition is "False" with reason "Unschedulable"
    Then its status is "Pending"

  @pro
  # source: pro:src/k8srules.js:76-78
  Scenario: Unschedulable applies only while the pod is Pending
    Given a pod in phase "Running" that still carries a "PodScheduled" condition of "False" with reason "Unschedulable"
    Then its status is "Running"

  # ----------------------------------------------------------------- readiness

  @oss @pro
  # source: oss:src/k8s-rules.js:31-37
  # source: pro:src/k8srules.js:55-59
  Scenario Outline: A pod's ready count is ready containers over total containers
    Given a pod with <containers>
    Then its ready count is "<ready>"

    Examples:
      | containers                                                        | ready |
      | two reported containers, both ready                               | 2/2   |
      | two reported containers, one ready                                | 1/2   |
      | three declared containers and no reported container status yet    | 0/3   |
      | no declared containers and no reported status                     | 0/0   |

  @oss @pro
  # source: oss:src/live.js:103
  # source: pro:src/k8srules.js:61
  Scenario: A pod's restarts are the sum over its containers
    Given a pod whose containers have restarted 3 times and 2 times
    Then its restart count is 5
    And a pod with no reported container status has a restart count of 0

  # --------------------------------------------------------------------- usage

  @oss @pro
  # source: oss:src/live.js:306-313
  # source: pro:src/live.js:612-619
  Scenario: A pod shows measured usage when its kubelet reported it
    Given a pod requests 100 millicores and 128 Mi
    And its kubelet reports the pod using 37.6 millicores and 210.4 Mi
    Then the pod row shows 38 millicores and 210 Mi

  @oss @pro
  # source: oss:src/live.js:306-313
  # source: pro:src/live.js:612-619
  Scenario: A pod falls back to its requests when no usage was reported for it
    Given a pod requests 100 millicores and 128 Mi
    And its node's kubelet did not answer
    Then the pod row shows 100 millicores and 128 Mi

  @diverged @oss
  # source: oss:src/live.js:63-64,193-199
  Scenario: A pod the kubelet lists without figures falls back to its requests
    Given a pod requests 100 millicores and 128 Mi
    And its kubelet lists the pod but reports no CPU or memory figures for it yet
    Then the pod row shows 100 millicores and 128 Mi

  @diverged @pro @drop
  # decision: D1, D2 — the oss variant (fall back to requests) stands (NOTES P7)
  # source: pro:src/live.js:489-494,614-617
  Scenario: A pod the kubelet lists without figures shows zero usage
    Given a pod requests 100 millicores and 128 Mi
    And its kubelet lists the pod but reports no CPU or memory figures for it yet
    Then the pod row shows 0 millicores and 0 Mi

  @oss @pro
  # source: oss:src/live.js:309-312
  # source: pro:src/live.js:615-618
  Scenario Outline: Missing pod details are shown as a dash
    Given a pod with no <detail>
    Then the pod row shows "—" for its <column>

    Examples:
      | detail           | column    |
      | assigned node    | node      |
      | pod IP           | IP        |
      | QoS class        | QoS class |
