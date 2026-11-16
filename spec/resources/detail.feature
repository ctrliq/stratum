Feature: Resource detail panel and manifest view
  Selecting a row opens a panel beside the list with the object's identity,
  its labels and its full manifest, readable as YAML or JSON.

  # ---- opening and selecting -------------------------------------------------

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:77,190,227-236,313
  # source: pro:src/ui/resource-list.jsx:178,215-223,306
  Scenario: Clicking a row opens its detail beside the list
    Given a kind's list has loaded
    When the user clicks a row
    Then a detail panel opens next to the list, titled with the object's name and subtitled with its kind
    And the list stays visible and usable

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:187
  # source: pro:src/ui/resource-list.jsx:177
  Scenario: The row whose detail is open is marked as selected
    Given a detail panel is open for an object
    Then that object's row is highlighted in the list

  @oss
  # source: oss:src/ui/resource-list.jsx:41,186-190
  Scenario: A row can be opened from the keyboard
    Given a kind's list has loaded
    When the user moves focus to a row and presses Enter or Space
    Then the row's detail panel opens
    And the row is announced as "Open <Kind> <name>"

  @oss
  # source: oss:src/ui/resource-list.jsx:34-41,189
  Scenario: Keys pressed on a row's action button belong to the button
    Given focus is on a contextual action button inside a row
    When the user presses Enter or Space
    Then the action runs
    And the row's detail panel does not open

  @diverged @oss
  # source: oss:src/ui/resource-list.jsx:50-55,228-229
  # source: oss:src/ui/resource-list.jsx:271-273
  Scenario: Selecting another row gives a fresh panel
    Given a detail panel is open in edit or scale mode for one object
    When the user clicks a different row
    Then the panel shows the newly selected object in read-only view
    And the unsaved manifest text or replica count from the previous object is gone

  @diverged @pro @drop
  # decision: D1, D2 — the oss fresh-panel variant stands
  # source: pro:src/ui/resource-list.jsx:216
  # source: pro:src/ui/resource-list.jsx:264-266
  Scenario: Selecting another row keeps the panel's pending edit or scale
    Given a detail panel is open in edit or scale mode for one object
    When the user clicks a different row
    Then the panel's title and summary change to the newly selected object
    And the panel stays in edit or scale mode holding the text or replica count taken from the previous object

  @diverged @oss
  # source: oss:src/ui/resource-list.jsx:228-229,273
  # source: oss:src/ui/codeview.jsx:121
  Scenario: Selecting another row in read-only view starts from the new object
    Given a detail panel is open in read-only view for one object, with the manifest switched to JSON
    When the user clicks a different row without closing the panel
    Then the manifest of the new object is shown as YAML again
    And pressing "Scale" prefills the new object's replica count

  @diverged @pro @drop
  # decision: D1, D2 — state from one object must not reach another; the oss variant above stands
  # source: pro:src/ui/resource-list.jsx:216,264-266
  # source: pro:src/ui/codeview.jsx:130
  Scenario: Selecting another row without closing keeps the first object's replica prefill and manifest format
    Given a detail panel is open in read-only view for one object, with the manifest switched to JSON
    When the user clicks a different row without closing the panel
    Then the new object's manifest is still shown as JSON
    And pressing "Scale" prefills the first object's replica count, which one more click applies to the new object

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:77,167,219-227
  # source: pro:src/ui/resource-list.jsx:159,178,207-215
  Scenario: The detail panel and the create panel replace each other
    Given the create panel is open
    When the user clicks a row
    Then the create panel closes and the row's detail opens
    And when the user then presses Create, the detail closes and the create panel opens

  # ---- content ---------------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:274-279,316-321
  # source: pro:src/ui/resource-list.jsx:267-272,309-314
  Scenario: The panel summarises a namespaced object
    Given a detail panel is open for a namespaced object
    Then it lists "Kind", "Namespace", "Created" and "UID"
    And "Created" reads the object's age followed by " ago"

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:276
  # source: pro:src/ui/resource-list.jsx:269
  Scenario: The panel omits Namespace for a cluster-scoped object
    Given a detail panel is open for a cluster-scoped object
    Then it lists "Kind", "Created" and "UID" and no "Namespace"

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:276-278
  # source: pro:src/ui/resource-list.jsx:269-271
  Scenario Outline: Missing identity fields read as a dash
    Given a detail panel is open for an object with no <field>
    Then "<row>" reads "—"

    Examples:
      | field         | row       |
      | namespace     | Namespace |
      | creation time | Created   |
      | UID           | UID       |

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:280,322-326
  # source: oss:src/k8sutil.js:127
  # source: pro:src/ui/resource-list.jsx:273,315-319
  # source: pro:src/k8sutil.js:23
  Scenario: Labels are listed as key=value chips
    Given a detail panel is open for an object with labels
    Then every label is shown as "<key>=<value>" below the summary
    And an object without labels shows no label area

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:27-32,281,345-346
  # source: pro:src/ui/resource-list.jsx:29-34,274,338-339
  Scenario: The manifest is shown without server bookkeeping
    Given a detail panel is open
    Then the section "Object" shows the whole object as the cluster returned it
    But the managed-fields bookkeeping under metadata is left out

  @oss @pro
  # source: oss:src/ui/codeview.jsx:120-125,133-137
  # source: pro:src/ui/codeview.jsx:129-134,138-142
  Scenario: The manifest opens as YAML and can be switched to JSON
    Given a detail panel is open
    Then the manifest is shown as YAML with line numbers
    And the toolbar offers "YAML" and "JSON" with "YAML" active
    When the user chooses "JSON"
    Then the same manifest is shown as JSON indented by two spaces

  @oss
  # source: oss:src/ui/codeview.jsx:128-131
  Scenario: The manifest toolbar counts its lines
    Given a detail panel is open
    Then the manifest toolbar reads "<n> lines", or "1 line" for a single line
    And the count follows the chosen format

  @oss @pro
  # source: oss:src/ui/codeview.jsx:25-58
  # source: pro:src/ui/codeview.jsx:30-63
  Scenario Outline: How YAML values are written
    Given the manifest holds <value> under a key
    When it is shown as YAML
    Then the value is written as <yaml>

    Examples:
      | value                                   | yaml                                       |
      | the text "nginx"                        | nginx                                      |
      | an empty text                           | ''                                         |
      | a null                                  | null                                       |
      | the number 3                            | 3                                          |
      | the boolean true                        | true                                       |
      | the text "true"                         | "true" in double quotes                    |
      | the text "8080"                         | "8080" in double quotes                    |
      | the text "a: b"                         | "a: b" in double quotes                    |
      | a text with a leading or trailing space | the text in double quotes                  |
      | a text spanning several lines           | one double-quoted line with \n escapes     |
      | a list of maps                          | one "- " item per map, keys indented under |

  @oss @pro
  # source: oss:src/ui/codeview.jsx:35-39,49-50
  # source: pro:src/ui/codeview.jsx:40-44,54-55
  Scenario Outline: A manifest that is itself an empty list or map is written in flow form
    Given the whole value being shown is <value>
    When it is shown as YAML
    Then it is written as <yaml>

    Examples:
      | value         | yaml |
      | an empty list | []   |
      | an empty map  | {}   |

  @oss @pro @drop
  # decision: D2 — legacy defect, not reproduced
  # source: oss:src/ui/codeview.jsx:25-34,40-47,51-57
  # source: pro:src/ui/codeview.jsx:30-39,45-52,56-62
  Scenario Outline: An empty list or map inside the manifest is shown as '' or "[object Object]" in YAML
    Given the manifest holds <value> under a key or as a list item
    When it is shown as YAML
    Then the value is written as <yaml>

    Examples:
      | value         | yaml                               |
      | an empty list | ''                                 |
      | an empty map  | "[object Object]" in double quotes |

  @oss @pro @fixed
  # decision: D2 — replaces the scenario above
  # source: oss:src/ui/codeview.jsx:25-34,40-47,51-57
  # source: pro:src/ui/codeview.jsx:30-39,45-52,56-62
  Scenario Outline: An empty list or map inside the manifest is written in flow form
    Given the manifest holds <value> under a key or as a list item
    When it is shown as YAML
    Then the value is written as <yaml>

    Examples:
      | value         | yaml |
      | an empty list | []   |
      | an empty map  | {}   |

  @oss @pro
  # source: oss:src/ui/codeview.jsx:49-57
  # source: pro:src/ui/codeview.jsx:54-62
  Scenario: Map keys are written unquoted whatever characters they contain
    Given the manifest holds a key containing a colon, a space or a slash
    When it is shown as YAML
    Then the key is written exactly as it is, without quotes

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:287-292
  # source: pro:src/ui/resource-list.jsx:280-285
  Scenario: The panel repeats the row's contextual actions as labelled buttons
    Given a kind defines contextual actions the user may see
    When a detail panel is open for one of its objects
    Then the panel's footer has one button per action, labelled with the action's title
    And pressing one runs the action for the object the panel shows

  # ---- closing ---------------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:231,307
  # source: oss:src/ui/shell.jsx:156-160
  # source: pro:src/ui/resource-list.jsx:218,300
  # source: pro:src/ui/shell.jsx:150-154
  Scenario: The panel closes from its footer or its header
    Given a detail panel is open in read-only view
    When the user presses "Close" in the footer or the close button in the header
    Then the panel closes and no row is selected

  @oss @pro
  # source: oss:src/ui/shell.jsx:134-145
  # source: pro:src/ui/shell.jsx:128-139
  Scenario: Escape closes the panel
    Given a detail or create panel is open and focus is not in one of its fields
    When the user presses Escape
    Then the panel closes

  @oss @pro
  # source: oss:src/ui/shell.jsx:118-128,142
  # source: pro:src/ui/shell.jsx:110-122,136
  Scenario: Escape does not close the panel while typing inside it
    Given a detail or create panel is open and the user is typing in a field inside it
    When the user presses Escape
    Then the panel stays open and the typed text is kept

  @oss @pro
  # source: oss:src/ui/shell.jsx:118-128
  # source: pro:src/ui/shell.jsx:110-122
  Scenario: Escape closes the panel while typing in the list's search box
    Given a detail panel is open and focus is in the list's search box
    When the user presses Escape
    Then the panel closes

  # ---- stale data ------------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:62,77,108,229
  # source: pro:src/ui/resource-list.jsx:44,95,178,216
  Scenario: An open panel keeps showing the object as it was when clicked
    Given a detail panel is open for an object
    When the list refreshes and the object has changed on the cluster
    Then the row shows the new values
    But the panel still shows the summary and manifest from the moment the row was clicked
    And only closing and clicking the row again shows the current object

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:62,116-124,227
  # source: pro:src/ui/resource-list.jsx:44,103-111,215
  Scenario: An open panel outlives its row
    Given a detail panel is open for an object
    When the object is deleted elsewhere, or the namespace selection or search hides its row
    Then the panel stays open showing the object
    And no row is highlighted

  @oss @pro @drop
  # decision: D2 — legacy defect, not reproduced
  # source: oss:src/ui/resource-list.jsx:187
  # source: pro:src/ui/resource-list.jsx:177
  # assumption: selection is matched on UID alone; for objects that carry no UID every row compares equal
  Scenario: Objects without a UID are all highlighted together
    Given a kind whose objects carry no UID
    When the user opens one row's detail
    Then every row in the list is highlighted as selected

  @oss @pro @fixed
  # decision: D2 — replaces the scenario above
  # source: oss:src/ui/resource-list.jsx:187
  # source: pro:src/ui/resource-list.jsx:177
  Scenario: Only the selected row is highlighted when objects carry no UID
    Given a kind whose objects carry no UID
    When the user opens one row's detail
    Then only that row is highlighted, told apart by its namespace and name

  # ---- accessible names ------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:341,368
  # source: oss:src/ui/shell.jsx:147
  # source: pro:src/ui/resource-list.jsx:334,361
  # source: pro:src/ui/shell.jsx:141
  Scenario: The manifest editors and the detail panel carry accessible names
    Then the detail or create panel is a region named by its title
    And the manifest editor is named "<Kind> <name> manifest"
    And the create editor is named "New <Kind> manifest"

  @oss
  # source: oss:src/ui/resource-list.jsx:162-164
  Scenario: The search box carries an accessible name
    Given a kind's list is open
    Then the search box is named "Search <kind's plural label>" for assistive technology
