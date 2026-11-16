Feature: Loading the server configuration
  GET "/api/v1/config" tells the dashboard what it is serving: the mode,
  whether sign-in is required, the Kubernetes proxy prefix, feature switches
  and external links.

  @oss @pro
  # source: oss:src/cluster-context.js:49-54,73-74
  # source: pro:src/cluster-context.js:35-39,57-58
  Scenario: The configuration is fetched once per page load
    When several parts of the dashboard ask for the configuration
    Then a single same-origin JSON request to "/api/v1/config" is made
    And every asker receives the same resolved configuration

  @oss @pro
  # source: oss:src/cluster-context.js:19-22,58-63
  # source: pro:src/cluster-context.js:20-23,43-48
  Scenario: Fields the server leaves out take defaults
    Given the server returns a valid configuration without some optional fields
    Then the Kubernetes proxy prefix defaults to "/k8s"
    And the version defaults to empty, the sign-in providers to none, and features and links to empty

  @diverged @oss @drop
  # decision: D1
  # source: oss:src/cluster-context.js:35-42,58
  Scenario: The open dashboard accepts only a well-formed standalone configuration
    When the server returns a configuration
    Then it is accepted only if its mode is "standalone" and it states whether sign-in is required as true or false
    And only if the proxy prefix, features and links, when present, have the expected types

  @diverged @pro @drop
  # decision: D1
  # source: pro:src/cluster-context.js:42
  Scenario: The fleet dashboard accepts fleet and standalone configurations
    When the server returns a configuration
    Then it is accepted if its mode is "fleet" or "standalone"
    And the other fields are not checked

  @oss @pro @fixed
  # decision: D1
  # decision: D4
  # source: oss:src/cluster-context.js:35-42,58
  # source: pro:src/cluster-context.js:42
  Scenario: A well-formed fleet or standalone configuration is accepted
    When the server returns a configuration
    Then it is accepted only if its mode is "fleet" or "standalone" and it states whether sign-in is required as true or false
    And only if the proxy prefix, features and links, when present, have the expected types

  # ---- configuration that cannot be used ----------------------------------------

  @diverged @oss
  # decision: D6
  # source: oss:src/cluster-context.js:52-57,64-67
  # source: oss:src/app.jsx:166,217,244-252
  Scenario: An unreachable configuration is a visible error in production
    Given a production build
    When GET "/api/v1/config" fails on the network or answers a non-success status
    Then the configuration resolves with an error naming the failure, such as "GET /api/v1/config -> HTTP 502"
    And the page shows that error instead of a dashboard

  @diverged @pro @drop
  # decision: D1
  # decision: D6
  # source: pro:src/cluster-context.js:50-53
  # source: pro:src/app.jsx:93-104
  Scenario: An unreachable configuration silently becomes login-free standalone mode
    When GET "/api/v1/config" fails on the network or answers a non-success status
    Then the dashboard assumes standalone mode with sign-in not required
    And it opens the shell without a sign-in screen and without an error
    And a warning is written to the browser console

  @diverged @oss
  # decision: D6
  # source: oss:src/cluster-context.js:58,64-67
  Scenario: An unsupported or malformed configuration is a visible error in production
    Given a production build
    When the server returns a configuration with another mode or a malformed shape
    Then the configuration resolves with the error "GET /api/v1/config returned an unsupported or malformed configuration"
    And the page shows that error instead of a dashboard

  @diverged @pro @drop
  # decision: D1
  # decision: D6
  # source: pro:src/cluster-context.js:42,50-53
  Scenario: An unrecognised configuration silently becomes login-free standalone mode
    When the server returns something that is not an object or has a mode other than "fleet" or "standalone"
    Then the dashboard assumes standalone mode with sign-in not required

  @oss
  # decision: D6
  # source: oss:src/cluster-context.js:56,65
  Scenario: A configuration answer that is not JSON is a visible error in production
    Given a production build
    When GET "/api/v1/config" answers success with a body that is not JSON
    Then the configuration resolves with an error carrying the parse failure

  @oss @drop
  # decision: D6
  # source: oss:src/cluster-context.js:30-33,68-71
  Scenario: Development mode falls back to the login-free default
    Given a development build, or an embedding page that declared development mode
    When the configuration cannot be fetched or is not valid
    Then the dashboard uses the default standalone configuration with sign-in not required
    And no configuration error is shown
    And a warning is written to the browser console

  @oss @fixed
  # decision: D6
  # source: oss:src/cluster-context.js:30-33,68-71
  Scenario: Only the development server falls back to the login-free default
    Given the app is served by the development server
    When the configuration cannot be fetched or is not valid
    Then the default standalone configuration with sign-in not required is used and a console warning is written
    And a built app never takes this fallback, whatever the embedding page declares

  @pro @drop
  # decision: D3
  # source: pro:src/cluster-context.js:49-53
  # assumption: no code in the fleet dashboard re-requests the configuration after boot, so this branch is not reachable today
  Scenario: A hub configuration already loaded is kept when a later reload fails
    Given the hub configuration was loaded successfully once
    When a forced reload of the configuration fails
    Then the previously loaded configuration stays in effect
    And the dashboard does not drop into login-free standalone mode

  @oss @drop
  # decision: D19
  # source: oss:src/cluster-context.js:49-50
  # source: oss:src/index.js:38
  Scenario: A forced configuration reload replaces the remembered configuration
    Given the configuration was already fetched
    When a consumer of the package asks for it again and forces a reload
    Then "/api/v1/config" is requested again and its answer replaces the remembered one

  # ---- what the configuration drives ---------------------------------------------

  @diverged @oss @drop
  # decision: D1
  # source: oss:src/cluster-context.js:81
  Scenario: Kubernetes calls use the configured proxy prefix
    Given the configuration names a Kubernetes proxy prefix
    When the dashboard calls the Kubernetes API
    Then the request path starts with that prefix, or with "/k8s" when none was given
    And the prefix is looked up for each request, not remembered from page load

  @diverged @pro
  # source: pro:src/cluster-context.js:78-84
  Scenario: Kubernetes calls are addressed per cluster in fleet mode
    Given the configuration mode is "fleet"
    When the dashboard calls the Kubernetes API of a cluster
    Then the request path starts with "/api/v1/clusters/<url-encoded cluster id>/k8s"
    And in standalone mode it starts with the configured prefix or "/k8s" whatever the cluster id

  @oss @pro
  # source: oss:src/ui/shell.jsx:279-283
  # source: pro:src/ui/shell.jsx:216-217
  Scenario: External links come from the configuration
    Given the configuration carries a URL for a named link
    Then the sidebar shows that link's row
    And rows whose link has no URL in the configuration are not shown
