Feature: Judging a feature diff
  As a developer evaluating committed work
  I want to judge findings against the feature diff
  So that unrelated repository findings do not obscure my change

  Rule: The feature is measured from a merge base
    Scenario: An explicit base selects the comparison history
      Given a committed feature branch with two possible bases
      And a gate finding belongs only to the explicitly based diff
      When I run the gauntlet with the older base
      Then the report records the explicit base and its merge base
      And the finding is in scope

    Scenario: The base is discovered from origin HEAD
      Given a committed feature branch whose origin HEAD points to "release"
      When I run the gauntlet without a base
      Then the report base is "origin/release"

    Scenario: A conventional local trunk is discovered without a remote
      Given a committed feature branch from local "main" without a remote
      When I run the gauntlet without a base
      Then the report base is "main"

    Scenario: Diverged history uses the merge base rather than the base tip
      Given trunk and the feature branch diverged after a shared commit
      And a gate reports findings on both branches' changes
      When I run the gauntlet against trunk
      Then the report merge base is the shared commit
      And only the feature finding is in scope

  Rule: Gate scope determines which findings survive
    Scenario: A point finding survives only on a changed line
      Given a committed feature changes line 8 but not line 3
      And a diff-scoped gate reports point findings on lines 3 and 8
      When I run the gauntlet
      Then only the finding on line 8 remains

    Scenario: Touching a Go declaration includes its structural finding
      Given a committed feature changes the body of function "calculate"
      And a diff-scoped gate reports an entity finding on the function signature
      When I run the gauntlet
      Then the structural finding for "calculate" remains

    Scenario: A repository-scoped finding survives outside the diff
      Given a committed feature changes "feature.go"
      And a whole-repo gate reports a finding in "legacy.go"
      When I run the gauntlet
      Then the finding in "legacy.go" remains

    Scenario: Deleting a line owns the adjacent deletion location
      Given a committed feature deletes line 5 from "feature.go"
      And a diff-scoped gate reports a point finding at the deletion location
      When I run the gauntlet
      Then the deletion finding remains in scope

    Scenario: A renamed file is judged at its new path
      Given a committed feature renames "before.go" to "after.go"
      And a gate reports a finding in "after.go"
      When I run the gauntlet
      Then the report records the finding in "after.go"

    Scenario: A binary change is recorded without inventing changed lines
      Given a committed feature changes the binary file "image.bin"
      When I run the gauntlet
      Then the diff records one changed file and zero changed lines

  Rule: Invalid repository state prevents a run from starting
    Scenario Outline: A repository precondition fails before tools or state
      Given a repository with <precondition>
      And a gate that records whether it starts
      When I run the gauntlet
      Then the run is rejected for the <precondition>
      And no gate, ledger, or target-repository file is created

      Examples:
        | precondition          |
        | dirty worktree        |
        | unsupported submodule |
        | missing base          |
        | invalid base          |
        | unrelated history     |
