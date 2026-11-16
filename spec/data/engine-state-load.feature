Feature: Loading the hub's engine-state evidence with each cluster load
  The fleet dashboard reads the hub's verdict on a cluster's engine (a summary,
  the version findings of one report generation and the evidence behind them)
  alongside every cluster load. This feature records only how that read is
  sequenced, bounded and guarded by the loader; how the result is displayed
  belongs to the clusters domain.

  @pro
  # source: pro:src/live.js:349-352,385-392
  Scenario: Engine-state evidence is requested before the cluster is probed
    When a cluster load starts
    Then the hub's engine-state summary for the cluster is requested without waiting for the liveness probe

  @pro
  # source: pro:src/live.js:386-391,400
  Scenario: Engine-state evidence is still published when the cluster itself is unreachable
    Given the hub can answer for the cluster's engine state
    When the cluster's liveness probe fails
    Then the engine-state evidence the hub returned is shown
    And the rest of the snapshot keeps its last good data

  @pro
  # source: pro:src/live.js:386-391,458,467
  Scenario: Engine-state evidence is published on every early stop of a load
    Given the hub can answer for the cluster's engine state
    When a load stops because every read failed as an outage, or because a known hub gave no facts
    Then the engine-state evidence the hub returned is shown

  @pro
  # source: pro:src/live.js:1030-1043
  Scenario: A standalone hub's engine state is addressed to the hub's own cluster record
    Given the hub runs in standalone mode and marks the record "site-a" as its own cluster
    When the engine state is requested
    Then it is requested for "site-a"
    And for "local" when no marked record is available

  @pro
  # source: pro:src/enginestate.js:193-197
  # source: pro:src/live.js:361-372
  # source: pro:src/live.js:895-921
  Scenario: Each engine-state request is same-origin, asks for JSON and is abandoned after 10 seconds
    When an engine-state page is requested
    Then the request includes the session cookie and the header "Accept: application/json"
    And it is abandoned if the hub has not answered within 10 seconds

  @pro
  # source: pro:src/live.js:876-879,1020-1028
  Scenario: One engine-state load is abandoned as a whole after 15 seconds
    Given the hub answers each page slowly
    When 15 seconds have passed since the engine-state load began
    Then every remaining request of that load is cancelled
    And the load ends as unavailable

  @pro
  # source: pro:src/live.js:1063-1078
  Scenario: A refused summary makes the engine state unavailable with a stated reason
    When the summary request is refused, fails, or answers something that is not an object
    Then the engine state is reported as unavailable with the reason "summary-unavailable"
    And no findings are requested

  @pro
  # source: pro:src/live.js:883-893,939,964,1089-1102
  Scenario Outline: A refused findings or evidence page has its own reason
    When a findings or evidence page is refused with <refusal>
    Then the refusal is classified as "<reason>"

    Examples:
      | refusal                                  | reason              |
      | status 409                               | generation-changed  |
      | the error code "generation-changed"      | generation-changed  |
      | status 410                               | detail-expired      |
      | the error code "detail-expired"          | detail-expired      |
      | status 500                               | detail-unavailable  |
      | status 403                               | detail-unavailable  |
      | a network error                          | detail-unavailable  |

  @pro
  # source: pro:src/live.js:1099-1102
  Scenario: A detail-expired or detail-unavailable refusal makes the engine state unavailable
    When a findings or evidence page is refused as "detail-expired" or "detail-unavailable"
    Then the engine state is reported as unavailable with that reason
    And no row from that load is shown

  @pro
  # source: pro:src/live.js:940-942,965
  Scenario: A page of the wrong shape makes the engine state unavailable
    When a findings page answers 200 without a findings list and an observations list
    Then the engine state is reported as unavailable with the reason "detail-unavailable"

  @pro
  # source: pro:src/live.js:1079-1098
  Scenario: A report generation that moved mid-read is re-read once
    Given the summary named report generation "41"
    When a findings page for generation "41" is refused as "generation-changed"
    Then the summary is read again
    And the findings are read from the start under the generation the new summary names

  @pro
  # source: pro:src/live.js:1085-1102
  Scenario: A report generation that moves twice ends the load
    Given the findings were already restarted once under a newer generation
    When a page is refused as "generation-changed" again
    Then the engine state is reported as unavailable with the reason "generation-changed"
    And no row from either generation is shown

  @pro
  # source: pro:src/live.js:923-953
  Scenario: Findings are read page by page, following the hub's cursor, for the version signal only
    Given the hub answers the first findings page with a next cursor "c2"
    When the next page is requested
    Then the request asks for the "version" signal with the cursor "c2"
    And reading stops when a page carries no next cursor

  @pro
  # source: pro:src/live.js:880,934,952
  Scenario: At most 100 findings pages are read
    Given the hub keeps answering each findings page with a new cursor
    When 100 pages have been read
    Then reading stops
    And the result is marked as truncated

  @pro
  # source: pro:src/live.js:932,948-950
  Scenario: A cursor the hub already issued ends the read as truncated
    Given the hub answers with the cursors "a", "b" and then "a" again
    When the repeated cursor is seen
    Then reading stops and the result is marked as truncated

  @pro
  # source: pro:src/live.js:945,1017
  Scenario: A page that says its snapshot is incomplete marks the result as partial
    When any findings page states that it is not complete
    Then the result is marked as partial, separately from truncation

  @pro
  # source: pro:src/live.js:976-1011
  Scenario: Evidence delivered with the findings is not requested again
    Given a finding's observation arrived on a findings page with its evidence inline
    Then no separate evidence request is made for that finding
    And an observation that carries no evidence yields an empty evidence list

  @pro
  # source: pro:src/live.js:955-974,1012-1015
  Scenario: Evidence with no matching observation is read from the hub, with the same bounds
    Given a finding references evidence that no delivered observation matches
    When its evidence is read
    Then it is requested for the same cluster and report generation, page by page
    And at most 100 pages are read, and a repeated cursor ends the read as truncated

  @pro
  # source: pro:src/live.js:988-998
  Scenario: An observation repeated across pages is kept once
    Given the same observation arrives on two consecutive findings pages
    Then it appears once in the result, as first delivered

  @pro
  # source: pro:src/live.js:211-222,1045-1059
  Scenario: An unavailable engine state leaves one console note per cluster and reason
    When the engine state is unavailable for the same cluster and reason on ten consecutive loads
    Then the evidence read writes one console warning for that cluster and reason, with the HTTP status when known
    And the per-poll refresh writes one console warning for that cluster, with the refusal's message, whatever the reason
    And a later, different reason is warned about again by the evidence read but not by the per-poll refresh

  @pro
  # source: pro:src/live.js:186-272,401-408,851-856
  Scenario: An engine-state refusal never turns a reachable cluster into an unreachable one
    Given the cluster answered its probe and its reads
    When the hub refuses the engine-state summary
    Then the cluster load still commits and reports the cluster as reachable

  @pro
  # source: pro:src/live.js:401-408
  Scenario: The hub's engine-state refresh runs only for a reachable cluster
    When the liveness probe finds the cluster unreachable
    Then the per-poll refresh of the engine-state summary and findings is not started
    And it is subject to the same poll backoff as the cluster reads

  @pro
  # source: pro:src/live.js:211,243-266
  Scenario: The per-poll engine-state refresh re-pins a moved generation at most twice
    Given the findings read is refused as "generation-changed"
    When the summary has been re-read and the findings retried two more times, each refused the same way
    Then the refresh stops for this poll and publishes the refusal
    And the next poll starts again from the summary

  @pro
  # source: pro:src/live.js:223-231,267-271
  Scenario: A per-poll engine-state refresh that exceeds 15 seconds is published as a timed-out refusal
    When the per-poll refresh has not finished 15 seconds after it began
    Then its result is published as a retryable network refusal with the message "the engine-state refresh timed out"
    And an unexpected error in the refresh is written to the console and changes nothing shown

  @pro
  # source: pro:src/live.js:251-252
  Scenario: A summary without a report generation is shown without findings
    When the per-poll refresh reads a summary that names no report generation
    Then the summary is published on its own
    And no findings are requested

  @pro
  # source: pro:src/live.js:353-384
  Scenario: The two engine-state reads of one load share one summary request
    When both the evidence read and the per-poll refresh need the summary during the same load
    Then a single summary request is made and both use its answer
    And one of them giving up on its own time budget does not cancel the request for the other

  @pro @fleet
  # source: pro:src/live.js:352,388-389,846-847
  # source: pro:src/data.js:39-45
  Scenario: Engine state from a cluster the user left is never shown
    Given an engine-state read for cluster "a" is in flight
    When the user switches to cluster "b"
    Then the result for "a" is discarded
    And the engine state shown is empty until cluster "b" answers
