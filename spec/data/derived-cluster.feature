Feature: Derived cluster-wide values
  Totals and facts computed across the whole cluster on every load: usage
  totals and their short history, events, storage classes, Helm releases,
  namespaces, control-plane facts, the operator's state and the cluster's own
  identity.

  # ---------------------------------------------------- usage totals + history

  @oss @pro
  # source: oss:src/live.js:259-282
  # source: pro:src/live.js:563-587
  Scenario: Cluster usage totals are measured usage over allocatable capacity
    Given two nodes, each with allocatable CPU "4" and memory "16Gi"
    And their kubelets report 1000 and 3000 millicores, and 4096 and 12288 Mi, in use
    Then the cluster totals are 4 of 8 cores and 16 of 32 Gi in use
    And the cluster shows 50% CPU and 50% memory

  @oss @pro
  # source: oss:src/live.js:267-268
  # source: pro:src/live.js:571-572
  Scenario: Cluster percentages are capped at 100 and absent without capacity
    Given measured usage exceeds the nodes' allocatable capacity
    Then the cluster percentage is 100
    And when the nodes state no allocatable capacity, the percentage is absent rather than 0

  @diverged @oss
  # source: oss:src/live.js:247-259
  Scenario: Cluster totals need a usage sample from every node
    Given three nodes of which only two kubelets answered
    Then the cluster totals and their history keep the previous complete sample
    And no partial sample is added to the history

  @diverged @pro @drop
  # decision: D1 — the oss variant (a sample from every node) stands
  # source: pro:src/live.js:556-563
  Scenario: Cluster totals cover only the nodes whose kubelet answered
    Given three nodes of which only two kubelets answered
    Then the cluster totals count 2 nodes, with usage and capacity summed over those two
    And that sample is added to the history

  @oss @pro
  # source: oss:src/live.js:254-255,259
  # source: pro:src/live.js:563
  Scenario: With no usage sample at all the totals keep their previous value
    Given nodes exist and no kubelet answered
    Then the cluster totals and their history are left as they were

  @oss
  # source: oss:src/live.js:253-258
  Scenario: A cluster that answers with no nodes has empty totals and an empty history
    Given the history holds 10 samples
    When the node list answers with no nodes
    Then the totals read 0 nodes with no percentages
    And the history is emptied

  @oss @pro
  # source: oss:src/live.js:251,269-282
  # source: pro:src/live.js:561,573-586
  Scenario: The usage history keeps the last 24 samples
    Given the history already holds 24 CPU samples
    When a load adds a new sample
    Then the oldest sample is dropped and the history still holds 24
    And each sample is a whole-number percentage

  @oss @pro
  # source: oss:src/live.js:267-274
  # source: pro:src/live.js:571-578
  Scenario: A sample with no percentage adds nothing to the history
    Given the nodes state no allocatable CPU, so the CPU percentage is absent
    When a load records the cluster totals
    Then the CPU history keeps the samples it had and gains none

  @oss @pro
  # source: oss:src/data.js:29-35
  # source: pro:src/data.js:46-52
  # source: pro:src/app.jsx:231
  Scenario: The usage history lives only in the open page
    Given the history holds samples
    When the page is reloaded
    Then the history starts empty

  @pro @fleet
  # source: pro:src/data.js:46-52,63-67
  # source: pro:src/app.jsx:231
  Scenario: The usage history starts empty on every cluster switch
    Given the history holds samples for cluster "a"
    When the user switches to cluster "b"
    Then the history is empty until cluster "b" has been sampled

  # -------------------------------------------------------------------- events

  @oss @pro
  # source: oss:src/live.js:346-352
  # source: pro:src/live.js:672-678
  Scenario: Events are the 50 most recently seen, newest first
    Given the cluster returns 180 events
    Then 50 event rows are kept
    And they are ordered by when each was last seen, newest first

  @oss @pro
  # source: oss:src/live.js:349
  # source: pro:src/live.js:675
  Scenario Outline: An event's last-seen time is its best available timestamp
    Given an event with <timestamps>
    Then its last-seen time is its <chosen>

    Examples:
      | timestamps                                             | chosen             |
      | a last timestamp, an event time and a creation time    | last timestamp     |
      | no last timestamp, an event time and a creation time   | event time         |
      | only a creation time                                   | creation time      |

  @oss @pro
  # source: oss:src/live.js:350-351
  # source: pro:src/live.js:676-677
  Scenario: An event row carries type, reason, object, namespace, message, count and age
    Given a Warning event "BackOff" about the Pod "web-1" in "shop", seen 14 times, last seen 3 minutes ago
    Then the row reads type "Warning", reason "BackOff", object "pod/web-1", namespace "shop", count 14 and age "3m"

  @oss @pro
  # source: oss:src/live.js:350-351
  # source: pro:src/live.js:676-677
  Scenario Outline: Missing event fields get defaults
    Given an event with no <field>
    Then its <column> is <value>

    Examples:
      | field                                   | column    | value     |
      | type                                    | type      | "Normal"  |
      | count                                   | count     | 1         |
      | reason                                  | reason    | empty     |
      | message                                 | message   | empty     |
      | namespace on the event or its object    | namespace | "—"       |
      | involved object                         | object    | "/"       |

  @oss @pro
  # source: oss:src/live.js:351
  # source: pro:src/live.js:677
  Scenario: An event's namespace falls back to the namespace of the object it is about
    Given an event with no namespace of its own about an object in "shop"
    Then its namespace is "shop"

  # ----------------------------------------------------------- storage classes

  @oss @pro
  # source: oss:src/live.js:355-357
  # source: pro:src/live.js:681-683
  Scenario: Storage class rows apply the Kubernetes defaults
    Given a storage class that states no reclaim policy, no binding mode and no expansion setting
    Then its reclaim policy is "Delete", its binding mode is "Immediate" and expansion is off

  @oss @pro
  # source: oss:src/live.js:357
  # source: pro:src/live.js:683
  Scenario: The default storage class is the one annotated as default
    Given a storage class annotated "storageclass.kubernetes.io/is-default-class" = "true"
    Then it is marked as the default storage class
    And a class annotated with any other value is not

  # ------------------------------------------------------------- Helm releases

  @oss @pro
  # source: oss:src/live.js:359-372
  # source: pro:src/live.js:698-711
  Scenario: One Helm release is shown per namespace and name, at its highest revision
    Given Helm release Secrets for "shop/cart" at revisions 1, 2 and 3, with statuses "superseded", "superseded" and "deployed"
    And one for "shop/search" at revision 1 with status "failed"
    Then two releases are shown
    And "cart" in "shop" is at revision 3 with status "deployed"
    And "search" in "shop" is at revision 1 with status "failed"

  @oss @pro
  # source: oss:src/live.js:365-369
  # source: pro:src/live.js:704-708
  Scenario: Helm release details the browser cannot read are shown as a dash
    Given a Helm release Secret
    Then its chart and app version are shown as "—"
    And its "updated" value is the age of the Secret that holds its latest revision
    And a release with no status label shows "—" as its status
    And a release with no name label takes the Secret's own name
    And a revision label that is not a number counts as revision 0

  # ---------------------------------------------------------------- namespaces

  @oss @pro
  # source: oss:src/live.js:301-303
  # source: pro:src/live.js:599-602
  # source: oss:src/data.js:18
  # source: pro:src/data.js:18
  Scenario: The namespace choices are "all-namespaces" followed by the cluster's namespaces
    Given the cluster has the namespaces "default" and "kube-system"
    Then the namespace choices are "all-namespaces", "default", "kube-system", in the order the cluster returned them
    And before any load answers, the only choice is "all-namespaces"

  # -------------------------------------------------------- control-plane facts

  @oss @pro
  # source: oss:src/live.js:376,405
  # source: pro:src/live.js:732,819
  Scenario: The Kubernetes version drops its leading "v"
    Given the apiserver reports the version "v1.37.1-stratum.0"
    Then the cluster's Kubernetes version is "1.37.1-stratum.0"

  @oss @pro
  # source: oss:src/live.js:392,406
  # source: pro:src/live.js:794,820
  Scenario Outline: The CNI is named only when a Cilium DaemonSet exists
    Given the cluster's DaemonSets are named <daemonsets>
    Then the CNI is shown as "<cni>"

    Examples:
      | daemonsets                    | cni    |
      | "cilium" and "kube-proxy"     | Cilium |
      | "Cilium-Agent"                | Cilium |
      | "calico-node"                 | —      |
      | none                          | —      |

  @oss @pro
  # source: oss:src/live.js:393-399,410-412
  # source: pro:src/live.js:795-801,824-826
  Scenario: Static control-plane pods are the mirror pods in kube-system
    Given "kube-system" holds the pods "kube-apiserver-cp1", "etcd-cp1" and "kube-scheduler-cp1", each annotated as a mirror pod and Running
    And a pod "coredns-abc" with no mirror annotation
    And a pod "kube-apiserver-fake" in the namespace "default" annotated as a mirror pod
    Then the static pod count is 3 running of 3
    And the apiserver count is 1 running of 1 and the etcd count is 1 running of 1

  @oss @pro
  # source: oss:src/live.js:395
  # source: pro:src/live.js:797
  Scenario Outline: A pod is a mirror pod by either annotation
    Given a "kube-system" pod annotated <annotation>
    Then it <counts> as a static pod

    Examples:
      | annotation                                       | counts          |
      | "kubernetes.io/config.mirror" with any value     | counts          |
      | "kubernetes.io/config.source" = "file"           | counts          |
      | "kubernetes.io/config.source" = "api"            | does not count  |

  @oss @pro
  # source: oss:src/live.js:397,410-413
  # source: pro:src/live.js:799,824-827
  Scenario: Only pods in the Running phase count as running
    Given three static pods of which one is in phase "Pending"
    Then the static pod count is 2 running of 3

  @oss @pro
  # source: oss:src/live.js:400,413
  # source: pro:src/live.js:802,827
  Scenario: kube-vip pods are any kube-system pods with kube-vip in their name
    Given "kube-system" holds the Running pods "kube-vip-cp1" and "kube-vip-ds-x7k2p"
    Then the kube-vip count is 2 running of 2
    And a kube-vip pod need not be a mirror pod to count

  @oss @pro
  # source: oss:src/live.js:393,407
  # source: pro:src/live.js:795,821
  Scenario: The default storage class name is recorded with the cluster facts
    Given the storage class "longhorn" is the default
    Then the cluster facts name "longhorn" as the default storage class
    And "—" when no class is the default

  # ------------------------------------------------------------ operator state

  @diverged @oss @drop
  # decision: D1, D2 — the pro rule stands on both backends (NOTES O2, O3)
  # source: oss:src/live.js:381-388,414
  Scenario Outline: The operator is active while its Lease has not expired
    Given the operator Lease was renewed <renewed> with a duration of <duration> seconds and holder "<holder>"
    Then the operator is <state> and its leader is shown as "<leader>"

    Examples:
      | renewed          | duration | holder    | state      | leader    |
      | 5 seconds ago    | 15       | op-7f9c   | active     | op-7f9c   |
      | 14 seconds ago   | 15       | op-7f9c   | active     | op-7f9c   |
      | 16 seconds ago   | 15       | op-7f9c   | not active | op-7f9c   |
      | 5 seconds ago    | 15       |           | active     | —         |
      | 5 seconds ago    | unstated | op-7f9c   | not active | op-7f9c   |
      | never            | 15       | op-7f9c   | not active | op-7f9c   |

  @diverged @pro
  # decision: D1 — also the rule on the dashboard server backend, where no hub answers
  # source: pro:src/live.js:61-102
  Scenario Outline: Without hub facts the operator's state allows twice the Lease duration and needs a holder
    Given no hub answers for the cluster
    And the operator Lease was renewed <renewed> with a duration of <duration> seconds and holder "<holder>"
    Then the operator state is "<state>" and its leader is shown as "<leader>"

    Examples:
      | renewed                 | duration | holder    | state    | leader    |
      | 5 seconds ago           | 15       | op-7f9c   | active   | op-7f9c   |
      | 16 seconds ago          | 15       | op-7f9c   | active   | op-7f9c   |
      | 30 seconds ago          | 15       | op-7f9c   | active   | op-7f9c   |
      | 31 seconds ago          | 15       | op-7f9c   | inactive | op-7f9c   |
      | 59 seconds ago          | unstated | op-7f9c   | active   | op-7f9c   |
      | 61 seconds ago          | unstated | op-7f9c   | inactive | op-7f9c   |
      | 5 seconds ago           | 15       |           | inactive | —         |
      | never                   | 15       | op-7f9c   | inactive | op-7f9c   |
      | 40 seconds in the future | 15      | op-7f9c   | unknown  | op-7f9c   |
      | 20 seconds in the future | 15      | op-7f9c   | active   | op-7f9c   |

  @pro
  # source: pro:src/live.js:93-101
  Scenario Outline: Without hub facts a Lease that could not be read has its own state
    Given no hub answers for the cluster
    When the operator Lease read ends with <outcome>
    Then the operator state is "<state>"
    And the Lease is flagged as refused: <refused>

    Examples:
      | outcome          | state   | refused |
      | status 404       | missing | no      |
      | status 403       | unknown | yes     |
      | status 500       | unknown | no      |
      | a network error  | unknown | no      |

  @pro
  # source: pro:src/live.js:104-122
  Scenario Outline: With hub facts the operator's state is the hub's verdict
    Given the hub's facts state the operator Lease as "<hub state>" held by "<holder>"
    Then the operator is <active> and its state is "<hub state>"
    And its leader is shown as "<leader>"

    Examples:
      | hub state  | holder   | active     | leader   |
      | active     | op-7f9c  | active     | op-7f9c  |
      | inactive   | op-7f9c  | not active | op-7f9c  |
      | missing    | op-7f9c  | not active | —        |
      | unknown    |          | not active | —        |
      | paused     | op-7f9c  | not active | op-7f9c  |

  # ---------------------------------------------------------- cluster identity

  @diverged @oss
  # decision: D1 — stands on the dashboard server backend (D4)
  # source: oss:src/live.js:417-427
  Scenario Outline: The cluster's identity and health are built from its own apiserver
    Given the load found <nodes> nodes of which <ready> are ready
    Then the cluster is named "local" with source "local"
    And its status is "<status>"
    And its Stratum version is the version the dashboard server reports for itself

    Examples:
      | nodes | ready | status   |
      | 3     | 3     | healthy  |
      | 3     | 2     | degraded |
      | 0     | 0     | degraded |

  @oss
  # source: oss:src/live.js:420-427
  # source: oss:src/screens/overview.jsx:27-31
  Scenario: The cluster's node counts can be fresher than its node rows
    Given the previous load showed 3 node rows
    When a load reads the version and the node list, now 4 nodes, but the pod list fails
    Then the cluster's identity says 4 nodes
    And the node rows still show the previous 3

  @diverged @pro
  # decision: D1 — stands on the hub backend (D4)
  # source: pro:src/app.jsx:203-206
  # source: pro:src/live.js:11-14,297-305
  Scenario: The cluster's identity and health come from the hub's cluster list
    Given the hub lists the selected cluster with a name, status, provider, region and versions
    Then those are what the screens show for the cluster
    And nothing in the browser recomputes the cluster's health from its nodes

  # ---------------------------------------------- pro: hardening and CSR facts

  @pro
  # source: pro:src/live.js:757-772,783-784
  # source: pro:src/k8sident.js:27
  Scenario Outline: Without hub facts a kubelet-serving CSR is pending only when nothing decided it
    Given no hub answers for the cluster
    And a certificate signing request for the signer "kubernetes.io/kubelet-serving" with <state>
    Then it counts as <counted>

    Examples:
      | state                                              | counted               |
      | an Approved condition                              | approved, not pending |
      | a Denied condition                                 | not pending           |
      | a Failed condition                                 | not pending           |
      | no condition but an issued certificate             | not pending           |
      | no condition and no certificate                    | pending               |

  @pro
  # source: pro:src/live.js:758
  Scenario: Requests for other signers are ignored
    Given a pending certificate signing request for the signer "kubernetes.io/kube-apiserver-client"
    Then it is not counted among the kubelet-serving requests

  @pro
  # source: pro:src/live.js:747-749,770-772,783-784
  Scenario Outline: An unread CSR list has no pending count
    Given no hub answers for the cluster
    When the certificate signing request list ends with <outcome>
    Then the pending count is <pending> and the list's availability is "<availability>"

    Examples:
      | outcome                      | pending | availability |
      | an answer with no requests   | 0       | available    |
      | status 403                   | unknown | forbidden    |
      | status 500                   | unknown | unknown      |
      | a network error              | unknown | unknown      |

  @pro
  # source: pro:src/live.js:783-788
  Scenario: With hub facts the pending count and availability are the hub's
    Given the hub's facts state 2 pending kubelet-serving requests
    And the cluster's own list shows 5 approved requests
    Then the dashboard shows 5 approved and 2 pending
    And a hub that states no availability leaves it "unknown"

  @pro
  # source: pro:src/live.js:757,774-782
  # source: pro:src/k8sident.js:26
  Scenario: Restricted namespaces are those enforcing the restricted pod security level
    Given 6 namespaces of which 2 carry the label "pod-security.kubernetes.io/enforce" = "restricted"
    Then the hardening facts say 2 of 6 namespaces are restricted and name them
    And when the namespace list could not be read, those counts are unknown rather than 0

  @pro
  # source: pro:src/live.js:785-788,829-831
  Scenario: The time the facts were observed is the hub's, or absent
    Given the hub's facts carry an observation time
    Then that time is shown as when the facts were observed
    And when no hub answered, no observation time is shown

  @pro @drop
  # decision: no screen reads this value (searched pro:src); found by the verification pass
  # source: pro:src/live.js:803-817,832-833
  Scenario Outline: Without hub facts the collection is complete only when six reads were whole
    Given no hub answers for the cluster
    When the load's reads of version, nodes, operator Lease, signing requests, namespaces and deployments end with <outcome>
    Then the collection is reported as <complete>

    Examples:
      | outcome                                      | complete     |
      | all six answering                            | complete     |
      | the Lease answering 404, the rest answering  | complete     |
      | the Lease answering 403                      | not complete |
      | the signing requests answering 403           | not complete |
      | the namespaces read failing                  | not complete |
      | the probe answering 403, so no version       | not complete |

  @pro @drop
  # decision: no screen reads this value (searched pro:src); found by the verification pass
  # source: pro:src/live.js:813-817
  Scenario: With hub facts completeness and the node total are the hub's statements
    Given the hub's facts say the collection is complete with 12 nodes in total
    Then the cluster facts say complete and 12 nodes
    And facts that state no completeness are treated as not complete
    And facts that state no valid node total leave it unknown
