Feature: Extension seam
  The open dashboard is published as a package. A downstream build can add
  its own screens and sidebar rows before the app starts, without changing
  the dashboard's source.

  Every scenario here is tagged @drop: the unified app imports its screens
  directly, so nothing registers at run time.

  # ---- adding a screen ----------------------------------------------------------------

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:149-158
  # source: oss:src/app.jsx:467,489-490
  Scenario: A registered screen answers its own route
    Given a downstream build registered a screen under an id before the app started
    When the browser opens "/<that id>"
    Then the registered screen is shown inside the shell

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/app.jsx:489-490
  Scenario: A registered screen is given the shell's context
    When a registered screen is shown
    Then it receives the cluster data and identity, the configuration, the signed-in user, the namespace filter and the "tab" and "name" parameters
    And it can navigate, raise shell actions, show a toast and ask for a refresh

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:157,161
  # source: oss:src/app.jsx:118-119,462
  Scenario: A registered screen's label is its breadcrumb title
    Given a screen was registered with a label
    When it is shown
    Then the last crumb reads that label
    And a screen registered without a label is titled with its id

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/app.jsx:471-491
  Scenario: A registered screen is consulted after built-in screens and before generic lists
    Then a registration can add a route but can never replace a built-in screen
    And it would be chosen ahead of a generic resource list with the same id if one were allowed

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/ui/palette.jsx:37-43
  Scenario: Registered screens do not appear in the command palette
    Given a downstream build registered a screen
    When the user opens the command palette
    Then the screen is not among its entries

  # ---- adding a sidebar row -----------------------------------------------------------

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:167-183,187-199
  Scenario: A registered row joins the section it names
    Given a downstream build registered a sidebar row for an existing section's label
    Then the row is shown at the end of that section

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:190-195
  Scenario: A row for an unknown section creates that section above Links
    Given a downstream build registered a sidebar row for a section label the sidebar does not have
    Then a new section with that label is shown just above "Links"
    And at the end of the sidebar when there is no "Links" section

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:167,190
  # source: oss:src/registry.js:30
  Scenario: A row registered without a section joins the top section
    Given a downstream build registered a sidebar row without naming a section
    Then the row is shown at the end of the unlabelled top section

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:163-166
  # source: oss:src/app.jsx:492
  Scenario: A registered row with no screen behind it leads to the not-built notice
    Given a sidebar row was registered with an id that names neither a registered screen nor a resource kind
    When the user clicks it
    Then the not-built notice is shown

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:169-173
  # source: oss:src/ui/shell.jsx:283,378-381
  Scenario: A registered link row behaves like the built-in link rows
    Given a sidebar row was registered as a named link
    Then it is shown only when the configuration carries a URL for that name
    And clicking it opens the URL in a new tab

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/ui/shell.jsx:245-251
  Scenario: A registered section collapses independently of a built-in one with the same name
    Given a registered section whose label equals a built-in section's id
    When the user collapses one of them
    Then the other stays expanded

  # ---- refusals -----------------------------------------------------------------------

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:150
  Scenario: A screen registration needs an id and something to render
    When a screen is registered without an id or without a way to render it
    Then the registration fails with "registerScreen: an id and a render(ctx) function are required"

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:112-127,151
  Scenario Outline: A screen id must be usable as one path segment
    When a screen is registered with <id>
    Then the registration fails saying the id is not a usable path segment

    Examples:
      | id                                        |
      | a value that is not text                  |
      | an id containing "/"                      |
      | an id containing a backslash              |
      | an id containing "?"                      |
      | an id containing "#"                      |
      | an id containing "%"                      |
      | an id containing whitespace               |
      | the id "."                                |
      | the id ".."                               |

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:124-126
  Scenario: A colon is allowed in an id
    When a screen or row is registered with an id containing ":"
    Then the registration is accepted

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:110,152
  Scenario: The dashboard's own ids cannot be registered as screens
    When a screen is registered under a built-in sidebar row's id, "crds", "login", "storage" or "rbac"
    Then the registration fails saying the id is one of the dashboard's own screens and cannot be replaced

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:139-144,152
  # source: oss:src/resources.jsx:269
  Scenario: Every resource kind's id is reserved too
    Given the app has loaded its resource kinds
    When a screen is registered under a resource kind's id
    Then the registration fails saying the id is one of the dashboard's own screens and cannot be replaced

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:139-144
  Scenario: A registration that took a resource kind's id first stops the app from loading
    Given a screen or row was registered under a resource kind's id before the app loaded its resource kinds
    When the app loads
    Then loading fails saying the id is one of the dashboard's own screens but a registration already took it

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:153
  Scenario: The same screen id cannot be registered twice
    When a second screen is registered under an id already registered
    Then the registration fails saying the id is already registered

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:168,172
  Scenario: A row registration needs a text id and a label
    When a sidebar row is registered without an id, without a label, or with an id that is not text
    Then the registration fails

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:173
  Scenario: A row's id must be a usable path segment unless it is a link row
    When a sidebar row that is not a link is registered with an id that is not a usable path segment
    Then the registration fails
    And the same id is accepted for a link row

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:178-180
  Scenario: A second row on one id, or a row on the dashboard's own id, is refused
    When a sidebar row is registered under an id that already has a registered row or is one of the dashboard's own ids
    Then the registration fails saying the id already has a sidebar row

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:174-177
  Scenario: One screen and one row on the same id is the normal pairing
    When a screen and a non-link sidebar row are registered under the same id, in either order
    Then both registrations are accepted and the row opens the screen

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:154-156,181
  Scenario: A link row and a screen cannot share an id
    When a link row and a screen are registered under the same id, in either order
    Then the later registration fails because the link would shadow the screen

  # ---- the package surface --------------------------------------------------------------

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/index.js:17-38
  Scenario: The package entry exposes registration, session and configuration helpers
    When a downstream build imports the package's main entry
    Then it can register screens and rows and read the merged sidebar
    And it can sign in, sign out, read the identity, ask whether writes are offered and call the server's own API
    And it can read the configuration, the Kubernetes proxy prefix and the node-shell feature settings
    And importing it loads no styles and renders nothing

  @oss @drop
  # decision: extension seam replaced by direct imports in the unified app
  # decision: D19
  # source: oss:src/registry.js:10-13
  # source: oss:src/ui/shell.jsx:283
  Scenario: Registrations are fixed once made
    Then there is no way to remove or reorder a registration
    And registrations made before the app's first draw are all present in it
