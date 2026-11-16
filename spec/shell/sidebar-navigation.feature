Feature: Sidebar navigation
  The sidebar lists every screen in labelled sections, shows a few live
  counts, links out to configured tools, and remembers which sections the user
  collapsed.

  # ---- contents ------------------------------------------------------------------

  @diverged @oss @drop
  # decision: D1
  # source: oss:src/registry.js:29-93
  Scenario: Sections of the open dashboard's sidebar
    Then the sidebar shows, in order, an unlabelled top section with Overview, Nodes, Namespaces and Events
    And the sections "Workloads", "Config", "Network", "Storage", "Helm", "Access Control", "Custom Resources" and "Links"
    And "Links" can hold Documentation, Grafana and Get Pro

  @diverged @pro
  # source: pro:src/nav.js:1-70
  Scenario: Sections of the fleet dashboard's sidebar
    Then the sidebar shows, in order, an unlabelled top section with Overview, Clusters, Operations, Nodes, Namespaces and Events
    And the sections "Workloads", "Config", "Network", "Storage", "Helm", "Access Control", "Custom Resources", "Stratum PRO" and "Links"
    And "Stratum PRO" holds App Catalog and Hardening
    And "Links" can hold Documentation and Grafana

  @oss @pro
  # source: oss:src/registry.js:36-87
  # source: pro:src/nav.js:10-61
  Scenario Outline: Rows of the sections both dashboards share
    Then the "<section>" section lists <rows>

    Examples:
      | section          | rows                                                                                                                                                                                                             |
      | Workloads        | Overview, Pods, Deployments, DaemonSets, StatefulSets, ReplicaSets, Replication Controllers, Jobs, CronJobs                                                                                                      |
      | Config           | ConfigMaps, Secrets, Resource Quotas, Limit Ranges, Horizontal Pod Autoscalers, Pod Disruption Budgets, Priority Classes, Runtime Classes, Leases, Mutating Webhooks, Validating Webhooks                        |
      | Network          | Services, Endpoints, Endpoint Slices, Ingresses, Ingress Classes, Network Policies                                                                                                                               |
      | Storage          | Persistent Volume Claims, Persistent Volumes, Storage Classes                                                                                                                                                    |
      | Helm             | Releases, Helm Charts, Helm Chart Configs                                                                                                                                                                        |
      | Access Control   | Service Accounts, Cluster Roles, Roles, Cluster Role Bindings, Role Bindings                                                                                                                                     |
      | Custom Resources | Definitions, followed by one sub-section per custom resource API group                                                                                                                                           |

  @pro @drop
  # decision: D2
  # source: pro:src/nav.js:4-5
  # source: pro:src/ui/shell.jsx:217
  Scenario: The Clusters and Operations rows are shown in standalone mode too
    Given the configuration mode is "standalone"
    Then the sidebar still shows the Clusters and Operations rows

  @oss @pro @fixed
  # decision: D2
  # decision: D4
  # source: oss:src/registry.js:29-93
  # source: pro:src/nav.js:4-5,62-65
  # source: pro:src/ui/shell.jsx:217
  Scenario: Fleet rows and edition sections are shown only where they apply
    Then the Clusters and Operations rows are shown only when the configuration mode is "fleet"
    And the "Stratum PRO" section is shown only against the fleet hub
    And the Get Pro link is shown only when the configuration carries its URL

  @diverged @pro
  # source: pro:src/ui/shell.jsx:295,297
  # source: pro:src/app.jsx:345-354
  Scenario: Clicking a row opens its screen and highlights the row
    When the user clicks a sidebar row
    Then the screen with that row's id opens, keeping the namespace filter
    And the Clusters and Operations rows open "/clusters" and "/operations" with no parameters
    And that row is highlighted

  @diverged @oss @drop
  # decision: D1
  # source: oss:src/ui/shell.jsx:381,391
  # source: oss:src/app.jsx:375-382
  Scenario: Clicking any row of the open dashboard keeps the namespace filter
    When the user clicks a sidebar row
    Then the screen with that row's id opens, keeping the namespace filter
    And that row is highlighted

  # ---- external links ---------------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/shell.jsx:378-384
  # source: pro:src/ui/shell.jsx:292-300
  Scenario: A link row opens its URL in a new tab
    Given the configuration carries a URL for the Documentation or Grafana link
    When the user clicks that row
    Then the URL opens in a new browser tab that cannot reach back into the dashboard
    And the dashboard stays on its current screen
    And the row shows an external-link marker

  @oss @pro
  # source: oss:src/ui/shell.jsx:283
  # source: pro:src/ui/shell.jsx:217
  Scenario: A section with no rows left is not shown
    Given the configuration carries no link URLs
    Then the "Links" section is not shown

  # ---- counts -------------------------------------------------------------------

  @oss @pro
  # source: oss:src/app.jsx:456-461
  # source: oss:src/registry.js:32,34,37-38
  # source: oss:src/ui/shell.jsx:385-387
  # source: pro:src/app.jsx:467-473
  # source: pro:src/nav.js:6,8,11-12
  # source: pro:src/ui/shell.jsx:301-303
  Scenario Outline: Rows that carry a live count
    Given cluster data has loaded
    Then the "<row>" row shows <count>

    Examples:
      | row                 | count                                |
      | Nodes               | the number of nodes                  |
      | Events              | the number of events of type Warning |
      | Workloads Overview  | the number of workloads              |
      | Pods                | the number of pods                   |

  @oss @pro
  # source: oss:src/app.jsx:458
  # source: pro:src/app.jsx:469
  Scenario: The pod count falls back to the per-node pod counts
    Given the pod list is empty but nodes report how many pods they run
    Then the Pods row shows the sum of the nodes' pod counts

  @pro @fleet
  # source: pro:src/app.jsx:472
  # source: pro:src/nav.js:4
  Scenario: The Clusters row shows the number of clusters
    Then the Clusters row shows how many clusters the list holds

  # ---- collapsing sections ----------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/shell.jsx:237-244,366-374
  # source: pro:src/ui/shell.jsx:206-214,279-288
  Scenario: A labelled section can be collapsed and stays collapsed after a reload
    When the user clicks a section's header
    Then the section's rows are hidden and the header offers to expand it
    And the choice is stored in the browser under "stratum.navCollapsed"
    And after a reload the section is still collapsed

  @oss @pro
  # source: oss:src/ui/shell.jsx:366,369,375
  # source: pro:src/ui/shell.jsx:280,283,289
  Scenario: The unlabelled top section cannot be collapsed
    Then the top section has no header to click and is always expanded

  @oss @pro
  # source: oss:src/ui/shell.jsx:238,242
  # source: pro:src/ui/shell.jsx:207,211
  Scenario: Collapsing works without browser storage
    Given browser storage cannot be read or written, or holds unreadable data
    Then every section starts expanded
    And collapsing still works until the page is reloaded

  @diverged @oss @drop
  # decision: D1
  # source: oss:src/ui/shell.jsx:250-251
  Scenario: The open dashboard honours collapse choices stored by an older build
    Given the stored choices name a section by its label rather than its id
    Then that section is still shown collapsed
    And a choice stored under the section's id takes precedence

  @diverged @pro
  # source: pro:src/ui/shell.jsx:279-280
  Scenario: The fleet dashboard reads collapse choices by section id only
    Given the stored choices name a section by its label rather than its id
    Then that section is shown expanded

  # ---- custom resource sub-sections -----------------------------------------------

  @oss @pro
  # source: oss:src/ui/shell.jsx:396-400,430-450
  # source: pro:src/ui/shell.jsx:307-311,323-344
  Scenario: Custom resources are grouped by API group, closed until opened
    Given the cluster has custom resource definitions
    Then the Custom Resources section shows one sub-section per API group with one row per kind
    And a sub-section is closed until the user opens it

  @oss @pro
  # source: oss:src/ui/shell.jsx:431-432
  # source: pro:src/ui/shell.jsx:324-325
  Scenario: The sub-section holding the current page opens itself
    Given the user never opened or closed a sub-section
    When the current page is one of its kinds
    Then that sub-section is shown open

  @oss @pro
  # source: oss:src/ui/shell.jsx:398-399,432
  # source: pro:src/ui/shell.jsx:309-310,325
  Scenario: A stored choice for a sub-section wins over the automatic opening
    Given the user closed a sub-section
    When the current page is one of its kinds
    Then the sub-section stays closed
    And the choice survives a reload

  @pro
  # source: pro:src/ui/shell.jsx:332
  Scenario: A sub-section header shows how many kinds it holds
    Then each custom resource sub-section header shows its number of kinds

  # ---- collapsing the whole sidebar (open dashboard's own frame) ----------------------
  # The fleet dashboard gets the equivalent from the design system's layout
  # (pro:src/ui/shell.jsx:276), so only the open dashboard's own behavior is recorded.

  @oss
  # source: oss:src/ui/shell.jsx:21,256-258,407-414
  Scenario: On a wide viewport the sidebar collapses to an icon rail
    Given a viewport at least 1024 pixels wide
    Then the sidebar starts expanded with a "Collapse" control at its foot
    When the user clicks it
    Then only row icons remain, each with its label as a tooltip, and the control reads "Expand sidebar"

  @oss
  # source: oss:src/ui/shell.jsx:396
  Scenario: The icon rail hides custom resource sub-sections
    Given the sidebar is collapsed to its icon rail
    Then custom resource sub-sections are not shown

  @oss
  # source: oss:src/ui/shell.jsx:257-258,302-307,346-348
  Scenario: On a narrow viewport the sidebar is an overlay opened from the header
    Given a viewport narrower than 1024 pixels
    Then the sidebar is hidden and cannot be focused
    When the user clicks the "Toggle sidebar" button
    Then the sidebar is shown over the page
    And clicking outside it or pressing Escape hides it again

  @oss
  # source: oss:src/ui/shell.jsx:259-267
  Scenario: Escape never collapses the sidebar on a wide viewport
    Given a viewport at least 1024 pixels wide with the sidebar expanded
    When the user presses Escape
    Then the sidebar stays expanded

  @oss
  # source: oss:src/ui/shell.jsx:258
  Scenario: Crossing the width threshold resets the sidebar
    When the viewport crosses 1024 pixels in either direction
    Then the sidebar becomes expanded on the wide side and hidden on the narrow side

  # ---- cluster block ---------------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/shell.jsx:197-203,465
  # source: pro:src/ui/shell.jsx:193-199,359
  Scenario Outline: The cluster block's caption is derived from the cluster's identity
    Given a cluster with <identity>
    Then the block's caption reads "<context> · <provider>"

    Examples:
      | identity                                                  | context            | provider |
      | a display name different from its name and provider "aws" | the display name   | aws      |
      | no distinct display name and labels                       | the first label value | —     |
      | neither a distinct display name nor labels                | the cluster's name | —        |

  @diverged @oss
  # decision: D4
  # source: oss:src/ui/shell.jsx:457-471
  Scenario: The open dashboard's cluster block is a label
    Then the block under the logo shows the caption and the cluster's name and does nothing when clicked
    And it reads "Cluster" until the cluster's identity is known

  @diverged @pro
  # decision: D4
  # source: pro:src/ui/shell.jsx:350-366
  Scenario: The fleet dashboard's cluster block is a button
    Then the block under the logo shows the caption and the cluster's name and opens the fleet page when clicked
    And it reads "No cluster" when no cluster is selected
