Feature: Narrowing a resource list by namespace and search
  The rows of any kind's list are narrowed in the browser by the namespace
  chosen in the top bar and by the list's own search box.

  # ---- namespace scoping -----------------------------------------------------

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:116-120
  # source: pro:src/ui/resource-list.jsx:103-107
  Scenario: A namespaced kind shows only the selected namespace
    Given a namespaced kind's list has loaded objects from several namespaces
    When the namespace "team-a" is selected
    Then only objects whose namespace is exactly "team-a" are rows

  @oss @pro
  # source: oss:src/app.jsx:262
  # source: oss:src/ui/resource-list.jsx:118
  # source: pro:src/app.jsx:163
  # source: pro:src/ui/resource-list.jsx:105
  Scenario: With no namespace selected every namespace is shown
    Given a namespaced kind's list has loaded
    When "All namespaces" is selected, or the address carries no namespace
    Then objects from every namespace are rows

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:118
  # source: pro:src/ui/resource-list.jsx:105
  Scenario: A cluster-scoped kind ignores the namespace selection
    Given a cluster-scoped kind's list has loaded
    When the namespace "team-a" is selected
    Then every object of the kind is still a row

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:118-119,206-210
  # source: pro:src/ui/resource-list.jsx:105-106,194-198
  Scenario: A namespace with none of the kind shows the empty message and the total
    Given a namespaced kind's list has loaded 12 objects, none of them in "team-b"
    When the namespace "team-b" is selected
    Then the table shows "No <kind's plural label in lower case> found."
    And the line under the title reads "0 of 12 <Kind>s · team-b"

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:116-124
  # source: pro:src/ui/resource-list.jsx:103-111
  Scenario: Changing namespace re-filters without reloading
    Given a namespaced kind's list has loaded
    When the user selects a different namespace
    Then the rows change at once from the objects already loaded
    And the search text and any open panel are kept

  # ---- search ----------------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:162-164
  # source: pro:src/ui/resource-list.jsx:154-156
  Scenario: The search box is labelled for the kind
    Given a kind's list is open
    Then the search box's placeholder reads "Search <kind's plural label in lower case>…"

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:121-122
  # source: oss:src/resources.jsx:306-308
  # source: pro:src/ui/resource-list.jsx:108-109
  # source: pro:src/resources.jsx:341-343
  Scenario Outline: Search matches part of the name or the namespace, ignoring case
    Given the list holds an object named "Api-Gateway" in namespace "payments"
    When the user types "<query>" in the search box
    Then that object <outcome>

    Examples:
      | query           | outcome         |
      | gate            | is a row        |
      | API             | is a row        |
      | paym            | is a row        |
      | "  gateway  "   | is a row        |
      | gateway pay     | is a row        |
      | payments api    | is not a row    |
      | nginx           | is not a row    |

  @oss @pro
  # source: oss:src/resources.jsx:306-308
  # source: pro:src/resources.jsx:341-343
  Scenario: Search looks at nothing but name and namespace
    Given the list holds an object whose labels, status or other column values contain "blue"
    And neither its name nor its namespace contains "blue"
    When the user types "blue" in the search box
    Then that object is not a row

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:121
  # source: pro:src/ui/resource-list.jsx:108
  Scenario: A search of only spaces filters nothing
    Given a kind's list has loaded
    When the user types only spaces in the search box
    Then every row is still shown

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:116-124
  # source: pro:src/ui/resource-list.jsx:103-111
  Scenario: Search and namespace narrow together
    Given a namespaced kind's list has loaded
    When a namespace is selected and a search is typed
    Then only objects in that namespace that also match the search are rows
    And the line under the title reads "<shown> of <all loaded> <Kind>s · <namespace>"

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:206-210
  # source: pro:src/ui/resource-list.jsx:194-198
  Scenario: A search that matches nothing shows the empty message
    Given a kind's list has loaded rows
    When the user types a search no object matches
    Then the table shows "No <kind's plural label in lower case> found."

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:61,98
  # source: pro:src/ui/resource-list.jsx:43,78-81
  # assumption: the search text is never written to the address; inferred from the absence of any such write
  Scenario: The search is not part of the address
    Given the user has typed a search in a kind's list
    When the page is reloaded or the link is shared
    Then the search box is empty
