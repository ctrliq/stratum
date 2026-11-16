Feature: RBAC tabbed screen
  An older screen that shows the five access-control lists as tabs under one
  title. It is no longer in the sidebar but still answers its route, old
  links and the command palette.

  @oss @pro
  # source: oss:src/app.jsx:487
  # source: oss:src/screens/tabbed.jsx:11,13-26
  # source: pro:src/app.jsx:512
  # source: pro:src/screens/tabbed.jsx:11,13-23
  Scenario: The rbac route shows five tabs with the first selected
    When the user opens the route "rbac" with no tab in the address
    Then a screen headed "RBAC" shows the tabs "Roles", "Role Bindings", "Cluster Roles", "Cluster Role Bindings" and "Service Accounts", in that order
    And "Roles" is selected and its list is shown below

  @oss @pro
  # source: oss:src/app.jsx:265,487
  # source: oss:src/screens/tabbed.jsx:14-15
  # source: pro:src/app.jsx:166,512
  # source: pro:src/screens/tabbed.jsx:14-15
  Scenario Outline: The address chooses the tab
    When the user opens the route "rbac" with ?tab=<tab>
    Then the tab "<selected>" is selected and its list is shown

    Examples:
      | tab                 | selected              |
      | roles               | Roles                 |
      | rolebindings        | Role Bindings         |
      | clusterroles        | Cluster Roles         |
      | clusterrolebindings | Cluster Role Bindings |
      | serviceaccounts     | Service Accounts      |
      | storageclasses      | Roles                 |
      | nonsense            | Roles                 |

  @oss @pro
  # source: oss:src/app.jsx:375-382,487
  # source: oss:src/screens/tabbed.jsx:23
  # source: pro:src/app.jsx:345-354,512
  # source: pro:src/screens/tabbed.jsx:20
  Scenario: Choosing a tab puts it in the address
    Given the rbac screen is shown with ?ns=payments and ?name=payments/reader
    When the user chooses the tab "Cluster Roles"
    Then the address becomes the route "rbac" with ?ns=payments and ?tab=clusterroles
    And ?name= is no longer in the address
    And the browser's Back button returns to the previous tab

  @oss @pro
  # source: oss:src/screens/tabbed.jsx:28-29
  # source: pro:src/screens/tabbed.jsx:26-27
  Scenario: Each tab is the kind's full generic list
    Given the rbac screen is shown
    Then the selected tab's list behaves exactly as the kind's own route does: loading, refresh, search, namespace filter, detail, create, edit and delete
    And the list keeps its own title under the "RBAC" heading

  @oss @pro
  # source: oss:src/screens/tabbed.jsx:28
  # source: pro:src/screens/tabbed.jsx:26
  Scenario: Switching tab starts the new list from clean
    Given a search is typed or a panel is open in one tab
    When the user chooses another tab
    Then the new tab's list has an empty search and no open panel and loads afresh

  @oss @pro
  # source: oss:src/app.jsx:264,487
  # source: oss:src/screens/tabbed.jsx:28-29
  # source: pro:src/app.jsx:165,512
  # source: pro:src/screens/tabbed.jsx:26-27
  Scenario: A link can open an object inside a tab
    When the user opens the route "rbac" with ?tab=clusterroles and ?name=<cluster role name>
    Then the Cluster Roles tab is selected
    And the detail panel opens for that cluster role once the list loads

  @oss @pro
  # source: oss:src/screens/tabbed.jsx:28
  # source: oss:src/ui/resource-list.jsx:118
  # source: pro:src/screens/tabbed.jsx:26
  # source: pro:src/ui/resource-list.jsx:105
  Scenario: The selected namespace narrows only the namespaced tabs
    Given a namespace is selected
    Then the Roles, Role Bindings and Service Accounts tabs show only that namespace
    And the Cluster Roles and Cluster Role Bindings tabs show everything

  @oss @pro
  # source: oss:src/app.jsx:124-129,496
  # source: pro:src/app.jsx:66-71,519
  Scenario Outline: The sidebar lights the row of the tab on screen
    When the rbac screen is shown with <tab in address>
    Then the sidebar row "<row>" is marked as current

    Examples:
      | tab in address            | row                   |
      | no tab                    | Roles                 |
      | ?tab=rolebindings         | Role Bindings         |
      | ?tab=clusterroles         | Cluster Roles         |
      | ?tab=clusterrolebindings  | Cluster Role Bindings |
      | ?tab=serviceaccounts      | Service Accounts      |
      | ?tab=nonsense             | Roles                 |

  @oss @pro
  # source: oss:src/app.jsx:114-119,462
  # source: pro:src/app.jsx:41-46,474
  Scenario: The breadcrumb reads RBAC whatever the tab
    When the rbac screen is shown on any tab
    Then the breadcrumb's last entry reads "RBAC"

  @oss @pro
  # source: oss:src/registry.js:27-28,78-84
  # source: oss:src/ui/palette.jsx:23
  # source: pro:src/nav.js:52-58
  # source: pro:src/ui/palette.jsx:25
  Scenario: The rbac screen is reached from the palette, not the sidebar
    Then no sidebar row leads to the route "rbac"
    And the command palette's "RBAC" entry opens it
