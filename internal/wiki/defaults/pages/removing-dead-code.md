# Removing dead code

Code that cannot affect the program's behaviour should not be in the program.
A function nothing calls, a constant nothing reads, a value assigned and then
overwritten before anyone looks at it — each is text that a reader must
understand and a refactorer must preserve, in exchange for nothing.

The tool reports two shapes of this: declarations with no reachable reference,
and assignments whose value is discarded before it is read. They are the same
defect at different scales, and they have the same first question: *was this
supposed to be connected to something?*

## Why this matters

Dead code is read as live code. A reviewer encountering an unused function has
no way to know it is unused without searching, so they spend the attention
anyway — and they extend it the same courtesy every live function gets. It is
kept compiling, updated during renames, migrated during framework changes, and
carried through every refactor, forever, for no benefit.

It also actively misleads. An unused helper named `validateChecksum` tells a
reader that checksums are validated somewhere. A dead store tells a reader
that the value it computed matters. Both are claims the code is making that
are not true, and a reader who believes either will reason incorrectly about
everything downstream.

Most importantly: **an unused declaration is often a bug rather than debris.**
Somebody wrote `validateChecksum` intending to call it, and the call site was
lost in a merge or never landed. An ineffectual assignment is very frequently
a mistake — the wrong variable was assigned, or an early return was added
above it. The finding is evidence that the author's intent and the code's
behaviour have diverged; deletion is only correct once you have established
which of the two is wrong.

## Techniques

**Establish Intent First** — before anything else, decide whether this is
debris or a missing connection. Read the declaration's name and body and ask
what it was for. If the answer is "this should be called from X", the fix is
to call it from X, and deleting it would bury a real defect.

**Delete It** — the primary move once intent is settled. Version control
remembers; a commented-out block or a `_ = unusedThing` reference to keep the
compiler quiet is strictly worse than removal, because it retains every cost
of the code while removing the possibility that anyone ever runs it.

**Inline Function** — the declaration has exactly one caller and exists only
to name a step that no longer needs naming. Fold it into that caller and
delete it.

**Remove the Dead Store** — the assignment's value is never read. Delete the
assignment. If the variable's only remaining assignment is its declaration,
narrow it to the scope that actually uses it, which usually makes the
liveness obvious to the next reader without the tool's help.

**Reorder to Restore the Read** — the store is dead because something above
it returns early or reassigns. That is the shape of a real bug: the value was
meant to be used. Fix the ordering rather than removing the store.

**Remove Unused Parameter** — a parameter no caller varies, or the callee
ignores. Dropping it simplifies every call site, but it is a signature change
— see the constraints.

## Example

Before — an ineffectual assignment that is not debris but a bug:

```go
func resolveHead(ctx context.Context, root string) (string, error) {
	head, err := revParse(ctx, root, "HEAD")
	if err != nil {
		return "", err
	}

	if detached(ctx, root) {
		head, err = revParse(ctx, root, "HEAD@{0}")
		if err != nil {
			return "", err
		}
	}

	return revParse(ctx, root, "HEAD")
}
```

The tool flags the assignment to `head` as ineffectual, and it is right — but
deleting the assignment would be exactly wrong. The defect is the final line,
which recomputes `HEAD` and throws away the detached-head resolution the
function just did. The dead store was pointing at the bug.

After:

```go
	return head, nil
```

Whereas a genuinely dead declaration gets no ceremony:

```go
// nothing references this; the caller it was written for was never added
func integrityBuildTag(file string) string { ... }
```

...is simply removed, and the commit message records that it never had a
caller.

## Constraints

**Prove unreachability before deleting.** Static analysis does not see
reflection, generated code, build-tag-guarded callers, or a platform variant
you are not currently compiling. A declaration used only by a file behind a
build tag your run does not include will be reported as unused and is not.
Check the other build configurations before removing anything.

**Do not delete an exported declaration on this page's account.** An exported
symbol with no in-repo caller may have callers you cannot see. Removing it is
an API break, which needs to be decided deliberately, not absorbed into a lint
cleanup.

**Do not delete tests, fixtures, or test helpers to clear this finding.**
Reducing test surface is exactly what the test-integrity gate blocks, and it
will block the run. A test helper that is genuinely unused is still a test
file change that deserves its own consideration.

**Never silence it with a fake reference.** `var _ = unusedThing` and
`//nolint:unused` both keep the code and destroy the signal. The second also
trips the suppression integrity gate.

**Behaviour is preserved.** If removing a declaration changes what the program
does, it was not dead, and the finding was a false positive worth
understanding rather than obeying.
