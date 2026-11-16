Feature: Routes
  Every screen has a URL. The open dashboard addresses screens directly; the
  fleet dashboard addresses them under the cluster they belong to.

  # ---- URL shapes ------------------------------------------------------------

  @diverged @oss @drop
  # decision: D1
  # decision: D11
  # source: oss:src/app.jsx:131-137
  Scenario: A screen of the open dashboard lives at one path segment
    When the browser opens "/<screen id>"
    Then the screen with that id is shown inside the shell

  @diverged @pro
  # decision: D11
  # source: pro:src/app.jsx:54-64
  Scenario: A screen of the fleet dashboard lives under its cluster
    When the browser opens "/c/<cluster id>/<screen id>"
    Then the screen with that id is shown inside the shell for that cluster

  @diverged @oss @drop
  # decision: D1
  # decision: D11
  # source: oss:src/app.jsx:132-133
  Scenario: A custom resource list of the open dashboard lives under /crds
    When the browser opens "/crds/<group>/<version>/<plural>"
    Then the instance list of that custom resource at that version is shown

  @diverged @pro
  # decision: D11
  # source: pro:src/app.jsx:59-60
  Scenario: A custom resource list of the fleet dashboard lives under its cluster
    When the browser opens "/c/<cluster id>/crds/<group>/<version>/<plural>"
    Then the instance list of that custom resource at that version is shown for that cluster

  @pro @fleet
  # source: pro:src/app.jsx:55-58,478-485
  Scenario: The fleet and operations pages are not under a cluster
    When the browser opens "/clusters" or "/operations"
    Then the fleet page or the operations page is shown inside the shell

  @pro @fleet
  # source: pro:src/app.jsx:189-201,481-485
  Scenario: The operations page is fetched on first use
    Given the operations page has not been opened in this page load
    When the browser opens "/operations"
    Then "Loading operations…" is shown until the page's code has loaded
    And later visits show it immediately

  # ---- root and unknown paths ----------------------------------------------------

  @diverged @oss @drop
  # decision: D1
  # decision: D11
  # source: oss:src/app.jsx:131-137,453
  Scenario: The root and unmatched paths redirect to the overview
    When the browser opens "/" or a path with more segments than any route has
    Then it is redirected to "/overview" without adding a history entry
    And any query string is dropped

  @diverged @pro
  # decision: D11
  # source: pro:src/app.jsx:49-52,461-464
  Scenario: The root and unmatched paths redirect to the default cluster's overview
    Given at least one cluster is known
    When the browser opens "/" or any path that matches no route
    Then it is redirected to "/c/<default cluster>/overview" without adding a history entry
    And the default cluster is the one marked as the fleet manager, else the first in the list

  @pro @drop
  # decision: D11
  # source: pro:src/app.jsx:54-64,461-464
  Scenario: Open-dashboard style links do not resolve in the fleet dashboard
    When the browser opens a single-segment path such as "/pods" or "/overview"
    Then it is redirected to the default cluster's overview rather than to that screen

  @oss @pro @fixed
  # decision: D11
  # source: oss:src/app.jsx:131-137
  # source: pro:src/app.jsx:54-64,461-464
  Scenario: An old cluster-less screen link redirects to that screen under the default cluster
    When the browser opens a single-segment path that names a screen, such as "/pods?ns=<namespace>"
    Then it is redirected to "/c/<default cluster>/pods?ns=<namespace>" without adding a history entry
    And the query string is kept

  @oss @pro @fixed
  # decision: D11
  # source: oss:src/app.jsx:132-133
  # source: pro:src/app.jsx:59-60,461-464
  Scenario: An old cluster-less custom resource link redirects under the default cluster
    When the browser opens "/crds/<group>/<version>/<plural>"
    Then it is redirected to "/c/<default cluster>/crds/<group>/<version>/<plural>" with its query string kept

  @oss @fixed
  # decision: D11
  # source: oss:src/app.jsx:131-137,453
  Scenario: A single-cluster backend addresses its cluster as local
    Given the configuration describes a single-cluster backend
    When the browser opens "/"
    Then it is redirected to "/c/local/overview"
    And every cluster screen is addressed under "/c/local/"

  @pro @drop
  # decision: D3
  # decision: D13
  # source: pro:src/app.jsx:462
  # assumption: the cluster list loader always leaves at least a stand-in "local" cluster (pro:src/live.js:297-305), so these two branches look unreachable after boot
  Scenario: With no clusters at all the root leads to the fleet page or a notice
    Given no cluster is known
    When the browser opens "/" or any path that matches no route
    Then in fleet mode it is redirected to "/clusters"
    And in standalone mode the page shows "No cluster available."

  @diverged @oss @drop
  # decision: D1
  # decision: D12
  # source: oss:src/app.jsx:118-119,492
  # source: oss:src/ui/shell.jsx:516-523
  Scenario: An unknown screen id shows a not-built notice in the open dashboard
    When the browser opens a single-segment path that is no known screen
    Then the shell shows a page titled "Overview" saying "This screen has not been built yet."

  @diverged @pro @drop
  # decision: D1
  # decision: D12
  # source: pro:src/app.jsx:46,515
  # source: pro:src/ui/shell.jsx:405-412
  Scenario: An unknown screen id shows a migration notice in the fleet dashboard
    When the browser opens "/c/<cluster id>/<unknown id>"
    Then the shell shows a page titled "Overview" saying "This screen is being migrated to the Quantic Design System."

  @oss @pro @fixed
  # decision: D12
  # source: oss:src/app.jsx:118-119,492
  # source: pro:src/app.jsx:46,515
  Scenario: An unknown screen id shows a not-found state
    When the browser opens "/c/<cluster id>/<id that names no screen and no resource kind>"
    Then the shell shows a not-found state that names the id
    And it is not titled "Overview"

  @pro @drop
  # decision: D12
  # source: pro:src/app.jsx:204-206,217-234
  # source: pro:src/ui/shell.jsx:233-236,351
  # assumption: what the chrome shows for an unlisted cluster is inferred from the lookup falling through to nothing; not exercised
  Scenario: A cluster id the list does not contain is still addressed
    When the browser opens "/c/<id not in the cluster list>/overview"
    Then no redirect happens and data is requested for that id
    And the breadcrumb shows no cluster name and the cluster block reads "No cluster"

  @pro @fixed
  # decision: D12
  # source: pro:src/app.jsx:204-206,217-234
  Scenario: An unknown cluster id shows a not-found state
    When the browser opens "/c/<id not in the cluster list>/overview"
    Then the shell shows a not-found state that names the cluster id
    And no cluster data is requested for it

  # ---- which screen answers an id ---------------------------------------------------

  @oss @pro
  # source: oss:src/app.jsx:471-492
  # source: pro:src/app.jsx:478-515
  Scenario: Built-in screens win over generic resource lists
    Given a screen id that names a built-in screen
    Then the built-in screen is shown even if a resource kind has the same id
    And any other id that names a resource kind shows the generic list for that kind

  @diverged @oss @drop
  # decision: D1
  # source: oss:src/app.jsx:481-488
  Scenario: The open dashboard's built-in screens
    Then the built-in screen ids are "overview", "helm", "nodes", "events", "workloads", "storage", "rbac" and "ingress"

  @diverged @pro
  # source: pro:src/app.jsx:504-513
  Scenario: The fleet dashboard's built-in screens
    Then the built-in cluster screen ids are "overview", "hardening", "helm", "catalog", "nodes", "events", "workloads", "storage", "rbac" and "ingress"

  @oss @pro
  # source: oss:src/registry.js:110
  # source: oss:src/app.jsx:224-227
  # source: pro:src/app.jsx:55-62,139-142
  Scenario: Ids the dashboard keeps for itself
    Then "login" always addresses the sign-in page and never a screen
    And "crds" addresses the custom resource definitions list and owns the paths beneath it
    And "storage" and "rbac" address the older tabbed screens
    And in the fleet dashboard "clusters", "operations" and "c" are top-level paths, not screen ids

  # ---- links that predate the current sidebar ---------------------------------------

  @oss @pro
  # source: oss:src/app.jsx:486-487
  # source: pro:src/app.jsx:511-512
  Scenario Outline: Old tabbed screens still answer their links
    When the browser opens the "<screen>" screen
    Then a tabbed screen titled "<title>" is shown on its first tab
    And choosing a tab puts "tab=<tab id>" in the URL

    Examples:
      | screen  | title   |
      | storage | Storage |
      | rbac    | RBAC    |

  @oss @pro
  # source: oss:src/app.jsx:124-129
  # source: pro:src/app.jsx:66-71
  Scenario: An old tabbed link lights the sidebar row of the tab it shows
    Given the "storage" or "rbac" screen is open with a "tab" parameter naming one of its tabs
    Then the sidebar row of that resource kind is highlighted
    And with a missing or unknown "tab" the row of the screen's first tab is highlighted

  @oss @pro @drop
  # decision: D2
  # source: oss:src/app.jsx:488,491
  # source: oss:src/registry.js:64
  # source: pro:src/app.jsx:513-514
  # source: pro:src/nav.js:38
  Scenario: Both the short and the plural ingress link show the ingress list
    When the browser opens the "ingress" screen or the "ingresses" screen
    Then the ingress list is shown
    And only "ingress" highlights the sidebar's Ingresses row

  @oss @pro @fixed
  # decision: D2
  # source: oss:src/app.jsx:488,491
  # source: pro:src/app.jsx:513-514
  Scenario: Both ingress links highlight the Ingresses row
    When the browser opens the "ingress" screen or the "ingresses" screen
    Then the ingress list is shown and the sidebar's Ingresses row is highlighted

  @oss @pro
  # source: oss:src/app.jsx:125
  # source: oss:src/ui/shell.jsx:430-431,443
  # source: pro:src/app.jsx:67
  # source: pro:src/ui/shell.jsx:324,338
  Scenario: A custom resource link at any version lights the same sidebar row
    Given a custom resource's sidebar row links to its first served version
    When the browser opens that custom resource's list at another version
    Then the same sidebar row is highlighted
