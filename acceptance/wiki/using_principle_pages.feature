Feature: Using principle pages
  As a developer understanding a finding
  I want to inspect and customize principle pages
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

  Rule: Alias problems are explicit
    Scenario: A dangling alias produces a warning without failing lint
      Given a gate alias whose principle page does not exist
      When I lint the principle pages
      Then the dangling alias is warned and the outcome is 0

    Scenario: Conflicting aliases fail wiki lint
      Given one rule is aliased to two principle pages
      When I lint the principle pages
      Then both conflicting pages are reported and the outcome is 1

  Rule: Ejection preserves operator work
    Scenario: Ejecting a page creates an operator copy
      Given no operator copy of "small-composable-functions"
      When I eject the "small-composable-functions" principle page
      Then the operator copy equals the shipped page

    Scenario: Ejecting never overwrites an existing operator copy
      Given an existing operator copy of "small-composable-functions"
      When I eject the "small-composable-functions" principle page
      Then the eject is rejected and the operator copy is unchanged
