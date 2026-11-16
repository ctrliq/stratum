Feature: Deleting a resource
  From the detail panel a user with write controls can delete the object
  after confirming.

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:293
  # source: pro:src/ui/resource-list.jsx:286
  Scenario: Delete is offered in the detail panel
    Given a detail panel is open for a kind the user may delete
    Then the footer offers "Delete" in every mode of the panel

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:145,246-259
  # source: pro:src/ui/resource-list.jsx:132,239-252
  Scenario Outline: Delete asks for confirmation and names the object
    Given a detail panel is open for <object>
    When the user presses "Delete"
    Then the detail panel closes
    And a dialog titled "Delete <Kind>?" says "<named> will be permanently deleted."
    And the dialog offers "Cancel" and "Delete"

    Examples:
      | object                                   | Kind       | named        |
      | Deployment "api" in namespace "payments" | Deployment | payments/api |
      | Namespace "payments"                     | Namespace  | payments     |

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:247,251
  # source: pro:src/ui/resource-list.jsx:240,244
  Scenario: Cancelling the confirmation deletes nothing
    Given the delete confirmation is open
    When the user presses "Cancel" or dismisses the dialog
    Then nothing is sent to the cluster
    And the dialog closes and the detail panel stays closed

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:130,128,253
  # source: oss:src/resources.jsx:271-276
  # source: pro:src/ui/resource-list.jsx:117,115,246
  # source: pro:src/resources.jsx:280-285
  Scenario: Confirming deletes the object
    Given the delete confirmation is open
    When the user presses "Delete" and the cluster accepts
    Then the dialog closes
    And a notice reads "Deleted <Kind> <name>"
    And the list is re-read immediately

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:128,130
  # source: pro:src/ui/resource-list.jsx:115,117
  Scenario: A refused delete is reported and nothing reopens
    Given the delete confirmation is open
    When the user presses "Delete" and the cluster rejects or forbids it
    Then a notice reads "Error: <the cluster's message>"
    And the object's row is still in the list
    And neither the dialog nor the detail panel reopens

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:130,183
  # source: pro:src/ui/resource-list.jsx:117,175
  # assumption: the row is removed only by the re-read; an object the cluster is still finalising remains a row until a later refresh no longer returns it
  Scenario: A deleted object leaves the list when the cluster stops returning it
    Given a delete was accepted for an object the cluster removes asynchronously
    Then the object stays a row until a refresh no longer includes it

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:62,130,145
  # source: pro:src/ui/resource-list.jsx:44,117,132
  Scenario: Deleting an object that is already gone reports the cluster's answer
    Given a detail panel is open for an object that was deleted elsewhere
    When the user deletes it and confirms
    Then a notice reads "Error: <the cluster's not-found message>"

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:78,252
  # source: pro:src/ui/resource-list.jsx:59,245
  Scenario: The confirm button waits for a write already in flight
    Given another write from this list has not been answered yet
    When a confirmation dialog is open
    Then its confirm button is disabled until that write is answered

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:98
  # source: pro:src/ui/resource-list.jsx:78-81
  Scenario: Navigating to another kind dismisses a pending confirmation
    Given a confirmation dialog is open
    When the user navigates to a different kind
    Then the dialog is gone and nothing was sent
