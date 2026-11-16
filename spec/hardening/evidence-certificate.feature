Feature: Certificate evidence view
  A certificate finding's evidence is shown as a read-only panel: coverage per
  certificate role, then one block per certificate observation with its
  identity, expiry, sample age, renewal record and every degrading cause kept
  separate. Nothing in it can be acted on.

  # source: pro:src/ui/enginestate.jsx:512-532,549,561,570-575,836-837,932-936
  @pro
  Scenario: A certificate finding's evidence opens as the certificate panel
    Given the user opens the evidence of a finding whose signal is "certificate"
    Then the panel lists every certificate observation of the findings that were loaded for that report at the moment the evidence was opened, in the order they were served
    And findings loaded later do not change an open panel
    And the observation the finding was opened from shows the certificate fragment the evidence served
    And any further certificate fragments served are appended as blocks of their own without sample or coverage data
    And certificate fragments are not repeated as raw data
    And a collapsed "Full evidence" section offers the complete served evidence as raw data

  # source: pro:src/ui/enginestate.jsx:515-518,549,554-568
  @pro
  Scenario: Certificate fragments under any other finding stay raw
    Given the user opens the evidence of a finding whose signal is not "certificate"
    When the evidence served contains a certificate fragment
    Then the fragment is shown as a generic raw evidence item, not as the certificate panel

  # source: pro:src/evidence-certificate.jsx:248-256
  @pro
  Scenario: The panel is headed as a read-only record
    When the certificate panel is shown
    Then it is headed "Certificates" with the caption "the publisher's own certificate record — read-only"

  # source: pro:src/evidence-certificate.jsx:249,256
  # source: pro:src/ui/enginestate.jsx:549
  # decision: D3 — the panel is never shown with an empty list, so this message is unreachable
  @pro @drop
  Scenario: A panel with no observations says so
    Given the panel is given no certificate observation
    Then it reads "no certificate observations reported."

  # source: pro:src/ui/enginestate.jsx:517-532,549
  @pro
  Scenario: A certificate finding with no observation and no certificate fragment shows no certificate panel
    Given the user opens the evidence of a certificate finding
    And no certificate observation was loaded and the evidence served carries no certificate fragment
    Then no certificate panel is shown
    And only the evidence source, any other evidence items and "Full evidence" are shown

  # source: pro:src/evidence-certificate.jsx:99,159-180
  @pro
  Scenario: Coverage is summed per certificate role
    Given two observations for the role "kubelet-serving" report expected 2, evaluated 1, unknown 1, not applicable 0 and expected 1, evaluated 1, unknown 0, not applicable 3
    And one observation for the role "apiserver" reports expected 1, evaluated 1, unknown 0, not applicable 0
    Then a block for "kubelet-serving" reads "expected 3 · evaluated 2 · unknown 1 · not applicable 3"
    And a block for "apiserver" reads "expected 1 · evaluated 1 · unknown 0 · not applicable 0"
    And the blocks appear in the order each role first appears
    And each block's tooltip reads "expected = evaluated + unknown; not applicable is its own count, outside expected"

  # source: pro:src/evidence-certificate.jsx:99,162-169
  @pro
  Scenario Outline: Coverage tolerates incomplete observations
    Given an observation that <gap>
    Then <effect>

    Examples:
      | gap                                          | effect                                                    |
      | names no role                                | it contributes to no coverage block                       |
      | has no certificate evidence at all           | it contributes to no coverage block                       |
      | has a role but no coverage counts            | its role's block is shown and it adds zero to each count  |
      | has a count that is not a number             | that count adds zero                                      |

  # source: pro:src/evidence-certificate.jsx:190-219
  @pro
  Scenario: Each observation is shown as its own certificate block
    Given a certificate observation with evidence
    Then its block shows the observation's identifier, or "—" when it has none
    And "node <node identifier>" when the observation is about a node
    And the rows "certificate", "role", "fingerprint", "read state", "renewal owner", "availability" and "outcome" as the record states them
    And "availability" and "outcome" read "—" when the observation states none

  # source: pro:src/evidence-certificate.jsx:212-216
  @pro
  Scenario: An observation without certificate evidence shows dashes
    Given a certificate observation that carries no evidence fragment
    Then "certificate", "role", "fingerprint", "read state" and "renewal owner" each read "—"

  # source: pro:src/evidence-certificate.jsx:108,192-199,214
  @pro
  Scenario Outline: Facts a read learned are shown only when the read stood
    Given a certificate observation whose availability is <availability> and whose evidence carries <facts>
    Then "fingerprint" reads <fingerprint> and the expiry line is <expiry>

    Examples:
      | availability   | facts                               | fingerprint        | expiry    |
      | "available"    | a fingerprint and an expiry         | the fingerprint    | shown     |
      | not stated     | a fingerprint and an expiry         | the fingerprint    | shown     |
      | "available"    | no fingerprint and no expiry        | "unknown"          | not shown |
      | "forbidden"    | a fingerprint and an expiry         | "unknown"          | not shown |
      | "unavailable"  | a fingerprint and an expiry         | "unknown"          | not shown |

  # source: pro:src/evidence-certificate.jsx:32-38,90-97,220-226
  @pro
  Scenario Outline: The expiry line is banded by the exact time remaining
    Given a certificate whose read stood and whose expiry is <remaining> from now
    Then the expiry line reads <line> and is styled <style>

    Examples:
      | remaining                         | line                                                              | style     |
      | 1 second in the past              | "notAfter <expiry time> · expired"                                | critical  |
      | exactly now                       | "notAfter <expiry time> · expired"                                | critical  |
      | 1 second ahead                    | "notAfter <expiry time> · 0d 0h 0m 1s left · critical"            | critical  |
      | 6 days 23 hours 59 minutes 59 seconds ahead | "notAfter <expiry time> · 6d 23h 59m 59s left · critical" | critical  |
      | exactly 7 days ahead              | "notAfter <expiry time> · 7d 0h 0m 0s left · warning"             | warning   |
      | 20 days ahead                     | "notAfter <expiry time> · 20d 0h 0m 0s left · warning"            | warning   |
      | 29 days 23 hours 59 minutes 59 seconds ahead | "notAfter <expiry time> · 29d 23h 59m 59s left · warning" | warning  |
      | exactly 30 days ahead             | "notAfter <expiry time> · 30d 0h 0m 0s left · healthy"            | healthy   |
      | 400 days ahead                    | "notAfter <expiry time> · 400d 0h 0m 0s left · healthy"           | healthy   |

  # source: pro:src/evidence-certificate.jsx:49-53,223-224
  @pro
  Scenario: Times are shown in the browser's locale
    When the panel shows an expiry, sample or renewal time
    Then a parsable time is shown as a local date and time
    And an unparsable time is shown exactly as published
    And a missing time is shown as "—"

  # source: pro:src/evidence-certificate.jsx:90-93,194,220
  @pro
  Scenario Outline: No expiry line is invented
    Given a certificate observation whose expiry is <expiry>
    Then no expiry line is shown

    Examples:
      | expiry          |
      | not stated      |
      | not a valid time |

  # source: pro:src/evidence-certificate.jsx:54-72,196-197,227-229
  @pro
  Scenario Outline: Each block states how old its sample is
    Given a certificate observation sampled <sampled>
    Then the sample line reads "<line>"

    Examples:
      | sampled                    | line                                    |
      | 20 seconds ago             | sampled <sample time> · 20s ago         |
      | 59 minutes 59 seconds ago  | sampled <sample time> · 59m ago         |
      | 23 hours ago               | sampled <sample time> · 23h ago         |
      | 24 hours ago               | sampled <sample time> · 1d ago          |
      | 9 days ago                 | sampled <sample time> · 9d ago          |
      | in the future              | sampled <sample time> · no sample age   |
      | at an unparsable time      | sampled <the text as published> · no sample age |
      | at no stated time          | sampled — · no sample age               |

  # source: pro:src/evidence-certificate.jsx:195,230-240
  @pro
  Scenario: A renewal record is shown as recorded
    Given the certificate evidence carries a renewal record
    Then the block shows "renewal outcome" (or "—"), "renewal attempted", "renewal completed" and "renewal last success" times
    And "renewal operation" only when the record names an operation

  # source: pro:src/evidence-certificate.jsx:230,238-240
  @pro
  Scenario: A missing renewal record is unknown, never success
    Given the certificate evidence carries no renewal record
    Then the block reads "no renewal record — unknown"

  # source: pro:src/evidence-certificate.jsx:40-47,110-114,123-153
  @pro
  Scenario Outline: Every degrading cause is shown as its own marked line
    Given a certificate observation with read state <read state>, availability <availability> and renewal outcome <renewal>
    Then its block shows <causes>

    Examples:
      | read state    | availability   | renewal     | causes                                                                              |
      | "readable"    | "available"    | "success"   | no cause                                                                            |
      | "readable"    | "available"    | none        | no cause                                                                            |
      | "corrupt"     | "available"    | "success"   | the cause "read state corrupt"                                                      |
      | "readable"    | "forbidden"    | "success"   | the cause "availability forbidden"                                                  |
      | "unreadable"  | "unavailable"  | "success"   | the single cause "read state unreadable · availability unavailable"                 |
      | "readable"    | "available"    | "failed"    | the cause "renewal failed"                                                          |
      | "missing"     | "forbidden"    | "failed"    | two causes: "read state missing · availability forbidden" and "renewal failed"      |
      | a word this dashboard predates | "available" | "success" | a cause naming that word                                                 |

  # source: pro:src/evidence-certificate.jsx:110-114,146,150
  @pro
  Scenario Outline: A cause quotes the record's own reason
    Given a degrading cause whose record carries <reason>
    Then the cause line ends with <suffix>

    Examples:
      | reason                                                                       | suffix                                                                |
      | code "certificate-unreadable" and message "the store could not be read"      | " — certificate-unreadable: the store could not be read"              |
      | only the message "the store could not be read"                               | " — the store could not be read"                                      |
      | only the code "certificate-unreadable"                                       | " — certificate-unreadable"                                           |
      | no reason                                                                    | nothing                                                               |

  # source: pro:src/evidence-certificate.jsx:146,150
  @pro
  Scenario: Read and renewal causes quote different reasons
    Given an observation whose read is degraded and whose renewal failed
    Then the read cause quotes the observation's reason
    And the renewal cause quotes the renewal record's reason

  # source: pro:src/evidence-certificate.jsx:16-21,241
  @pro
  Scenario: Causes are never collapsed into a single severity
    Given a cluster with several certificate observations that each carry causes
    Then every cause of every observation remains visible in its own block
    And the panel shows no overall severity or worst-case badge

  # source: pro:src/evidence-certificate.jsx:27-29,248-261
  @pro
  Scenario: The certificate panel is read-only for every user
    When the certificate panel is shown to an admin or to a read-only user
    Then it offers no button, link or other control
    And it issues no request of its own
