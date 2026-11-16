Feature: Editing a resource's manifest
  From the detail panel a user with write controls can edit the object's
  manifest as JSON and apply it as a full replacement.

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:298
  # source: pro:src/ui/resource-list.jsx:291
  Scenario: Edit is offered only in read-only view
    Given a detail panel is open in read-only view for a kind the user may edit
    Then the footer offers "Edit"
    And once the panel is in edit or scale mode "Edit" is not offered

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:281-282,337-342
  # source: pro:src/ui/resource-list.jsx:274-275,330-335
  Scenario: Edit opens the manifest as editable JSON
    Given a detail panel is open in read-only view
    When the user presses "Edit"
    Then the manifest is replaced by an editor holding the object as JSON indented by two spaces
    And the editor is captioned "Object (JSON) — Apply does a full replace (PUT)"
    And the managed-fields bookkeeping is not part of the text

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:282
  # source: oss:src/ui/codeview.jsx:156
  # source: pro:src/ui/resource-list.jsx:275
  # source: pro:src/ui/codeview.jsx:153
  Scenario: The editor is JSON even when the manifest was being read as YAML
    Given a detail panel is showing the manifest as YAML
    When the user presses "Edit"
    Then the editor holds JSON
    And there is no way to edit the manifest as YAML

  @oss @pro
  # source: oss:src/ui/codeview.jsx:160-198
  # source: pro:src/ui/codeview.jsx:157-195
  Scenario: The editor numbers its lines and does not wrap or autocorrect
    Given the manifest editor is open
    Then every line is numbered and long lines scroll sideways instead of wrapping
    And spell-check, autocomplete, autocorrect and auto-capitalisation are off

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:299-302,307
  # source: pro:src/ui/resource-list.jsx:292-295,300
  Scenario: Edit mode swaps the footer to Cancel and Apply
    Given the manifest editor is open
    Then the footer offers "Cancel" and "Apply"
    And the footer's "Close" is not offered, though the header's close button remains

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:300,282
  # source: pro:src/ui/resource-list.jsx:293,275
  Scenario: Cancel discards the edit
    Given the user has changed text in the manifest editor
    When the user presses "Cancel"
    Then the panel returns to the read-only manifest
    And pressing "Edit" again starts from the object's original text

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:133-134,232
  # source: pro:src/ui/resource-list.jsx:120-121,219
  Scenario: Text that is not valid JSON is refused before anything is sent
    Given the manifest editor holds text that is not valid JSON
    When the user presses "Apply"
    Then a notice reads "Invalid JSON: <the parser's message>"
    And nothing is sent to the cluster
    And the editor stays open with the text as typed

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:133-137,128,232
  # source: oss:src/resources.jsx:271-276
  # source: pro:src/ui/resource-list.jsx:120-124,115,219
  # source: pro:src/resources.jsx:280-285
  Scenario: Apply replaces the object and closes the panel
    Given the manifest editor holds valid JSON
    When the user presses "Apply" and the cluster accepts the replacement
    Then the whole object is replaced with the edited manifest
    And a notice reads "Applied changes to <Kind> <name>"
    And the list is re-read immediately
    And the panel closes

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:128,135-136,232
  # source: pro:src/ui/resource-list.jsx:115,122-123,219
  Scenario: A rejected Apply keeps the editor open
    Given the manifest editor holds valid JSON
    When the user presses "Apply" and the cluster rejects the replacement
    Then a notice reads "Error: <the cluster's message>"
    And the editor stays open with the text as typed

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:128,135-136
  # source: pro:src/ui/resource-list.jsx:115,122-123
  Scenario: An Apply the user is not permitted to make is reported like any rejection
    Given the user sees write controls but the cluster does not allow them to update the object
    When the user presses "Apply"
    Then a notice reads "Error: <the cluster's refusal message>"
    And the editor stays open

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:62,135,282
  # source: pro:src/ui/resource-list.jsx:44,122,275
  # assumption: the edited text carries the resource version captured when the row was clicked, so the cluster's own conflict check is what rejects a stale edit; the dashboard adds no check of its own. A runtime check against a real apiserver (open a panel, change the object elsewhere, Apply) would confirm the 409 conflict message reaches the notice
  Scenario: Applying an edit of an object that changed meanwhile is rejected by the cluster
    Given the manifest editor was opened from a panel whose object has since changed on the cluster
    When the user presses "Apply" without touching the resource version in the text
    Then the cluster's conflict message is shown as "Error: <message>"
    And the editor stays open, still holding the outdated manifest

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:135
  # source: oss:src/resources.jsx:271-276
  # source: pro:src/ui/resource-list.jsx:122
  # source: pro:src/resources.jsx:280-285
  Scenario: The edit always targets the object the panel was opened for
    Given the manifest editor is open for an object
    When the user changes the name or namespace inside the text and presses "Apply"
    Then the replacement is still sent to the original object's name and namespace

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:78,135,301
  # source: pro:src/ui/resource-list.jsx:59,122,294
  Scenario: Apply cannot be pressed twice
    Given an Apply has been sent and not yet answered
    Then "Apply" is disabled until the answer arrives

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:293-295
  # source: pro:src/ui/resource-list.jsx:286-288
  Scenario: Other actions stay available while editing and discard the edit without warning
    Given the manifest editor holds unsaved changes
    Then "Delete", and where the kind supports them "Restart" and "Scale", are still offered
    When the user presses one of them or uses the header's close button
    Then the unsaved changes are dropped without a confirmation
