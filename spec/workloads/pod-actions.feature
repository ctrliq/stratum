Feature: Pod logs and shell from the Pods list
  Pods carry two contextual actions, Logs and Exec, on every row and in the
  detail panel. Both open the pod in the bottom drawer (the drawer itself is
  specified in spec/terminal).

  @oss @pro
  # source: oss:src/resources.jsx:32-36
  # source: oss:src/ui/resource-list.jsx:93,192-203,287-292
  # source: pro:src/resources.jsx:48-52
  # source: pro:src/ui/resource-list.jsx:74,180-191,280-285
  Scenario: Pods offer Logs and Exec on the row and in the detail panel
    Given the session is offered write controls
    When the Pods list is shown
    Then every row ends with a "Logs" and an "Exec" icon button
    And a pod's detail panel offers "Logs" and "Exec" buttons before its other actions

  @oss @pro
  # source: oss:src/resources.jsx:32-36
  # source: oss:src/ui/resource-list.jsx:88-93
  # source: pro:src/resources.jsx:48-52
  # source: pro:src/ui/resource-list.jsx:69-74
  Scenario: Without write controls a pod offers Logs but not Exec
    Given the session is not offered write controls
    When the Pods list is shown
    Then every row and the detail panel offer "Logs"
    And "Exec" is not offered anywhere on the list

  @oss @pro
  # source: oss:src/app.jsx:399-408,434
  # source: pro:src/app.jsx:387-396,424
  Scenario: Logs opens the pod's log view in the bottom drawer
    Given the Pods list is shown
    When the user presses "Logs" for a pod
    Then the bottom drawer opens a tab for that pod, named by its namespace and name, on the log view
    And the tab knows the pod's containers in the order its spec lists them
    And the tab becomes the active one

  @oss @pro
  # source: oss:src/app.jsx:399-408,435
  # source: pro:src/app.jsx:387-396,425
  Scenario: Exec opens the pod's shell view in the bottom drawer
    Given the Pods list is shown and the session is offered write controls
    When the user presses "Exec" for a pod
    Then the bottom drawer opens a tab for that pod on the shell view and makes it active

  @oss @pro
  # source: oss:src/app.jsx:404-407
  # source: pro:src/app.jsx:392-395
  Scenario: A pod has one drawer tab however often it is opened
    Given the bottom drawer already has a tab for a pod on the log view
    When the user presses "Exec" for the same pod
    Then no second tab is added
    And the existing tab switches to the shell view and becomes active

  @oss @pro
  # source: oss:src/app.jsx:400-401
  # source: pro:src/app.jsx:388-389
  Scenario: A pod object without a namespace or a name opens nothing
    When Logs or Exec is requested for an object lacking a namespace or a name
    Then the drawer does not change

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:196-197,288-289
  # source: pro:src/ui/resource-list.jsx:184-185,281-282
  Scenario: Opening logs or a shell leaves the list as it was
    Given a pod's detail panel is open
    When the user presses "Logs" or "Exec" on a row or in the panel
    Then the list, its search and the open detail panel are unchanged

  @oss @pro
  # source: oss:src/app.jsx:254-256,509-511
  # source: pro:src/app.jsx:155-157,532-534
  Scenario: Drawer tabs opened from the Pods list survive navigation
    Given a pod's logs are open in the bottom drawer
    When the user navigates to another screen on the same cluster
    Then the drawer and its tabs are still open

  @pro @fleet
  # source: pro:src/app.jsx:416-417
  Scenario: Switching cluster closes every pod tab
    Given pod tabs are open in the bottom drawer
    When the address changes to another cluster
    Then the drawer is emptied and closed
