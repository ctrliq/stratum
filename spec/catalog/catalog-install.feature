Feature: Installing a catalog application
  Installing creates one HelmChart in kube-system and hands over to the
  cluster's operator; everything shown afterwards is the operator's own
  status. The catalog offers no upgrade or reconfigure action.

  # source: pro:src/screens/catalog.jsx:672,946-951
  @pro
  Scenario Outline: Install in the detail panel is available only when the form is safe to submit
    Given an admin has the detail panel open for an application with no release
    And <condition>
    Then the panel's "Install" button is <state>

    Examples:
      | condition                                                    | state    |
      | every field is valid and the installed releases are known    | enabled  |
      | the release name is rejected                                 | disabled |
      | the target namespace is rejected                             | disabled |
      | the credentials Secret name is rejected                      | disabled |
      | a required parameter has no value                            | disabled |
      | another install or uninstall request is in progress          | disabled |
      | the installed releases are unknown                           | disabled |

  # source: pro:src/screens/catalog.jsx:316-320,947-950
  # source: pro:src/catalog.js:38
  @pro
  Scenario: Installing from the detail panel creates the HelmChart
    Given an admin has filled a valid install form
    When the admin presses "Install"
    Then the HelmChart built from the form is created in the "kube-system" namespace of the selected cluster
    And while the request is in progress the button reads "Installing…" with a spinner and every install and uninstall button is disabled

  # source: pro:src/screens/catalog.jsx:328,331
  @pro
  Scenario: A successful install is announced and then tracked from the cluster
    When the cluster accepts the new HelmChart
    Then a notification reads "Installing <application name> — <chart name> <chart version> → ns <target namespace>"
    And the installed releases are re-read immediately
    And once the release appears, the application shows "Installing" and its panel shows the release status instead of the form

  # source: pro:src/screens/catalog.jsx:329
  @pro
  Scenario: A release name that already exists is reported
    When the cluster answers that a HelmChart with that name already exists
    Then a notification reads "A HelmChart named "<release name>" already exists in kube-system"
    And the form keeps its values

  # source: pro:src/screens/catalog.jsx:330
  # source: pro:src/k8sproxy.js:111-133
  @pro
  Scenario Outline: Any other install failure is reported with the cluster's reason
    When the install request fails with <failure>
    Then a notification reads "Error: " followed by <detail>
    And the form keeps its values and the installed releases are re-read

    Examples:
      | failure                                   | detail                                   |
      | a forbidden answer                        | the cluster's error message              |
      | a validation rejection                    | the cluster's error message              |
      | a server error with a non-JSON body       | the response text                        |
      | a server error with an empty body         | "HTTP <status>"                          |
      | a redirect                                | "the write was redirected, and the dashboard does not follow redirects on writes" |
      | a network failure                         | the network error message                |

  # source: pro:src/screens/catalog.jsx:322-327
  # source: pro:src/catalog.js:359-376
  @pro
  Scenario Outline: An install whose values the cluster silently discarded is announced as such
    Given the created HelmChart returned by the cluster lacks <dropped>
    Then the notification reads "Installing <application name>, but this cluster's Stratum operator does not support <named>: ignored, so the chart's defaults apply; upgrade the cluster to engine v1.37.1-stratum.0 or later"
    And the normal success notification is not shown

    Examples:
      | dropped                                                   | named                                                                     |
      | the values Secret "my-creds" that was sent                | spec.valuesSecrets (Secret my-creds)                                      |
      | the text values "a" and "b" that were sent                | spec.setString (a, b)                                                     |
      | only the text value "b" out of "a" and "b"                | spec.setString (b)                                                        |
      | both the values Secret "my-creds" and the text value "a"  | spec.valuesSecrets (Secret my-creds) or spec.setString (a)                |

  # source: pro:src/catalog.js:363-376
  @pro
  Scenario: Discarded values are not reported when the cluster returns no created object
    Given the install request succeeds but the answer carries no HelmChart spec
    Then the normal success notification is shown

  # source: pro:src/screens/catalog.jsx:397-405,462-467,533
  @pro
  Scenario: Quick install is offered on applications without a release
    Given an admin is browsing and the installed releases are known
    Then every application without a release shows an "Install" button in its table row, its card and its essentials card
    And an essentials card adds "defaults · <the entry's install timeout, or 5m>"
    And applications with a release in any state show no quick "Install" button

  # source: pro:src/screens/catalog.jsx:334-339,400,464
  # source: pro:src/catalog-rules.js:44-54
  @pro
  Scenario: Quick install uses the catalog defaults without a form
    When an admin presses an application's quick "Install" button
    Then the application is selected and its detail panel opens
    And a HelmChart built from the application's seeded form values is created immediately, without confirmation
    And the outcome is announced exactly as for an install from the panel

  # source: pro:src/screens/catalog.jsx:334-339,672
  # decision: D2 — quick install sends values the panel would refuse (NOTES (a) 1)
  @pro @drop
  Scenario: Quick install does not apply the form's validation
    Given an application whose seeded values would leave the panel's Install disabled, such as an unsatisfied required parameter or an id that is not a valid release name
    When an admin presses its quick "Install" button
    Then the create request is sent anyway and the cluster's answer is reported

  # source: pro:src/screens/catalog.jsx:334-339,661-672
  # decision: D2
  @pro @fixed
  Scenario: Quick install applies the same checks as the install form
    Given an application whose seeded values would leave the panel's Install disabled
    When an admin presses its quick "Install" button
    Then the application's detail panel opens with the invalid or missing fields marked
    And no create request is sent until the form is valid and the admin presses "Install"

  # source: pro:src/screens/catalog.jsx:181,948-949
  # decision: D2 — the label claims an install while an uninstall is running (NOTES (a) 8)
  @pro @drop
  Scenario: The panel's Install button reads "Installing…" while any request, including an uninstall, is in progress
    Given an uninstall request for one application is in progress
    When an admin has the detail panel open for another application that has no release
    Then that panel's Install button is disabled and reads "Installing…" with a spinner

  # source: pro:src/screens/catalog.jsx:181,948-949
  # decision: D2
  @pro @fixed
  Scenario: The Install button reads "Installing…" only for its own install
    Given an install or uninstall request for another application is in progress
    When an admin has the detail panel open for an application that has no release
    Then that panel's Install button is disabled and still reads "Install"

  # source: pro:src/screens/catalog.jsx:320,349
  # source: pro:src/k8sproxy.js:111-121
  # decision: D8 — legacy writes have no deadline
  @pro @drop
  Scenario: An install or uninstall request waits for the cluster without limit
    When the cluster never answers an install or uninstall request
    Then every install and uninstall button stays disabled for as long as the request is pending

  # source: pro:src/screens/catalog.jsx:320,349
  # decision: D8
  @pro @fixed
  Scenario: An install or uninstall request is given up after 15 seconds
    When the cluster has not answered an install or uninstall request after 15 seconds
    Then the request is cancelled and a notification reports the failure
    And the buttons are enabled again and the installed releases are re-read
    And a redirect answered to the request is refused, not followed

  # source: pro:src/screens/catalog.jsx:399,463
  @pro
  Scenario: Quick install buttons are disabled while another request is in progress
    Given an install or uninstall request is in progress
    Then every quick "Install" button is disabled

  # source: pro:src/screens/catalog.jsx:316-317,397,462,946
  # decision: D14 — hub: admin only; see "Install and uninstall are offered according to the backend's write gating" in catalog-detail.feature
  @pro
  Scenario: A read-only user cannot install
    Given the signed-in user does not have the admin role
    Then no "Install" button is shown in the table, the cards, the essentials strip or the detail panel
    And no create request can be issued from the catalog

  # source: pro:src/catalog-rules.js:21-23,32-35
  # source: pro:src/screens/catalog.jsx:741,809,928-951
  # assumption: recorded from the absence of any upgrade, reconfigure or edit control in the catalog code
  @pro
  Scenario: The catalog offers no upgrade or reconfiguration of an installed application
    Given an application has a release
    Then its panel offers only "Uninstall" (or its removing variants) and no control to change its version or values
    And a release the operator is upgrading by other means is shown as "Installing"
    And the catalog does not compare the installed chart version with the index's version
