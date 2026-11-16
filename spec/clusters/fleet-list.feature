Feature: Fleet list loading and refresh
  Where the cluster list comes from, when it is re-read, and what the
  dashboard does when the hub does not answer it.

  @pro @fleet
  # source: pro:src/app.jsx:90-107
  # source: pro:src/fleet.js:156-161
  Scenario: The cluster list is read once at start, after the session is known
    Given the hub requires sign-in and the user has a valid session
    When the dashboard starts
    Then it reads the hub configuration, then the session, then the cluster list
    And the application is shown only after the cluster list read has finished

  @pro @fleet
  # source: pro:src/app.jsx:96-100,133-136
  Scenario: Without a session the cluster list is not read
    Given the hub requires sign-in and the user has no session
    When the dashboard starts
    Then the sign-in screen is shown
    And the cluster list is not requested until the user has signed in

  @pro @fleet
  # source: pro:src/app.jsx:121-125
  Scenario: Signing in loads the cluster list before the application appears
    Given the sign-in screen is shown
    When the user signs in successfully
    Then the cluster list is read
    And the application is shown afterwards

  @drop @pro @fleet
  # decision: D9
  # source: pro:src/live.js:317-325
  # source: pro:src/app.jsx:270-299
  Scenario: Every refresh of the selected cluster re-reads the cluster list first
    Given a cluster is selected and auto-refresh is on
    When a periodic refresh runs
    Then the cluster list is requested before the selected cluster is probed
    And the list is requested even when the selected cluster turns out to be unreachable

  @fixed @pro @fleet
  # decision: D9
  # source: pro:src/live.js:317-325
  # source: pro:src/app.jsx:270-299
  Scenario: The cluster list refreshes on its own interval
    Given the Clusters page is open and auto-refresh is on
    When 15 seconds pass since the last cluster list read
    Then the cluster list is read again and the page shows the answer
    And this does not depend on any cluster being selected or reachable

  @pro @fleet
  # source: pro:src/poll.js:9
  # source: pro:src/app.jsx:270-299
  # source: pro:src/load-outcome.js:41-44
  Scenario: The periodic refresh runs every 15 seconds while healthy
    Given a cluster is selected and auto-refresh is on
    And the last refresh succeeded
    When 15 seconds pass
    Then the next refresh starts

  @pro @fleet
  # source: pro:src/app.jsx:273,522
  Scenario: Pausing auto-refresh stops the periodic re-read of the cluster list
    Given the Clusters page is open
    When the user pauses auto-refresh
    Then the cluster list is no longer re-read periodically
    And it is still re-read after a fleet action, a manual refresh or a cluster switch

  @pro @fleet
  # source: pro:src/app.jsx:384
  # source: pro:src/screens/clusters.jsx:133,146,156,606,814
  Scenario: A successful fleet action re-reads the cluster list at once
    Given the Clusters page is open
    When a kubeconfig registration, deploy, unregister, destroy or protection change succeeds
    Then the cluster list is re-read without waiting for the next periodic refresh

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/app.jsx:384
  # source: pro:src/live.js:289-310
  Scenario: A list re-read requested while one is in flight reuses the older request
    Given a cluster list request started before a fleet action has not answered yet
    When the action succeeds and asks for the list to be re-read
    Then no new request is sent
    And the list shown may not reflect the action until the next refresh

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/app.jsx:384
  # source: pro:src/live.js:289-310
  Scenario: The list read after a fleet action always starts after the action
    Given a cluster list request started before a fleet action has not answered yet
    When the action succeeds
    Then the list shown afterwards comes from a read that started after the action finished

  @pro @fleet
  # source: pro:src/live.js:288-310
  Scenario: Only one cluster list request is in flight at a time
    Given a cluster list request has not answered yet
    When another refresh asks for the cluster list
    Then no second request is sent
    And both callers receive the answer of the request already in flight

  @drop @pro @fleet
  # decision: D8
  # source: pro:src/live.js:288,295
  # source: pro:src/fleet.js:71-87
  Scenario: A cluster list request that takes longer than 10 seconds is abandoned
    Given the hub accepts the cluster list request but never answers
    When 10 seconds pass
    Then the request is cancelled and treated as a failed read

  @fixed @pro @fleet
  # decision: D8
  # source: pro:src/live.js:288,295
  # source: pro:src/fleet.js:71-87
  Scenario: A cluster list request that takes longer than 15 seconds is abandoned
    Given the hub accepts the cluster list request but never answers
    When 15 seconds pass
    Then the request is cancelled and treated as a failed read

  @drop @pro @fleet
  # decision: D8
  # source: pro:src/fleet.js:65-75,100-103
  # source: pro:src/screens/clusters.jsx:124,142,152,585,595,746,798
  Scenario: Fleet management requests have no deadline
    Given the hub accepts a register, deploy, unregister, destroy, protection or provider-list request but never answers
    When time passes
    Then the request stays pending and its dialog or control stays in its busy state for as long as the browser keeps the request open

  @fixed @pro @fleet
  # decision: D8
  # source: pro:src/fleet.js:65-75,100-103
  # source: pro:src/screens/clusters.jsx:124,142,152,585,595,746,798
  Scenario: Every fleet management request is given up after 15 seconds
    Given the hub accepts a register, deploy, unregister, destroy, protection or provider-list request but never answers
    When 15 seconds pass
    Then the request is cancelled and reported like any other failed request of that flow
    And the dialog or control leaves its busy state

  @fixed @pro @fleet
  # decision: D8
  # source: pro:src/ui/enginestate.jsx:156-166
  # source: pro:src/screens/clusters.jsx:744-758
  Scenario: Leaving the Clusters page cancels its pending reads
    Given reads started by the Clusters page or one of its dialogs have not answered
    When the user navigates to another screen
    Then those reads are cancelled

  @drop @pro @fleet
  # decision: D13
  # source: pro:src/live.js:295-306
  Scenario: A failed cluster list read keeps the list already shown
    Given the Clusters page shows a list read earlier
    When a later cluster list read fails
    Then the previously shown clusters stay on the page
    And no error is shown to the user

  @fixed @pro @fleet
  # decision: D13
  # source: pro:src/live.js:295-306
  Scenario: A failed cluster list read keeps the list and says it could not be refreshed
    Given the Clusters page shows a list read earlier
    When a later cluster list read fails
    Then the previously shown clusters stay on the page
    And the page states that the fleet could not be refreshed, with the hub's message

  @drop @pro @fleet
  # decision: D13
  # source: pro:src/live.js:297-306
  Scenario: An empty answer does not clear a list already shown
    Given the Clusters page shows a list read earlier
    When a later cluster list read answers successfully with no clusters
    Then the previously shown clusters stay on the page

  @fixed @pro @fleet
  # decision: D13
  # source: pro:src/live.js:297-306
  Scenario: An empty answer empties the list
    Given the Clusters page shows a list read earlier
    When a later cluster list read answers successfully with no clusters
    Then the page shows "No clusters registered yet." and no cluster

  @drop @pro @fleet
  # decision: D13
  # source: pro:src/live.js:297-305
  Scenario Outline: With nothing to show, a placeholder local cluster stands in for the list
    Given no cluster list has been shown yet
    When the first cluster list read <answer>
    Then the list holds exactly one cluster named "local"
    And it is shown as healthy, as the fleet manager, with provider "—", region "—" and 0/0 nodes

    Examples:
      | answer                         |
      | fails                          |
      | succeeds with no clusters      |

  @fixed @pro @fleet
  # decision: D13
  # source: pro:src/live.js:297-305
  Scenario: A fleet that cannot be read at start says so
    Given no cluster list has been shown yet
    When the first cluster list read fails
    Then the Clusters page states that the fleet could not be read, with the hub's message
    And no cluster is listed and none is invented
    And the read is retried on the refresh interval

  @pro @fleet
  # source: pro:src/fleet.js:159
  Scenario: A cluster list answer without a list of items counts as empty
    Given the hub answers the cluster list successfully with a body that carries no item list
    When the dashboard reads it
    Then it is treated as an answer with no clusters

  @pro @fleet
  # source: pro:src/fleet.js:89
  # source: pro:src/app.jsx:111-119
  # source: pro:src/auth-redirect.js:12-16
  Scenario: An expired session on a fleet request sends the user to sign-in
    Given the hub requires sign-in
    And the user is on a page other than the sign-in screen
    When any fleet request answers 401
    Then the sign-in screen is shown
    And the page the user was on, with its query string, is remembered for after sign-in

  @pro @fleet
  # source: pro:src/auth-redirect.js:12-13
  Scenario: A 401 does nothing when sign-in is not required
    Given the hub does not require sign-in
    When a fleet request answers 401
    Then the user stays on the current page

  @pro @fleet
  # decision: D5
  # source: pro:src/fleet.js:68
  Scenario: Every change request carries the session's anti-forgery token
    Given the user is signed in and the hub issued an anti-forgery token
    When the dashboard sends any fleet request that is not a read
    Then the request carries that token in the "X-Fleet-CSRF" header
    And read requests do not carry it

  @pro @fleet
  # source: pro:src/fleet.js:82-97
  Scenario Outline: A failed fleet request is reported with the most specific message available
    Given a fleet request fails with <failure>
    When the dashboard reports the failure to the user
    Then the message used is <message>

    Examples:
      | failure                                              | message                          |
      | a JSON error body with a message                     | the body's message               |
      | a JSON error body with only an error code            | the error code                   |
      | a non-JSON body                                      | the raw body text                |
      | an empty body and status 503                         | "HTTP 503"                       |
      | no response at all (network failure or timeout)      | the browser's own error message  |

  @drop @pro @fleet
  # decision: D6
  # source: pro:src/cluster-context.js:35-59
  Scenario Outline: An unusable configuration answer at start means standalone mode
    Given the dashboard is starting
    When the configuration request <outcome>
    Then the dashboard behaves as a standalone, single-cluster dashboard without sign-in
    And no fleet management controls are offered

    Examples:
      | outcome                                        |
      | fails at the network level                     |
      | answers with an error status                   |
      | answers with a mode other than fleet/standalone |
      | answers with a body that is not an object      |

  @fixed @pro @fleet
  # decision: D6
  # source: pro:src/cluster-context.js:35-59
  Scenario: An unusable configuration answer at start is an error screen
    Given the dashboard is starting
    When the configuration request fails or answers with something that is not a usable configuration
    Then an error screen is shown instead of the application
    And no screen is offered without sign-in
