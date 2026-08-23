Feature: Supporting platforms
  As an operator on an unfamiliar machine
  I want an explicit platform result
  So that an unsupported machine cannot appear to have passed its gates

  @linux
  Scenario: The real Linux host executes the gauntlet
    Given a committed Go repository with a clear gate
    When I run the gauntlet on the real host
    Then a completed unverified report is persisted

  @unsupported-host
  Scenario: A real unsupported host rejects the gauntlet before startup
    Given a committed Go repository with a gate that records whether it starts
    When I run the gauntlet on the real host
    Then the platform is rejected before repository, gate, or ledger access

  @simulated-platform
  Scenario Outline: A simulated unsupported platform is rejected before startup
    Given a committed Go repository with a gate that records whether it starts
    And the runtime platform is <platform>
    When I run the gauntlet
    Then the platform is rejected before repository, gate, or ledger access

    Examples:
      | platform |
      | darwin   |
      | windows  |
      | freebsd  |
