Feature: Restarting a workload
  Workloads that own a pod template can be given a rolling restart from
  their detail panel.

  @oss @pro
  # source: oss:src/resources.jsx:241,256
  # source: oss:src/ui/resource-list.jsx:294
  # source: pro:src/resources.jsx:258,273
  # source: pro:src/ui/resource-list.jsx:287
  Scenario Outline: Restart is offered for rolling workloads only
    Given the session is offered write controls
    When a detail panel is open for a <kind>
    Then the footer <offer> "Restart"

    Examples:
      | kind                  | offer          |
      | Deployment            | offers         |
      | StatefulSet           | offers         |
      | DaemonSet             | offers         |
      | ReplicaSet            | does not offer |
      | ReplicationController | does not offer |
      | Pod                   | does not offer |
      | Job                   | does not offer |
      | CronJob               | does not offer |

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:146,246-259
  # source: pro:src/ui/resource-list.jsx:133,239-252
  Scenario: Restart asks for confirmation
    Given a detail panel is open for Deployment "api"
    When the user presses "Restart"
    Then the detail panel closes
    And a dialog titled "Restart Deployment?" says "Triggers a rolling restart of api — all its pods are recreated."
    And the dialog offers "Cancel" and "Restart"

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:247,251
  # source: pro:src/ui/resource-list.jsx:240,244
  Scenario: Cancelling the confirmation restarts nothing
    Given the restart confirmation is open
    When the user presses "Cancel" or dismisses the dialog
    Then nothing is sent to the cluster and the detail panel stays closed

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:132,128,253
  # source: pro:src/ui/resource-list.jsx:119,115,246
  Scenario: Confirming stamps the pod template so the workload rolls
    Given the restart confirmation is open for workload "api"
    When the user presses "Restart" and the cluster accepts
    Then the workload's pod template is annotated "kubectl.kubernetes.io/restartedAt" with the current time
    And nothing else in the workload is changed
    And a notice reads "Rolling restart of api triggered"
    And the list is re-read immediately

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:128,132
  # source: pro:src/ui/resource-list.jsx:115,119
  Scenario: A refused restart is reported
    Given the restart confirmation is open
    When the user presses "Restart" and the cluster rejects or forbids it
    Then a notice reads "Error: <the cluster's message>"

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:132
  # source: pro:src/ui/resource-list.jsx:119
  # assumption: the dashboard only reports that the restart was accepted; it does not follow the rollout, inferred from the absence of any follow-up read beyond the list refresh
  Scenario: The dashboard does not track the rollout it triggered
    Given a restart was accepted
    Then no progress indicator is shown beyond the list's own columns refreshing
