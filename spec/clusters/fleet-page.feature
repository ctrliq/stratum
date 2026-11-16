Feature: Fleet page
  The Clusters page lists every cluster the hub knows, as cards or as a
  table, with a fleet-wide summary line, a search box and per-cluster
  entry points. It is reachable at /clusters in fleet and standalone mode.

  # ---- header -----------------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:113-114,161-167
  Scenario: The header counts healthy clusters and names the current context
    Given the hub lists 3 clusters of which 2 have status "healthy"
    And the cluster "prod" is the selected cluster
    When the user opens the Clusters page
    Then the page heading is "Clusters"
    And the summary line reads "2 of 3 healthy · context: prod"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:165
  # source: pro:src/app.jsx:204-206
  Scenario: The header shows a dash when no listed cluster is the selected one
    Given the remembered cluster selection matches no cluster in the list
    When the user opens the Clusters page
    Then the summary line ends with "context: —"
    And no cluster is marked as current

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:114
  Scenario Outline: Only the status "healthy" counts as healthy
    Given the hub lists one cluster with status "<status>"
    When the user opens the Clusters page
    Then the summary line starts with "<count> of 1 healthy"

    Examples:
      | status       | count |
      | healthy      | 1     |
      | degraded     | 0     |
      | disconnected | 0     |
      | pending      | 0     |
      | provisioning | 0     |
      |              | 0     |

  # ---- what a cluster shows ---------------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:206-240
  Scenario: A cluster card shows identity, status and inventory
    Given the card view is active
    And the hub lists a cluster with name "prod", provider "aws", region "us-east-1", version "1.34.0", 5 ready nodes of 6 and status "healthy"
    When the user opens the Clusters page
    Then the card shows the name "prod"
    And the card shows a line with the cluster's context and "aws" separated by "·"
    And the card shows a "Healthy" status
    And the card shows Region "us-east-1", Version "v1.34.0" and Nodes "5/6"
    And the card offers "Use" and "Engine state"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:241-280
  Scenario: The table view shows one row per cluster with the same facts
    Given the table view is active
    And the hub lists at least one cluster
    When the user opens the Clusters page
    Then the table has the columns "Cluster", "Context", "Status", "Provider", "Region", "Version", "Nodes", "Current", "Engine state" and an unlabelled actions column
    And each row shows the cluster's name, context, status, provider, region, version and ready/total nodes

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:216,256
  Scenario: The cluster that runs the fleet manager is labelled
    Given the hub lists a cluster flagged as the fleet manager
    When the user opens the Clusters page
    Then that cluster shows a "Fleet manager" label next to its name in both views

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:120,209,229-231,251,266-268
  Scenario: The selected cluster is marked instead of offering Use
    Given the cluster "prod" is the selected cluster
    When the user opens the Clusters page
    Then the "prod" card is highlighted and shows "Current context" instead of "Use"
    And in the table view the "prod" row is highlighted and shows "Current" instead of "Use"

  @pro @fleet
  # source: pro:src/ui/shell.jsx:62-80
  # source: pro:src/screens/clusters.jsx:220,260
  Scenario Outline: A cluster status is shown with a label and a tone
    Given the hub lists a cluster with status "<status>"
    When the user opens the Clusters page
    Then the cluster's status reads "<label>" with a <tone> tone

    Examples:
      | status       | label        | tone          |
      | healthy      | Healthy      | success       |
      | degraded     | Degraded     | warning       |
      | disconnected | Disconnected | error         |
      | pending      | Pending      | informational |
      | provisioning | Provisioning | informational |
      | deleting     | deleting     | neutral       |
      | anything     | anything     | neutral       |

  @pro @fleet
  # source: pro:src/ui/shell.jsx:193-198
  Scenario Outline: The context text is derived from display name, labels and name
    Given a cluster with name "<name>", display name "<displayName>" and labels "<labels>"
    When the cluster is shown on the Clusters page or in the cluster switcher
    Then its context reads "<context>"

    Examples:
      | name | displayName  | labels             | context      |
      | prod | Production   |                    | Production   |
      | prod | Production   | env=live           | Production   |
      | prod | prod         | env=live           | live         |
      | prod |              | env=live,tier=gold | live         |
      | prod | prod         |                    | prod         |
      | prod |              |                    | prod         |

  @pro @fleet
  # source: pro:src/ui/shell.jsx:197
  Scenario: A cluster with neither display name, labels nor name falls back to its id
    Given a cluster with only an id "c-1"
    When the cluster is shown on the Clusters page
    Then its context reads "c-1"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:67,224,263
  Scenario Outline: The Kubernetes version always carries exactly one leading "v"
    Given a cluster whose reported Kubernetes version is "<reported>"
    When the cluster is shown on the Clusters page
    Then its Version reads "<shown>"

    Examples:
      | reported | shown   |
      | 1.34.0   | v1.34.0 |
      | v1.34.0  | v1.34.0 |
      |          | —       |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:223,225,262,264
  # source: pro:src/ui/shell.jsx:199
  Scenario: Missing inventory facts are shown as placeholders
    Given a cluster that reports no provider, no region and no node counts
    When the cluster is shown on the Clusters page
    Then its provider reads "—"
    And its Region reads "—"
    And its Nodes read "0/0"

  # ---- views and search --------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:98,192-199
  Scenario: The card view is the default
    Given the user has never chosen a view on this browser
    When the user opens the Clusters page
    Then the clusters are shown as cards
    And a "Grid" / "Table" switch shows "Grid" as selected

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:60,98,111
  Scenario: The chosen view is remembered on this browser
    Given the user switched the Clusters page to "Table"
    When the user reloads the dashboard and opens the Clusters page
    Then the clusters are shown as a table

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/screens/clusters.jsx:98,193-196,206,241
  Scenario: An unrecognised remembered view shows the table
    Given the browser remembers a view value that is neither the card view nor the table view
    When the user opens the Clusters page
    Then the clusters are shown as a table
    And neither "Grid" nor "Table" is shown as selected

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/screens/clusters.jsx:98,193-196,206,241
  Scenario: An unrecognised remembered view falls back to the card view
    Given the browser remembers a view value that is neither the card view nor the table view
    When the user opens the Clusters page
    Then the clusters are shown as cards and "Grid" is shown as selected

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:98,111
  Scenario: The view choice still works when browser storage is unavailable
    Given the browser refuses access to local storage
    When the user switches the Clusters page to "Table"
    Then the clusters are shown as a table for the rest of the visit
    And no error is shown

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:115-119,188-190
  Scenario Outline: Search narrows the list by a case-insensitive substring
    Given the hub lists a cluster with name "prod", id "prod", display name "Production", provider "aws", region "us-east-1", status "healthy" and label "env=live"
    And the hub lists another cluster that matches none of those values
    When the user types "<query>" in "Search clusters…"
    Then only the "prod" cluster is shown, matched on its <matches>

    Examples:
      | query      | matches       |
      | PROD       | name / id     |
      | duction    | context       |
      | aws        | provider      |
      | us-east    | region        |
      | healthy    | status        |
      | env=live   | label pair    |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:116-117
  Scenario: A blank search shows every cluster
    Given the hub lists 3 clusters
    When the user types only spaces in the search box
    Then all 3 clusters are shown

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:114,165
  Scenario: Searching does not change the header counts
    Given the hub lists 3 clusters of which 2 are healthy
    When the user types a query that matches one cluster
    Then the summary line still starts with "2 of 3 healthy"

  # ---- empty and loading states -----------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:202-204
  Scenario: A search with no match says so
    Given the hub lists at least one cluster
    When the user types "zzz" in the search box and nothing matches
    Then the page shows 'No clusters match "zzz".'
    And no card and no table is shown

  @fixed @pro @fleet
  # decision: D13
  # source: pro:src/screens/clusters.jsx:113,202-204
  Scenario: An empty fleet says no clusters are registered
    Given the cluster list is empty
    When the user opens the Clusters page
    Then the page shows "No clusters registered yet."
    And the summary line starts with "0 of 0 healthy"

  @pro @fleet
  # source: pro:src/app.jsx:132,90-107
  Scenario: Nothing of the fleet page is shown until the first cluster list answer
    Given the dashboard is starting
    When the configuration, the session check or the first cluster list read is still pending
    Then only "Connecting to the fleet manager…" is shown

  # ---- role and capability gating -----------------------------------------

  @pro @fleet
  # decision: D14
  # source: pro:src/screens/clusters.jsx:91,168-173
  # source: pro:src/fleet.js:48-59
  Scenario: An admin on a fleet hub is offered Register existing
    Given the hub runs in fleet mode
    And the signed-in user has the "admin" role
    When the user opens the Clusters page
    Then the header offers "Register existing"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:97,171
  Scenario Outline: Deploy cluster is offered only when the hub reports cluster provisioning
    Given the hub runs in fleet mode and the user is an admin
    And the hub configuration reports the provisioning feature as <capi>
    When the user opens the Clusters page
    Then "Deploy cluster" is <offered>

    Examples:
      | capi     | offered     |
      | enabled  | offered     |
      | disabled | not offered |
      | absent   | not offered |

  @pro @fleet
  # decision: D14
  # source: pro:src/screens/clusters.jsx:168,234-235,273-274,385,535
  # source: pro:src/fleet.js:48-50
  Scenario: A read-only user sees the fleet but none of its management controls
    Given the hub runs in fleet mode
    And the signed-in user has no "admin" role
    When the user opens the Clusters page
    Then every cluster, its status, "Use" and "Engine state" are shown
    And "Register existing" and "Deploy cluster" are not shown
    And no cluster shows unregister, destroy-protection, destroy or evidence-archive controls

  @pro @fleet
  # source: pro:src/ui/shell.jsx:383,394
  Scenario: A read-only session is labelled in the user menu
    Given sign-in is required and the signed-in user has no "admin" role
    When the user opens the user menu
    Then the identity line carries a "Read-only" label

  @pro @fleet
  # source: pro:src/fleet.js:30-37,57-59
  Scenario: Management controls follow the identity without a reload
    Given the Clusters page is open for an admin
    When the session's identity changes to one without the "admin" role
    Then the management controls disappear without reloading the page

  # ---- standalone mode -----------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:168,234-235,273-274
  # source: pro:src/app.jsx:207,478-480
  Scenario: Standalone mode shows the one cluster without registry controls
    Given the hub runs in standalone mode
    And the user is an admin
    When the user opens the Clusters page
    Then the single cluster is listed with its status, "Use" or the current marker, and "Engine state"
    And "Register existing" and "Deploy cluster" are not shown
    And the cluster shows no unregister, destroy-protection or destroy control
    And the cluster shows only the evidence-archive control

  @pro @fleet
  # source: pro:src/fleet.js:48-55
  # source: pro:src/cluster-context.js:20-23
  Scenario: Standalone mode without sign-in treats the user as an admin
    Given the hub runs in standalone mode and does not require sign-in
    When the user opens the Clusters page
    Then admin-only controls that apply to standalone mode are shown

  @pro @fleet
  # decision: D14
  # source: pro:src/fleet.js:48-55
  Scenario: A missing identity where sign-in is required is not an admin
    Given the hub requires sign-in
    And no identity is known for the session
    When admin-only controls are evaluated
    Then they are hidden

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:184
  Scenario: The fleet engine-state summary is shown in standalone mode too
    Given the hub runs in standalone mode
    When the user opens the Clusters page
    Then the "Fleet Engine State" card is shown above the cluster list

  @drop @pro @fleet
  # decision: D4
  # source: pro:src/ui/shell.jsx:217
  # source: pro:src/nav.js:4-5
  # source: pro:src/ui/palette.jsx:19,52
  Scenario: Standalone mode keeps the Clusters and Operations navigation entries
    Given the hub runs in standalone mode
    When the user looks at the sidebar and the command palette
    Then the sidebar still lists "Clusters" and "Operations"
    And the command palette does not list the "Clusters" navigation command

  @fixed @pro @fleet
  # decision: D4
  # source: pro:src/ui/shell.jsx:217
  # source: pro:src/nav.js:4-5
  # source: pro:src/ui/palette.jsx:19,52
  Scenario: Fleet navigation entries are offered only on a fleet hub
    Given the backend's configuration does not report fleet mode
    When the user looks at the sidebar and the command palette
    Then neither lists "Clusters" or "Operations"
