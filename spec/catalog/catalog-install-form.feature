Feature: Catalog install form
  An application without a release shows an install form seeded from its
  catalog entry. The form's fields, their defaults and their validation
  decide the HelmChart that Install will create.

  # source: pro:src/catalog-rules.js:44-54
  # source: pro:src/screens/catalog.jsx:809-833
  @pro
  Scenario Outline: The form is seeded from the catalog entry
    Given an application with id "loki" whose index entry <entry>
    When its detail panel opens and it has no release
    Then "<field>" is <value>

    Examples:
      | entry                                   | field                                      | value      |
      | sets nothing under install              | Release name                               | "loki"     |
      | sets nothing under install              | Target namespace                           | "loki"     |
      | sets install target namespace "logging" | Target namespace                           | "logging"  |
      | sets nothing under install              | Create the namespace if it does not exist  | ticked     |
      | sets install create namespace to false  | Create the namespace if it does not exist  | not ticked |

  # source: pro:src/catalog-rules.js:13
  # source: pro:src/screens/catalog.jsx:661,820-824
  @pro
  Scenario Outline: The release name must be a Helm release name
    When the user enters <name> as the release name
    Then the release name is <verdict>

    Examples:
      | name                                         | verdict  |
      | "a"                                          | accepted |
      | "loki"                                       | accepted |
      | "my-app-2"                                   | accepted |
      | "0abc"                                       | accepted |
      | a 53-character lowercase alphanumeric name   | accepted |
      | an empty name                                | rejected |
      | a 54-character lowercase alphanumeric name   | rejected |
      | "My-App"                                     | rejected |
      | "-app"                                       | rejected |
      | "app-"                                       | rejected |
      | "my_app"                                     | rejected |
      | "my.app"                                     | rejected |
      | " app"                                       | rejected |

  # source: pro:src/screens/catalog.jsx:820-821
  @pro
  Scenario: A rejected release name is explained
    When the release name is rejected
    Then the field is marked invalid with "Lowercase letters, digits and dashes; must start and end alphanumeric (max 53 chars)."
    And an accepted release name shows no help text

  # source: pro:src/catalog-rules.js:14
  # source: pro:src/screens/catalog.jsx:662,826-830
  @pro
  Scenario Outline: The target namespace must be a namespace name
    When the user enters <namespace> as the target namespace
    Then the target namespace is <verdict>

    Examples:
      | namespace                                    | verdict  |
      | "a"                                          | accepted |
      | "kube-system"                                | accepted |
      | a 63-character lowercase alphanumeric name   | accepted |
      | an empty name                                | rejected |
      | a 64-character lowercase alphanumeric name   | rejected |
      | "Logging"                                    | rejected |
      | "-logging"                                   | rejected |
      | "logging-"                                   | rejected |
      | "log.ging"                                   | rejected |

  # source: pro:src/screens/catalog.jsx:826
  @pro
  Scenario: A rejected target namespace is explained
    When the target namespace is rejected
    Then the field is marked invalid with "Must be a valid namespace name (DNS-1123 label)."

  # source: pro:src/ui/fields.jsx:24-50
  # source: pro:src/screens/catalog.jsx:835-842
  @pro
  Scenario Outline: Each catalog parameter gets an editor that fits its type
    Given the application declares a parameter of type <type>
    Then the form shows <editor>

    Examples:
      | type     | editor                                                                      |
      | string   | a text box labelled with the parameter's label                              |
      | number   | a number box labelled with the parameter's label                            |
      | enum     | a drop-down labelled with the parameter's label, offering exactly its options in index order |
      | boolean  | a tick box labelled with the parameter's label                              |
      | password | no editor of its own (see the credentials Secret)                           |

  # source: pro:src/ui/fields.jsx:9-22,29,34
  @pro
  Scenario: Parameter help and the required mark
    Given a parameter has help text and is required
    Then its help text is shown under its editor
    And a text, number or drop-down parameter's label is followed by " *"
    But a tick-box parameter's label never carries the required mark

  # source: pro:src/catalog-rules.js:56-66
  # source: pro:src/screens/catalog.jsx:835-842
  @pro
  Scenario: Parameters are shown under their group headings in index order
    Given the application declares parameters in the order A (group "Storage"), B (no group), C (group "Storage") and D (group "Ingress")
    Then the form shows the heading "Storage" with A and C, then B without a heading, then the heading "Ingress" with D

  # source: pro:src/catalog.js:259-271
  # source: pro:src/ui/fields.jsx:39-42
  # assumption: for an enum without a default the form's value is empty, so nothing is sent; what the drop-down displays for an empty value (nothing or its first option) is not readable from the code — opening an application with a default-less enum parameter in the running legacy dashboard would confirm it
  @pro
  Scenario Outline: A parameter starts at its catalog default
    Given a <type> parameter whose default in the index is <default>
    Then its editor starts <initial>

    Examples:
      | type    | default        | initial                 |
      | boolean | true           | ticked                  |
      | boolean | false          | not ticked              |
      | boolean | absent         | not ticked              |
      | boolean | the text "true" | not ticked             |
      | number  | 3              | at 3                    |
      | number  | the text "8080" | at 8080                |
      | number  | 0              | at 0                    |
      | number  | absent         | empty                   |
      | number  | null           | empty                   |
      | number  | ""             | empty                   |
      | number  | "abc"          | empty                   |
      | string  | "standard"     | with "standard"         |
      | string  | 42             | with "42"               |
      | string  | true           | with "true"             |
      | string  | absent         | empty                   |
      | string  | null           | empty                   |
      | enum    | "medium"       | with "medium" selected  |
      | enum    | absent         | with no value           |

  # source: pro:src/catalog-rules.js:68
  # source: pro:src/screens/catalog.jsx:668-672,910-914
  # assumption: the enum row is stated in terms of the form's value (none, so reported missing); whether the drop-down visibly shows an option at that moment needs the same runtime check as the default-less enum above
  @pro
  Scenario Outline: A required parameter must have a value before installing
    Given a required <type> parameter labelled "Thing"
    And its editor holds <value>
    Then "Required: Thing" is <shown> under the form and Install is <install>

    Examples:
      | type    | value          | shown     | install      |
      | string  | nothing        | shown     | unavailable  |
      | string  | "x"            | not shown | available    |
      | string  | a single space | not shown | available    |
      | number  | nothing        | shown     | unavailable  |
      | number  | 0              | not shown | available    |
      | enum    | no value       | shown     | unavailable  |
      | boolean | not ticked     | not shown | available    |

  # source: pro:src/screens/catalog.jsx:910-914
  @pro
  Scenario: Every unsatisfied required parameter is named
    Given several required parameters have no value
    Then one line reads "Required: " followed by their labels separated by commas, in index order

  # source: pro:src/screens/catalog.jsx:665,844-866
  @pro
  Scenario: An application with password parameters asks for a credentials Secret, never for the passwords
    Given the application declares password parameters labelled "Admin password" and "DB password"
    Then the form shows a "Credentials Secret" text box, empty by default
    And its help reads "Name of a Secret in kube-system holding Admin password, DB password. Leave empty to keep the chart's defaults."
    And a note explains that the values never pass through the dashboard, are not stored in the HelmChart, remain readable by anyone who can read Secrets in kube-system, and are ignored by a Stratum operator that predates the field
    And no box accepts a password value

  # source: pro:src/screens/catalog.jsx:844
  @pro
  Scenario: An application without password parameters has no credentials field
    Given the application declares no password parameter
    Then the form shows no "Credentials Secret" box and no credentials note

  # source: pro:src/catalog.js:355-357
  # source: pro:src/screens/catalog.jsx:666-667,847-851
  @pro
  Scenario Outline: The credentials Secret name must be a Secret name
    When the user enters <name> as the credentials Secret
    Then the name is <verdict>

    Examples:
      | name                                     | verdict                          |
      | nothing                                  | accepted (no Secret is used)     |
      | only spaces                              | accepted (no Secret is used)     |
      | "my-creds"                               | accepted                         |
      | "  my-creds  "                           | accepted as "my-creds"           |
      | "creds.prod.v1"                          | accepted                         |
      | a 253-character valid name               | accepted                         |
      | a 254-character otherwise valid name     | rejected                         |
      | "My-Creds"                               | rejected                         |
      | "-creds"                                 | rejected                         |
      | "creds-"                                 | rejected                         |
      | "creds..prod"                            | rejected                         |
      | "my_creds"                               | rejected                         |

  # source: pro:src/screens/catalog.jsx:847-850
  @pro
  Scenario: A rejected credentials Secret name is explained
    When the credentials Secret name is rejected
    Then the field is marked invalid with "Must be a valid Secret name (DNS-1123 subdomain, at most 253 characters)."
    And Install is unavailable

  # source: pro:src/screens/catalog.jsx:668-669
  @pro
  Scenario: A required password parameter is satisfied by naming a credentials Secret
    Given the application declares a required password parameter labelled "Admin password"
    When the credentials Secret box is empty
    Then "Required: Admin password" is shown and Install is unavailable
    When the user enters a valid Secret name
    Then the requirement is satisfied

  # source: pro:src/screens/catalog.jsx:855-858
  # source: pro:src/catalog.js:381-417
  @pro
  Scenario: A Secret manifest to apply is shown once a valid Secret name is entered
    Given the application declares password parameters with keys "auth.adminPassword" and "auth.db.password"
    When the user enters "my-creds" as the credentials Secret
    Then a copyable manifest is shown, captioned "Create it on this cluster first, your values in place of the placeholders: kubectl apply -f <file>"
    And the manifest is a Secret named "my-creds" in "kube-system" whose "values.yaml" entry nests "adminPassword" and "db: password" under "auth"
    And each password's value is a placeholder made of its key in angle brackets, such as "<auth.adminPassword>"
    And the manifest is hidden again when the box is emptied or the name is rejected

  # source: pro:src/catalog.js:385-405
  @pro
  Scenario Outline: Password keys map to nested entries of the Secret's values
    Given a password parameter with key <key>
    Then the Secret manifest carries <entry>

    Examples:
      | key                 | entry                                                                 |
      | "password"          | a top-level entry "password" with placeholder "<password>"            |
      | "auth.password"     | an entry "password" nested under "auth"                               |
      | "a.b.c"             | an entry "c" nested under "b" under "a"                               |
      | "ingress\.tls.key"  | an entry "key" nested under a quoted entry "ingress.tls"              |
      | "my key"            | a top-level entry whose name is quoted                                |
      | "__proto__.x"       | an ordinary entry "x" nested under "__proto__", with no other effect  |

  # source: pro:src/screens/catalog.jsx:654,868-888
  # source: pro:src/catalog-rules.js:50
  @pro
  Scenario: Advanced values are collapsed until asked for
    When the install form is first shown
    Then "ADVANCED VALUES" is collapsed
    When the user expands it
    Then a YAML text area appears, holding the application's default values from the index or the placeholder "# Extra Helm values (YAML), merged over the defaults" when there are none
    And a note states that the text is passed as-is without validation and that the form fields above take precedence over it

  # source: pro:src/screens/catalog.jsx:655,894-908
  @pro
  Scenario: The manifest that Install will create can be previewed
    When the user presses "PREVIEW MANIFEST"
    Then the HelmChart that Install would create from the current form is shown, and the control reads "HIDE MANIFEST"
    And the preview follows every change made in the form
    And pressing "HIDE MANIFEST" hides it again

  # source: pro:src/catalog.js:448-470
  # source: pro:src/screens/catalog.jsx:905
  @pro
  Scenario: The preview masks free-form values for an application with credentials
    Given the application declares at least one password parameter
    And the advanced values hold any non-blank text
    When the manifest is previewed
    Then the free-form values appear as "••••••••"
    But the HelmChart actually created by Install carries the text as entered

  # source: pro:src/catalog.js:449-450
  @pro
  Scenario: The preview is unmasked for an application without credentials
    Given the application declares no password parameter
    When the manifest is previewed
    Then the free-form values are shown as entered

  # source: pro:src/screens/catalog.jsx:656,813-817
  @pro
  Scenario: Reset restores the catalog defaults
    Given the user has changed any field of the install form
    Then a "Reset" button is shown beside the "Install" heading
    When the user presses it
    Then every field, the advanced values and the credentials Secret return to their seeded values and "Reset" disappears
    And "Reset" is not shown while the form equals its seeded values

  # source: pro:src/screens/catalog.jsx:617,653
  # source: pro:src/catalog-rules.js:42-43
  @pro
  Scenario: Edits never carry over to another application
    Given the user has edited the install form of one application
    When the user selects a different application
    Then that application's form shows its own seeded values, collapsed advanced values and a hidden preview
    And returning to the first application shows its seeded values again

  # source: pro:src/catalog.js:280-339
  @pro
  Scenario: The HelmChart built from the form
    Given an application "Loki" with id "loki" from chart "loki" version "6.1.0" in repository "https://charts.example/loki"
    And the form holds release name "loki-prod", target namespace "logging" and a ticked create-namespace box
    Then the HelmChart to create is of API version "stratum.ciq.com/v1" and kind "HelmChart", named "loki-prod" in namespace "kube-system"
    And it is labelled "catalog.stratum.ciq.com/app: loki" and "app.kubernetes.io/managed-by: stratum-catalog"
    And it is annotated "catalog.stratum.ciq.com/app-name: Loki"
    And its spec carries the repository, the chart name, the version, target namespace "logging" and create namespace true
    And its spec carries no release name of its own

  # source: pro:src/catalog.js:284-306,332-333
  @pro
  Scenario Outline: Form parameters become chart values by type
    Given a <type> parameter with key "k" whose editor holds <value>
    Then the HelmChart to create carries <result>

    Examples:
      | type     | value            | result                                             |
      | boolean  | ticked           | the typed value k = "true"                         |
      | boolean  | not ticked       | the typed value k = "false"                        |
      | number   | 3                | the typed value k = "3"                            |
      | number   | 0                | the typed value k = "0"                            |
      | number   | 1.50             | the typed value k = "1.5"                          |
      | number   | nothing          | no value for k                                     |
      | string   | "standard"       | the text value k = "standard"                      |
      | string   | "true"           | the text value k = "true"                          |
      | string   | "0123"           | the text value k = "0123"                          |
      | string   | nothing          | no value for k                                     |
      | string   | "a,b"            | the text value k = "a\,b"                          |
      | string   | "C:\dir"         | the text value k = "C:\\dir"                       |
      | string   | "a\,b"           | the text value k = "a\\\,b"                        |
      | string   | "{x}"            | the text value k = "\{x}"                          |
      | enum     | "medium"         | the text value k = "medium"                        |
      | enum     | no value         | no value for k                                     |
      | password | anything         | no value for k                                     |

  # source: pro:src/catalog.js:332-333
  @pro
  Scenario: Value sections are omitted when empty
    Given no parameter contributes a typed value, or none contributes a text value
    Then the HelmChart to create omits the corresponding section entirely

  # source: pro:src/catalog.js:308,334
  @pro
  Scenario Outline: Advanced values travel verbatim or not at all
    Given the advanced values hold <text>
    Then the HelmChart to create carries <result>

    Examples:
      | text                                   | result                                           |
      | nothing                                | no free-form values                              |
      | only spaces and line breaks            | no free-form values                              |
      | "  replicas: 2\n"                      | exactly that text, leading spaces and line break included |

  # source: pro:src/catalog.js:309,336
  # source: pro:src/catalog-rules.js:49
  # assumption: the form offers no timeout field; the timeout is always the catalog entry's
  @pro
  Scenario: The install timeout comes from the catalog entry
    Given the application's index entry sets install timeout "10m"
    Then the HelmChart to create carries timeout "10m"
    But an application whose entry sets no timeout creates a HelmChart with no timeout

  # source: pro:src/catalog.js:310,335,353
  @pro
  Scenario: A named credentials Secret is referenced, never copied
    Given the application declares a password parameter
    And the credentials Secret box holds "my-creds"
    Then the HelmChart to create references the Secret "my-creds" and its key "values.yaml" as a values Secret
    And it carries no password value anywhere
    But with an empty box the HelmChart to create references no Secret
