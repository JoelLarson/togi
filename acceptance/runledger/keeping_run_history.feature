Feature: Keeping run history
  As a developer returning to a repository
  I can inspect durable run history
  So that results survive checkouts without modifying the target repository

  Rule: Completed runs live outside the target repository
    Scenario: A run persists its report and raw gate output externally
      Given a committed Go repository with one gate finding
      When I run the gauntlet
      Then report.json and both raw gate streams are persisted under XDG state

    Scenario: Running togi adds no files to the target repository
      Given a committed Go repository with one gate finding
      When I run the gauntlet
      Then the target repository tree and status are unchanged

    Scenario: Linked worktrees share one repository history
      Given a repository with primary and linked feature worktrees
      And a completed run in the linked worktree
      When I inspect status from the primary worktree
      Then status renders the linked worktree run

  Rule: The repository history has one active writer
    Scenario: A second run is rejected while the first run is active
      Given a committed Go repository with a gate paused after startup
      When I start another gauntlet run for the repository
      Then the second run is rejected as locked
      And the first run can complete after the gate resumes

    Scenario: An abandoned unlocked lock file does not block a new run
      Given a committed Go repository with an abandoned unlocked ledger file
      When I run the gauntlet
      Then a completed report is persisted

  Rule: History remains bounded and readable
    Scenario: Starting a run prunes history to the retention limit
      Given a committed Go repository with 20 completed runs
      When I complete one more run
      Then only the newest 20 run directories remain

    Scenario: Status selects the latest complete valid report
      Given a committed Go repository with two completed runs
      When I inspect repository status
      Then status renders the newer completed run

    Scenario: Status ignores newer incomplete and corrupt runs
      Given a committed Go repository with one completed run
      And newer incomplete and corrupt run directories
      When I inspect repository status
      Then status renders the completed run
