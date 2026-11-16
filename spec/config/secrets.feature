Feature: How secrets are exposed
  Secrets go through the generic list, detail and edit flow with no special
  handling. This feature pins what that means for their values, because a
  rewrite has to decide deliberately whether to keep it.

  @oss @pro
  # source: oss:src/resources.jsx:118-122
  # source: pro:src/resources.jsx:137-141
  Scenario: The Secrets list shows no secret values
    When the Secrets list is shown
    Then each row shows the secret's name, namespace, type, number of keys and age
    And no value or key name appears in the list

  @oss @pro
  # decision: D21 — secret values stay visible in the detail panel; flagged for a product decision
  # source: oss:src/ui/resource-list.jsx:27-32,281,345-346
  # source: oss:src/resources.jsx:118-122
  # source: pro:src/ui/resource-list.jsx:29-34,274,338-339
  # source: pro:src/resources.jsx:137-141
  # assumption: nothing masks, hides or decodes secret data anywhere in the detail panel; inferred from the absence of any kind-specific handling
  Scenario: Opening a secret shows every value in its stored encoding
    Given the user may read secrets
    When the user clicks a secret's row
    Then the detail panel's manifest shows every key with its base64-encoded value, unmasked
    And there is no reveal step and no decoded view

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:281-282
  # source: pro:src/ui/resource-list.jsx:274-275
  # assumption: as above, no kind-specific handling exists in the editor
  Scenario: Editing a secret edits its encoded values as JSON
    Given the session is offered write controls
    When the user presses "Edit" on a secret
    Then the editor holds the secret's manifest with the base64-encoded values
    And any value the user changes must be supplied already encoded

  @oss @pro @drop
  # decision: D10 — secrets are never listed with their data
  # source: oss:src/ui/resource-list.jsx:102-114
  # source: oss:src/resources.jsx:118
  # source: pro:src/ui/resource-list.jsx:86-101
  # source: pro:src/resources.jsx:137
  Scenario: The open Secrets list repeatedly reads every secret in full
    Given the Secrets list is open
    Then each refresh reads all secrets of all namespaces including their data
    And only the key count of each is displayed

  @oss @pro @fixed
  # decision: D10, D21 — replaces the scenario above
  # source: oss:src/ui/resource-list.jsx:102-114
  # source: oss:src/resources.jsx:118-122
  # source: pro:src/ui/resource-list.jsx:86-101
  # source: pro:src/resources.jsx:137-141
  # assumption: the key count can be obtained without the values (for instance from the cluster's own table view of secrets); a runtime check against a real apiserver would confirm it, otherwise the Keys column has to go
  Scenario: The Secrets list is read without secret values
    Given the Secrets list is open
    Then no refresh of the list brings any secret's values to the browser
    And each row still shows name, namespace, type, number of keys and age
    And a secret's values are read only when the user opens that secret's detail panel

  @oss @pro @drop
  # decision: D9 — a forbidden kind gets its own state
  # source: oss:src/ui/resource-list.jsx:107,206-212
  # source: pro:src/ui/resource-list.jsx:94,194-200
  Scenario: A user who may not list secrets sees the generic failure
    Given the signed-in user is not allowed to list secrets
    When the Secrets list is opened
    Then the table shows "Unable to load — the apiserver returned an error (or the RBAC lacks access)."

  @oss @pro @fixed
  # decision: D9 — replaces the scenario above; the state itself is specified in spec/resources/list.feature
  # source: oss:src/ui/resource-list.jsx:107,206-212
  # source: pro:src/ui/resource-list.jsx:94,194-200
  Scenario: A user who may not list secrets sees the forbidden state
    Given the signed-in user is not allowed to list secrets
    When the Secrets list is opened
    Then the table says the user is not permitted to list secrets, in words distinct from a failed load
