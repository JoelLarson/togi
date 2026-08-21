# Nothing togi-related is ever embedded in a target repo

togi must work on open-source repos where no AI tooling may land in the tree,
and on repos owned by people who never agreed to use togi. So the only thing
that ever touches a target repo is the code diff itself: no `.togi/` directory,
no config file, no gate definitions, no state, no bot commits.

Everything togi needs about a repo — its gauntlet, thresholds, models, rails,
suite commands, ratchet, waivers — lives outside the repo, keyed by
**repo-id** (ADR-0011) under XDG paths (ADR-0003).

## Consequences

- Repo identity has to be derived rather than read from a file in the tree,
  which is why ADR-0011 exists at all.
- Per-project config is invisible to collaborators — deliberately. togi
  encodes *one engineer's* standards, not a team's shared CI policy.
- Suite/build commands can't be read from a committed togi config, so they're
  detected per language and overridden in external config.
