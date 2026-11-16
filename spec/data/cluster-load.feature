Feature: Loading a cluster's live state
  The dashboard keeps one in-memory snapshot of the cluster it shows and
  rebuilds it from the Kubernetes API. This feature records which reads one
  load makes, in what order, and what happens to the snapshot when some or
  all of them fail. How often a load runs is in polling-and-refresh.feature;
  how values are derived from the objects is in the derived-*.feature files.

  # ------------------------------------------------------------ what is read

  @diverged @oss
  # decision: D1, D9 — this set of kinds stands; each kind is read as its own query
  # source: oss:src/live.js:169-176
  Scenario: One load reads twelve cluster-wide resources in parallel
    When a load starts
    Then these reads are issued at the same time, each across all namespaces:
      | read                                              |
      | the apiserver version                             |
      | nodes                                             |
      | pods                                              |
      | namespaces                                        |
      | deployments                                       |
      | statefulsets                                      |
      | daemonsets                                        |
      | jobs                                              |
      | cronjobs                                          |
      | storage classes                                   |
      | Helm release Secrets, as summaries or metadata    |
      | events                                            |

  @diverged @pro @drop
  # decision: D1, D9, D10 — the oss set stands; nine of these reads feed no screen and the Secret read carries data
  # source: pro:src/live.js:434-449
  Scenario: One load reads twenty-one cluster-wide resources in parallel
    Given the cluster answered the liveness probe
    When the load continues
    Then these reads are issued at the same time, each across all namespaces:
      | read                    |
      | nodes                   |
      | pods                    |
      | namespaces              |
      | services                |
      | deployments             |
      | statefulsets            |
      | daemonsets              |
      | jobs                    |
      | cronjobs                |
      | ingresses               |
      | persistent volume claims |
      | persistent volumes      |
      | storage classes         |
      | config maps             |
      | secrets, in full        |
      | service accounts        |
      | cluster roles           |
      | roles                   |
      | cluster role bindings   |
      | role bindings           |
      | events                  |

  @pro @drop
  # decision: no screen reads these collections (services, ingresses, PVCs, PVs, config maps, secret rows, service accounts, roles, bindings, per-namespace rollups, the cluster drift summary) — every kind list fetches its own data. Recorded by the extraction pass with nobody available to confirm; the rewrite should not load them on every poll. Helm releases still need a Secret read, see derived-cluster.feature.
  # source: pro:src/live.js:603-608,648,653,656-670,684-696,713-728,737-738
  # source: pro:src/data.js:22-36
  Scenario: Collections are prepared on every load that no screen displays
    When a load commits
    Then rows for services, ingresses, persistent volume claims, persistent volumes, config maps, secrets, service accounts, roles, bindings and per-namespace totals are rebuilt
    And no screen shows them

  @oss @pro
  # source: oss:src/live.js:181-185
  # source: pro:src/live.js:476-479
  Scenario: Usage is read from each node's kubelet after the node list answers
    Given the node list answered with the nodes "cp-1" and "w 1"
    When the first round of reads has settled
    Then one usage summary is requested per node, in parallel, through the node proxy
    And the node name is percent-encoded in the path, so "w 1" is requested as "w%201"

  @diverged @oss
  # decision: D1 — stands on the dashboard server backend (D4), where no hub gives a verdict
  # source: oss:src/live.js:33-36,145-149,378
  Scenario: The operator's Lease is read last, on every load
    When the usage summaries have settled
    Then the Lease "stratum-operator" in "kube-system" is read

  @diverged @pro
  # decision: D1 — stands on the hub backend (D4)
  # source: pro:src/live.js:747-752
  Scenario: The operator's Lease is read only when no hub answers for the cluster
    Given the hub's facts for the cluster were not available and no hub is known
    When the usage summaries have settled
    Then the Lease "stratum-operator" in "kube-system" is read, together with the certificate signing requests

  @pro
  # source: pro:src/live.js:747-752
  Scenario: Certificate signing requests are read on every load
    When the usage summaries have settled
    Then the cluster's certificate signing requests are listed

  @pro @drop
  # decision: D2, D8 — every request has a 15 second deadline (NOTES P1); the corrected behavior is the transport scenario "Every non-streaming request is abandoned after 15 seconds by default"
  # source: pro:src/live.js:46-49,288,295,421-422,436-449,479,749-751
  # source: pro:src/k8sproxy.js:47-48
  # source: pro:src/app.jsx:277-296
  Scenario: Cluster reads after the probe have no deadline, so one stalled read stops polling
    Given the probe, the cluster list and the hub facts are each bounded at 10 seconds
    And the list reads, the per-node usage reads, the signing requests and the Lease read have no deadline
    When one of those unbounded reads is accepted by the proxy and never answered
    Then the load never finishes
    And no further poll is scheduled until the user switches cluster or reloads the page

  @diverged @oss
  # decision: D10
  # source: oss:src/live.js:30-32,156-165
  # source: oss:src/cluster-context.js:97
  Scenario: Helm releases are found without ever downloading Secret contents
    Given the server offers Secret summaries
    When a load reads Helm releases
    Then it requests the server's own summary list, outside the Kubernetes proxy prefix
    And no Secret data reaches the browser

  @oss
  # decision: D10 — the fallback on any backend that offers no summaries
  # source: oss:src/live.js:30-31,65-73,159-165
  Scenario: Without server summaries, Helm releases come from a metadata-only table
    Given the server does not offer Secret summaries
    When a load reads Helm releases
    Then it lists only Secrets of type "helm.sh/release.v1", as a table with object metadata only
    And no Secret data reaches the browser

  @diverged @pro @drop
  # decision: D1, D10 — Secrets are never listed with their data for bookkeeping (NOTES P4)
  # source: pro:src/live.js:445,695-711
  Scenario: Helm releases are found by downloading every Secret in the cluster
    When a load reads Helm releases
    Then it lists every Secret in every namespace, data included
    And keeps only those of type "helm.sh/release.v1"

  # ------------------------------------------------------- pro: before reads

  @pro
  # source: pro:src/live.js:283-310,325
  Scenario: Every load first refreshes the list of clusters from the hub
    When a load starts
    Then the hub's cluster list is requested with a 10 second deadline
    And at most one such request is in flight at a time, shared by everyone who asks

  @pro
  # source: pro:src/live.js:297-306
  Scenario: A failed cluster-list refresh keeps the clusters already known
    Given the dashboard already knows three clusters
    When the hub's cluster list fails or answers empty
    Then the three clusters stay listed

  @pro @drop
  # decision: D13, D2 — no fabricated local cluster (NOTES P10)
  # source: pro:src/live.js:299-305
  Scenario: With no hub at all the dashboard assumes a single local cluster
    Given the dashboard knows no clusters
    When the hub's cluster list fails
    Then one cluster named "local" is assumed, with status "healthy" and no versions or node counts
    And a console warning says a single local cluster is being assumed

  @pro @drop
  # decision: D13, D2 — no fabricated local cluster
  # source: pro:src/live.js:297-305
  Scenario: An empty cluster list from a reachable hub is treated as a single local cluster
    Given the dashboard knows no clusters
    When the hub's cluster list answers successfully with no clusters
    Then one cluster named "local" is assumed, with status "healthy"
    And no console warning is written

  @pro @fleet @fixed
  # decision: D13, D2
  # source: pro:src/live.js:297-305
  Scenario: An empty fleet is shown as empty and an unreadable fleet as unreadable
    Given the dashboard knows no clusters
    When the hub's cluster list answers with no clusters
    Then the dashboard says the fleet has no clusters
    And when the list cannot be read, it says the fleet could not be read
    And no cluster named "local" is invented in either case

  @pro @drop
  # decision: D9 — the pre-flight liveness probe is dropped
  # source: pro:src/live.js:37-58,392,400
  # source: pro:src/poll.js:28-44
  Scenario Outline: A liveness probe decides whether the full load runs
    When the apiserver version probe ends with <probe outcome>
    Then the cluster is treated as <verdict>
    And the remaining reads are <fan-out>

    Examples:
      | probe outcome                      | verdict     | fan-out    |
      | status 200                         | reachable   | issued     |
      | status 401                         | reachable   | issued     |
      | status 403                         | reachable   | issued     |
      | status 404                         | reachable   | issued     |
      | status 429                         | unreachable | not issued |
      | status 500                         | unreachable | not issued |
      | status 502                         | unreachable | not issued |
      | status 503                         | unreachable | not issued |
      | a network error                    | unreachable | not issued |
      | no answer within 10 seconds        | unreachable | not issued |
      | status 200 with an undecodable body | unreachable | not issued |

  @pro @drop
  # decision: D9 — the pre-flight liveness probe is dropped
  # source: pro:src/live.js:386-391,400
  Scenario: An unreachable cluster costs one request and keeps the last good data
    Given the snapshot holds data from an earlier successful load
    When the liveness probe finds the cluster unreachable
    Then no other Kubernetes read is issued for this load
    And the snapshot is left as it was, except for the engine-state evidence the hub returned
    And the load reports the cluster as unreachable

  @pro @drop
  # decision: D9 — the pre-flight liveness probe is dropped; the version is its own read
  # source: pro:src/live.js:409,732
  Scenario: A reachable probe that carried no version leaves the Kubernetes version blank
    When the probe answers 403
    And the load commits
    Then the cluster's Kubernetes version is shown as unknown

  @pro
  # source: pro:src/live.js:137-174,410-415,463-467
  Scenario: The hub's facts for the cluster are fetched alongside the reads
    When the probe finds the cluster reachable
    Then the hub's facts for the cluster are requested with a 10 second deadline, in parallel with the reads

  @pro
  # source: pro:src/live.js:153-162
  Scenario: A standalone hub's facts are addressed to the hub's own cluster record
    Given the hub runs in standalone mode
    And the hub's cluster list marks the record "site-a" as the hub's own cluster
    When the facts are requested
    Then they are requested for "site-a", whatever cluster id the browser remembered

  @pro
  # source: pro:src/live.js:158-162
  Scenario: A standalone hub with no marked record is addressed as "local"
    Given the hub runs in standalone mode
    And the cluster list is unavailable
    When the facts are requested
    Then they are requested for "local"

  @pro
  # source: pro:src/live.js:386-391
  # source: pro:src/live.js:124-135,164-166,463-467
  Scenario: A known hub that cannot give facts stops the load from committing
    Given a hub is known, from its configuration, an earlier facts answer or a real cluster list
    When the facts request fails or answers with an unexpected shape
    Then nothing from this load's Kubernetes reads replaces the snapshot
    And the engine-state evidence the hub returned is still published
    And the load reports the cluster as unreachable

  @pro
  # source: pro:src/live.js:155-157,166-172,463-467
  Scenario: With no hub at all, verdicts fall back to rules applied in the browser
    Given no hub configuration was ever loaded and the cluster list was fabricated
    When the facts request fails
    Then the load still commits
    And readiness, deployment status, operator state, pending CSRs and version drift are derived in the browser

  @pro
  # source: pro:src/live.js:146-152,166-172
  Scenario: Missing hub facts are reported in the console once per cluster per session
    When the facts request fails on ten consecutive loads of the same cluster
    Then exactly one console warning names the cluster

  # ------------------------------------------------------------ partial failure

  @diverged @oss
  # source: oss:src/live.js:75-96,128-135,150-154
  Scenario Outline: A slice of the snapshot is replaced only when every read behind it answered
    Given the snapshot holds data from an earlier successful load
    When a load finishes and the read of <failed read> did not answer
    Then <slice> keeps its previous content
    And the load names "<stale name>" as stale

    Examples:
      | failed read      | slice                                   | stale name       |
      | nodes            | the node rows                           | NODES            |
      | pods             | the node rows                           | NODES            |
      | nodes            | the cluster usage totals                | FLEET            |
      | nodes            | the node pressure conditions            | NODE_CONDITIONS  |
      | namespaces       | the namespace list                      | NAMESPACES       |
      | pods             | the pod rows                            | PODS             |
      | deployments      | the workload rows                       | WORKLOADS        |
      | statefulsets     | the workload rows                       | WORKLOADS        |
      | daemonsets       | the workload rows                       | WORKLOADS        |
      | jobs             | the workload rows                       | WORKLOADS        |
      | cronjobs         | the workload rows                       | WORKLOADS        |
      | events           | the event rows                          | EVENTS           |
      | storage classes  | the storage classes                     | STORAGE_CLASSES  |
      | Helm Secrets     | the Helm releases                       | HELM             |
      | version          | the cluster facts shown on the Overview | CLUSTER_INFO     |
      | nodes            | the cluster facts shown on the Overview | CLUSTER_INFO     |
      | pods             | the cluster facts shown on the Overview | CLUSTER_INFO     |
      | daemonsets       | the cluster facts shown on the Overview | CLUSTER_INFO     |
      | storage classes  | the cluster facts shown on the Overview | CLUSTER_INFO     |
      | version          | the cluster's identity                  | CLUSTER          |
      | nodes            | the cluster's identity                  | CLUSTER          |

  @diverged @oss
  # source: oss:src/live.js:128-131,245,303,306,342,348,355,372
  Scenario: An answered empty list replaces the previous rows
    Given the snapshot shows 4 pods
    When a load reads the pod list and the cluster answers with no pods
    Then the snapshot shows 0 pods

  @diverged @pro @drop
  # decision: D1, D2 — the oss per-slice rule stands; an answered-empty list keeping stale rows is a defect (NOTES P3)
  # source: pro:src/live.js:422-429,554,601,612,647,658,674,721,727
  Scenario Outline: A read that fails or answers empty keeps the previous rows for some collections
    Given the snapshot shows rows for <collection> from an earlier load
    When a load commits and the read behind <collection> failed or answered with no items
    Then <collection> keeps the previous rows

    Examples:
      | collection   |
      | nodes        |
      | namespaces   |
      | pods         |
      | workloads, when all five workload reads failed or answered empty |
      | events       |

  @diverged @pro @drop
  # decision: D1, D2 — the oss per-slice rule stands; a failed read blanking a list is a defect (NOTES P2)
  # source: pro:src/live.js:422-429,590-597,681-683,701-711,818-834
  Scenario Outline: A read that fails blanks other collections
    Given the snapshot shows <collection> from an earlier load
    When a load commits and the read of <failed read> failed
    Then <collection> is replaced by <result>

    Examples:
      | collection                   | failed read     | result                          |
      | storage classes              | storage classes | an empty list                   |
      | Helm releases                | secrets         | an empty list                   |
      | node pressure conditions     | nodes           | conditions with no nodes named  |
      | the control-plane pod counts | pods            | zero counts                     |
      | the CNI name                 | daemonsets      | "—"                             |

  @pro @drop
  # decision: D2, D9 — each kind is its own query; a failed kind keeps its last rows (NOTES P2)
  # source: pro:src/live.js:627-647
  Scenario: A failed read of one workload kind removes that kind's rows
    Given the snapshot shows Deployments, StatefulSets and Jobs
    When a load commits in which the Deployments read failed and the other workload reads answered
    Then every Deployment row disappears
    And the rows of the other workload kinds are refreshed

  @diverged @oss
  # source: oss:src/live.js:38-62,120-126,137,170
  Scenario Outline: An answer of the wrong shape is a failed read
    When the read of <read> answers 200 with <body>
    Then it is treated exactly as a failed read
    And a console warning says the response shape was malformed

    Examples:
      | read                   | body                                         |
      | pods                   | an object with no "items" list               |
      | the apiserver version  | an object whose "gitVersion" is blank        |
      | a node's usage summary | an object with no node CPU usage figure      |
      | a node's usage summary | a pod entry with no namespace or name        |
      | the operator Lease     | an object with no metadata                   |
      | Secret summaries       | an item with no name                         |
      | Secret summaries       | a Helm-typed item with no Helm details       |
      | the Secret table       | a table with no "Type" or "Data" column      |
      | the Secret table       | a row whose cell count differs from the columns |

  @diverged @pro @drop
  # decision: D1 — the oss variant (shape checked) stands
  # source: pro:src/live.js:59,422-429
  # assumption: no shape validation exists in this root; a 200 with any JSON body is accepted
  Scenario: An answer of the wrong shape is accepted as an empty list
    When the read of pods answers 200 with an object that has no "items" list
    Then the load treats the cluster as having answered with no pods

  @oss
  # source: oss:src/live.js:34-37,145-149,383-388,404
  Scenario: A missing operator Lease is an answer, not a failure
    When the operator Lease read answers 404
    Then the cluster facts are replaced, with the operator shown as not active
    And nothing is named stale because of the Lease

  @oss @drop
  # decision: D2, D9 — one failed read must not freeze values that other reads answered (NOTES O6)
  # source: oss:src/live.js:93-94,145-149,404
  Scenario: A failed operator Lease read keeps every cluster fact on its previous value
    Given the snapshot holds cluster facts from an earlier load
    When a load finishes in which the operator Lease read failed with anything other than 404
    Then the Kubernetes version, CNI, static pod counts and operator state all keep their previous values
    And the load names "CLUSTER_INFO" as stale

  @oss @fixed
  # decision: D2, D9
  # source: oss:src/live.js:93-94,145-149,404
  Scenario: A failed operator Lease read leaves only the operator's state without a fresh verdict
    Given the Kubernetes version, CNI and static pod counts were read successfully
    When the operator Lease read fails with anything other than 404
    Then the version, CNI and static pod counts show what was just read
    And only the operator's state is reported as not refreshed

  @oss
  # source: oss:src/live.js:138-144,201,215,225-226,437
  Scenario: An unreachable kubelet leaves that node on request-based figures
    Given the node list answered with "w-1" and "w-2"
    When the usage summary for "w-2" does not answer
    Then the row for "w-1" shows measured CPU and memory
    And the row for "w-2" shows CPU and memory computed from pod requests
    And the cluster usage totals keep their previous sample and are named stale as "FLEET"

  @diverged @oss
  # source: oss:src/live.js:201-204,334-341,438
  Scenario: Workload usage is replaced only by a complete sample
    Given the snapshot shows the deployment "web" using 120 millicores
    When a load finishes in which one kubelet did not answer, or one pod reported no usage figures
    Then "web" still shows 120 millicores
    And its readiness, status and image are refreshed
    And the load names "WORKLOADS" as stale

  @oss
  # source: oss:src/live.js:338-340
  Scenario: A workload first seen without a complete usage sample shows no usage
    Given a deployment "new" did not exist at the previous load
    When a load finishes without a complete usage sample
    Then "new" shows 0 CPU and 0 memory

  @diverged @pro @drop
  # decision: D1 — the oss variant (complete samples only) stands
  # source: pro:src/live.js:496,650-654
  Scenario: Workload usage is recomputed whenever any kubelet answered
    Given three nodes exist and only one kubelet answered its usage summary
    When the load commits
    Then each workload's usage is the sum over the pods that one kubelet reported
    And workloads on the other nodes show 0 CPU and 0 memory

  @pro @drop
  # decision: D1 — the oss variant (the previous usage is kept) stands
  # source: pro:src/live.js:496,638-654
  Scenario: With no usage summary at all every workload shows zero usage
    Given the snapshot shows the deployment "web" using 120 millicores
    When a load commits in which no kubelet answered its usage summary
    Then "web" shows 0 CPU and 0 memory

  @oss
  # source: oss:src/live.js:201-204,334-341,438
  Scenario: A failed node list also leaves workload usage on its last sample
    Given the snapshot shows the deployment "web" using 120 millicores
    When a load finishes in which the node list did not answer and the workload reads did
    Then "web" still shows 120 millicores
    And the load names "WORKLOADS" as stale, in addition to the slices that depend on nodes

  @pro
  # source: pro:src/live.js:416-429,451-461
  Scenario Outline: Only outage-like failures count against the cluster's reachability
    When a read in the parallel round fails with <failure>
    Then it <counts> as an outage of the cluster path

    Examples:
      | failure               | counts         |
      | a network error       | counts         |
      | an undecodable body   | counts         |
      | status 429            | counts         |
      | status 500            | counts         |
      | status 503            | counts         |
      | status 401            | does not count |
      | status 403            | does not count |
      | status 404            | does not count |

  @pro
  # source: pro:src/live.js:457-458
  Scenario: A round in which nothing succeeded and something was an outage keeps the last good data
    Given the probe answered
    When every read of the parallel round fails and at least one of them is an outage
    Then nothing replaces the snapshot
    And the load reports the cluster as unreachable

  @pro
  # source: pro:src/live.js:459-461
  Scenario: A partly failed round commits what loaded and says so in the console
    When 3 of the 21 reads fail as outages and the rest succeed
    Then the load commits
    And a console warning says "partial fan-out — 3/21 calls failed; some panels may show empty"

  @pro @drop
  # decision: D2, D9 — NOTES P6; each kind is its own query
  # source: pro:src/live.js:422-429,458
  # assumption: follows from the counting rule above; no dedicated branch exists for "everything forbidden"
  Scenario: A round in which every read is forbidden still counts as reachable
    Given the user's credentials may list nothing
    When every read of the parallel round answers 403
    Then the cluster is not reported as unreachable

  @pro @fixed
  # decision: D2, D9
  # source: pro:src/live.js:422-429,458
  Scenario: A kind the user may not list is shown as forbidden without affecting other kinds
    Given the user's credentials may not list pods
    When the cluster's data is loaded
    Then the pods are reported as not permitted, not as an empty list
    And every other kind shows what its own read returned
    And the cluster is not reported as unreachable because of the refusal

  # --------------------------------------------------------- outcome of a load

  @oss
  # source: oss:src/live.js:429-439
  Scenario: A load reports whether anything answered and which slices are stale
    When a load finishes in which only the events read failed
    Then the load reports that it answered
    And it names exactly "EVENTS" as stale

  @oss
  # source: oss:src/live.js:118-126,439
  # source: oss:src/live.js:447-450
  Scenario: A load that hit a 401 reports the session as ended
    When any read of a load answers 401
    Then the load reports the session as ended
    And no "refreshed" time is recorded and no refresh message is shown

  @oss
  # source: oss:src/live.js:119,124
  Scenario: Every failed read leaves a console warning
    When the read of jobs answers 503 with the message "etcd unavailable"
    Then the console shows "Stratum live: /apis/batch/v1/jobs -> HTTP 503: etcd unavailable"

  @pro
  # source: pro:src/live.js:836-857
  Scenario: A committed load publishes every collection at once
    When a load reaches its end still owning the selection
    Then the whole per-cluster snapshot is replaced in a single step
    And the list of clusters is not overwritten by that step
    And the load reports the cluster as reachable

  # ------------------------------------------------------------- when it runs

  @oss
  # source: oss:src/app.jsx:292-306
  Scenario: The first load runs once when the dashboard shell appears
    Given the user is signed in, or no sign-in is required
    When the dashboard shell is first shown
    Then one load runs
    And if no load had ever succeeded in this page, the snapshot starts empty

  @pro
  # source: pro:src/app.jsx:216-261
  Scenario: A load runs when a cluster is opened and on every switch
    When the address names a cluster the dashboard has not loaded yet
    Then that cluster becomes the selection and one load runs for it

  @pro
  # source: pro:src/live.js:317-318
  Scenario: Asking to load a cluster other than the selected one selects it
    Given cluster "a" is selected
    When a load is requested for cluster "b"
    Then cluster "b" becomes the selection before any request is sent

  @oss @pro
  # source: oss:src/app.jsx:441-449
  # source: pro:src/app.jsx:432-456
  Scenario: A successful cordon or uncordon is followed by an immediate load
    When the user cordons a node and the cluster accepts the change
    Then a load runs straight away, without waiting for the next poll

  @oss @pro
  # source: oss:src/app.jsx:445-447
  # source: pro:src/app.jsx:436-439
  Scenario: A refused cordon triggers no load
    When the user cordons a node and the cluster refuses the change
    Then the refusal's message is shown
    And no load runs because of it

  @oss @pro
  # source: oss:src/app.jsx:262,384-389
  # source: pro:src/app.jsx:356-361
  # assumption: no code path reloads on a namespace change; the namespace lives in the address and filters rows already loaded
  Scenario: Changing the namespace filter issues no request
    Given a load has completed
    When the user picks the namespace "kube-system"
    Then no Kubernetes request is sent because of the change
    And the rows already loaded are filtered to "kube-system"
