Feature: Running the gauntlet
  As a developer evaluating a feature
  I want to run independent quality gates
  So that I receive a complete, trustworthy quality report

  Rule: Selected gates produce one normalized report
    Scenario: Shipped Go gates run without installed operator tools
      Given a committed Go repository with a changed function
      And the shipped Go gates report one complexity and one lint finding
      When I run the gauntlet
      Then the report contains the "complexity" and "lint" gates
      And the report contains both shipped findings

    Scenario: An operator can select one gate from the gauntlet
      Given a committed Go repository with a changed function
      And the "complexity" and "lint" gates report findings
      When I run the gauntlet with only the "lint" gate
      Then the report contains only the "lint" gate

    Scenario: Tool findings are normalized into the public schema
      Given a committed Go repository with a changed function
      And the "lint" gate reports rule "golangci-lint/errcheck" on "feature.go" line 4
      When I run the gauntlet
      Then the finding records its gate, language, rule, severity, location, message, and fingerprint

    Scenario: Repeated occurrences are grouped under one finding
      Given a committed Go repository with a changed function
      And one gate reports the same finding on lines 3, 8, and 13
      When I run the gauntlet
      Then the report contains one finding with two occurrences

    Scenario: An unchanged finding keeps its fingerprint across runs
      Given a committed Go repository with one gate finding
      When I run the unchanged gauntlet twice
      Then the finding fingerprint is identical in both reports

    Scenario: Gate reports are ordered independently of completion time
      Given a committed Go repository with "alpha" and "zeta" gates
      And the "alpha" gate finishes after the "zeta" gate
      When I run the gauntlet
      Then the report orders gates as "alpha,zeta"

    Scenario: Compiler-style output excludes raw tool diagnostics
      Given a committed Go repository with one gate finding
      And the gate writes the raw diagnostic "PRIVATE RAW DIAGNOSTIC"
      When I run the gauntlet
      Then stdout contains compiler-style findings
      And the raw diagnostic exists only in a persisted raw artifact

  Rule: An errored gate never suppresses a healthy sibling
    Scenario Outline: A gate infrastructure problem is reported as errored
      Given a committed Go repository with a healthy gate finding
      And a sibling gate experiences <problem>
      When I run the gauntlet
      Then the sibling gate is errored
      And the healthy gate finding remains in the report

      Examples:
        | problem                |
        | a missing tool         |
        | a crashed tool         |
        | a timed out tool       |
        | malformed output       |

    Scenario: A version mismatch is advisory
      Given a committed Go repository with a healthy versioned gate
      And the tool version is outside the gate constraint
      When I run the gauntlet
      Then the gate has a version warning
      And the gate is not errored

  Rule: The report and application outcome agree
    Scenario Outline: A completed run has a classified verdict
      Given a committed Go repository whose gate result is <gate result>
      When I run the gauntlet
      Then the report verdict is <verdict>
      And the application outcome is <outcome>

      Examples:
        | gate result | verdict    | outcome |
        | findings    | findings   | 1       |
        | errored     | errored    | 4       |
        | clear       | unverified | 5       |
