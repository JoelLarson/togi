# Handling every error

Every call that can fail returns that fact to you, and every one of those
returns is a decision you are making whether or not you write it down.
Discarding the value silently is still a decision — it just leaves no record
that you made it, and no way for a reader to tell a considered choice from an
oversight.

The tool reports calls whose error return goes unread. The work is not to make
the report go away; it is to decide, at each call, what a failure there would
actually mean, and then to say so in the code.

## Why this matters

A discarded error does not make the failure disappear. It relocates it. The
operation that failed returns to a caller that believes it succeeded, and the
program continues on a false premise until some later, unrelated operation
trips over the state that was never written, never released, or never valid.
The stack trace you eventually get points at the victim, not the cause.

Deferred cleanup is where this bites hardest, because it is where it looks
most harmless. On a handle opened for reading, a close failure genuinely is
uninteresting — nothing was pending. On a handle opened for *writing*, close
is where buffered data is flushed and where the filesystem reports that the
disk was full or the quota was exceeded. Discarding that error discards the
write failure itself, and the function returns `nil` having lost the data it
was asked to save.

The third cost is review. A codebase that ignores errors inconsistently
teaches its readers that an unchecked call means nothing in particular, so the
one place where it means "this genuinely cannot fail" reads identically to the
place where someone was in a hurry. Once the signal is gone, it cannot be
recovered by inspection.

## Techniques

Pick by what a failure at that call would mean, not in list order.

**Propagate with Context** — the default. The caller is better placed to
decide than you are, so return the error, wrapped with what you were trying to
do: `fmt.Errorf("write manifest: %w", err)`. Wrap with `%w` so callers can
still match on the underlying error.

**Name the Decision** — the failure genuinely does not matter, and you can say
why. Assign to blank with a comment giving the reason: not `_ = f.Close()` on
its own, but `_ = f.Close() // read-only handle; nothing to flush`. The
assignment silences the tool; the comment is the part that has value.

**Capture from Defer** — a deferred cleanup can fail meaningfully. Give the
function a named error return and let the defer assign into it, so the
cleanup's failure reaches the caller instead of being swallowed on the way
out.

**Join Cleanup Errors** — the deferred cleanup can fail *and* the body can
fail, and you need both. Combine them rather than picking one arbitrarily;
losing the first error to report the second is its own bug.

**Separate Read Handles from Write Handles** — much of the ambiguity around
deferred close comes from treating both the same way. A handle you only read
from can close-and-ignore with a reason; a handle you wrote to must have its
close error captured. Making the two look different in the code is what lets a
reader trust either one.

**Handle at the Boundary** — the error is real but there is nobody above you
to return it to, as in a background goroutine or a top-level handler. Do
something observable with it — log it with the context that makes it
actionable — rather than dropping it. "Nobody to return it to" is a reason to
record it, not a reason to discard it.

## Example

Before — the deferred close discards the flush, so a full disk is reported as
success:

```go
func WriteManifest(path string, manifest Manifest) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create manifest: %w", err)
	}
	defer file.Close()

	if err := json.NewEncoder(file).Encode(manifest); err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	return nil
}
```

`json.Encode` writes into a buffer. The bytes reach the disk during `Close`,
and that is precisely the error being thrown away.

After — a named return lets the deferred close report, and `errors.Join` keeps
the encode failure when both go wrong:

```go
func WriteManifest(path string, manifest Manifest) (err error) {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create manifest: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close manifest: %w", closeErr))
		}
	}()

	if err := json.NewEncoder(file).Encode(manifest); err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	return nil
}
```

And the read side, where the decision is the opposite one and says so:

```go
func ReadManifest(path string) (Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("open manifest: %w", err)
	}
	defer func() {
		_ = file.Close() // read-only handle; nothing buffered to flush
	}()
	...
}
```

Two calls to the same method, two different treatments, and a reader can tell
at a glance which one was thought about.

## Constraints

**A blank assignment without a reason is not a fix.** Rewriting
`f.Close()` as `_ = f.Close()` silences the tool and communicates nothing —
the next reader still cannot tell whether the error was considered. If you
cannot state why the failure does not matter, that is evidence it does.

**Do not check an error only to discard it.** Assigning to `err` and then
neither returning, wrapping, nor logging it is worse than the original: it
looks handled. The tool will be satisfied and the reader will be misled.

**Preserve behavior; do not edit tests to accommodate the change.** Returning
a previously-swallowed error is a *behavior* change at the boundary where the
caller now sees a failure it used to be shielded from. If a test fails, the
test is telling you the caller depended on the old silence — fix the caller,
or use a blank assignment with a reason. Changing the assertion trips the
test-integrity gate and blocks the run.

**Blank assignment is not a lint suppression.** `_ =` is a language construct
and does not trip the suppression integrity gate; a `//nolint` directive does.
Prefer the former when you have a real reason, and never reach for the latter
to clear this finding.

**Do not widen an interface to move the error.** Changing an exported
signature to return an error it did not return before is a permanent API
commitment made to satisfy a lint finding. Handle it internally, or raise the
change deliberately rather than as a side effect of this fix.
