Feature: Using principle pages
  As a developer understanding a finding
  I want to read the principle behind the rule that fired
  So that tool-specific rules lead to stable engineering guidance

  Rule: Pages explain the principles behind findings
    Scenario: A shipped principle page can be shown
      Given no operator copy of "small-composable-functions"
      When I show the "small-composable-functions" principle page
      Then the shipped page body and provenance are displayed

    Scenario: An operator page overrides the shipped page
      Given an operator copy of "small-composable-functions"
      When I show the "small-composable-functions" principle page
      Then the operator page body and provenance are displayed

    Scenario: Page aliases are displayed in deterministic order
      Given several gate aliases for "small-composable-functions"
      When I show the "small-composable-functions" principle page
      Then its aliases are displayed in gate, language, and rule order
