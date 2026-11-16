Feature: Namespace filter
  One namespace picker in the header narrows every namespaced screen. Its
  value lives in the URL.

  @oss @pro
  # source: oss:src/ui/shell.jsx:311-326
  # source: oss:src/app.jsx:498
  # source: pro:src/ui/shell.jsx:246-261
  # source: pro:src/app.jsx:521
  Scenario: The picker lists All namespaces and every namespace of the cluster
    Given cluster data has loaded
    When the user opens the namespace picker
    Then it lists "All namespaces" followed by the cluster's namespaces
    And the current choice is marked

  @oss @pro
  # source: oss:src/app.jsx:498
  # source: pro:src/app.jsx:521
  Scenario: Before data loads the picker offers only All namespaces
    Given no cluster data has loaded yet
    When the user opens the namespace picker
    Then it lists only "All namespaces"

  @oss @pro
  # source: oss:src/app.jsx:384-389
  # source: pro:src/app.jsx:356-361
  Scenario: Choosing a namespace writes it to the URL in place
    Given a screen URL with other parameters
    When the user chooses a namespace
    Then the URL gains "ns=<namespace>" and keeps its path and other parameters
    And no history entry is added
    And the picker's button shows the namespace

  @oss @pro
  # source: oss:src/app.jsx:386
  # source: pro:src/app.jsx:358
  Scenario: Choosing All namespaces removes the parameter
    Given a namespace filter is set
    When the user chooses "All namespaces"
    Then the "ns" parameter is removed from the URL
    And the picker's button shows "All namespaces"

  @oss @pro
  # source: oss:src/app.jsx:262
  # source: oss:src/ui/shell.jsx:314
  # source: pro:src/app.jsx:163
  # source: pro:src/ui/shell.jsx:249
  Scenario: A namespace named in the URL is used as given
    When the URL carries "ns=<a namespace the cluster does not list>"
    Then the picker's button shows that name and screens filter by it
    And no entry in the picker's list is marked as current

  @oss @pro
  # source: oss:src/app.jsx:375-382
  # source: pro:src/app.jsx:345-354
  Scenario: The filter follows the user between screens
    Given a namespace filter is set
    When the user navigates to another screen of the cluster
    Then the filter is still set

  @oss @pro
  # source: oss:src/app.jsx:484-491
  # source: pro:src/app.jsx:507-514
  Scenario: The filter is handed to every screen that lists namespaced things
    Given a namespace filter is set
    Then the events screen, the workloads overview, the tabbed screens and every generic resource list receive it
    And the overview, nodes and Helm releases screens do not

  @oss @pro
  # source: oss:src/ui/shell.jsx:38-59
  # source: pro:src/ui/shell.jsx:26-47
  Scenario: The picker closes on a choice, an outside click or Escape
    Given the namespace picker is open
    When the user chooses an entry, clicks outside it or presses Escape
    Then the picker closes
