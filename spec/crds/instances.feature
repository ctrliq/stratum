Feature: Custom resource instance lists
  Any custom kind's instances are listed by the generic resource list (see
  spec/resources) at an address naming the kind's API group, version and
  plural. The list is shaped from the kind's definition when that can be
  read, and from the address alone when it cannot.

  # ---- address ---------------------------------------------------------------

  @diverged @oss @drop
  # decision: D1, D11 — every cluster screen lives under /c/$cluster/…; the old address redirects there (spec/shell)
  # source: oss:src/app.jsx:4,131-134
  Scenario: An instance list is addressed by group, version and plural
    When the user opens "/crds/example.io/v1/widgets"
    Then the instance list for "widgets" in "example.io" at version "v1" is shown

  @diverged @pro
  # decision: D11 — applies to both backends; a single-cluster backend uses the cluster id "local"
  # source: pro:src/app.jsx:5,59-60
  Scenario: An instance list is addressed under its cluster by group, version and plural
    When the user opens "/c/<cluster id>/crds/example.io/v1/widgets"
    Then that cluster's instance list for "widgets" in "example.io" at version "v1" is shown

  # ---- resolving the definition ---------------------------------------------

  @oss @pro
  # source: oss:src/app.jsx:348-354,477-478
  # source: pro:src/app.jsx:323-329,500-501
  Scenario: The page says it is resolving while the definition is read
    Given an instance list address was opened
    When the kind's definition has not been answered yet
    Then the page reads "Resolving <plural>.<group>…" and shows no list

  @oss @pro
  # source: oss:src/app.jsx:349-352,370
  # source: pro:src/app.jsx:324-327,340
  Scenario: Moving to another kind or version resolves again
    Given an instance list is shown
    When the address changes to another group, version or plural
    Then the list is replaced by the "Resolving…" message until the new definition is answered
    And a late answer for the previous address is ignored

  @oss @pro
  # source: oss:src/app.jsx:348-370
  # source: pro:src/app.jsx:323-340
  # assumption: the definition is read once per address; nothing re-reads it while the list stays open, inferred from the absence of a timer
  Scenario: A definition changed while its instances are on screen is not picked up
    Given an instance list is shown
    When the kind's definition changes on the cluster
    Then the list keeps the shape it was given until the user navigates away and back

  @oss @pro
  # source: oss:src/app.jsx:462
  # source: pro:src/app.jsx:474
  Scenario Outline: The breadcrumb names the kind under Custom Resources
    Given an instance list address for plural "widgets" whose kind is "Widget"
    When the page is <state>
    Then the breadcrumb ends "Custom Resources › <last>"

    Examples:
      | state                    | last    |
      | still resolving          | widgets |
      | resolved                 | Widget  |

  @diverged @oss @drop
  # decision: D1 — "← Definitions" names the row it returns to
  # source: oss:src/app.jsx:474
  Scenario: A back button labelled Custom Resources returns to the definitions
    Given an instance list page is shown, resolved or not
    When the user presses "← Custom Resources"
    Then the Definitions list opens, keeping the selected namespace

  @diverged @pro
  # source: pro:src/app.jsx:489
  Scenario: A back button labelled Definitions returns to the definitions
    Given an instance list page is shown, resolved or not
    When the user presses "← Definitions"
    Then the Definitions list opens, keeping the selected namespace

  # ---- shape when the definition is readable --------------------------------

  @diverged @oss @drop
  # decision: D1 — pro's title plus tags is the superset
  # source: oss:src/resources.jsx:289-303
  Scenario Outline: The list is titled with the kind and its API group
    Given the definition declares kind <kind> and plural "widgets" in group "example.io"
    Then the instance list is titled "<title>"

    Examples:
      | kind     | title                |
      | "Widget" | Widget (example.io)  |
      | nothing  | widgets (example.io) |

  @diverged @pro
  # source: pro:src/resources.jsx:307-329
  # source: pro:src/ui/resource-list.jsx:141-146
  Scenario Outline: The list is titled with the kind and tagged with its API facts
    Given the definition declares kind "Widget" in group "example.io", scope <scope> and short names <short names>
    And the address names version "v1"
    Then the instance list is titled "Widget"
    And tags under the title read <tags>

    Examples:
      | scope      | short names | tags                                                  |
      | Namespaced | none        | "example.io/v1", "Namespaced"                         |
      | Cluster    | none        | "example.io/v1", "Cluster-scoped"                     |
      | Namespaced | wd and wdg  | "example.io/v1", "Namespaced", "short: wd, wdg"       |

  @diverged @oss @drop
  # decision: D1 — pro's printer columns are the superset
  # source: oss:src/resources.jsx:300
  Scenario Outline: Instances show name, namespace and age only
    Given the definition declares scope <scope>, with or without printer columns
    Then the instance list's columns are, in order: <columns>

    Examples:
      | scope      | columns              |
      | Namespaced | Name, Namespace, Age |
      | Cluster    | Name, Age            |

  @diverged @pro
  # source: pro:src/resources.jsx:321,327
  Scenario Outline: Instances show the definition's printer columns between identity and age
    Given the definition declares scope <scope> and, for the version in the address, the printer columns "Phase" and "Replicas"
    Then the instance list's columns are, in order: <columns>

    Examples:
      | scope      | columns                                |
      | Namespaced | Name, Namespace, Phase, Replicas, Age  |
      | Cluster    | Name, Phase, Replicas, Age             |

  @oss @pro
  # source: oss:src/resources.jsx:295,299
  # source: oss:src/ui/resource-list.jsx:118,157
  # source: pro:src/resources.jsx:318,324
  # source: pro:src/ui/resource-list.jsx:105,149
  Scenario: A namespaced custom kind follows the namespace selector
    Given the definition declares scope "Namespaced"
    When a namespace is selected
    Then only instances in that namespace are rows
    And a cluster-scoped custom kind ignores the selection

  @oss @pro
  # source: oss:src/app.jsx:356-360
  # source: pro:src/app.jsx:332
  # source: pro:src/resources.jsx:313,319,324
  Scenario: Instances are read at the version the address names
    Given the definition serves "v1" and "v1beta1"
    When the address names version "v1beta1"
    Then the instances are read, created, edited and deleted at "v1beta1"

  @oss @pro
  # source: oss:src/app.jsx:356-360
  # source: oss:src/ui/resource-list.jsx:107,206-212
  # source: pro:src/app.jsx:332
  # source: pro:src/resources.jsx:310-313
  # source: pro:src/ui/resource-list.jsx:94,194-200
  Scenario: A version the definition does not have fails like any unreadable list
    Given the address names a version the definition does not list
    Then the page still shows a list shaped from the definition
    And the table shows "Unable to load — the apiserver returned an error (or the RBAC lacks access)."

  @pro
  # source: pro:src/resources.jsx:310-312,321
  Scenario: A version the definition does not have contributes no printer columns
    Given the address names a version the definition does not list
    Then the list's columns are Name, Namespace for a namespaced kind, and Age only

  @oss @pro
  # source: oss:src/app.jsx:477
  # source: pro:src/app.jsx:500
  Scenario: A link can open one instance
    When the user opens an instance list address with ?name=<namespace>/<name>
    Then the detail panel opens for that instance once the list loads

  @diverged @oss
  # decision: D15 — both variants stand: this one on the dashboard server backend
  # source: oss:src/resources.jsx:301
  # source: oss:src/ui/resource-list.jsx:87,166
  # source: oss:src/ui/resource-list.jsx:22-26,221-224
  Scenario: Instances of every custom kind can be created, edited and deleted
    Given the session is offered write controls
    When an instance list is shown for any custom kind, Stratum's own included
    Then Create is offered with a skeleton naming "<group>/<version>" and the kind
    And an instance's detail panel offers Edit and Delete and no Scale or Restart

  @diverged @pro @fleet
  # decision: D15 — both variants stand: this one on the hub backend
  # source: pro:src/resources.jsx:270,320,328
  # source: pro:src/ui/resource-list.jsx:68,158
  Scenario Outline: Instances can be created, edited and deleted except Stratum's Helm kinds
    Given the signed-in user has the admin role
    When an instance list is shown for "<plural>" in group "<group>"
    Then Create, Edit and Delete are <offered>

    Examples:
      | group           | plural           | offered     |
      | example.io      | widgets          | offered     |
      | stratum.ciq.com | helmcharts       | not offered |
      | stratum.ciq.com | helmchartconfigs | not offered |
      | example.io      | helmcharts       | offered     |

  # ---- switching version -----------------------------------------------------

  @pro
  # source: pro:src/app.jsx:490-497
  # source: pro:src/crd-columns.js:64-66
  # source: pro:src/resources.jsx:325
  Scenario: A kind served at several versions offers a version switch
    Given the definition serves "v1" and "v1beta1" and also lists an unserved "v1alpha1"
    When the instance list is shown at "v1"
    Then beside the back button a group labelled "API version" offers "v1" and "v1beta1" only
    And "v1" is marked as the pressed one

  @pro
  # source: pro:src/app.jsx:490-494
  # source: pro:src/app.jsx:345-354
  Scenario: Choosing a version opens the same kind at that version
    Given the version switch is shown
    When the user chooses "v1beta1"
    Then the address changes to the same group and plural at "v1beta1", keeping the selected namespace and dropping ?name=
    And the list resolves again and shows that version's printer columns

  @pro
  # source: pro:src/app.jsx:490
  Scenario: A kind served at one version has no version switch
    Given the definition serves a single version
    Then no version switch is shown

  @pro
  # source: pro:src/app.jsx:492-494
  Scenario: An address at a version that is not served marks no version as current
    Given the definition serves "v1" and "v1beta1"
    When the address names "v1alpha1"
    Then the version switch offers "v1" and "v1beta1" with neither pressed

  @oss @drop
  # decision: D1 — pro's version switch is the superset
  # source: oss:src/app.jsx:471-480
  # assumption: there is no version control on the page; another version is reached only by editing the address, inferred from the absence of any such control
  Scenario: Another version of a kind is reached only through the address
    Given the definition serves "v1" and "v1beta1"
    When the instance list is shown at "v1"
    Then nothing on the page offers "v1beta1"

  # ---- when the definition cannot be read -----------------------------------

  @oss @pro
  # source: oss:src/app.jsx:361-367,475
  # source: pro:src/app.jsx:333-337,498
  # source: pro:src/resources.jsx:331-338
  Scenario: An unreadable or missing definition still lists what the address names
    Given the user may not read the kind's definition, or no such definition exists
    When the instance list address is opened
    Then a note beside the back button reads "definition not readable — listing <the API path of the group, version and plural>"
    And the generic list is shown for that path, titled "<plural> (<group>)"
    And its only column is Name

  @oss @pro
  # source: oss:src/app.jsx:364-365
  # source: pro:src/resources.jsx:332-333
  Scenario Outline: Without a definition the kind's name is guessed from the plural
    Given the definition for plural "<plural>" cannot be read
    Then the page's wording and the breadcrumb use the kind "<kind>"

    Examples:
      | plural   | kind    |
      | widgets  | Widget  |
      | policies | Policy  |
      | gateways | Gateway |
      | meshes   | Meshe   |
      | status   | Statu   |
      | sheep    | Sheep   |

  @oss @pro @drop
  # decision: D2 — legacy defect (wrong rows, wrong address), not reproduced
  # source: oss:src/app.jsx:365
  # source: oss:src/ui/resource-list.jsx:118,276
  # source: pro:src/resources.jsx:318,335
  # source: pro:src/ui/resource-list.jsx:105,269
  Scenario: Without a definition every kind is treated as cluster-scoped
    Given the definition cannot be read and the kind is in fact namespaced
    When a namespace is selected
    Then instances from every namespace are rows and nothing shows which namespace a row is in
    And the detail panel's summary has no Namespace line, though the manifest shows it

  @oss @pro @drop
  # decision: D2 — legacy defect (wrong address), not reproduced
  # source: oss:src/app.jsx:365-366
  # source: oss:src/resources.jsx:271-284
  # source: pro:src/resources.jsx:280-293,335
  # assumption: the cluster rejects the cluster-scoped address for a namespaced kind; the dashboard only builds the address. A runtime check with a namespaced custom kind whose definition the user cannot read would confirm the not-found style refusal and its wording in the notice
  Scenario: Without a definition, writes to a namespaced kind go to the wrong address
    Given the definition cannot be read and the kind is in fact namespaced
    And the session is offered write controls
    When the user edits, deletes or creates an instance
    Then the request is addressed as if the kind were cluster-scoped
    And the cluster's refusal is shown as "Error: <message>"

  @oss @pro @fixed
  # decision: D2 — replaces the two scenarios above
  # source: oss:src/app.jsx:365-366
  # source: oss:src/resources.jsx:271-284
  # source: pro:src/resources.jsx:280-293,318,335
  Scenario: Without a definition, instances that carry a namespace are handled as namespaced
    Given the definition cannot be read and the listed instances carry a namespace
    Then the list has a Namespace column and follows the namespace selector
    And the detail panel's summary shows the instance's namespace
    And an edit or delete is addressed to the instance within its own namespace

  @diverged @oss
  # decision: D15 — both variants stand: this one on the dashboard server backend
  # source: oss:src/app.jsx:365-366
  Scenario: Without a definition every kind offers Create, Edit and Delete
    Given the definition cannot be read
    And the session is offered write controls
    Then Create, Edit and Delete are offered for the listed instances, whatever the kind

  @diverged @pro @fleet
  # decision: D15 — both variants stand: this one on the hub backend
  # source: pro:src/resources.jsx:320,328,331-337
  Scenario: Without a definition Stratum's Helm kinds are still read-only
    Given the definition cannot be read
    And the signed-in user has the admin role
    Then Create, Edit and Delete are offered for the listed instances
    But not when the address names "helmcharts" or "helmchartconfigs" in group "stratum.ciq.com"

  @pro
  # source: pro:src/resources.jsx:336
  # source: pro:src/app.jsx:490
  Scenario: Without a definition the page shows one tag and no version switch
    Given the definition cannot be read
    Then the only tag under the title reads "<group>/<version>" from the address
    And no version switch is shown

  @oss @pro
  # source: oss:src/app.jsx:361-367
  # source: oss:src/ui/resource-list.jsx:107,206-212
  # source: pro:src/app.jsx:333-337
  # source: pro:src/ui/resource-list.jsx:94,194-200
  Scenario: An address that names nothing real shows the note and a failed list
    Given the address names a group, version and plural the cluster does not serve
    Then the note "definition not readable — listing <path>" is shown
    And the table shows "Unable to load — the apiserver returned an error (or the RBAC lacks access)."

  @pro @fleet
  # source: pro:src/app.jsx:323-340
  Scenario: Switching cluster resolves the same kind on the new cluster
    Given an instance list is shown on one cluster
    When the address changes to the same kind on another cluster
    Then the page shows "Resolving…" and reads the definition from the new cluster
