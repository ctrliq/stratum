Feature: Deploy a new cluster
  When the hub reports the provisioning feature, an admin can ask the fleet
  manager to build a new cluster with one of the infrastructure providers
  the hub lists.

  Background:
    Given the hub runs in fleet mode and reports the provisioning feature
    And the signed-in user is an admin
    And the Clusters page is open

  # ---- the dialog and its providers -----------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:171,347-350,819-824
  Scenario: Deploy cluster opens the deploy dialog
    When the user clicks "Deploy cluster"
    Then a dialog titled "Deploy a new cluster" opens with the subtitle "Provision infrastructure with ClusterAPI"
    And it offers "Cancel" and a disabled "Deploy cluster"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:722-732
  Scenario: The form starts with sensible defaults
    When the user opens the deploy dialog
    Then "Kubernetes version" is "v1.34.0"
    And "Control plane" is 3 and "Worker nodes" is 3
    And the name, display name and placement values are empty

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:744-758,828-829
  # source: pro:src/fleet.js:190
  Scenario: The providers are asked from the hub each time the dialog opens
    When the user opens the deploy dialog
    Then the provider area reads "Loading…" until the hub answers the provider list

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:753-755,839-846
  Scenario: The listed providers are offered and the first is preselected
    Given the hub lists the providers "proxmox" and "vsphere" in that order
    When the provider list arrives
    Then both are offered as choices labelled with their ids
    And "proxmox" is selected

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:841-842
  Scenario: Exactly one provider is selected at a time
    Given the providers "proxmox" and "vsphere" are offered and "proxmox" is selected
    When the user clicks "vsphere"
    Then "vsphere" is selected and "proxmox" is not

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:753,834-837
  Scenario Outline: A hub with no providers says it cannot build clusters
    When the hub answers the provider list successfully with <answer>
    Then the provider area reads "This fleet manager has no infrastructure providers installed, so it cannot build clusters."
    And "Deploy cluster" stays disabled whatever else is filled in

    Examples:
      | answer              |
      | an empty list       |
      | no list at all      |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:748-752,830-833
  Scenario Outline: A failed provider read is reported as a failure, not as an empty list
    When the provider list request fails with <failure>
    Then the provider area reads "Could not list providers: <detail>" as an error
    And "Deploy cluster" stays disabled whatever else is filled in

    Examples:
      | failure                              | detail                        |
      | 403 and the message "forbidden"      | forbidden                     |
      | 500 and the message "internal error" | internal error                |
      | no response at all                   | the browser's error message   |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:745-757
  Scenario: A provider answer that arrives after the dialog closed is ignored
    Given the provider list has not answered
    When the user closes the dialog
    Then the late answer changes nothing

  # ---- field validation -------------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:61,783,785,850-854
  Scenario Outline: The cluster name must be a lowercase DNS label that is not in use
    Given the fleet already lists a cluster with name "prod" and id "prod"
    When the user types "<name>" as the cluster name
    Then the name hint reads "<hint>" shown as <style>

    Examples:
      | name       | hint                                               | style        |
      |            | DNS-1123 label; the id used in URLs                | neutral help |
      | staging-eu | DNS-1123 label; the id used in URLs                | neutral help |
      | Staging    | lowercase DNS-1123 label: a-z, 0-9, "-" (max 63)   | an error     |
      | -staging   | lowercase DNS-1123 label: a-z, 0-9, "-" (max 63)   | an error     |
      | prod       | already registered                                 | an error     |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:852-853
  Scenario: Spaces around the deploy cluster name are removed as the user types
    When the user types " staging-eu " as the cluster name
    Then the field holds "staging-eu"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:64,784,861-865
  Scenario Outline: The Kubernetes version must look like a v-prefixed semantic version
    When the user sets "Kubernetes version" to "<version>"
    Then the version is <verdict>
    And the version hint reads "<hint>"

    Examples:
      | version         | verdict  | hint                                |
      | v1.34.0         | accepted | the version ClusterAPI will build   |
      | v1.34.0-rc.1    | accepted | the version ClusterAPI will build   |
      | v10.2.33        | accepted | the version ClusterAPI will build   |
      | 1.34.0          | rejected | must look like v1.34.0              |
      | v1.34           | rejected | must look like v1.34.0              |
      | v1.34.0+build   | rejected | must look like v1.34.0              |
      | latest          | rejected | must look like v1.34.0              |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:784-785,861-862
  Scenario: An empty version blocks the deploy without an error hint
    When the user clears "Kubernetes version"
    Then the version hint still reads "the version ClusterAPI will build" as neutral help
    And "Deploy cluster" is disabled

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:784,791
  Scenario: Spaces around the version are ignored
    When the user sets "Kubernetes version" to " v1.34.0 "
    Then the version is accepted
    And the request carries the version "v1.34.0"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:866-874
  Scenario Outline: Node counts are picked on bounded sliders
    When the user moves the "<field>" slider
    Then it accepts whole numbers from <min> to <max>
    And the field label shows the chosen number

    Examples:
      | field         | min | max |
      | Control plane | 1   | 7   |
      | Worker nodes  | 0   | 100 |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:763-779,875-884
  Scenario Outline: Placement values are key=value lines
    When the user enters the placement values <text>
    Then the values are <verdict>
    And the request would carry <values>

    Examples:
      | text                                   | verdict  | values                               |
      | nothing                                | accepted | no values at all                     |
      | "bridge=vmbr0"                         | accepted | bridge → vmbr0                       |
      | "  bridge = vmbr0  "                   | accepted | bridge → vmbr0                       |
      | "a=1" and "b=2" on two lines           | accepted | a → 1 and b → 2                      |
      | "a=1", a blank line, "b=2"             | accepted | a → 1 and b → 2                      |
      | "# comment" and "a=1" on two lines     | accepted | a → 1                                |
      | "url=http://x/?q=1"                    | accepted | url → http://x/?q=1                  |
      | "a="                                   | accepted | a → an empty value                   |
      | "a=1" and "a=2" on two lines           | accepted | a → 2                                |
      | "bridge"                               | rejected | nothing, the form cannot be sent     |
      | "=vmbr0"                               | rejected | nothing, the form cannot be sent     |
      | " =vmbr0"                              | rejected | nothing, the form cannot be sent     |
      | "a=1" and "oops" on two lines          | rejected | nothing, the form cannot be sent     |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:875-879
  Scenario Outline: The placement hint follows the validity of the text
    When the placement values are <validity>
    Then the hint reads "<hint>"

    Examples:
      | validity | hint                                                                                                        |
      | valid    | one key=value per line; what the provider template needs (storage pool, bridge, image template…)             |
      | invalid  | each line must be key=value                                                                                  |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:785,823
  Scenario Outline: Deploy is enabled only when every part of the form is valid
    Given the form is otherwise valid
    When <condition>
    Then "Deploy cluster" is <state>

    Examples:
      | condition                               | state    |
      | nothing else is wrong                   | enabled  |
      | the name is empty                       | disabled |
      | the name is invalid                     | disabled |
      | the name is already registered          | disabled |
      | no provider is selected                 | disabled |
      | the version is rejected                 | disabled |
      | the placement values are rejected       | disabled |

  # ---- submitting ---------------------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:787-799
  # source: pro:src/fleet.js:183
  Scenario: Deploying sends the full specification
    Given the name "staging-eu", provider "proxmox", version "v1.34.0", 3 control plane nodes, 5 workers, display name "Staging EU" and the placement value "bridge=vmbr0"
    When the user clicks "Deploy cluster"
    Then the hub is asked to create a cluster with exactly that name, provider, Kubernetes version, control plane count, worker count, display name and placement values

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:794-795
  Scenario: Empty optional parts are left out of the request
    Given a valid form with a blank display name and no placement values
    When the user clicks "Deploy cluster"
    Then the request carries neither a display name nor placement values

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:788-789,823
  Scenario: The submit button shows progress and cannot be clicked twice
    Given a valid form
    When the user clicks "Deploy cluster" and the hub has not answered
    Then the button reads "Submitting…" and is disabled

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:813-815,885-888
  Scenario: An accepted deploy closes the dialog and refreshes the list
    When the hub accepts the request for "staging-eu"
    Then a toast reads "Provisioning staging-eu…"
    And the cluster list is re-read
    And the dialog closes

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:885-888
  Scenario: The dialog warns what a deploy does
    When the user opens the deploy dialog
    Then it states that the fleet manager creates real machines and will be able to destroy them
    And that the cluster joins the fleet on its own, with no agent installed

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:806-811,889
  # source: pro:src/fleet.js:383-384
  Scenario Outline: A hub without provisioning answers with a fixed message
    When the hub refuses the deploy with <refusal>
    Then the dialog shows "Provisioning via ClusterAPI is not enabled in this release" as an inline error
    And the dialog stays open with its values

    Examples:
      | refusal                           |
      | status 501                        |
      | the code "capi-not-enabled"       |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:806-811,889
  Scenario Outline: Any other refusal shows the hub's message inline
    When the hub refuses the deploy with <refusal>
    Then the dialog shows the hub's message as an inline error
    And no toast is shown and the cluster list is not re-read
    And the dialog stays open with its values

    Examples:
      | refusal                                             |
      | 400 naming a missing placement value                |
      | 409 because the name is taken on the hub            |
      | 403 because the role is insufficient                |
      | 500 and a server message                            |
      | no response at all                                  |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:789
  Scenario: Submitting again clears the previous inline error
    Given an inline error is shown from a refused attempt
    When the user submits the form again
    Then the previous error disappears while the new request is pending

  @pro @fleet
  # source: pro:src/fleet.js:89
  # source: pro:src/app.jsx:111-119
  Scenario: An expired session during deploy sends the user to sign-in
    Given the hub requires sign-in
    When the provider list or the deploy request answers 401
    Then the sign-in screen is shown and the Clusters page is remembered for after sign-in

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:822
  Scenario: Cancel closes the dialog without creating anything
    When the user fills the form and clicks "Cancel"
    Then the dialog closes and no create request is sent

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:347-350,787-816
  Scenario: Closing the deploy dialog while its request is pending does not cancel it
    Given a deploy request has been sent and has not answered
    When the user closes the dialog
    Then the request still completes on the hub
    And on success the "Provisioning <name>…" toast is still shown and the cluster list is re-read
    And a refusal is not shown anywhere
