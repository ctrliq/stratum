Feature: Overview header
  The top of the Overview: which cluster this is, its health, where it runs
  and a manual refresh.

  @pro
  # source: pro:src/screens/overview.jsx:47-51
  Scenario: The header names the cluster and shows its status
    Given the cluster is named "prod-eu" with status "degraded"
    When the Overview is shown
    Then the heading reads "prod-eu"
    And a status badge beside it reads "Degraded"

  @oss
  # source: oss:src/screens/overview.jsx:46-50
  # source: oss:src/live.js:420-427
  Scenario: The single-cluster header is always named "local" and shows the derived status
    Given a load found 3 nodes of which 2 are ready
    When the Overview is shown
    Then the heading reads "local"
    And the status badge beside it reads "Degraded"

  @oss @pro
  # source: oss:src/ui/shell.jsx:74-88
  # source: pro:src/ui/shell.jsx:62-76
  Scenario Outline: The cluster status badge has a label and a tone per status
    Given the cluster's status is "<status>"
    Then the badge reads "<label>" in the <tone> tone

    Examples:
      | status        | label        | tone    |
      | healthy       | Healthy      | success |
      | degraded      | Degraded     | warning |
      | disconnected  | Disconnected | error   |
      | pending       | Pending      | blue    |
      | provisioning  | Provisioning | blue    |
      | something-new | something-new | gray   |

  @diverged @oss
  # source: oss:src/screens/overview.jsx:19-21
  Scenario: Before the first load answers the cluster is shown as pending
    Given no load has produced the cluster's identity yet
    When the Overview is shown
    Then the heading reads "local"
    And the status badge reads "Pending", never "Healthy"

  @diverged @pro @drop
  # source: pro:src/app.jsx:204-206
  # decision: D1 — the oss variant (Pending, never Healthy over no data) stands
  # source: pro:src/screens/overview.jsx:21
  Scenario: When the cluster is not in the hub's list it is shown as healthy
    Given the selected cluster is not found in the list of clusters
    When the Overview is shown
    Then the heading reads "local"
    And the status badge reads "Healthy"

  @pro
  # source: pro:src/screens/overview.jsx:61-63
  # source: pro:src/ui/shell.jsx:191-199
  Scenario: The line under the heading reads context, provider, region and Kubernetes version
    Given the cluster "prod-eu" has the display name "Production EU", the provider "aws" and the region "eu-west-1"
    And its Kubernetes version is "1.37.1"
    Then the line reads "Production EU · aws · eu-west-1 · Kubernetes v1.37.1"

  @oss
  # source: oss:src/screens/overview.jsx:40,51-53
  # source: oss:src/ui/shell.jsx:195-203
  # source: oss:src/live.js:420-427
  Scenario: The single-cluster line reads the fixed name, no provider, the first node zone and the version
    Given the first node with a zone is in "eu-west-1a"
    And the Kubernetes version is "1.37.1"
    Then the line reads "local · — · eu-west-1a · Kubernetes v1.37.1"

  @pro
  # source: pro:src/ui/shell.jsx:193-198
  Scenario Outline: The context is the display name, else the first label value, else the name
    Given a cluster named "<name>" with display name "<display name>" and labels <labels>
    Then its context reads "<context>"

    Examples:
      | name    | display name  | labels             | context       |
      | prod-eu | Production EU | env=prod           | Production EU |
      | prod-eu | prod-eu       | env=prod, tier=1   | prod          |
      | prod-eu | prod-eu       | none               | prod-eu       |
      | prod-eu |               | none               | prod-eu       |

  @oss @pro
  # source: oss:src/ui/shell.jsx:203
  # source: pro:src/ui/shell.jsx:199
  Scenario: A cluster with no known provider shows a dash
    Given the cluster states no provider
    Then the provider part of the line reads "—"

  @pro
  # source: pro:src/screens/overview.jsx:42
  Scenario Outline: The region falls back to the first node zone
    Given the cluster's region is <region>
    And the node rows have the zones <zones>
    Then the region part of the line reads "<shown>"

    Examples:
      | region        | zones                         | shown        |
      | "eu-west-1"   | "eu-west-1a", "eu-west-1b"    | eu-west-1    |
      | not stated    | "—", "eu-west-1b"             | eu-west-1b   |
      | "—"           | "eu-west-1a"                  | eu-west-1a   |
      | not stated    | "—", "—"                      | —            |
      | not stated    | no node rows                  | —            |

  @oss
  # source: oss:src/screens/overview.jsx:40
  # source: oss:src/live.js:420-427
  Scenario Outline: The single-cluster region is always the first node zone
    Given the node rows have the zones <zones>
    Then the region part of the line reads "<shown>"

    Examples:
      | zones                         | shown        |
      | "—", "eu-west-1b"             | eu-west-1b   |
      | "—", "—"                      | —            |
      | no node rows                  | —            |

  @oss @pro
  # source: oss:src/screens/overview.jsx:22,52
  # source: pro:src/screens/overview.jsx:22,62
  Scenario: The Kubernetes version prefers what the load found over the cluster record
    Given the latest load found the Kubernetes version "1.37.2"
    And the cluster's record says "1.37.1"
    Then the line ends with "Kubernetes v1.37.2"

  @oss @pro
  # source: oss:src/screens/overview.jsx:22,52
  # source: pro:src/screens/overview.jsx:22,62
  Scenario: The Kubernetes version is omitted from the line when unknown
    Given neither the load nor the cluster's record states a Kubernetes version
    Then the line ends after the region, with no version part

  @oss @pro
  # source: oss:src/screens/overview.jsx:52
  # source: pro:src/screens/overview.jsx:62
  Scenario: The version is shown with exactly one leading "v"
    Given the cluster's record says the Kubernetes version is "v1.37.1"
    Then the line ends with "Kubernetes v1.37.1"

  @oss @pro
  # source: oss:src/screens/overview.jsx:55
  # source: oss:src/app.jsx:390-396,481
  # source: pro:src/screens/overview.jsx:65
  # source: pro:src/app.jsx:366-383,504
  Scenario: The header offers a Refresh button that reloads the cluster's data
    When the user presses "Refresh" in the Overview header
    Then a load of the cluster's data runs immediately
    And its outcome is reported as described in the data domain's polling and refresh feature

  @pro @fleet
  # source: pro:src/screens/overview.jsx:52
  Scenario: The cluster that hosts the fleet manager is marked
    Given the hub's record marks the cluster as the one running the fleet manager
    Then a "Fleet manager" badge is shown beside the status

  @pro
  # source: pro:src/screens/overview.jsx:53-59
  # source: pro:src/app.jsx:504
  Scenario: While the cluster is unreachable the header says how old the figures are
    Given the dashboard is reconnecting to the cluster
    And node rows from an earlier load are still shown
    And the last successful refresh was at 14:05
    Then a badge beside the status reads "as of 14:05" in the browser's local time format
    And its tooltip reads "The control plane is unreachable — these figures are the last successful read, not live."

  @pro
  # source: pro:src/screens/overview.jsx:53
  Scenario: The "as of" badge is not shown without earlier data
    Given the dashboard is reconnecting to the cluster
    And no node rows have ever loaded
    Then no "as of" badge is shown

  @oss
  # assumption: the Overview in this root receives no staleness input; staleness is reported only by the shell's live indicator
  # source: oss:src/screens/overview.jsx:14
  # source: oss:src/app.jsx:481
  Scenario: The Overview itself carries no staleness badge
    Given the latest load left some data stale
    When the Overview is shown
    Then the header shows no "as of" badge
