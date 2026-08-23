Feature: Customizing gates
  As an operator evolving personal standards
  I want to customize gates outside the target repository
  So that I can hold my own bar on repositories I do not own

  Rule: Shipped definitions work without repository configuration
    Scenario: Shipped gates normalize representative Go tool output
      Given a committed Go repository with a changed function
      And the shipped Go gates report representative findings
      When I run the gauntlet
      Then the shipped findings are normalized without repository configuration

  Rule: XDG definitions replace or extend the shipped gauntlet
    Scenario: An XDG definition wholly overrides a shipped gate
      Given a committed Go repository with an XDG override for the "lint" gate
      When I run the gauntlet
      Then the report contains the overridden lint behavior only
      And the target repository contains no gate definition

    Scenario: An XDG-only gate joins the shipped gates
      Given a committed Go repository with an additional "architecture" gate in XDG config
      When I run the gauntlet
      Then the report contains the shipped gates and the "architecture" gate
      And the target repository contains no gate definition

  Rule: Invalid gate data prevents tool execution
    Scenario: An invalid definition is rejected before any gate starts
      Given a committed Go repository with an invalid XDG gate definition
      And every available gate records whether it starts
      When I run the gauntlet
      Then the run is rejected for invalid gate data
      And no gate, ledger, or target-repository file is created
