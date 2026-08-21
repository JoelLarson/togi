# togi judges the feature's diff, not the repo — at touched-entity granularity

Findings are filtered to the diff against `git merge-base HEAD <base>`. This
collapses brownfield and greenfield into the same problem — greenfield is just
brownfield from commit one — and keeps togi polite on open-source repos: my
contribution meets my bar, and the maintainer's legacy code is not my hostage.

Scope is **touched-entity**, not strict line intersection: a finding is in
scope if its range intersects any changed line **or contains** one. Add two
lines to a 300-line legacy function and its complexity finding is yours,
because you chose to extend that function. Strict line intersection would make
structural gates nearly inert on modified files — a function is over-complex
as a whole, not at a line — and structural gates are the ones I care most
about. For point findings, touched-entity degenerates to intersection anyway.

## Consequences

The obvious failure mode is a two-line change to a legacy monster obligating a
large refactor. Three relief valves, in order of use: **waivers** (approve the
specific fingerprint, with a reason, and move on), whole-repo gates staying
**report-only** on brownfield repos, and the optional **ratchet** for repos
being healed slowly rather than all at once.
