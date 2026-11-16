Feature: Workload kinds and their list columns
  Pods, Deployments, StatefulSets, DaemonSets, ReplicaSets, Replication
  Controllers, Jobs and CronJobs each have a list built on the generic
  resource list (see spec/resources). This feature records what is specific
  to each: its route, its columns and how every column value is derived.

  @oss @pro
  # source: oss:src/resources.jsx:31,44,50,54,59,64,69,74
  # source: oss:src/registry.js:36-46
  # source: oss:src/app.jsx:491
  # source: pro:src/resources.jsx:47,60,69,73,78,83,88,93
  # source: pro:src/nav.js:10-20
  # source: pro:src/app.jsx:514
  Scenario Outline: Each workload kind has its own list
    When the user opens the route "<route>" or its row in the sidebar's "Workloads" section
    Then the generic list for kind "<kind>" is shown, titled "<title>"
    And it is a namespaced kind

    Examples:
      | route                  | kind                  | title                   |
      | pods                   | Pod                   | Pods                    |
      | deployments            | Deployment            | Deployments             |
      | statefulsets           | StatefulSet           | StatefulSets            |
      | daemonsets             | DaemonSet             | DaemonSets              |
      | replicasets            | ReplicaSet            | ReplicaSets             |
      | replicationcontrollers | ReplicationController | Replication Controllers |
      | jobs                   | Job                   | Jobs                    |
      | cronjobs               | CronJob               | CronJobs                |

  @oss @pro
  # source: oss:src/resources.jsx:37-79
  # source: pro:src/resources.jsx:53-98
  Scenario Outline: Each workload kind shows its columns in order
    When the "<route>" list is shown
    Then its columns are, in order: <columns>

    Examples:
      | route                  | columns                                                  |
      | pods                   | Name, Namespace, Status, Ready, Restarts, Node, Age      |
      | deployments            | Name, Namespace, Ready, Up-to-date, Available, Age       |
      | statefulsets           | Name, Namespace, Ready, Age                              |
      | daemonsets             | Name, Namespace, Desired, Ready, Age                     |
      | replicasets            | Name, Namespace, Desired, Ready, Age                     |
      | replicationcontrollers | Name, Namespace, Desired, Ready, Age                     |
      | jobs                   | Name, Namespace, Completions, Status, Age                |
      | cronjobs               | Name, Namespace, Schedule, Suspend, Active, Age          |

  # ---- column values identical in both dashboards ---------------------------

  @oss @pro
  # source: oss:src/resources.jsx:40-42,47,50-79
  # source: oss:src/k8s-rules.js:33-37,56-60
  # source: pro:src/resources.jsx:56-58,66,69-98
  # source: pro:src/k8srules.js:55-61
  Scenario Outline: A workload column shows a value read from the object
    Given the "<route>" list is shown
    Then the "<column>" column shows <value>

    Examples:
      | route                  | column      | value                                                                                  |
      | pods                   | Ready       | containers reporting ready over containers reported, as "<ready>/<total>"              |
      | pods                   | Restarts    | the sum of every reported container's restart count                                    |
      | pods                   | Node        | the node the pod is assigned to, or "—" when unassigned                                |
      | deployments            | Up-to-date  | the updated replica count from status, or 0 when absent                                |
      | statefulsets           | Ready       | ready replicas from status over desired replicas from spec, each 0 when absent         |
      | daemonsets             | Desired     | the desired scheduled count from status, or 0 when absent                              |
      | daemonsets             | Ready       | the ready count from status, or 0 when absent                                          |
      | replicasets            | Desired     | desired replicas from spec, or 0 when absent                                           |
      | replicasets            | Ready       | ready replicas from status, or 0 when absent                                           |
      | replicationcontrollers | Desired     | desired replicas from spec, or 1 when the field is absent                              |
      | replicationcontrollers | Ready       | ready replicas from status, or 0 when absent                                           |
      | jobs                   | Completions | succeeded pods over wanted completions, as "<succeeded>/<completions>"                 |
      | cronjobs               | Schedule    | the cron expression from spec                                                          |
      | cronjobs               | Suspend     | "true" when the cron job is suspended, otherwise "false"                               |
      | cronjobs               | Active      | the number of jobs currently listed as active in status                                |

  @oss @pro
  # source: oss:src/k8s-rules.js:33-37
  # source: pro:src/k8srules.js:55-59
  Scenario: A pod with no reported containers counts its declared containers
    Given a pod for which no container status has been reported yet
    And its spec declares 2 containers
    Then its Ready column reads "0/2"

  @oss @pro
  # source: oss:src/resources.jsx:71
  # source: pro:src/resources.jsx:90
  Scenario Outline: Job completions default to one wanted completion
    Given a job with <succeeded> succeeded pods and <completions> in its spec
    Then its Completions column reads "<text>"

    Examples:
      | succeeded | completions     | text |
      | no        | no completions  | 0/1  |
      | 1         | no completions  | 1/1  |
      | 2         | completions 5   | 2/5  |

  @oss @pro
  # source: oss:src/k8s-rules.js:43-50
  # source: oss:src/resources.jsx:72
  # source: pro:src/resources.jsx:91
  Scenario Outline: Job status is derived from the job's pod counters
    Given a job wanting <completions> completions with <succeeded> succeeded, <failed> failed and <active> active pods
    Then its Status badge reads "<status>"

    Examples:
      | completions | succeeded | failed | active | status    |
      | 1           | 1         | 0      | 0      | Complete  |
      | 3           | 3         | 1      | 0      | Complete  |
      | 3           | 1         | 1      | 1      | Failed    |
      | 3           | 1         | 0      | 2      | Running   |
      | 1           | 0         | 0      | 0      | Scheduled |

  @oss @pro
  # source: oss:src/k8s-rules.js:19-29
  # source: pro:src/k8srules.js:63-72,80
  Scenario Outline: Pod status ranks deletion, then stuck containers, then phase
    Given a pod that <condition>
    Then its Status badge reads "<badge>"

    Examples:
      | condition                                                           | badge       |
      | is being deleted, whatever else is true                             | Terminating |
      | has a container waiting with reason "CrashLoopBackOff"              | CrashLoop   |
      | has a container waiting with reason "ErrImagePull"                  | Failed      |
      | has a container waiting with reason "ImagePullBackOff"              | Failed      |
      | has a container waiting with reason "InvalidImageName"              | Failed      |
      | has a container waiting with reason "CreateContainerConfigError"    | Failed      |
      | is in phase "Pending" with a container waiting on "ContainerCreating" | Pending   |
      | reports phase "Running" with no stuck container                     | Running     |
      | reports phase "Succeeded"                                           | Succeeded   |
      | reports no phase yet                                                | Pending     |

  # ---- column values that differ ---------------------------------------------

  @diverged @oss @drop
  # decision: D1 — pro's Unschedulable badge stands
  # source: oss:src/k8s-rules.js:19-29
  Scenario: A pod the scheduler cannot place reads as Pending
    Given a pod in phase "Pending" whose scheduling condition is false with reason "Unschedulable"
    Then its Status badge reads "Pending"

  @diverged @pro
  # source: pro:src/k8srules.js:72-80
  # source: pro:src/ui/shell.jsx:70
  Scenario: A pod the scheduler cannot place reads as Unschedulable
    Given a pod in phase "Pending" whose scheduling condition is false with reason "Unschedulable"
    Then its Status badge reads "Unschedulable" in the warning colour

  @diverged @oss
  # source: oss:src/resources.jsx:46
  # source: oss:src/k8s-rules.js:56-60
  Scenario: Deployment Ready is read from the deployment in the list
    Given the Deployments list is shown
    Then each row's Ready column reads ready replicas from status over desired replicas from spec, each 0 when absent
    And the value is as fresh as the list's own last refresh

  @diverged @pro @drop
  # decision: D1 — the listed object is the source, as in oss
  # source: pro:src/resources.jsx:22-40,62-65
  # source: pro:src/ui/resource-list.jsx:179
  # source: pro:src/live.js:622-638
  Scenario: Deployment Ready is read from the dashboard's cluster snapshot
    Given the Deployments list is shown
    Then each row's Ready column shows the ready figure the cluster snapshot holds for the deployment with the same namespace and name
    And when the snapshot records when that figure was observed, hovering the value shows "Observed <time>"
    And the value changes only when the snapshot refreshes, not when the list does

  @pro @drop
  # decision: D1, D2 — consequence of the dropped snapshot variant; the oss Ready and Available scenarios state the corrected behavior
  # source: pro:src/resources.jsx:25-27,39,64,67
  Scenario: A deployment the snapshot does not know shows dashes
    Given the Deployments list holds a deployment that the cluster snapshot does not contain, or no snapshot has loaded
    Then that row's Ready and Available columns read "—"
    And its Up-to-date column still shows the count from the listed object

  @pro @drop
  # decision: D1, D2 — consequence of the dropped snapshot variant; every column of a row comes from the same read
  # source: pro:src/resources.jsx:62-68
  # source: pro:src/app.jsx:270-299
  # source: pro:src/ui/resource-list.jsx:86-101
  Scenario: Pausing auto-refresh freezes Ready and Available but not the rest of the row
    Given the Deployments list is shown and the user pauses auto-refresh
    When a deployment's replicas change on the cluster
    Then the row's Up-to-date and Age keep updating with the list
    But its Ready and Available stay at the snapshot's last values until auto-refresh resumes or the user refreshes

  @diverged @oss
  # source: oss:src/resources.jsx:48
  Scenario: Deployment Available is read from the deployment in the list
    Given the Deployments list is shown
    Then each row's Available column shows the available replica count from status, or 0 when absent

  @diverged @pro @drop
  # decision: D1 — the listed object is the source, as in oss
  # source: pro:src/resources.jsx:67
  # source: pro:src/live.js:634-638
  Scenario: Deployment Available is read from the dashboard's cluster snapshot
    Given the Deployments list is shown
    Then each row's Available column shows the available figure the cluster snapshot holds for that deployment

  # ---- what each kind lets the user do ---------------------------------------

  @oss @pro
  # source: oss:src/resources.jsx:36,240-241,254-257
  # source: pro:src/resources.jsx:52,257-258,271-274
  Scenario Outline: Kind-specific actions offered in the detail panel
    Given the session is offered write controls
    When a detail panel is open for a <kind>
    Then besides Edit and Delete it offers <extra>

    Examples:
      | kind        | extra             |
      | Pod         | Logs and Exec     |
      | Deployment  | Scale and Restart |
      | StatefulSet | Scale and Restart |
      | DaemonSet   | Restart           |
      | ReplicaSet  | Scale             |
      | Job         | nothing more      |
      | CronJob     | nothing more      |

  @diverged @oss
  # decision: D15 — both variants stand: this one on the dashboard server backend
  # source: oss:src/resources.jsx:240,254-257
  Scenario: Replication controllers can be created, edited, scaled and deleted
    Given the session is offered write controls
    When the Replication Controllers list is open
    Then Create is offered
    And a row's detail panel offers Edit, Scale and Delete

  @diverged @pro @fleet
  # decision: D15 — both variants stand: this one on the hub backend
  # source: pro:src/resources.jsx:257,270-274
  # source: pro:src/ui/resource-list.jsx:158
  Scenario: Replication controllers are read-only
    Given the signed-in user has the admin role
    When the Replication Controllers list is open
    Then no Create button is shown
    And a row's detail panel offers no Edit, Scale or Delete
