Feature: Storage tabbed screen
  An older screen that shows the three storage lists as tabs under one
  title. It is no longer in the sidebar but still answers its route, old
  links and the command palette.

  @oss @pro
  # source: oss:src/app.jsx:486
  # source: oss:src/screens/tabbed.jsx:10,13-26
  # source: pro:src/app.jsx:511
  # source: pro:src/screens/tabbed.jsx:10,13-23
  Scenario: The storage route shows three tabs with the first selected
    When the user opens the route "storage" with no tab in the address
    Then a screen headed "Storage" shows the tabs "Persistent Volume Claims", "Persistent Volumes" and "Storage Classes", in that order
    And "Persistent Volume Claims" is selected and its list is shown below

  @oss @pro
  # source: oss:src/app.jsx:265,486
  # source: oss:src/screens/tabbed.jsx:14-15
  # source: pro:src/app.jsx:166,511
  # source: pro:src/screens/tabbed.jsx:14-15
  Scenario Outline: The address chooses the tab
    When the user opens the route "storage" with ?tab=<tab>
    Then the tab "<selected>" is selected and its list is shown

    Examples:
      | tab                    | selected                 |
      | persistentvolumeclaims | Persistent Volume Claims |
      | persistentvolumes      | Persistent Volumes       |
      | storageclasses         | Storage Classes          |
      | roles                  | Persistent Volume Claims |
      | nonsense               | Persistent Volume Claims |

  @oss @pro
  # source: oss:src/app.jsx:375-382,486
  # source: oss:src/screens/tabbed.jsx:23
  # source: pro:src/app.jsx:345-354,511
  # source: pro:src/screens/tabbed.jsx:20
  Scenario: Choosing a tab puts it in the address
    Given the storage screen is shown with ?ns=payments and ?name=payments/data
    When the user chooses the tab "Persistent Volumes"
    Then the address becomes the route "storage" with ?ns=payments and ?tab=persistentvolumes
    And ?name= is no longer in the address
    And the browser's Back button returns to the previous tab

  @oss @pro
  # source: oss:src/screens/tabbed.jsx:28-29
  # source: pro:src/screens/tabbed.jsx:26-27
  Scenario: Each tab is the kind's full generic list
    Given the storage screen is shown
    Then the selected tab's list behaves exactly as the kind's own route does: loading, refresh, search, namespace filter, detail, create, edit and delete
    And the list keeps its own title under the "Storage" heading

  @oss @pro
  # source: oss:src/screens/tabbed.jsx:28
  # source: pro:src/screens/tabbed.jsx:26
  Scenario: Switching tab starts the new list from clean
    Given a search is typed or a panel is open in one tab
    When the user chooses another tab
    Then the new tab's list has an empty search and no open panel and loads afresh

  @oss @pro
  # source: oss:src/app.jsx:264,486
  # source: oss:src/screens/tabbed.jsx:28-29
  # source: pro:src/app.jsx:165,511
  # source: pro:src/screens/tabbed.jsx:26-27
  Scenario: A link can open an object inside a tab
    When the user opens the route "storage" with ?tab=persistentvolumes and ?name=<volume name>
    Then the Persistent Volumes tab is selected
    And the detail panel opens for that volume once the list loads

  @oss @pro
  # source: oss:src/screens/tabbed.jsx:28
  # source: oss:src/ui/resource-list.jsx:118
  # source: pro:src/screens/tabbed.jsx:26
  # source: pro:src/ui/resource-list.jsx:105
  Scenario: The selected namespace narrows only the claims tab
    Given a namespace is selected
    Then the Persistent Volume Claims tab shows only that namespace
    And the Persistent Volumes and Storage Classes tabs show everything

  @oss @pro
  # source: oss:src/app.jsx:124-129,496
  # source: pro:src/app.jsx:66-71,519
  Scenario Outline: The sidebar lights the row of the tab on screen
    When the storage screen is shown with <tab in address>
    Then the sidebar row "<row>" is marked as current

    Examples:
      | tab in address          | row                      |
      | no tab                  | Persistent Volume Claims |
      | ?tab=persistentvolumes  | Persistent Volumes       |
      | ?tab=storageclasses     | Storage Classes          |
      | ?tab=nonsense           | Persistent Volume Claims |

  @oss @pro
  # source: oss:src/app.jsx:114-119,462
  # source: pro:src/app.jsx:41-46,474
  Scenario: The breadcrumb reads Storage whatever the tab
    When the storage screen is shown on any tab
    Then the breadcrumb's last entry reads "Storage"

  @oss @pro
  # source: oss:src/registry.js:27-28,68-72
  # source: oss:src/ui/palette.jsx:22
  # source: pro:src/nav.js:42-46
  # source: pro:src/ui/palette.jsx:24
  Scenario: The storage screen is reached from the palette, not the sidebar
    Then no sidebar row leads to the route "storage"
    And the command palette's "Storage" entry opens it
