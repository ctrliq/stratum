Feature: Helm releases
  The Helm Releases screen lists the Helm releases installed in the cluster,
  as discovered from Helm's release secrets. It is a read-only view in both
  dashboards.

  # ------------------------------------------------------------- discovery

  # source: oss:src/live.js:362-372
  # source: pro:src/live.js:701-711
  @oss @pro
  Scenario: One row per release, showing its highest revision
    Given Helm release secrets exist for release "ingress" in namespace "kube-system" at revisions 1, 2 and 3
    When the Helm Releases screen is shown
    Then a single row is shown for "ingress" in "kube-system" with Rev 3
    And its Status and Updated come from the revision-3 secret

  # source: oss:src/live.js:365-366
  # source: pro:src/live.js:704-705
  @oss @pro
  Scenario: Releases with the same name in different namespaces are separate rows
    Given a release "redis" in namespace "shop" and a release "redis" in namespace "test"
    When the Helm Releases screen is shown
    Then two rows named "redis" are shown, one per namespace

  # source: oss:src/live.js:365
  # source: pro:src/live.js:704
  @oss @pro
  Scenario: The release name falls back to the secret's name
    Given a Helm release secret that carries no "name" label
    When the Helm Releases screen is shown
    Then the row is named after the secret itself

  # source: oss:src/live.js:31-33,65-73,159-165
  # source: oss:src/cluster-context.js:97
  # decision: D1, D10
  @diverged @oss
  Scenario: Releases are discovered without reading secret contents
    When the cluster data is loaded
    Then release information is read from the server's secret summaries when the server offers them
    And otherwise from a metadata-only listing of secrets of type "helm.sh/release.v1"
    And no secret payload is transferred to the browser

  # source: pro:src/live.js:445,701-711
  # decision: D1, D10
  @diverged @pro @drop
  Scenario: Releases are discovered from the full secret list
    When the cluster data is loaded
    Then every secret of the cluster is listed in full, payload included
    And those of type "helm.sh/release.v1" are turned into release rows

  # ------------------------------------------------------------------ rows

  # source: oss:src/screens/infra.jsx:386-405
  # decision: D1
  @diverged @oss
  Scenario: The table shows seven columns and no row actions
    When the Helm Releases screen is shown
    Then the table has the columns Release, Namespace, Chart, App version, Status, Rev and Updated
    And rows offer no action and cannot be opened

  # source: pro:src/screens/infra.jsx:724-744
  # decision: D1
  @diverged @pro @drop
  Scenario: The table shows seven columns and an inert menu button per row
    When the Helm Releases screen is shown
    Then the table has the columns Release, Namespace, Chart, App version, Status, Rev and Updated
    And every row ends with a "more" button
    And pressing the button does nothing

  # source: oss:src/live.js:360-361,368
  # source: oss:src/screens/infra.jsx:399-400
  # source: pro:src/live.js:699-700,707
  # source: pro:src/screens/infra.jsx:737-738
  @oss @pro
  Scenario: Chart and app version are always a dash
    Given any installed release
    When the Helm Releases screen is shown
    Then its Chart and App version cells both read "—"

  # source: oss:src/live.js:369
  # source: oss:src/ui/shell.jsx:72-88
  # source: oss:src/screens/infra.jsx:401
  # source: pro:src/live.js:708
  # source: pro:src/ui/shell.jsx:62-77
  # source: pro:src/screens/infra.jsx:739
  # decision: D2
  @oss @pro @drop
  Scenario Outline: Status is the release's Helm status
    Given a release whose latest revision has the Helm status "<helm>"
    When the Helm Releases screen is shown
    Then its Status reads "<shown>" in the <colour> colour

    Examples:
      | helm            | shown           | colour  |
      | deployed        | Deployed        | success |
      | failed          | failed          | gray    |
      | pending-upgrade | pending-upgrade | gray    |
      | (none)          | —               | gray    |

  # source: oss:src/live.js:369
  # source: oss:src/ui/shell.jsx:72-88
  # source: pro:src/live.js:708
  # source: pro:src/ui/shell.jsx:62-77
  # decision: D2
  @fixed @oss @pro
  Scenario Outline: A release's Helm status is labelled and coloured by what it means
    Given a release whose latest revision has the Helm status "<helm>"
    When the Helm Releases screen is shown
    Then its Status reads "<shown>" in the <colour> colour

    Examples:
      | helm            | shown           | colour  |
      | deployed        | Deployed        | success |
      | failed          | Failed          | error   |
      | pending-upgrade | Pending upgrade | blue    |
      | (none)          | —               | gray    |

  # source: oss:src/live.js:365,369
  # source: pro:src/live.js:704,708
  @oss @pro
  Scenario: Revision and Updated come from the release secret
    Given a release whose latest revision secret is labelled version "7" and was created 2 hours ago
    When the Helm Releases screen is shown
    Then Rev reads "7" and Updated reads "2h"
    And a secret with a missing or non-numeric version label counts as revision 0

  # source: oss:src/screens/infra.jsx:382,393
  # source: oss:src/app.jsx:482
  # source: pro:src/screens/infra.jsx:720,731
  # source: pro:src/app.jsx:506
  # decision: D2
  @oss @pro @drop
  Scenario: Releases from every namespace are listed whatever namespace is selected
    Given releases in namespaces "shop" and "kube-system"
    When the Helm Releases screen is shown with namespace "shop" selected
    Then both releases are listed

  # source: oss:src/screens/infra.jsx:382,393
  # source: pro:src/screens/infra.jsx:720,731
  # decision: D2
  @fixed @oss @pro
  Scenario: Helm Releases follows the selected namespace
    Given releases in namespaces "shop" and "kube-system"
    When the Helm Releases screen is shown with namespace "shop" selected
    Then only the release in "shop" is listed and counted in the subtitle
    And with all namespaces selected both are listed

  # source: oss:src/screens/infra.jsx:382
  # decision: D1
  @diverged @oss @drop
  Scenario: The subtitle counts releases and points to the command palette for charts
    Given 4 releases are installed
    When the Helm Releases screen is shown
    Then the subtitle reads "4 releases installed · manage HelmChart CRs from the ⌘K palette → Helm Charts"

  # source: pro:src/screens/infra.jsx:720
  # decision: D1, D2
  @diverged @pro @drop
  Scenario: The subtitle counts releases and points to the Extensions menu for charts
    Given 4 releases are installed
    When the Helm Releases screen is shown
    Then the subtitle reads "4 releases installed · manage charts under Extensions → Helm Charts"

  # source: oss:src/screens/infra.jsx:382
  # source: oss:src/registry.js:73-77
  # source: pro:src/screens/infra.jsx:720
  # source: pro:src/nav.js:47-51
  # decision: D1, D2
  @fixed @oss @pro
  Scenario: The subtitle counts releases and points to where charts are actually listed
    Given 4 releases are installed
    When the Helm Releases screen is shown
    Then the subtitle reads "4 releases installed · manage charts under Helm → Helm Charts"

  # source: oss:src/screens/infra.jsx:382,393
  # source: pro:src/screens/infra.jsx:720,731
  # assumption: no empty-state message is coded for this table in either root
  # decision: D2
  @oss @pro @drop
  Scenario: A cluster with no releases shows an empty table
    Given no Helm release secret exists
    When the Helm Releases screen is shown
    Then the subtitle begins "0 releases installed" and the table has no rows

  # source: oss:src/screens/infra.jsx:382,393
  # source: pro:src/screens/infra.jsx:720,731
  # decision: D2, D9
  @fixed @oss @pro
  Scenario: No releases and unreadable release data are told apart
    When no Helm release exists
    Then the table area says no releases are installed
    And when the release data cannot be read or is forbidden the screen says so instead, keeping any rows it already had

  # ------------------------------------------------------------ unhappy paths

  # source: oss:src/live.js:92,372,436,452-455
  # source: oss:src/app.jsx:390-394
  # decision: D1, D9
  @diverged @oss
  Scenario: A failed or forbidden secrets read keeps the last known releases
    Given the Helm Releases screen shows 4 releases
    When a reload cannot read the secret information, for example because the user is forbidden
    Then the 4 releases stay on screen
    And a manual refresh started from another screen shows the toast "Refreshed — HELM kept last data"

  # source: pro:src/live.js:423-430,445,711
  # decision: D1, D2
  @diverged @pro @drop
  Scenario: A failed or forbidden secrets read empties the release list
    Given the Helm Releases screen shows 4 releases
    When a reload cannot read the secrets while other reads succeed
    Then the table becomes empty and the subtitle begins "0 releases installed"
    And nothing on the screen says the read failed

  # source: oss:src/live.js:45-51,160
  @oss
  Scenario: A malformed secret summary answer is treated as a failed read
    Given the server's secret summaries answer does not have the expected shape
    When the cluster data is loaded
    Then the last known releases stay on screen

  # source: oss:src/live.js:53-57,161-165
  @oss
  Scenario: A malformed secrets table answer is treated as a failed read
    Given the server offers no secret summaries
    And the metadata-only secrets listing lacks its Type or Data column, a cell per column, or metadata on some row
    When the cluster data is loaded
    Then the last known releases stay on screen

  # --------------------------------------------------------------- no writes

  # source: oss:src/screens/infra.jsx:376-411
  # source: pro:src/screens/infra.jsx:714-750
  @oss @pro
  Scenario: Releases cannot be installed, upgraded, rolled back or uninstalled from this screen
    Given a user allowed to write
    When the Helm Releases screen is shown
    Then no install, upgrade, rollback, uninstall or values control is offered
    And release values and manifests cannot be viewed

  # source: oss:src/app.jsx:314-323
  # source: pro:src/app.jsx:270-299
  # decision: D9
  @oss @pro
  Scenario: The release list refreshes with the cluster's periodic reload
    Given auto-refresh is on
    When the periodic reload completes
    Then the release list reflects the newly read release secrets
    And the screen offers no refresh button of its own
