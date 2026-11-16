Feature: Access-control kinds and their list columns
  Service Accounts, Roles, Role Bindings, Cluster Roles and Cluster Role
  Bindings each have a list built on the generic resource list (see
  spec/resources). This feature records their routes, columns and how each
  column value is derived.

  @oss @pro
  # source: oss:src/resources.jsx:190,194,196,198,200
  # source: oss:src/app.jsx:491
  # source: pro:src/resources.jsx:209,213,215,217,219
  # source: pro:src/app.jsx:514
  Scenario Outline: Each access-control kind has its own list
    When the user opens the route "<route>"
    Then the generic list for kind "<kind>" is shown, titled "<title>"
    And the kind is <scope>

    Examples:
      | route               | kind               | title                 | scope          |
      | serviceaccounts     | ServiceAccount     | Service Accounts      | namespaced     |
      | roles               | Role               | Roles                 | namespaced     |
      | rolebindings        | RoleBinding        | Role Bindings         | namespaced     |
      | clusterroles        | ClusterRole        | Cluster Roles         | cluster-scoped |
      | clusterrolebindings | ClusterRoleBinding | Cluster Role Bindings | cluster-scoped |

  @oss @pro
  # source: oss:src/registry.js:78-84
  # source: pro:src/nav.js:52-58
  Scenario: The sidebar's Access Control section lists the five kinds
    Then the sidebar section "Access Control" has the rows Service Accounts, Cluster Roles, Roles, Cluster Role Bindings and Role Bindings, in that order

  @oss @pro
  # source: oss:src/resources.jsx:190-201
  # source: pro:src/resources.jsx:209-220
  Scenario Outline: Each access-control kind shows its columns in order
    When the "<route>" list is shown
    Then its columns are, in order: <columns>

    Examples:
      | route               | columns                       |
      | serviceaccounts     | Name, Namespace, Secrets, Age |
      | roles               | Name, Namespace, Rules, Age   |
      | rolebindings        | Name, Namespace, Role, Age    |
      | clusterroles        | Name, Rules, Age              |
      | clusterrolebindings | Name, Role, Age               |

  @oss @pro
  # source: oss:src/resources.jsx:192,195,197,199,201
  # source: pro:src/resources.jsx:211,214,216,218,220
  Scenario Outline: An access-control column shows a value read from the object
    Given the "<route>" list is shown
    Then the "<column>" column shows <value>

    Examples:
      | route               | column  | value                                                        |
      | serviceaccounts     | Secrets | the number of secrets the account references, 0 when none    |
      | roles               | Rules   | the number of rules in the role, 0 when none                 |
      | clusterroles        | Rules   | the number of rules in the cluster role, 0 when none         |
      | rolebindings        | Role    | the name of the role the binding refers to, "—" when absent  |
      | clusterrolebindings | Role    | the name of the role the binding refers to, "—" when absent  |

  @oss @pro
  # source: oss:src/resources.jsx:197,201
  # source: pro:src/resources.jsx:216,220
  # assumption: no column reads a binding's subjects or the kind of role it refers to; inferred from their absence
  Scenario: A binding's subjects appear only in its manifest
    Given a role binding that grants a cluster role to two users
    Then its row shows the role's name without saying it is a cluster role
    And who the binding applies to can be read only in the detail panel's manifest

  @oss @pro
  # source: oss:src/resources.jsx:199
  # source: pro:src/resources.jsx:218
  Scenario: An aggregated cluster role counts the rules it currently holds
    Given a cluster role whose rules are filled in by aggregation
    Then its Rules column shows the number of rules present on the object when it was read

  @oss @pro
  # source: oss:src/resources.jsx:240-241,254-257
  # source: pro:src/resources.jsx:257-258,270-274
  Scenario: Access-control kinds have no actions of their own
    Given the session is offered write controls
    When any access-control kind's list is open
    Then Create is offered and a row's detail panel offers Edit and Delete
    And no Scale, Restart or contextual action is offered

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:128,133-137
  # source: pro:src/ui/resource-list.jsx:115,120-124
  # assumption: the dashboard applies no extra confirmation or self-lockout check when access rules are edited or deleted; inferred from the absence of any kind-specific handling
  Scenario: Changing access rules gets no extra safeguard
    Given the session is offered write controls
    When the user edits or deletes a role or binding that grants their own access
    Then the change goes through the same edit and delete flow as any other kind
