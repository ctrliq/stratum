Feature: URL parameters
  The query string carries the state a link must be able to restore: the
  namespace filter and what a screen should open on.

  @oss @pro
  # source: oss:src/app.jsx:261-265
  # source: pro:src/app.jsx:162-166
  Scenario Outline: Parameters the shell reads on every screen
    When a screen URL carries "<parameter>"
    Then <effect>

    Examples:
      | parameter               | effect                                                       |
      | ns=<namespace>          | the namespace filter is set to that namespace                |
      | node=<name>             | the nodes screen opens with that node's detail               |
      | name=<namespace>/<name> | a resource list opens with that row selected                 |
      | tab=<id>                | the storage or RBAC tabbed screen opens on that tab          |

  @pro @fleet
  # source: pro:src/app.jsx:167-169,482
  Scenario: The operations page tracks the operations named in the URL
    When "/operations" carries one or more "op=<operation id>" parameters
    Then the operations page tracks each of those operations

  @oss @pro
  # source: oss:src/app.jsx:262
  # source: pro:src/app.jsx:163
  Scenario: A missing or empty namespace parameter means all namespaces
    When the URL has no "ns" parameter or an empty one
    Then the namespace filter is "All namespaces"

  @diverged @oss @drop
  # decision: D1
  # decision: D11
  # source: oss:src/app.jsx:375-382
  Scenario: In-app navigation in the open dashboard keeps only the namespace filter
    Given a namespace filter is set and the URL carries other parameters
    When the user navigates to another screen through the sidebar, the palette or a screen's own links
    Then the new URL is "/<screen id>?ns=<namespace>"
    And "node", "name" and "tab" are dropped unless the link sets them

  @diverged @pro
  # decision: D11
  # source: pro:src/app.jsx:345-354
  Scenario: In-app navigation in the fleet dashboard keeps the cluster and the namespace filter
    Given a namespace filter is set and the URL carries other parameters
    When the user navigates to another screen through the sidebar, the palette or a screen's own links
    Then the new URL is "/c/<url-encoded current cluster id>/<screen id>?ns=<namespace>"
    And "node", "name" and "tab" are dropped unless the link sets them

  @oss @pro
  # source: oss:src/app.jsx:377
  # source: pro:src/app.jsx:349
  Scenario: No namespace parameter is written when all namespaces are shown
    Given the namespace filter is "All namespaces"
    When the user navigates to another screen
    Then the new URL has no "ns" parameter

  @pro @fleet
  # source: pro:src/app.jsx:346-347
  Scenario: Going to the fleet or operations page drops every parameter
    Given a namespace filter is set
    When the user navigates to the fleet page or the operations page
    Then the URL is exactly "/clusters" or "/operations"

  @oss @pro
  # source: oss:src/app.jsx:378
  # source: pro:src/app.jsx:350
  Scenario: A link that sets a parameter to nothing removes it
    When a screen navigates with a parameter whose value is empty
    Then that parameter is absent from the new URL

  @oss @pro
  # source: oss:src/app.jsx:382
  # source: pro:src/app.jsx:354
  Scenario: In-app navigation adds a history entry
    When the user navigates to another screen
    Then the browser's back button returns to the previous screen

  @oss @pro
  # source: oss:src/app.jsx:383,483
  # source: pro:src/app.jsx:355,508
  Scenario: Opening a node puts it in the URL
    When the user opens a node from the overview or selects one on the nodes screen
    Then the URL becomes the nodes screen with "node=<name>"

  @oss @pro
  # source: oss:src/app.jsx:485
  # source: pro:src/app.jsx:510
  Scenario: Opening a workload links to its kind's list with the row preselected
    When the user opens a workload from the workloads overview
    Then the URL becomes that kind's list with "name=<namespace>/<name>"
    And a workload of a kind without its own list opens the deployments list
