Feature: Creating a resource from a manifest
  Every kind's list offers Create to users with write controls: a JSON
  manifest skeleton that is posted to the cluster.

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:166-170,219-226,355-358
  # source: pro:src/ui/resource-list.jsx:158-162,207-214,348-351
  Scenario: Create opens a panel beside the list
    Given a kind's list is open and the user has write controls
    When the user presses "Create"
    Then a panel titled "Create <Kind>" opens beside the list
    And any open detail panel closes and no row is selected

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:22-26,221-224
  # source: pro:src/ui/resource-list.jsx:24-28,209-212
  Scenario Outline: The panel starts from a skeleton for the kind
    Given the list of <kind> is open with <selection> selected
    When the user presses "Create"
    Then the editor holds JSON with apiVersion "<apiVersion>", kind "<Kind>" and an empty metadata name
    And metadata namespace is <namespace>

    Examples:
      | kind            | selection         | apiVersion | Kind         | namespace |
      | deployments     | namespace team-a  | apps/v1    | Deployment   | "team-a"  |
      | deployments     | All namespaces    | apps/v1    | Deployment   | "default" |
      | config maps     | namespace team-a  | v1         | ConfigMap    | "team-a"  |
      | storage classes | namespace team-a  | storage.k8s.io/v1 | StorageClass | absent |

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:355-356
  # source: pro:src/ui/resource-list.jsx:348-349
  Scenario: The skeleton is fixed when the panel opens
    Given the create panel is open
    When the user changes the selected namespace
    Then the text in the editor does not change

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:366
  # source: pro:src/ui/resource-list.jsx:359
  Scenario Outline: The panel says where the manifest will be posted
    Given the create panel is open for a <scope> kind
    Then the editor is captioned "Manifest (JSON) — POST to the <target>"

    Examples:
      | scope          | target    |
      | namespaced     | namespace |
      | cluster-scoped | cluster   |

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:138-139,226
  # source: pro:src/ui/resource-list.jsx:125-126,214
  Scenario: Text that is not valid JSON is refused before anything is sent
    Given the create panel holds text that is not valid JSON
    When the user presses "Create" in the panel
    Then a notice reads "Invalid JSON: <the parser's message>"
    And nothing is sent and the panel stays open

  @oss @pro @drop
  # decision: D2 — legacy defect (silent failure), not reproduced
  # source: oss:src/ui/resource-list.jsx:138-142
  # source: pro:src/ui/resource-list.jsx:125-129
  Scenario: A manifest that is the JSON literal null is posted empty and reports nothing
    Given the create panel's text is replaced by the word null
    When the user presses "Create" in the panel
    Then a request with no manifest is posted to the kind's collection
    And neither a "Created…" nor an "Error:…" notice appears, whatever the cluster answered
    And the panel stays open

  @oss @pro @fixed
  # decision: D2 — replaces the scenario above
  # source: oss:src/ui/resource-list.jsx:138-142
  # source: pro:src/ui/resource-list.jsx:125-129
  Scenario: A manifest that is not a JSON object is refused before anything is sent
    Given the create panel holds valid JSON that is not an object, such as null, a number or a list
    When the user presses "Create" in the panel
    Then a notice says the manifest must be a JSON object
    And nothing is sent and the panel stays open

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:138-143,226
  # source: pro:src/ui/resource-list.jsx:125-130,214
  Scenario: A created object is announced and the panel closes
    Given the create panel holds a valid manifest
    When the user presses "Create" in the panel and the cluster accepts it
    Then a notice reads "Created <Kind> <the manifest's name>"
    And the list is re-read immediately
    And the panel closes

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:128,141-142,226
  # source: pro:src/ui/resource-list.jsx:115,128-129,214
  Scenario: A rejected create keeps the panel open
    Given the create panel holds a valid manifest
    When the user presses "Create" in the panel and the cluster rejects or forbids it
    Then a notice reads "Error: <the cluster's message>"
    And the panel stays open with the text as typed

  @oss @pro
  # source: oss:src/resources.jsx:279-284
  # source: oss:src/ui/resource-list.jsx:141
  # source: pro:src/resources.jsx:288-293
  # source: pro:src/ui/resource-list.jsx:128
  Scenario Outline: A namespaced object is created in the namespace its manifest names
    Given the create panel is open for a namespaced kind with <selection> selected
    And the manifest's metadata namespace is <manifest namespace>
    When the user creates it
    Then it is posted to namespace "<target>"

    Examples:
      | selection        | manifest namespace | target  |
      | namespace team-a | "team-b"           | team-b  |
      | namespace team-a | removed            | team-a  |
      | All namespaces   | removed            | default |
      | All namespaces   | "team-b"           | team-b  |

  @oss @pro
  # source: oss:src/resources.jsx:281
  # source: pro:src/resources.jsx:290
  Scenario: A cluster-scoped object is posted to the cluster regardless of namespace
    Given the create panel is open for a cluster-scoped kind
    When the user creates a manifest, whatever namespace is selected
    Then it is posted to the kind's cluster-wide collection

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:138-143
  # source: pro:src/ui/resource-list.jsx:125-130
  # assumption: the manifest's own kind and apiVersion are not checked against the list's kind before posting; inferred from the absence of a check
  Scenario: The manifest is posted to the open list's kind whatever kind it declares
    Given the create panel is open for one kind
    When the user replaces the manifest with one of a different kind and creates it
    Then it is still posted to the open list's collection and the cluster decides

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:361-362
  # source: oss:src/ui/shell.jsx:156-160
  # source: pro:src/ui/resource-list.jsx:354-355
  # source: pro:src/ui/shell.jsx:150-154
  Scenario: Cancelling create discards the manifest
    Given the create panel holds typed text
    When the user presses "Cancel" or the header's close button
    Then the panel closes without sending anything
    And reopening Create starts from a fresh skeleton

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:78,362
  # source: pro:src/ui/resource-list.jsx:59,355
  Scenario: Create cannot be pressed twice
    Given a create has been sent and not yet answered
    Then the panel's "Create" button is disabled until the answer arrives
