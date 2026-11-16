# decision: D22 — the HelmChart and HelmChartConfig lists belong to the helm domain; moved here from spec/crds
Feature: Helm Chart and Helm Chart Config lists
  Helm Charts and Helm Chart Configs are custom resources Stratum installs.
  Both dashboards give them fixed entries in the generic resource list (see
  spec/resources), in the sidebar's Helm section beside the Helm releases
  screen. What their instance lists do when reached through the custom
  resource route is specified in spec/crds/instances.feature.

  @oss @pro
  # source: oss:src/resources.jsx:221-233
  # source: oss:src/registry.js:73-77
  # source: oss:src/app.jsx:491
  # source: pro:src/resources.jsx:240-250
  # source: pro:src/nav.js:47-51
  # source: pro:src/app.jsx:514
  Scenario Outline: Each Stratum Helm kind has its own list
    When the user opens the route "<route>" or its row in the sidebar's "Helm" section
    Then the generic list for kind "<kind>" is shown, titled "<title>"
    And it is a namespaced kind
    And its columns are, in order: <columns>

    Examples:
      | route            | kind            | title              | columns                                                 |
      | helmcharts       | HelmChart       | Helm Charts        | Name, Namespace, Chart, Version, Target NS, Phase, Age  |
      | helmchartconfigs | HelmChartConfig | Helm Chart Configs | Name, Namespace, Values, Age                            |

  @oss @pro
  # source: oss:src/resources.jsx:223-226,232
  # source: pro:src/resources.jsx:242-245,249
  Scenario Outline: A Helm kind column shows a value read from the object
    Given the "<route>" list is shown
    Then the "<column>" column shows <value>

    Examples:
      | route            | column    | value                                                              |
      | helmcharts       | Chart     | the chart named in spec, or "—" when unset                         |
      | helmcharts       | Version   | the chart version in spec, or "—" when unset                       |
      | helmcharts       | Target NS | the namespace the chart installs into, or "—" when unset           |
      | helmcharts       | Phase     | the phase from status as a status badge, "Pending" when none       |
      | helmchartconfigs | Values    | the number of non-blank lines of values, as "<n> lines"            |

  @oss @pro
  # source: oss:src/resources.jsx:232
  # source: pro:src/resources.jsx:249
  Scenario Outline: Values counts only lines with content
    Given a Helm chart config whose values text is <values>
    Then its Values column reads "<shown>"

    Examples:
      | values                                   | shown   |
      | absent                                   | 0 lines |
      | three lines, one of them blank           | 2 lines |
      | one line                                 | 1 lines |

  @oss @pro
  # source: oss:src/resources.jsx:228-233
  # source: pro:src/resources.jsx:247-250
  Scenario: Helm chart configs have no Phase column
    When the Helm Chart Configs list is shown
    Then it has no status or phase column

  @diverged @oss
  # decision: D15 — both variants stand: this one on the dashboard server backend
  # source: oss:src/resources.jsx:221,230,254-257
  # source: oss:src/ui/resource-list.jsx:87,166
  Scenario: Helm charts and chart configs can be created, edited and deleted from their lists
    Given the session is offered write controls
    When the Helm Charts or Helm Chart Configs list is open
    Then Create is offered
    And a row's detail panel offers Edit and Delete

  @diverged @pro @fleet
  # decision: D15 — both variants stand: this one on the hub backend
  # source: pro:src/resources.jsx:259-274
  # source: pro:src/ui/resource-list.jsx:68,158
  Scenario: Helm charts and chart configs are read-only in their lists
    Given the signed-in user has the admin role
    When the Helm Charts or Helm Chart Configs list is open
    Then no Create button is shown
    And a row's detail panel offers no Edit or Delete
    And the manifest can still be read as YAML or JSON
