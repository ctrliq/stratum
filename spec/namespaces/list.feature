Feature: Namespaces list
  Namespaces are a kind like any other: a cluster-scoped list built on the
  generic resource list (see spec/resources).

  @oss @pro
  # source: oss:src/resources.jsx:204-208
  # source: oss:src/registry.js:33
  # source: oss:src/app.jsx:491
  # source: pro:src/resources.jsx:223-227
  # source: pro:src/nav.js:7
  # source: pro:src/app.jsx:514
  Scenario: Namespaces have their own list in the sidebar's top section
    When the user opens the route "namespaces" or the sidebar row "Namespaces"
    Then the generic list for kind "Namespace" is shown, titled "Namespaces"
    And its columns are, in order: Name, Status, Labels, Age

  @oss @pro
  # source: oss:src/resources.jsx:206
  # source: pro:src/resources.jsx:225
  Scenario Outline: Namespace status is its phase
    Given a namespace that <phase>
    Then its Status badge reads "<badge>"

    Examples:
      | phase                       | badge       |
      | reports phase "Active"      | Active      |
      | reports phase "Terminating" | Terminating |
      | reports no phase            | Active      |

  @oss @pro
  # source: oss:src/resources.jsx:207
  # source: oss:src/k8sutil.js:127
  # source: pro:src/resources.jsx:226
  # source: pro:src/k8sutil.js:23
  Scenario Outline: The Labels column shows the first three labels
    Given a namespace with <labels>
    Then its Labels column shows <shown>

    Examples:
      | labels      | shown                                                                |
      | no labels   | nothing                                                              |
      | two labels  | both as "<key>=<value>" joined by ", "                               |
      | five labels | the first three as "<key>=<value>" joined by ", ", with no sign of more |

  @oss @pro
  # source: oss:src/resources.jsx:204
  # source: oss:src/ui/resource-list.jsx:118,157
  # source: pro:src/resources.jsx:223
  # source: pro:src/ui/resource-list.jsx:105,149
  Scenario: The Namespaces list ignores the selected namespace
    Given a namespace is selected in the top bar
    When the Namespaces list is shown
    Then every namespace is a row
    And the line under the title does not name the selected namespace

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:77,190
  # source: pro:src/ui/resource-list.jsx:178
  # assumption: a row click only opens the detail panel; nothing links a namespace row to the namespace selector, inferred from the absence of any such wiring
  Scenario: Clicking a namespace opens its detail and does not select it
    When the user clicks a namespace's row
    Then the detail panel opens with the namespace's summary, labels and manifest
    And the namespace selected in the top bar does not change

  @oss @pro
  # source: oss:src/resources.jsx:254-257
  # source: oss:src/ui/resource-list.jsx:22-26,221-224
  # source: pro:src/resources.jsx:270-274
  # source: pro:src/ui/resource-list.jsx:24-28,209-212
  Scenario: A namespace is created from the generic skeleton
    Given the session is offered write controls
    When the user presses "Create" on the Namespaces list
    Then the editor holds JSON with apiVersion "v1", kind "Namespace", an empty name and no namespace field

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:145
  # source: pro:src/ui/resource-list.jsx:132
  # assumption: deleting a namespace uses the generic confirmation with no warning about its contents; inferred from the absence of kind-specific handling
  Scenario: Deleting a namespace uses the ordinary confirmation
    Given the session is offered write controls
    When the user presses "Delete" in a namespace's detail panel
    Then the dialog titled "Delete Namespace?" says "<name> will be permanently deleted."
    And nothing warns that everything inside the namespace is deleted with it

  @oss @pro
  # source: oss:src/resources.jsx:206
  # source: oss:src/ui/resource-list.jsx:128,130
  # source: pro:src/resources.jsx:225
  # source: pro:src/ui/resource-list.jsx:115,117
  Scenario: A namespace being deleted stays in the list as Terminating
    Given a namespace's delete was accepted and the cluster is still emptying it
    Then the list keeps its row with the Status badge "Terminating" until a refresh no longer returns it

  @oss @pro
  # source: oss:src/app.jsx:262,384-389
  # source: pro:src/app.jsx:163,356-361
  # assumption: nothing clears the selected namespace when that namespace is deleted; inferred from the absence of any such handling
  Scenario: Deleting the selected namespace leaves it selected
    Given the namespace "payments" is selected in the top bar
    When "payments" is deleted from the Namespaces list
    Then the address still carries ?ns=payments and the selector still reads "payments"
    And namespaced lists show no rows until another namespace is chosen

  @oss @pro
  # source: oss:src/resources.jsx:240-241,254-257
  # source: pro:src/resources.jsx:257-258,270-274
  Scenario: Namespaces have no actions of their own
    Given the session is offered write controls
    When a namespace's detail panel is open
    Then it offers Edit and Delete and no Scale, Restart or contextual action
