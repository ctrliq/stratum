Feature: Register an existing cluster
  An admin on a fleet hub adds a running cluster to the fleet, either by
  minting a one-time registration token for the fleet agent, or (when the
  hub supports it) by handing the hub a kubeconfig.

  Background:
    Given the hub runs in fleet mode
    And the signed-in user is an admin
    And the Clusters page is open

  # ---- the dialog -----------------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:170,343-346,618-631
  Scenario: Register existing opens the registration dialog in agent mode
    When the user clicks "Register existing"
    Then a dialog titled "Register an existing cluster" opens
    And its subtitle reads "Mint a one-time registration token for the fleet agent"
    And it offers "Cancel" and a disabled "Create token"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:94,649-664
  Scenario Outline: The registration method choice depends on the hub's kubeconfig feature
    Given the hub configuration reports kubeconfig registration as <feature>
    When the user opens the registration dialog
    Then the "Agent" / "Kubeconfig" method choice is <shown>

    Examples:
      | feature  | shown     |
      | enabled  | shown     |
      | disabled | not shown |
      | absent   | not shown |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:658-662
  Scenario Outline: Each registration method explains how the fleet manager reaches the cluster
    Given the method choice is shown
    When the user selects "<method>"
    Then the explanation reads "<explanation>"

    Examples:
      | method     | explanation                                                                                                                                  |
      | Agent      | The cluster runs an agent that dials out to the fleet manager. Preferred: nothing needs to reach the cluster from here.                      |
      | Kubeconfig | For clusters that cannot run an agent. The fleet manager connects to the cluster itself, so it must be able to reach its API server.         |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:561-567,698-712
  Scenario: Agent mode defaults to the full profile and a one hour token
    When the user opens the registration dialog
    Then "Agent profile" is "full" with the hint "the dashboard can read and write"
    And "Token lifetime" is "1 hour"
    And "Display name" is empty and marked optional

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:700-704
  Scenario: The read-only profile is explained
    When the user sets "Agent profile" to "read-only"
    Then the hint reads "read-only RBAC on the member"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:583,701-704
  Scenario: The read-only profile is sent as "readonly"
    Given the user set "Agent profile" to "read-only"
    When the user creates the token
    Then the request names the profile "readonly"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:65,706-710
  Scenario Outline: The token lifetime is one of three fixed choices
    When the user sets "Token lifetime" to "<choice>" and creates the token
    Then the request asks for a lifetime of <seconds> seconds

    Examples:
      | choice   | seconds |
      | 1 hour   | 3600    |
      | 4 hours  | 14400   |
      | 24 hours | 86400   |

  # ---- name validation -------------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:61,570-572,665-669
  Scenario Outline: The cluster name must be a lowercase DNS label that is not in use
    Given the fleet already lists a cluster with name "prod" and id "prod"
    When the user types "<name>" as the cluster name
    Then the name hint reads "<hint>"
    And the submit button is <submit>

    Examples:
      | name                                                             | hint                                                 | submit   |
      |                                                                  | the id used in URLs; immutable                       | disabled |
      | staging                                                          | the id used in URLs; immutable                       | enabled  |
      | a                                                                | the id used in URLs; immutable                       | enabled  |
      | staging-2                                                        | the id used in URLs; immutable                       | enabled  |
      | Staging                                                          | lowercase DNS-1123 label: a-z, 0-9, "-" (max 63)     | disabled |
      | -staging                                                         | lowercase DNS-1123 label: a-z, 0-9, "-" (max 63)     | disabled |
      | staging-                                                         | lowercase DNS-1123 label: a-z, 0-9, "-" (max 63)     | disabled |
      | stag_ing                                                         | lowercase DNS-1123 label: a-z, 0-9, "-" (max 63)     | disabled |
      | stag.ing                                                         | lowercase DNS-1123 label: a-z, 0-9, "-" (max 63)     | disabled |
      | aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa | lowercase DNS-1123 label: a-z, 0-9, "-" (max 63)     | disabled |
      | prod                                                             | already registered                                   | disabled |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:665-666
  Scenario: An invalid or taken name is flagged as an error; an empty name is not
    When the cluster name is empty
    Then the name hint is shown as neutral help
    When the cluster name is invalid or already registered
    Then the name hint is shown as an error

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:570
  Scenario: A name equal to an existing cluster's id is taken even if its name differs
    Given the fleet lists a cluster with id "c-1" and name "prod"
    When the user types "c-1" as the cluster name
    Then the name hint reads "already registered"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:667-668
  Scenario: Spaces around the cluster name are removed as the user types
    When the user types " staging " as the cluster name
    Then the field holds "staging"

  # ---- agent mode: minting the token -------------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:582-589,610-615
  # source: pro:src/fleet.js:378
  Scenario: Creating a token sends the name, profile and lifetime
    Given the user entered the name "staging", the profile "full" and the lifetime "1 hour"
    And the display name is empty
    When the user clicks "Create token"
    Then the hub is asked for a registration token for cluster "staging" with profile "full" and a lifetime of 3600 seconds
    And the request carries no display name

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:584
  Scenario: A display name is sent trimmed when given
    Given the user entered the display name "  Staging EU  "
    When the user clicks "Create token"
    Then the request carries the display name "Staging EU"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:610-615,628-630
  Scenario: The submit button shows progress and cannot be clicked twice
    Given a valid agent registration form
    When the user clicks "Create token" and the hub has not answered
    Then the button reads "Creating…" and is disabled

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:587-588,619-625,632-646
  Scenario: A minted token is shown once as the command to run on the member cluster
    Given the hub answers the token request with a token name, an apply command, a manifest URL and an expiry
    When the answer arrives
    Then a toast reads "Registration token <token name> created"
    And the dialog subtitle changes to "Run this on the cluster you are registering"
    And the apply command is shown under "Apply on the member cluster" with a copy button
    And the dialog states that the token is embedded in the command, is shown once, is stored hashed on the hub and cannot be retrieved again
    And the dialog states that the agent will appear on the page under the entered cluster name once it connects
    And "manifest: <manifest URL>" is shown
    And the only button left is "Done"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:68,637
  Scenario Outline: The token expiry is shown in the browser's local date-time format
    Given the hub answered the token request with the expiry "<expiresAt>"
    When the result is shown
    Then the dialog says the token expires "<shown>"

    Examples:
      | expiresAt            | shown                                           |
      | 2026-11-13T12:00:00Z | that instant in the browser's locale and zone   |
      | not-a-date           | not-a-date                                      |
      |                      | —                                               |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:639-644
  Scenario: The result explains the executor opt-in
    When a minted token is shown
    Then the dialog explains that nodes running executors for fleet operations need "&executor=true" appended to the manifest URL
    And that this is an opt-in on the fetch, not on the token

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:645
  Scenario: The manifest line is omitted when the hub returns no manifest URL
    Given the hub answered the token request without a manifest URL
    When the result is shown
    Then no "manifest:" line is shown

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:634
  Scenario: A missing apply command leaves the command box empty
    Given the hub answered the token request without an apply command
    When the result is shown
    Then the command box is shown empty

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:71-88
  Scenario: Copying the command confirms briefly
    Given a minted token is shown
    When the user clicks the copy button
    Then the command is placed on the clipboard
    And the copy button shows a check mark for about 1.6 seconds

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:73-76
  Scenario: A blocked clipboard fails silently
    Given the browser refuses clipboard access
    When the user clicks the copy button
    Then nothing changes and no error is shown
    And the command text can still be selected by hand

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:343-346,569,625
  Scenario: Closing the result discards the token for good
    Given a minted token is shown
    When the user clicks "Done" or closes the dialog
    Then the dialog closes
    And reopening "Register existing" shows an empty form, not the token

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:582-589
  Scenario: Minting a token does not add a cluster to the list
    Given a token was minted for "staging"
    When the dialog is closed
    Then the cluster list is not re-read because of the token
    And "staging" appears only after its agent has connected and a later list refresh returns it

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:586,713
  Scenario: A name reserved on the hub is explained inline
    When the hub refuses the token request with the code "name-taken"
    Then the dialog shows 'The name "staging" is already reserved by a cluster or an outstanding token.' for the entered name "staging"
    And the form stays open with its values

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:586,713
  Scenario Outline: Any other refusal shows the hub's message inline
    When the hub refuses the token request with <refusal>
    Then the dialog shows the hub's message as an inline error
    And no toast is shown
    And the form stays open with its values

    Examples:
      | refusal                              |
      | 400 and a validation message         |
      | 403 because the role is insufficient |
      | 500 and a server message             |
      | no response at all                   |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:612
  Scenario: Submitting again clears the previous inline error
    Given an inline error is shown from a refused attempt
    When the user submits the form again
    Then the previous error disappears while the new request is pending

  @pro @fleet
  # source: pro:src/fleet.js:89
  # source: pro:src/app.jsx:111-119
  Scenario: An expired session during registration sends the user to sign-in
    Given the hub requires sign-in
    When the token request answers 401
    Then the sign-in screen is shown and the Clusters page is remembered for after sign-in

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:627
  Scenario: Cancel closes the dialog without contacting the hub
    When the user fills the form and clicks "Cancel"
    Then the dialog closes and no request is sent

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:343-346,582-615
  Scenario: Closing the registration dialog while its request is pending does not cancel it
    Given a token or kubeconfig registration request has been sent and has not answered
    When the user closes the dialog
    Then the request still completes on the hub
    And on success its toast is still shown, and for a kubeconfig registration the cluster list is re-read
    And a token minted this way is never shown

  # ---- kubeconfig mode ---------------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:622-623,629,674-697
  Scenario: Kubeconfig mode asks for the file, an optional context and warns about the credential
    Given the method choice is shown
    When the user selects "Kubeconfig"
    Then the subtitle reads "Give the fleet manager a kubeconfig it can use to connect out"
    And the form shows a file picker and a text area for the kubeconfig
    And an optional "Context" field with the hint "optional — defaults to the file's current-context"
    And a notice that the fleet manager will hold the credential, that the dashboard cannot read it back, and that credential plugins, file paths and "insecure-skip-tls-verify" are refused
    And the agent profile and token lifetime fields are hidden
    And the submit button reads "Register cluster"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:572
  Scenario Outline: Kubeconfig registration needs a valid name and a non-blank kubeconfig
    Given kubeconfig mode is selected
    When the name is <name> and the kubeconfig text is <kubeconfig>
    Then "Register cluster" is <submit>

    Examples:
      | name    | kubeconfig        | submit   |
      | valid   | present           | enabled  |
      | valid   | empty             | disabled |
      | valid   | only white space  | disabled |
      | invalid | present           | disabled |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:575-580,678-679
  Scenario: Picking a file fills the kubeconfig text
    Given kubeconfig mode is selected
    When the user picks a kubeconfig file
    Then its content appears in the kubeconfig text area
    And nothing is sent to the hub yet

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:578-579
  Scenario: An unreadable file is reported inline
    Given kubeconfig mode is selected
    When the browser cannot read the picked file
    Then the dialog shows "Could not read that file."

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:591-595
  # source: pro:src/fleet.js:187
  Scenario: Registering by kubeconfig sends the name, the file and the optional fields
    Given kubeconfig mode with name "edge", a kubeconfig, display name "Edge" and context "prod-ctx"
    When the user clicks "Register cluster"
    Then the hub is asked to register "edge" with that kubeconfig, display name "Edge" and context "prod-ctx"
    And the button reads "Connecting…" until the hub answers

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:593-594
  Scenario: Blank optional kubeconfig fields are left out of the request
    Given kubeconfig mode with a blank display name and a blank context
    When the user clicks "Register cluster"
    Then the request carries neither a display name nor a context

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:605-607
  Scenario: A successful kubeconfig registration closes the dialog and refreshes the list
    When the hub accepts the kubeconfig registration of "edge"
    Then a toast reads "edge registered"
    And the cluster list is re-read
    And the dialog closes

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:596-604
  Scenario Outline: A refused kubeconfig registration is explained inline
    When the hub refuses the kubeconfig registration of "edge" with the code "<code>" and the message "<message>"
    Then the dialog shows "<shown>"
    And the dialog stays open with its values

    Examples:
      | code               | message                | shown                                                               |
      | name-taken         | already exists         | The name "edge" is already taken.                                    |
      | member-unreachable | dial tcp: i/o timeout  | The fleet manager could not reach that cluster: dial tcp: i/o timeout |
      | bad-request        | exec plugins refused   | exec plugins refused                                                 |
      | insufficient-role  | admin role required    | admin role required                                                  |

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:654-656
  Scenario: Switching method clears the error and keeps what was typed
    Given an inline error is shown and a name was typed
    When the user switches between "Agent" and "Kubeconfig"
    Then the error disappears
    And the name, display name and kubeconfig text are kept
