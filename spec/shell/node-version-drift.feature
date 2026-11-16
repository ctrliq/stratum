Feature: Node version drift rule
  A node "drifts" when the Kubernetes version stamped on it by the operator
  disagrees with the version its kubelet reports. Each dashboard has one rule
  for that comparison, shared by every place that shows it.

  @oss @pro
  # source: oss:src/version.js:21-22
  # source: pro:src/screens/infra.jsx:72
  Scenario: A node without a stamped version never drifts
    Given a node whose stamped version is missing or shown as "—"
    Then the node is not reported as drifted

  @oss @pro
  # source: oss:src/version.js:19-22
  # source: pro:src/version.js:11-17
  # source: pro:src/screens/infra.jsx:72
  Scenario: A leading v makes no difference
    Given a node stamped "1.36.4" whose kubelet reports "v1.36.4"
    Then the node is not reported as drifted

  @diverged @oss @drop
  # decision: D1
  # source: oss:src/version.js:19-22
  Scenario Outline: The open dashboard ignores only the Stratum build suffix
    Given a node stamped "<stamped>" whose kubelet reports "<kubelet>"
    Then the node is <verdict>

    Examples:
      | stamped  | kubelet             | verdict                 |
      | v1.36.4  | v1.36.4-stratum     | not reported as drifted |
      | v1.36.4  | v1.36.4-stratum.2   | not reported as drifted |
      | v1.36.4  | v1.36.5-stratum     | reported as drifted     |
      | v1.36.4  | v1.36.4+k3s1        | reported as drifted     |
      | v1.36    | v1.36.0-stratum     | reported as drifted     |

  @diverged @pro
  # source: pro:src/version.js:11-17
  # source: pro:src/screens/infra.jsx:72
  Scenario Outline: The fleet dashboard ignores any build or pre-release suffix and a missing patch
    Given a node stamped "<stamped>" whose kubelet reports "<kubelet>"
    Then the node is <verdict>

    Examples:
      | stamped  | kubelet             | verdict                 |
      | v1.36.4  | v1.36.4-stratum     | not reported as drifted |
      | v1.36.4  | v1.36.4-stratum.2   | not reported as drifted |
      | v1.36.4  | v1.36.5-stratum     | reported as drifted     |
      | v1.36.4  | v1.36.4+k3s1        | not reported as drifted |
      | v1.36    | v1.36.0-stratum     | not reported as drifted |

  @diverged @oss @drop
  # decision: D1
  # source: oss:src/version.js:19
  # source: oss:src/screens/infra.jsx:64
  Scenario: The open dashboard's drift marker names the stamped version without its Stratum suffix
    Given a drifted node
    Then its marker reads "drift → <stamped version without a leading v or Stratum suffix>"

  @diverged @pro
  # source: pro:src/version.js:11-17
  # source: pro:src/screens/infra.jsx:80-86
  Scenario: The fleet dashboard's drift marker names the stamped version as major.minor.patch
    Given a drifted node
    Then its marker reads "drift → <stamped version reduced to major.minor.patch>"

  # ---- cluster-level verdict (fleet dashboard) ------------------------------------------------

  @pro
  # source: pro:src/version.js:22-35
  # source: pro:src/live.js:738
  Scenario: The cluster drifts when a node's stamp differs from the serving version
    Given the hub supplied no drift verdict of its own
    And the cluster's API server reports a version
    When any node's stamped version differs from it in major, minor or patch
    Then the cluster is reported as having version drift

  @pro
  # source: pro:src/version.js:30-34
  Scenario: Kubelet patch skew is not cluster drift
    Given the hub supplied no drift verdict of its own
    When a node's kubelet differs from the serving version only in its patch number
    Then that node does not make the cluster drift
    But a kubelet that differs in major or minor version does

  @pro
  # source: pro:src/version.js:23-29
  Scenario: An unreadable serving version yields no cluster drift
    Given the cluster's API server version is missing or has no major.minor form
    Then the cluster is not reported as having version drift

  @pro
  # source: pro:src/version.js:23-27,31-33
  Scenario: Unreadable node versions are left out of the cluster verdict
    Given a node whose stamped or kubelet version has no major.minor form
    Then that value is ignored when deciding cluster drift

  @pro
  # source: pro:src/live.js:738
  Scenario: The hub's own drift verdict takes precedence
    Given the hub supplied a drift verdict for the cluster
    Then that verdict is shown and the browser's own comparison is not used
