Feature: Fixing a feature diff safely
  As a developer delegating fixes
  I want every mutation validated and bounded
  So that only witnessed improvements reach my feature branch

  Rule: Mutation starts only from a green and complete baseline
    Scenario: A clean fix lands as one squash commit
      Given a green feature with a blocking finding
      And the agent removes the finding
      When I run the fix loop
      Then the fix run is unsealed with exit 6
      And one squash commit with the fixed tree reaches the feature branch
      And the fix audit contains its report plan and brief

    Scenario: A clean initial gauntlet needs no agent
      Given a green feature without blockers
      When I run the fix loop
      Then the fix run is unsealed with exit 6
      And the agent was invoked 0 times
      And the feature branch is unchanged

    Scenario: A clean run seals its glacial gate once
      Given a green feature without blockers
      And a glacial gate records its invocations
      When I run the fix loop
      Then the fix run is unsealed with exit 6
      And the fix loop used 0 iterations
      And the agent was invoked 0 times
      And the glacial gate ran 1 time
      And the fix report explains the gate schedule
      And the feature branch is unchanged

    Scenario: A missing agent is an explicit run error
      Given a green feature with a blocking finding
      But the selected agent is missing
      When I run the fix loop
      Then the fix run is errored with exit 4
      And the feature branch is unchanged

    Scenario Outline: An absent or red behavioral baseline is unverified
      Given a feature whose behavioral suite is <condition>
      When I run the fix loop
      Then the fix run is unverified with exit 5
      And the agent was invoked 0 times
      And the feature branch is unchanged

      Examples:
        | condition |
        | missing   |
        | red       |

    Scenario: An initial gate error prevents mutation
      Given a green feature whose initial gate errors
      When I run the fix loop
      Then the fix run is errored with exit 4
      And the agent was invoked 0 times
      And the feature branch is unchanged

  Rule: Each agent batch earns a rollback commit through local validation
    Scenario: A primary-file batch may fix a related file
      Given a green feature with a blocking finding
      And the agent makes a valid cross-file fix
      When I run the fix loop
      Then the fix run is unsealed with exit 6
      And the landed tree contains both related edits

    Scenario: Repeated no-op attempts become stuck
      Given a green feature with a blocking finding
      And the agent makes no changes
      When I run the fix loop
      Then the fix run is blocked with exit 2
      And the agent was invoked 2 times
      And the validated run branch is absent

  Rule: Integrity prevents an agent from weakening the evidence
    Scenario Outline: Evidence weakening blocks the batch
      Given a green feature with a blocking finding
      And the agent attempts <violation>
      When I run the fix loop
      Then the fix run is blocked with exit 2
      And the feature branch is unchanged

      Examples:
        | violation                  |
        | an unauthorized Git commit |
        | a new suppression           |
        | test deletion               |
        | an assertion change         |

    Scenario: A production rename witnessed by tests is allowed
      Given a green feature with a blocking finding
      And the agent performs a witnessed compilation-only rename
      When I run the fix loop
      Then the fix run is unsealed with exit 6
      And the witnessed rename reaches the feature branch

    Scenario: A waived integrity violation permits the next fix run
      Given a green feature with a blocking finding
      And the agent attempts a new suppression
      When I run the fix loop
      Then the fix run is blocked with exit 2
      And each blocked integrity fingerprint is printed
      When I waive each blocked integrity fingerprint
      And I run the fix loop
      Then the fix run is unsealed with exit 6

  Rule: Rails and stalemate bound unattended execution
    Scenario: The iteration rail stops retries
      Given a green feature with a blocking finding
      And the agent makes no changes
      But only one iteration is allowed
      When I run the fix loop
      Then the fix run is rails-exhausted with exit 3
      And the agent was invoked 1 time

    Scenario: The wall-clock rail stops a running agent
      Given a green feature with a blocking finding
      And the agent exceeds the wall-clock budget
      When I run the fix loop
      Then the fix run is rails-exhausted with exit 3
      And the agent was invoked 1 time

  Rule: Only a guarded squash commit reaches the feature branch
    Scenario: A final full-suite regression is not landed
      Given a green feature with a blocking finding
      And the agent introduces a regression outside local validation
      When I run the fix loop
      Then the fix run is blocked with exit 2
      And the validated run branch is preserved
      And the feature branch is unchanged

    Scenario Outline: A changed landing target is refused
      Given a green feature with a blocking finding
      And the agent fixes it while the original worktree becomes <condition>
      When I run the fix loop
      Then the fix run is blocked with exit 2
      And the validated run branch is preserved
      And the concurrent feature state is preserved

      Examples:
        | condition      |
        | dirty          |
        | detached       |
        | branch-moved   |
