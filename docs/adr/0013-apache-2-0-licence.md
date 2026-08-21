# Apache-2.0, permissive, single licence for the whole repo

togi is licensed Apache-2.0, covering code, gate defaults, and wiki principle
pages alike. The goal is unimpeded adoption: anyone may use it, companies may
use it without paying, and forking it into a commercial product is
permitted.

Apache-2.0 over MIT for three reasons that matter to a tool intended for
corporate use: the explicit patent grant that legal reviewers look for,
section 5 placing outside contributions under the same terms without needing a
CLA, and section 6 withholding trademark rights so a fork cannot trade on the
name.

## Considered options

- **MIT** — shorter and universally recognised, but silent on patents, silent
  on contributor terms, and it gives up all three advantages above.
- **Copyleft (GPL / AGPL)** — rejected because it works against the stated
  goal. Companies may use GPL software freely, but many have blanket policies
  against it, and AGPL more so.

## GPLv2 compatibility: considered and accepted as a non-issue

Apache-2.0 is compatible with GPLv3 but not GPLv2, because GPLv2 section 6
forbids further restrictions and the FSF reads Apache's patent-termination and
indemnity clauses as exactly that.

This does not affect togi in practice. The incompatibility governs combining
*source* into one distributed work; it says nothing about running a program. A
GPLv2 project can use togi exactly as it uses gcc. ADR-0002 makes that
structural: togi never enters a target tree, never links into a build, and
exposes no importable package. The block also applies only to **GPLv2-only**
projects — anything licensed "version 2 or later" can elect GPLv3, where
Apache-2.0 is explicitly compatible.

**Dual-licensing "MIT OR Apache-2.0"** (the Rust convention) would close the
remaining gap and was rejected: it solves a hypothetical — someone vendoring
togi's source into a GPLv2-only codebase — at the cost of making the patent
grant electable rather than guaranteed, since any adopter could simply choose
the MIT branch. Revisit if a real GPLv2-only consumer ever appears.
