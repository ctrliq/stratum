Feature: Backup evidence view
  A backup finding's evidence is shown as a read-only panel: a protection
  verdict, the backup policy as published, the completeness of the storage
  listing, warnings, each distinct backup with its copies and attempts, and
  the prune attempt. Nothing in it can be acted on.

  # source: pro:src/ui/enginestate.jsx:512-516,533-535,550-553,562,570-575
  @pro
  Scenario: A backup finding's evidence opens as backup panels
    Given the user opens the evidence of a finding whose signal is "backup"
    When the evidence served contains backup fragments
    Then one backup panel is shown per backup fragment
    And those fragments are not repeated as raw data
    And a collapsed "Full evidence" section offers the complete served evidence as raw data

  # source: pro:src/ui/enginestate.jsx:533-535,554-568
  @pro
  Scenario: Backup fragments under any other finding stay raw
    Given the user opens the evidence of a finding whose signal is not "backup"
    When the evidence served contains a backup fragment
    Then the fragment is shown as a generic raw evidence item, not as a backup panel

  # source: pro:src/ui/enginestate.jsx:551-552
  # source: pro:src/evidence-backup.jsx:306-322
  @pro
  Scenario Outline: The verdict shown is the report's published outcome
    Given the finding's observation publishes the outcome <published>
    Then the panel's verdict word is "<word>"

    Examples:
      | published        | word          |
      | "satisfactory"   | satisfactory  |
      | "partial"        | partial       |
      | "critical"       | critical      |
      | "disabled"       | disabled      |
      | "unknown"        | uncertain     |
      | nothing          | uncertain     |

  # source: pro:src/evidence-backup.jsx:309
  # source: pro:src/ui/enginestate.jsx:552
  @pro
  Scenario: A published outcome the dashboard does not know is shown verbatim
    Given the finding's observation publishes an outcome word outside the known ones, such as "unconfigured" or a word a newer hub adds
    Then the panel's verdict word is that word exactly as published
    And only "unknown" is ever renamed, to "uncertain"

  # source: pro:src/evidence-backup.jsx:308-310,320-321
  @pro
  Scenario: The verdict is explained only when the evidence itself supports the same word
    Given the panel's own reading of the fragment yields a verdict word and an explanation
    When the published verdict word equals the panel's own
    Then the explanation follows the verdict word
    But when the two words differ, only the published word is shown, without explanation

  # source: pro:src/ui/enginestate.jsx:552
  # source: pro:src/evidence-backup.jsx:319
  @pro
  Scenario: The verdict is styled by the finding's severity
    Given the finding carries a severity
    Then the verdict line is styled for that severity
    But a finding without a severity styles it as unknown

  # source: pro:src/evidence-backup.jsx:173-250,306-310
  # source: pro:src/ui/enginestate.jsx:551-552
  @pro
  Scenario Outline: The explanation is the panel's own reading of the fragment
    Given a backup fragment whose own reading is that <condition>
    Then the panel's own verdict is "<word>"
    And when the published verdict word equals it, the explanation reads "<explanation>"

    Examples:
      | condition                                                                                   | word         | explanation                                                                                     |
      | the policy state is "unconfigured"                                                          | unconfigured | no backup policy is configured for this engine                                                  |
      | the policy state is "disabled"                                                              | disabled     | the backup policy is switched off at its source; its backups still render below                 |
      | the policy is configured but not enabled                                                    | disabled     | the backup policy is switched off at its source; its backups still render below                 |
      | the fragment has no policy                                                                  | uncertain    | the effective backup policy is unknown, so the evidence supports no verdict of its own          |
      | the policy state is any word other than "configured", "unconfigured" or "disabled" and the policy is not explicitly switched off | uncertain    | the effective backup policy is unknown, so the evidence supports no verdict of its own          |
      | the policy does not say whether it is enabled                                               | uncertain    | the effective backup policy is unknown, so the evidence supports no verdict of its own          |
      | the policy has no schedule                                                                  | uncertain    | the schedule or its state is unknown, so nothing is known about when backups are due             |
      | the schedule's state is neither "enabled" nor "disabled"                                    | uncertain    | the schedule or its state is unknown, so nothing is known about when backups are due             |
      | the policy states no copy age bound                                                         | uncertain    | the copy age bound is unknown, so copy freshness cannot be judged                               |
      | the fragment has no listing or the listing is not complete                                  | uncertain    | the storage listing is incomplete, so the copies the storage holds are uncertain                 |
      | every required location holds a successful copy within the age bound and no attempt failed   | satisfactory | every required location holds a copy within the configured age                                  |

  # source: pro:src/evidence-backup.jsx:178-202
  @pro
  Scenario: The panel's own reading asks its questions in a fixed order
    Given a fragment that is unconfigured, has no schedule and has an incomplete listing at once
    Then the panel's own verdict, and so the explanation it can offer, is the first that applies in this order: unconfigured, disabled, unknown policy, unknown schedule, unknown age bound, incomplete listing, then coverage
    And a policy that is explicitly not enabled reads "disabled" whatever its state word, unless that word is "unconfigured"

  # source: pro:src/evidence-backup.jsx:204-248
  @pro
  Scenario Outline: Coverage problems decide between critical and partial
    Given a configured, enabled policy with a known schedule and age bound and a complete listing
    And <situation>
    Then the panel's own verdict is "<word>"
    And when the published verdict word equals it, the explanation contains "<part>"

    Examples:
      | situation                                                                                         | word     | part                                                                              |
      | required location "s3-east" has no copy with a valid success                                      | critical | no copy covers required location s3-east                                          |
      | the freshest success at "s3-east" is 3 days old against an 86400-second bound                     | critical | the freshest copy at s3-east is 3d old, past the configured 24h bound             |
      | all locations are covered and fresh but the upload of copy "c2" at "s3-east" of backup "b7" failed | partial  | the upload attempt for copy c2 at s3-east of b7 failed                            |
      | all locations are covered and fresh but the save of copy "c1" at "local" of backup "b7" failed     | partial  | the save attempt for copy c1 at local of b7 failed                                |

  # source: pro:src/evidence-backup.jsx:240-247
  @pro
  Scenario: Every coverage problem is listed
    Given several required locations are missing or stale and several attempts failed
    Then the explanation, when shown, lists every missing location, then every stale location, then every failed attempt, separated by "; "
    And the panel's own verdict is "critical" as soon as any location is missing or stale

  # source: pro:src/evidence-backup.jsx:62-81,212-218,236-237
  @pro
  Scenario Outline: What counts as a copy's success, failure and age
    Given <fact>
    Then <consequence>

    Examples:
      | fact                                                                                   | consequence                                                             |
      | a remote copy                                                                          | its success time is its upload's last success                           |
      | a local copy                                                                           | its success time is its save's last success                             |
      | a copy of any other kind                                                               | it has no success time and covers no location                           |
      | an attempt that completed with an outcome other than success                           | the attempt failed                                                      |
      | an attempt that completed as success but has no last success                           | the attempt failed                                                      |
      | an attempt that has not completed                                                      | the attempt did not fail                                                |
      | a last success stamped more than 30 seconds in the future                              | it covers no location                                                   |
      | a last success stamped 10 seconds in the future                                        | it covers its location with an age of zero                              |
      | a freshest success exactly as old as the age bound                                     | the location is not stale                                               |
      | a location listed twice among the required locations                                   | it is judged and reported once                                          |

  # source: pro:src/evidence-backup.jsx:228,249
  # assumption: follows from the loop over required locations; an empty list finds nothing missing
  @pro
  Scenario: A policy that requires no location is satisfied by an empty set of backups
    Given a configured, enabled policy with a known schedule and age bound, a complete listing, no required location and no failed attempt
    Then the panel's own verdict is "satisfactory", so a published "satisfactory" is explained with "every required location holds a copy within the configured age"

  # source: pro:src/evidence-backup.jsx:306-310,319
  # source: pro:src/ui/enginestate.jsx:551-552
  # decision: D3 — the panel's "no published outcome" default has no caller; the drawer always supplies an outcome
  @pro @drop
  Scenario: A panel given no published outcome would show its own verdict
    Given a backup panel shown with no published outcome at all
    Then its verdict word would be the panel's own reading, with "partial" styled as a warning

  # source: pro:src/evidence-backup.jsx:256-285,324-326
  @pro
  Scenario Outline: The policy is printed as published, with unknowns named
    Given the fragment's policy <policy>
    Then the policy block includes the line "<line>"

    Examples:
      | policy                                                        | line                                        |
      | is absent                                                     | policy unknown                              |
      | has state "configured"                                        | state configured                            |
      | has no state                                                  | state unknown                               |
      | has revision 12                                               | revision 12                                 |
      | has no revision                                               | revision unknown                            |
      | has provenance "engine config"                                | provenance engine config                    |
      | has no provenance                                             | provenance unknown                          |
      | is enabled                                                    | enabled                                     |
      | is not enabled                                                | disabled                                    |
      | does not say whether it is enabled                            | enabled unknown                             |
      | requires locations "local" and "s3-east"                      | required locations local, s3-east           |
      | requires no location                                          | required locations none                     |
      | has schedule "0 2 * * *" in time zone "UTC"                   | schedule 0 2 * * * (UTC)                    |
      | has schedule "0 2 * * *" and no time zone                     | schedule 0 2 * * *                          |
      | has no schedule or no schedule expression                     | schedule unknown                            |
      | retains 604800 seconds and 14 distinct backups                | retention keep 7d · 14 distinct backups     |
      | retains 14 distinct backups only                              | retention 14 distinct backups               |
      | has a retention with neither bound                            | retention unknown                           |
      | has no retention                                              | retention unknown                           |
      | has a copy age bound of 86400 seconds                         | copy age bound 24h                          |
      | has no copy age bound                                         | copy age bound unknown                      |

  # source: pro:src/evidence-backup.jsx:257-259
  @pro
  Scenario: An absent policy prints a single line
    Given the fragment has no policy
    Then the policy block reads only "policy unknown"

  # source: pro:src/evidence-backup.jsx:49-60
  @pro
  Scenario Outline: Durations are shown floored in one coarse unit
    Given a duration of <seconds> seconds
    Then it reads "<text>"

    Examples:
      | seconds | text |
      | 0       | 0s   |
      | 59      | 59s  |
      | 60      | 1m   |
      | 240     | 4m   |
      | 3599    | 59m  |
      | 14640   | 4h   |
      | 172799  | 47h  |
      | 172800  | 2d   |
      | 604800  | 7d   |

  # source: pro:src/evidence-backup.jsx:313-315,328-337
  @pro
  Scenario Outline: The listing line states how complete the storage listing was
    Given the fragment's listing <listing>
    Then the listing line reads "<line>"

    Examples:
      | listing                                                                     | line                                                                    |
      | is complete with 3 of 3 distinct backups                                    | listing complete — 3 of 3 distinct backups served                       |
      | is not complete with 2 of 5 distinct backups and the reason "page limit"    | listing partial — 2 of 5 distinct backups served · page limit           |
      | is not complete and states no counts                                        | listing partial — distinct backup counts unknown                        |
      | does not say whether it is complete                                         | listing partial — distinct backup counts unknown                        |

  # source: pro:src/evidence-backup.jsx:328
  @pro
  Scenario: A fragment without a listing shows no listing line
    Given the fragment has no listing
    Then no listing line is shown

  # source: pro:src/evidence-backup.jsx:312,339-343
  @pro
  Scenario: Every warning the evidence carries is shown
    Given the fragment carries warnings
    Then each warning's message is shown as its own note, in order
    And a warning without a message reads "an unlabelled warning was reported"

  # source: pro:src/evidence-backup.jsx:137-151,345-354
  @pro
  Scenario: Each distinct backup is shown once
    Given the fragment lists the same backup identifier in several records
    Then one block is shown for that backup, in the position of its first record
    And it shows the identifier, "created <the creation time as published>" or "created unknown", and "· <size> bytes" when a size is known
    And the creation time and size are taken from the first record that states them
    And records without a backup identifier are not shown

  # source: pro:src/evidence-backup.jsx:152-166,355-362
  @pro
  Scenario: Each copy of a backup is shown once with its location, kind and age
    Given a backup's records list copies
    Then one row is shown per distinct pair of location and copy identifier
    And the row shows the copy identifier, its location or "location unknown", its kind or "kind unknown", and its age
    And copies without a copy identifier are not shown

  # source: pro:src/evidence-backup.jsx:292-297
  @pro
  Scenario Outline: A copy's age is measured from its own last success
    Given a copy whose governing last success is <success>
    Then its age reads "<text>"

    Examples:
      | success                                  | text                                                            |
      | 4 hours ago                              | age 4h old                                                      |
      | absent or unparsable                     | never succeeded at its location                                 |
      | 10 seconds in the future                 | age 0s old                                                      |
      | more than 30 seconds in the future       | age unknown — success timestamp exceeds allowed clock skew      |

  # source: pro:src/evidence-backup.jsx:299-304,363-364
  @pro
  Scenario Outline: A copy's save and upload attempts are shown as recorded
    Given a copy carries a <kind> attempt with <facts>
    Then a line reads "<line>"

    Examples:
      | kind   | facts                                                                                      | line                                                                 |
      | save   | outcome "success" attempted at "2026-01-05T02:00:00Z"                                      | save success · attempted 2026-01-05T02:00:00Z                        |
      | upload | outcome "failed" attempted at "2026-01-05T02:01:00Z" and the reason "bucket unreachable"   | upload failed · attempted 2026-01-05T02:01:00Z · bucket unreachable  |
      | upload | no outcome and no attempt time                                                             | upload unknown                                                       |

  # source: pro:src/evidence-backup.jsx:363-364
  @pro
  Scenario: A copy without an attempt record shows no line for it
    Given a copy carries no save record or no upload record
    Then no line is shown for the missing record

  # source: pro:src/evidence-backup.jsx:83-131,159-164
  @pro
  Scenario Outline: Duplicate observations of one copy are reconciled into one
    Given two records describe the same copy of the same backup
    And <observations>
    Then the copy shows <result>

    Examples:
      | observations                                                                                   | result                                                                       |
      | one attempt was made later than the other                                                      | the later attempt's outcome, time and reason                                 |
      | both attempts were made at the same time and only one completed                                | the completed attempt                                                        |
      | both were made at the same time and completed at different times                               | the one that completed later                                                 |
      | both share attempt and completion times but one failed                                         | the failed attempt                                                           |
      | the later attempt failed with no last success and the earlier one carries a last success       | the later attempt with the earlier one's last success kept                   |
      | the later attempt's last success is stamped beyond the 30-second skew and the earlier one's is valid | the later attempt with the earlier, valid last success                 |
      | both last successes are valid                                                                  | the more recent last success                                                 |
      | only one of the two records carries the attempt                                                | that attempt                                                                 |

  # source: pro:src/evidence-backup.jsx:87-99
  @pro
  Scenario Outline: Observations with no attempt time are ordered by completion, then failure, then outcome word
    Given two records describe the same attempt of the same copy
    And <observations>
    Then the copy shows <result>

    Examples:
      | observations                                                                    | result                                             |
      | only one states an attempt time                                                 | the one that states it                             |
      | neither states an attempt time and only one completed                           | the completed one                                  |
      | neither states an attempt time and they completed at different times            | the one that completed later                       |
      | neither states an attempt time, completion is equal and only one failed         | the failed one                                     |
      | everything else is equal and their outcome words differ                         | the one whose outcome word sorts first alphabetically |

  # source: pro:src/evidence-backup.jsx:314,371-376
  @pro
  Scenario Outline: The prune attempt is shown when the evidence carries one
    Given the fragment's prune record <prune>
    Then <line>

    Examples:
      | prune                                            | line                                              |
      | has outcome "success"                            | a line reads "prune success"                      |
      | has outcome "failed" and reason "lock held"      | a line reads "prune failed · lock held"           |
      | has no outcome                                   | a line reads "prune unknown"                      |
      | is absent                                        | no prune line is shown                            |

  # source: pro:src/evidence-backup.jsx:306-316,324-326,345
  @pro
  Scenario: An empty fragment still renders
    Given a backup fragment that carries nothing
    Then the panel shows the verdict, the single policy line "policy unknown" and nothing else

  # source: pro:src/evidence-backup.jsx:30-32,306-378
  @pro
  Scenario: The backup panel is read-only for every user
    When a backup panel is shown to an admin or to a read-only user
    Then it offers no button, link or other control
    And it issues no request of its own
