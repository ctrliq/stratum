Feature: Deep links into resource lists
  A link can carry the namespace to filter by (?ns=) and an object to open
  (?name=), so another screen or a shared address lands on one object's
  detail.

  @diverged @oss @drop
  # decision: D1, D11 — every cluster screen lives under /c/$cluster/…; the old address redirects there (spec/shell)
  # source: oss:src/app.jsx:6-7,264,491
  # source: oss:src/ui/resource-list.jsx:63-74
  Scenario: A link names an object to open in a kind's list
    When the user opens "/deployments?name=payments/api"
    Then the Deployments list loads
    And the detail panel opens for "api" in namespace "payments"

  @diverged @pro
  # decision: D11 — applies to both backends; a single-cluster backend uses the cluster id "local"
  # source: pro:src/app.jsx:7-8,165,514
  # source: pro:src/ui/resource-list.jsx:45-56
  Scenario: A link names a cluster and an object to open in a kind's list
    When the user opens "/c/<cluster id>/deployments?name=payments/api"
    Then that cluster's Deployments list loads
    And the detail panel opens for "api" in namespace "payments"

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:68-74
  # source: pro:src/ui/resource-list.jsx:50-56
  Scenario: The object opens only once the list has rows
    Given a link with ?name= was opened
    When the list is still loading
    Then no panel is open
    And the panel opens as soon as the loaded rows include the named object

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:71-72
  # source: pro:src/ui/resource-list.jsx:53-54
  Scenario Outline: The name may be given with or without a namespace
    Given the list holds "api" in namespace "payments" and "api" in namespace "search", in that order
    When the link carries ?name=<value>
    Then the panel opens for <opened>

    Examples:
      | value        | opened                             |
      | payments/api | "api" in "payments"                |
      | search/api   | "api" in "search"                  |
      | api          | "api" in "payments", the first hit |

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:71
  # source: pro:src/ui/resource-list.jsx:53
  Scenario: A cluster-scoped object is named without a namespace
    When the link to a cluster-scoped kind carries ?name=<name>
    Then the panel opens for the object with that name

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:71
  # source: pro:src/ui/resource-list.jsx:53
  Scenario: Only the first two slash-separated parts of the name are used
    When the link carries ?name=a/b/c
    Then the list looks for an object named "b" in namespace "a"

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:70-73
  # source: pro:src/ui/resource-list.jsx:52-55
  Scenario: A name that matches nothing opens nothing and says nothing
    Given a link names an object the list does not contain
    When the list loads
    Then no panel opens and no message is shown
    And the list is otherwise shown normally

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:68-74
  # source: pro:src/ui/resource-list.jsx:50-56
  Scenario: A named object that appears later is opened when it appears
    Given a link names an object the list does not contain yet
    When a later refresh returns that object and no panel was opened for the link so far
    Then the detail panel opens for it

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:65-67,70,73
  # source: pro:src/ui/resource-list.jsx:47-49,52,55
  Scenario: The link opens the panel once
    Given a link opened an object's detail panel
    When the user closes the panel and the list refreshes
    Then the panel does not reopen

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:67
  # source: pro:src/ui/resource-list.jsx:49
  Scenario: A different name in the address opens the new object
    Given a link opened one object's detail panel
    When the address changes to name a different object of the same kind
    Then the panel opens for the newly named object

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:72,116-124
  # source: pro:src/ui/resource-list.jsx:54,103-111
  Scenario: The named object opens even when the namespace selection hides its row
    Given a link carries ?name=payments/api together with ?ns=search
    When the list loads
    Then the detail panel opens for "api" in "payments"
    And the rows show only namespace "search", with none highlighted

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:70
  # source: pro:src/ui/resource-list.jsx:52
  Scenario: An empty list opens nothing
    Given a link with ?name= was opened
    When the list loads empty or fails to load
    Then no panel opens

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:231
  # source: oss:src/app.jsx:384-389
  # source: pro:src/ui/resource-list.jsx:218
  # source: pro:src/app.jsx:356-361
  # assumption: nothing rewrites the address when the panel is closed; inferred from the absence of any navigation on close
  Scenario: Closing the panel leaves the name in the address
    Given a link opened an object's detail panel
    When the user closes the panel
    Then the address still carries ?name=
    And reloading the page opens the same object again
    And changing the selected namespace keeps ?name= in the address

  @oss @pro
  # source: oss:src/app.jsx:375-382
  # source: pro:src/app.jsx:345-354
  Scenario: Navigating elsewhere keeps the namespace and drops the name
    Given the address carries ?ns=payments and ?name=payments/api
    When the user navigates to another screen from the sidebar or the palette
    Then the new address carries ?ns=payments and no ?name=

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:57-74
  # source: pro:src/ui/resource-list.jsx:39-56
  # assumption: clicking a row does not write ?name= to the address; inferred from the absence of any navigation on selection
  Scenario: Opening a row by hand does not produce a shareable link
    Given a kind's list is open
    When the user clicks a row
    Then the address does not change

  @oss @pro
  # source: oss:src/app.jsx:262,491
  # source: pro:src/app.jsx:163,514
  Scenario: A link carries the namespace to filter by
    When the user opens a namespaced kind's list with ?ns=payments
    Then the namespace selector shows "payments"
    And only objects in "payments" are rows

  @oss @pro
  # source: oss:src/app.jsx:262
  # source: oss:src/ui/resource-list.jsx:118-119
  # source: pro:src/app.jsx:163
  # source: pro:src/ui/resource-list.jsx:105-106
  Scenario: A link naming a namespace that does not exist shows an empty list
    When the user opens a namespaced kind's list with ?ns= naming a namespace the cluster does not have
    Then the list shows "No <kind's plural label in lower case> found."
    And no error about the namespace is shown
