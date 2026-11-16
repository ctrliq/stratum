Feature: Helm chart resources
  HelmChart and HelmChartConfig objects ("stratum.ciq.com/v1") are the
  cluster's declarative way to install charts. Both dashboards list them
  with the generic resource list; they differ in whether the list may write.

  # The list, search, detail pane, YAML view/edit, create and delete mechanics
  # are the generic ones specified in the `resources` domain. Only what is
  # specific to these two kinds is specified here.
  # decision: D22 — these two list screens belong to this domain.
  # In the fleet dashboard every HelmChart write (install, uninstall) goes
  # through the App Catalog screen, specified in the `catalog` domain.

  # ------------------------------------------------------------- navigation

  # source: oss:src/registry.js:73-77
  # source: pro:src/nav.js:47-51
  @oss @pro
  Scenario: The sidebar has a Helm group with three entries
    When the sidebar is shown
    Then a "Helm" group lists "Releases", "Helm Charts" and "Helm Chart Configs"

  # source: oss:src/ui/palette.jsx:21,38-41
  # source: pro:src/ui/palette.jsx:23,53-55
  @oss @pro
  Scenario: The command palette can open the Helm screens
    When the user searches the command palette for "helm"
    Then it offers "Helm releases", "Helm Charts" and "Helm Chart Configs"

  # ------------------------------------------------------------- HelmCharts

  # source: oss:src/resources.jsx:221-227
  # source: pro:src/resources.jsx:240-246
  @oss @pro
  Scenario: Helm Charts lists HelmChart objects with chart-specific columns
    Given HelmChart objects exist in the cluster
    When the Helm Charts screen is shown
    Then the objects are read from "/apis/stratum.ciq.com/v1/helmcharts"
    And the columns are Name, Namespace, Chart, Version, Target NS, Phase and Age
    And Chart, Version and Target NS show the object's "spec.chart", "spec.version" and "spec.targetNamespace", or "—" when unset

  # source: oss:src/resources.jsx:226
  # source: pro:src/resources.jsx:245
  @oss @pro
  Scenario: A chart with no reported phase reads Pending
    Given a HelmChart whose status has no phase
    When the Helm Charts screen is shown
    Then its Phase reads "Pending"
    And a HelmChart whose status phase is "Deployed" reads "Deployed" in the success colour

  # source: oss:src/resources.jsx:221
  # source: pro:src/resources.jsx:240
  # source: oss:src/ui/resource-list.jsx:118-119
  # source: pro:src/ui/resource-list.jsx:105-106
  @oss @pro
  Scenario: Helm Charts follows the selected namespace
    Given HelmCharts in namespaces "kube-system" and "shop"
    When the Helm Charts screen is shown with namespace "shop" selected
    Then only the HelmCharts in "shop" are listed

  # source: oss:src/resources.jsx:254-257
  # source: oss:src/ui/resource-list.jsx:83-87,166-170
  # decision: D14, D15
  @diverged @oss
  Scenario: A session that can write may create, edit and delete HelmCharts
    Given a signed-in user, or a server that requires no sign-in
    When the Helm Charts screen is shown
    Then a Create button is offered
    And an opened HelmChart offers Edit and Delete
    And whether a write succeeds is decided by the cluster's own access rules

  # source: pro:src/resources.jsx:259-274
  # source: pro:src/ui/resource-list.jsx:64-68,158
  # decision: D14, D15
  @diverged @pro
  Scenario: HelmCharts are read-only in the resource list, even for admins
    Given a user with the "admin" role
    When the Helm Charts screen is shown
    Then no Create button is offered
    And an opened HelmChart offers neither Edit nor Delete

  # source: oss:src/ui/resource-list.jsx:83-87,166
  # source: pro:src/ui/resource-list.jsx:64-68,158
  # decision: D14
  @oss @pro
  Scenario: A user who cannot write sees HelmCharts without write controls
    Given a user who is not allowed to write
    When the Helm Charts screen is shown
    Then the HelmCharts are listed and can be opened
    And no Create, Edit or Delete control is offered

  # ------------------------------------------------------- HelmChartConfigs

  # source: oss:src/resources.jsx:228-233
  # source: pro:src/resources.jsx:247-250
  @oss @pro
  Scenario: Helm Chart Configs lists HelmChartConfig objects with a values size
    Given a HelmChartConfig whose "spec.valuesContent" has 12 non-blank lines
    When the Helm Chart Configs screen is shown
    Then the objects are read from "/apis/stratum.ciq.com/v1/helmchartconfigs"
    And the columns are Name, Namespace, Values and Age
    And its Values cell reads "12 lines"
    And a HelmChartConfig with no values content reads "0 lines"

  # source: oss:src/resources.jsx:228-233
  # source: pro:src/resources.jsx:247-250
  @oss @pro
  Scenario: Helm Chart Configs has no phase column
    When the Helm Chart Configs screen is shown
    Then no status or phase is shown for a HelmChartConfig

  # source: oss:src/resources.jsx:254-257
  # source: oss:src/ui/resource-list.jsx:83-87,166-170
  # decision: D14, D15
  @diverged @oss
  Scenario: A session that can write may create, edit and delete HelmChartConfigs
    Given a signed-in user, or a server that requires no sign-in
    When the Helm Chart Configs screen is shown
    Then a Create button is offered
    And an opened HelmChartConfig offers Edit and Delete

  # source: pro:src/resources.jsx:267-274
  # source: pro:src/ui/resource-list.jsx:64-68,158
  # decision: D14, D15
  @diverged @pro
  Scenario: HelmChartConfigs are read-only in the resource list, even for admins
    Given a user with the "admin" role
    When the Helm Chart Configs screen is shown
    Then no Create button is offered
    And an opened HelmChartConfig offers neither Edit nor Delete

  # ------------------------------------------------------ other entry points

  # source: pro:src/resources.jsx:320,328
  # decision: D15
  @pro
  Scenario: Browsing the same kinds through Custom Resources is read-only too
    Given an admin opens the instances of the "helmcharts" or "helmchartconfigs" definition of group "stratum.ciq.com" from Custom Resources
    Then no Create, Edit or Delete control is offered there either

  # source: oss:src/app.jsx:347-361
  # source: oss:src/resources.jsx:301
  # source: oss:src/ui/resource-list.jsx:166-170
  # decision: D15
  @oss
  Scenario: Browsing the same kinds through Custom Resources allows edit and delete
    Given a session that can write opens the instances of the "helmcharts" definition from Custom Resources
    Then Create, Edit and Delete are offered as for any custom resource

  # source: pro:src/resources.jsx:259-263
  # source: pro:src/screens/catalog.jsx:319-329,638
  @pro
  Scenario: Installing and uninstalling charts is done from the App Catalog
    Given an admin
    When the Helm Charts screen is shown
    Then it offers no install or uninstall control
    And the App Catalog screen offers Install for a catalog app and Uninstall for an installed one

  # source: oss:src/resources.jsx:221-233,254-257
  # source: pro:src/resources.jsx:240-250,270-274
  @oss @pro
  Scenario: Neither resource list offers a values editor, upgrade or rollback
    When a HelmChart or HelmChartConfig is opened
    Then no dedicated values form, upgrade action or rollback action is offered
    And the only way to change values, where writes are allowed, is editing the object's manifest
