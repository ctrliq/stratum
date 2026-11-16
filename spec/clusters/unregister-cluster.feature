Feature: Unregister a cluster
  An admin on a fleet hub removes a cluster's record from the fleet
  manager. The cluster keeps running; for an agent-registered cluster the
  hub returns a command that removes the agent.

  Background:
    Given the hub runs in fleet mode
    And the signed-in user is an admin
    And the Clusters page is open

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:234-235,273-274,392-399,412-414
  Scenario: Every cluster offers Unregister to an admin
    When the user looks at any cluster in the card or table view
    Then it offers an "Unregister" control with the tooltip "Unregister from the fleet manager"
    And this holds whether or not the hub reports the provisioning feature

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:395,412
  Scenario: The fleet manager's own cluster offers Unregister too
    Given the hub lists a cluster flagged as the fleet manager
    When the user looks at that cluster
    Then its "Unregister" control is shown and enabled

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:283-288
  Scenario: Unregister asks for confirmation first
    When the user clicks "Unregister" on the cluster "edge"
    Then a confirmation titled "Unregister edge?" opens
    And it reads "Removes edge from the fleet manager. The cluster keeps running — it just won't be visible or manageable from here. You can re-register it later."
    And it offers "Unregister" as a destructive action and "Cancel"
    And nothing has been sent to the hub

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:287
  Scenario: Cancelling the confirmation changes nothing
    Given the unregister confirmation for "edge" is open
    When the user clicks "Cancel"
    Then the confirmation closes and no request is sent

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:122-126,285
  Scenario: The confirmation shows progress while the hub is asked
    Given the unregister confirmation for "edge" is open
    When the user confirms and the hub has not answered
    Then the confirmation shows a loading state

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:122-134,329-341
  # source: pro:src/fleet.js:174
  Scenario: Unregistering an agent cluster shows the cleanup command
    Given the unregister confirmation for "edge" is open
    When the user confirms and the hub answers with a cleanup command
    Then the confirmation closes
    And a toast reads "edge removed from the fleet manager"
    And the cluster list is re-read
    And a dialog titled "edge unregistered" opens
    And it reads "The agent credential has been revoked. To remove the fleet agent from the cluster itself, run this against it:"
    And it shows the command under "Cleanup command" with a copy button
    And it offers "Done"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:330-331
  Scenario: Done closes the cleanup dialog
    Given the cleanup dialog for "edge" is open
    When the user clicks "Done" or closes the dialog
    Then the dialog closes and the command is no longer available

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:129-132
  Scenario Outline: No cleanup dialog when the hub returns no command
    Given the unregister confirmation for "edge" is open
    When the user confirms and the hub answers successfully with <answer>
    Then a toast reads "edge removed from the fleet manager"
    And the cluster list is re-read
    And no cleanup dialog opens

    Examples:
      | answer                       |
      | an empty cleanup command     |
      | no cleanup command field     |
      | no body at all               |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:127
  Scenario: The fleet manager's own cluster cannot be unregistered
    Given the unregister confirmation for the fleet manager's cluster is open
    When the user confirms and the hub refuses with the code "fleet-manager-cluster"
    Then the confirmation closes
    And a toast reads "The fleet manager cluster cannot be unregistered"
    And the cluster list is not re-read

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:126-127
  Scenario Outline: Any other refusal is shown as an error toast
    Given the unregister confirmation for "edge" is open
    When the user confirms and the hub refuses with <refusal>
    Then the confirmation closes
    And a toast reads "Error: " followed by the hub's message
    And the cluster stays in the list

    Examples:
      | refusal                              |
      | 403 because the role is insufficient |
      | 404 because the cluster is gone      |
      | 500 and a server message             |
      | no response at all                   |

  @pro @fleet
  # source: pro:src/fleet.js:89
  # source: pro:src/app.jsx:111-119
  Scenario: An expired session during unregister sends the user to sign-in
    Given the hub requires sign-in
    When the unregister request answers 401
    Then the sign-in screen is shown and the Clusters page is remembered for after sign-in

  @pro @fleet
  # source: pro:src/app.jsx:342
  Scenario: Toasts disappear on their own
    When any toast is shown
    Then it disappears after about 2.8 seconds
