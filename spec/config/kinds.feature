Feature: Config kinds and their list columns
  The sidebar's Config section: ConfigMaps and Secrets, plus the policy and
  cluster-plumbing kinds the dashboards file beside them (quotas, limits,
  autoscalers, disruption budgets, priority and runtime classes, leases and
  admission webhooks). Each has a list built on the generic resource list
  (see spec/resources).

  @oss @pro
  # source: oss:src/resources.jsx:114,118,123,128,132,139,145,150,154,158,162
  # source: oss:src/registry.js:47-59
  # source: oss:src/app.jsx:491
  # source: pro:src/resources.jsx:133,137,142,147,151,158,164,169,173,177,181
  # source: pro:src/nav.js:21-33
  # source: pro:src/app.jsx:514
  Scenario Outline: Each config kind has its own list
    When the user opens the route "<route>" or its row in the sidebar's "Config" section
    Then the generic list for kind "<kind>" is shown, titled "<title>"
    And the kind is <scope>

    Examples:
      | route                           | kind                           | title                      | scope          |
      | configmaps                      | ConfigMap                      | ConfigMaps                 | namespaced     |
      | secrets                         | Secret                         | Secrets                    | namespaced     |
      | resourcequotas                  | ResourceQuota                  | Resource Quotas            | namespaced     |
      | limitranges                     | LimitRange                     | Limit Ranges               | namespaced     |
      | horizontalpodautoscalers        | HorizontalPodAutoscaler        | Horizontal Pod Autoscalers | namespaced     |
      | poddisruptionbudgets            | PodDisruptionBudget            | Pod Disruption Budgets     | namespaced     |
      | priorityclasses                 | PriorityClass                  | Priority Classes           | cluster-scoped |
      | runtimeclasses                  | RuntimeClass                   | Runtime Classes            | cluster-scoped |
      | leases                          | Lease                          | Leases                     | namespaced     |
      | mutatingwebhookconfigurations   | MutatingWebhookConfiguration   | Mutating Webhooks          | cluster-scoped |
      | validatingwebhookconfigurations | ValidatingWebhookConfiguration | Validating Webhooks        | cluster-scoped |

  @oss @pro
  # source: oss:src/resources.jsx:114-165
  # source: pro:src/resources.jsx:133-184
  Scenario Outline: Each config kind shows its columns in order
    When the "<route>" list is shown
    Then its columns are, in order: <columns>

    Examples:
      | route                           | columns                                                        |
      | configmaps                      | Name, Namespace, Keys, Age                                     |
      | secrets                         | Name, Namespace, Type, Keys, Age                               |
      | resourcequotas                  | Name, Namespace, Scopes, Hard Limits, Age                      |
      | limitranges                     | Name, Namespace, Limits, Age                                   |
      | horizontalpodautoscalers        | Name, Namespace, Reference, Min, Max, Replicas, Age            |
      | poddisruptionbudgets            | Name, Namespace, Min Available, Max Unavailable, Current Healthy, Age |
      | priorityclasses                 | Name, Value, Global Default, Age                               |
      | runtimeclasses                  | Name, Handler, Age                                             |
      | leases                          | Name, Namespace, Holder, Age                                   |
      | mutatingwebhookconfigurations   | Name, Webhooks, Age                                            |
      | validatingwebhookconfigurations | Name, Webhooks, Age                                            |

  @oss @pro
  # source: oss:src/resources.jsx:116-164
  # source: pro:src/resources.jsx:135-183
  Scenario Outline: A config column shows a value read from the object
    Given the "<route>" list is shown
    Then the "<column>" column shows <value>

    Examples:
      | route                           | column          | value                                                                             |
      | configmaps                      | Keys            | the number of text keys plus the number of binary keys                            |
      | secrets                         | Type            | the secret's type, or "Opaque" when unset                                         |
      | secrets                         | Keys            | the number of keys in the secret's data                                           |
      | resourcequotas                  | Scopes          | the quota's scopes joined by ", ", or "—" when none                               |
      | resourcequotas                  | Hard Limits     | the number of hard limits in status; spec is consulted only when status carries no hard-limit map at all |
      | limitranges                     | Limits          | the number of limit entries in spec                                               |
      | horizontalpodautoscalers        | Reference       | "<kind>/<name>" of the scale target, with "?" for a missing part                  |
      | horizontalpodautoscalers        | Min             | the minimum replicas from spec, or "—" when unset                                 |
      | horizontalpodautoscalers        | Max             | the maximum replicas from spec, or "—" when unset                                 |
      | horizontalpodautoscalers        | Replicas        | the current replicas from status, or 0 when absent                                |
      | poddisruptionbudgets            | Min Available   | the minimum available from spec, as a number or a percentage, or "—" when unset   |
      | poddisruptionbudgets            | Max Unavailable | the maximum unavailable from spec, as a number or a percentage, or "—" when unset |
      | poddisruptionbudgets            | Current Healthy | the current healthy pod count from status, or 0 when absent                       |
      | priorityclasses                 | Value           | the class's priority value                                                        |
      | priorityclasses                 | Global Default  | "true" when the class is the global default, otherwise "false"                    |
      | runtimeclasses                  | Handler         | the class's handler                                                               |
      | leases                          | Holder          | the identity currently holding the lease, or "—" when none                        |
      | mutatingwebhookconfigurations   | Webhooks        | the number of webhooks in the configuration                                       |
      | validatingwebhookconfigurations | Webhooks        | the number of webhooks in the configuration                                       |

  @oss @pro
  # source: oss:src/resources.jsx:116
  # source: pro:src/resources.jsx:135
  Scenario: A config map with no data counts zero keys
    Given a config map with neither text nor binary data
    Then its Keys column reads "0"

  @oss @pro
  # source: oss:src/resources.jsx:132-138
  # source: pro:src/resources.jsx:151-157
  # assumption: no column reads the autoscaler's metrics, targets or desired replicas; inferred from their absence
  Scenario: An autoscaler's metrics appear only in its manifest
    Given a horizontal pod autoscaler with metric targets
    Then no list column shows its metrics or desired replica count

  @oss @pro
  # source: oss:src/resources.jsx:240-241,254-257
  # source: pro:src/resources.jsx:257-258,270-274
  Scenario: Config kinds have no actions of their own
    Given the session is offered write controls
    When any config kind's list is open
    Then Create is offered and a row's detail panel offers Edit and Delete
    And no Scale, Restart or contextual action is offered
