Feature: Requests to the server's own API and anti-forgery protection
  The dashboard talks to its server under "/api/v1" with the session cookie.
  Requests that change state carry an anti-forgery token header; failures are
  reported as results, never as exceptions.

  # ---- anti-forgery header ---------------------------------------------------

  @diverged @oss
  # decision: D5
  # source: oss:src/session.js:188
  # source: oss:src/k8sutil.js:75-76
  Scenario: Open dashboard writes carry the X-Stratum-CSRF header
    Given an anti-forgery token is known
    When the dashboard sends a non-GET request to the server's own API or through the Kubernetes proxy
    Then the request carries the token in the "X-Stratum-CSRF" header

  @diverged @pro
  # decision: D5
  # source: pro:src/fleet.js:68
  # source: pro:src/k8sproxy.js:114-115
  Scenario: Fleet dashboard writes carry the X-Fleet-CSRF header
    Given an anti-forgery token is known
    When the dashboard sends a non-GET request to the server's own API or through the Kubernetes proxy
    Then the request carries the token in the "X-Fleet-CSRF" header

  @oss @pro @fixed
  # decision: D5
  # decision: D4
  # source: oss:src/session.js:188
  # source: pro:src/fleet.js:68
  Scenario: The anti-forgery header name follows the backend
    Given an anti-forgery token is known
    When the dashboard sends a non-GET request
    Then the token travels in "X-Stratum-CSRF" when the configuration describes the dashboard server
    And in "X-Fleet-CSRF" when it describes the fleet hub

  @oss @pro
  # source: oss:src/session.js:188
  # source: pro:src/fleet.js:68
  Scenario: GET requests carry no anti-forgery header
    Given an anti-forgery token is known
    When the dashboard sends a GET request
    Then the request carries no anti-forgery header

  @oss @pro
  # source: oss:src/session.js:188
  # source: pro:src/fleet.js:68
  Scenario: A write without a known token is sent without the header
    Given no anti-forgery token is known
    When the dashboard sends a non-GET request
    Then the request is still sent, without an anti-forgery header

  @oss @pro
  # source: oss:src/session.js:326
  # source: pro:src/fleet.js:141
  Scenario: The token is taken from the sign-in answer
    When a sign-in answer carries a "csrfToken"
    Then later writes carry that token

  @oss @pro
  # source: oss:src/session.js:289,304
  # source: pro:src/fleet.js:111
  Scenario: The token is refreshed from every successful identity answer
    When an identity answer carries a "csrfToken"
    Then later writes carry that token

  @oss
  # source: oss:src/session.js:112
  Scenario: The token is withheld while the session is changing
    Given a sign-in or sign-out is in progress in any tab, or the shared session marker cannot be read
    When the dashboard sends a write
    Then the write carries no anti-forgery header

  # ---- request and result contract -------------------------------------------

  @oss @pro
  # source: oss:src/session.js:186-191
  # source: pro:src/fleet.js:66-79
  Scenario: Requests are same-origin JSON with the session cookie
    When the dashboard calls the server's own API
    Then the request asks for JSON and sends same-origin credentials
    And a request with a body declares a JSON content type

  @oss @pro
  # source: oss:src/session.js:210
  # source: pro:src/fleet.js:101,103
  Scenario: A write without a body sends an empty JSON object
    When the dashboard posts to the server's own API without a body
    Then the request body is "{}"

  @oss @pro
  # source: oss:src/session.js:190-195
  # source: pro:src/fleet.js:77-84
  Scenario: A request that gets no HTTP answer is reported as a network failure
    When a request fails before any HTTP response
    Then the caller receives a failure with status 0 and code "network" carrying the browser's message
    And no exception is raised

  @oss @pro
  # source: oss:src/session.js:197-205
  # source: pro:src/fleet.js:88-96
  Scenario Outline: An error answer is reported with a code and a message
    When the server answers with status <status> and <body>
    Then the caller receives a failure with code "<code>" and message "<message>"

    Examples:
      | status | body                                              | code         | message      |
      | 403    | JSON {"error":"forbidden","message":"Not allowed"} | forbidden    | Not allowed  |
      | 403    | JSON {"error":"forbidden"}                         | forbidden    | forbidden    |
      | 502    | the plain text "Bad Gateway"                      | http_502     | Bad Gateway  |
      | 500    | an empty body                                     | http_500     | HTTP 500     |

  @oss @pro
  # source: oss:src/session.js:197,206
  # source: pro:src/fleet.js:88,97
  Scenario: A success answer without JSON is still a success
    When the server answers 2xx with an empty or non-JSON body
    Then the caller receives a success whose body is empty

  @pro
  # source: pro:src/fleet.js:69-87
  Scenario: A request can be bounded by a deadline
    Given a caller set a deadline on a request
    When the server has not answered by the deadline
    Then the request is abandoned and reported as a network failure

  @pro
  # source: pro:src/fleet.js:71-74,78,81-83
  Scenario: A request can be cancelled by its caller
    Given a caller cancels a request, before or while it is in flight
    Then the request is reported as a network failure and its answer is not used

  @oss
  # source: oss:src/session.js:39,236-237,258-259
  Scenario: Sign-in and sign-out requests are bounded
    When a sign-in or sign-out request has not been answered within 15 seconds
    Then it is abandoned and reported as a network failure
