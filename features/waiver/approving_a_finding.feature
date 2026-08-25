Feature: Approving a finding
  As an operator whose run was stopped by a finding I judge acceptable
  I want to approve that finding with a recorded reason
  So that a reader can reconstruct the judgement instead of guessing

  Rule: An approval is explained and durable
    Scenario: Approving a finding records its reason and approval time
      Given a repository and a reported finding fingerprint
      When I waive that fingerprint for "the deleted test covered a removed feature"
      Then the waiver is recorded with that reason and the time it was approved

    Scenario: An approval without a reason is refused
      Given a repository and a reported finding fingerprint
      When I waive that fingerprint for no stated reason
      Then the approval is refused and no waiver is recorded

    Scenario: Approving the same finding twice keeps the first judgement
      Given a repository and a reported finding fingerprint
      And that fingerprint was already waived for "the deleted test covered a removed feature"
      When I waive that fingerprint for "a second, later judgement"
      Then one waiver is recorded, keeping the first reason

  Rule: An approval belongs to the operator, not to the repository
    Scenario: An approval writes nothing into the target repository
      Given a repository and a reported finding fingerprint
      When I waive that fingerprint for "the flagged fixture is deliberate"
      Then the waiver is stored outside the repository, whose files are unchanged

    Scenario: An approval made in one checkout holds in another
      Given a repository and a reported finding fingerprint
      And a second checkout of that repository
      When I waive that fingerprint from the second checkout for "approved while reviewing the branch"
      Then the waiver is visible from the first checkout
