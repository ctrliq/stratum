Feature: Namespace selector
  The top bar carries one namespace selector for the whole dashboard. Its
  choice lives in the address (?ns=) and narrows every namespaced list.

  # ---- what it offers --------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/shell.jsx:311-326
  # source: oss:src/app.jsx:498
  # source: pro:src/ui/shell.jsx:246-261
  # source: pro:src/app.jsx:521
  Scenario: The selector lists All namespaces and then every namespace
    Given the cluster snapshot holds the cluster's namespaces
    When the user opens the namespace selector
    Then the first option is "All namespaces"
    And one option follows for each namespace, in the order the cluster returned them

  @oss @pro
  # source: oss:src/ui/shell.jsx:312-316
  # source: oss:src/app.jsx:262
  # source: pro:src/ui/shell.jsx:247-251
  # source: pro:src/app.jsx:163
  Scenario Outline: The selector's button shows the current choice
    Given the address carries <ns in address>
    Then the selector's button reads "<label>"

    Examples:
      | ns in address | label          |
      | no ?ns=       | All namespaces |
      | ?ns=payments  | payments       |
      | an empty ?ns= | All namespaces |

  @oss @pro
  # source: oss:src/ui/shell.jsx:61-69,321
  # source: pro:src/ui/shell.jsx:49-57,256
  Scenario: The current choice is marked in the open selector
    Given the namespace "payments" is selected
    When the user opens the selector
    Then the option "payments" is marked as the current one

  @oss @pro
  # source: oss:src/ui/shell.jsx:38-59
  # source: pro:src/ui/shell.jsx:26-47
  Scenario Outline: The open selector closes without changing anything
    Given the selector is open
    When the user <dismissal>
    Then the selector closes and the selected namespace is unchanged

    Examples:
      | dismissal                   |
      | clicks outside the selector |
      | presses Escape              |
      | clicks the selector's button again |

  @oss @pro
  # source: oss:src/ui/shell.jsx:318-325
  # source: pro:src/ui/shell.jsx:253-260
  # assumption: the selector has no filter box and no keyboard list navigation of its own; inferred from the absence of either
  Scenario: The selector cannot be searched
    Given a cluster with many namespaces
    Then the selector shows them all in one scrolling menu with no way to filter by typing

  # ---- choosing --------------------------------------------------------------

  @oss @pro
  # source: oss:src/app.jsx:384-389
  # source: oss:src/ui/shell.jsx:53,321
  # source: pro:src/app.jsx:356-361
  # source: pro:src/ui/shell.jsx:41,256
  Scenario: Choosing a namespace writes it to the address
    Given the user is on any screen with other query parameters in the address
    When the user chooses "payments" in the selector
    Then the selector closes
    And the address stays on the same screen with ?ns=payments added
    And every other query parameter is kept

  @oss @pro
  # source: oss:src/app.jsx:386
  # source: pro:src/app.jsx:358
  Scenario: Choosing All namespaces removes the namespace from the address
    Given the address carries ?ns=payments
    When the user chooses "All namespaces"
    Then ?ns= is removed from the address and the other query parameters are kept

  @oss @pro
  # source: oss:src/app.jsx:388
  # source: pro:src/app.jsx:360
  Scenario: Changing namespace does not add a browser history entry
    Given the user changed namespace several times on one screen
    When the user presses the browser's Back button
    Then the browser leaves the screen rather than stepping back through the namespaces

  @oss @pro
  # source: oss:src/app.jsx:375-382
  # source: pro:src/app.jsx:345-354
  Scenario: The selected namespace follows the user across screens
    Given the namespace "payments" is selected
    When the user navigates through the sidebar, the palette or a link inside a screen
    Then the new address carries ?ns=payments

  @oss @pro
  # source: oss:src/app.jsx:262,375-389
  # source: pro:src/app.jsx:163,345-361
  # assumption: the choice is held only in the address, never in browser storage; inferred from the absence of any storage write
  Scenario: The selected namespace is not remembered between visits
    Given the user selected a namespace
    When the dashboard is opened afresh at an address without ?ns=
    Then "All namespaces" is selected

  @pro @fleet
  # source: pro:src/app.jsx:362-365
  Scenario: Picking another cluster starts on All namespaces
    Given the namespace "payments" is selected
    When the user picks a different cluster
    Then that cluster's overview opens with no namespace selected

  @pro @fleet
  # source: pro:src/app.jsx:59-62,163
  # assumption: the namespace in the address is not checked against the cluster named in the address
  Scenario: A link may carry a namespace into any cluster
    When the user opens "/c/<another cluster>/pods?ns=payments"
    Then the selector reads "payments" on that cluster, whether or not it has such a namespace

  # ---- what the choice does --------------------------------------------------

  @oss @pro
  # source: oss:src/app.jsx:477,485-491
  # source: oss:src/ui/resource-list.jsx:118
  # source: pro:src/app.jsx:500,510-514
  # source: pro:src/ui/resource-list.jsx:105
  Scenario: The choice narrows every namespaced list and no cluster-scoped one
    Given a namespace is selected
    Then every namespaced kind's list, including custom resources and the tabbed Storage and RBAC screens, shows only that namespace
    And the Workloads overview shows only that namespace
    And cluster-scoped lists are unaffected

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:141,223
  # source: pro:src/ui/resource-list.jsx:128,211
  Scenario: The choice is the default namespace for new objects
    Given a namespace is selected
    When the user starts creating a namespaced object
    Then the manifest skeleton names the selected namespace
    And with "All namespaces" selected it names "default"

  # ---- empty, loading, forbidden, stale --------------------------------------

  @oss @pro
  # source: oss:src/data.js:18
  # source: oss:src/app.jsx:498
  # source: pro:src/data.js:18
  # source: pro:src/app.jsx:521
  Scenario: Before the snapshot loads the selector offers only All namespaces
    Given the cluster snapshot has not loaded yet
    When the user opens the selector
    Then "All namespaces" is the only option

  @diverged @oss
  # source: oss:src/live.js:301-303
  # source: oss:src/data.js:18
  Scenario: A failed namespace read keeps the options the selector already had
    Given the selector lists the cluster's namespaces
    When a later snapshot refresh cannot read namespaces
    Then the selector keeps the previous options
    And when the read succeeds with no namespaces the selector is reduced to "All namespaces"

  @diverged @pro @drop
  # decision: D1 — oss tells a failed read from an empty one
  # source: pro:src/live.js:599-602
  # source: pro:src/data.js:18
  Scenario: A namespace read that returns nothing keeps the options the selector already had
    Given the selector lists the cluster's namespaces
    When a later snapshot refresh reads no namespaces, whether through failure or an empty answer
    Then the selector keeps the previous options

  @oss @pro
  # source: oss:src/live.js:301-303
  # source: oss:src/data.js:18
  # source: pro:src/live.js:599-602
  # source: pro:src/data.js:18
  Scenario: A user who may not list namespaces can only choose one by address
    Given the signed-in user is not allowed to list namespaces
    Then the selector offers only "All namespaces"
    And a namespace can still be selected by opening an address with ?ns=<name>

  @oss @pro
  # source: oss:src/app.jsx:262
  # source: oss:src/ui/shell.jsx:314,318-325
  # source: pro:src/app.jsx:163
  # source: pro:src/ui/shell.jsx:249,253-260
  Scenario: A namespace in the address that the cluster does not have is shown as chosen
    Given the address carries ?ns= naming a namespace missing from the selector's options
    Then the selector's button reads that name
    And no option in the open selector is marked as current
    And namespaced lists are empty with no explanation

  @oss @pro
  # source: oss:src/app.jsx:314-323,498
  # source: pro:src/app.jsx:270-299,521
  Scenario: New namespaces reach the selector with the next snapshot refresh
    Given a namespace was created or deleted, from the dashboard or elsewhere
    Then the selector's options change only when the cluster snapshot next refreshes
    And while auto-refresh is paused they do not change until the user refreshes or resumes
