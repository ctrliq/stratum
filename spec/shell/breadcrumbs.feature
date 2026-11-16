Feature: Breadcrumbs
  The title bar names the cluster and the screen being shown.

  @diverged @oss
  # decision: D4
  # source: oss:src/ui/shell.jsx:287-297
  Scenario: The open dashboard's trail starts with the cluster's name
    Then the trail reads "<cluster name> › <screen title>"
    And it starts with "Cluster" until the cluster's name is known
    And no part of the trail is a link

  @diverged @pro
  # decision: D4
  # source: pro:src/ui/shell.jsx:231-243
  Scenario: The fleet dashboard's trail starts with a link to the fleet page
    Then the trail reads "Clusters › <cluster name> › <screen title>"
    And "Clusters" opens the fleet page when clicked
    And the cluster name is left out when no cluster is selected

  @pro @fleet
  # source: pro:src/ui/shell.jsx:232-236
  # source: pro:src/app.jsx:43-44,205-206,474
  Scenario: The fleet and operations pages keep the current cluster in the trail
    Given a cluster is selected
    When the fleet page or the operations page is shown
    Then the trail reads "Clusters › <cluster name> › Clusters" or "Clusters › <cluster name> › Operations"

  @oss @pro
  # source: oss:src/app.jsx:114-119,462
  # source: pro:src/app.jsx:41-46,474
  Scenario Outline: Built-in screens have fixed titles
    When the "<screen>" screen is shown
    Then the last crumb reads "<title>"

    Examples:
      | screen    | title         |
      | overview  | Overview      |
      | helm      | Helm Releases |
      | nodes     | Nodes         |
      | events    | Events        |
      | workloads | Workloads     |
      | storage   | Storage       |
      | rbac      | RBAC          |

  @diverged @oss
  # source: oss:src/app.jsx:116
  Scenario: The ingress screen is titled in the plural in the open dashboard
    When the "ingress" screen is shown
    Then the last crumb reads "Ingresses"

  @diverged @pro @drop
  # decision: D1
  # source: pro:src/app.jsx:43
  Scenario: The ingress screen is titled in the singular in the fleet dashboard
    When the "ingress" screen is shown
    Then the last crumb reads "Ingress"

  @pro
  # source: pro:src/app.jsx:42-44
  Scenario Outline: Fleet-only screens have fixed titles
    When the "<screen>" screen is shown
    Then the last crumb reads "<title>"

    Examples:
      | screen     | title       |
      | hardening  | Hardening   |
      | catalog    | App Catalog |
      | clusters   | Clusters    |
      | operations | Operations  |

  @oss @pro
  # source: oss:src/app.jsx:118-119
  # source: pro:src/app.jsx:46
  Scenario: A generic resource list is titled with its kind's label
    When a generic resource list is shown
    Then the last crumb reads that resource kind's plural label

  @oss @pro @drop
  # decision: D12
  # source: oss:src/app.jsx:118-119
  # source: pro:src/app.jsx:46
  Scenario: An unknown screen is titled Overview
    When a screen id that names nothing is shown
    Then the last crumb reads "Overview"

  @oss @pro
  # source: oss:src/app.jsx:462
  # source: pro:src/app.jsx:474
  Scenario: A custom resource list has a two-step trail
    When a custom resource's instance list is shown
    Then the trail ends "Custom Resources › <kind>"
    And until the definition has been resolved the last crumb shows the plural from the URL

  @oss
  # source: oss:src/ui/shell.jsx:302-308,344
  Scenario: The trail moves below the header on a narrow viewport
    Given a viewport narrower than 1024 pixels
    Then the trail is shown on its own row under the header
