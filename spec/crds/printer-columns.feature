Feature: Printer columns on custom resource instance lists
  The fleet dashboard reads the extra columns a custom resource definition
  declares for a version and shows them on that version's instance list,
  evaluating each column's path against every instance in the browser.

  # ---- which columns appear --------------------------------------------------

  @pro
  # source: pro:src/crd-columns.js:58-62
  # source: pro:src/resources.jsx:321,327
  Scenario: Declared columns appear in their declared order, labelled by their name
    Given the version in the address declares the printer columns "Phase" then "Replicas"
    Then the instance list shows "Phase" then "Replicas" between the identity columns and Age

  @pro
  # source: pro:src/crd-columns.js:56-61
  Scenario Outline: Some declared columns are left out
    Given the version declares a printer column that <condition>
    Then that column is not shown

    Examples:
      | condition                                              |
      | has a priority above 0                                 |
      | has no name                                            |
      | has no path                                            |
      | has the path written exactly .metadata.name            |
      | has the path written exactly .metadata.namespace       |
      | has the path written exactly .metadata.creationTimestamp |

  @pro
  # source: pro:src/crd-columns.js:60
  Scenario: A column with priority 0 or no priority is shown
    Given the version declares a printer column with priority 0, or with no priority
    Then that column is shown

  @pro
  # source: pro:src/crd-columns.js:56,60
  # source: pro:src/resources.jsx:17,327
  Scenario: A definition's own Age column is dropped in favour of the list's
    Given the version declares a column named "Age" whose path is written exactly .metadata.creationTimestamp
    Then the list shows a single Age column, the generic one, in last place

  @pro @drop
  # decision: D2 — legacy defect (the name appears twice), not reproduced
  # source: pro:src/crd-columns.js:2,56,60
  Scenario Outline: A column pointing at the name through a braced or $-prefixed path is still shown
    Given the version declares a printer column whose path is written <path>
    Then that column is shown, repeating what the <generic> column already shows

    Examples:
      | path                           | generic   |
      | {.metadata.name}               | Name      |
      | $.metadata.name                | Name      |
      | {.metadata.namespace}          | Namespace |
      | {.metadata.creationTimestamp}  | Age       |

  @pro @fixed
  # decision: D2 — replaces the scenario above
  # source: pro:src/crd-columns.js:2,56,60
  Scenario: Identity columns are left out however their path is spelled
    Given the version declares a printer column pointing at the object's name, namespace or creation time
    When the path is written plainly, in braces or with a leading $
    Then that column is not shown

  @pro
  # source: pro:src/crd-columns.js:61
  # source: pro:src/resources.jsx:321
  # source: pro:src/ui/resource-list.jsx:170
  Scenario: A column's description is its header tooltip
    Given a printer column declares a description
    When the user hovers the column's header
    Then the description is shown
    And a column without a description has no header tooltip

  @pro
  # source: pro:src/resources.jsx:310-313,321
  Scenario: Columns belong to the version in the address
    Given the definition declares different printer columns for "v1" and "v1beta1"
    When the instance list is shown at "v1beta1"
    Then only the columns declared for "v1beta1" are shown

  @pro
  # source: pro:src/crd-columns.js:59
  # source: pro:src/resources.jsx:327
  Scenario: A version that declares no printer columns shows identity and age only
    Given the version in the address declares no printer columns
    Then the instance list's columns are Name, Namespace for a namespaced kind, and Age

  # ---- how a cell is shown ---------------------------------------------------

  @pro
  # source: pro:src/resources.jsx:295-301
  # source: pro:src/crd-columns.js:61,68-73
  Scenario Outline: A cell is formatted by the column's declared type
    Given a printer column of type <type> whose path finds <found> on an instance
    Then the cell shows <shown>

    Examples:
      | type      | found                               | shown                                              |
      | string    | the text "Ready"                    | Ready                                              |
      | undeclared| the text "Ready"                    | Ready, as for a string                             |
      | boolean   | false                               | false                                              |
      | integer   | 3                                   | 3 as a number                                      |
      | number    | 0                                   | 0 as a number                                      |
      | date      | a timestamp 5 minutes old           | 5m, the same relative form as Age                  |
      | date      | two timestamps                      | the relative age of the first only                 |
      | string    | the texts "a" and "b"               | a, b                                               |
      | string    | a map or a list as the final value  | that value written as compact JSON                 |
      | any       | nothing                             | —                                                  |
      | any       | only nulls                          | —                                                  |

  @pro
  # source: pro:src/resources.jsx:300
  Scenario Outline: Long text is cut at 40 characters
    Given a text printer cell holds <length> characters
    Then the cell shows <shown>

    Examples:
      | length | shown                                                                  |
      | 40     | all 40 characters and no tooltip                                       |
      | 41     | the first 40 characters followed by "…", with the full text as tooltip |

  @pro @drop
  # decision: D2 — legacy defect (wrong result), not reproduced
  # source: pro:src/resources.jsx:298
  # source: pro:src/k8srules.js:9-16
  # assumption: a date column whose value is not a parseable time is passed to the age formatter unchecked. Running the legacy age formatter on a non-time text would confirm the exact text shown
  Scenario: A date column holding something that is not a time shows a meaningless age
    Given a printer column of type date finds the text "soon"
    Then the cell shows "NaNd" rather than a dash or the text

  @pro @fixed
  # decision: D2 — replaces the scenario above
  # source: pro:src/resources.jsx:298
  # source: pro:src/k8srules.js:9-16
  Scenario: A date column holding something that is not a time shows a dash
    Given a printer column of type date finds the text "soon"
    Then the cell shows "—"

  # ---- which paths are understood -------------------------------------------

  @pro
  # source: pro:src/crd-columns.js:1-54
  Scenario Outline: Column paths the dashboard can evaluate
    Given an instance whose status holds phase "Ready", replicas 3, the list conditions [type "Ready" status "True", type "Synced" status "False"] and the map labels {tier "db"}
    And its metadata holds the annotation "example.io/owner" with value "ops"
    When a printer column's path is <path>
    Then the cell shows "<shown>"

    Examples:
      | path                                             | shown        |
      | .status.phase                                    | Ready        |
      | {.status.phase}                                  | Ready        |
      | $.status.phase                                   | Ready        |
      | .status.replicas                                 | 3            |
      | .status.conditions[0].type                       | Ready        |
      | .status.conditions[-1].type                      | Synced       |
      | .status.conditions[*].type                       | Ready, Synced |
      | .status.conditions[?(@.type=="Ready")].status    | True         |
      | .status.conditions[?(@.type=='Ready')].status    | True         |
      | .status.conditions[?(@.type!="Ready")].type      | Synced       |
      | .status.labels[*]                                | db           |
      | .status['phase']                                 | Ready        |
      | .metadata.annotations.example\.io/owner          | ops          |
      | .metadata.annotations['example.io/owner']        | ops          |
      | .status.missing.deeper                           | —            |
      | .status.conditions[5].type                       | —            |
      | .status.phase[0]                                 | —            |

  @pro
  # source: pro:src/crd-columns.js:7-19,31-33
  Scenario Outline: Column paths the dashboard cannot evaluate show a dash on every row
    When a printer column's path is <path>
    Then the column is still shown and every cell reads "—", because the path uses <unsupported>

    Examples:
      | path                                         | unsupported                                        |
      | .status.conditions[0                         | an unclosed bracket                                |
      | .status.conditions[0:2].type                 | a slice                                            |
      | .status.conditions[?(@.count>2)].type        | a comparison other than == or !=                   |
      | .status.conditions[?(@.type=="A"&&@.x=="B")] | a compound filter                                  |
      | ..phase                                      | recursive descent, which is read as plain ".phase" |

  @pro
  # source: pro:src/crd-columns.js:42-48
  Scenario: A filter skips elements that lack the compared field
    Given a list where one element has no "type" field
    When a printer column's path filters with @.type != "Ready"
    Then the element without "type" is not selected, although it is not "Ready"

  @pro
  # source: pro:src/crd-columns.js:46
  Scenario: A filter compares values as text
    Given a list element whose "port" is the number 80
    When a printer column's path filters with @.port == "80" or @.port == 80
    Then the element is selected either way

  @pro
  # source: pro:src/resources.jsx:295-301
  # source: pro:src/ui/resource-list.jsx:86-101,179
  Scenario: Printer cells are recomputed from each refresh of the list
    Given an instance list with printer columns is open
    When the list refreshes
    Then every printer cell reflects the freshly read instance, with no extra request per column

  @pro
  # source: pro:src/resources.jsx:341-343
  Scenario: Search does not look inside printer columns
    Given an instance list with printer columns is open
    When the user searches for a value that appears only in a printer column
    Then no row matches

  # ---- the other dashboard ---------------------------------------------------

  @oss @drop
  # decision: D1 — pro's printer columns are the superset
  # source: oss:src/resources.jsx:289-303
  # assumption: the open dashboard never reads a definition's printer columns; inferred from the absence of any such code
  Scenario: Printer columns a definition declares are ignored
    Given a definition declares printer columns
    Then its instance list shows Name, Namespace for a namespaced kind, and Age only
