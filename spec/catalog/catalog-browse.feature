Feature: Browsing the App Catalog
  The catalog screen lists applications with an essentials strip, a search
  box, category chips, an Installed filter and two layouts, and opens a
  detail panel for the selected application.

  # source: pro:src/screens/catalog.jsx:547-553,567-584
  @pro
  Scenario: The catalog shows placeholders while the index is being read
    When the user opens the App Catalog and the index has not resolved yet
    Then the title "App Catalog" is shown with the subtitle "Loading…"
    And six placeholder cards are shown in place of the applications
    And "Refresh" is disabled

  # source: pro:src/screens/catalog.jsx:277-279,549-553
  @pro
  Scenario Outline: The subtitle summarises the catalog
    Given the catalog resolved <count> applications from <source>
    And <installed state>
    When the App Catalog is shown
    Then the subtitle reads "<subtitle>"

    Examples:
      | count | source        | installed state                                   | subtitle                                                       |
      | 1     | the ConfigMap | no catalog application is installed               | 1 application · source: ConfigMap · 0 installed                |
      | 12    | the ConfigMap | 3 catalog applications have a release             | 12 applications · source: ConfigMap · 3 installed              |
      | 12    | the URL       | 3 catalog applications have a release             | 12 applications · source: remote URL · 3 installed             |
      | 12    | the ConfigMap | the installed releases have not been read yet     | 12 applications · source: ConfigMap · install state unknown    |
      | 12    | the ConfigMap | the latest read of installed releases failed      | 12 applications · source: ConfigMap · install state unknown    |

  # source: pro:src/screens/catalog.jsx:291-294,373-414
  @pro
  Scenario: Core applications are featured as cluster essentials
    Given the catalog contains applications in the "Core" category
    And no search text, no category and no Installed filter is active
    When the App Catalog is shown
    Then a "Cluster essentials" strip captioned "Core apps most lab clusters install first" lists every Core application
    And each essentials card shows the application's mark, name, chart version and short description
    And the Core applications also appear in the "All applications" list below

  # source: pro:src/screens/catalog.jsx:291-294,373
  @pro
  Scenario Outline: The essentials strip is hidden whenever the list is narrowed or there is nothing to feature
    Given <condition>
    When the App Catalog is shown
    Then no "Cluster essentials" strip is shown

    Examples:
      | condition                                   |
      | the search box contains text                |
      | a category other than "All" is selected     |
      | the Installed filter is on                  |
      | the catalog has no "Core" application       |

  # source: pro:src/screens/catalog.jsx:63-70
  @pro
  Scenario: An application shows its logo, or its icon when it has no usable logo
    Given one application ships an inline image logo and another does not
    When the applications are listed
    Then the first shows its logo
    And the second shows its named icon tinted with its accent colour when it has one

  # source: pro:src/screens/catalog.jsx:281-289,429-431
  @pro
  Scenario Outline: Search matches several fields without regard to case
    Given an application with id "cert-manager", name "Cert Manager", short description "X.509 certificates", category "Security" and chart name "jetstack-cm"
    When the user types "<text>" in "Search apps..."
    Then the application <outcome>

    Examples:
      | text          | outcome        |
      | cert-man      | is listed      |
      | MANAGER       | is listed      |
      | x.509         | is listed      |
      | security      | is listed      |
      | jetstack      | is listed      |
      |   manager     | is listed      |
      | prometheus    | is not listed  |

  # source: pro:src/screens/catalog.jsx:287
  @pro
  Scenario: Search does not look at the long description, the notes or the chart repository
    Given an application whose long description, notes and chart repository contain "airgap" and whose other fields do not
    When the user searches for "airgap"
    Then the application is not listed

  # source: pro:src/screens/catalog.jsx:271-275,433-440
  @pro
  Scenario: Category chips show every category with its number of applications
    Given the catalog holds 2 "Core", 3 "Monitoring" and 1 "Other" applications, first appearing in that order
    When the App Catalog is shown
    Then the chips read "ALL 6", "CORE 2", "MONITORING 3" and "OTHER 1" in that order
    And "ALL" is selected

  # source: pro:src/screens/catalog.jsx:271-275
  @pro
  Scenario: Chip counts ignore the other filters
    Given the user has typed a search that matches one application
    Then every category chip still shows its total number of applications

  # source: pro:src/screens/catalog.jsx:285,435
  @pro
  Scenario: Choosing a category narrows the list to that category
    When the user selects the "MONITORING" chip
    Then only applications in the "Monitoring" category are listed
    And selecting "ALL" lists every application again

  # source: pro:src/screens/catalog.jsx:262-269,284,442-446
  @pro
  Scenario: The Installed filter keeps applications that have a release in any state
    Given one application is deployed, one is installing, one failed, one is being removed and one has no release
    When the user turns on "Installed"
    Then the four applications that have a release are listed and the fifth is not
    And the "Installed" button shows the count 4

  # source: pro:src/screens/catalog.jsx:176,445
  @pro
  Scenario: The Installed count is a dash while the installed releases are unknown
    Given the installed releases have not been read yet or their latest read failed
    Then the "Installed" button shows "—" instead of a count

  # source: pro:src/screens/catalog.jsx:171,245,284
  @pro
  Scenario: The Installed filter falls back to the last releases that were read
    Given the installed releases were read successfully and a later read failed
    When the user turns on "Installed"
    Then the applications installed according to the last successful read are listed

  # source: pro:src/screens/catalog.jsx:171,284,309-313
  @pro
  Scenario: The Installed filter lists nothing before the first successful read
    Given the installed releases have never been read successfully on this cluster
    When the user turns on "Installed"
    Then no application is listed and the message "No apps match this filter." is shown

  # source: pro:src/screens/catalog.jsx:281-289
  @pro
  Scenario: Search, category and the Installed filter combine
    When the user sets a search text, a category and the Installed filter together
    Then only applications that satisfy all three are listed

  # source: pro:src/screens/catalog.jsx:308-314,538,604
  @pro
  Scenario Outline: An empty result explains itself and offers to clear the filters
    Given the active filters match no application
    And the search box contains <search>
    Then the message reads <message>
    And a "Clear filters" link is shown

    Examples:
      | search          | message                           |
      | "zzz"           | No apps match "zzz".              |
      | "  zzz  "       | No apps match "zzz".              |
      | nothing         | No apps match this filter.        |

  # source: pro:src/screens/catalog.jsx:308
  @pro
  Scenario: Clear filters resets search, category and the Installed filter
    Given a search text, a category and the Installed filter are active
    When the user follows "Clear filters"
    Then the search box is empty, "ALL" is selected and the Installed filter is off
    And the layout and the open application are unchanged

  # source: pro:src/screens/catalog.jsx:159-161,601-606
  @pro
  Scenario: The list layout is the default
    Given the user has never chosen a layout in this browser
    When the App Catalog is shown
    Then the applications are shown as a table

  # source: pro:src/screens/catalog.jsx:46,159-161,185,447-456
  @pro
  Scenario: The chosen layout is remembered in the browser
    When the user switches to "Card view"
    Then the applications are shown as cards
    And the App Catalog opens in card view on later visits from the same browser, on any cluster
    And switching to "List view" is remembered the same way

  # source: pro:src/screens/catalog.jsx:159-161,185
  @pro
  Scenario: The layout still works when the browser refuses storage
    Given the browser does not allow local storage
    When the user switches layout
    Then the layout changes for the current visit and no error is shown

  # source: pro:src/screens/catalog.jsx:503-537
  @pro
  Scenario: The table lists one row per application
    When the applications are shown as a table
    Then the columns are "Application", "Version", "Category", "Status" and "Action"
    And each row shows the application's mark, name and short description, its chart version and its category
    And an application without a release shows "—" as its status

  # source: pro:src/screens/catalog.jsx:469-499
  @pro
  Scenario: A card shows the application and its chart
    When the applications are shown as cards
    Then each card shows the application's mark, name, chart name and version, short description and category

  # source: pro:src/screens/catalog.jsx:296-304,383,474,516,616-622
  @pro
  Scenario Outline: Selecting an application opens its detail panel
    When the user <action> an application's row or card, in the table, the cards or the essentials strip
    Then that application is highlighted
    And its detail panel opens beside the list, which stays usable

    Examples:
      | action                              |
      | clicks                              |
      | focuses and presses Enter on        |
      | focuses and presses Space on        |

  # source: pro:src/screens/catalog.jsx:254-260,709-712
  @pro
  Scenario Outline: The detail panel can be dismissed
    Given an application's detail panel is open
    When the user <action>
    Then the panel closes and no application is highlighted

    Examples:
      | action                          |
      | presses Escape                  |
      | presses the panel's Close button |

  # source: pro:src/screens/catalog.jsx:296,616-617
  @pro
  Scenario: The detail panel stays open for an application that the filters hide
    Given an application's detail panel is open
    When the user changes the filters so that the application is no longer listed
    Then the detail panel remains open for it

  # source: pro:src/screens/catalog.jsx:296,616
  @pro
  Scenario: The detail panel closes when a refreshed catalog no longer contains the application
    Given an application's detail panel is open
    When the catalog is refreshed and the application is no longer in the index
    Then the detail panel is no longer shown

  # source: pro:src/screens/catalog.jsx:147-151
  @pro
  Scenario: The shell's namespace selection has no effect on the catalog
    When the user changes the namespace selected in the application shell
    Then the catalog's applications, installed states and install targets are unchanged
