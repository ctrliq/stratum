Feature: Derived workload values
  One row per Deployment, StatefulSet, DaemonSet, Job and CronJob, with a
  ready count, a status, an image and a usage figure. These rows feed the
  Workloads screen and the Overview's workload health card.

  # --------------------------------------------------------------- deployments

  @oss @pro
  # source: oss:src/k8s-rules.js:52-60
  # source: oss:src/live.js:322-323
  # source: pro:src/live.js:633-638
  Scenario Outline: A Deployment's ready count is ready replicas over desired replicas
    Given no hub's facts were loaded for the cluster
    And a Deployment that wants <desired> replicas and has <ready> ready and <available> available
    Then its ready count is "<shown>"

    Examples:
      | desired | ready | available | shown |
      | 3       | 3     | 3         | 3/3   |
      | 3       | 3     | 1         | 3/3   |
      | 3       | 1     | 1         | 1/3   |
      | 0       | 0     | 0         | 0/0   |

  @diverged @oss @drop
  # decision: D1, D2 — the pro variant stands (NOTES O1)
  # source: oss:src/live.js:105,322-323
  Scenario Outline: A Deployment's status follows its available replicas alone
    Given a Deployment that wants <desired> replicas and has <ready> ready and <available> available
    Then its status is "<status>"

    Examples:
      | desired | ready | available | status      |
      | 3       | 3     | 3         | Running     |
      | 3       | 3     | 1         | Progressing |
      | 3       | 3     | 0         | Degraded    |
      | 3       | 1     | 1         | Progressing |
      | 3       | 0     | 0         | Degraded    |
      | 0       | 0     | 0         | Scheduled   |

  @diverged @pro
  # decision: D1 — also the rule on the dashboard server backend, where no hub answers
  # source: pro:src/live.js:176-184,633-638
  Scenario Outline: Without hub facts a Deployment's status needs replicas both ready and available
    Given no hub answers for the cluster
    And a Deployment that wants <desired> replicas and has <ready> ready and <available> available
    Then its status is "<status>"

    Examples:
      | desired | ready | available | status      |
      | 3       | 3     | 3         | Running     |
      | 3       | 3     | 1         | Progressing |
      | 3       | 3     | 0         | Progressing |
      | 3       | 1     | 1         | Progressing |
      | 3       | 0     | 0         | Degraded    |
      | 0       | 0     | 0         | Scheduled   |

  @pro
  # source: pro:src/live.js:628-638
  Scenario: With hub facts the hub's numbers and status are shown for a Deployment
    Given the hub's facts list the Deployment "shop/web" with 4 replicas, 2 ready, 2 available and status "Progressing"
    And the Deployment object itself reports 4 ready replicas
    Then the row shows a ready count of "2/4" and the status "Progressing"
    And the row carries the time the hub observed the facts

  @pro
  # source: pro:src/live.js:634-638
  Scenario: With hub facts a Deployment the hub does not list has no verdict
    Given the hub's facts were loaded and do not list the Deployment "shop/new"
    Then the row for "shop/new" shows a ready count of "—" and the status "Unknown"

  # ------------------------------------------------ statefulsets and daemonsets

  @oss @pro
  # source: oss:src/live.js:105,324-327
  # source: pro:src/live.js:281,640-643
  Scenario Outline: StatefulSets and DaemonSets take their status from ready over desired
    Given a <kind> with <desired> desired and <ready> ready
    Then its ready count is "<shown>" and its status is "<status>"

    Examples:
      | kind        | desired | ready | shown | status      |
      | StatefulSet | 3       | 3     | 3/3   | Running     |
      | StatefulSet | 3       | 2     | 2/3   | Progressing |
      | StatefulSet | 3       | 0     | 0/3   | Degraded    |
      | StatefulSet | 0       | 0     | 0/0   | Scheduled   |
      | DaemonSet   | 5       | 5     | 5/5   | Running     |
      | DaemonSet   | 5       | 4     | 4/5   | Progressing |
      | DaemonSet   | 5       | 0     | 0/5   | Degraded    |
      | DaemonSet   | 0       | 0     | 0/0   | Scheduled   |

  @oss @pro
  # source: oss:src/live.js:326-327
  # source: pro:src/live.js:642-643
  Scenario: A DaemonSet's desired count is the number of nodes it should run on
    Given a DaemonSet scheduled on 5 nodes with 4 pods ready
    Then its desired count is 5 and its ready count is "4/5"

  # ---------------------------------------------------------- jobs and cronjobs

  @oss @pro
  # source: oss:src/k8s-rules.js:39-50
  # source: oss:src/live.js:328-329
  # source: pro:src/live.js:644-645
  Scenario Outline: A Job's status comes from its succeeded, failed and active counters
    Given a Job wanting <completions> completions with <succeeded> succeeded, <failed> failed and <active> active
    Then its status is "<status>" and its ready count is "<shown>"

    Examples:
      | completions | succeeded | failed | active | status    | shown |
      | not stated  | 1         | 0      | 0      | Complete  | 1/1   |
      | 3           | 3         | 2      | 0      | Complete  | 3/3   |
      | 3           | 1         | 1      | 1      | Failed    | 1/3   |
      | 3           | 1         | 0      | 2      | Running   | 1/3   |
      | not stated  | 0         | 0      | 0      | Scheduled | 0/1   |

  @oss @pro
  # source: oss:src/live.js:330
  # source: pro:src/live.js:646
  Scenario: A CronJob is always Scheduled and carries its schedule
    Given a CronJob with the schedule "*/5 * * * *"
    Then its status is "Scheduled", its ready count is "—" and its schedule is "*/5 * * * *"

  # --------------------------------------------------------------------- image

  @oss @pro
  # source: oss:src/live.js:104
  # source: pro:src/live.js:280
  Scenario: A workload shows the image of its first container
    Given a Deployment whose pod template lists the containers "app" (image "shop:1.4") and "sidecar" (image "envoy:1.30")
    Then its image is "shop:1.4"
    And a workload whose template lists no containers shows "—"

  @oss @pro
  # source: oss:src/live.js:330
  # source: pro:src/live.js:646
  Scenario: A CronJob's image is read from its job template
    Given a CronJob whose job template's first container uses the image "backup:2"
    Then its image is "backup:2"

  # --------------------------------------------------------------------- usage

  @oss @pro
  # source: oss:src/live.js:334-336
  # source: pro:src/live.js:650-652
  Scenario: A workload's usage is the sum over pods in its namespace whose name starts with its name
    Given the Deployment "web" in the namespace "shop"
    And measured pods "shop/web-7c9d-abcde" using 40 millicores and "shop/web-7c9d-fghij" using 60 millicores
    And a measured pod "other/web-1" using 500 millicores
    Then the workload "web" shows 100 millicores

  @oss @pro
  # source: oss:src/live.js:336
  # source: pro:src/live.js:652
  Scenario: A pod named exactly like the workload counts towards it
    Given the workload "etcd" in "kube-system" and a measured pod "kube-system/etcd" using 80 millicores
    Then the workload "etcd" shows 80 millicores

  @oss @pro @drop
  # decision: D2 — usage counted towards a workload that does not own the pod (NOTES B1)
  # source: oss:src/live.js:336
  # source: pro:src/live.js:652
  Scenario: Workloads whose names share a prefix also share usage
    Given the Deployments "web" and "web-api" in the namespace "shop"
    And a measured pod "shop/web-api-5d8f-xyz12" using 70 millicores
    Then the workload "web-api" shows 70 millicores
    And the workload "web" also includes those 70 millicores

  @oss @pro @fixed
  # decision: D2
  # source: oss:src/live.js:336
  # source: pro:src/live.js:652
  Scenario: A pod's usage counts only towards the workload it belongs to
    Given the Deployments "web" and "web-api" in the namespace "shop"
    And a measured pod "shop/web-api-5d8f-xyz12" using 70 millicores
    Then the workload "web-api" shows 70 millicores
    And the workload "web" does not include them

  @oss @pro @drop
  # decision: D2 — a restart count of 0 is shown for workloads whose pods restarted (NOTES B9)
  # source: oss:src/live.js:323-330
  # source: pro:src/live.js:638-646
  Scenario: Workload rows carry no restart count
    Given a Deployment whose pods have restarted 12 times in total
    Then the workload row's restart count is 0

  @oss @pro @fixed
  # decision: D2
  # source: oss:src/live.js:323-330
  # source: pro:src/live.js:638-646
  Scenario: A workload row never states zero restarts for pods that restarted
    Given a Deployment whose pods have restarted 12 times in total
    Then the workload row shows 12 restarts, or no restart figure at all, never 0

  @oss @pro
  # source: oss:src/live.js:321-330
  # source: pro:src/live.js:627-646
  Scenario: Workload rows are ordered by kind
    When a load builds the workload rows
    Then Deployments come first, then StatefulSets, DaemonSets, Jobs and CronJobs
    And within a kind the rows keep the order the cluster returned
