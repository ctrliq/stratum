Feature: Destroy a provisioned cluster and protect it from destroy
  When the hub reports the provisioning feature, an admin can tear down a
  cluster the fleet manager built, after typing its identifier, and can
  switch a per-cluster protection that blocks the destroy.

  Background:
    Given the hub runs in fleet mode
    And the signed-in user is an admin
    And the Clusters page is open

  # ---- availability ---------------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:392-401
  Scenario: Without the provisioning feature there is no destroy or protection control
    Given the hub configuration does not report the provisioning feature
    When the user looks at any cluster
    Then it shows no destroy control and no protection control

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:402-409,421-425
  Scenario Outline: The destroy control explains why it is or is not available
    Given the hub reports the provisioning feature
    And a cluster that is <fleetManager> the fleet manager, <provisioned> by the fleet manager and <protected> from destroy
    When the user hovers its destroy control
    Then the control is <state>
    And the tooltip reads "<tooltip>"

    Examples:
      | fleetManager | provisioned | protected     | state    | tooltip                                                                        |
      | not          | built       | not protected | enabled  | Destroy cluster (ClusterAPI)                                                   |
      | not          | built       | protected     | disabled | Cannot destroy — deletion protection is on                                     |
      | not          | not built   | not protected | disabled | Cannot destroy — this cluster was registered, not built by the fleet manager   |
      | not          | not built   | protected     | disabled | Cannot destroy — this cluster was registered, not built by the fleet manager   |
      | indeed       | built       | not protected | disabled | Cannot destroy — this cluster runs the fleet manager                           |
      | indeed       | not built   | not protected | disabled | Cannot destroy — this cluster runs the fleet manager                           |
      | indeed       | built       | protected     | disabled | Cannot destroy — this cluster runs the fleet manager                           |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:415-420
  Scenario Outline: The protection control is shown only for clusters the fleet manager built
    Given the hub reports the provisioning feature
    And a cluster that was <origin>
    When the user looks at it
    Then the protection control is <shown>

    Examples:
      | origin                         | shown     |
      | built by the fleet manager     | shown     |
      | registered, not built          | not shown |

  # ---- protection -------------------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:416-418
  Scenario Outline: The protection control states what a click will do
    Given a built cluster that is <state>
    When the user hovers its protection control
    Then the tooltip reads "<tooltip>"
    And the control is <emphasis>

    Examples:
      | state         | tooltip                   | emphasis        |
      | protected     | Remove destroy protection | highlighted     |
      | not protected | Protect from destroy      | not highlighted |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:151-157
  # source: pro:src/fleet.js:181-182
  Scenario Outline: Clicking the protection control flips it at once, without confirmation
    Given the built cluster "lab" is <before>
    When the user clicks its protection control
    Then the hub is asked to set the protection of "lab" to <asked>
    And when the hub confirms the protection is <asked>, a toast reads "<toast>"
    And the cluster list is re-read

    Examples:
      | before        | asked | toast                                   |
      | not protected | on    | lab is now protected from destroy       |
      | protected     | off   | Removed destroy protection from lab     |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:153-154
  Scenario: The toast reports the state the hub answered, not the one asked for
    Given the built cluster "lab" is not protected
    When the user clicks its protection control and the hub answers that the protection is off
    Then a toast reads "Removed destroy protection from lab"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:151-157,416
  Scenario: Repeated clicks on the protection control each send a request
    Given a protection change has been sent and has not answered
    When the user clicks the protection control again
    Then the control is still enabled and another request is sent, asking for the same change as the first

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/screens/clusters.jsx:153-154
  Scenario: A protection answer without a body reports nothing
    When the hub accepts a protection change but answers without a body
    Then no toast is shown and the cluster list is not re-read

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/screens/clusters.jsx:153-156
  Scenario: A protection answer without a body is still reported
    When the hub accepts a protection change but answers without a body
    Then a toast reports the change that was asked for
    And the cluster list is re-read

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:153-156
  # source: pro:src/fleet.js:383-384
  Scenario Outline: A refused protection change is reported in a toast
    When the hub refuses the protection change with <refusal>
    Then a toast reads "<toast>"
    And the cluster list is not re-read

    Examples:
      | refusal                              | toast                                                        |
      | status 501                           | Provisioning via ClusterAPI is not enabled in this release   |
      | the code "capi-not-enabled"          | Provisioning via ClusterAPI is not enabled in this release   |
      | 403 with the message "admin only"    | Error: admin only                                            |
      | 500 with the message "boom"          | Error: boom                                                  |

  # ---- destroy ------------------------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:235,274,295-326
  Scenario: Destroy opens a typed confirmation
    Given the built, unprotected cluster with name "Lab" and id "lab", provider "proxmox" and 4 nodes
    When the user clicks its destroy control
    Then a dialog titled "Destroy Lab?" opens
    And it states that this deletes "Lab" and every machine ClusterAPI built for it, with no undo, and that the cluster's workloads go with it
    And it shows "Provider: proxmox · Nodes: 4"
    And it states that the record stays visible in "Deleting" until its machines are gone, and that Unregister removes the record without touching the infrastructure
    And it asks to "Type lab to confirm" in an empty field
    And it offers "Cancel" and a disabled "Destroy this cluster"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:311-313
  Scenario: The confirmation notes a cluster that is still being built
    Given a built cluster with status "provisioning"
    When the destroy dialog opens
    Then the facts line adds "(still being built — machines may not exist yet)"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:311-312
  Scenario: Missing provider and node count are shown as placeholders
    Given a built cluster that reports no provider and no node count
    When the destroy dialog opens
    Then it shows "Provider: — · Nodes: 0"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:300,319-322
  Scenario Outline: Destroy is enabled only when the typed text equals the cluster's id
    Given the destroy dialog for the cluster with name "Lab" and id "lab" is open
    When the user types "<typed>"
    Then "Destroy this cluster" is <state>

    Examples:
      | typed   | state    |
      |         | disabled |
      | la      | disabled |
      | Lab     | disabled |
      | LAB     | disabled |
      | lab     | enabled  |
      | " lab " | enabled  |
      | lab2    | disabled |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:135-150
  # source: pro:src/fleet.js:179-180
  Scenario: Destroying sends what the user typed as the confirmation
    Given the destroy dialog for the cluster "lab" is open and the user typed "lab"
    When the user clicks "Destroy this cluster"
    Then the hub is asked to destroy "lab" with the typed text, trimmed, as the confirmation
    And the button reads "Destroying…" and is disabled until the hub answers

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:142-146
  Scenario: An accepted destroy closes the dialog and refreshes the list
    When the hub accepts the destroy of the cluster named "Lab"
    Then the dialog closes and the typed text is cleared
    And a toast reads "Destroying Lab…"
    And the cluster list is re-read

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:142-146
  # source: pro:src/fleet.js:383-384
  Scenario Outline: A refused destroy closes the dialog and reports in a toast
    When the hub refuses the destroy with <refusal>
    Then the dialog closes and the typed text is cleared
    And a toast reads "<toast>"
    And the cluster list is not re-read

    Examples:
      | refusal                                  | toast                                                        |
      | status 501                               | Provisioning via ClusterAPI is not enabled in this release   |
      | the code "capi-not-enabled"              | Provisioning via ClusterAPI is not enabled in this release   |
      | 400 with the message "confirm mismatch"  | Error: confirm mismatch                                      |
      | 403 with the message "admin only"        | Error: admin only                                            |
      | 409 with the message "protected"         | Error: protected                                             |
      | no response at all                       | Error: followed by the browser's error message               |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:296,299
  Scenario: Cancelling the destroy dialog forgets the typed text
    Given the destroy dialog is open and the user typed part of the id
    When the user clicks "Cancel" or closes the dialog
    Then the dialog closes and no request is sent
    And reopening the dialog shows an empty confirmation field

  @drop @pro @fleet
  # decision: D3
  # source: pro:src/screens/clusters.jsx:136
  # assumption: not reachable through the page today, because the destroy control is disabled for such clusters; the confirm step repeats the check
  Scenario: A destroy is never sent for a cluster flagged as protected or as the fleet manager
    Given a destroy confirmation exists for a cluster that was flagged as protected or as the fleet manager when it was opened
    When the user confirms
    Then no request is sent
