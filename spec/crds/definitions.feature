Feature: Custom resource definitions list
  The cluster's custom resource definitions are a kind like any other, shown
  by the generic resource list (see spec/resources), with one extra action:
  browsing a definition's instances.

  @oss @pro
  # source: oss:src/resources.jsx:211-220
  # source: oss:src/registry.js:85-87
  # source: oss:src/app.jsx:491
  # source: pro:src/resources.jsx:230-239
  # source: pro:src/nav.js:59-61
  # source: pro:src/app.jsx:514
  Scenario: Definitions have their own list under Custom Resources
    When the user opens the route "crds" or the sidebar row "Definitions" under "Custom Resources"
    Then the generic list for kind "CustomResourceDefinition" is shown, titled "Custom Resource Definitions"
    And it is cluster-scoped
    And its columns are, in order: Name, Group, Kind, Scope, Versions, Age

  @oss @pro
  # source: oss:src/resources.jsx:216-219
  # source: pro:src/resources.jsx:235-238
  Scenario Outline: A definition column shows a value read from the definition
    Given the Definitions list is shown
    Then the "<column>" column shows <value>

    Examples:
      | column   | value                                                                    |
      | Group    | the API group the definition belongs to, or "—" when absent              |
      | Kind     | the kind name the definition declares                                    |
      | Scope    | "Namespaced" or "Cluster" as the definition declares, or "—" when absent |
      | Versions | every version name the definition lists, served or not, joined by ", "   |

  @oss @pro
  # source: oss:src/resources.jsx:213
  # source: oss:src/ui/resource-list.jsx:93,192-203,287-292
  # source: pro:src/resources.jsx:232
  # source: pro:src/ui/resource-list.jsx:74,180-191,280-285
  Scenario: Every definition offers Browse instances to every user
    Given the Definitions list is shown, with or without write controls
    Then every row ends with a "Browse instances" icon button
    And a definition's detail panel offers a "Browse instances" button

  @oss @pro
  # source: oss:src/app.jsx:437-440
  # source: oss:src/crd-nav.js:10-17
  # source: pro:src/app.jsx:427-431
  Scenario Outline: Browse instances opens the definition's first served version
    Given a definition in group "example.io" with plural "widgets" whose versions are <versions>
    When the user presses "Browse instances" for it
    Then the instance list for "example.io", version "<version>", "widgets" opens

    Examples:
      | versions                                  | version |
      | v1 served                                 | v1      |
      | v1alpha1 not served, then v1 served       | v1      |
      | v1beta1 served, then v1 served            | v1beta1 |
      | v1alpha1 and v1beta1, neither served      | v1alpha1 |
      | not listed at all                         | v1      |

  @oss @pro @drop
  # decision: D2 — the only in-app navigation that loses the namespace; not reproduced
  # source: oss:src/app.jsx:439
  # source: pro:src/app.jsx:430
  Scenario: Browse instances drops the selected namespace
    Given the namespace "payments" is selected
    When the user presses "Browse instances" for a definition
    Then the instance list opens with no namespace selected

  @oss @pro @fixed
  # decision: D2 — replaces the scenario above
  # source: oss:src/app.jsx:439
  # source: pro:src/app.jsx:430
  Scenario: Browse instances keeps the selected namespace
    Given the namespace "payments" is selected
    When the user presses "Browse instances" for a definition
    Then the instance list opens with "payments" still selected

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:77,190
  # source: pro:src/ui/resource-list.jsx:178
  Scenario: Clicking a definition's row opens its manifest, not its instances
    When the user clicks a definition's row away from the action button
    Then the detail panel opens with the definition's summary and full manifest, schema included

  @oss @pro
  # source: oss:src/resources.jsx:254-257
  # source: pro:src/resources.jsx:270-274
  Scenario: Definitions can be created, edited and deleted like any kind
    Given the session is offered write controls
    When the Definitions list is open
    Then Create is offered and a definition's detail panel offers Edit and Delete

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:145
  # source: pro:src/ui/resource-list.jsx:132
  # assumption: deleting a definition uses the generic confirmation with no warning about its instances; inferred from the absence of kind-specific handling
  Scenario: Deleting a definition uses the ordinary confirmation
    Given the session is offered write controls
    When the user presses "Delete" in a definition's detail panel
    Then the dialog titled "Delete CustomResourceDefinition?" says "<definition name> will be permanently deleted."
    And nothing warns that every instance of the kind is deleted with it

  @oss @pro @drop
  # decision: D9 — a forbidden kind gets its own state
  # source: oss:src/ui/resource-list.jsx:107,206-212
  # source: pro:src/ui/resource-list.jsx:94,194-200
  Scenario: A user who may not list definitions sees the generic failure
    Given the signed-in user is not allowed to list custom resource definitions
    When the Definitions list is opened
    Then the table shows "Unable to load — the apiserver returned an error (or the RBAC lacks access)."

  @oss @pro @fixed
  # decision: D9 — replaces the scenario above; the state itself is specified in spec/resources/list.feature
  # source: oss:src/ui/resource-list.jsx:107,206-212
  # source: pro:src/ui/resource-list.jsx:94,194-200
  Scenario: A user who may not list definitions sees the forbidden state
    Given the signed-in user is not allowed to list custom resource definitions
    When the Definitions list is opened
    Then the table says the user is not permitted to list definitions, in words distinct from a failed load
