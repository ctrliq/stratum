Feature: CIS control profile
  Beside the observed signals, the Hardening screen lists the static profile
  of controls the hub says the CIS profile applies. It is a reference list:
  no control carries a pass, fail, severity or status of its own.

  # source: pro:src/screens/hardening.jsx:64-66,132-136
  # source: pro:src/cluster-context.js:35-48
  @pro
  Scenario: The control list is the one the hub serves in its configuration
    Given the hub configuration carries a CIS profile of 24 controls
    When the user opens Hardening
    Then a card titled "CIS control profile (24)" is shown
    And it is captioned "applied by --profile cis · per-node PASS/FAIL via stratum hardening check"

  # source: pro:src/screens/hardening.jsx:16-30,141-143
  @pro
  Scenario: Controls are grouped under their group in the order the hub lists them
    Given the profile lists controls of the groups "API Server", "Kubelet" and "API Server" again, from sources "kube-apiserver.yaml", "kubelet.conf" and "admission-webhook.yaml"
    Then the "API Server" group appears first and contains both of its controls in hub order
    And its heading reads "API Server · kube-apiserver.yaml, admission-webhook.yaml"
    And the "Kubelet" group follows with the heading "Kubelet · kubelet.conf"
    And a source repeated within a group is named once

  # source: pro:src/screens/hardening.jsx:144-154
  @pro
  Scenario Outline: Each control shows its identifier, description and how it is applied
    Given a control with id "1.2.15-19", a description, <note> and tag <tag>
    Then its row shows the id, the description, <note shown> and <badge>

    Examples:
      | note      | tag          | note shown             | badge                        |
      | no note   | "cis"        | no second line         | a "--profile cis" badge      |
      | a note    | "exception"  | the note beneath       | an "exception" badge         |
      | no note   | none         | no second line         | no badge                     |
      | no note   | "other"      | no second line         | no badge                     |

  # source: pro:src/screens/hardening.jsx:137-139
  # source: pro:src/cluster-context.js:47,50-52
  @pro
  Scenario: A hub that serves no control list is reported as such
    Given the hub configuration carries no CIS profile, or the configuration could not be read
    When the user opens Hardening
    Then the card is titled "CIS control profile (0)"
    And it reads "Control list unavailable — the hub did not serve one."

  # source: pro:src/screens/hardening.jsx:159-162
  @pro
  Scenario: The card always explains which controls depend on the CIS profile
    When the control profile card is shown, with or without controls
    Then it ends with "Safe controls (profiling off, localhost binds, SA-lookup, TLS ciphers, perms) are always on; --profile cis adds audit logging, encryption-at-rest, PodSecurity, and kubelet protectKernelDefaults."

  # source: pro:src/screens/hardening.jsx:1-11,144-154
  # assumption: recorded from the absence of any result field in the rendered rows
  @pro
  Scenario: Controls carry no result
    When the control profile is shown
    Then no control shows a pass, fail, severity, status or timestamp
    And the list cannot be filtered, sorted or searched
