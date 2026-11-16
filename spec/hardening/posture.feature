Feature: Hardening posture signals
  The Hardening screen shows only what can be observed about a cluster's CIS
  posture from the cluster and the hub: the operator that approves kubelet
  certificates, PodSecurity enforcement, kubelet-serving certificate requests
  and control-plane nodes. It never shows per-control pass or fail results,
  and it has no report selector, findings list or filters.

  # source: pro:src/screens/hardening.jsx:46-56
  # source: pro:src/data.js:37
  # decision: D2 — a load still in progress is reported as an unreachable cluster, with no way to retry (NOTES (a) 1)
  @pro @drop
  Scenario: The screen says so when no posture has been observed
    Given the selected cluster's posture has not been loaded, because the first load has not finished or the cluster could not be reached
    When the user opens Hardening
    Then the title "Hardening" is shown with the message "Posture signals unavailable — the apiserver was unreachable."
    And no cards, no signals, no control list and no Refresh button are shown

  # source: pro:src/screens/hardening.jsx:46-56,78
  # source: pro:src/data.js:37
  # decision: D2
  @pro @fixed
  Scenario: Loading and unreachable are told apart
    When the user opens Hardening before the selected cluster's posture has been loaded for the first time
    Then a loading state is shown, not an error
    But when the cluster could not be reached, the message "Posture signals unavailable — the apiserver was unreachable." is shown together with a "Refresh" button

  # source: pro:src/screens/hardening.jsx:71-79
  @pro
  Scenario: The header names the benchmark and offers a refresh
    Given the selected cluster's posture is available
    When the user opens Hardening
    Then the title "Hardening" is shown with the subtitle "CIS Kubernetes Benchmark posture · stratum up --profile cis"
    And a "Refresh" button is shown

  # source: pro:src/screens/hardening.jsx:78
  # source: pro:src/app.jsx:366-383,505
  @pro
  Scenario Outline: Refresh reloads the cluster's data and reports the outcome
    When the user presses "Refresh" on Hardening and the reload <result>
    Then a notification reads "<notification>"
    And the screen shows <data>

    Examples:
      | result    | notification                         | data                         |
      | succeeds  | Refreshed                            | the newly observed posture   |
      | fails     | Refresh failed — keeping last data   | the previously shown posture |

  # source: pro:src/screens/hardening.jsx:46-166
  # assumption: recorded from the absence of any role check or action control on the screen
  @pro
  Scenario: Read-only users see the same Hardening screen as admins
    Given the signed-in user does not have the admin role
    When the user opens Hardening
    Then the same cards, signals, control list and Refresh button are shown as for an admin

  # source: pro:src/screens/hardening.jsx:83-87
  # source: pro:src/live.js:113-122
  @pro
  Scenario Outline: The CSR approver card states the operator's verdict
    Given the operator lease verdict is <verdict> with holder <holder>
    Then the "CSR approver" card reads "<value>" with "<caption>" beneath

    Examples:
      | verdict                         | holder        | value    | caption      |
      | active                          | "operator-0"  | Active   | operator-0   |
      | inactive                        | "operator-0"  | Inactive | operator-0   |
      | inactive                        | nobody        | Inactive | —            |
      | missing                         | anyone        | Inactive | —            |
      | unknown                         | nobody        | Unknown  | —            |
      | a word this dashboard predates  | "operator-0"  | Unknown  | operator-0   |

  # source: pro:src/screens/hardening.jsx:62-63,88-92
  @pro
  Scenario Outline: The PodSecurity card counts namespaces that enforce the restricted level
    Given <observation>
    Then the "PodSecurity restricted" card reads "<value>" with the caption "<caption>"

    Examples:
      | observation                                                   | value        | caption                     |
      | 3 of 12 namespaces enforce PodSecurity "restricted"           | 3 / 12 ns    | enforce=restricted          |
      | none of 12 namespaces enforces PodSecurity "restricted"       | 0 / 12 ns    | not enforced                |
      | the namespaces could not be read                              | unknown      | namespace read unavailable  |

  # source: pro:src/screens/hardening.jsx:58-59,93-97
  @pro
  Scenario Outline: The kubelet serving certificates card counts approved and pending requests
    Given <observation>
    Then the "Kubelet serving certs" card reads "<value>" with the caption "<caption>"

    Examples:
      | observation                                                                 | value       | caption                     |
      | 5 approved kubelet-serving requests and none pending, state available       | 5 approved  | 0 pending                   |
      | 5 approved kubelet-serving requests and 2 pending, state available          | 5 approved  | 2 pending, as a warning     |
      | 5 approved requests were read but the pending state is not available        | 5 approved  | CSR pending count unknown   |

  # source: pro:src/screens/hardening.jsx:95-96,120-121
  # source: pro:src/live.js:749,760,783-784
  # decision: D2 — an unread request list is shown as an observed zero (NOTES (a) 3)
  @pro @drop
  Scenario Outline: An unreadable cluster request list reads as 0 approved
    Given the cluster's certificate signing requests could not be listed
    And <hub>
    Then the "Kubelet serving certs" card reads "0 approved" with the caption "<caption>"

    Examples:
      | hub                                                             | caption                     |
      | no hub facts are available                                      | CSR pending count unknown   |
      | the hub states the availability "available" and 2 pending       | 2 pending, as a warning     |

  # source: pro:src/screens/hardening.jsx:95-96,120-121
  # source: pro:src/live.js:749,760,783-784
  # decision: D2
  @pro @fixed
  Scenario: An unreadable cluster request list leaves the approved count unknown
    Given the cluster's certificate signing requests could not be listed
    Then the "Kubelet serving certs" card and the request signal state that the approved count is unknown instead of showing 0
    And the pending count the hub states, if any, is still shown

  # source: pro:src/screens/hardening.jsx:96,120-121
  # source: pro:src/live.js:783-784
  # decision: D2 — a missing number is printed as the word "null" (NOTES (a) 2)
  @pro @drop
  Scenario: A hub that states the request availability but no pending count
    Given the hub states the kubelet-serving request availability "available" and no pending count
    Then the card's caption reads "null pending"
    And the request signal is a warning whose detail reads "<approved> approved · null pending (kubelet-serving signer)"

  # source: pro:src/screens/hardening.jsx:96,120-121
  # source: pro:src/live.js:783-784
  # decision: D2
  @pro @fixed
  Scenario: A missing pending count reads as unknown whatever the availability
    Given the hub states the kubelet-serving request availability "available" and no pending count
    Then the card's caption reads "CSR pending count unknown"
    And the request signal is labelled "Kubelet-serving CSR pending state unknown"

  # source: pro:src/screens/hardening.jsx:98-102
  # source: pro:src/live.js:527-530,731,779
  @pro
  Scenario: The control plane card counts control-plane nodes
    Given 3 of the cluster's nodes carry the label "node-role.kubernetes.io/control-plane" or "stratum.ciq.com/control-plane"
    Then the "Control plane" card reads "3 nodes" with the caption "Stratum control-plane label"

  # source: pro:src/screens/hardening.jsx:107-111
  # source: pro:src/live.js:785-788
  @pro
  Scenario: The observed signals state when the hub observed them
    Given the hub recorded the cluster's facts at a known time
    Then the "Observed from the cluster" card is captioned "live · cluster-visible signals · observed <that time of day in the browser's locale>"
    But without a hub observation time the caption is just "live · cluster-visible signals"

  # source: pro:src/screens/hardening.jsx:33-44,112-119
  @pro
  Scenario Outline: The operator signal explains its verdict
    Given the operator lease verdict is <verdict>
    Then the signal "Operator / CSR approver running" is marked <mark> with the detail "<detail>"

    Examples:
      | verdict                                                   | mark     | detail                                                                                   |
      | active                                                    | ok       | operator lease held                                                                      |
      | inactive                                                  | warning  | operator lease stale or held by nobody                                                   |
      | missing                                                   | warning  | no operator lease found                                                                  |
      | unknown because the lease read was refused                | warning  | unknown: this cluster's credential cannot read the operator lease                        |
      | unknown for any other reason                              | warning  | no verdict yet — the cluster has not reported the operator lease, or the read failed     |
      | the unrecognised word "draining"                          | warning  | no verdict from state "draining" — a newer hub's value                                   |

  # source: pro:src/screens/hardening.jsx:120-122
  @pro
  Scenario Outline: The certificate request signal explains what is pending or why it is unknown
    Given the kubelet-serving request state is <state> with 5 approved and <pending> pending
    Then the signal is labelled "<label>", marked <mark>, with the detail "<detail>"

    Examples:
      | state         | pending | label                                        | mark    | detail                                                                         |
      | available     | 0       | No kubelet-serving CSR stuck pending         | ok      | 5 approved · 0 pending (kubelet-serving signer)                                |
      | available     | 2       | No kubelet-serving CSR stuck pending         | warning | 5 approved · 2 pending (kubelet-serving signer)                                |
      | forbidden     | unknown | Kubelet-serving CSR pending state unknown    | warning | CSR pending count unknown — the cluster credential cannot list requests        |
      | unknown       | unknown | Kubelet-serving CSR pending state unknown    | warning | CSR pending count unknown — no usable observation                              |

  # source: pro:src/screens/hardening.jsx:123-126
  @pro
  Scenario Outline: The PodSecurity signal names the enforcing namespaces
    Given <observation>
    Then the signal is labelled "<label>", marked <mark>, with the detail "<detail>"

    Examples:
      | observation                                             | label                                    | mark    | detail                                                                           |
      | namespaces "prod" and "payments" enforce "restricted"   | PodSecurity 'restricted' enforced        | ok      | prod, payments                                                                   |
      | no namespace enforces "restricted"                      | PodSecurity 'restricted' enforced        | warning | no namespace enforces PodSecurity 'restricted'                                   |
      | the namespaces could not be read                        | PodSecurity 'restricted' state unknown   | warning | PodSecurity restricted count unknown — the namespace read was unavailable        |

  # source: pro:src/screens/hardening.jsx:127-128
  @pro
  Scenario Outline: The control-plane signal reports labelled nodes
    Given <count> nodes carry the control-plane label
    Then the signal "Control-plane nodes labeled" is marked <mark> with the detail "<count> node(s) carrying the Stratum control-plane label"

    Examples:
      | count | mark    |
      | 3     | ok      |
      | 0     | warning |

  # source: pro:src/live.js:163-164,773,783-788
  # source: pro:src/fleet.js:169-172
  @pro
  Scenario: The hub's facts decide the operator verdict and the pending request count
    Given the hub answers the selected cluster's facts
    Then the operator verdict and holder, the pending kubelet-serving request count and its availability, and the observation time are the hub's
    And an availability the hub does not state reads as unknown
    And the approved request count, the PodSecurity counts and the control-plane count are still read from the cluster itself

  # source: pro:src/live.js:74-101,750-751,773
  @pro
  Scenario Outline: Without hub facts the operator verdict is derived from the operator's lease
    Given no hub facts are available for the cluster
    And the read of the lease "stratum-operator" in "kube-system" <lease>
    Then the operator verdict is <verdict> and the holder shown is <holder>

    Examples:
      | lease                                                                              | verdict                        | holder          |
      | returns a holder that renewed within twice the lease's duration                    | active                         | the holder      |
      | returns a holder that renewed 50 seconds ago with no stated duration               | active                         | the holder      |
      | returns a holder that renewed 70 seconds ago with no stated duration               | inactive                       | the holder      |
      | returns a holder whose renewal is stamped more than 30 seconds in the future       | unknown                        | the holder      |
      | returns a lease with no holder                                                     | inactive                       | "—"             |
      | returns a holder that never renewed                                                | inactive                       | the holder      |
      | answers not found                                                                  | missing                        | "—"             |
      | is forbidden                                                                       | unknown, flagged as refused    | "—"             |
      | fails any other way                                                                | unknown                        | "—"             |

  # source: pro:src/live.js:747-749,758-772
  @pro
  Scenario Outline: Without hub facts, kubelet-serving requests are counted from the cluster's certificate signing requests
    Given no hub facts are available for the cluster
    And a certificate signing request for the signer "kubernetes.io/kubelet-serving" has <conditions>
    Then it counts as <counted>

    Examples:
      | conditions                                          | counted                         |
      | an Approved condition                               | approved, not pending           |
      | a Denied condition                                  | neither approved nor pending    |
      | a Failed condition                                  | neither approved nor pending    |
      | no condition but an issued certificate              | neither approved nor pending    |
      | no condition and no certificate                     | pending                         |

  # source: pro:src/live.js:758
  @pro
  Scenario: Requests for other signers are ignored
    Given certificate signing requests exist for signers other than "kubernetes.io/kubelet-serving"
    Then they are counted in neither the approved nor the pending number

  # source: pro:src/live.js:747-749,770-772,783-784
  @pro
  Scenario Outline: Without hub facts, a request list that could not be read leaves the pending count unknown
    Given no hub facts are available for the cluster
    And listing certificate signing requests <failure>
    Then the pending state is "<state>" and no pending count is shown

    Examples:
      | failure                 | state     |
      | is forbidden            | forbidden |
      | fails any other way     | unknown   |

  # source: pro:src/live.js:437,757,780-782
  @pro
  Scenario: PodSecurity enforcement is read from namespace labels
    Given the cluster's namespaces were listed
    Then a namespace counts as restricted when its label "pod-security.kubernetes.io/enforce" is exactly "restricted"
    And the total is the number of namespaces listed
    But when the namespace list could not be read, both numbers and the list of names are unknown rather than zero
