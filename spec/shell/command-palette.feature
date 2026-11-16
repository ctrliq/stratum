Feature: Command palette
  A searchable list of every screen and a few actions, opened from the
  keyboard or the header.

  # ---- opening and closing ----------------------------------------------------------

  @oss @pro
  # source: oss:src/app.jsx:341-345
  # source: pro:src/app.jsx:316-320
  Scenario: Cmd+K or Ctrl+K toggles the palette from anywhere in the shell
    When the user presses K with the Command or Control key held, in either letter case
    Then the palette opens if it was closed and closes if it was open
    And the browser's own shortcut for that key is suppressed

  @oss @pro
  # source: oss:src/ui/shell.jsx:327-332
  # source: oss:src/app.jsx:500
  # source: pro:src/ui/shell.jsx:262-266
  # source: pro:src/app.jsx:523
  Scenario: The header's search button opens the palette
    When the user clicks the search button in the header
    Then the palette opens

  @oss @pro
  # source: oss:src/ui/palette.jsx:33-35
  # source: pro:src/ui/palette.jsx:47-49
  Scenario: The palette always opens fresh
    Given the palette was used before with a query and a highlighted entry
    When it is opened again
    Then the query is empty, the first entry is highlighted and the search field has focus

  @oss @pro
  # source: oss:src/ui/palette.jsx:61,82
  # source: pro:src/ui/palette.jsx:80,101
  Scenario: Escape or the dialog's close control closes the palette
    Given the palette is open
    When the user presses Escape or closes the dialog
    Then the palette closes and nothing is run

  # ---- entries ----------------------------------------------------------------------

  @diverged @oss @drop
  # decision: D1
  # source: oss:src/ui/palette.jsx:16-25,38
  Scenario: The open dashboard's fixed navigation entries
    Then the "Navigate" group starts with Cluster overview, Workloads, Nodes, Events, Helm releases, Storage, RBAC and Ingress

  @diverged @pro
  # source: pro:src/ui/palette.jsx:17-29,52
  Scenario: The fleet dashboard's fixed navigation entries
    Then the "Navigate" group starts with Cluster overview, Clusters, Workloads, Nodes, Events, Helm releases, Storage, RBAC, Ingress, App Catalog and Hardening

  @pro
  # source: pro:src/ui/palette.jsx:19,52
  # source: pro:src/app.jsx:539
  Scenario: The Clusters entry is offered only in fleet mode
    Given the configuration mode is "standalone"
    Then the palette has no Clusters navigation entry

  @oss @pro
  # source: oss:src/ui/palette.jsx:39-41
  # source: pro:src/ui/palette.jsx:53-55
  Scenario: Every resource kind is reachable from the palette
    Then after the fixed entries the "Navigate" group lists every resource kind by its label
    And an entry shows the kind's singular name beneath it when that differs from the label
    And this includes kinds that have no sidebar row

  @diverged @pro
  # source: pro:src/ui/palette.jsx:52-55,83,120
  # source: pro:src/app.jsx:345-354,539
  Scenario: Running a navigation entry opens that screen and closes the palette
    Given a namespace filter is set
    When the user runs a navigation entry
    Then that screen opens with the namespace filter kept
    And the Clusters entry opens "/clusters" with no parameters
    And the palette closes

  @diverged @oss @drop
  # decision: D1
  # source: oss:src/ui/palette.jsx:38-41,64,103
  # source: oss:src/app.jsx:375-382,516
  Scenario: Running any navigation entry of the open dashboard keeps the namespace filter
    Given a namespace filter is set
    When the user runs a navigation entry
    Then that screen opens with the namespace filter kept
    And the palette closes

  @diverged @oss
  # decision: D14
  # source: oss:src/ui/palette.jsx:28,42
  Scenario: The pod shell action is offered whenever writes are offered in the open dashboard
    Given write controls are offered to the current session
    Then the palette has an "Actions" group with "Open shell in pod…"

  @diverged @pro
  # decision: D14
  # source: pro:src/ui/palette.jsx:42,56
  Scenario: The pod shell action is offered only to admins in the fleet dashboard
    Given the current session is an admin
    Then the palette has an "Actions" group with "Open shell in pod…"

  @oss @pro
  # source: oss:src/ui/palette.jsx:42
  # source: pro:src/ui/palette.jsx:56
  Scenario: Sessions that cannot write see no Actions group
    Given write controls are not offered to the current session
    Then the palette has no "Actions" group

  @oss @pro
  # source: oss:src/ui/palette.jsx:42
  # source: oss:src/app.jsx:435
  # source: pro:src/ui/palette.jsx:56
  # source: pro:src/app.jsx:425
  Scenario: The pod shell action opens the pod picker
    When the user runs "Open shell in pod…"
    Then the palette closes and a terminal dialog opens asking which pod to open

  @pro @fleet
  # source: pro:src/ui/palette.jsx:57-61,127
  # source: pro:src/app.jsx:362-365,540-541
  Scenario: Every cluster can be switched to from the palette
    Then a "Switch cluster" group lists each cluster by name
    And each entry shows its context, provider, ready and total node counts and status
    And the current cluster's entry carries a check mark
    When the user runs a cluster's entry
    Then that cluster's overview opens

  # ---- searching --------------------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/palette.jsx:45-47
  # source: pro:src/ui/palette.jsx:64-66
  Scenario: Typing filters entries by title, subtitle and group name
    When the user types a query
    Then only entries whose title, subtitle or group name contains the query, ignoring case, remain
    And the remaining entries keep their groups and order

  @oss @pro
  # source: oss:src/ui/palette.jsx:45
  # source: pro:src/ui/palette.jsx:64
  Scenario: A blank query shows everything
    When the query is empty or only whitespace
    Then every entry is shown

  @oss @pro @drop
  # decision: D2
  # source: oss:src/ui/palette.jsx:45-46
  # source: pro:src/ui/palette.jsx:64-65
  Scenario: Leading and trailing spaces in a palette query are part of the match
    When the user types a query that starts or ends with a space
    Then the query is matched as typed, spaces included, against each entry's title, subtitle and group name joined by single spaces
    And an entry whose text does not contain those spaces in that place is not shown

  @oss @pro @fixed
  # decision: D2
  # source: oss:src/ui/palette.jsx:45-46
  # source: pro:src/ui/palette.jsx:64-65
  Scenario: Spaces around a palette query are ignored
    When the user types a query that starts or ends with spaces
    Then the entries shown are those the query matches without the surrounding spaces

  @oss @pro
  # source: oss:src/ui/palette.jsx:89-91
  # source: pro:src/ui/palette.jsx:108-110
  Scenario: A query that matches nothing says so
    When the query matches no entry
    Then the palette shows 'No matches for "<query>"'

  @oss @pro
  # source: oss:src/ui/palette.jsx:86
  # source: pro:src/ui/palette.jsx:104-105
  Scenario: Typing moves the highlight back to the first entry
    Given the highlight was moved down the list
    When the user changes the query
    Then the first remaining entry is highlighted

  # ---- keyboard and pointer ---------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/palette.jsx:62-63
  # source: pro:src/ui/palette.jsx:81-82
  Scenario: Arrow keys move the highlight and stop at the ends
    When the user presses the down or up arrow
    Then the highlight moves one entry across group boundaries
    And it does not move past the last entry or before the first

  @oss @pro
  # source: oss:src/ui/palette.jsx:64
  # source: pro:src/ui/palette.jsx:83
  Scenario: Enter runs the highlighted entry
    When the user presses Enter
    Then the highlighted entry is run and the palette closes

  @oss @pro
  # source: oss:src/ui/palette.jsx:64
  # source: pro:src/ui/palette.jsx:83
  Scenario: Enter does nothing when no entry matches
    Given the query matches no entry
    When the user presses Enter
    Then the palette stays open and nothing is run

  @oss @pro
  # source: oss:src/ui/palette.jsx:102-103
  # source: pro:src/ui/palette.jsx:119-120
  Scenario: Pointing highlights and clicking runs
    When the user moves the pointer over an entry
    Then that entry is highlighted
    When the user clicks it
    Then it is run and the palette closes

  @oss @pro
  # source: oss:src/ui/palette.jsx:72-78
  # source: pro:src/ui/palette.jsx:91-97
  Scenario: The palette explains its keys
    Then its footer shows that the arrows navigate, Enter selects and Escape closes
